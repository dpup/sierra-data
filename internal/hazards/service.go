package hazards

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/config"
)

// RoadsAPI / WeatherAPI are the narrow slices of the roads/weather services the
// surviving condition builders need — road_segment reads ListRoads, fire_weather
// reads ListWeather. Interfaces keep the package testable and let the /v1 grid
// API construct the service with fakes via NewServiceWithAPIs.
type RoadsAPI interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
	// LocationForecasts returns per-location fire-weather forecasts (fail-soft;
	// nil/partial, never an error).
	LocationForecasts(context.Context) map[string]*nws.Forecast
}

// Service projects the roads/weather feeds into the unified GeoJSON hazard
// model. Since the /api/v1 hazards surface was removed, its only consumer is the
// /v1 grid API (internal/gridapi), which calls BuildLayer for the three
// condition layers (road_segment, chain_control, fire_weather); the event
// layers are projected directly from the store by gridapi.
type Service struct {
	cfg      *config.Config
	roads    RoadsAPI
	weather  WeatherAPI
	caltrans *caltrans.FeedParser
	cache    *cache.Cache

	// layerBuilders is derived once from layerRegistry() so the dispatch map has
	// one source of truth.
	layerBuilders map[string]builder
}

// NewServiceWithAPIs constructs the service with the roads/weather dependencies
// as their narrow interfaces, so consumers outside this package (the /v1 grid
// API) can construct it with fakes.
func NewServiceWithAPIs(cfg *config.Config, roads RoadsAPI, weather WeatherAPI, ct *caltrans.FeedParser, c *cache.Cache) *Service {
	s := &Service{
		cfg:      cfg,
		roads:    roads,
		weather:  weather,
		caltrans: ct,
		cache:    c,
	}
	reg := s.layerRegistry()
	s.layerBuilders = make(map[string]builder, len(reg))
	for _, e := range reg {
		s.layerBuilders[e.name] = e.build
	}
	return s
}

// builder produces a layer's features for an area. Returning an error makes the
// layer fail-loud (UNAVAILABLE metadata, empty features) rather than fabricating
// a clear state. A builder may return partialData(err) to keep its (incomplete)
// features while signalling STALE.
type builder func(ctx context.Context, area config.HazardArea) ([]Feature, error)

// layerEntry binds a layer name to its builder. layerRegistry is the single
// canonical list the dispatch map derives from.
type layerEntry struct {
	name  string
	build builder
}

func (s *Service) layerRegistry() []layerEntry {
	return []layerEntry{
		{LayerChainControl, s.chainControls},
		{LayerFireWeather, s.fireWeather},
		{LayerRoadSegment, s.roadSegments},
	}
}

// layerMetadata carries per-layer collection metadata.
type layerMetadata struct {
	attribution string
	sourceURL   string
}

func layerMeta(layer string) layerMetadata {
	switch layer {
	case LayerEvacuation:
		// Always carry the authoritative Genasys link + "reference only" framing,
		// in every state (OK/STALE/UNAVAILABLE) — a confirmed-empty is "no active
		// zones per Cal OES", never a guarantee.
		return layerMetadata{
			attribution: "Cal OES / California County Governments — reference only",
			sourceURL:   caloes.SourceURL,
		}
	default:
		return layerMetadata{}
	}
}

// layerResult is the outcome of building one layer: its features plus the
// fail-loud-adjusted source status and metadata.
type layerResult struct {
	features         []Feature
	status           string
	meta             layerMetadata
	lastSourceUpdate time.Time // when the underlying data was last fetched OK (STALE only)
}

// partialDataError signals a builder produced usable but incomplete data (e.g.
// one of several sources failed). buildLayer surfaces it as STALE and KEEPS the
// returned features, rather than UNAVAILABLE with empty features.
type partialDataError struct{ err error }

