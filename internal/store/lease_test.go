package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store/migrations"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

func newReader(t *testing.T, url string, opts ...Option) *Store {
	t.Helper()
	s, err := OpenReader(url, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func fastHeartbeat(t *testing.T) {
	prev := leaseHeartbeat
	leaseHeartbeat = 100 * time.Millisecond
	t.Cleanup(func() { leaseHeartbeat = prev })
}

// A reader never writes: every write path refuses before touching the
// database, place matching included.
func TestReaderCannotWrite(t *testing.T) {
	ctx := context.Background()
	r := newReader(t, storetest.URL(t))
	assert.False(t, r.HoldsLease())
	assert.ErrorIs(t, r.SeedSources(ctx, []SourceSeed{{ID: "usgs", Name: "u", PollInterval: time.Minute}}), ErrNotWriter)
	assert.ErrorIs(t, r.UpsertPlace(ctx, testPlace("area:x", "x", "X", 1, nil)), ErrNotWriter)
	_, err := r.UpsertEvent(ctx, testEvent("usgs:q1", 2, 1, "x"))
	assert.ErrorIs(t, err, ErrNotWriter)
	_, err = r.TouchSeen(ctx, []string{"usgs:q1"}, baseTime, baseTime)
	assert.ErrorIs(t, err, ErrNotWriter)

	// The place cache is a writer-only structure, by check rather than by
	// convention: outside a fenced write transaction it refuses.
	_, err = r.loadPlaceGeoms(nil)
	assert.ErrorIs(t, err, ErrNotWriter)
}

// Acquire bumps the epoch and records holder and address; release clears
// them (readers stop forwarding at once) and the next holder bumps again.
func TestLeaseEpochHolderAndRelease(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	a := newReader(t, url)
	r := newReader(t, url)

	l, err := a.AcquireLease(ctx, LeaseOptions{Holder: "a", Address: "a:1"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), l.Epoch())
	w, err := r.WriterLease(ctx)
	require.NoError(t, err)
	assert.Equal(t, WriterInfo{Epoch: 1, Holder: "a", Address: "a:1", Live: true}, WriterInfo{
		Epoch: w.Epoch, Holder: w.Holder, Address: w.Address, Live: w.Live})
	seedSource(t, a, "usgs")

	_, err = r.AcquireLease(ctx, LeaseOptions{Holder: "r", Wait: 200 * time.Millisecond})
	assert.ErrorIs(t, err, ErrLockHeld, "one holder at a time")

	require.NoError(t, a.ReleaseLease())
	assert.False(t, a.HoldsLease())
	select {
	case <-l.Lost():
	default:
		t.Fatal("a released lease reports ended")
	}
	w, err = r.WriterLease(ctx)
	require.NoError(t, err)
	assert.False(t, w.Live)
	assert.Empty(t, w.Address)
	assert.ErrorIs(t, a.SeedSources(ctx, nil), ErrNotWriter, "a released holder is a reader again")

	l2, err := r.AcquireLease(ctx, LeaseOptions{Holder: "r", Address: "r:1", Wait: time.Second})
	require.NoError(t, err)
	assert.Equal(t, int64(2), l2.Epoch())
	seedSource(t, r, "usgs")
}

// Gate 1's fencing test: a writer whose epoch is stale (another process has
// started a term) cannot commit, and learns that it lost the lease.
func TestStaleEpochIsFenced(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	var lost atomic.Value
	a, err := Open(url, WithLockLost(func(err error) { lost.Store(err) }))
	require.NoError(t, err)
	defer a.Close()
	seedSource(t, a, "usgs")

	// What the next holder's bump does, done behind a's back (a still holds
	// the advisory lock, which is exactly the case the lock alone misses: a
	// session a thinks it still has).
	admin := newReader(t, url)
	_, err = admin.db.ExecContext(ctx, `UPDATE writer_lease SET epoch = epoch + 1`)
	require.NoError(t, err)

	_, err = a.UpsertEvent(ctx, testEvent("usgs:q1", 2, 1, "stale"))
	assert.ErrorIs(t, err, ErrFenced)
	assert.ErrorIs(t, err, ErrNotWriter)
	assert.False(t, a.HoldsLease())
	require.Eventually(t, func() bool { return lost.Load() != nil }, 2*time.Second, 10*time.Millisecond,
		"the OnLost hook fires")
	var n int
	require.NoError(t, admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n))
	assert.Zero(t, n, "no write from the stale writer landed")
	assert.ErrorIs(t, a.SeedSources(ctx, nil), ErrNotWriter, "and it stays a reader")
}

// The bump waits for a write transaction already past the fence, which then
// commits as the old term's last write; the old writer's next write fails.
func TestEpochBumpWaitsForInFlightWrite(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	a, err := Open(url)
	require.NoError(t, err)
	defer a.Close()
	seedSource(t, a, "usgs")
	admin := newReader(t, url)

	inside, release := make(chan struct{}), make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- a.inTx(ctx, func(tx *sql.Tx) error {
			close(inside)
			<-release
			_, err := tx.Exec(`UPDATE sources SET name = 'old term' WHERE id = 'usgs'`)
			return err
		})
	}()
	<-inside
	bumped := make(chan error, 1)
	go func() {
		tx, err := admin.db.BeginTx(ctx, nil)
		if err != nil {
			bumped <- err
			return
		}
		defer tx.Rollback()
		var e int64
		if err := tx.QueryRow(`SELECT epoch FROM writer_lease WHERE id = 1 FOR UPDATE`).Scan(&e); err != nil {
			bumped <- err
			return
		}
		if _, err := tx.Exec(`UPDATE writer_lease SET epoch = epoch + 1 WHERE id = 1`); err != nil {
			bumped <- err
			return
		}
		bumped <- tx.Commit()
	}()
	select {
	case err := <-bumped:
		t.Fatalf("the bump must wait for the open fenced write (got %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-txDone, "the in-flight write commits: it belongs to the old term")
	require.NoError(t, <-bumped)
	assert.ErrorIs(t, a.SeedSources(ctx, nil), ErrFenced)
}

// The loss path: the lease connection's backend is terminated (what an RDS
// failover or a network cut looks like). The heartbeat notices, the writer
// becomes a reader, and another process takes the lease with a newer epoch.
func TestLeaseLostWhenBackendTerminated(t *testing.T) {
	fastHeartbeat(t)
	ctx := context.Background()
	url := storetest.URL(t)
	a := newReader(t, url)
	lostCh := make(chan error, 1)
	l, err := a.AcquireLease(ctx, LeaseOptions{Holder: "a", Address: "a:1",
		OnLost: func(err error) { lostCh <- err }})
	require.NoError(t, err)
	seedSource(t, a, "usgs")

	admin := newReader(t, url)
	l.connMu.Lock()
	pid := l.conn.PgConn().PID()
	l.connMu.Unlock()
	var ok bool
	require.NoError(t, admin.db.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&ok))
	require.True(t, ok)

	select {
	case err := <-lostCh:
		assert.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the heartbeat did not notice the terminated session")
	}
	assert.False(t, a.HoldsLease())
	assert.ErrorIs(t, a.SeedSources(ctx, nil), ErrNotWriter)

	b := newReader(t, url)
	lb, err := b.AcquireLease(ctx, LeaseOptions{Holder: "b", Address: "b:1", Wait: 2 * time.Second})
	require.NoError(t, err, "the session's death released the lock")
	assert.Equal(t, l.Epoch()+1, lb.Epoch())
	seedSource(t, b, "usgs")

	// a can campaign again once b lets go, and is fenced from b's term until then.
	require.NoError(t, a.ReleaseLease(), "forgetting a lost lease is fine")
	require.NoError(t, b.ReleaseLease())
	la, err := a.AcquireLease(ctx, LeaseOptions{Holder: "a", Wait: 2 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, lb.Epoch()+1, la.Epoch())
	seedSource(t, a, "usgs")
}

// Every process migrates at boot, without the writer lease: a reader opened
// on a database an older release left at the baseline (0001 only) brings it
// to this build's schema, takes no lease and leaves writer_lease's epoch
// alone; a second reader finds nothing to do.
func TestReaderMigratesAnOlderSchema(t *testing.T) {
	ctx := context.Background()
	url := storetest.EmptyURL(t)
	setup, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer setup.Close()
	v, err := migrations.ApplyThrough(ctx, setup, 1)
	require.NoError(t, err)
	require.Equal(t, 1, v, "the fixture is the baseline only")

	r := newReader(t, url)
	assert.False(t, r.HoldsLease())
	v, err = migrations.Current(ctx, r.db)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest(), v)
	st, err := r.Settings(ctx)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest(), st.SchemaVersion)
	w, err := r.WriterLease(ctx)
	require.NoError(t, err)
	assert.Zero(t, w.Epoch, "migrating is not taking the lease")
	assert.Empty(t, w.Holder)

	r2 := newReader(t, url)
	var applied int
	require.NoError(t, r2.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE source = 'postgres'`).Scan(&applied))
	assert.Equal(t, migrations.Latest(), applied, "each version applied once")
}

// Validators on a reader are cached and refreshed by NOTIFY (fast path) or
// the poll (backstop); a reader never answers from a stale value longer than
// the poll interval.
func TestReaderValidatorsFollowTheWriter(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	w, err := Open(url)
	require.NoError(t, err)
	defer w.Close()
	seedSource(t, w, "usgs")
	write := func(id string) {
		t.Helper()
		_, err := w.UpsertEvent(ctx, testEvent(id, 2, 1, id))
		require.NoError(t, err)
	}
	dv := func(s *Store) int64 {
		v, err := s.DataVersion(ctx)
		require.NoError(t, err)
		return v
	}

	t.Run("notify", func(t *testing.T) {
		r := newReader(t, url, WithVersionPoll(time.Hour))
		before := dv(r)
		write("usgs:n1")
		assert.Equal(t, before+1, dv(w), "the writer validates against its own commit at once")
		require.Eventually(t, func() bool { return dv(r) == before+1 }, 2*time.Second, 10*time.Millisecond)
	})
	t.Run("cached without notify or poll", func(t *testing.T) {
		r := newReader(t, url, WithVersionPoll(time.Hour), WithoutVersionListen())
		before := dv(r)
		write("usgs:c1")
		time.Sleep(200 * time.Millisecond)
		assert.Equal(t, before, dv(r), "no notification and no poll: the cached value stands")
	})
	t.Run("poll backstop", func(t *testing.T) {
		poll := 300 * time.Millisecond
		r := newReader(t, url, WithVersionPoll(poll), WithoutVersionListen())
		before := dv(r)
		write("usgs:p1")
		start := time.Now()
		require.Eventually(t, func() bool { return dv(r) == before+1 }, 2*time.Second, 10*time.Millisecond)
		assert.Less(t, time.Since(start), poll+200*time.Millisecond, "within one poll interval")
	})
	t.Run("refresh and mesh counters", func(t *testing.T) {
		r := newReader(t, url, WithVersionPoll(time.Hour))
		mv, err := r.MeshActivityVersion(ctx)
		require.NoError(t, err)
		require.NoError(t, w.RecordMeshTick(ctx, nil, []MeshActivity{{PubKey: "AB", LastHeardAt: baseTime}}))
		require.Eventually(t, func() bool {
			v, err := r.MeshActivityVersion(ctx)
			return err == nil && v == mv+1
		}, 2*time.Second, 10*time.Millisecond)
		rv, err := r.RefreshVersion(ctx, []gridv1.Layer{gridv1.Layer_EARTHQUAKE})
		require.NoError(t, err)
		assert.Zero(t, rv)
	})
}

// Release waits for a write in flight; nothing commits after it returns.
func TestReleaseWaitsForInFlightWrite(t *testing.T) {
	ctx := context.Background()
	a, err := Open(storetest.URL(t))
	require.NoError(t, err)
	defer a.Close()
	seedSource(t, a, "usgs")
	inside, finish := make(chan struct{}), make(chan struct{})
	txErr := make(chan error, 1)
	go func() {
		txErr <- a.inTx(ctx, func(tx *sql.Tx) error { close(inside); <-finish; return nil })
	}()
	<-inside
	released := make(chan error, 1)
	go func() { released <- a.ReleaseLease() }()
	select {
	case <-released:
		t.Fatal("release must wait for the write in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(finish)
	require.NoError(t, <-txErr)
	require.NoError(t, <-released)
	assert.True(t, errors.Is(a.SeedSources(ctx, nil), ErrNotWriter))
}
