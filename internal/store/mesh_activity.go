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

// meshActivityReadChunk bounds one = ANY($1) array.
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
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertMeshObservations(ctx, tx, obs); err != nil {
			return err
		}
		if len(act) == 0 {
			return nil
		}
		if err := upsertMeshActivity(ctx, tx, act); err != nil {
			return err
		}
		return bumpVersion(tx, versionMeshActivity, 1)
	})
}

// upsertMeshActivity merges the batch in one INSERT ... ON CONFLICT over
// unnest. A statement may not touch a row twice, so repeated keys are folded
// first: the per-clock MAX of the batch, which is what applying the rows one
// by one through GREATEST produced.
func upsertMeshActivity(ctx context.Context, tx *sql.Tx, act []MeshActivity) error {
	type clocks struct{ heard, relayed int64 }
	folded := make(map[string]*clocks, len(act))
	order := make([]string, 0, len(act))
	for _, a := range act {
		pk := strings.ToLower(a.PubKey)
		if pk == "" {
			continue
		}
		c := folded[pk]
		if c == nil {
			c = &clocks{}
			folded[pk] = c
			order = append(order, pk)
		}
		c.heard = max(c.heard, unixOrZero(a.LastHeardAt))
		c.relayed = max(c.relayed, unixOrZero(a.LastRelayedAt))
	}
	if len(order) == 0 {
		return nil
	}
	heard, relayed := make([]int64, len(order)), make([]int64, len(order))
	for i, pk := range order {
		heard[i], relayed[i] = folded[pk].heard, folded[pk].relayed
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mesh_activity (pubkey, last_heard_at, last_relayed_at)
		SELECT pubkey, last_heard_at, last_relayed_at
		FROM unnest($1::text[], $2::bigint[], $3::bigint[]) AS t(pubkey, last_heard_at, last_relayed_at)
		ON CONFLICT (pubkey) DO UPDATE SET
		  last_heard_at   = GREATEST(mesh_activity.last_heard_at, excluded.last_heard_at),
		  last_relayed_at = GREATEST(mesh_activity.last_relayed_at, excluded.last_relayed_at)`,
		order, heard, relayed); err != nil {
		return fmt.Errorf("store: upsert %d mesh activity rows: %w", len(order), err)
	}
	return nil
}

// MeshActivityFor returns the activity on record for each of the given public
// keys (case-insensitive). Keys with no row are absent from the map.
func (s *Store) MeshActivityFor(ctx context.Context, pubkeys []string) (map[string]MeshActivity, error) {
	seen := make(map[string]bool, len(pubkeys))
	keys := make([]string, 0, len(pubkeys))
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
			`SELECT pubkey, last_heard_at, last_relayed_at FROM mesh_activity WHERE pubkey = ANY($1)`,
			chunk)
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
