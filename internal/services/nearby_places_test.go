package services

import (
	"testing"

	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geo"
)

// realGeoConfig is the SHIPPED prefab.yaml geography, verbatim — all six towns
// and all four corridors at their real coordinates. An abridged fixture hid a
// defect once already: with Columbia and Twain Harte omitted, a "wrong
// neighbour" test passed that the real config would have exercised very
// differently (Sonora/Columbia are 5.9 km apart, not the ~14 km the radius
// comment once claimed).
func realGeoConfig() *config.Config {
	return &config.Config{
		Weather: config.WeatherConfig{Locations: []config.WeatherLocation{
			{ID: "murphys", Name: "Murphys, CA", Coordinates: config.Coordinates{Latitude: 38.139117, Longitude: -120.456111}},
			{ID: "arnold", Name: "Arnold, CA", Coordinates: config.Coordinates{Latitude: 38.265006, Longitude: -120.333654}},
			{ID: "sonora", Name: "Sonora, CA", Coordinates: config.Coordinates{Latitude: 37.984100, Longitude: -120.382700}},
			{ID: "columbia", Name: "Columbia, CA", Coordinates: config.Coordinates{Latitude: 38.034900, Longitude: -120.401600}},
			{ID: "twainharte", Name: "Twain Harte, CA", Coordinates: config.Coordinates{Latitude: 38.038300, Longitude: -120.229400}},
			{ID: "dorrington", Name: "Dorrington, CA", Coordinates: config.Coordinates{Latitude: 38.333800, Longitude: -120.271300}},
		}},
		Roads: config.RoadsConfig{MonitoredRoads: []config.MonitoredRoad{
			{Name: "Hwy 4", Section: "Angels Camp to Murphys", ID: "hwy4-angels-murphys",
				Origin:           config.Coordinates{Latitude: 38.067400, Longitude: -120.540200},
				Destination:      config.Coordinates{Latitude: 38.139117, Longitude: -120.456111},
				LocationKeywords: []string{"Vallecito", "Douglas Flat", "Copperopolis"}},
			{Name: "Hwy 4", Section: "Murphys to Arnold", ID: "hwy4-murphys-arnold",
				Origin:           config.Coordinates{Latitude: 38.139117, Longitude: -120.456111},
				Destination:      config.Coordinates{Latitude: 38.265006, Longitude: -120.333654},
				LocationKeywords: []string{"Avery", "Hathaway Pines"}},
			{Name: "Hwy 4", Section: "Arnold to Bear Valley", ID: "hwy4-arnold-bearvalley",
				Origin:           config.Coordinates{Latitude: 38.265006, Longitude: -120.333654},
				Destination:      config.Coordinates{Latitude: 38.461045, Longitude: -120.042368},
				LocationKeywords: []string{"Camp Connell", "Dorrington", "White Pines", "Big Trees", "Ganns", "Tamarack", "Lake Alpine", "Mt Reba", "Ebbetts"}},
			{Name: "Hwy 49", Section: "Angels Camp to Sonora", ID: "hwy49-angels-sonora",
				Origin:      config.Coordinates{Latitude: 38.067400, Longitude: -120.540200},
				Destination: config.Coordinates{Latitude: 37.984100, Longitude: -120.382700}},
		}},
		Hazards: config.HazardsConfig{Areas: []config.HazardArea{{
			ID: "ebbetts-pass", Name: "Ebbetts Pass Corridor",
			Bounds: config.GeoBounds{MinLatitude: 37.87, MaxLatitude: 38.59, MinLongitude: -120.72, MaxLongitude: -119.89},
		}}},
	}
}

func placesSvc() *RoadsService {
	return &RoadsService{config: realGeoConfig(), geoUtils: geo.NewGeoUtils()}
}

// TestNearbyPlaceNames_NearTownGroundsTheModel: an incident in Murphys gets a
// usable list, closest first, so the model can say "near Murphys" truthfully.
func TestNearbyPlaceNames_NearTownGroundsTheModel(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(38.139117, -120.456111)
	if len(got) == 0 {
		t.Fatal("an incident in Murphys must have grounding")
	}
	if got[0] != "Murphys" {
		t.Errorf("closest first: got %v, want Murphys leading", got)
	}
	// The state suffix is noise inside a California-only service.
	for _, n := range got {
		if n == "Murphys, CA" {
			t.Errorf("state suffix should be trimmed: %v", got)
		}
	}
	// The corridor it sits on, and the settlements along it, are usable too.
	joined := map[string]bool{}
	for _, n := range got {
		joined[n] = true
	}
	if !joined["Hwy 4"] {
		t.Errorf("the corridor should be offered: %v", got)
	}
}

