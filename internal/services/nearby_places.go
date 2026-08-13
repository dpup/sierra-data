package services

import (
	"sort"
	"strings"

	"github.com/dpup/sierra-data/internal/lib/geo"
)

// nearbyPlaceRadiusMeters is how close a configured place must be before we let
// the model name it.
//
// THE CAP IS THE WHOLE POINT. Grounding without one just trades a wrong place
// for a less-wrong one: the nearest configured town to the Sonora Pass
// collision that acquired "(near Merced)" is Dorrington, 52.8 km away — naming
// that would be nearly as misleading. At 15 km an incident on Hwy 4 at Avery
// gets "Avery"; an incident on a remote pass gets an EMPTY list, and the
// prompt's Place Names rule then forbids naming any locality, leaving the
// feed's own route-and-cross-street text intact.
//
// 15 km is sized against the CORRIDORS, not the towns. The monitored segments
// run 10.9 km (Angels-Murphys), 16.6 (Angels-Sonora), 17.6 (Murphys-Arnold) and
// 33.5 (Arnold-Bear Valley), so 15 km guarantees a named anchor almost anywhere
// on the network while never reaching more than one hop away. It also sits
// between the two proximity constants already in this package: 5 km "on this
// route" and 10 km "best chain control" (roads.go).
//
// Town spacing is NOT the constraint — the towns are closer than the radius
// (Sonora/Columbia 5.9 km, Arnold/Dorrington 9.4), so a single incident will
// legitimately see several. That is fine: they really are all nearby, and the
// list is ordered closest-first so the model has a defensible first choice.
//
// METRES. internal/lib/geo works in metres throughout ("Earth's radius in
// meters", geo.go:44), so both PointToPoint and PointToPolyline return metres.
const nearbyPlaceRadiusMeters = 15000.0

// maxNearbyPlaces bounds the list. A long list invites the model to pick a
// distant name that reads well; the closest few are the only useful ones.
// 8, not 5: one corridor's keyword run must not be able to evict a neighbouring
// town that has real coordinates (see the tier comment below).
const maxNearbyPlaces = 8

// nearbyPlaceNames returns the localities the enhancer may name for an incident
// at (lat, lng), CLOSEST FIRST, or nil when nothing is near enough.
//
// Everything comes from config — no store, no network, no geocoder. That is
// deliberate: incident enhancement runs in this service, upstream of the grid
// store, so the place directory is not reachable here and would not be even if
// we wanted it (places are attached to events later, by store.matchPlaces).
// The config already carries named towns, named corridors, and the settlement
// keywords along each corridor, which is exactly the vocabulary a local reader
// uses.
func (s *RoadsService) nearbyPlaceNames(lat, lng float64) []string {
	if s.config == nil || s.geoUtils == nil {
		return nil
	}
	// tier separates places whose distance we MEASURED from places we merely
	// associated with a nearby corridor. A LocationKeyword has no coordinates of
	// its own, so treating it as "the corridor's distance" asserts something we
	// cannot support — and at Arnold, where two corridors meet at ~0 m, the
	// keyword run from both would otherwise fill the list and evict Dorrington,
	// a real town 9.4 km away with real coordinates.
	const (
		tierMeasured = 0
		tierKeyword  = 1
	)
	type candidate struct {
		name string
		tier int
		dist float64
	}
	var found []candidate
	point := geo.Point{Latitude: lat, Longitude: lng}

	// Named towns — the most useful label a reader recognizes.
	for _, loc := range s.config.Weather.Locations {
		d, err := s.geoUtils.DistanceFromCoords(lat, lng, loc.Coordinates.Latitude, loc.Coordinates.Longitude)
		if err != nil || d > nearbyPlaceRadiusMeters {
			continue
		}
		// "Murphys, CA" -> "Murphys": the state suffix is noise in a label that
		// already sits inside a California-only service.
		found = append(found, candidate{name: trimStateSuffix(loc.Name), tier: tierMeasured, dist: d})
	}

	// Monitored corridors, plus the settlements strung along them. A corridor is
	// a segment, so measure to the line rather than to its endpoints — an
	// incident mid-corridor is close to the road but far from both ends.
	for _, road := range s.config.Roads.MonitoredRoads {
		line := geo.Polyline{Points: []geo.Point{
			{Latitude: road.Origin.Latitude, Longitude: road.Origin.Longitude},
			{Latitude: road.Destination.Latitude, Longitude: road.Destination.Longitude},
		}}
		d, err := s.geoUtils.PointToPolyline(point, line)
		if err != nil || d > nearbyPlaceRadiusMeters {
			continue
		}
		if name := strings.TrimSpace(road.Name); name != "" {
			found = append(found, candidate{name: name, tier: tierMeasured, dist: d})
		}
		// The keywords are the communities the corridor passes through. They
		// have no coordinates of their own, so they inherit the corridor's
		// distance and sort just behind it.
		for _, kw := range road.LocationKeywords {
			if kw = strings.TrimSpace(kw); kw != "" {
				found = append(found, candidate{name: kw, tier: tierKeyword, dist: d})
			}
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].tier != found[j].tier {
			return found[i].tier < found[j].tier
		}
		return found[i].dist < found[j].dist
	})

	out := make([]string, 0, maxNearbyPlaces)
	seen := make(map[string]bool, len(found))
	for _, c := range found {
		key := strings.ToLower(c.name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c.name)
		if len(out) == maxNearbyPlaces {
			break
		}
	}
	// The containing service area is a LAST RESORT, not a list member: it is the
	// coarsest label there is, and offering it alongside a town would invite the
	// model to reach for the region when it has a settlement to hand.
	if len(out) == 0 {
		for _, area := range s.config.Hazards.Areas {
			if area.Bounds.Contains(lat, lng) {
				if name := strings.TrimSpace(area.Name); name != "" {
					return []string{name}
				}
			}
		}
		return nil // empty is meaningful: the model may then name no locality
	}
	return out
}

// trimStateSuffix drops a trailing ", CA" from a configured location name.
func trimStateSuffix(name string) string {
	name = strings.TrimSpace(name)
	for _, suffix := range []string{", CA", ", California"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(name, suffix))
		}
	}
	return name
}
