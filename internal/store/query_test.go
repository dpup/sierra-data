package store

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
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

// Page.After[i] resumes immediately after item i, so a caller may serve any
// prefix of a page (gridapi stops at a byte budget) and the walk continues
// with nothing skipped or repeated. Next follows the same rule, and is ""
// only when the served prefix ends the result set.
func TestPageResumesAfterAnyPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedInterleaved(t, s, 9)

	full, err := s.QueryEventsPage(ctx, EventQuery{PageSize: 200})
	require.NoError(t, err)
	require.Len(t, full.Items, 9)
	require.Len(t, full.After, 9)
	assert.False(t, full.More)
	assert.Empty(t, full.Next(9), "the whole set served: no next page")
	ids := eventIDs(full.Items)

	for n := 1; n < len(ids); n++ {
		tok := full.Next(n)
		require.Equal(t, full.After[n-1], tok)
		rest, err := s.QueryEventsPage(ctx, EventQuery{PageSize: 200, PageToken: tok})
		require.NoError(t, err)
		assert.Equal(t, ids[n:], eventIDs(rest.Items), "resuming after item %d", n)
	}

	short, err := s.QueryEventsPage(ctx, EventQuery{PageSize: 4})
	require.NoError(t, err)
	assert.True(t, short.More)
	assert.Equal(t, short.After[3], short.Next(4), "a full page with more rows hands back a token")

	for _, history := range []func(tok string) (Page[*gridv1.EventRevision], error){
		func(tok string) (Page[*gridv1.EventRevision], error) {
			return s.QueryHistoryPage(ctx, HistoryQuery{PageSize: 200, PageToken: tok})
		},
		func(tok string) (Page[*gridv1.EventRevision], error) {
			return s.EventHistoryPage(ctx, "usgs:q03", 200, tok)
		},
	} {
		all, err := history("")
		require.NoError(t, err)
		require.NotEmpty(t, all.Items)
		for n := 1; n < len(all.Items); n++ {
			rest, err := history(all.Next(n))
			require.NoError(t, err)
			require.Len(t, rest.Items, len(all.Items)-n)
			assert.Equal(t, all.Items[n].GetRevision(), rest.Items[0].GetRevision())
			assert.Equal(t, all.Items[n].GetEvent().GetId(), rest.Items[0].GetEvent().GetId())
		}
	}
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

// TestQueryHistoryUsesObservedIndex pins the cross-event history query to
// idx_revisions_observed. Without a matching index the unscoped
// /api/v1/history scanned every revision and sorted (6-40 s in production on
// the legacy store, 2026-08-06). A regression is invisible in behaviour and
// shows only as latency, so assert that the index can serve both the range
// and the ORDER BY: no Sort node over event_revisions.
func TestQueryHistoryUsesObservedIndex(t *testing.T) {
	s := newTestStore(t)
	const unscoped = `SELECT r.revision, r.observed_at, r.ingested_at, r.proto, r.event_id
		FROM event_revisions r JOIN events e ON e.id = r.event_id
		WHERE true AND r.observed_at >= $1 AND r.observed_at < $2
		ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC LIMIT $3`
	plan := explain(t, s, true, unscoped, 0, int64(1)<<40, 51)
	assert.Contains(t, plan.indexes(), "idx_revisions_observed",
		"unscoped history must read the observed_at index; plan was:\n%s", plan)
	isSort := func(n planNode) bool { return n.NodeType == "Sort" || n.NodeType == "Incremental Sort" }
	_, sorted := plan.find(isSort)
	assert.False(t, sorted, "the index must satisfy the ORDER BY without a sort; plan was:\n%s", plan)

	// Not vacuous: without the index the same check sees a sort.
	_, err := s.db.Exec(`DROP INDEX idx_revisions_observed`)
	require.NoError(t, err)
	_, sorted = explain(t, s, true, unscoped, 0, int64(1)<<40, 51).find(isSort)
	assert.True(t, sorted, "without idx_revisions_observed the history query sorts")
}

// TestQueryEventsSortsKeysNotBlobs pins QueryEvents' two-phase shape. Any
// status LIST sorts (the index orders by severity within ONE status), and in
// the legacy store sorting a SELECT that carried the proto blob copied every
// matching row's blob into the sorter: an all-statuses page took 25-40 s
// regardless of pageSize (2026-11-07). The filter, sort and LIMIT stay in the
// key-only subquery k (PostgreSQL inlines its scan node, so the plan shows its
// Limit), which must never carry the blob, and the page's blobs are fetched
// afterwards by primary key.
func TestQueryEventsSortsKeysNotBlobs(t *testing.T) {
	s := newTestStore(t)
	all := []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED,
		gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED}
	for _, q := range []EventQuery{{}, {Statuses: all}, {Statuses: all, Layers: []gridv1.Layer{gridv1.Layer_WILDFIRE}},
		{PlaceID: "county:calaveras"}, {Lite: true, Statuses: all}} {
		query, args, err := eventsPageSQL(q, DefaultPageSize)
		require.NoError(t, err)
		plan := explain(t, s, false, query, args...)
		k, ok := plan.find(func(n planNode) bool { return n.NodeType == "Limit" })
		require.True(t, ok, "QueryEvents must limit keys in a subquery k (%+v); plan was:\n%s", q, plan)
		assert.NotEqual(t, "Limit", plan.NodeType, "the LIMIT is inside the join, not over it (%+v); plan was:\n%s", q, plan)
		k.walk(func(n planNode) {
			for _, col := range n.Output {
				assert.NotContains(t, col, "proto", "the key query must not carry a blob (%+v); plan was:\n%s", q, plan)
			}
		})
		pk := explain(t, s, true, query, args...)
		assert.Contains(t, pk.indexes(), "events_pkey", "blobs can come by primary key (%+v); plan was:\n%s", q, pk)
	}
}

