package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MeshObservation is one received MeshCore advert — an immutable measurement,
// NOT event content. Stored append-only in mesh_observations (Tier 0 of the
// relay-topology model, docs/mesh-topology-design.md); compaction later rolls
// these into mesh_link_rollup and prunes the raw rows.
//
// HeardAt is OUR receive time — the trustworthy clock. Node-reported advert
// stamps are frequently skewed (we have seen them months in the future), so they
// never anchor ordering here.
type MeshObservation struct {
	PubKey    string
	HeardAt   time.Time
	Broker    string    // MQTT server URL the advert arrived on
	Gateway   string    // origin/gateway that reported it
	SNR       float64
	RSSI      int32
	HopCount  uint32
	Path      []string // per-hop repeater pubkey-prefix hashes (hex), as received
	PathNodes []string // Path resolved to full pubkeys; "" where a hop was unresolved
}

// InsertMeshObservations appends a batch of receptions in one transaction. The
// scheduler drains the registry's in-memory buffer once per tick and flushes it
// here on the single writer goroutine — never a write per packet, so
// single-writer discipline holds. Path/PathNodes are stored comma-joined (the
// hop lists, parallel; compaction splits them back). An empty batch is a no-op.
func (s *Store) InsertMeshObservations(ctx context.Context, obs []MeshObservation) error {
	if len(obs) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO mesh_observations
			  (pubkey, heard_at, broker, gateway, snr, rssi, hop_count, path, path_nodes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: prepare mesh observation insert: %w", err)
		}
		defer stmt.Close()
		for _, o := range obs {
			if _, err := stmt.ExecContext(ctx,
				o.PubKey, o.HeardAt.Unix(), o.Broker, o.Gateway,
				o.SNR, o.RSSI, o.HopCount,
				strings.Join(o.Path, ","), strings.Join(o.PathNodes, ","),
			); err != nil {
				return fmt.Errorf("store: insert mesh observation %s: %w", o.PubKey, err)
			}
		}
		return nil
	})
}

// CountMeshObservations returns the number of rows in mesh_observations. A test
// and diagnostic aid (the write path has no reader yet); cheap enough for the
// volumes involved.
func (s *Store) CountMeshObservations(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mesh_observations`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting mesh observations: %w", err)
	}
	return n, nil
}
