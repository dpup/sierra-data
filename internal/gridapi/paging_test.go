package gridapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// testBudget stands in for PageByteBudget so the fixtures stay small: the
// paging rules are the same at 64 KiB as at 3 MiB.
const testBudget = 64 << 10

var allStatuses = []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}

// polygonGeom returns a closed ring of n vertices around (lat, lng): a
// perimeter-sized geometry whose GeoJSON is roughly 22 bytes per vertex.
func polygonGeom(lat, lng float64, n int) *gridv1.Geometry {
	var sb strings.Builder
	sb.WriteString(`{"type":"Polygon","coordinates":[[`)
	for i := 0; i <= n; i++ {
		k := i % n
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "[%.5f,%.5f]", lng+float64(k)*1e-5, lat+float64(k%7)*1e-5)
	}
	sb.WriteString(`]]}`)
	return &gridv1.Geometry{
		Geojson:  []byte(sb.String()),
		Bbox:     &gridv1.BoundingBox{MinLat: lat, MinLng: lng, MaxLat: lat + 1e-4, MaxLng: lng + float64(n)*1e-5},
		Centroid: &gridv1.LatLng{Lat: lat, Lng: lng},
	}
}

// seedPerimeters writes n RESOLVED wildfire events whose geometry is about
// vertices*22 bytes each, with distinct observed times so the order is total.
func seedPerimeters(t *testing.T, s *Service, n, vertices int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("calfire:p%03d", i)
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: ids[i], Layer: gridv1.Layer_WILDFIRE,
			Severity: gridv1.Severity_SEVERE, Status: gridv1.EventStatus_RESOLVED,
			Headline:   "Old fire " + ids[i],
			Geometry:   polygonGeom(38.0, -120.5, vertices),
			ObservedAt: timestamppb.New(base.Add(-time.Duration(i) * time.Minute)),
			Provenance: &gridv1.Provenance{SourceId: "calfire"},
		})
		require.NoError(t, err)
	}
	return ids
}

// walkEvents follows nextPageToken to the end, checking every page against the
// byte budget, and returns the ids in the order served.
func walkEvents(t *testing.T, g *GridServer, req *gridv1.ListEventsRequest) (ids []string, pages int) {
	t.Helper()
	for {
		out, err := g.ListEvents(rpcBG(), req)
		require.NoError(t, err, "page %d", pages)
		pages++
		require.NotEmpty(t, out.GetEvents(), "a page with a next token is never empty (page %d)", pages)
		if len(out.GetEvents()) > 1 {
			assert.LessOrEqual(t, proto.Size(out), g.pageBudget, "page %d exceeds the byte budget", pages)
		}
		for _, ev := range out.GetEvents() {
			ids = append(ids, ev.GetId())
		}
		if out.GetNextPageToken() == "" {
			return ids, pages
		}
		next := proto.Clone(req).(*gridv1.ListEventsRequest)
		next.PageToken = out.GetNextPageToken()
		req = next
		require.Less(t, pages, 1000, "runaway paging")
	}
}

// The production failure: pageSize=200 over perimeter-heavy events built a
// response past the gateway's 4 MiB gRPC receive limit and the client got 429
// RESOURCE_EXHAUSTED with no way forward. A page now stops at the byte budget
// and hands back a token, so following tokens always yields every event.
func TestListEventsPagesStopAtTheByteBudget(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	g.pageBudget = testBudget
	want := seedPerimeters(t, s, 40, 400) // ~9 KB each, ~360 KB total: 6+ pages

	got, pages := walkEvents(t, g, &gridv1.ListEventsRequest{Layer: []string{"WILDFIRE"}, Status: allStatuses, PageSize: 200})

	assert.Greater(t, pages, 1, "the budget split the walk")
	// Severity SEVERE first (the seeded calfire:f1 and the perimeters), each in
	// observed order; nothing lost, nothing repeated.
	assert.Equal(t, append([]string{"calfire:f1"}, want...), got)
}

// A page is cut by bytes, never padded or reordered: pageSize still caps it,
// and the cut-off page resumes exactly where it stopped.
func TestByteBudgetKeepsKeysetOrder(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	g.pageBudget = testBudget
	seedPerimeters(t, s, 30, 400)

	// The reference order with no budget pressure (tiny geometries need none).
	ref := NewGridServer(s)
	refIDs, _ := walkEvents(t, ref, &gridv1.ListEventsRequest{Status: allStatuses, PageSize: 200})

	for _, size := range []int32{1, 3, 7, 50, 200} {
		got, _ := walkEvents(t, g, &gridv1.ListEventsRequest{Status: allStatuses, PageSize: size})
		assert.Equal(t, refIDs, got, "pageSize=%d", size)
	}
	out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: allStatuses, PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, out.GetEvents(), 2, "pageSize still caps a page that fits")
}

