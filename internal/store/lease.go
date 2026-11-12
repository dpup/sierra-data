package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sierragridteam/sierra-data/internal/store/migrations"
)

// The writer lease: which ONE process may write a database.
//
// Every process opens the store as a reader (OpenReader) and serves reads.
// One process at a time holds the writer lease, which has two parts:
//
//   - A session advisory lock (pg_try_advisory_lock) on a DEDICATED
//     connection, taken from the pool with Acquire and then Hijack()ed out of
//     it, never a pooled connection: a session lock belongs to the backend that
//     took it, and a pooled connection would carry it into whatever borrowed
//     the connection next. The connection dying releases the lock server-side,
//     so a hard-killed task never leaves the database locked. This is the
//     mutual exclusion.
//   - The writer_lease row (migration 0002): an epoch the new holder bumps,
//     its holder id and address, and a heartbeat. Every write transaction
//     checks the epoch (inTx's fence), which is what the lock alone cannot
//     give: a process whose session died (RDS failover, a network cut) may not
//     know yet, and its pooled connections would happily keep writing. With
//     the fence, the first write after the next holder's bump fails.
//
// Why the fence is SELECT ... FOR KEY SHARE and the bump SELECT ... FOR
// UPDATE: a write transaction holds its key-share lock until it ends, and the
// next holder's FOR UPDATE waits for it, so a stale writer's transaction that
// passed the check commits BEFORE the new epoch exists (it is the old term's
// last write, not a write into the new term), and every later one fails the
// check. Key-share does not conflict with the holder's own heartbeat UPDATE
// (a no-key update), so a long write transaction never stalls the heartbeat,
// and the fence writes no row version.
//
// Lease loss: the heartbeat (every 2 s on the lease connection) updates
// heartbeat_at WHERE epoch = mine; an error (the session is gone) or zero rows
// (someone bumped the epoch) marks the lease lost, closes the connection and
// calls OnLost. From that moment inTx refuses (ErrNotWriter). Dropping the
// writers and re-campaigning is the caller's job (cmd/server).

// writerLockKey is the pg_try_advisory_lock key for the Grid writer. Advisory
// locks are scoped to the current database, so separate databases (one per
// test) never contend.
const writerLockKey int64 = 0x5349455252414731 // "SIERRAG1"

// leaseHeartbeat is how often the holder refreshes the lease row and so
// proves its session is alive; a variable for tests.
var leaseHeartbeat = 2 * time.Second

// LeaseStaleAfter is how long a holder may be silent (no heartbeat) before a
// reader treats the lease as having no live writer (WriterLease). Five missed
// heartbeats: generous, because the cost of a wrong "no writer" is a 503 the
// pusher retries, and of a wrong "live" a forward that fails the same way.
const LeaseStaleAfter = 10 * time.Second

// leasePoll is how often a campaign retries pg_try_advisory_lock.
var leasePoll = 250 * time.Millisecond

// DefaultLockTimeout bounds how long Open (and a coordinator-driven
// activation) waits for the writer lease before failing. The contention case
// is a deploy whose previous writer has not released yet.
const DefaultLockTimeout = 90 * time.Second

// lockAcquireTimeout is the effective default for Open, overridden per Open
// with WithLockTimeout, and by tests.
var lockAcquireTimeout = DefaultLockTimeout

var (
	// ErrLockHeld is wrapped by AcquireLease's (and Open's) error when another
	// process holds the writer lease for the whole wait.
	ErrLockHeld = errors.New("store: the grid writer lock is held by another process")
	// ErrNotWriter is returned by every write when this process does not hold
	// a live writer lease: a reader, a lost lease, a released one.
	ErrNotWriter = errors.New("store: this process does not hold the writer lease")
	// ErrFenced wraps ErrNotWriter: a write found a newer epoch in
	// writer_lease. Another process has taken the lease.
	ErrFenced = fmt.Errorf("%w (fenced: another process has taken it since)", ErrNotWriter)
	// errLeaseReleased is a lease's terminal error after Release.
	errLeaseReleased = errors.New("store: writer lease released")
)

// LeaseOptions configure AcquireLease.
type LeaseOptions struct {
	// Holder identifies the process in writer_lease.holder (cmd/server: the
	// handoff instance id and hostname).
	Holder string
	// Address is where other processes reach this one (writer_lease.address;
	// push ingest on a reader forwards there). Empty records none.
	Address string
	// Wait bounds how long to wait for the lock; zero or negative waits until
	// ctx ends (a background campaign).
	Wait time.Duration
	// OnLost, when set, is called once, from a background goroutine, if the
	// lease is lost after it was acquired (not on Release).
	OnLost func(error)
}

// Lease is a held writer lease. Lost is closed once it is lost or released.
type Lease struct {
	s      *Store
	epoch  int64
	holder string

	connMu sync.Mutex // serializes use of conn (a pgx.Conn is not concurrency-safe)
	conn   *pgx.Conn

	endOnce sync.Once
	ended   chan struct{}
	endErr  error // valid once ended is closed
	onLost  func(error)

	stop chan struct{}
	done chan struct{} // heartbeat goroutine exited
}

