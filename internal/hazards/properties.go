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
	LayerMesh         = "mesh_node"
	LayerMeshLink     = "mesh_link"
	// LayerPower carries BOTH electric outages and Public Safety Power
	// Shutoffs — they are one question to a reader ("is the power on?") and the
	// per-feature `category` (unplanned|planned|psps) separates them. The slug
	// deliberately matches the Layer enum name so properties.layer ("POWER")
	// reads identically to Event.layer on the /events RPCs.
	LayerPower = "power"
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
	Mesh         *MeshProps         `json:"mesh,omitempty"`
	MeshLink     *MeshLinkProps     `json:"meshLink,omitempty"`
	Power        *PowerProps        `json:"power,omitempty"`
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

// FireWeatherProps is the fire_weather kind block. On the region banner it carries
// the issued State/zones; on a per-location forecast Point it carries Forecast
// (State empty) — informational, never an issued warning.
type FireWeatherProps struct {
	State       string           `json:"state,omitempty"` // normal | elevated | red-flag (banner only)
	SourceEvent string           `json:"sourceEvent,omitempty"`
	Zones       []string         `json:"zones,omitempty"`
	Forecast    *ForecastSummary `json:"forecast,omitempty"`
}

// ForecastSummary is the at-a-glance NWS fire-weather forecast for a location:
// worst wind gust + lowest humidity over the horizon. Informational only — it
// never sets/escalates the feature's severity.
type ForecastSummary struct {
	Source             string `json:"source,omitempty"`
	IssuedAt           string `json:"issuedAt,omitempty"`
	HorizonHours       int    `json:"horizonHours,omitempty"`
	PeakWindGustKmh    int32  `json:"peakWindGustKmh,omitempty"`
	PeakWindGustAt     string `json:"peakWindGustAt,omitempty"`
	MinHumidityPercent int32  `json:"minHumidityPercent,omitempty"`
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

// MeshProps is the mesh_node (MeshCore) kind block. The signal metrics are
// the last-heard values (volatile; not part of the event's content hash). The
// relay path is NOT here — a path is per-reception, not per-node; the drawable
// topology is served derived at GET /api/v1/mesh/links.
type MeshProps struct {
	PublicKey string   `json:"publicKey"`
	NodeType  string   `json:"nodeType,omitempty"` // companion | repeater | room_server | sensor
	Name      string   `json:"name,omitempty"`
	SNR       float64  `json:"snr,omitempty"`
	RSSI      int32    `json:"rssi,omitempty"`
	HopCount  uint32   `json:"hopCount,omitempty"`
	Gateways  []string `json:"gateways,omitempty"` // observers that heard the node
	// InRegion is set ONLY on the mesh_link topology layer: true for a node inside
	// the queried place, false for a 1-hop neighbour pulled in because it links to
	// one. Nil (omitted) on the plain mesh_node layer, where every node is in-place.
	InRegion *bool `json:"inRegion,omitempty"`
}

// MeshLinkProps is the mesh_link kind block — one relay link (LineString) on the
// derived topology layer. A/B are node public keys (canonical A < B). DaysActive
// (distinct days observed) and LastSeen let a client weight and recency-fade the
// link; BestSnr is the peak SNR of an advert seen traversing it.
type MeshLinkProps struct {
	A            string  `json:"a"`
	B            string  `json:"b"`
	Observations int     `json:"observations"`
	DaysActive   int     `json:"daysActive"`
	FirstSeen    string  `json:"firstSeen,omitempty"`
	LastSeen     string  `json:"lastSeen,omitempty"`
	BestSnr      float64 `json:"bestSnr,omitempty"`
}

// PowerProps is the power kind block, covering both PG&E feeds. The outage
// fields and the PSPS fields are mutually exclusive in practice — the feature's
// `category` (unplanned|planned|psps) says which set is populated — but they
// share one block because they answer the same question and a client renders
// them from one card.
type PowerProps struct {
	// Outage (category unplanned|planned).
	OutageID          string `json:"outageId,omitempty"`
	Cause             string `json:"cause,omitempty"` // raw PG&E code, often blank upstream
	CustomersAffected int32  `json:"customersAffected,omitempty"`
	CrewStatus        string `json:"crewStatus,omitempty"`
	// EstimatedRestoration is PG&E's ETOR — an ESTIMATE that is routinely
	// overrun, which is why it is not the feature's `expires`.
	EstimatedRestoration string `json:"estimatedRestoration,omitempty"`

	// PSPS (category psps).
	EventName string `json:"eventName,omitempty"`
	Stage     string `json:"stage,omitempty"` // Watch | Warning
	// MedicalBaselineAffected is customers whose medical equipment depends on
	// grid power. No other feed we carry reports this.
	MedicalBaselineAffected int32  `json:"medicalBaselineAffected,omitempty"`
	DeEnergizationStart     string `json:"deEnergizationStart,omitempty"`
	DeEnergizationEnd       string `json:"deEnergizationEnd,omitempty"`
	// AllClear is PG&E's PLANNED all-clear, not proof the shutoff ended (it is
	// populated on rows still at stage Watch). Render it as an estimate.
	AllClear string `json:"allClear,omitempty"`
}

// setSeverity sets both Severity and the derived SeverityRank.
func (p *Properties) setSeverity(s string) {
	p.Severity = s
	p.SeverityRank = severityRank(s)
}
