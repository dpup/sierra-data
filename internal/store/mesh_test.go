package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertMeshObservations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	obs := []MeshObservation{
		{
			PubKey: "abcd", HeardAt: time.Unix(1_700_000_000, 0), Broker: "wss://b", Gateway: "gw1",
			SNR: 4.5, RSSI: -93, HopCount: 2,
			Path: []string{"c2", "e2"}, PathNodes: []string{"abcd", ""},
		},
		{PubKey: "ef01", HeardAt: time.Unix(1_700_000_060, 0), Gateway: "gw2"},
	}
	require.NoError(t, s.InsertMeshObservations(ctx, obs))

	n, err := s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Path/PathNodes are stored comma-joined, parallel — an unresolved hop stays a
	// blank slot ("abcd," has a trailing empty), and HeardAt is our-clock seconds.
	var heard int64
	var path, pathNodes string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT heard_at, path, path_nodes FROM mesh_observations WHERE pubkey = 'abcd'`).
		Scan(&heard, &path, &pathNodes))
	assert.Equal(t, int64(1_700_000_000), heard)
	assert.Equal(t, "c2,e2", path)
	assert.Equal(t, "abcd,", pathNodes)

	// An empty batch is a no-op (not an error), leaving the count unchanged.
	require.NoError(t, s.InsertMeshObservations(ctx, nil))
	n, err = s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

type rollupRow struct {
	a, b                string
	bucket              int64
	observations        int
	bestSNR             float64
	firstSeen, lastSeen int64
}

func readRollup(t *testing.T, s *Store) []rollupRow {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT a_pubkey, b_pubkey, bucket, observations, best_snr, first_seen, last_seen
		FROM mesh_link_rollup ORDER BY bucket, a_pubkey, b_pubkey`)
	require.NoError(t, err)
	defer rows.Close()
	var out []rollupRow
	for rows.Next() {
		var r rollupRow
		require.NoError(t, rows.Scan(&r.a, &r.b, &r.bucket, &r.observations, &r.bestSNR, &r.firstSeen, &r.lastSeen))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func dayBucket(ts time.Time) int64 { return ts.Unix() - ts.Unix()%secondsPerDay }

func TestCompactMeshObservations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	day1 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	// Node "aa" relayed via bb then cc; a later advert via bb only (better SNR);
	// and a third the next day. Chain = [pubkey, ...pathNodes] → consecutive edges.
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: day1, SNR: -5, Path: []string{"b", "c"}, PathNodes: []string{"bb", "cc"}},
		{PubKey: "aa", HeardAt: day1.Add(time.Hour), SNR: -3, Path: []string{"b"}, PathNodes: []string{"bb"}},
		{PubKey: "aa", HeardAt: day1.Add(24 * time.Hour), SNR: -8, Path: []string{"b"}, PathNodes: []string{"bb"}},
	}))

	n, err := s.CompactMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "all three observations processed")

	got := readRollup(t, s)
	b1, b2 := dayBucket(day1), dayBucket(day1.Add(24*time.Hour))
	assert.Equal(t, []rollupRow{
		// day 1: aa-bb seen twice (best SNR -3, spanning 10:00→11:00); bb-cc once.
		{a: "aa", b: "bb", bucket: b1, observations: 2, bestSNR: -3, firstSeen: day1.Unix(), lastSeen: day1.Add(time.Hour).Unix()},
		{a: "bb", b: "cc", bucket: b1, observations: 1, bestSNR: -5, firstSeen: day1.Unix(), lastSeen: day1.Unix()},
		// day 2: aa-bb once.
		{a: "aa", b: "bb", bucket: b2, observations: 1, bestSNR: -8, firstSeen: day1.Add(24 * time.Hour).Unix(), lastSeen: day1.Add(24 * time.Hour).Unix()},
	}, got)

	// Watermark advanced: re-running compaction processes nothing and does not
	// double-count (rollup unchanged).
	n, err = s.CompactMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, got, readRollup(t, s))
}

func TestPruneMesh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// obsA: compacted (produces an aa-bb edge). obsB: inserted AFTER compaction, so
	// still past the watermark.
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: old, SNR: -5, PathNodes: []string{"bb"}},
	}))
	_, err := s.CompactMeshObservations(ctx)
	require.NoError(t, err)
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: old, SNR: -5, PathNodes: []string{"bb"}},
	}))

	// Prune with a far-future cutoff: only the COMPACTED obsA (id <= watermark) is
	// removed; the un-compacted obsB survives even though it's equally old.
	removed, err := s.PruneMeshObservations(ctx, old.Add(365*24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed, "watermark guard keeps un-compacted rows")
	remaining, err := s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, remaining)

	// Rollup prune is by bucket age: a cutoff before the bucket keeps it, after drops.
	kept, err := s.PruneMeshLinkRollup(ctx, old.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 0, kept)
	assert.Len(t, readRollup(t, s), 1)

	dropped, err := s.PruneMeshLinkRollup(ctx, old.Add(48*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, dropped)
	assert.Empty(t, readRollup(t, s))
}
