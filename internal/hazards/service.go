package hazards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/calfire"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/clients/caltrans"
	"github.com/dpup/info.ersn.net/server/internal/clients/usgs"
	"github.com/dpup/info.ersn.net/server/internal/clients/wfigs"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/services"
)

// RoadsAPI / WeatherAPI are the slices of the existing services the hazard layer
// re-projects. Interfaces keep the package testable; exported so external
// consumers (the /v1 grid API, the store→GeoJSON byte-compat harness) can
// construct the service with fakes via NewServiceWithAPIs.
type RoadsAPI interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
	ListIncidents(context.Context, *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error)
}
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
	ListWeatherAlerts(context.Context, *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error)
}

// StoreBackend serves an event-backed layer's already-projected features from
// the persistent grid event store (internal/store through the shared T13
// projection — see cmd/server/gridadapter.go). It is the strangler seam of
// docs/v2-implementation-plan.md T14: nil = live mode (builders + TTL cache,
// the shipped behavior); non-nil re-backs the five event-backed layers
// (wildfire, evacuation, weather_alert, earthquake, road_incident) onto the
// store. Conditions layers (road_segment, chain_control, fire_weather) never
// consult it.
//
// placeID is a place-directory id — configured hazard areas are seeded as
// "area:{id}", so the event_places join gives per-area scoping. status is the
// layer's source-registry health (OK | STALE | UNAVAILABLE); lastSourceUpdate
// is the most recent successful source fetch (zero when OK, or when the
// source never succeeded). features must be only ACTIVE/SCHEDULED events (the
// live-map read); err is reserved for the store itself being unreadable.
type StoreBackend interface {
	QueryActive(ctx context.Context, placeID, layer string) (features []Feature, status string, lastSourceUpdate time.Time, err error)
}

// Service re-projects the service's existing feeds into the unified GeoJSON
// hazard model and serves them at /api/v1/hazards/{area}/{layer}.geojson.
type Service struct {
	cfg      *config.Config
	roads    RoadsAPI
	weather  WeatherAPI
	caltrans *caltrans.FeedParser
	usgs     *usgs.Client
	calfire  *calfire.Client
	wfigs    *wfigs.Client
	caloes   *caloes.Client
	cache    *cache.Cache

	// storeBackend, when non-nil, re-backs the event-backed layers onto the
	// grid event store (T14); nil keeps every layer on its live builder.
	storeBackend StoreBackend

	// layerBuilders and layerOrder are derived once from layerRegistry() so the
	// dispatch map and the situation iteration order share one source of truth.
	layerBuilders map[string]builder
	layerOrder    []string
}

// NewService wires the hazard service to the existing services + clients. The
// new-upstream clients (USGS, CAL FIRE, WFIGS, ...) are keyless and constructed
// here. The shared cache is reused for stale-on-error resilience on the new
// upstreams (see buildLayer); pass nil to disable hazard-layer caching.
// No store backend is passed (nil = live mode); production wiring uses
// NewServiceWithAPIs to re-back the event layers onto the grid store.
func NewService(cfg *config.Config, roads *services.RoadsService, weather *services.WeatherService, ct *caltrans.FeedParser, c *cache.Cache) *Service {
	return NewServiceWithAPIs(cfg, roads, weather, ct, c)
}

