package config

import (
	"log"
	"time"

	"github.com/dpup/prefab"

	api "github.com/dpup/info.ersn.net/server/api/v1"
)

// Config represents the complete server configuration
type Config struct {
	GoogleRoutes GoogleRoutesClient `koanf:"googleRoutes"`
	OpenAI       OpenAIClient       `koanf:"openai"`
	OpenWeather  OpenWeatherClient  `koanf:"openweather"`
	Roads        RoadsConfig        `koanf:"roads"`
	Weather      WeatherConfig      `koanf:"weather"`
	Hazards      HazardsConfig      `koanf:"hazards"`
	Grid         GridConfig         `koanf:"grid"`
}

// GridConfig holds the grid event store + ingest scheduler configuration
// (docs/v2-implementation-plan.md). DBPath locates the SQLite database
// (production overrides via PF__GRID__DBPATH); Sources keys are source
// registry ids ("usgs", "nws", ...) — a poller may span several.
type GridConfig struct {
	DBPath      string                  `koanf:"dbPath"`
	Enhancement GridEnhancement         `koanf:"enhancement"`
	Sources     map[string]SourceTuning `koanf:"sources"`
}

// GridEnhancement gates the NWS weather-alert AI enhancer. BudgetPerTick caps
// OpenAI calls per scheduler tick — cost scales with alert change rate, and
// deferred alerts pick up enhancement on a later content change.
type GridEnhancement struct {
	Enabled       bool `koanf:"enabled"`
	BudgetPerTick int  `koanf:"budgetPerTick"`
}

// SourceTuning is one source's poll cadence and lifecycle policy.
// Disappearance is "resolve" (authoritative active-only feed: missing =>
// RESOLVED) or "expire" (missing AND past expires/ExpireAfter => EXPIRED);
// StaleAfter 0 defaults to 3x PollInterval, ExpireAfter 0 means no time-based
// grace (only an event's own expires timestamp can expire it).
type SourceTuning struct {
	PollInterval  time.Duration `koanf:"pollInterval"`
	StaleAfter    time.Duration `koanf:"staleAfter"`
	ExpireAfter   time.Duration `koanf:"expireAfter"`
	Disappearance string        `koanf:"disappearance"`
}

// HazardsConfig holds the unified hazard/situation feed configuration
// (docs/hazard-aggregation-design.md). Each area is a named region the
// /api/v1/hazards/{area}/{layer}.geojson endpoints serve.
type HazardsConfig struct {
	Areas []HazardArea `koanf:"areas"`
}

// HazardArea is a named region for the hazard feed.
type HazardArea struct {
	ID     string    `koanf:"id"`
	Name   string    `koanf:"name"`
	Bounds GeoBounds `koanf:"bounds"`
	// IncidentArea is the roads.incidentAreas id reused for the road_incident
	// layer (so we don't re-implement region filtering).
	IncidentArea string `koanf:"incidentArea"`
	// Zones are the NWS forecast zones (e.g. "CAZ064") this area covers. The
	// weather_alert and fire_weather layers — which carry zones, not coordinates —
	// are scoped to these. Empty means unscoped (every alert matches), which is
	// only correct for a single-area deployment.
	Zones []string `koanf:"zones"`
	// ScannerFeeds is operator-authored Broadcastify config (no upstream fetch).
	ScannerFeeds []ScannerFeed `koanf:"scannerFeeds"`
}

// ScannerFeed is one Broadcastify dispatch feed (operator-authored).
type ScannerFeed struct {
	FeedID       string `koanf:"feedId"`
	ChannelLabel string `koanf:"channelLabel"`
	Agency       string `koanf:"agency"`
}

// Client configurations - moved to top level
type GoogleRoutesClient struct {
	APIKey string `koanf:"apiKey"`
}

type OpenAIClient struct {
	APIKey     string        `koanf:"apiKey"`
	Model      string        `koanf:"model"`
	Timeout    time.Duration `koanf:"timeout"`
	MaxRetries int           `koanf:"maxRetries"`
}

type OpenWeatherClient struct {
	APIKey string `koanf:"apiKey"`
}

// RoadsConfig holds road monitoring configuration
type RoadsConfig struct {
	CaltransFeeds   CaltransConfig  `koanf:"caltransFeeds"`
	MonitoredRoads  []MonitoredRoad `koanf:"monitoredRoads"`
	IncidentAreas   []IncidentArea  `koanf:"incidentAreas"`
	RefreshInterval time.Duration   `koanf:"refreshInterval"`
}

// IncidentArea defines a named geographic region for the region-wide incidents
// feed (GET /api/v1/incidents/{area}). Incidents whose coordinates fall inside
// Bounds are included.
type IncidentArea struct {
	ID     string    `koanf:"id"`
	Name   string    `koanf:"name"`
	Bounds GeoBounds `koanf:"bounds"`
}