// An event bigger than the whole budget is still served: alone on its page,
// with a token for the rest.
func TestOversizedEventIsServedAlone(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	g.pageBudget = testBudget
	ctx := context.Background()
	_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
		Id: "calfire:huge", Layer: gridv1.Layer_WILDFIRE,
		Severity: gridv1.Severity_EXTREME, Status: gridv1.EventStatus_ACTIVE,
		Headline:   "Huge perimeter",
		Geometry:   polygonGeom(38.0, -120.5, 6000), // ~130 KB, twice the budget
		ObservedAt: timestamppb.New(base),
		Provenance: &gridv1.Provenance{SourceId: "calfire"},
	})
	require.NoError(t, err)

	out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{PageSize: 200})
	require.NoError(t, err)
	require.Len(t, out.GetEvents(), 1)
	assert.Equal(t, "calfire:huge", out.GetEvents()[0].GetId())
	assert.Greater(t, proto.Size(out), testBudget)
	require.NotEmpty(t, out.GetNextPageToken())

	rest, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{PageSize: 200, PageToken: out.GetNextPageToken()})
	require.NoError(t, err)
	assert.Equal(t, []string{"calfire:f1", "usgs:q1", "wx:a1"}, eventIDs(rest.GetEvents()))
	assert.Empty(t, rest.GetNextPageToken())
}

// The same budget applies to both revision lists: one event's history and the
// cross-event archive.
func TestRevisionListsStopAtTheByteBudget(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	g.pageBudget = testBudget
	ctx := context.Background()
	ev := &gridv1.Event{
		Id: "calfire:grow", Layer: gridv1.Layer_WILDFIRE,
		Severity: gridv1.Severity_SEVERE, Status: gridv1.EventStatus_ACTIVE,
		Provenance: &gridv1.Provenance{SourceId: "calfire"},
	}
	const revisions = 30
	for i := range revisions {
		ev.Headline = fmt.Sprintf("Growing fire, update %d", i)
		ev.Geometry = polygonGeom(38.0, -120.5, 400+i) // a new perimeter each revision
		ev.ObservedAt = timestamppb.New(base.Add(time.Duration(i) * time.Hour))
		_, err := s.Store.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}

	var got []uint32
	pages := 0
	for tok := ""; ; pages++ {
		out, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "calfire:grow", PageSize: 200, PageToken: tok})
		require.NoError(t, err)
		require.NotEmpty(t, out.GetRevisions())
		if len(out.GetRevisions()) > 1 {
			assert.LessOrEqual(t, proto.Size(out), testBudget)
		}
		for _, r := range out.GetRevisions() {
			got = append(got, r.GetRevision())
		}
		if tok = out.GetNextPageToken(); tok == "" {
			break
		}
	}
	assert.Greater(t, pages, 1)
	require.Len(t, got, revisions)
	for i, r := range got {
		assert.Equal(t, uint32(revisions-i), r, "newest first, none skipped")
	}

	seen := map[string]bool{}
	pages = 0
	for tok := ""; ; pages++ {
		out, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{PageSize: 200, PageToken: tok})
		require.NoError(t, err)
		require.NotEmpty(t, out.GetRevisions())
		if len(out.GetRevisions()) > 1 {
			assert.LessOrEqual(t, proto.Size(out), testBudget)
		}
		for _, r := range out.GetRevisions() {
			k := fmt.Sprintf("%s@%d", r.GetEvent().GetId(), r.GetRevision())
			assert.False(t, seen[k], "repeated %s", k)
			seen[k] = true
		}
		if tok = out.GetNextPageToken(); tok == "" {
			break
		}
	}
	assert.Greater(t, pages, 1)
	// 30 growth revisions + the fixture's 2 quake + 1 fire + 1 alert + 1 incident.
	assert.Len(t, seen, revisions+5)
}

// A request whose context is cancelled mid-query (the deploy handoff's drain
// cancels in-flight requests after its grace period) is a retryable 503, not a
// 500: nothing is wrong with the server or the request.
func TestInterruptedReadIsUnavailableNotInternal(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	ctx, cancel := context.WithCancel(rpcBG())
	cancel()

	_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{Status: allStatuses})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "got %v", err)

	_, err = g.ListHistory(ctx, &gridv1.ListHistoryRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "got %v", err)

	// An ordinary store failure is still INTERNAL.
	require.NoError(t, s.Store.Close())
	_, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{})
	assert.Equal(t, codes.Internal, status.Code(err), "got %v", err)
}

func eventIDs(evs []*gridv1.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.GetId()
	}
	return out
}