// NewServiceWithAPIs is NewService with the roads/weather dependencies as
// their narrow interfaces, so consumers outside this package (the /v1 grid
// API, the byte-compat harness) can construct the service with fakes.
//
// storeBackend is an optional trailing argument (at most one is used): a
// non-nil backend serves the event-backed layers from the grid store instead
// of their live builders (see buildLayerFromStore). Omitting it — every
// pre-T14 call site — keeps live mode, byte-identical shipped behavior.
func NewServiceWithAPIs(cfg *config.Config, roads RoadsAPI, weather WeatherAPI, ct *caltrans.FeedParser, c *cache.Cache, storeBackend ...StoreBackend) *Service {
	s := &Service{
		cfg:      cfg,
		roads:    roads,
		weather:  weather,
		caltrans: ct,
		usgs:     usgs.NewClient(),
		calfire:  calfire.NewClient(),
		wfigs:    wfigs.NewClient(),
		caloes:   caloes.NewClient(),
		cache:    c,
	}
	if len(storeBackend) > 0 {
		s.storeBackend = storeBackend[0]
	}
	reg := s.layerRegistry()
	s.layerBuilders = make(map[string]builder, len(reg))
	s.layerOrder = make([]string, 0, len(reg))
	for _, e := range reg {
		s.layerBuilders[e.name] = e.build
		s.layerOrder = append(s.layerOrder, e.name)
	}
	return s
}

// WithClients overrides the keyless upstream clients (nil keeps the default)
// and returns s for chaining. The client fields are unexported, so this is
// the injection seam external tests use — the store→GeoJSON byte-compat
// harness feeds the real builders fixture HTTPDoer-backed clients through it.
// Not for production wiring: NewService's defaults are the real upstreams.
func (s *Service) WithClients(u *usgs.Client, cf *calfire.Client, wf *wfigs.Client, co *caloes.Client) *Service {
	if u != nil {
		s.usgs = u
	}
	if cf != nil {
		s.calfire = cf
	}
	if wf != nil {
		s.wfigs = wf
	}
	if co != nil {
		s.caloes = co
	}
	return s
}

// HandlerPrefix is where the layer endpoints mount.
const HandlerPrefix = "/api/v1/hazards/"

// builder produces a layer's features for an area. Returning an error makes the
// layer fail-loud (UNAVAILABLE metadata, empty features) rather than fabricating
// a clear state. A builder may return partialData(err) to keep its (incomplete)
// features while signalling STALE.
type builder func(ctx context.Context, area config.HazardArea) ([]Feature, error)

// layerEntry binds a layer name to its builder. layerRegistry is the single
// canonical list — both the dispatch map and the situation order derive from it,
// in this order.
type layerEntry struct {
	name  string
	build builder
}

func (s *Service) layerRegistry() []layerEntry {
	return []layerEntry{
		{LayerEvacuation, s.evacuations},
		{LayerWildfire, s.wildfires},
		{LayerRoadIncident, s.roadIncidents},
		{LayerChainControl, s.chainControls},
		{LayerWeatherAlert, s.weatherAlerts},
		{LayerFireWeather, s.fireWeather},
		{LayerEarthquake, s.earthquakes},
		{LayerRoadSegment, s.roadSegments},
	}
}

// ServeHTTP handles GET /api/v1/hazards/{area}/{layer}.geojson.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, HandlerPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".geojson") {
		http.Error(w, "not found: expected /api/v1/hazards/{area}/{layer}.geojson", http.StatusNotFound)
		return
	}
	areaID := parts[0]
	layer := strings.TrimSuffix(parts[1], ".geojson")

	area, ok := s.resolveArea(areaID)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown hazard area: %q", areaID), http.StatusNotFound)
		return
	}
	build, ok := s.layerBuilders[layer]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown hazard layer: %q", layer), http.StatusNotFound)
		return
	}

	ctx := r.Context()
	res := s.buildLayer(ctx, area, layer, build)

	fc := newCollection(res.features, &Metadata{
		Layer:            layer,
		Area:             areaID,
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		SourceStatus:     res.status,
		LastSourceUpdate: tsOrEmpty(res.lastSourceUpdate),
		Attribution:      res.meta.attribution,
		SourceURL:        res.meta.sourceURL,
		SchemaVersion:    schemaVersion,
	})

	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	if err := json.NewEncoder(w).Encode(fc); err != nil {
		logging.Errorw(ctx, "Failed to encode hazard GeoJSON", "error", err)
	}
}

