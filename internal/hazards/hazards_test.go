package hazards

import (
	"context"
	"testing"
	"time"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPointGeom_SwapsAndTrims(t *testing.T) {
	g := PointGeom(38.0671234, -120.5402987) // internal {lat,lng}
	coords, ok := g.Coordinates.([]float64)
	if !ok || len(coords) != 2 {
		t.Fatalf("coordinates = %v", g.Coordinates)
	}
	// RFC 7946 order is [lon, lat], trimmed to 5 decimals.
	if coords[0] != -120.5403 {
		t.Errorf("lon = %v, want -120.5403 (trimmed)", coords[0])
	}
	if coords[1] != 38.06712 {
		t.Errorf("lat = %v, want 38.06712 (trimmed)", coords[1])
	}
	if g.Type != "Point" {
		t.Errorf("type = %q", g.Type)
	}
}

func TestLineStringAndPolygon(t *testing.T) {
	ls := LineStringGeom([]LatLng{{Lat: 38.0, Lng: -120.5}, {Lat: 38.1, Lng: -120.4}})
	if ls == nil || ls.Type != "LineString" {
		t.Fatalf("linestring = %+v", ls)
	}
	if LineStringGeom([]LatLng{{Lat: 38, Lng: -120}}) != nil {
		t.Error("single-point LineString should be nil")
	}

	poly := PolygonGeom([]LatLng{{Lat: 38, Lng: -120.5}, {Lat: 38.1, Lng: -120.4}, {Lat: 38, Lng: -120.3}})
	if poly == nil || poly.Type != "Polygon" {
		t.Fatalf("polygon = %+v", poly)
	}
	rings := poly.Coordinates.([][][]float64)
	ring := rings[0]
	if ring[0][0] != ring[len(ring)-1][0] || ring[0][1] != ring[len(ring)-1][1] {
		t.Error("polygon ring should be closed (first == last)")
	}
}

func TestSeverityMappings(t *testing.T) {
	cases := []struct {
		got  string
		want string
		rank int
	}{
		{fromAlertSeverity(api.AlertSeverity_CRITICAL), SevSevere, 3},
		{fromAlertSeverity(api.AlertSeverity_WARNING), SevModerate, 2},
		{fromAlertSeverity(api.AlertSeverity_INFO), SevMinor, 1},
		{fromAlertSeverity(api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED), SevInfo, 0},
		{fromChainLevelStr("R3"), SevSevere, 3},
		{fromChainLevelStr("R1"), SevMinor, 1},
		{fromFireWeatherState("red-flag"), SevSevere, 3},
		{fromFireWeatherState("NORMAL"), SevInfo, 0},
		{fromNWSSeverity("Extreme"), SevExtreme, 4},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("severity = %q, want %q", c.got, c.want)
		}
		if severityRank(c.got) != c.rank {
			t.Errorf("rank(%q) = %d, want %d", c.got, severityRank(c.got), c.rank)
		}
	}
}

func TestNormFireName(t *testing.T) {
	// CAL FIRE "Salt Springs Fire" and WFIGS "Salt Springs" must join.
	if normFireName("Salt Springs Fire") != normFireName("Salt Springs") {
		t.Errorf("%q != %q", normFireName("Salt Springs Fire"), normFireName("Salt Springs"))
	}
	if normFireName("Salt Springs Fire") != "saltsprings" {
		t.Errorf("got %q", normFireName("Salt Springs Fire"))
	}
}

func TestFromWildfire(t *testing.T) {
	cases := map[int32]string{0: SevSevere, 49: SevSevere, 50: SevModerate, 99: SevModerate, 100: SevMinor}
	for c, want := range cases {
		if got := fromWildfire(c); got != want {
			t.Errorf("fromWildfire(%d) = %q, want %q", c, got, want)
		}
	}
}

func TestEvacLevelAndSeverity(t *testing.T) {
	cases := []struct{ status, level, sev string }{
		{"Evacuation Order", "ORDER", SevExtreme},
		{"Evacuation Warning", "WARNING", SevSevere},
		{"Shelter in Place", "SHELTER_IN_PLACE", SevSevere},
		{"Advisory", "ADVISORY", SevModerate},
		{"Evacuation Order Lifted", "", SevInfo},
		{"Normal", "", SevInfo},
		{"All Clear", "", SevInfo},
		{"Mandatory Evacuation", "ORDER", SevExtreme},
		{"Voluntary Evacuation", "ADVISORY", SevModerate},
		// Life-safety: an unrecognized but non-inactive status must NOT vanish —
		// it defaults to a conservative active WARNING, not "".
		{"Évacuation immédiate", "WARNING", SevSevere},
	}
	for _, c := range cases {
		if got := normalizeEvacLevel(c.status); got != c.level {
			t.Errorf("normalizeEvacLevel(%q) = %q, want %q", c.status, got, c.level)
		}
		if got := fromEvacLevel(c.level); got != c.sev {
			t.Errorf("fromEvacLevel(%q) = %q, want %q", c.level, got, c.sev)
		}
	}
	// evacStatusRecognized distinguishes a keyword hit from the conservative default.
	if evacStatusRecognized("Évacuation immédiate") {
		t.Error("unrecognized status should report not-recognized (so the builder logs it)")
	}
	if !evacStatusRecognized("Evacuation Order") {
		t.Error("known status should report recognized")
	}
}

// TestEvacAlwaysLinksSource documents the load-bearing safety rule: the
// evacuation layer always carries the authoritative Genasys link + "reference
// only" framing, in every state — a confirmed-empty is "no active zones per Cal
// OES", never a guarantee. (The error-vs-empty distinction is exercised in
// buildlayer_test.go.)
func TestEvacAlwaysLinksSource(t *testing.T) {
	m := layerMeta(LayerEvacuation)
	if m.sourceURL == "" {
		t.Error("evacuation layer must always carry the authoritative source URL")
	}
	if m.attribution == "" {
		t.Error("evacuation layer must always carry the reference-only attribution")
	}
}

