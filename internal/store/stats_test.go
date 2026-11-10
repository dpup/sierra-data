package store

import (
	"context"
	"testing"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTotals(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	empty, err := s.Totals(ctx, "")
	require.NoError(t, err)
	assert.Zero(t, empty.Events)
	assert.True(t, empty.Since.IsZero())

	seedTotalsFixture(t, s)

	all, err := s.Totals(ctx, "")
	require.NoError(t, err)
	assert.EqualValues(t, 3, all.Events)
	assert.EqualValues(t, 4, all.Revisions, "one event has two revisions")
	assert.Equal(t, 2, all.Sources)
	assert.False(t, all.Since.IsZero())
	assert.WithinDuration(t, time.Now(), all.Since, time.Minute)
	require.NotEmpty(t, all.Layers)
	assert.GreaterOrEqual(t, all.Layers[0].Events, all.Layers[len(all.Layers)-1].Events, "largest first")

	placed, err := s.Totals(ctx, totalsPlace)
	require.NoError(t, err)
	assert.Less(t, placed.Events, all.Events, "a place sees only its attached events")
}

const totalsPlace = "county:calaveras"

func seedTotalsFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	seedPlaceDirectory(t, s)
	seedSource(t, s, "usgs")
	seedSource(t, s, "chp")

	quake := testEvent("usgs:1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "M3.1")
	quake.Geometry = pointGeometry(38.2, -120.5) // inside Calaveras
	_, err := s.UpsertEvent(ctx, quake)
	require.NoError(t, err)
	quake.Headline = "M3.2"
	_, err = s.UpsertEvent(ctx, quake)
	require.NoError(t, err)

	far := testEvent("usgs:2", gridv1.Severity_MINOR, gridv1.EventStatus_RESOLVED, "M2.0")
	far.Geometry = pointGeometry(40.2, -121.5) // Faraway County
	_, err = s.UpsertEvent(ctx, far)
	require.NoError(t, err)

	crash := testEvent("chp:1", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "Collision")
	crash.Layer = gridv1.Layer_ROAD_INCIDENT
	crash.Provenance = &gridv1.Provenance{SourceId: "chp", SourceName: "CHP"}
	crash.Geometry = pointGeometry(40.1, -121.6)
	_, err = s.UpsertEvent(ctx, crash)
	require.NoError(t, err)
}
