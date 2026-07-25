package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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

// MeshNodeHeardTimes returns, per node public key, the recent advert receive
// times (ascending) held in mesh_observations — capped to the most recent
// maxPerNode per node (0 = uncapped). It feeds Registry rehydration on boot:
// replaying these reconstructs each node's advert cadence so presence survives a
// restart instead of resetting to unknown-cadence. Bounded by the Tier-0
// retention (48h), so this is a one-time indexed scan.
func (s *Store) MeshNodeHeardTimes(ctx context.Context, maxPerNode int) (map[string][]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pubkey, heard_at FROM mesh_observations ORDER BY pubkey, heard_at`)
	if err != nil {
		return nil, fmt.Errorf("store: reading node heard times: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]time.Time)
	for rows.Next() {
		var pk string
		var h int64
		if err := rows.Scan(&pk, &h); err != nil {
			return nil, fmt.Errorf("store: scanning heard time: %w", err)
		}
		out[pk] = append(out[pk], time.Unix(h, 0))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating heard times: %w", err)
	}
	if maxPerNode > 0 {
		for pk, ts := range out {
			if len(ts) > maxPerNode {
				out[pk] = ts[len(ts)-maxPerNode:] // keep the most recent
			}
		}
	}
	return out, nil
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
			eachEdge(pubkey, splitList(pathNodes), func(a, b string) {
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
			})
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

// eachEdge calls fn for every canonical undirected edge in an advert's relay
// chain ([pubkey, ...pathNodes]) — consecutive pairs, skipping an unresolved hop
// ("") or a self-loop. Shared by compaction and the windowed link read so both
// derive topology identically.
func eachEdge(pubkey string, pathNodes []string, fn func(a, b string)) {
	prev := pubkey
	for _, hop := range pathNodes {
		a, b := prev, hop
		prev = hop
		if a == "" || b == "" || a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		fn(a, b)
	}
}

// MeshLink is one undirected relay link aggregated over a query window — the
// derived topology a mesh map draws. LastSeen drives the recency fade;
// DaysActive (distinct UTC days the link appeared) distinguishes a reliable
// backbone link from a one-off long-haul shot on a shaky network.
type MeshLink struct {
	A, B         string
	Observations int
	BestSNR      float64
	FirstSeen    time.Time
	LastSeen     time.Time
	DaysActive   int
}

type linkKey struct{ a, b string }

type linkAcc struct {
	observations int
	bestSNR      float64
	haveSNR      bool
	firstSeen    int64
	lastSeen     int64
	days         map[int64]struct{}
}

// MeshLinks returns links observed since `since`, merging the Tier 1 rollup
// (compacted per-day history) with the un-compacted Tier 0 raw tail (recent
// freshness newer than the compaction watermark). Each link aggregates the
// observation count, peak SNR, first/last seen, and daysActive (distinct UTC
// days it appeared). Sorted strongest-first (observation count) for a stable,
// legible draw order.
func (s *Store) MeshLinks(ctx context.Context, since time.Time) ([]MeshLink, error) {
	acc := map[linkKey]*linkAcc{}
	get := func(a, b string) *linkAcc {
		k := linkKey{a, b}
		e := acc[k]
		if e == nil {
			e = &linkAcc{firstSeen: 1 << 62, days: map[int64]struct{}{}}
			acc[k] = e
		}
		return e
	}
	note := func(e *linkAcc, count int, snr sql.NullFloat64, first, last, day int64) {
		e.observations += count
		if snr.Valid && (!e.haveSNR || snr.Float64 > e.bestSNR) {
			e.bestSNR = snr.Float64
			e.haveSNR = true
		}
		if first < e.firstSeen {
			e.firstSeen = first
		}
		if last > e.lastSeen {
			e.lastSeen = last
		}
		e.days[day] = struct{}{}
	}

	// (1) Rollup history — one row per (edge, day); observations already summed.
	sinceBucket := since.Unix() - since.Unix()%secondsPerDay
	rows, err := s.db.QueryContext(ctx, `
		SELECT a_pubkey, b_pubkey, bucket, observations, best_snr, first_seen, last_seen
		FROM mesh_link_rollup WHERE bucket >= ?`, sinceBucket)
	if err != nil {
		return nil, fmt.Errorf("store: querying link rollup: %w", err)
	}
	for rows.Next() {
		var a, b string
		var bucket, obs, first, last int64
		var snr sql.NullFloat64
		if err := rows.Scan(&a, &b, &bucket, &obs, &snr, &first, &last); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scanning link rollup: %w", err)
		}
		// A rollup row is a whole day of this edge: add its summed count, mark the day.
		note(get(a, b), int(obs), snr, first, last, bucket)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterating link rollup: %w", err)
	}
	rows.Close()

	// (2) Un-compacted tail — raw receptions past the watermark, within the window.
	var watermark int64
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM mesh_meta WHERE key = ?`, watermarkKey).
		Scan(&watermark); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: reading watermark: %w", err)
	}
	rows2, err := s.db.QueryContext(ctx, `
		SELECT pubkey, heard_at, snr, path_nodes FROM mesh_observations
		WHERE id > ? AND heard_at >= ?`, watermark, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: querying observation tail: %w", err)
	}
	for rows2.Next() {
		var pubkey, pathNodes string
		var heard int64
		var snr sql.NullFloat64
		if err := rows2.Scan(&pubkey, &heard, &snr, &pathNodes); err != nil {
			rows2.Close()
			return nil, fmt.Errorf("store: scanning observation tail: %w", err)
		}
		day := heard - heard%secondsPerDay
		eachEdge(pubkey, splitList(pathNodes), func(a, b string) {
			note(get(a, b), 1, snr, heard, heard, day)
		})
	}
	if err := rows2.Err(); err != nil {
		rows2.Close()
		return nil, fmt.Errorf("store: iterating observation tail: %w", err)
	}
	rows2.Close()

	out := make([]MeshLink, 0, len(acc))
	for k, e := range acc {
		out = append(out, MeshLink{
			A: k.a, B: k.b, Observations: e.observations, BestSNR: e.bestSNR,
			FirstSeen:  time.Unix(e.firstSeen, 0).UTC(),
			LastSeen:   time.Unix(e.lastSeen, 0).UTC(),
			DaysActive: len(e.days),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Observations != out[j].Observations {
			return out[i].Observations > out[j].Observations
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out, nil
}