func (e *partialDataError) Error() string { return e.err.Error() }
func (e *partialDataError) Unwrap() error { return e.err }
func partialData(err error) error         { return &partialDataError{err} }

// layerTTL is the cache lifetime for a layer's upstream data, or 0 for layers
// that are already cached by the underlying roads/weather services (no
// double-caching). The new keyless upstreams + the live Caltrans KML chain-
// control fetch are cached here so a burst of map clients doesn't fan out to
// every source on every request, and so a transient upstream blip can fall back
// to the last good fetch (STALE) instead of going UNAVAILABLE.
func layerTTL(layer string) time.Duration {
	switch layer {
	case LayerEarthquake, LayerWildfire, LayerChainControl:
		return 5 * time.Minute
	case LayerEvacuation:
		return 2 * time.Minute // life-safety: short, so STALE fallback stays recent
	default:
		return 0
	}
}

// buildLayer runs one layer's builder and applies the fail-loud rules uniformly.
//
// Status resolution:
//   - fresh cache hit            -> OK (served from cache, no upstream call)
//   - builder OK                 -> OK (and the non-empty result is cached)
//   - builder partialData(err)   -> STALE, features kept (one source degraded)
//   - builder hard error + cache -> STALE, last good features served
//   - builder hard error, none   -> UNAVAILABLE, empty
//
// A clean empty success is OK with zero features — NOT UNAVAILABLE. The
// life-safety property is "an error never becomes a 0": UNAVAILABLE means the
// source genuinely failed (so a consumer shows "unknown / check the official
// source"), while OK + 0 means the source is healthy and currently reports
// nothing. The two are deliberately distinguishable.
func (s *Service) buildLayer(ctx context.Context, area config.HazardArea, layer string, build builder) layerResult {
	meta := layerMeta(layer)
	ttl := layerTTL(layer)
	key := "hazard:" + area.ID + ":" + layer

	if ttl > 0 && s.cache != nil {
		var cached []Feature
		if ok, _ := s.cache.Get(key, &cached); ok {
			return finalize(meta, cached, "OK", time.Time{})
		}
	}

	features, err := build(ctx, area)
	if err != nil {
		var pd *partialDataError
		if errors.As(err, &pd) {
			// Usable but incomplete — keep the features, flag STALE.
			logging.Warnw(ctx, "Hazard layer degraded (partial data)", "layer", layer, "area", area.ID, "error", err)
			return finalize(meta, features, "STALE", time.Now())
		}
		logging.Errorw(ctx, "Hazard layer build failed", "layer", layer, "area", area.ID, "error", err)
		// Stale-on-error: serve the last good fetch if we have one.
		if ttl > 0 && s.cache != nil {
			var stale []Feature
			if entry, ok, derr := s.cache.GetWithMetadata(key, &stale); ok && derr == nil && len(stale) > 0 {
				logging.Warnw(ctx, "Serving stale cached hazard layer after upstream failure",
					"layer", layer, "area", area.ID, "age", time.Since(entry.CreatedAt).String())
				return finalize(meta, stale, "STALE", entry.CreatedAt)
			}
		}
		return finalize(meta, nil, "UNAVAILABLE", time.Time{})
	}

	// Success. Cache non-empty results so stale-on-error has something to serve;
	// never cache an empty result — that keeps the safety property that a later
	// fetch error falls through to UNAVAILABLE, never replaying a stale "0".
	if ttl > 0 && s.cache != nil && len(features) > 0 {
		_ = s.cache.Set(key, features, ttl, "hazard:"+layer)
	}
	return finalize(meta, features, "OK", time.Time{})
}