// ScannersPrefix is where the scanner-config endpoint mounts.
const ScannersPrefix = "/api/v1/scanners/"

// scannerOut is the response shape for GET /api/v1/scanners/{area}. Note: no
// raw HTML `embed` field (the client builds the official Broadcastify iframe
// from feed_id) — see the security review.
type scannerOut struct {
	FeedID          string `json:"feed_id"`
	ChannelLabel    string `json:"channel_label"`
	Agency          string `json:"agency,omitempty"`
	BroadcastifyURL string `json:"broadcastify_url"`
}

// ServeScanners handles GET /api/v1/scanners/{area} from operator config.
func (s *Service) ServeScanners(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	areaID := parseAreaID(r.URL.Path, ScannersPrefix)
	area, ok := s.resolveArea(areaID)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown hazard area: %q", areaID), http.StatusNotFound)
		return
	}
	out := s.scanners(area)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(out)
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
// Both the single-layer endpoint and the situation aggregator go through here so
// the "empty never means all-clear" semantics can't drift between them.
//
// When a StoreBackend is configured, the event-backed layers short-circuit to
// buildLayerFromStore — the store replaces both the live builder AND the TTL
// cache for those layers (the store is the persisted last-good state).
//
// Live-mode status resolution:
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
// nothing (e.g. no active evacuation zones — still caveated via attribution +
// source_url, never a guarantee). The two are deliberately distinguishable.
func (s *Service) buildLayer(ctx context.Context, area config.HazardArea, layer string, build builder) layerResult {
	meta := layerMeta(layer)
	if s.storeBackend != nil && eventBackedLayer(layer) {
		return s.buildLayerFromStore(ctx, area, layer, meta)
	}
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

// eventBackedLayer reports whether a layer is served from the grid event
// store when a StoreBackend is configured (plan decision 5: the conditions
// layers — road_segment, chain_control, fire_weather — stay live projections
// of the roads/weather services and never touch the store).
func eventBackedLayer(layer string) bool {
	switch layer {
	case LayerWildfire, LayerEvacuation, LayerWeatherAlert, LayerEarthquake, LayerRoadIncident:
		return true
	}
	return false
}

// buildLayerFromStore serves an event-backed layer from the grid event store
// — the T14 strangler path replacing the live builder + TTL cache. The store
// persists the last-good state (richer than the old 5m in-memory cache: it
// survives restarts and never expires), and per-source registry health
// replaces the live fetch error as the fail-loud signal. Status mapping,
// implemented EXACTLY against the fail-loud table in CLAUDE.md:
//
//   - backend OK          -> OK. A clean empty is OK + 0 features (the
//     caveated confirmed-empty — "no active zones per Cal OES"), never
//     UNAVAILABLE. Mirrors "builder OK (incl. a clean empty) -> OK".
//   - backend STALE       -> STALE + last_source_update (the last good source
//     fetch). Mirrors "partialData / stale-on-error -> STALE, features kept".
//   - backend UNAVAILABLE + stored features -> STALE + last_source_update:
//     the store serves the persisted last-good data while the source is down.
//     Mirrors "builder hard error WITH a cached value -> STALE, last-good
//     features served" — the store IS that cache now.
//   - backend UNAVAILABLE, no stored features -> UNAVAILABLE + empty. The
//     source never succeeded, so nothing vouches for an empty feed. Mirrors
//     "builder hard error, nothing cached -> UNAVAILABLE, empty".
//   - backend error (store unreadable) -> UNAVAILABLE + empty (fail loud).
//
// This preserves "an error never becomes a 0": emptiness is only ever
// OK-empty (clean fetch reporting nothing) or UNAVAILABLE-empty
// (never-succeeded) — a source failure with stored data degrades to STALE
// with its data intact instead of fabricating a clear state.
func (s *Service) buildLayerFromStore(ctx context.Context, area config.HazardArea, layer string, meta layerMetadata) layerResult {
	// Configured hazard areas are seeded into the place directory as
	// "area:{id}" (plan decision 11: slug preserved), so the event_places
	// join scopes the query to this area — a second area never inherits the
	// first's events.
	features, status, lastUpdate, err := s.storeBackend.QueryActive(ctx, "area:"+area.ID, layer)
	if err != nil {
		logging.Errorw(ctx, "Store-backed hazard layer query failed", "layer", layer, "area", area.ID, "error", err)
		return finalize(meta, nil, "UNAVAILABLE", time.Time{})
	}
	mapped, mappedUpdate := DegradeStoreStatus(status, len(features) > 0, lastUpdate)
	if mapped == "UNAVAILABLE" {
		return finalize(meta, nil, "UNAVAILABLE", time.Time{})
	}
	if mapped == "STALE" && status != "STALE" {
		logging.Warnw(ctx, "Serving stored last-good hazard layer; source unavailable",
			"layer", layer, "area", area.ID, "last_source_update", tsOrEmpty(lastUpdate))
	}
	return finalize(meta, features, mapped, mappedUpdate)
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

func (s *Service) resolveArea(id string) (config.HazardArea, bool) {
	for _, a := range s.cfg.Hazards.Areas {
		if a.ID == id {
			return a, true
		}
	}
	return config.HazardArea{}, false
}

// --- layer builders (re-project existing feeds) ---

func (s *Service) roadIncidents(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.roads.ListIncidents(ctx, &api.ListIncidentsRequest{Area: area.IncidentArea})
	if err != nil {
		return nil, err
	}
	var out []Feature
	for _, in := range resp.GetIncidents() {
		loc := in.GetLocation()
		if loc == nil {
			continue
		}
		// Prefer the short condensed_summary as the headline; the long detail
		// text moves to description. Unenhanced incidents have no condensed
		// summary, so their detail text stays the headline (see the grid
		// road_incident normalizer — the two must agree for byte-compat).
		short := in.GetCondensedSummary()
		long := in.GetDescription()
		headline := short
		desc := ""
		if short != "" {
			desc = long
		} else {
			headline = long
		}
		p := Properties{
			ID:          "chp:" + in.GetId(),
			Layer:       LayerRoadIncident,
			Kind:        "Road incident",
			Category:    strings.ToLower(strings.TrimPrefix(in.GetType().String(), "ALERT_TYPE_")),
			Headline:    headline,
			Description: desc,
			Status:      strings.ToLower(strings.TrimPrefix(in.GetStatus().String(), "INCIDENT_STATUS_")),
			AreaLabel:   in.GetLocationDescription(),
			Source:      Source{ID: "chp", Name: "CHP / Caltrans", Attribution: "quickmap.dot.ca.gov"},
			Incident:    &IncidentProps{LogNumber: in.GetLogNumber()},
			Effective:   tsToRFC3339(in.GetStarted()),
			UpdatedAt:   tsToRFC3339(in.GetLastUpdated()),
		}
		p.setSeverity(fromAlertSeverity(in.GetSeverity()))
		out = append(out, Feature{Type: "Feature", Geometry: PointGeom(loc.GetLatitude(), loc.GetLongitude()), Properties: p})
	}
	return out, nil
}

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
			Layer:        LayerChainControl,
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
			Layer:     LayerRoadSegment,
			Kind:      "Road segment",
			Headline:  strings.TrimSpace(mr.Name + " — " + mr.Section),
			AreaLabel: mr.Section,
			Source:    Source{ID: "google", Name: "Google Routes + Caltrans"},
			Road:      &RoadProps{RoadID: mr.ID},
		}
		sev := SevInfo
		if rd != nil {
			p.Status = strings.ToLower(strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_"))
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

func (s *Service) weatherAlerts(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.weather.ListWeatherAlerts(ctx, &api.ListWeatherAlertsRequest{})
	if err != nil {
		return nil, err
	}
	var out []Feature
	for _, a := range resp.GetAlerts() {
		// Scope to this area's NWS zones (alerts carry zones, not geometry). A
		// zoneless alert (e.g. OpenWeatherMap) can't be scoped, so it's kept.
		if !zonesMatch(area.Zones, a.GetZones()) {
			continue
		}
		// M1: NWS zone polygons aren't fetched yet, so these are null-geometry
		// banner features (valid per the model).
		p := Properties{
			ID:          "wx:" + a.GetId(),
			Layer:       LayerWeatherAlert,
			Kind:        "Weather alert",
			Category:    a.GetEvent(),
			Headline:    nonEmpty(a.GetHeadline(), a.GetEvent()),
			Description: a.GetDescription(),
			Effective:   tsToRFC3339(a.GetStartTime()),
			Expires:     tsToRFC3339(a.GetEndTime()),
			Source:      Source{ID: strings.ToLower(a.GetSource().String()), Name: a.GetSenderName()},
			Weather: &WeatherProps{
				Event:  a.GetEvent(),
				Source: a.GetSource().String(),
				Zones:  a.GetZones(),
			},
		}
		p.setSeverity(fromAlertSeverity(a.GetSeverity()))
		out = append(out, Feature{Type: "Feature", Geometry: nil, Properties: p})
	}
	return out, nil
}

func (s *Service) fireWeather(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		return nil, err
	}
	fw := resp.GetFireWeather()
	if fw == nil {
		return nil, nil
	}
	// Fire-weather products are issued per NWS zone; only surface it for areas
	// whose zones the product covers.
	if !zonesMatch(area.Zones, fw.GetZones()) {
		return nil, nil
	}
	state := strings.ToLower(strings.TrimPrefix(fw.GetState().String(), "FIRE_WEATHER_STATE_"))
	state = strings.ReplaceAll(state, "_", "-")
	p := Properties{
		ID:        "fw:region",
		Layer:     LayerFireWeather,
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
	// Region-wide, so null geometry (banner).
	return []Feature{{Type: "Feature", Geometry: nil, Properties: p}}, nil
}

func (s *Service) earthquakes(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	quakes, err := s.usgs.GetEarthquakes(ctx, usgs.Bounds{
		MinLatitude:  area.Bounds.MinLatitude,
		MaxLatitude:  area.Bounds.MaxLatitude,
		MinLongitude: area.Bounds.MinLongitude,
		MaxLongitude: area.Bounds.MaxLongitude,
	}, 2.5, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	var out []Feature
	for _, q := range quakes {
		p := Properties{
			ID:        "usgs:" + q.ID,
			Layer:     LayerEarthquake,
			Kind:      "Earthquake",
			Category:  "earthquake",
			Headline:  fmt.Sprintf("M%.1f — %s", q.Magnitude, q.Place),
			AreaLabel: q.Place,
			Source:    Source{ID: "usgs", Name: "USGS", URL: safeURL(q.URL), Attribution: "U.S. Geological Survey"},
			Earthquake: &EarthquakeProps{
				Magnitude: q.Magnitude,
				DepthKm:   q.DepthKm,
				Felt:      q.Felt,
			},
		}
		if !q.Time.IsZero() {
			p.Effective = q.Time.Format(time.RFC3339)
		}
		if !q.Updated.IsZero() {
			p.UpdatedAt = q.Updated.Format(time.RFC3339)
		}
		p.setSeverity(fromMagnitude(q.Magnitude))
		out = append(out, Feature{Type: "Feature", Geometry: PointGeom(q.Lat, q.Lng), Properties: p})
	}
	return out, nil
}

func (s *Service) wildfires(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	bounds := wfigs.Bounds{
		MinLatitude:  area.Bounds.MinLatitude,
		MaxLatitude:  area.Bounds.MaxLatitude,
		MinLongitude: area.Bounds.MinLongitude,
		MaxLongitude: area.Bounds.MaxLongitude,
	}
	// Fetch the two independent sources concurrently — sequential fetches stack
	// their timeouts (up to ~45s) inside the /situation fan-out.
	var (
		incidents  []calfire.Incident
		perims     []wfigs.Perimeter
		ierr, perr error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); incidents, ierr = s.calfire.GetActiveIncidents(ctx) }()
	go func() { defer wg.Done(); perims, perr = s.wfigs.GetPerimeters(ctx, bounds) }()
	wg.Wait()

	if ierr != nil && perr != nil {
		return nil, fmt.Errorf("both wildfire sources failed: calfire=%v wfigs=%v", ierr, perr)
	}
	// One source failed: build from the survivor but flag the layer STALE
	// (degraded) via partialData so consumers don't read partial data as complete.
	var partialErr error
	if ierr != nil {
		logging.Warnw(ctx, "CAL FIRE incident source failed; wildfire layer is partial (WFIGS perimeters only)", "error", ierr)
		partialErr = fmt.Errorf("CAL FIRE incidents unavailable: %w", ierr)
	}
	if perr != nil {
		logging.Warnw(ctx, "WFIGS perimeter source failed; wildfire layer is partial (CAL FIRE incidents only)", "error", perr)
		partialErr = fmt.Errorf("WFIGS perimeters unavailable: %w", perr)
	}

	// Index perimeters by normalized name so a CAL FIRE incident can adopt its
	// polygon geometry (join on incident name).
	byName := make(map[string]wfigs.Perimeter, len(perims))
	ambiguous := make(map[string]bool)
	for _, p := range perims {
		n := normFireName(p.Name)
		if _, seen := byName[n]; seen {
			// Two distinct perimeters normalize to the same name — don't let an
			// incident adopt an arbitrary one (wrong-geometry risk); emit both as
			// standalone polygons instead.
			ambiguous[n] = true
		}
		byName[n] = p
	}
	used := make(map[string]bool)

	var out []Feature
	for _, in := range incidents {
		if in.Lat == 0 && in.Lng == 0 {
			continue
		}
		if !area.Bounds.Contains(in.Lat, in.Lng) {
			continue
		}
		wf := &WildfireProps{
			Acres:       in.Acres,
			Containment: in.PercentContained,
			County:      in.County,
		}
		p := Properties{
			ID:        "calfire:" + nonEmpty(in.UniqueID, normFireName(in.Name)),
			Layer:     LayerWildfire,
			Kind:      "Wildfire",
			Category:  "wildfire",
			Headline:  fmt.Sprintf("%s — %.0f ac, %d%% contained", in.Name, in.Acres, in.PercentContained),
			AreaLabel: nonEmpty(in.Location, in.County),
			Status:    "active",
			Effective: tsOrEmpty(in.Started),
			UpdatedAt: tsOrEmpty(in.Updated),
			Source:    Source{ID: "calfire", Name: "CAL FIRE", URL: safeURL(in.URL), Attribution: "CAL FIRE / WFIGS"},
			Wildfire:  wf,
		}
		p.setSeverity(fromWildfire(in.PercentContained))
		// Adopt the matching perimeter polygon if we have an unambiguous one; else
		// a point. An ambiguous name (multiple distinct perimeters) is left for the
		// standalone pass rather than risk adopting the wrong polygon.
		n := normFireName(in.Name)
		if perim, ok := byName[n]; ok && !ambiguous[n] {
			used[n] = true
			wf.HasPerimeter = true
			out = append(out, Feature{Type: "Feature", Geometry: RawGeom(perim.GeometryType, perim.GeometryCoords), Properties: p})
		} else {
			out = append(out, Feature{Type: "Feature", Geometry: PointGeom(in.Lat, in.Lng), Properties: p})
		}
	}

	// Emit perimeters that didn't match a CAL FIRE incident as standalone
	// polygons (don't drop mapped fires CAL FIRE's curated list omits). Index the
	// ID so two perimeters sharing a normalized name stay distinct.
	for i, perim := range perims {
		if used[normFireName(perim.Name)] || perim.GeometryType == "" {
			continue
		}
		p := Properties{
			ID:       fmt.Sprintf("wfigs:%s:%d", normFireName(perim.Name), i),
			Layer:    LayerWildfire,
			Kind:     "Wildfire",
			Category: "wildfire",
			Headline: fmt.Sprintf("%s — %.0f ac, %d%% contained", perim.Name, perim.Acres, perim.PercentContained),
			Status:   "active",
			Source:   Source{ID: "wfigs", Name: "NIFC WFIGS", Attribution: "NIFC / WFIGS"},
			Wildfire: &WildfireProps{Acres: perim.Acres, Containment: perim.PercentContained, Cause: perim.Cause, HasPerimeter: true},
		}
		p.setSeverity(fromWildfire(perim.PercentContained))
		out = append(out, Feature{Type: "Feature", Geometry: RawGeom(perim.GeometryType, perim.GeometryCoords), Properties: p})
	}
	if partialErr != nil {
		return out, partialData(partialErr)
	}
	return out, nil
}

func (s *Service) evacuations(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	zones, err := s.caloes.GetActiveEvacuations(ctx, caloes.Bounds{
		MinLatitude:  area.Bounds.MinLatitude,
		MaxLatitude:  area.Bounds.MaxLatitude,
		MinLongitude: area.Bounds.MinLongitude,
		MaxLongitude: area.Bounds.MaxLongitude,
	})
	if err != nil {
		return nil, err
	}
	var out []Feature
	for _, z := range zones {
		level := normalizeEvacLevel(z.Status)
		if level == "" {
			continue // only surface active Order/Warning/Advisory/SIP zones
		}
		if !evacStatusRecognized(z.Status) {
			// Conservatively classified as WARNING by the fail-loud default. Log so
			// the phrasing can be added to normalizeEvacLevel explicitly.
			logging.Warnw(ctx, "Unrecognized Cal OES evacuation STATUS; defaulted to WARNING",
				"status", z.Status, "zone", nonEmpty(z.ZoneID, z.ZoneName), "county", z.County)
		}
		human := titleCase(strings.ToLower(strings.ReplaceAll(level, "_", " ")))
		p := Properties{
			ID:          "evac:" + nonEmpty(z.ZoneID, z.ZoneName),
			Layer:       LayerEvacuation,
			Kind:        "Evacuation",
			Category:    strings.ToLower(level),
			Headline:    fmt.Sprintf("Evacuation %s — %s", human, nonEmpty(z.ZoneName, z.County)),
			Description: z.PublicInfo,
			Status:      level,
			AreaLabel:   nonEmpty(z.ZoneName, z.County),
			UpdatedAt:   tsOrEmpty(z.LastUpdated),
			Source:      Source{ID: "caloes", Name: "Cal OES", URL: caloes.SourceURL, Attribution: "Cal OES — reference only"},
			Evacuation:  &EvacuationProps{ZoneID: z.ZoneID, Level: level, EventType: z.EventType, County: z.County},
		}
		p.setSeverity(fromEvacLevel(level))
		out = append(out, Feature{Type: "Feature", Geometry: RawGeom(z.GeometryType, z.GeometryCoords), Properties: p})
	}
	return out, nil
}

// normFireName normalizes an incident/perimeter name for joining CAL FIRE and
// WFIGS (e.g. "Salt Springs Fire" and "Salt Springs" → "saltsprings").
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

// tsOrEmpty formats a time.Time as RFC3339, or "" if zero.
func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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

// parseAreaID extracts the {area} segment from a single-segment endpoint path
// (the /scanners/ and /situation/ handlers; /hazards/ is two-segment).
func parseAreaID(path, prefix string) string {
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

// titleCase upper-cases the first letter of each space-separated word (ASCII).
// Replaces the deprecated strings.Title; inputs are known evac-level words.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
