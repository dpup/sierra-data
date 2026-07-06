package gridapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/proto"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// Conditions passthroughs (plan §2.3): /v1/roads and /v1/weather re-serve the
// live RoadsService/WeatherService state — conditions, not events — with the
// existing /api/v1 response shapes preserved (protojson, UseProtoNames), plus
// a ?place= bbox filter. Weather is served MINUS per-location alerts: alerts
// are events now (/v1/events?layer=weather_alert); fire_weather stays.

// serveRoads handles GET /v1/roads and /v1/roads/{id}. The ?place= filter
// keeps roads whose configured origin or destination endpoint
// (cfg.Roads.MonitoredRoads — the API response carries no coordinates) falls
// inside the place's geometry bbox.
func (s *Service) serveRoads(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	resp, err := s.Roads.ListRoads(ctx, &api.ListRoadsRequest{})
	if err != nil {
		internal(ctx, w, err)
		return
	}
	roads := resp.GetRoads()

	if p := r.URL.Query().Get("place"); p != "" {
		box, ok, err := s.placeBbox(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown place: %q", p))
			return
		}
		if err != nil {
			internal(ctx, w, err)
			return
		}
		roads = filterRoads(roads, s.Cfg.Roads.MonitoredRoads, box, ok)
	}

	if id == "" {
		writeMessage(w, r, &api.ListRoadsResponse{Roads: roads, LastUpdated: resp.GetLastUpdated()}, maxAgeConditions)
		return
	}
	for _, rd := range roads {
		if rd.GetId() == id {
			writeMessage(w, r, &api.GetRoadResponse{Road: rd, LastUpdated: resp.GetLastUpdated()}, maxAgeConditions)
			return
		}
	}
	notFound(w, fmt.Sprintf("unknown road: %q", id))
}

// serveWeather handles GET /v1/weather and /v1/weather/{location}. Alerts are
// STRIPPED from every weather_data entry (on a clone — the underlying
// response is the weather service's live/cached state and must never be
// mutated); fire_weather is kept. ?place= keeps locations whose configured
// coordinates fall inside the place's bbox.
func (s *Service) serveWeather(w http.ResponseWriter, r *http.Request, locationID string) {
	ctx := r.Context()
	resp, err := s.Weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		internal(ctx, w, err)
		return
	}
	clone := proto.Clone(resp).(*api.ListWeatherResponse)
	for _, wd := range clone.GetWeatherData() {
		wd.Alerts = nil // alerts are events on /v1 (spec §6): query /v1/events?layer=weather_alert
	}
	data := clone.GetWeatherData()

	if p := r.URL.Query().Get("place"); p != "" {
		box, ok, err := s.placeBbox(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown place: %q", p))
			return
		}
		if err != nil {
			internal(ctx, w, err)
			return
		}
		data = filterWeather(data, s.Cfg.Weather.Locations, box, ok)
	}

	if locationID == "" {
		writeMessage(w, r, &api.ListWeatherResponse{
			WeatherData: data,
			LastUpdated: clone.GetLastUpdated(),
			FireWeather: clone.GetFireWeather(),
		}, maxAgeConditions)
		return
	}
	for _, wd := range data {
		if wd.GetLocationId() == locationID {
			writeMessage(w, r, &api.GetLocationWeatherResponse{
				WeatherData: wd,
				LastUpdated: clone.GetLastUpdated(),
				FireWeather: clone.GetFireWeather(),
			}, maxAgeConditions)
			return
		}
	}
	notFound(w, fmt.Sprintf("unknown weather location: %q", locationID))
}

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

// filterRoads keeps roads with a configured endpoint inside the box. Roads
// missing from MonitoredRoads config have no coordinates to test and are
// dropped — unlocatable is not "everywhere".
func filterRoads(roads []*api.Road, monitored []config.MonitoredRoad, b bbox, ok bool) []*api.Road {
	if !ok {
		return nil
	}
	byID := make(map[string]config.MonitoredRoad, len(monitored))
	for _, m := range monitored {
		byID[m.ID] = m
	}
	var out []*api.Road
	for _, rd := range roads {
		m, found := byID[rd.GetId()]
		if !found {
			continue
		}
		if b.contains(m.Origin.Latitude, m.Origin.Longitude) ||
			b.contains(m.Destination.Latitude, m.Destination.Longitude) {
			out = append(out, rd)
		}
	}
	return out
}

// filterWeather keeps weather entries whose configured location coordinates
// fall inside the box; entries without a matching configured location are
// dropped (same rationale as filterRoads).
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
