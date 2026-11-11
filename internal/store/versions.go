package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// Change counters for ETag validators (the versions table).
//
// They used to be process memory (a COUNT(*) over event_revisions, per-layer
// refresh counters, a mesh-activity counter, a places counter). With more
// than one process serving reads, a memory counter answers wrong 304s: a
// reader that never saw the write would hand out the validator of a body it
// no longer serves. So each is now a row bumped in the SAME transaction as the
// write it describes. A reader that sees the new value is guaranteed to see
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
	var v int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT value FROM versions WHERE key = $1), 0)`, key).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: version %s: %w", key, err)
	}
	return v, nil
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
	var v int64
	if len(layers) == 0 {
		err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(value), 0)::bigint FROM versions
			WHERE key LIKE 'refresh:%'`).Scan(&v)
		if err != nil {
			return 0, fmt.Errorf("store: refresh version: %w", err)
		}
		return v, nil
	}
	keys := make([]string, 0, len(layers))
	for _, l := range slices.Compact(slices.Sorted(slices.Values(layers))) {
		keys = append(keys, refreshKey(l))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(value), 0)::bigint FROM versions
		WHERE key = ANY($1)`, keys).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: refresh version: %w", err)
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
