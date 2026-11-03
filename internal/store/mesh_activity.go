package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MeshActivity is one node's activity clocks, both OUR receive times (see
// migrationV9). A zero time means none on record.
type MeshActivity struct {
	PubKey string // full public key, lowercase hex
	// LastHeardAt: the node's own last advert reached us.
	LastHeardAt time.Time
	// LastRelayedAt: a flood packet naming the node as a relay hop reached us.
	LastRelayedAt time.Time
}

// meshActivityReadChunk bounds one IN (...) list, well under SQLite's
// host-parameter limit.
const meshActivityReadChunk = 500

// RecordMeshTick writes one mesh poll's measurements in ONE transaction: the
// reception batch (Tier 0) and the activity advances. One commit, not two: on
// EFS every commit invalidates every reader connection's page cache, and the
// observation insert already commits on nearly every tick.
//
// Activity merges by MAX per clock, so a write can never move a node's time
// backwards: a replay after a restart, or a tick that lost a race, is a no-op
// rather than a regression. A zero time leaves that clock alone. Empty
// batches are a no-op that opens no transaction.
func (s *Store) RecordMeshTick(ctx context.Context, obs []MeshObservation, act []MeshActivity) error {
	if len(obs) == 0 && len(act) == 0 {
		return nil
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertMeshObservations(ctx, tx, obs); err != nil {
			return err
		}
		return upsertMeshActivity(ctx, tx, act)
	})
	if err == nil && len(act) > 0 {
		s.meshActivityVersion.Add(1)
	}
	return err
}

func upsertMeshActivity(ctx context.Context, tx *sql.Tx, act []MeshActivity) error {
	if len(act) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO mesh_activity (pubkey, last_heard_at, last_relayed_at)
		VALUES (?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
		  last_heard_at   = MAX(last_heard_at, excluded.last_heard_at),
		  last_relayed_at = MAX(last_relayed_at, excluded.last_relayed_at)`)
	if err != nil {
		return fmt.Errorf("store: prepare mesh activity upsert: %w", err)
	}
	defer stmt.Close()
	for _, a := range act {
		pk := strings.ToLower(a.PubKey)
		if pk == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, pk, unixOrZero(a.LastHeardAt), unixOrZero(a.LastRelayedAt)); err != nil {
			return fmt.Errorf("store: upsert mesh activity %s: %w", pk, err)
		}
	}
	return nil
}

// MeshActivityFor returns the activity on record for each of the given public
// keys (case-insensitive). Keys with no row are absent from the map.
func (s *Store) MeshActivityFor(ctx context.Context, pubkeys []string) (map[string]MeshActivity, error) {
	seen := make(map[string]bool, len(pubkeys))
	keys := make([]any, 0, len(pubkeys))
	for _, pk := range pubkeys {
		pk = strings.ToLower(pk)
		if pk != "" && !seen[pk] {
			seen[pk] = true
			keys = append(keys, pk)
		}
	}
	out := make(map[string]MeshActivity, len(keys))
	for start := 0; start < len(keys); start += meshActivityReadChunk {
		chunk := keys[start:min(start+meshActivityReadChunk, len(keys))]
		rows, err := s.db.QueryContext(ctx,
			`SELECT pubkey, last_heard_at, last_relayed_at FROM mesh_activity WHERE pubkey IN (`+placeholders(len(chunk))+`)`,
			chunk...)
		if err != nil {
			return nil, fmt.Errorf("store: reading mesh activity: %w", err)
		}
		for rows.Next() {
			var pk string
			var heard, relayed int64
			if err := rows.Scan(&pk, &heard, &relayed); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scanning mesh activity: %w", err)
			}
			out[pk] = MeshActivity{PubKey: pk, LastHeardAt: timeOrZero(heard), LastRelayedAt: timeOrZero(relayed)}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("store: reading mesh activity: %w", err)
		}
	}
	return out, nil
}

// MeshActivityVersion changes whenever mesh_activity is written by this
// process. An ETag input only: it is meaningful compared within one process
// (validators also carry a per-boot nonce), never as a count of anything.
func (s *Store) MeshActivityVersion() int64 {
	return s.meshActivityVersion.Load()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}
