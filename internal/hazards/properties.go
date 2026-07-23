package hazards

// Layer identifiers (docs/hazard-aggregation-design.md §4.4).
const (
	LayerRoadIncident = "road_incident"
	LayerRoadSegment  = "road_segment"
	LayerChainControl = "chain_control"
	LayerWeatherAlert = "weather_alert"
	LayerFireWeather  = "fire_weather"
	LayerEarthquake   = "earthquake"
	LayerWildfire     = "wildfire"
	LayerEvacuation   = "evacuation"
	LayerNetwork      = "mesh_node"
)

// Properties is the common envelope shared by every hazard feature, plus a
// namespaced per-kind block (only the relevant one is set).
type Properties struct {
	ID           string `json:"id"`
	Layer        string `json:"layer"`
	Kind         string `json:"kind"`
	Category     string `json:"category,omitempty"`
	Severity     string `json:"severity"`
	SeverityRank int    `json:"severityRank"`
	Headline     string `json:"headline"`
	Description  string `json:"description,omitempty"`
	Status       string `json:"status,omitempty"`
	Effective    string `json:"effective,omitempty"`
	Expires      string `json:"expires,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	AreaLabel    string `json:"areaLabel,omitempty"`
	Source       Source `json:"source"`

	// Per-kind typed blocks (exactly one populated).
	Incident     *IncidentProps     `json:"incident,omitempty"`
	Road         *RoadProps         `json:"road,omitempty"`
	ChainControl *ChainControlProps `json:"chainControl,omitempty"`
	Weather      *WeatherProps      `json:"weather,omitempty"`
	FireWeather  *FireWeatherProps  `json:"fireWeather,omitempty"`
	Earthquake   *EarthquakeProps   `json:"earthquake,omitempty"`
	Wildfire     *WildfireProps     `json:"wildfire,omitempty"`
	Evacuation   *EvacuationProps   `json:"evacuation,omitempty"`
	Network      *NetworkProps      `json:"network,omitempty"`
}

// Source identifies the upstream feed a feature came from.
type Source struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Attribution string `json:"attribution,omitempty"`
	FetchedAt   string `json:"fetchedAt,omitempty"`
}

// IncidentProps is the road_incident kind block.
type IncidentProps struct {
	LogNumber string `json:"logNumber,omitempty"`
}

// RoadProps is the road_segment kind block. The numeric fields are pointers so a
// segment with no live data yet (the source hasn't reported) omits them entirely,
// distinct from a genuine 0 (e.g. zero delay on a clear road).
type RoadProps struct {
	RoadID          string `json:"roadId"`
	Congestion      string `json:"congestion,omitempty"`
	DelayMinutes    *int32 `json:"delayMinutes,omitempty"`
	DurationMinutes *int32 `json:"durationMinutes,omitempty"`
	DistanceKm      *int32 `json:"distanceKm,omitempty"`
}

// ChainControlProps is the chain_control kind block.
type ChainControlProps struct {
	Level     string `json:"level,omitempty"` // R1 | R2 | R3
	Highway   string `json:"highway,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// WeatherProps is the weather_alert kind block.
type WeatherProps struct {
	Event  string   `json:"event,omitempty"`
	Source string   `json:"source,omitempty"` // NWS | OPENWEATHERMAP
	Zones  []string `json:"zones,omitempty"`
}

// FireWeatherProps is the fire_weather kind block.
type FireWeatherProps struct {
	State       string   `json:"state"` // normal | elevated | red-flag
	SourceEvent string   `json:"sourceEvent,omitempty"`
	Zones       []string `json:"zones,omitempty"`
}

// EarthquakeProps is the earthquake kind block.
type EarthquakeProps struct {
	Magnitude float64 `json:"magnitude"`
	DepthKm   float64 `json:"depthKm"`
	Felt      int32   `json:"felt,omitempty"`
}

// WildfireProps is the wildfire kind block.
type WildfireProps struct {
	Acres        float64 `json:"acres,omitempty"`
	Containment  int32   `json:"containment"` // 0-100
	County       string  `json:"county,omitempty"`
	Cause        string  `json:"cause,omitempty"`
	HasPerimeter bool    `json:"hasPerimeter"`
}

// EvacuationProps is the evacuation kind block.
type EvacuationProps struct {
	ZoneID    string `json:"zoneId,omitempty"`
	Level     string `json:"level"` // ORDER | WARNING | ADVISORY | SHELTER_IN_PLACE
	EventType string `json:"eventType,omitempty"`
	County    string `json:"county,omitempty"`
}

// NetworkProps is the mesh_node (MeshCore) kind block. The signal metrics are
// the last-heard values (volatile; not part of the event's content hash).
type NetworkProps struct {
	PublicKey string  `json:"publicKey"`
	NodeType  string  `json:"nodeType,omitempty"` // companion | repeater | room_server | sensor
	Name      string  `json:"name,omitempty"`
	SNR       float64 `json:"snr,omitempty"`
	RSSI      int32   `json:"rssi,omitempty"`
	HopCount  uint32  `json:"hopCount,omitempty"`
	// Path is the relay chain (per-hop repeater hashes, hex) the last-heard
	// advert traversed — the mesh topology; Gateways are the observers that heard it.
	Path     []string `json:"path,omitempty"`
	Gateways []string `json:"gateways,omitempty"`
}

// setSeverity sets both Severity and the derived SeverityRank.
func (p *Properties) setSeverity(s string) {
	p.Severity = s
	p.SeverityRank = severityRank(s)
}
