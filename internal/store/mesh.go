package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// secondsPerDay truncates a heard_at unix time to its UTC day bucket.
	secondsPerDay = 86400
	// maxCompactBatch bounds one compaction transaction so a large backlog never
	// holds the single writer for long; CompactMeshObservations loops over chunks.
	maxCompactBatch = 5000
	// watermarkKey is the mesh_meta row tracking the last observation id compacted.
	watermarkKey = "compaction_watermark"
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

// edgeKey identifies one undirected link on one day (canonical a < b).
type edgeKey struct {
	a, b   string
	bucket int64
}

// edgeAgg accumulates a link's stats within a compaction chunk.
type edgeAgg struct {
	observations int
	bestSNR      float64
	firstSeen    int64
	lastSeen     int64
}

// CompactMeshObservations folds new raw receptions (Tier 0) into the per-link-
// per-day rollup (Tier 1). It reads observations past the stored watermark in
// bounded chunks — each chunk its own transaction so a large backlog never holds
// the single writer for long — explodes each [pubkey, ...pathNodes] chain into
// canonical undirected edges (skipping unresolved hops), and upserts the day
// bucket. Idempotent: the watermark advances only on commit, so a crash re-runs
// the same chunk. Returns the number of observations processed.
func (s *Store) CompactMeshObservations(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.compactChunk(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < maxCompactBatch {
			return total, nil
		}
	}
}

func (s *Store) compactChunk(ctx context.Context) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		watermark, err := readWatermark(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, pubkey, heard_at, snr, path_nodes
			FROM mesh_observations WHERE id > ? ORDER BY id LIMIT ?`,
			watermark, maxCompactBatch)
		if err != nil {
			return fmt.Errorf("store: reading observations for compaction: %w", err)
		}
		agg := map[edgeKey]*edgeAgg{}
		var maxID int64
		for rows.Next() {
			var id, heardAt int64
			var pubkey, pathNodes string
			var snr sql.NullFloat64
			if err := rows.Scan(&id, &pubkey, &heardAt, &snr, &pathNodes); err != nil {
				rows.Close()
				return fmt.Errorf("store: scanning observation: %w", err)
			}
			maxID = id
			n++
			// The advert's relay chain is the origin then each resolved hop. SNR is
			// our receiver's reading of the whole relayed packet, so attributing it
			// to every edge is a coarse link-quality proxy (documented approximation).
			bucket := heardAt - (heardAt % secondsPerDay)
			chain := append([]string{pubkey}, splitList(pathNodes)...)
			for i := 0; i+1 < len(chain); i++ {
				a, b := chain[i], chain[i+1]
				if a == "" || b == "" || a == b {
					continue // unresolved hop or self-loop — no edge
				}
				if a > b {
					a, b = b, a
				}
				k := edgeKey{a: a, b: b, bucket: bucket}
				e := agg[k]
				if e == nil {
					e = &edgeAgg{bestSNR: snr.Float64, firstSeen: heardAt, lastSeen: heardAt}
					agg[k] = e
				}
				e.observations++
				if snr.Valid && snr.Float64 > e.bestSNR {
					e.bestSNR = snr.Float64
				}
				if heardAt < e.firstSeen {
					e.firstSeen = heardAt
				}
				if heardAt > e.lastSeen {
					e.lastSeen = heardAt
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("store: iterating observations: %w", err)
		}
		rows.Close()
		if n == 0 {
			return nil // caught up
		}
		if len(agg) > 0 {
			stmt, err := tx.PrepareContext(ctx, `
				INSERT INTO mesh_link_rollup
				  (a_pubkey, b_pubkey, bucket, observations, best_snr, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(a_pubkey, b_pubkey, bucket) DO UPDATE SET
				  observations = observations + excluded.observations,
				  best_snr     = max(best_snr, excluded.best_snr),
				  first_seen   = min(first_seen, excluded.first_seen),
				  last_seen    = max(last_seen, excluded.last_seen)`)
			if err != nil {
				return fmt.Errorf("store: prepare rollup upsert: %w", err)
			}
			defer stmt.Close()
			for k, e := range agg {
				if _, err := stmt.ExecContext(ctx,
					k.a, k.b, k.bucket, e.observations, e.bestSNR, e.firstSeen, e.lastSeen,
				); err != nil {
					return fmt.Errorf("store: upsert rollup edge %s|%s: %w", k.a, k.b, err)
				}
			}
		}
		// Advance the watermark past every observation scanned (including ones with
		// no resolvable edge — they must not be re-scanned each tick).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mesh_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, watermarkKey, maxID); err != nil {
			return fmt.Errorf("store: advancing compaction watermark: %w", err)
		}
		return nil
	})
	return n, err
}

// readWatermark returns the last compacted observation id (0 if none yet).
func readWatermark(ctx context.Context, tx *sql.Tx) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `SELECT value FROM mesh_meta WHERE key = ?`, watermarkKey).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: reading compaction watermark: %w", err)
	}
	return v, nil
}

// PruneMeshObservations deletes raw receptions older than cutoff that have
// already been compacted (id <= the watermark). Gating on the watermark ensures a
// stalled compaction never drops un-rolled-up data. Returns rows removed.
func (s *Store) PruneMeshObservations(ctx context.Context, cutoff time.Time) (int64, error) {
	var affected int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		watermark, err := readWatermark(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM mesh_observations WHERE heard_at < ? AND id <= ?`, cutoff.Unix(), watermark)
		if err != nil {
			return fmt.Errorf("store: pruning observations: %w", err)
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	return affected, err
}

// PruneMeshLinkRollup deletes rollup buckets older than cutoff (by day). Returns
// rows removed.
func (s *Store) PruneMeshLinkRollup(ctx context.Context, cutoff time.Time) (int64, error) {
	var affected int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM mesh_link_rollup WHERE bucket < ?`, cutoff.Unix())
		if err != nil {
			return fmt.Errorf("store: pruning rollup: %w", err)
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	return affected, err
}

// splitList splits a comma-joined hop list, mapping "" to nil (an empty
// path_nodes yields no hops, not one blank hop).
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
