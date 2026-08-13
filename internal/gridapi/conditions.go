package gridapi

import (
	"context"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
)

// Shared conditions helpers for the ?place= bbox filter. GridServer.GetConditions
// (grpc.go) serves current weather + fire-weather (there is no roads passthrough
// — road conditions are the road_segment/chain_control map layers); the
// place-bbox resolution and the weather filter below back that path.

// bbox is an axis-aligned lat/lng box (internal lat/lng order, unlike
// GeoJSON's [lng, lat]).
type bbox struct {
	minLat, minLng, maxLat, maxLng float64
}

func (b bbox) contains(lat, lng float64) bool {
	return lat >= b.minLat && lat <= b.maxLat && lng >= b.minLng && lng <= b.maxLng
}

// placeBbox resolves a place (slug or id) and derives its bbox from the
// stored geometry via lib/geojson. ok is false when the place has no
// parseable geometry — a bbox filter against such a place matches nothing
// (it cannot be located, and matching everything would be worse). Point and
// linestring places get degenerate-but-correct boxes.
func (s *Service) placeBbox(ctx context.Context, key string) (bbox, bool, error) {
	place, err := s.Store.GetPlace(ctx, key)
	if err != nil {
		return bbox{}, false, err
	}
	raw := place.GetGeometry().GetGeojson()
	if len(raw) == 0 {
		return bbox{}, false, nil
	}
	g, err := geojson.Parse(raw)
	if err != nil {
		return bbox{}, false, nil
	}
	var b bbox
	b.minLat, b.minLng, b.maxLat, b.maxLng = g.Bbox()
	return b.padDegenerate(), true, nil
}

// coordRoundingEpsilon absorbs the repo-wide 5-decimal coordinate trim
// (~1.1 m) when comparing a stored point against a full-precision configured
// one. It is deliberately tiny — a rounding allowance, NOT a "near this town"
// radius. The nearest pair of configured weather locations is kilometres apart,
// so this cannot pull in a neighbouring town.
const coordRoundingEpsilon = 1e-4 // ~11 m

// padDegenerate widens a zero-width or zero-height box by the rounding epsilon.
//
// A POINT place (every TOWN) yields a box whose corners are the point itself,
// and stored place geometry is trimmed to 5 decimals while the configured
// coordinates it is compared against are not. So a town matched its own weather
// only when its configured coordinates happened to have no more than 5 decimal
// places: `columbia` (38.034900 vs stored 38.0349) matched, `murphys`
// (38.139117 vs stored 38.13912) did not. Live, that meant
// `/api/v1/conditions?place=murphys` — a resident asking for their own town's
// weather — returned an empty list, as did arnold and bearvalley, while the
// other four towns worked.
func (b bbox) padDegenerate() bbox {
	if b.minLat == b.maxLat {
		b.minLat -= coordRoundingEpsilon
		b.maxLat += coordRoundingEpsilon
	}
	if b.minLng == b.maxLng {
		b.minLng -= coordRoundingEpsilon
		b.maxLng += coordRoundingEpsilon
	}
	return b
}

// filterWeather keeps weather entries whose configured location coordinates
// fall inside the box; entries without a matching configured location are
// dropped (unlocatable is not "everywhere").
func filterWeather(data []*api.WeatherData, locations []config.WeatherLocation, b bbox, ok bool) []*api.WeatherData {
	if !ok {
		return nil
	}
	byID := make(map[string]config.WeatherLocation, len(locations))
	for _, l := range locations {
		byID[l.ID] = l
	}
	var out []*api.WeatherData
	for _, wd := range data {
		l, found := byID[wd.GetLocationId()]
		if !found {
			continue
		}
		if b.contains(l.Coordinates.Latitude, l.Coordinates.Longitude) {
			out = append(out, wd)
		}
	}
	return out
}