// TestPlaceScopedEventsSkipsDeadAttachments: event_places attachments are
// deliberately NEVER removed when an event transitions to RESOLVED/EXPIRED
// (place-scoped history needs them), so a place's attachments grow without
// bound while its live set stays small. The default place query must still
// return only the live set. (The legacy store also pinned SQLite's join order
// here, which depended on ANALYZE statistics; PostgreSQL keeps statistics
// itself through autovacuum, and the plan check under realistic volumes is
// part of the P5 performance pass.)
func TestPlaceScopedEventsSkipsDeadAttachments(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	const live = 20
	seedDeadAttachmentSkew(t, s, live)

	got, _, err := s.QueryEvents(ctx, EventQuery{PlaceID: "county:calaveras", PageSize: 500})
	require.NoError(t, err)
	assert.Len(t, got, live, "only ACTIVE/SCHEDULED events belong in a default place query")
}

// Keyset paging compares ids as bytes. Ids carry ':' and '-' (usgs:nc-1),
// which a linguistic collation orders by ignoring punctuation first; the
// store's database must compare bytewise (C collation, checked by Open) or
// pages would skip and repeat rows. Walk a page at a time over ids chosen to
// differ between the two orders and compare with Go's byte order.
func TestKeysetPagingFollowsByteOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	ids := []string{"usgs:a-z", "usgs:a:a", "usgs:aa", "usgs:a-a", "usgs:A", "usgs:a", "usgs:a.b",
		"usgs:nc-1", "usgs:nc:1", "usgs:nc1", "usgs:B", "usgs:b-", "usgs:b:"}
	for _, id := range ids {
		// Same severity and observed time, so id alone decides the order.
		_, err := s.UpsertEvent(ctx, testEvent(id, gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, id))
		require.NoError(t, err)
	}
	var seen []string
	token := ""
	for {
		page, next, err := s.QueryEvents(ctx, EventQuery{PageSize: 2, PageToken: token})
		require.NoError(t, err)
		seen = append(seen, eventIDs(page)...)
		if next == "" {
			break
		}
		token = next
	}
	want := slices.Clone(ids)
	sort.Strings(want)
	assert.Equal(t, want, seen, "ids page in byte order, each exactly once")

	places := []string{"x:a-z", "x:a:a", "x:aa", "x:A"}
	for _, id := range places {
		require.NoError(t, s.UpsertPlace(ctx, testPlace(id, strings.ReplaceAll(id, ":", "_"), "Same", gridv1.PlaceKind_AREA, nil)))
	}
	got, err := s.ListPlaces(ctx, gridv1.PlaceKind_AREA, "")
	require.NoError(t, err)
	wantPlaces := slices.Clone(places)
	sort.Strings(wantPlaces)
	assert.Equal(t, wantPlaces, placeIDs(got), "ListPlaces orders ties by id bytewise")
}

// seedDeadAttachmentSkew reproduces the production skew: a small live set and
// a large dead one, all attached to the same place.
func seedDeadAttachmentSkew(t *testing.T, s *Store, live int) {
	t.Helper()
	ctx := context.Background()
	seedSource(t, s, "usgs")
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))

	// The planner only picks the good plan once it can see this.
	const dead = 400
	var deadIDs []string
	for i := 0; i < live+dead; i++ {
		ev := testEvent(fmt.Sprintf("usgs:e%04d", i), gridv1.Severity_MINOR,
			gridv1.EventStatus_ACTIVE, "quake")
		ev.Geometry = pointGeometry(38.2, -120.45) // inside the county
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
		if i >= live {
			deadIDs = append(deadIDs, ev.GetId())
		}
	}
	// Retire most of them. Their event_places rows deliberately survive.
	require.NoError(t, s.TransitionEvents(ctx, deadIDs, gridv1.EventStatus_RESOLVED, baseTime))

	var attached int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_places WHERE place_id = $1`, "county:calaveras").Scan(&attached))
	require.Equal(t, live+dead, attached, "dead attachments must survive a transition")
}