// DegradeStoreStatus maps a store-derived source status onto the SERVED
// envelope status — the fail-loud table buildLayerFromStore documents. It is
// shared with the /v1 surface (internal/gridapi map layers and place summary)
// so the two endpoints can never disagree about identical store state:
//
//	OK                    -> OK, zero lastUpdate (freshness needs no caveat)
//	STALE                 -> STALE + lastUpdate
//	UNAVAILABLE + hasData -> STALE + lastUpdate: the store IS the last-good
//	                         cache, so a down source with stored data serves
//	                         stale data — never UNAVAILABLE hiding live data
//	UNAVAILABLE, no data  -> UNAVAILABLE, zero lastUpdate (the envelope never
//	                         carries last_source_update in this state)
//
// Any unrecognized status ranks as UNAVAILABLE — health unknown is not OK.
// The invariant preserved: UNAVAILABLE always means empty features, so a
// client that draws nothing on UNAVAILABLE never hides data the server sent.
func DegradeStoreStatus(status string, hasData bool, lastUpdate time.Time) (string, time.Time) {
	switch status {
	case "OK":
		return "OK", time.Time{}
	case "STALE":
		return "STALE", lastUpdate
	default:
		if hasData {
			return "STALE", lastUpdate
		}
		return "UNAVAILABLE", time.Time{}
	}
}

// finalize packages a built layer's result.
func finalize(meta layerMetadata, features []Feature, status string, lastUpdate time.Time) layerResult {
	return layerResult{features: features, status: status, meta: meta, lastSourceUpdate: lastUpdate}
}

// BuildLayer builds one layer for an area through the same fail-loud path the
// GeoJSON and /situation endpoints use (buildLayer + layerMeta), exported for
// the /v1 grid API's condition-backed map layers (road_segment, chain_control,
// fire_weather). ok is false for an unknown layer; every other return mirrors
// the metadata block the shipped endpoints emit (status OK|STALE|UNAVAILABLE,
// lastSourceUpdate zero unless serving stale, per-layer attribution/sourceURL).
func (s *Service) BuildLayer(ctx context.Context, area config.HazardArea, layer string) (features []Feature, status string, lastSourceUpdate time.Time, attribution, sourceURL string, ok bool) {
	build, found := s.layerBuilders[layer]
	if !found {
		return nil, "", time.Time{}, "", "", false
	}
	res := s.buildLayer(ctx, area, layer, build)
	return res.features, res.status, res.lastSourceUpdate, res.meta.attribution, res.meta.sourceURL, true
}

// --- layer builders (re-project existing feeds) ---
func (s *Service) chainControls(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	controls, err := s.caltrans.ParseChainControlsDetailed(ctx)
	if err != nil {
		return nil, err
	}
	var out []Feature
	for _, c := range controls {
		if c.Coordinates == nil || !area.Bounds.Contains(c.Coordinates.Latitude, c.Coordinates.Longitude) {
			continue
		}
		p := Properties{
			ID:           "cc:" + nonEmpty(c.MessageID, c.LocationName),
			Layer:        strings.ToUpper(LayerChainControl),
			Kind:         "Chain control",
			Category:     strings.ToLower(c.Level),
			Headline:     strings.TrimSpace(c.Highway + " chain control " + c.Level),
			Description:  c.Description,
			AreaLabel:    c.LocationName,
			Effective:    c.EffectiveTime,
			Source:       Source{ID: "caltrans", Name: "Caltrans", Attribution: "quickmap.dot.ca.gov"},
			ChainControl: &ChainControlProps{Level: c.Level, Highway: c.Highway, Direction: c.Direction},
		}
		p.setSeverity(fromChainLevelStr(c.Level))
		out = append(out, Feature{Type: "Feature", Geometry: PointGeom(c.Coordinates.Latitude, c.Coordinates.Longitude), Properties: p})
	}
	return out, nil
}

