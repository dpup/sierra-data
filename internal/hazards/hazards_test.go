package hazards

import (
	"context"
	"testing"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
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

func TestLineString(t *testing.T) {
	ls := LineStringGeom([]LatLng{{Lat: 38.0, Lng: -120.5}, {Lat: 38.1, Lng: -120.4}})
	if ls == nil || ls.Type != "LineString" {
		t.Fatalf("linestring = %+v", ls)
	}
	if LineStringGeom([]LatLng{{Lat: 38, Lng: -120}}) != nil {
		t.Error("single-point LineString should be nil")
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
	// CAL FIRE "Salt Springs Fire" and FIRIS "Salt Springs" must join.
	if normFireName("Salt Springs Fire") != normFireName("Salt Springs") {
		t.Errorf("%q != %q", normFireName("Salt Springs Fire"), normFireName("Salt Springs"))
	}
	if normFireName("Salt Springs Fire") != "saltsprings" {
		t.Errorf("got %q", normFireName("Salt Springs Fire"))
	}
}

func TestFromWildfire(t *testing.T) {
	cases := []struct {
		acres     float64
		contained int32
		want      string
	}{
		// Containment floor (small fires: size adds no escalation) — unchanged.
		{5, 0, SevSevere},      // tiny, uncontained
		{9.5, 60, SevModerate}, // Priest Fire: small, partly contained
		{9.5, 100, SevMinor},   // small, fully contained
		// Size escalation for larger fires (the new behavior; old capped at MODERATE).
		{250, 10, SevSevere},   // class D, uncontained
		{500, 60, SevSevere},   // class E, partly contained → SEVERE (was MODERATE)
		{1200, 35, SevExtreme}, // class F, <50% contained → EXTREME (was SEVERE)
		{5000, 55, SevSevere},  // class G, ≥50% contained → SEVERE (was MODERATE)
		{8000, 20, SevExtreme}, // class G, uncontained → EXTREME
		{15000, 100, SevMinor}, // huge but fully contained → containment floor MINOR
	}
	for _, c := range cases {
		if got := fromWildfire(c.acres, c.contained); got != c.want {
			t.Errorf("fromWildfire(%.0f ac, %d%%) = %q, want %q", c.acres, c.contained, got, c.want)
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

// --- fakes ---

type fakeRoads struct {
	roads []*api.Road
}

func (f fakeRoads) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	return &api.ListRoadsResponse{Roads: f.roads}, nil
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
