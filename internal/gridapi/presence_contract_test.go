package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/openapispec"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// TestResponsesHonorFieldPresence holds the `(google.api.field_behavior) =
// REQUIRED` annotations in grid.proto to what the RPCs really serve. The spec
// types an annotated field as always present and never null, so a client
// generated from it skips the null check; a path that leaves one unset breaks
// that client. This builds every GridService response over the shared
// fixtures plus one event per remaining layer, marshals it with the gateway's
// own marshaler, and checks it against the contract with openapispec.CheckWire,
// which reads the annotations off the descriptor (no hand-kept list).
//
// It also fails when an annotated field is never observed: a REQUIRED field on
// a path these fixtures do not reach is a claim nothing checks, so add a
// fixture for it. The normalizer side of the same contract (every layer as the
// pollers build it) is TestNormalizersHonorFieldPresence in internal/ingest.
func TestResponsesHonorFieldPresence(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)
	ctx := rpcBG()

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

	allStatuses := []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}
	// Both geometry encodings: the default base64 `geojson`, and OBJECT, which
	// empties it and fills the optional `object` instead.
	for _, format := range []string{"", "OBJECT"} {
		f := " format=" + format
		events, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{Status: allStatuses, PageSize: 200, GeometryFormat: format})
		check("ListEvents"+f, events, err)
		require.NotEmpty(t, events.GetEvents())
		for _, ev := range events.GetEvents() {
			got, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: ev.GetId(), EnhancementIo: true, GeometryFormat: format})
			check("GetEvent "+ev.GetId()+f, got, err)
			hist, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: ev.GetId(), GeometryFormat: format})
			check("GetEventHistory "+ev.GetId()+f, hist, err)
		}
		hist, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{PageSize: 200, GeometryFormat: format})
		check("ListHistory"+f, hist, err)
		places, err := g.ListPlaces(rpcBG(), &gridv1.ListPlacesRequest{GeometryFormat: format})
		check("ListPlaces"+f, places, err)
		for _, p := range places.GetPlaces() {
			got, err := g.GetPlace(rpcBG(), &gridv1.GetPlaceRequest{Place: p.GetSlug(), GeometryFormat: format})
			check("GetPlace "+p.GetSlug()+f, got, err)
		}
		res, err := g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Lat: "38.2552", Lng: "-120.3512", GeometryFormat: format})
		check("ResolvePlace lat/lng"+f, res, err)
	}

	places, err := g.ListPlaces(rpcBG(), &gridv1.ListPlacesRequest{})
	check("ListPlaces", places, err)
	require.NotEmpty(t, places.GetPlaces())
	for _, p := range places.GetPlaces() {
		slug := p.GetSlug()
		sum, err := g.GetPlaceSummary(rpcBG(), &gridv1.GetPlaceSummaryRequest{Place: slug})
		check("GetPlaceSummary "+slug, sum, err)
		// The same builder with condition layers wired (the concrete GridServer
		// binds the live hazards service, which tests cannot construct).
		sum, err = s.buildPlaceSummary(rpcBG(), condOKBuilder("red-flag"), slug)
		check("buildPlaceSummary(conditions) "+slug, sum, err)
		list, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Place: slug, Status: allStatuses})
		check("ListEvents place="+slug, list, err)
		cams, err := g.ListCameras(rpcBG(), &gridv1.ListCamerasRequest{Place: slug})
		check("ListCameras place="+slug, cams, err)
		cond, err := g.GetConditions(rpcBG(), &gridv1.GetConditionsRequest{Place: slug})
		check("GetConditions place="+slug, cond, err)
		scan, err := g.ListScanners(rpcBG(), &gridv1.ListScannersRequest{Place: slug})
		check("ListScanners place="+slug, scan, err)
	}

	res, err := g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Address: "1 Main St, Arnold, CA"})
	check("ResolvePlace address", res, err)
	res, err = g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Lat: "0", Lng: "0"})
	check("ResolvePlace outside every place", res, err)

	cams, err := g.ListCameras(rpcBG(), &gridv1.ListCamerasRequest{})
	check("ListCameras", cams, err)
	unconfigured := *s
	unconfigured.Cameras = nil
	cams, err = NewGridServer(&unconfigured).ListCameras(rpcBG(), &gridv1.ListCamerasRequest{})
	check("ListCameras unconfigured", cams, err)

	cond, err := g.GetConditions(rpcBG(), &gridv1.GetConditionsRequest{})
	check("GetConditions", cond, err)
	srcs, err := g.ListSources(rpcBG(), &gridv1.ListSourcesRequest{})
	check("ListSources", srcs, err)
	stats, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	check("GetStats", stats, err)
	scan, err := g.ListScanners(rpcBG(), &gridv1.ListScannersRequest{})
	check("ListScanners", scan, err)

	for _, node := range []string{presenceMeshNode, "ffffffffffffffff"} { // with samples, and an empty archive
		arch, err := g.GetMeshTelemetry(rpcBG(), &gridv1.GetMeshTelemetryRequest{Node: node})
		check("GetMeshTelemetry "+node, arch, err)
	}

	// Every annotation a GridService response can carry was seen holding a value.
	svc := gridv1.File_grid_proto.Services().ByName("GridService")
	var roots []protoreflect.MessageDescriptor
	for i := 0; i < svc.Methods().Len(); i++ {
		roots = append(roots, svc.Methods().Get(i).Output())
	}
	for _, f := range openapispec.RequiredFieldsReachable(roots...) {
		assert.True(t, seen[f], "%s is REQUIRED in grid.proto but no fixture here produced it; add one", f)
	}
}

