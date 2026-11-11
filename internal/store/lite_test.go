package store

import (
	"context"
	"testing"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func liteFixture(t *testing.T) (*Store, *gridv1.Event) {
	t.Helper()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	ev := testEvent("usgs:poly", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "Perimeter")
	ev.Geometry = polyGeometry(38.0, -120.5, 38.2, -120.3)
	_, err := s.UpsertEvent(context.Background(), ev)
	require.NoError(t, err)
	return s, ev
}

func queryOne(t *testing.T, s *Store, lite bool) *gridv1.Event {
	t.Helper()
	page, err := s.QueryEventsPage(context.Background(), EventQuery{
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_RESOLVED}, Lite: lite,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	return page.Items[0]
}

// A lite read carries everything but the GeoJSON: the bbox and centroid stay
// so a list can still place the event.
func TestQueryEventsLiteDropsOnlyTheShape(t *testing.T) {
	s, _ := liteFixture(t)
	full := queryOne(t, s, false)
	lite := queryOne(t, s, true)
	require.NotEmpty(t, full.GetGeometry().GetGeojson())
	assert.Empty(t, lite.GetGeometry().GetGeojson())
	assert.Equal(t, full.GetGeometry().GetBbox().GetMinLat(), lite.GetGeometry().GetBbox().GetMinLat())
	assert.NotNil(t, lite.GetGeometry().GetCentroid())
	assert.Equal(t, full.GetHeadline(), lite.GetHeadline())
	assert.Equal(t, full.GetRevision(), lite.GetRevision())
}

// Every write of the current blob writes the lite copy too, so the two always
// describe the same revision.
func TestLiteFollowsTransitions(t *testing.T) {
	s, ev := liteFixture(t)
	require.NoError(t, s.TransitionEvents(context.Background(), []string{ev.GetId()}, gridv1.EventStatus_RESOLVED, time.Now()))
	lite := queryOne(t, s, true)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, lite.GetStatus())
	assert.Equal(t, queryOne(t, s, false).GetRevision(), lite.GetRevision())
}

// Events stored before the migration have no lite row: the read falls back to
// the full blob, and the backfill fills them in batches.
func TestLiteBackfill(t *testing.T) {
	s, _ := liteFixture(t)
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `DELETE FROM event_lite`)
	require.NoError(t, err)
	before := queryOne(t, s, true)
	assert.Empty(t, before.GetGeometry().GetGeojson(), "no lite row yet: the full blob is read, its shape still dropped")
	assert.NotNil(t, before.GetGeometry().GetBbox(), "bbox stays")

	b, err := s.BackfillEventLite(ctx, "", 10)
	require.NoError(t, err)
	assert.Equal(t, LiteBatch{Written: 1}, b, "one short batch: done")
	assert.Empty(t, queryOne(t, s, true).GetGeometry().GetGeojson())
}

// The backfill walks by id, so each call reads only its own rows, and a blob
// that will not unmarshal is skipped rather than stopping the walk on the
// same row after every restart.
func TestLiteBackfillWalksPastUnreadableRows(t *testing.T) {
	s, _ := liteFixture(t)
	ctx := context.Background()
	for _, id := range []string{"usgs:a", "usgs:b", "usgs:c"} {
		ev := testEvent(id, gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, id)
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM event_lite`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE events SET proto = x'ffff' WHERE id = 'usgs:b'`)
	require.NoError(t, err)

	var cursor string
	written, skipped, calls := 0, 0, 0
	for {
		b, err := s.BackfillEventLite(ctx, cursor, 1)
		require.NoError(t, err)
		written += b.Written
		skipped += b.Skipped
		calls++
		if b.Next == "" {
			break
		}
		require.Greater(t, b.Next, cursor, "the cursor only moves forward")
		cursor = b.Next
	}
	assert.Equal(t, 1, skipped)
	assert.Equal(t, 3, written, "usgs:a, usgs:c and the fixture event")
	assert.LessOrEqual(t, calls, 5)
}