// Epoch is the lease's epoch: the writer_lease.epoch value this term wrote.
func (l *Lease) Epoch() int64 { return l.epoch }

// Lost is closed when the lease ends: lost (Err says why) or released.
func (l *Lease) Lost() <-chan struct{} { return l.ended }

// Err is nil while the lease is live, then why it ended.
func (l *Lease) Err() error {
	select {
	case <-l.ended:
		return l.endErr
	default:
		return nil
	}
}

func (l *Lease) live() bool { return l != nil && l.Err() == nil }

// end records the lease's end once. lost reports a loss (OnLost fires and the
// connection is closed, which releases the lock server-side if it was not
// already gone); otherwise it is a release, which closes the connection
// itself after unlocking.
func (l *Lease) end(err error, lost bool) {
	l.endOnce.Do(func() {
		l.endErr = err
		close(l.ended)
		if !lost {
			return
		}
		log.Printf("store: LOST the writer lease (epoch %d): %v", l.epoch, err)
		go func() {
			l.connMu.Lock()
			if l.conn != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = l.conn.Close(ctx)
				cancel()
			}
			l.connMu.Unlock()
			if l.onLost != nil {
				l.onLost(err)
			}
		}()
	})
}

// AcquireLease makes this store the writer: it waits for the advisory lock
// (up to opts.Wait, or until ctx ends), fences the previous term out by
// bumping writer_lease.epoch, applies pending migrations, and records the
// holder and address. From then until the lease ends, inTx accepts writes.
//
// The epoch is bumped BEFORE migrating when the table exists, so a previous
// holder that has lost its session but not noticed is fenced out before any
// DDL runs; on a database without writer_lease yet (empty, or schema 1) it is
// bumped right after the migration that creates it.
func (s *Store) AcquireLease(ctx context.Context, opts LeaseOptions) (*Lease, error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.lease.Load().live() {
		return nil, errors.New("store: this store already holds the writer lease")
	}
	conn, waited, err := acquireAdvisoryLock(ctx, s, opts.Wait)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Lease, error) {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, writerLockKey)
		_ = conn.Close(uctx)
		return nil, err
	}

	epoch, err := bumpEpoch(ctx, conn, opts.Holder, opts.Address)
	bumped := err == nil
	if err != nil && !isUndefinedTable(err) {
		return fail(err)
	}
	v, err := migrations.Apply(ctx, s.db)
	if err == nil {
		v, err = migrations.CheckVersion(ctx, s.db)
	}
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	s.schemaVersion.Store(int64(v))
	s.schemaReady.Store(true)
	if !bumped {
		if epoch, err = bumpEpoch(ctx, conn, opts.Holder, opts.Address); err != nil {
			return fail(err)
		}
	}

	l := &Lease{s: s, epoch: epoch, holder: opts.Holder, conn: conn, onLost: opts.OnLost,
		ended: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	// A new term: anything cached under a previous one is suspect (another
	// writer may have changed places meanwhile).
	s.mu.Lock()
	s.placesGeoValid = false
	s.lease.Store(l)
	s.mu.Unlock()
	s.versions.markDirty()
	go l.heartbeat()
	if waited > 0 {
		// The line that tells "the deploy was slow because the previous
		// writer was draining" apart from every other slow start.
		log.Printf("store: acquired the writer lease (epoch %d) after waiting %s", epoch,
			waited.Round(100*time.Millisecond))
	}
	return l, nil
}

// acquireAdvisoryLock takes the writer lock on a dedicated connection,
// polling pg_try_advisory_lock until it succeeds, wait elapses (wait > 0) or
// ctx ends. It returns how long it waited when it had to.
func acquireAdvisoryLock(ctx context.Context, s *Store, wait time.Duration) (*pgx.Conn, time.Duration, error) {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("store: connecting for the writer lock: %w", err)
	}
	conn := pc.Hijack() // ours alone from here; the pool forgets it
	start := time.Now()
	announced := false
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, writerLockKey).Scan(&ok); err != nil {
			conn.Close(context.Background())
			if ctx.Err() != nil {
				return nil, 0, fmt.Errorf("store: stopped waiting for the writer lock: %w", ctx.Err())
			}
			return nil, 0, fmt.Errorf("store: taking the writer lock: %w", err)
		}
		if ok {
			if announced {
				return conn, time.Since(start), nil
			}
			return conn, 0, nil
		}
		if wait > 0 && time.Since(start) >= wait {
			conn.Close(context.Background())
			return nil, 0, fmt.Errorf("%w: still held after %s. On a deploy this means the previous "+
				"writer has not released it yet (check that it was quiesced or yielded); otherwise stop "+
				"the other server or point PF__GRID__DATABASE__URL at a different database", ErrLockHeld, wait)
		}
		if !announced && wait > 0 {
			log.Printf("store: the writer lock is held by another process; waiting up to %s for it", wait)
		}
		announced = true
		select {
		case <-ctx.Done():
			conn.Close(context.Background())
			return nil, 0, fmt.Errorf("store: stopped waiting for the writer lock: %w", ctx.Err())
		case <-time.After(leasePoll):
		}
	}
}

