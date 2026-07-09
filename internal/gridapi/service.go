// Package gridapi holds the shared backing logic for the /api/v1 data API
// (docs/grpc-gateway-migration-plan.md, docs/v2-api-spec.md). The Service type
// carries the dependencies and hand-built handlers that both the gRPC GridServer
// (grpc.go, the proto RPCs served camelCase over gRPC-Gateway) and the two
// hand-built gateway routes — the place summary and the .geojson map layers —
// delegate to. The Service is no longer itself an http.Handler. The hand-built
// summary/map-layer responses are protojson with UseProtoNames (snake_case on the
// wire, per those endpoints' contracts); the proto RPCs are marshaled by the
// gateway (camelCase). ETag/If-None-Match is not yet wired (deferred; see the
// migration plan §4).
package gridapi

import (
	"context"
	"time"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// Cache-Control max-age per endpoint family. Store-backed reads revalidate
// fast (ingest ticks are 2–5 min); conditions passthroughs match the shipped
// hazards GeoJSON cadence; scanner config is operator-authored and near-static
// (matches the shipped /api/v1/scanners max-age).
const (
	maxAgeEntities   = 30
	maxAgeConditions = 60
	maxAgeScanners   = 3600
)

// RoadsAPI is the slice of RoadsService the map layers / conditions consume. A
// narrow local interface keeps the package testable with fakes (the
// internal/hazards convention).
type RoadsAPI interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}

// WeatherAPI is the slice of WeatherService the GetConditions RPC and the
// fire_weather map layer consume.
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
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
	Roads   RoadsAPI
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
func NewService(st *store.Store, roads RoadsAPI, weather WeatherAPI, census CensusAPI, cfg *config.Config, hz *hazards.Service) *Service {
	return &Service{
		Store:   st,
		Roads:   roads,
		Weather: weather,
		Census:  census,
		Cfg:     cfg,
		Hazards: hz,
		Now:     time.Now,
	}
}
