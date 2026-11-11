package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the SQLite -> PostgreSQL dialect traps: each exercises the
// conflict branch (or the construct) that compiles in both dialects but
// behaves differently, so a regression cannot hide behind the happy path.

// No SQLite-style ? placeholder may survive in the store's SQL: pgx would
// send it to the server verbatim, and the statement fails only at runtime,
// on whichever path reaches it first.
func TestNoQuestionMarkPlaceholders(t *testing.T) {
	sqlish := regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|WHERE|VALUES)\b`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !sqlish.MatchString(s) {
				return true
			}
			checked++
			assert.NotContains(t, s, "?", "%s: SQL literal with a ? placeholder: %q", f, s)
			return true
		})
	}
	assert.Greater(t, checked, 50, "the scan found the store's SQL")
}

// mesh_link_rollup's upsert merges a later compaction into an existing day
// bucket. In PostgreSQL an unqualified column on the right of DO UPDATE SET is
// ambiguous (target row or excluded?), and SQLite's scalar max()/min() became
// GREATEST/LEAST, which IGNORE a NULL where SQLite's returned NULL.
func TestRollupUpsertMergesAcrossCompactions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: day, SNR: -5, Path: []string{"b"}, PathNodes: []string{"bb"}},
	}))
	_, err := s.CompactMeshObservations(ctx)
	require.NoError(t, err)

	// A second chunk for the same edge and day: better SNR, an earlier and a
	// later reception.
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: day.Add(-time.Hour), SNR: -9, Path: []string{"b"}, PathNodes: []string{"bb"}},
		{PubKey: "aa", HeardAt: day.Add(time.Hour), SNR: -2, Path: []string{"b"}, PathNodes: []string{"bb"}},
	}))
	_, err = s.CompactMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, []rollupRow{{a: "aa", b: "bb", bucket: dayBucket(day), observations: 3, bestSNR: -2,
		firstSeen: day.Add(-time.Hour).Unix(), lastSeen: day.Add(time.Hour).Unix()}}, readRollup(t, s))

	// A NULL best_snr (possible in imported rows) does not poison the merge.
	_, err = s.db.ExecContext(ctx, `UPDATE mesh_link_rollup SET best_snr = NULL`)
	require.NoError(t, err)
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "aa", HeardAt: day.Add(2 * time.Hour), SNR: -7, Path: []string{"b"}, PathNodes: []string{"bb"}},
	}))
	_, err = s.CompactMeshObservations(ctx)
	require.NoError(t, err)
	got := readRollup(t, s)
	require.Len(t, got, 1)
	assert.Equal(t, 4, got[0].observations)
	assert.Equal(t, -7.0, got[0].bestSNR, "GREATEST(NULL, x) = x")
}

// UPDATE OR IGNORE has no PostgreSQL form; the rename is an insert-select
// that skips a (pubkey, reported_at) the full key already holds, then a
// delete. On a collision the full key's own reading wins.
func TestRenameMeshTelemetryKeyOnCollision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	prefix, full := "de0715314cfa9b5e", "de0715314cfa9b5e00112233"

	fullRead := sample(full, t0)
	fullRead.BatteryVolts = f64(3.9)
	require.NoError(t, s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{fullRead}))
	require.NoError(t, s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{
		sample(prefix, t0), sample(prefix, t0.Add(15*time.Minute)),
	}))

	moved, err := s.RenameMeshTelemetryPubKey(ctx, prefix, full)
	require.NoError(t, err)
	assert.Equal(t, int64(1), moved, "the colliding instant is not moved")

	got, _, err := s.MeshTelemetry(ctx, full, t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 3.9, *got[0].BatteryVolts, "the full key's reading wins the collision")
	got, _, err = s.MeshTelemetry(ctx, prefix, t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, got)
}

// INSERT OR IGNORE -> ON CONFLICT DO NOTHING: a road refresh stamped with a
// time already archived keeps the first sample and does not fail the batch.
func TestRoadSampleDuplicateIsIgnored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	require.NoError(t, s.InsertRoadSamples(ctx, []RoadSample{{RoadID: "hwy4", At: at, Status: 1}}))
	require.NoError(t, s.InsertRoadSamples(ctx, []RoadSample{
		{RoadID: "hwy4", At: at, Status: 3},
		{RoadID: "hwy4", At: at.Add(time.Minute), Status: 2},
	}))
	got, err := s.RoadSamples(ctx, "hwy4", at.Add(-time.Hour), at.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, int32(1), got[0].Status, "the first sample at an instant is kept")
	assert.Equal(t, int32(2), got[1].Status)
}

// The validators are database rows now, bumped in the writing transaction:
// a second store on the same database (another process, in production) sees
// what the first wrote.
func TestVersionsAreSharedThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	_, err := s.UpsertEvent(ctx, testEvent("usgs:q1", 2, 1, "x"))
	require.NoError(t, err)
	require.NoError(t, s.RecordMeshTick(ctx, nil, []MeshActivity{{PubKey: "AB", LastHeardAt: baseTime}}))
	require.NoError(t, s.UpsertPlace(ctx, testPlace("area:x", "x", "X", 1, nil)))

	var rows int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions
		WHERE key IN ('revisions', 'mesh_activity', 'places') AND value = 1`).Scan(&rows))
	assert.Equal(t, 3, rows)
	pv, err := s.PlacesVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pv)
}

// first_ingested_at is written once, on insert: a revision or a transition
// never moves it (it is what /stats' "since" and FirstIngested read).
func TestFirstIngestedAtIsWrittenOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	ev := testEvent("usgs:q1", 2, 1, "v1")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE events SET first_ingested_at = 1000 WHERE id = 'usgs:q1'`)
	require.NoError(t, err)

	ev.Headline = "v2"
	_, err = s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	require.NoError(t, s.TransitionEvents(ctx, []string{"usgs:q1"}, 3, baseTime))

	got, err := s.FirstIngested(ctx, []string{"usgs:q1", "usgs:missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]time.Time{"usgs:q1": time.Unix(1000, 0).UTC()}, got)
}