// TestNearbyPlaceNames_RemoteIncidentGetsNothing is the case this whole
// mechanism exists for. The Sonora Pass collision that acquired "(near Merced)"
// is 52.8 km from the nearest configured town — naming ANY of them would be
// nearly as misleading. An empty list is the correct answer, and the prompt
// then forbids the model from naming a locality at all.
func TestNearbyPlaceNames_RemoteIncidentGetsNothing(t *testing.T) {
	if got := placesSvc().nearbyPlaceNames(38.320789, -119.671727); len(got) != 0 {
		t.Errorf("Sonora Pass is %v; nothing is within the radius, so the list must be empty", got)
	}
}

// A neighbouring town must not be offered for an incident in the other one —
// the closest configured pair is ~14 km apart, which is what sets the radius.
func TestNearbyPlaceNames_DoesNotOfferTheWrongNeighbour(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(37.984100, -120.382700) // Sonora
	if len(got) == 0 || got[0] != "Sonora" {
		t.Fatalf("expected Sonora to lead, got %v", got)
	}
	for _, n := range got {
		if n == "Murphys" {
			t.Errorf("Murphys is ~19 km away and must not be offered for a Sonora incident: %v", got)
		}
	}
}

// Mid-corridor incidents are close to the ROAD but far from both endpoints, so
// the distance must be measured to the segment.
func TestNearbyPlaceNames_MidCorridorMatchesTheRoad(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(38.2000, -120.3950) // between Murphys and Arnold, off both
	found := false
	for _, n := range got {
		if n == "Hwy 4" || n == "Avery" || n == "Hathaway Pines" {
			found = true
		}
	}
	if !found {
		t.Errorf("a mid-corridor incident should match the corridor, got %v", got)
	}
}

func TestNearbyPlaceNames_CapsAndDedupes(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(38.139117, -120.456111)
	if len(got) > maxNearbyPlaces {
		t.Errorf("list should be capped at %d, got %d: %v", maxNearbyPlaces, len(got), got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("duplicate %q in %v", n, got)
		}
		seen[n] = true
	}
}

func TestNearbyPlaceNames_NilConfigIsSafe(t *testing.T) {
	s := &RoadsService{geoUtils: geo.NewGeoUtils()}
	if got := s.nearbyPlaceNames(38.1, -120.4); got != nil {
		t.Errorf("nil config should ground nothing, got %v", got)
	}
}

// TestNearbyPlaceNames_MeasuredPlacesOutrankKeywords: a LocationKeyword has no
// coordinates, so treating it as "the corridor's distance" asserts something we
// cannot support. At Arnold two corridors meet at ~0 m, and their combined
// keyword run would otherwise fill the list and evict Dorrington — a real town
// 9.4 km away with real coordinates.
func TestNearbyPlaceNames_MeasuredPlacesOutrankKeywords(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(38.265006, -120.333654) // Arnold
	if len(got) == 0 || got[0] != "Arnold" {
		t.Fatalf("expected Arnold to lead, got %v", got)
	}
	idxOf := func(name string) int {
		for i, n := range got {
			if n == name {
				return i
			}
		}
		return -1
	}
	dorrington, keyword := idxOf("Dorrington"), idxOf("Camp Connell")
	if dorrington == -1 {
		t.Fatalf("a real town 9.4 km away must survive the cap: %v", got)
	}
	if keyword != -1 && keyword < dorrington {
		t.Errorf("a coordinate-less keyword outranked a measured town: %v", got)
	}
}

// The area name is a LAST RESORT: it must never appear alongside a finer place.
func TestNearbyPlaceNames_AreaOnlyWhenNothingFiner(t *testing.T) {
	got := placesSvc().nearbyPlaceNames(38.139117, -120.456111) // Murphys
	for _, n := range got {
		if n == "Ebbetts Pass Corridor" {
			t.Errorf("the region should not be offered when a town is at hand: %v", got)
		}
	}
	// A point inside the area but far from every town and corridor falls back
	// to the region rather than to nothing.
	got = placesSvc().nearbyPlaceNames(37.90, -119.95)
	if len(got) != 1 || got[0] != "Ebbetts Pass Corridor" {
		t.Errorf("expected the area as the sole fallback, got %v", got)
	}
}

func TestNearbyPlaceNames_IsDeterministic(t *testing.T) {
	s := placesSvc()
	a := s.nearbyPlaceNames(38.2, -120.395)
	b := s.nearbyPlaceNames(38.2, -120.395)
	if len(a) != len(b) {
		t.Fatalf("unstable length: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("unstable order: %v vs %v", a, b)
		}
	}
}
