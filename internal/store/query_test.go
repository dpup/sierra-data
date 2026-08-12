package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// seedInterleaved inserts n ACTIVE events with severities and observed times
// deliberately interleaved (including ties) to stress the keyset ordering.
func seedInterleaved(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	seedSource(t, s, "usgs")
	for i := 0; i < n; i++ {
		ev := testEvent(
			fmt.Sprintf("usgs:q%02d", i),
			gridv1.Severity(i%5),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("quake %d", i),
		)
		ev.ObservedAt = timestamppb.New(baseTime.Add(time.Duration(i%7) * time.Minute))
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
}

func eventIDs(events []*gridv1.Event) []string {
	ids := make([]string, len(events))
	for i, ev := range events {
		ids[i] = ev.GetId()
	}
	return ids
}

func TestQueryEventsPaginationStableAndComplete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedInterleaved(t, s, 23)

	full, token, err := s.QueryEvents(ctx, EventQuery{PageSize: 200})
	require.NoError(t, err)
	require.Len(t, full, 23)
	assert.Empty(t, token)

	// Ordering: severity DESC, observed_at DESC, id ASC.
	for i := 1; i < len(full); i++ {
		a, b := full[i-1], full[i]
		if a.GetSeverity() != b.GetSeverity() {
			assert.Greater(t, int32(a.GetSeverity()), int32(b.GetSeverity()))
			continue
		}
		ao, bo := a.GetObservedAt().AsTime(), b.GetObservedAt().AsTime()
		if !ao.Equal(bo) {
			assert.True(t, ao.After(bo))
			continue
		}
		assert.Less(t, a.GetId(), b.GetId())
	}

	// Page through with size 5: pages concatenate to exactly the full set.
	var paged []string
	pageToken := ""
	pages := 0
	for {
		events, next, err := s.QueryEvents(ctx, EventQuery{PageSize: 5, PageToken: pageToken})
		require.NoError(t, err)
		paged = append(paged, eventIDs(events)...)
		pages++
		if next == "" {
			break
		}
		require.LessOrEqual(t, len(events), 5)
		pageToken = next
	}
	assert.Equal(t, 5, pages)
	assert.Equal(t, eventIDs(full), paged)
}

func TestQueryEventsPaginationStableUnderInserts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedInterleaved(t, s, 12)

	full, _, err := s.QueryEvents(ctx, EventQuery{PageSize: 200})
	require.NoError(t, err)
	fullIDs := eventIDs(full)

	page1, token, err := s.QueryEvents(ctx, EventQuery{PageSize: 4})
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// A new top-sorting event arrives mid-pagination: keyset pagination must
	// not shift, duplicate, or skip the remaining pre-existing rows.
	late := testEvent("usgs:late", gridv1.Severity_EXTREME, gridv1.EventStatus_ACTIVE, "late arrival")
	late.ObservedAt = timestamppb.New(baseTime.Add(time.Hour))
	_, err = s.UpsertEvent(ctx, late)
	require.NoError(t, err)

	rest := eventIDs(page1)
	for token != "" {
		var events []*gridv1.Event
		events, token, err = s.QueryEvents(ctx, EventQuery{PageSize: 4, PageToken: token})
		require.NoError(t, err)
		rest = append(rest, eventIDs(events)...)
	}
	assert.Equal(t, fullIDs, rest)
}

func TestQueryEventsFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	seedSource(t, s, "nws")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	quake := testEvent("usgs:q1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "small quake")
	quake.Geometry = pointGeometry(38.2, -120.45) // inside the county
	_, err := s.UpsertEvent(ctx, quake)
	require.NoError(t, err)

	alert := testEvent("wx:a1", gridv1.Severity_SEVERE, gridv1.EventStatus_SCHEDULED, "storm watch")
	alert.Layer = gridv1.Layer_WEATHER_ALERT
	alert.Provenance.SourceId = "nws"
	alert.ObservedAt = timestamppb.New(baseTime.Add(2 * time.Hour))
	_, err = s.UpsertEvent(ctx, alert)
	require.NoError(t, err)

	resolved := testEvent("usgs:q2", gridv1.Severity_EXTREME, gridv1.EventStatus_RESOLVED, "old quake")
	_, err = s.UpsertEvent(ctx, resolved)
	require.NoError(t, err)

	// Default statuses = ACTIVE + SCHEDULED: RESOLVED excluded.
	events, _, err := s.QueryEvents(ctx, EventQuery{})
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:a1", "usgs:q1"}, eventIDs(events))

	// Explicit statuses.
	events, _, err = s.QueryEvents(ctx, EventQuery{Statuses: []gridv1.EventStatus{gridv1.EventStatus_RESOLVED}})
	require.NoError(t, err)
	assert.Equal(t, []string{"usgs:q2"}, eventIDs(events))

	// Layer filter.
	events, _, err = s.QueryEvents(ctx, EventQuery{Layers: []gridv1.Layer{gridv1.Layer_WEATHER_ALERT}})
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:a1"}, eventIDs(events))

	// Minimum severity.
	events, _, err = s.QueryEvents(ctx, EventQuery{MinSeverity: gridv1.Severity_MODERATE})
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:a1"}, eventIDs(events))

	// Since: observed_at >= bound.
	events, _, err = s.QueryEvents(ctx, EventQuery{Since: baseTime.Add(time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:a1"}, eventIDs(events))

	// Place filter joins event_places.
	events, _, err = s.QueryEvents(ctx, EventQuery{PlaceID: "county:calaveras"})
	require.NoError(t, err)
	assert.Equal(t, []string{"usgs:q1"}, eventIDs(events))

	// Bad page token surfaces as an error, not a silent full restart.
	_, _, err = s.QueryEvents(ctx, EventQuery{PageToken: "not-base64!!!"})
	assert.Error(t, err)
}

func TestEventHistoryPagination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:q1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "rev 1")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	for i := 2; i <= 5; i++ {
		ev.Headline = fmt.Sprintf("rev %d", i)
		res, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
		require.Equal(t, uint32(i), res.Revision)
	}

	page1, token, err := s.EventHistory(ctx, "usgs:q1", 3, "")
	require.NoError(t, err)
	require.Len(t, page1, 3)
	require.NotEmpty(t, token)
	assert.Equal(t, uint32(5), page1[0].GetRevision(), "revisions descend")
	assert.Equal(t, "rev 5", page1[0].GetEvent().GetHeadline())

	page2, token, err := s.EventHistory(ctx, "usgs:q1", 3, token)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Empty(t, token)
	assert.Equal(t, uint32(2), page2[0].GetRevision())
	assert.Equal(t, uint32(1), page2[1].GetRevision())
}

func TestQueryHistory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	seedSource(t, s, "nws")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	quake := testEvent("usgs:q1", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake v1")
	quake.Geometry = pointGeometry(38.2, -120.45)
	_, err := s.UpsertEvent(ctx, quake)
	require.NoError(t, err)
	quake.Headline = "quake v2"
	quake.ObservedAt = timestamppb.New(baseTime.Add(30 * time.Minute))
	_, err = s.UpsertEvent(ctx, quake)
	require.NoError(t, err)

	alert := testEvent("wx:a1", gridv1.Severity_SEVERE, gridv1.EventStatus_ACTIVE, "storm")
	alert.Layer = gridv1.Layer_WEATHER_ALERT
	alert.Provenance.SourceId = "nws"
	alert.ObservedAt = timestamppb.New(baseTime.Add(2 * time.Hour))
	_, err = s.UpsertEvent(ctx, alert)
	require.NoError(t, err)

	// Unfiltered: all three revisions, observed_at DESC.
	revs, token, err := s.QueryHistory(ctx, HistoryQuery{})
	require.NoError(t, err)
	assert.Empty(t, token)
	require.Len(t, revs, 3)
	assert.Equal(t, "storm", revs[0].GetEvent().GetHeadline())
	assert.Equal(t, "quake v2", revs[1].GetEvent().GetHeadline())
	assert.Equal(t, "quake v1", revs[2].GetEvent().GetHeadline())

	// Half-open window [From, To) on the revision's observed_at.
	revs, _, err = s.QueryHistory(ctx, HistoryQuery{
		From: baseTime.Add(15 * time.Minute),
		To:   baseTime.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, revs, 1)
	assert.Equal(t, "quake v2", revs[0].GetEvent().GetHeadline())

	// Layer filter joins the current events row.
	revs, _, err = s.QueryHistory(ctx, HistoryQuery{Layers: []gridv1.Layer{gridv1.Layer_EARTHQUAKE}})
	require.NoError(t, err)
	assert.Len(t, revs, 2)

	// Place filter: both quake revisions attach via current event_places.
	revs, _, err = s.QueryHistory(ctx, HistoryQuery{PlaceID: "county:calaveras"})
	require.NoError(t, err)
	assert.Len(t, revs, 2)

	// Keyset pagination walks the full stream without gaps or repeats.
	var seen []string
	pageToken := ""
	for {
		page, next, err := s.QueryHistory(ctx, HistoryQuery{PageSize: 2, PageToken: pageToken})
		require.NoError(t, err)
		for _, r := range page {
			seen = append(seen, fmt.Sprintf("%s@%d", r.GetEvent().GetId(), r.GetRevision()))
		}
		if next == "" {
			break
		}
		pageToken = next
	}
	assert.Equal(t, []string{"wx:a1@1", "usgs:q1@2", "usgs:q1@1"}, seen)
}

// planFor returns the EXPLAIN QUERY PLAN detail lines for a query, joined.
func planFor(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan += detail + "\n"
	}
	require.NoError(t, rows.Err())
	return plan
}

// TestQueryHistoryUsesObservedIndex pins the cross-event history query to
// idx_revisions_observed (migration v4). Without it SQLite scans every revision
// and sorts into a temp B-tree, which is what took an unscoped /api/v1/history
// 6-40s in production (measured 2026-08-06). A regression here is invisible in
// behavior and shows up only as latency, so assert the plan rather than trusting
// the index to stay matched to QueryHistory's ORDER BY.
func TestQueryHistoryUsesObservedIndex(t *testing.T) {
	s := newTestStore(t)

	// The unscoped / time-bounded shape — the pathological one, since without
	// the index there is nothing to narrow the scan.
	const unscoped = `SELECT r.revision, r.observed_at, r.ingested_at, r.proto, r.event_id
		FROM event_revisions r JOIN events e ON e.id = r.event_id
		WHERE 1=1 AND r.observed_at >= ? AND r.observed_at < ?
		ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC LIMIT ?`

	plan := planFor(t, s, unscoped, 0, 1<<40, 51)
	assert.Contains(t, plan, "idx_revisions_observed",
		"unscoped history must use the observed_at index; plan was:\n%s", plan)
	assert.NotContains(t, plan, "TEMP B-TREE",
		"the index must satisfy the ORDER BY without a sort; plan was:\n%s", plan)

	// The place-scoped shape deliberately drives from event_places instead: that
	// join is far more selective than a time range, so SQLite sorts the narrowed
	// set rather than walking the observed_at index and filtering. That is the
	// right plan (place-scoped calls were already ~6x faster than unscoped ones
	// in the same production measurement) - asserted here so a future index
	// change that flips it gets noticed rather than silently pessimized.
	const scoped = `SELECT r.revision, r.observed_at, r.ingested_at, r.proto, r.event_id
		FROM event_revisions r JOIN events e ON e.id = r.event_id
		JOIN event_places ep ON ep.event_id = r.event_id AND ep.place_id = ?
		WHERE 1=1
		ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC LIMIT ?`

	plan = planFor(t, s, scoped, "area:test", 51)
	assert.Contains(t, plan, "idx_event_places_place",
		"place-scoped history should drive from event_places; plan was:\n%s", plan)
}
