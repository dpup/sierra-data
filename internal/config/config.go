package config

import (
	"log"
	"os"
	"time"

	"github.com/dpup/prefab"

	api "github.com/dpup/sierra-data/api/v1"
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
// (production overrides via PF__GRID__DB_PATH); Sources keys are source
// registry ids ("usgs", "nws", ...) — a poller may span several.
type GridConfig struct {
	DBPath string `koanf:"dbPath"`
	// JournalMode is the SQLite journal mode. Default TRUNCATE (safe on both
	// local disk and a network filesystem). Use WAL only on a real local disk —
	// its memory-mapped -shm file does NOT work over NFS/EFS. Empty => TRUNCATE.
	JournalMode string                  `koanf:"journalMode"`
	Enhancement GridEnhancement         `koanf:"enhancement"`
	Sources     map[string]SourceTuning `koanf:"sources"`
	Meshcore    MeshcoreConfig          `koanf:"meshcore"`
	Wildfire    WildfireConfig          `koanf:"wildfire"`
}

// Default wildfire geography. Both are applied when the corresponding
// grid.wildfire key is unset or non-positive, so a deployment that never heard
// of this section still gets the wide fire scope — the narrow, footprint-only
// behaviour is not something anyone should fall back into by omission.
const (
	// DefaultWildfireMarginDegrees ~ 55 km on a side. It puts the fire box a
	// little outside the CHP/Caltrans incident box (roads.incidentAreas), which
	// is the intent: fire should be the widest thing we watch.
	DefaultWildfireMarginDegrees = 0.5
	// DefaultWildfirePlaceBufferMeters ~ 20 km: far enough that a fire gets on
	// the board while there is still time to act on it, close enough that a fire
	// on the far side of a county is not "at" a town.
	DefaultWildfirePlaceBufferMeters = 20000
)

// WildfireConfig widens the wildfire layer's geography relative to every other
// source. Fire is the one hazard where something OUTSIDE the coverage footprint
// is a threat TO it — it moves, it closes the highways out, and an hour of
// warning changes what people do. Every other source only matters where it
// actually happens, so only this layer gets its own, wider geography.
type WildfireConfig struct {
	// MarginDegrees grows the hazards.areas union bbox on every side to form the
	// wildfire INGEST scope: the FIRIS perimeter query envelope and the in-scope
	// test applied to CAL FIRE's statewide incident list. Expressed in degrees
	// (not metres) because it feeds bounding boxes, like every other bounds key.
	// Unset/<=0 => DefaultWildfireMarginDegrees. It can only widen the union,
	// never narrow it.
	MarginDegrees float64 `koanf:"marginDegrees"`
	// PlaceBufferMeters is how close a wildfire has to come to an AREA or TOWN
	// place to ATTACH to it — so an approaching fire shows on that place's map
	// and summary before its perimeter crosses the boundary. Counties and
	// corridors are excluded: counties tile the map (a nearby fire already
	// attaches to some county exactly) and corridors have their own tuned 1.5 km
	// buffer for point events. Unset/<=0 => DefaultWildfirePlaceBufferMeters.
	PlaceBufferMeters float64 `koanf:"placeBufferMeters"`
}

// Margin is MarginDegrees with the default applied.
func (w WildfireConfig) Margin() float64 {
	if w.MarginDegrees > 0 {
		return w.MarginDegrees
	}
	return DefaultWildfireMarginDegrees
}

// PlaceBuffer is PlaceBufferMeters with the default applied.
func (w WildfireConfig) PlaceBuffer() float64 {
	if w.PlaceBufferMeters > 0 {
		return w.PlaceBufferMeters
	}
	return DefaultWildfirePlaceBufferMeters
}

// MeshcoreConfig configures the MeshCore mesh-node presence source: a set of
// community MQTT bridges we subscribe to (several for resilience). It maps to
// the meshcore.Config client struct in cmd/server. Poll cadence and lifecycle
// (disappearance: expire, expireAfter) live in the grid.sources.meshcore
// SourceTuning entry, not here.
type MeshcoreConfig struct {
	Enabled bool `koanf:"enabled"`
	// ActiveWindow is DEPRECATED and ignored: presence is now cadence-aware (each
	// node stays in the snapshot for CadenceK × its own advert interval, clamped
	// to [GraceFloor, GraceCeil]), so a single global window no longer applies.
	// See docs/mesh-topology-design.md §9.
	ActiveWindow time.Duration `koanf:"activeWindow"`
	// CadenceK / GraceFloor / GraceCeil tune cadence-aware presence. A node stays
	// present for CadenceK × its measured inter-advert interval, clamped to
	// [GraceFloor, GraceCeil]; a node with no cadence yet (one-shot / brand-new)
	// gets GraceFloor, so drive-through transients evaporate while a slow backbone
	// repeater is protected in proportion to its rhythm. Defaults (cmd/server):
	// k=3, floor=3h, ceil=72h. The registry's memory retention is GraceCeil.
	CadenceK   float64       `koanf:"cadenceK"`
	GraceFloor time.Duration `koanf:"graceFloor"`
	GraceCeil  time.Duration `koanf:"graceCeil"`
	// RequireValidSignature drops adverts whose Ed25519 signature fails to verify
	// (on by default; framing confirmed against a live capture, 2026-07-17).
	RequireValidSignature bool `koanf:"requireValidSignature"`
	// Bounds is the geofence for node inclusion — nodes whose advertised location
	// falls inside any box are ingested. When empty, the normalizer falls back to
	// the union of hazards.areas. Mesh presence is deliberately monitored over a
	// WIDER area than the hazard region (e.g. the Bay Area too) so there is enough
	// traffic to confirm the source is live; tighten once it's noisy.
	Bounds []GeoBounds `koanf:"bounds"`
	// Username/Password are the subscriber credentials applied to every broker
	// that doesn't set its own. These are SECRETS: inject them per-environment
	// (dev .env, prod terraform) via PF__GRID__MESHCORE__USERNAME /
	// PF__GRID__MESHCORE__PASSWORD — a scalar env key merges reliably, unlike a
	// per-broker key inside the brokers array. Never commit real credentials.
	Username string           `koanf:"username"`
	Password string           `koanf:"password"`
	Brokers  []MeshcoreBroker `koanf:"brokers"`
	// SpamFloor is the minimum gap between persisted raw receptions from the SAME
	// node on the SAME gateway — a guard so a pathological fast-adverting node
	// can't flood the relay-observation store (Tier 0). Multi-gateway copies of
	// one advert are unaffected (different gateways are kept — resilience signal).
	// Defaults to 30s in cmd/server when unset. See docs/mesh-topology-design.md.
	SpamFloor time.Duration `koanf:"spamFloor"`
	// CompactionInterval is the cadence of the relay-topology maintenance tick
	// (fold Tier 0 receptions into the Tier 1 per-link-per-day rollup, then prune).
	// Defaults to 1h in cmd/server when unset.
	CompactionInterval time.Duration `koanf:"compactionInterval"`
	// ObservationRetention caps the age of Tier 0 raw receptions (they survive
	// only long enough for live-map freshness + hop re-resolution once compacted).
	// Defaults to 48h.
	ObservationRetention time.Duration `koanf:"observationRetention"`
	// RollupRetention caps the age of Tier 1 link history — the interesting,
	// cheap-to-keep topology record. Defaults to 2 years.
	RollupRetention time.Duration `koanf:"rollupRetention"`
}

// MeshcoreBroker is one MQTT endpoint. URL scheme selects transport
// (tcp:// | ssl:// | ws:// | wss://). Secrets should come from PF__ env, not
// the committed prefab.yaml.
type MeshcoreBroker struct {
	URL      string   `koanf:"url"`
	ClientID string   `koanf:"clientId"`
	Username string   `koanf:"username"`
	Password string   `koanf:"password"`
	Topics   []string `koanf:"topics"`
	QoS      uint8    `koanf:"qos"`
	// Operator identifies who runs this MQTT server, for event provenance —
	// a human-readable name and an https page (not the wss:// broker URL). A
	// node's events attribute to the operator(s) of the broker(s) that heard it.
	Operator    string `koanf:"operator"`
	OperatorURL string `koanf:"operatorUrl"`
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
	RefreshInterval time.Duration  `koanf:"refreshInterval"`
	Forecast        ForecastConfig `koanf:"forecast"`
}

// ForecastConfig gates the per-location NWS fire-weather forecast (wind/gust/RH,
// on conditions + the fire_weather layer). Keyless, reuses NWS.UserAgent. See
// docs/fire-weather-forecast-design.md. Zero RefreshInterval/HorizonHours default
// to 1h / 48h.
type ForecastConfig struct {
	Enabled         bool          `koanf:"enabled"`
	RefreshInterval time.Duration `koanf:"refreshInterval"`
	HorizonHours    int           `koanf:"horizonHours"`
}

// NWSConfig holds National Weather Service (api.weather.gov) settings used for
// authoritative zone alerts (issue #4) and fire-weather classification (issue #5).
type NWSConfig struct {
	// UserAgent identifies the app to api.weather.gov (required by NWS).
	UserAgent string `koanf:"userAgent"`
	// Zones is the set of NWS forecast zones covering the service area
	// (e.g. CAZ137, CAZ138, CAZ139 — verify with api.weather.gov/points).
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
	// Fail before reading anything: an env override that maps to no real key is
	// silently dropped by koanf, so the value below would be prefab.yaml's while
	// the operator believes it is theirs. See ValidateEnvOverrides — this is a
	// hard stop precisely because the symptom is "it works" (the wrong DB path
	// costs the entire event history on the next container replacement).
	if err := ValidateEnvOverrides(os.Environ()); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

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