func (s *Service) roadSegments(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.roads.ListRoads(ctx, &api.ListRoadsRequest{})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*api.Road, len(resp.GetRoads()))
	for _, rd := range resp.GetRoads() {
		byID[rd.GetId()] = rd
	}

	var out []Feature
	for _, mr := range s.cfg.Roads.MonitoredRoads {
		// Include the segment if either endpoint is in the area.
		if !area.Bounds.Contains(mr.Origin.Latitude, mr.Origin.Longitude) &&
			!area.Bounds.Contains(mr.Destination.Latitude, mr.Destination.Longitude) {
			continue
		}
		rd := byID[mr.ID]

		// Draw the segment along the actual route (the decoded Google polyline
		// exposed on Road.polyline) so it follows the highway; fall back to a
		// straight origin→destination line when the polyline is unavailable.
		var geom *Geometry
		if rd != nil && len(rd.GetPolyline()) >= 2 {
			pts := make([]LatLng, len(rd.GetPolyline()))
			for i, c := range rd.GetPolyline() {
				pts[i] = LatLng{Lat: c.GetLatitude(), Lng: c.GetLongitude()}
			}
			geom = LineStringGeom(pts)
		}
		if geom == nil {
			geom = LineStringGeom([]LatLng{
				{Lat: mr.Origin.Latitude, Lng: mr.Origin.Longitude},
				{Lat: mr.Destination.Latitude, Lng: mr.Destination.Longitude},
			})
		}

		p := Properties{
			ID:        "road:" + mr.ID,
			Layer:     strings.ToUpper(LayerRoadSegment),
			Kind:      "Road segment",
			Headline:  strings.TrimSpace(mr.Name + " — " + mr.Section),
			AreaLabel: mr.Section,
			Source:    Source{ID: "google", Name: "Google Routes + Caltrans"},
			Road:      &RoadProps{RoadID: mr.ID},
		}
		sev := SevInfo
		if rd != nil {
			p.Status = strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_")
			p.Road.Congestion = strings.TrimPrefix(rd.GetCongestionLevel().String(), "CONGESTION_LEVEL_")
			p.Road.DelayMinutes = i32ptr(rd.GetDelayMinutes())
			p.Road.DurationMinutes = i32ptr(rd.GetDurationMinutes())
			p.Road.DistanceKm = i32ptr(rd.GetDistanceKm())
			sev = roadSeverity(rd)
			if e := rd.GetStatusExplanation(); e != "" {
				p.Description = e
			}
		}
		p.setSeverity(sev)
		out = append(out, Feature{Type: "Feature", Geometry: geom, Properties: p})
	}
	return out, nil
}
func (s *Service) fireWeather(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		return nil, err
	}
	var out []Feature

	// Banner: the region's ISSUED state (per-zone), colored by severity. Only for
	// areas whose zones the product covers.
	if fw := resp.GetFireWeather(); fw != nil && zonesMatch(area.Zones, fw.GetZones()) {
		state := strings.ToLower(strings.TrimPrefix(fw.GetState().String(), "FIRE_WEATHER_STATE_"))
		state = strings.ReplaceAll(state, "_", "-")
		p := Properties{
			ID:        "fw:region",
			Layer:     strings.ToUpper(LayerFireWeather),
			Kind:      "Fire weather",
			Category:  state,
			Headline:  nonEmpty(fw.GetHeadline(), "Fire weather: "+state),
			Effective: tsToRFC3339(fw.GetEffective()),
			Expires:   tsToRFC3339(fw.GetExpires()),
			Source:    Source{ID: "nws", Name: nonEmpty(fw.GetSenderName(), "National Weather Service")},
			FireWeather: &FireWeatherProps{
				State:       state,
				SourceEvent: fw.GetSourceEvent(),
				Zones:       fw.GetZones(),
			},
		}
		p.setSeverity(fromFireWeatherState(state))
		out = append(out, Feature{Type: "Feature", Geometry: nil, Properties: p}) // null geometry = banner
	}

	// Per-location forecast Points: geolocated wind/RH outlook, INFO severity —
	// informational, never an issued warning (see docs/fire-weather-forecast-design.md).
	// Fail-soft: LocationForecasts never errors, so a forecast outage just omits points.
	if forecasts := s.weather.LocationForecasts(ctx); len(forecasts) > 0 {
		for _, loc := range s.cfg.Weather.Locations {
			if !area.Bounds.Contains(loc.Coordinates.Latitude, loc.Coordinates.Longitude) {
				continue
			}
			if f := forecasts[loc.ID]; f != nil {
				out = append(out, forecastPointFeature(loc, f))
			}
		}
	}
	return out, nil
}

