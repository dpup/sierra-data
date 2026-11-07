package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/clients/usgs"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/gridapi"
	"github.com/dpup/sierra-data/internal/openapispec"
	"github.com/dpup/sierra-data/internal/places"
)

// TestNormalizersHonorFieldPresence is the normalizer half of the spec's
// presence contract (the RPC half is TestResponsesHonorFieldPresence in
// internal/gridapi). grid.proto marks a field `(google.api.field_behavior) =
// REQUIRED` when the server always sets it, and the published spec then types
// it as present and never null. Most of those fields inside an event are set
// by a poller (provenance, its fetchedAt, a geometry's bbox and centroid, a
// lane closure's planned start, a burn reading's time), so this runs every
// poller over its own test fixtures through a real scheduler tick and store,
// serves the result through the GridService RPCs, and checks the marshaled
// JSON with openapispec.CheckWire. Each event is then resolved, so the
// lifecycle-transition revision is checked too.
//
// A new poller belongs in pollers below. A new REQUIRED field inside an event
// must be produced by at least one of them, or the coverage check fails.
func TestNormalizersHonorFieldPresence(t *testing.T) {
	ctx := testCtx()
	cfg := testConfig()
	st := newSchedStore(t)
	require.NoError(t, places.Seed(ctx, st, &config.Config{Hazards: cfg.Hazards}))

	pollers := presencePollers(t)
	var sourceIDs []string
	for _, n := range pollers {
		sourceIDs = append(sourceIDs, n.SourceIDs()...)
	}
	seedSchedSources(t, st, sourceIDs...)

	sched := NewScheduler(st, SchedulerConfig{
		Enhancer: &fakeNWSEnhancer{summary: "A regional summary."}, EnhancerModel: "gpt-5-mini", BudgetPerTick: 5,
	})
	for _, n := range pollers {
		sched.tick(ctx, PollerSpec{Normalizer: n, Interval: time.Minute}, &pollerState{})
	}

	g := gridapi.NewGridServer(gridapi.NewService(st, nil, nil, cfg, nil))
	seen := map[string]bool{}
	check := func(what string, m proto.Message, err error) {
		t.Helper()
		require.NoError(t, err, what)
		body, err := prefab.JSONMarshalOptions.Marshal(m)
		require.NoError(t, err, what)
		problems, err := openapispec.CheckWire(m.ProtoReflect().Descriptor(), body, seen)
		require.NoError(t, err, what)
		assert.Empty(t, problems, "%s violates the spec's presence contract", what)
	}
	stored := map[string]bool{} // source ids with at least one stored event
	sweep := func(phase string) []string {
		var ids []string
		req := &gridv1.ListEventsRequest{Status: []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}, PageSize: 200}
		var events []*gridv1.Event
		for {
			list, err := g.ListEvents(rpcStreamCtx(), req)
			check(phase+" ListEvents", list, err)
			events = append(events, list.GetEvents()...)
			if list.GetNextPageToken() == "" {
				break
			}
			req.PageToken = list.GetNextPageToken()
		}
		for _, ev := range events {
			ids = append(ids, ev.GetId())
			stored[ev.GetProvenance().GetSourceId()] = true
			got, err := g.GetEvent(rpcStreamCtx(), &gridv1.GetEventRequest{Id: ev.GetId(), EnhancementIo: true})
			check(phase+" GetEvent "+ev.GetId(), got, err)
			hist, err := g.GetEventHistory(rpcStreamCtx(), &gridv1.GetEventHistoryRequest{Id: ev.GetId()})
			check(phase+" GetEventHistory "+ev.GetId(), hist, err)
		}
		hist, err := g.ListHistory(rpcStreamCtx(), &gridv1.ListHistoryRequest{PageSize: 200})
		check(phase+" ListHistory", hist, err)
		return ids
	}

	ids := sweep("ingested")
	for name, n := range pollers {
		found := false
		for _, src := range n.SourceIDs() {
			found = found || stored[src]
		}
		assert.True(t, found, "poller %s stored no events: its fixture no longer exercises the contract", name)
	}
	require.NoError(t, st.TransitionEvents(ctx, ids, gridv1.EventStatus_RESOLVED, time.Now()))
	sweep("resolved")

	event := gridv1.File_grid_proto.Messages().ByName("Event")
	revisions := gridv1.File_grid_proto.Messages().ByName("EventRevisionList")
	for _, f := range openapispec.RequiredFieldsReachable(event, revisions) {
		assert.True(t, seen[f], "%s is REQUIRED in grid.proto but no poller fixture here produced it; extend one", f)
	}
}

