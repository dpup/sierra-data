// Package gridapi serves the hand-built /v1 entity API (docs/v2-api-spec.md,
// docs/v2-implementation-plan.md §2.3/§2.4, task T12a). Every endpoint is a
// plain net/http handler mounted at HandlerPrefix via prefab.WithHTTPHandler
// (the hazards pattern) — no gRPC service. Entity responses are the protojson
// of grid.v1 / api.v1 messages with UseProtoNames (snake_case on the wire);
// binary proto is served on Accept: application/proto. Errors are
// google.rpc.Status protojson. All bodies carry a strong ETag.
package gridapi

import (
	"context"
	"net/http"
	"time"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// HandlerPrefix is where the /v1 API mounts. It must end in "/" so the
// prefab mux longest-prefix match keeps /api/ (gateway) and / (site) intact.
const HandlerPrefix = "/v1/"

// Cache-Control max-age per endpoint family. Store-backed reads revalidate
// fast (ingest ticks are 2–5 min); conditions passthroughs match the shipped
// hazards GeoJSON cadence; scanner config is operator-authored and near-static
// (matches the shipped /api/v1/scanners max-age).
const (
	maxAgeEntities   = 30
	maxAgeConditions = 60
	maxAgeScanners   = 3600
)

// RoadsAPI is the slice of RoadsService the /v1/roads conditions passthrough
// consumes. A narrow local interface keeps the package testable with fakes
// (the internal/hazards convention).
type RoadsAPI interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}

// WeatherAPI is the slice of WeatherService the /v1/weather passthrough
// consumes.
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
}

// CensusAPI is the geocoder slice /v1/places/resolve?address= consumes
// (implemented by internal/clients/census.Client).
type CensusAPI interface {
	Geocode(ctx context.Context, oneline string) (lat, lng float64, matchedAddress string, err error)
}

// Service holds the /v1 API's dependencies. Fields are exported so the T12b
// additions (summary.go, maplayers.go, project.go) plug in as methods on the
// same receiver without touching this file.
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

// NewService wires the /v1 API. hz may be nil until the map-layer endpoints
// (T12b) need it — the entity endpoints never touch it.
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

var _ http.Handler = (*Service)(nil)