// forecastPointFeature builds the INFO-severity Point for a location's forecast
// summary. Severity is always INFO — a windy/dry forecast never colors like an
// issued Red Flag.
func forecastPointFeature(loc config.WeatherLocation, f *nws.Forecast) Feature {
	gust := int32(math.Round(f.PeakGustKmh))
	summary := &ForecastSummary{
		Source:          f.Source,
		IssuedAt:        rfc3339Time(f.IssuedAt),
		HorizonHours:    f.HorizonHours,
		PeakWindGustKmh: gust,
		PeakWindGustAt:  rfc3339Time(f.PeakGustAt),
	}
	if f.HasMinHumidity {
		summary.MinHumidityPercent = int32(math.Round(f.MinHumidityPct))
	}
	p := Properties{
		ID:        "fw:forecast:" + loc.ID,
		Layer:     strings.ToUpper(LayerFireWeather),
		Kind:      "Fire-weather forecast",
		AreaLabel: loc.Name,
		Headline:  fmt.Sprintf("%s forecast — peak gust %d km/h", loc.Name, gust),
		Source:    Source{ID: "nws", Name: "National Weather Service"},
		FireWeather: &FireWeatherProps{
			Forecast: summary,
		},
	}
	p.setSeverity(SevInfo)
	return Feature{Type: "Feature", Geometry: PointGeom(loc.Coordinates.Latitude, loc.Coordinates.Longitude), Properties: p}
}

// rfc3339Time renders a time as RFC 3339 UTC, "" for the zero value.
func rfc3339Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// normFireName normalizes an incident/perimeter name for joining CAL FIRE
// incidents and FIRIS perimeters (e.g. "Salt Springs Fire" and "Salt Springs" →
// "saltsprings").
func normFireName(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(strings.TrimSpace(s), " fire")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// roadSeverity derives a unified severity from a road's status + congestion.
func roadSeverity(rd *api.Road) string {
	switch rd.GetStatus() {
	case api.RoadStatus_CLOSED:
		return SevSevere
	case api.RoadStatus_RESTRICTED, api.RoadStatus_MAINTENANCE:
		return SevModerate
	}
	switch rd.GetCongestionLevel() {
	case api.CongestionLevel_SEVERE, api.CongestionLevel_HEAVY:
		return SevModerate
	case api.CongestionLevel_MODERATE:
		return SevMinor
	default:
		return SevInfo
	}
}

func tsToRFC3339(ts interface{ GetSeconds() int64 }) string {
	// Accept any *timestamppb.Timestamp via its getter; nil-safe.
	if ts == nil {
		return ""
	}
	secs := ts.GetSeconds()
	if secs == 0 {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339)
}

// i32ptr returns a pointer to v (for optional JSON numerics).
func i32ptr(v int32) *int32 { return &v }

// zonesMatch reports whether an alert belongs to an area, by NWS forecast zone:
//   - area has no configured zones    -> matches (unscoped single-area deployment)
//   - alert has no zones (e.g. OWM)   -> matches (can't be scoped; keep it)
//   - otherwise                       -> the zone sets intersect
func zonesMatch(areaZones, alertZones []string) bool {
	if len(areaZones) == 0 || len(alertZones) == 0 {
		return true
	}
	set := make(map[string]bool, len(areaZones))
	for _, z := range areaZones {
		set[z] = true
	}
	for _, z := range alertZones {
		if set[z] {
			return true
		}
	}
	return false
}
func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