// GeoBounds is an axis-aligned latitude/longitude bounding box.
type GeoBounds struct {
	MinLatitude  float64 `koanf:"minLatitude"`
	MaxLatitude  float64 `koanf:"maxLatitude"`
	MinLongitude float64 `koanf:"minLongitude"`
	MaxLongitude float64 `koanf:"maxLongitude"`
}

// Contains reports whether the given coordinate falls within the bounds.
func (b GeoBounds) Contains(lat, lon float64) bool {
	return lat >= b.MinLatitude && lat <= b.MaxLatitude &&
		lon >= b.MinLongitude && lon <= b.MaxLongitude
}

// CaltransConfig holds Caltrans KML feed settings
type CaltransConfig struct {
	LaneClosures   CaltransFeedConfig `koanf:"laneClosures"`
	CHPIncidents   CaltransFeedConfig `koanf:"chpIncidents"`
	RoadConditions CaltransFeedConfig `koanf:"roadConditions"`
}

// CaltransFeedConfig holds individual feed configuration
type CaltransFeedConfig struct {
	RefreshInterval time.Duration `koanf:"refreshInterval"`
	URL             string        `koanf:"url"`
}

// MonitoredRoad represents a road to monitor
type MonitoredRoad struct {
	Name             string      `koanf:"name"`
	Section          string      `koanf:"section"`
	ID               string      `koanf:"id"`
	Origin           Coordinates `koanf:"origin"`
	Destination      Coordinates `koanf:"destination"`
	LocationKeywords []string    `koanf:"locationKeywords"`
}

// WeatherConfig holds weather monitoring configuration
type WeatherConfig struct {
	Locations []WeatherLocation `koanf:"locations"`
	NWS       NWSConfig         `koanf:"nws"`
	// RefreshInterval is the cache TTL; entries are servable-stale until 2x
	// this value (the "very stale" bound), then eligible for eviction.
	RefreshInterval time.Duration `koanf:"refreshInterval"`
}

// NWSConfig holds National Weather Service (api.weather.gov) settings used for
// authoritative zone alerts (issue #4) and fire-weather classification (issue #5).
type NWSConfig struct {
	// UserAgent identifies the app to api.weather.gov (required by NWS).
	UserAgent string `koanf:"userAgent"`
	// Zones is the set of NWS forecast zones covering the service area
	// (e.g. CAZ064, CAZ065, CAZ258, CAZ259).
	Zones []string `koanf:"zones"`
}

// WeatherLocation represents a location to monitor for weather
type WeatherLocation struct {
	ID          string      `koanf:"id"`
	Name        string      `koanf:"name"`
	Coordinates Coordinates `koanf:"coordinates"`
	// Zone is the NWS forecast zone containing this location (e.g. CAZ065).
	// Per-location alerts are the NWS alerts active in this zone, so it must be
	// one of the zones listed in weather.nws.zones or no alerts will attach.
	Zone string `koanf:"zone"`
}

// Coordinates represents lat/lon coordinates - unified structure
type Coordinates struct {
	Latitude  float64 `koanf:"latitude"`
	Longitude float64 `koanf:"longitude"`
}

// ToProto converts Coordinates to protobuf Coordinates
func (c Coordinates) ToProto() *api.Coordinates {
	return &api.Coordinates{
		Latitude:  c.Latitude,
		Longitude: c.Longitude,
	}
}

// ToProto converts WeatherLocation to protobuf Coordinates
func (w WeatherLocation) ToProto() *api.Coordinates {
	return w.Coordinates.ToProto()
}

// LoadConfig loads configuration using Prefab's config system
// Configuration is loaded from prefab.yaml and environment variables with PF__ prefix
func LoadConfig() *Config {
	appConfig := &Config{}
	// Unmarshal client configurations
	if err := prefab.Config.Unmarshal("googleRoutes", &appConfig.GoogleRoutes); err != nil {
		log.Fatalf("Failed to unmarshal googleRoutes section: %v", err)
	}
	if err := prefab.Config.Unmarshal("openai", &appConfig.OpenAI); err != nil {
		log.Fatalf("Failed to unmarshal openai section: %v", err)
	}
	if err := prefab.Config.Unmarshal("openweather", &appConfig.OpenWeather); err != nil {
		log.Fatalf("Failed to unmarshal openweather section: %v", err)
	}
	// Unmarshal service configurations
	if err := prefab.Config.Unmarshal("roads", &appConfig.Roads); err != nil {
		log.Fatalf("Failed to unmarshal roads section: %v", err)
	}
	if err := prefab.Config.Unmarshal("weather", &appConfig.Weather); err != nil {
		log.Fatalf("Failed to unmarshal weather section: %v", err)
	}
	if err := prefab.Config.Unmarshal("hazards", &appConfig.Hazards); err != nil {
		log.Fatalf("Failed to unmarshal hazards section: %v", err)
	}
	if err := prefab.Config.Unmarshal("grid", &appConfig.Grid); err != nil {
		log.Fatalf("Failed to unmarshal grid section: %v", err)
	}
	return appConfig
}