func TestSafeURL(t *testing.T) {
	if safeURL("https://protect.genasys.com/x") == "" {
		t.Error("https URL should pass")
	}
	if safeURL("javascript:alert(1)") != "" {
		t.Error("javascript: URL must be dropped")
	}
}

// --- fakes ---

type fakeRoads struct {
	incidents []*api.Incident
	roads     []*api.Road
}

func (f fakeRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return &api.ListRoadsResponse{Roads: f.roads}, nil
}
func (f fakeRoads) ListIncidents(context.Context, *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	return &api.ListIncidentsResponse{Incidents: f.incidents}, nil
}

func TestRoadIncidents_Reprojection(t *testing.T) {
	s := &Service{
		cfg: &config.Config{},
		roads: fakeRoads{incidents: []*api.Incident{{
			Id:                  "260625SA0982",
			Type:                api.AlertType_INCIDENT,
			Severity:            api.AlertSeverity_WARNING,
			Location:            &api.Coordinates{Latitude: 38.0671, Longitude: -120.5402},
			LocationDescription: "Sr49 / Monitor Rd",
			Description:         "Traffic Hazard",
			Status:              api.IncidentStatus_ACTIVE,
			LogNumber:           "260625SA0982",
			Started:             timestamppb.New(time.Unix(1782400000, 0)),
		}}},
	}
	feats, err := s.roadIncidents(context.Background(), config.HazardArea{IncidentArea: "mother-lode"})
	if err != nil {
		t.Fatal(err)
	}
	if len(feats) != 1 {
		t.Fatalf("got %d features, want 1", len(feats))
	}
	f := feats[0]
	if f.Properties.Layer != LayerRoadIncident {
		t.Errorf("layer = %q", f.Properties.Layer)
	}
	if f.Properties.Severity != SevModerate || f.Properties.SeverityRank != 2 {
		t.Errorf("severity = %q rank %d, want MODERATE/2", f.Properties.Severity, f.Properties.SeverityRank)
	}
	coords := f.Geometry.Coordinates.([]float64)
	if coords[0] != -120.5402 || coords[1] != 38.0671 {
		t.Errorf("coords = %v, want [-120.5402, 38.0671]", coords)
	}
	if f.Properties.Incident == nil || f.Properties.Incident.LogNumber != "260625SA0982" {
		t.Errorf("incident props = %+v", f.Properties.Incident)
	}
	if f.Properties.Status != "active" {
		t.Errorf("status = %q", f.Properties.Status)
	}
}

// TestRoadSegments_FollowsPolyline verifies the road_segment layer draws along
// the road's decoded polyline (Road.polyline) when present, and falls back to a
// straight origin->destination line when it is absent.
func TestRoadSegments_FollowsPolyline(t *testing.T) {
	area := config.HazardArea{Bounds: config.GeoBounds{
		MinLatitude: 37, MaxLatitude: 39, MinLongitude: -121, MaxLongitude: -120,
	}}
	cfg := &config.Config{Roads: config.RoadsConfig{MonitoredRoads: []config.MonitoredRoad{{
		ID: "hwy4-x", Name: "Hwy 4", Section: "A to B",
		Origin:      config.Coordinates{Latitude: 38.0, Longitude: -120.5},
		Destination: config.Coordinates{Latitude: 38.2, Longitude: -120.3},
	}}}}

	lineCoords := func(t *testing.T, f Feature) [][]float64 {
		t.Helper()
		if f.Geometry == nil || f.Geometry.Type != "LineString" {
			t.Fatalf("geometry = %+v, want LineString", f.Geometry)
		}
		c, ok := f.Geometry.Coordinates.([][]float64)
		if !ok {
			t.Fatalf("coordinates type = %T, want [][]float64", f.Geometry.Coordinates)
		}
		return c
	}

	t.Run("uses the road polyline when present", func(t *testing.T) {
		s := &Service{cfg: cfg, roads: fakeRoads{roads: []*api.Road{{
			Id: "hwy4-x",
			Polyline: []*api.Coordinates{
				{Latitude: 38.0, Longitude: -120.5},
				{Latitude: 38.1, Longitude: -120.42}, // interior point a straight line wouldn't have
				{Latitude: 38.2, Longitude: -120.3},
			},
		}}}}
		feats, err := s.roadSegments(context.Background(), area)
		if err != nil {
			t.Fatal(err)
		}
		if len(feats) != 1 {
			t.Fatalf("got %d features, want 1", len(feats))
		}
		c := lineCoords(t, feats[0])
		if len(c) != 3 {
			t.Fatalf("got %d coords, want the 3-point polyline (a straight line would be 2)", len(c))
		}
		// GeoJSON is [lng, lat]; the middle vertex is the polyline's interior point.
		if c[1][1] <= 38.0 || c[1][1] >= 38.2 {
			t.Errorf("middle coord lat = %v, want an interior polyline point", c[1][1])
		}
	})

	t.Run("falls back to origin/destination without a polyline", func(t *testing.T) {
		s := &Service{cfg: cfg, roads: fakeRoads{roads: []*api.Road{{Id: "hwy4-x"}}}}
		feats, err := s.roadSegments(context.Background(), area)
		if err != nil {
			t.Fatal(err)
		}
		if len(feats) != 1 {
			t.Fatalf("got %d features, want 1", len(feats))
		}
		if c := lineCoords(t, feats[0]); len(c) != 2 {
			t.Errorf("got %d coords, want 2 (straight origin->destination)", len(c))
		}
	})
}
