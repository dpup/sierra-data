package gridapi

import (
	"context"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
)

// Shared conditions helpers for the ?place= bbox filter. The /v1 roads and
// weather conditions passthroughs live on the gRPC GridServer (grpc.go); the
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
	return b, true, nil
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
