package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i32(v int32) *int32 { return &v }

const (
	roadOpen   = 1
	roadClosed = 3
)

func TestRoadSamplesAndStatusSince(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	var batch []RoadSample
	for i, st := range []int32{roadOpen, roadOpen, roadClosed, roadClosed, roadClosed} {
		batch = append(batch, RoadSample{RoadID: "hwy4", At: t0.Add(time.Duration(i) * 15 * time.Minute), Status: st, DurationMinutes: i32(20)})
	}
	batch = append(batch, batch[0]) // a re-offer is ignored
	require.NoError(t, s.InsertRoadSamples(ctx, batch))

	got, err := s.RoadSamples(ctx, "hwy4", t0, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 4, "[from, to)")
	assert.Equal(t, int32(20), *got[0].DurationMinutes)

	since, status, atStart, ok, err := s.RoadStatusSince(ctx, "hwy4")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int32(roadClosed), status)
	assert.Equal(t, t0.Add(30*time.Minute), since, "the first sample of the current run")
	assert.False(t, atStart)

	_, _, _, ok, err = s.RoadStatusSince(ctx, "nope")
	require.NoError(t, err)
	assert.False(t, ok)

	// A run reaching back to the first sample we hold says so.
	require.NoError(t, s.InsertRoadSamples(ctx, []RoadSample{{RoadID: "hwy108", At: t0, Status: roadOpen}}))
	since, _, atStart, _, _ = s.RoadStatusSince(ctx, "hwy108")
	assert.Equal(t, t0, since)
	assert.True(t, atStart)
}

func TestTypicalRoadMinutes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	// Four Fridays at 17:00 Pacific took 30, 32, 34, 60 minutes; other hours 20.
	fri := time.Date(2026, 10, 9, 17, 0, 0, 0, loc)
	var batch []RoadSample
	for i, d := range []int32{30, 32, 34, 60} {
		batch = append(batch, RoadSample{RoadID: "r", At: fri.AddDate(0, 0, 7*i), Status: roadOpen, DurationMinutes: i32(d)})
		batch = append(batch, RoadSample{RoadID: "r", At: fri.AddDate(0, 0, 7*i).Add(-5 * time.Hour), Status: roadOpen, DurationMinutes: i32(20)})
	}
	batch = append(batch, RoadSample{RoadID: "r", At: fri.Add(time.Hour), Status: roadClosed, DurationMinutes: i32(200)})
	require.NoError(t, s.InsertRoadSamples(ctx, batch))
	from, to := fri.AddDate(0, 0, -1), fri.AddDate(0, 0, 30)

	m, basis, ok, err := s.TypicalRoadMinutes(ctx, "r", roadOpen, fri.AddDate(0, 0, 28), from, to, loc, 3)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int32(33), m, "median of 30, 32, 34, 60")
	assert.Equal(t, "HOUR_OF_WEEK", basis)

	m, basis, ok, _ = s.TypicalRoadMinutes(ctx, "r", roadOpen, fri.Add(2*time.Hour), from, to, loc, 3)
	require.True(t, ok)
	assert.Equal(t, "ALL_HOURS", basis, "no samples at 19:00, so every open sample")
	assert.Equal(t, int32(25), m, "closed samples are never typical")

	_, _, ok, _ = s.TypicalRoadMinutes(ctx, "r", roadOpen, fri, from, to, loc, 100)
	assert.False(t, ok)
}

func TestFirstIngested(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSource(t, s, "usgs")
	ev := testEvent("usgs:1", 0, 0, "M3")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	got, err := s.FirstIngested(ctx, []string{"usgs:1", "nope"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.WithinDuration(t, time.Now(), got["usgs:1"], time.Minute)
}

// RoadStatusSince's "last other status" read must use the status index, not
// walk the road's whole history. Seeded with a long single-status history
// and analyzed, so the planner chooses as it would in production.
func TestRoadStatusSinceUsesStatusIndex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.db.ExecContext(ctx, `INSERT INTO road_samples (road_id, at, status, congestion)
		SELECT 'hwy4', g, 1, 0 FROM generate_series(1, 20000) AS g`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `ANALYZE road_samples`)
	require.NoError(t, err)
	plan := explain(t, s, false, `SELECT MAX(m) FROM (
			SELECT MAX(at) AS m FROM road_samples WHERE road_id = $1 AND status < $2
			UNION ALL
			SELECT MAX(at) FROM road_samples WHERE road_id = $1 AND status > $2) AS other`, "hwy4", 1)
	assert.Contains(t, plan.indexes(), "idx_road_samples_status", "plan was:\n%s", plan)
	_, seq := plan.find(func(n planNode) bool { return n.NodeType == "Seq Scan" })
	assert.False(t, seq, "plan was:\n%s", plan)
}