// presencePollers builds every production poller over the fixtures its own
// tests use, keyed by name for failure messages.
func presencePollers(t *testing.T) map[string]Normalizer {
	t.Helper()
	weatherNow := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	weather := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{alerts: testNWSAlerts(weatherNow), fetchedAt: weatherNow})
	weather.now = func() time.Time { return weatherNow }

	enhanced, closure, locationless := testIncidents()
	roads := &fakeRoadsAPI{byArea: map[string]*api.ListIncidentsResponse{
		"mother-lode": {Incidents: []*api.Incident{enhanced, closure, locationless}},
	}}

	laneCfg := laneClosureConfig(10)
	laneCfg.Roads.IncidentAreas = []config.IncidentArea{motherLode}
	lanes := newTestLaneClosureNormalizer(laneCfg, &fakeLaneClosures{rows: map[int][]cwwp2.LaneClosure{10: d10Fixture(t)}}, lcsFixtureNow)

	power := freshDoer()
	power.points = collection(
		pgeOutageRow("330042", "PLNND SHUTDOWN", "Awaiting T-Man", 1, 38.43, -120.07),
		pgeOutageRow("330217", "TREE CONTACT", "Crew Enroute", 180, 38.2, -120.3),
	)
	power.polygons = collection(pgePolygonRow("330217", -120.3, 38.2))
	power.psps = collection(pspsRow("20725", "TP02_05172026", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.5, 38.1))

	mesh := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: "aa11bb22cc33", Role: meshcore.RoleRepeater, Name: "Murphys Ridge",
		HasLocation: true, Lat: 38.137412, Lng: -120.457934,
		SNR: 4.5, RSSI: -93, HopCount: 2, HasSignal: true, Gateways: []string{"ag loft rpt"},
		LastAdvertAt: time.Unix(1_782_400_000, 0).UTC(), LastHeardAt: time.Now().UTC(),
	}}}

	return map[string]Normalizer{
		"earthquake":    NewEarthquakeNormalizer(testConfig(), usgs.NewClientWithHTTPDoer("https://usgs.test", &fakeDoer{resp: quakeFixture})),
		"evacuation":    NewEvacuationNormalizer(testConfig(), caloes.NewClientWithHTTPDoer("https://caloes.test", &fakeDoer{resp: evacFixture})),
		"wildfire":      newWildfireNormalizer(&fakeDoer{resp: calfireFixture}, &fakeDoer{resp: firisFixture}),
		"weather alert": weather,
		"road incident": NewRoadIncidentNormalizer(testConfig(), roads),
		"lane closure":  lanes,
		"power":         newPowerNormalizer(power, testNow),
		"burn status":   newBurnNormalizer(stdCfg(), &fakeCalfireBurn{rows: suspendedRows()}, freshLine("red")),
		"mesh":          NewNetworkNormalizer(testConfig(), mesh, nil),
	}
}

// rpcStreamCtx is a context an RPC's ETag guard can set headers on.
func rpcStreamCtx() context.Context {
	return grpc.NewContextWithServerTransportStream(context.Background(), &headerStream{})
}

type headerStream struct{}

func (headerStream) Method() string               { return "/grid.v1.GridService/ListEvents" }
func (headerStream) SetHeader(metadata.MD) error  { return nil }
func (headerStream) SendHeader(metadata.MD) error { return nil }
func (headerStream) SetTrailer(metadata.MD) error { return nil }
