// Package gridapi holds the shared backing logic for the /api/v1 data API
// (docs/grpc-gateway-migration-plan.md, docs/v2-api-spec.md). The Service type
// carries the dependencies and helpers that both the gRPC GridServer (grpc.go,
// the proto RPCs served camelCase over gRPC-Gateway — including GetPlaceSummary
// and GetConditions) and the one remaining hand-built gateway route (the
// .geojson map layers) delegate to. The Service is no longer itself an
// http.Handler. The whole surface is camelCase — the proto RPCs via the gateway
// marshaler, the hand-built .geojson via camelCase json struct tags. Conditional
// GET (ETag/If-None-Match -> 304) is wired via prefab's etag plugin on most read
// RPCs (see grpc.go) and the .geojson keeps its own body-hash ETag.
package gridapi

import (
	"context"
	"time"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// maxAgeConditions is the Cache-Control max-age for the .geojson map layers (the
// one hand-built endpoint that writes through writeJSON) — the conditions
// cadence; store-backed reads revalidate fast (ingest ticks are 2–5 min).
const maxAgeConditions = 60

// WeatherAPI is the slice of WeatherService the GetConditions RPC and the
// fire_weather map layer consume.
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
	// LocationForecasts returns per-location fire-weather forecasts (fail-soft;
	// nil/partial, never an error). See internal/services WeatherService.
	LocationForecasts(context.Context) map[string]*nws.Forecast
}

// CensusAPI is the geocoder slice /api/v1/places:resolve?address= consumes
// (implemented by internal/clients/census.Client).
type CensusAPI interface {
	Geocode(ctx context.Context, oneline string) (lat, lng float64, matchedAddress string, err error)
}

// Service holds the /api/v1 API's dependencies. Fields are exported so the
// summary/maplayers/project handlers plug in as methods on the same receiver.
type Service struct {
	Store   *store.Store
	Weather WeatherAPI
	Census  CensusAPI
	Cfg     *config.Config
	// Hazards is the live hazards service the condition-backed map layers
	// (road_segment, chain_control, fire_weather) delegate to (T12b).
	Hazards *hazards.Service
	// Now is the clock (injectable for tests; summary's generated_at uses it).
	Now func() time.Time
}

// NewService wires the /api/v1 API. hz may be nil until the map-layer endpoints
// need it — the entity endpoints never touch it.
func NewService(st *store.Store, weather WeatherAPI, census CensusAPI, cfg *config.Config, hz *hazards.Service) *Service {
	return &Service{
		Store:   st,
		Weather: weather,
		Census:  census,
		Cfg:     cfg,
		Hazards: hz,
		Now:     time.Now,
	}
}
