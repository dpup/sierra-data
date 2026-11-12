package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// Change counters for ETag validators (the versions table).
//
// They used to be process memory (a COUNT(*) over event_revisions, per-layer
// refresh counters, a mesh-activity counter, a places counter). With more
// than one process serving reads, a memory counter answers wrong 304s: a
// reader that never saw the write would hand out the validator of a body it
// no longer serves. So each is now a row bumped in the SAME transaction as the
// write it describes, and readers cache the table (versionCache: refreshed on
// their own commits, on NOTIFY, and by a backstop poll). A reader that sees the new value is guaranteed to see
// the new body; one that reads the old value and then the new body caches the
// new body under the old tag for one request, which costs a 200, never a
// stale 304.
const (
	versionRevisions    = "revisions"
	versionMeshActivity = "mesh_activity"
	versionPlaces       = "places"
	versionRefreshPfx   = "refresh:"
)

func refreshKey(l gridv1.Layer) string { return versionRefreshPfx + l.String() }

// bumpVersion adds n to a counter inside the caller's write transaction.
func bumpVersion(tx *sql.Tx, key string, n int64) error {
	if _, err := tx.Exec(`
		INSERT INTO versions (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = versions.value + excluded.value`, key, n); err != nil {
		return fmt.Errorf("store: bump version %s: %w", key, err)
	}
	return nil
}

func (s *Store) readVersion(ctx context.Context, key string) (int64, error) {
	vals, err := s.versions.get(ctx, s.db)
	if err != nil {
		return 0, err
	}
	return vals[key], nil
}

// versionCache holds the versions table in memory, so a validator costs no
// query. It is marked dirty (and re-read by the next validator call) when:
//   - this process commits a write (inTx), so a writer always validates
//     against its own latest commit;
//   - a notification arrives on the grid_versions channel (migration 0002's
//     trigger: every versions change, delivered at commit), the fast path for
//     a reader;
//   - the backstop poll ticks (DefaultVersionPoll), which bounds how long a
//     reader with a broken listener can answer a stale 304.
//
// Reading a snapshot older than the body is harmless (the body is cached
// under the older tag and the next request misses); the poll bounds the
// opposite case, a 304 for a body that has moved on.
type versionCache struct {
	mu    sync.Mutex
	dirty bool
	vals  map[string]int64 // replaced wholesale, never mutated
}

func (c *versionCache) markDirty() {
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
}

func (c *versionCache) get(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vals != nil && !c.dirty {
		return c.vals, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM versions`)
	if err != nil {
		return nil, fmt.Errorf("store: reading versions: %w", err)
	}
	defer rows.Close()
	vals := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: reading versions: %w", err)
		}
		vals[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading versions: %w", err)
	}
	c.vals, c.dirty = vals, false
	return vals, nil
}

// versionsChannel is the NOTIFY channel migration 0002's trigger uses.
const versionsChannel = "grid_versions"

// startVersionWatch runs the poll (and, with listen, the LISTEN connection)
// until Close. The poll also re-checks the schema version while it is behind,
// so a reader turns ready once a writer has migrated.
func (s *Store) startVersionWatch(poll time.Duration, listen bool) {
	ctx, cancel := context.WithCancel(context.Background())
	s.watchStop, s.watchDone = cancel, make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.versions.markDirty()
			if !s.schemaReady.Load() {
				_, _ = s.RefreshSchema(ctx)
			}
		}
	}()
	if listen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.listenVersions(ctx)
		}()
	}
	go func() { wg.Wait(); close(s.watchDone) }()
}

// listenVersions holds a dedicated connection LISTENing on the versions
// channel and marks the cache dirty on every notification. On a broken
// connection it marks the cache dirty (a notification may have been missed)
// and reconnects with backoff; the poll covers the gap.
func (s *Store) listenVersions(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.listenOnce(ctx)
		s.versions.markDirty()
		if ctx.Err() != nil {
			return
		}
		log.Printf("store: versions listener: %v; reconnecting in %s (the %s poll covers the gap)",
			err, backoff, DefaultVersionPoll)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Store) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+versionsChannel); err != nil {
		return err
	}
	s.versions.markDirty() // anything committed before LISTEN took effect
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		s.versions.markDirty()
	}
}

// DataVersion is a monotonically increasing count of event revisions written
// (the 'revisions' counter: every content change and every lifecycle
// transition bumps it, in the transaction that writes the revision). A query's
// result cannot change without a revision write, so an unchanged count means
// an unchanged result, which makes it a safe conservative validator for the
// cross-event list endpoints. One indexed row read; it replaced a COUNT(*)
// over the whole revision history.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	return s.readVersion(ctx, versionRevisions)
}

// RefreshVersion changes whenever the blob of an event in any of the given
// layers (all layers when none are given) is rewritten in place without a
// revision (the hash-equal refresh path). DataVersion cannot see those
// rewrites, so list validators fold this in. Per layer so a mesh node's
// telemetry refresh does not invalidate a road-incident list.
func (s *Store) RefreshVersion(ctx context.Context, layers []gridv1.Layer) (int64, error) {
	vals, err := s.versions.get(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("store: refresh version: %w", err)
	}
	var v int64
	if len(layers) == 0 {
		for k, n := range vals {
			if strings.HasPrefix(k, versionRefreshPfx) {
				v += n
			}
		}
		return v, nil
	}
	for _, l := range slices.Compact(slices.Sorted(slices.Values(layers))) {
		v += vals[refreshKey(l)]
	}
	return v, nil
}

// MeshActivityVersion changes whenever mesh_activity is written. Read-path
// ETag validators fold it in for responses that carry mesh activity: the
// table moves without minting a revision, so DataVersion cannot see it.
func (s *Store) MeshActivityVersion(ctx context.Context) (int64, error) {
	return s.readVersion(ctx, versionMeshActivity)
}

// PlacesVersion changes whenever the place set changes. It is the signal that
// derived event->place attachments need recomputing; see the ingest
// scheduler's full-reconcile pass.
func (s *Store) PlacesVersion(ctx context.Context) (int64, error) {
	return s.readVersion(ctx, versionPlaces)
}