// bumpEpoch starts a new term on the lease connection. FOR UPDATE first: it
// waits for every open fenced write of the previous term (they hold key-share
// locks), which is what makes the fence airtight.
func bumpEpoch(ctx context.Context, conn *pgx.Conn, holder, address string) (int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: lease: begin: %w", err)
	}
	defer tx.Rollback(context.Background())
	var cur int64
	err = tx.QueryRow(ctx, `SELECT epoch FROM writer_lease WHERE id = 1 FOR UPDATE`).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `INSERT INTO writer_lease (id) VALUES (1) ON CONFLICT DO NOTHING`); err == nil {
			err = tx.QueryRow(ctx, `SELECT epoch FROM writer_lease WHERE id = 1 FOR UPDATE`).Scan(&cur)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("store: lease: reading the epoch: %w", err)
	}
	var epoch int64
	if err := tx.QueryRow(ctx, `UPDATE writer_lease SET epoch = epoch + 1, holder = $1, address = $2,
		acquired_at = now(), heartbeat_at = now() WHERE id = 1 RETURNING epoch`, holder, address).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("store: lease: bumping the epoch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: lease: commit: %w", err)
	}
	return epoch, nil
}

func isUndefinedTable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42P01"
}

// heartbeat refreshes the lease row until the lease ends. An error (the
// session is gone, and with it the lock) or zero rows (the epoch moved on)
// is a loss.
func (l *Lease) heartbeat() {
	defer close(l.done)
	t := time.NewTicker(leaseHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-l.ended:
			return
		case <-t.C:
		}
		l.connMu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), leaseHeartbeat)
		tag, err := l.conn.Exec(ctx,
			`UPDATE writer_lease SET heartbeat_at = now() WHERE id = 1 AND epoch = $1`, l.epoch)
		cancel()
		l.connMu.Unlock()
		switch {
		case err != nil:
			l.end(fmt.Errorf("store: writer lease heartbeat failed (the session and its lock are gone): %w", err), true)
			return
		case tag.RowsAffected() == 0:
			l.end(ErrFenced, true)
			return
		}
	}
}

// ReleaseLease ends this store's writer lease, if it holds one: refuses new
// writes, waits for an in-flight write transaction, clears holder and address
// (so readers stop forwarding here at once), unlocks and closes the lease
// connection. The epoch is left for the next holder to bump. The store stays
// open as a reader. A lost lease is simply forgotten.
func (s *Store) ReleaseLease() error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	l := s.lease.Load()
	if l == nil {
		return nil
	}
	s.mu.Lock() // an in-flight inTx finishes first; later ones see the lease gone
	wasLive := l.live()
	l.end(errLeaseReleased, false)
	s.lease.Store(nil)
	s.mu.Unlock()
	close(l.stop)
	<-l.done
	if !wasLive {
		return nil // lost: end() closed the connection
	}
	l.connMu.Lock()
	defer l.connMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.conn.Exec(ctx, `UPDATE writer_lease SET holder = '', address = '' WHERE id = 1 AND epoch = $1`, l.epoch)
	if _, uerr := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, writerLockKey); err == nil {
		err = uerr
	}
	if cerr := l.conn.Close(ctx); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("store: releasing the writer lease: %w", err)
	}
	return nil
}

// HoldsLease reports whether this store holds a live writer lease.
func (s *Store) HoldsLease() bool { return s.lease.Load().live() }

// CurrentLease is the held lease, or nil.
func (s *Store) CurrentLease() *Lease {
	if l := s.lease.Load(); l.live() {
		return l
	}
	return nil
}

// WriterInfo is the writer_lease row as a reader sees it.
type WriterInfo struct {
	Epoch        int64
	Holder       string
	Address      string
	HeartbeatAge time.Duration // since the holder's last heartbeat (database clock)
	// Live: a holder and address are recorded and the heartbeat is younger
	// than LeaseStaleAfter.
	Live bool
}

// WriterLease reads the lease row (database clock for the age, so task clock
// skew does not matter).
func (s *Store) WriterLease(ctx context.Context) (WriterInfo, error) {
	var w WriterInfo
	var ageSec float64
	err := s.db.QueryRowContext(ctx, `SELECT epoch, holder, address,
		COALESCE(EXTRACT(EPOCH FROM now() - heartbeat_at), 1e9)::float8
		FROM writer_lease WHERE id = 1`).Scan(&w.Epoch, &w.Holder, &w.Address, &ageSec)
	if err != nil {
		return w, fmt.Errorf("store: reading the writer lease: %w", err)
	}
	w.HeartbeatAge = time.Duration(ageSec * float64(time.Second))
	w.Live = w.Holder != "" && w.Address != "" && w.HeartbeatAge < LeaseStaleAfter
	return w, nil
}

// defaultHolder names a process that opened the store with Open (tools,
// tests): hostname and pid.
func defaultHolder() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s/%d", h, os.Getpid())
}