const presenceMeshNode = "a2d649457de4481a"

// seedPresenceFixtures adds what newTestService lacks for the presence sweep:
// one event of each layer it does not seed (the ones with REQUIRED fields
// inside their detail blocks among them), a mesh node with archived
// telemetry, cameras, a forecast and a geocoder answer. The events carry what
// the pollers always set (provenance with fetchedAt, geometry with bbox and
// centroid) and nothing the pollers leave optional.
func seedPresenceFixtures(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
		{ID: "caloes", Name: "Cal OES", PollInterval: 5 * time.Minute},
		{ID: "caltrans", Name: "Caltrans", PollInterval: 10 * time.Minute},
		{ID: "pge", Name: "PG&E", PollInterval: 5 * time.Minute},
		{ID: "burnline", Name: "County burn line", PollInterval: time.Hour},
	}))
	require.NoError(t, s.Store.RecordAttempt(ctx, "caloes", nil))
	prov := func(id string) *gridv1.Provenance {
		return &gridv1.Provenance{SourceId: id, SourceName: id, FetchedAt: timestamppb.New(base)}
	}

	evac := evacEvent("evac:CAL-E-1", "ORDER", gridv1.EventStatus_ACTIVE)
	evac.Provenance = prov("caloes")
	upsert(t, s.Store, evac)

	upsert(t, s.Store, &gridv1.Event{
		Id: "caltrans:d10-1", Layer: gridv1.Layer_ROAD_INCIDENT, Category: "closure",
		Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_SCHEDULED,
		Headline: "Lane closure — SR-4", Geometry: pointGeom(38.2, -120.4), Provenance: prov("caltrans"),
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
			Closure: &gridv1.LaneClosureDetail{
				WindowId: "d10-1", PlannedStart: timestamppb.New(base.Add(24 * time.Hour)), EndIndefinite: true,
			},
		}},
	})

	// A CHP incident with its parsed log (every entry's `at` is REQUIRED).
	upsert(t, s.Store, &gridv1.Event{
		Id: "chp:261109MR0185", Layer: gridv1.Layer_ROAD_INCIDENT, Category: "incident",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
		Headline: "Report of fire", Geometry: pointGeom(37.98, -120.38), Provenance: prov("chp"),
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
			LogNumber: "261109MR0185",
			Log:       []*gridv1.IncidentLogEntry{{Number: 1, At: timestamppb.New(base), Text: "BLK SMOKE"}},
		}},
	})

	upsert(t, s.Store, &gridv1.Event{
		Id: "pge:1", Layer: gridv1.Layer_POWER, Category: "unplanned",
		Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE,
		Headline: "Power outage — 40 customers", Geometry: pointGeom(38.15, -120.45), Provenance: prov("pge"),
		Detail: &gridv1.Event_Power{Power: &gridv1.PowerDetail{OutageId: "1", CustomersAffected: 40}},
	})

	upsert(t, s.Store, &gridv1.Event{
		Id: "burn:calaveras-county", Layer: gridv1.Layer_BURN_STATUS, Category: "burn_status",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
		Headline: "Calaveras County: no-burn day", PlaceIds: []string{"county:calaveras-county"},
		ObservedAt: timestamppb.New(base), Provenance: prov("burnline"),
		Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
			BurnDay: gridv1.BurnDay_BURN_DAY_NO,
			BurnLines: []*gridv1.BurnLineReading{
				{Id: "calaveras-apcd", BurnDay: gridv1.BurnDay_BURN_DAY_NO,
					Observation: &gridv1.BurnObservation{Message: "Not a burn day.", ObservedAt: timestamppb.New(base)}},
				{Id: "undialed"}, // a line with no reading: observation null
			},
		}},
	})

	seedMeshNode(t, s, presenceMeshNode)
	recordActivity(t, s, store.MeshActivity{PubKey: presenceMeshNode, LastHeardAt: base, LastRelayedAt: base})
	require.NoError(t, s.Store.InsertMeshTelemetry(ctx, []store.MeshTelemetrySample{
		telemetryAt(presenceMeshNode, base.Add(-time.Hour), 97, 1000),
		telemetryAt(presenceMeshNode, base.Add(-45*time.Minute), 96, 1900),
	}))

	s.Cameras = &fakeCameras{cams: []cwwp2.Camera{camSoulsbyville, camPineGrove}, status: "OK", last: base}
	s.Census = &fakeCensus{lat: 38.2552, lng: -120.3512, matched: "1 MAIN ST, ARNOLD, CA, 95223"}
	s.Weather = &fakeWeather{resp: weatherResp(), forecasts: map[string]*nws.Forecast{
		"arnold": {Source: "nws", IssuedAt: base, HorizonHours: 24,
			Points: []nws.ForecastPoint{{Time: base.Add(time.Hour), TempC: 20}}},
	}}
}
