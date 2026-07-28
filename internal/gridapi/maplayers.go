package gridapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// GET /api/v1/places/{place}/map/{layer}.geojson (plan §2.3, task T12b-1).
//
// Event-backed layers (wildfire, evacuation, weather_alert, earthquake,
// road_incident) are served from the store via the shared T13 projection, with
// metadata.source_status derived from the layer's source registry rows.
// Condition-backed layers (road_segment, chain_control, fire_weather) stay
// live projections: they delegate to the hazards builders through the narrow
// hazardsBuilder interface. Both paths emit the SHIPPED FeatureCollection
// envelope so /v1 map clients and /api/v1/hazards clients read one schema.

// mapSchemaVersion mirrors the shipped hazards metadata schema_version (the
// unexported hazards const); bump both on a breaking envelope change.
const mapSchemaVersion = 1

// eventLayers maps the shipped layer slug of each event-backed map layer onto
// its store layer enum. Absence from this map means the layer is not served
// from the store.
var eventLayers = map[string]gridv1.Layer{
	hazards.LayerWildfire:     gridv1.Layer_WILDFIRE,
	hazards.LayerEvacuation:   gridv1.Layer_EVACUATION,
	hazards.LayerWeatherAlert: gridv1.Layer_WEATHER_ALERT,
	hazards.LayerEarthquake:   gridv1.Layer_EARTHQUAKE,
	hazards.LayerRoadIncident: gridv1.Layer_ROAD_INCIDENT,
	hazards.LayerMesh:         gridv1.Layer_MESH,
}

// conditionLayers is the set of layers that remain live projections of the
// roads/weather services (plan decision 5) and delegate to hazards builders.
var conditionLayers = map[string]bool{
	hazards.LayerRoadSegment:  true,
	hazards.LayerChainControl: true,
	hazards.LayerFireWeather:  true,
}

// layerSourceIDs maps each event-backed layer slug onto the source registry
// rows whose health decides the layer's metadata.source_status (plan
// decision 4: poller ≠ source — wildfire and road_incident aggregate two
// feeds each).
var layerSourceIDs = map[string][]string{
	hazards.LayerWildfire:     {"calfire", "firis"},
	hazards.LayerEvacuation:   {"caloes"},
	hazards.LayerWeatherAlert: {"nws"},
	hazards.LayerEarthquake:   {"usgs"},
	hazards.LayerRoadIncident: {"chp", "caltrans"},
	hazards.LayerMesh:         {"meshcore"},
	hazards.LayerMeshLink:     {"meshcore"},
}

// hazardsBuilder is the slice of *hazards.Service the condition-backed layers
// consume. A narrow local interface keeps the delegation fakeable in tests
// (the package convention); *hazards.Service satisfies it via BuildLayer.
type hazardsBuilder interface {
	BuildLayer(ctx context.Context, area config.HazardArea, layer string) (features []hazards.Feature, status string, lastSourceUpdate time.Time, attribution, sourceURL string, ok bool)
}

var _ hazardsBuilder = (*hazards.Service)(nil)

// serveMapLayer handles GET /api/v1/places/{place}/map/{layer}.geojson.
func (s *Service) serveMapLayer(w http.ResponseWriter, r *http.Request, placeKey, layer string) {
	ctx := r.Context()
	place, err := s.Store.GetPlace(ctx, placeKey)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, fmt.Sprintf("unknown place: %q", placeKey))
		return
	}
	if err != nil {
		internal(ctx, w, err)
		return
	}

	switch {
	case layer == hazards.LayerMeshLink:
		s.serveMeshLinkLayer(w, r, place)
	case eventLayers[layer] != gridv1.Layer_LAYER_UNSPECIFIED:
		s.serveEventLayer(w, r, place, layer)
	case conditionLayers[layer]:
		var hb hazardsBuilder
		if s.Hazards != nil {
			// A nil *hazards.Service must stay a nil INTERFACE, not a typed nil
			// (BuildLayer would nil-deref inside the map lookup).
			hb = s.Hazards
		}
		s.serveConditionLayer(w, r, hb, place, layer)
	default:
		notFound(w, fmt.Sprintf("unknown map layer: %q", layer))
	}
}

// serveEventLayer serves an event-backed layer from the store: the place's
// ACTIVE+SCHEDULED events (the live-map lifecycle read — resolved/expired
// events belong to /api/v1/history), projected onto the shipped envelope.
func (s *Service) serveEventLayer(w http.ResponseWriter, r *http.Request, place *gridv1.Place, layer string) {
	ctx := r.Context()
	q := store.EventQuery{
		PlaceID:  place.GetId(),
		Layers:   []gridv1.Layer{eventLayers[layer]},
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED},
		PageSize: 200, // the store max; the keyset loop below drains any overflow
	}
	var events []*gridv1.Event
	for {
		page, next, err := s.Store.QueryEvents(ctx, q)
		if err != nil {
			internal(ctx, w, err)
			return
		}
		events = append(events, page...)
		if next == "" {
			break
		}
		q.PageToken = next
	}

	sources, err := s.Store.ListSources(ctx)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	status, lastUpdate := LayerSourceStatus(sources, layer)
	// Envelope parity with the store-backed /api/v1/hazards path (plan §2.3):
	// a down source whose events are still stored degrades to STALE with the
	// data — hazards.DegradeStoreStatus is the same mapping
	// buildLayerFromStore applies, so the two surfaces can never answer
	// differently for one store state, and UNAVAILABLE (a contract-level
	// "draw nothing") is only ever emitted with empty features.
	status, lastUpdate = hazards.DegradeStoreStatus(status, len(events) > 0, lastUpdate)
	attribution, sourceURL := eventLayerMeta(layer)
	s.writeFeatureCollection(w, r, ProjectEvents(layer, events), &hazards.Metadata{
		Layer:            layer,
		Area:             place.GetSlug(),
		GeneratedAt:      s.Now().UTC().Format(time.RFC3339),
		SourceStatus:     status,
		LastSourceUpdate: timeOrEmpty(lastUpdate),
		Attribution:      attribution,
		SourceURL:        sourceURL,
		SchemaVersion:    mapSchemaVersion,
	})
}

// serveConditionLayer serves a condition-backed layer by delegating to the
// hazards builders (live projections of the roads/weather services). hb is
// passed explicitly so tests can drive this path with a fake; the router
// hands it s.Hazards.
func (s *Service) serveConditionLayer(w http.ResponseWriter, r *http.Request, hb hazardsBuilder, place *gridv1.Place, layer string) {
	if hb == nil {
		// Wiring gap (main.go constructs the Service without a hazards service):
		// fail loud rather than fabricate an empty-but-OK layer.
		notImplemented(w, "condition-backed map layers are not wired")
		return
	}
	area, covered := s.resolveHazardArea(place)
	// fire_weather is zone-scoped; an out-of-coverage place has no fire-weather
	// zone, so serve a clean empty layer rather than another region's product.
	if layer == hazards.LayerFireWeather && !covered {
		s.writeFeatureCollection(w, r, nil, &hazards.Metadata{
			Layer: layer, Area: place.GetSlug(),
			GeneratedAt:  s.Now().UTC().Format(time.RFC3339),
			SourceStatus: "OK", SchemaVersion: mapSchemaVersion,
		})
		return
	}
	features, status, lastUpdate, attribution, sourceURL, ok := hb.BuildLayer(r.Context(), area, layer)
	if !ok {
		// Unreachable via the router (conditionLayers gates), kept for safety.
		notFound(w, fmt.Sprintf("unknown map layer: %q", layer))
		return
	}
	s.writeFeatureCollection(w, r, features, &hazards.Metadata{
		Layer:            layer,
		Area:             place.GetSlug(),
		GeneratedAt:      s.Now().UTC().Format(time.RFC3339),
		SourceStatus:     status,
		LastSourceUpdate: timeOrEmpty(lastUpdate),
		Attribution:      attribution,
		SourceURL:        sourceURL,
		SchemaVersion:    mapSchemaVersion,
	})
}

// resolveHazardArea maps a place onto the config.HazardArea the hazards
// builders scope by. An AREA place is (by the seed contract) a configured
// hazards area whose id is the place slug — use that entry verbatim. Any
// other kind (county, town, corridor, ...) has no config entry, so one is
// SYNTHESIZED: bounds come from the place geometry's bbox, and the
// config-only knobs a bbox cannot supply are INHERITED from the configured
// areas whose bounds intersect it — Zones as the union (weather layers scope
// by NWS zone, not coordinates) and IncidentArea from the first intersecting
// area (the roads incident feed is per configured region). A stale AREA row
// whose config entry was removed falls through to the same synthesis.
// resolveHazardArea returns the config.HazardArea for a place and whether the
// place is within the service's configured hazard coverage. `covered` is false
// for a synthesized place (a non-AREA place, or an AREA with no matching config)
// that intersects no configured area — such a place has no NWS-zone
// relationship, so zone-scoped condition layers (fire_weather) must serve
// nothing rather than fall through zonesMatch's empty-zones "match all" and
// inherit an unrelated region's fire weather.
func (s *Service) resolveHazardArea(place *gridv1.Place) (config.HazardArea, bool) {
	if place.GetKind() == gridv1.PlaceKind_AREA {
		for _, a := range s.Cfg.Hazards.Areas {
			if a.ID == place.GetSlug() {
				return a, true
			}
		}
	}

	// Places are seeded with Geometry.Bbox always populated; a geometry-less
	// place degrades to a zero box that contains nothing (unlocatable is not
	// "everywhere" — the conditions passthroughs apply the same rule).
	b := place.GetGeometry().GetBbox()
	area := config.HazardArea{
		ID:   place.GetSlug(),
		Name: place.GetName(),
		Bounds: config.GeoBounds{
			MinLatitude:  b.GetMinLat(),
			MaxLatitude:  b.GetMaxLat(),
			MinLongitude: b.GetMinLng(),
			MaxLongitude: b.GetMaxLng(),
		},
	}
	seen := make(map[string]bool)
	covered := false
	for _, a := range s.Cfg.Hazards.Areas {
		if !geojson.BboxIntersects(
			a.Bounds.MinLatitude, a.Bounds.MinLongitude, a.Bounds.MaxLatitude, a.Bounds.MaxLongitude,
			area.Bounds.MinLatitude, area.Bounds.MinLongitude, area.Bounds.MaxLatitude, area.Bounds.MaxLongitude) {
			continue
		}
		covered = true
		if area.IncidentArea == "" {
			area.IncidentArea = a.IncidentArea
		}
		for _, z := range a.Zones {
			if !seen[z] {
				seen[z] = true
				area.Zones = append(area.Zones, z)
			}
		}
	}
	return area, covered
}

// LayerSourceStatus derives an event-backed layer's metadata.source_status
// from its source registry rows (the fail-loud honesty mechanism now that the
// store, not a live fetch, backs these layers). Single-source layers map
// directly; multi-source layers (wildfire = calfire+firis, road_incident =
// chp+caltrans): all OK -> OK, all down -> UNAVAILABLE, otherwise (some
// degraded) -> STALE — partial data must not present as complete, matching
// the shipped partialData semantics. A missing or never-polled row counts as
// down: health unknown is not OK.
//
// lastUpdate is zero when OK (freshness needs no caveat) and otherwise the
// most recent last_success_at across the layer's sources — zero if none ever
// succeeded, which the metadata omits.
func LayerSourceStatus(sources []*gridv1.Source, layer string) (status string, lastUpdate time.Time) {
	ids := layerSourceIDs[layer]
	if len(ids) == 0 {
		// Not an event-backed layer: no source rows can vouch for it.
		return "UNAVAILABLE", time.Time{}
	}
	byID := make(map[string]*gridv1.Source, len(sources))
	for _, src := range sources {
		byID[src.GetId()] = src
	}

	okCount, downCount := 0, 0
	var lastSuccess time.Time
	for _, id := range ids {
		src := byID[id]
		st := gridv1.SourceStatus_UNAVAILABLE // unseeded row: nothing vouches for the feed
		if src != nil {
			st = src.GetStatus()
			if st == gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED {
				st = gridv1.SourceStatus_UNAVAILABLE // seeded but never polled
			}
			if ts := src.GetLastSuccessAt(); ts != nil && ts.AsTime().After(lastSuccess) {
				lastSuccess = ts.AsTime()
			}
		}
		switch st {
		case gridv1.SourceStatus_OK:
			okCount++
		case gridv1.SourceStatus_UNAVAILABLE:
			downCount++
		}
	}
	switch {
	case okCount == len(ids):
		return "OK", time.Time{}
	case downCount == len(ids):
		return "UNAVAILABLE", lastSuccess
	default:
		return "STALE", lastSuccess
	}
}

// eventLayerMeta mirrors hazards.layerMeta for the store-backed layers:
// evacuation metadata carries the Cal OES attribution + the authoritative
// Genasys source_url in EVERY state — OK, STALE and UNAVAILABLE alike
// (life-safety framing: "no active zones per Cal OES" is a caveated
// confirmed-empty, never a guarantee). Other layers carry per-feature source
// blocks instead.
func eventLayerMeta(layer string) (attribution, sourceURL string) {
	if layer == hazards.LayerEvacuation {
		return "Cal OES / California County Governments — reference only", caloes.SourceURL
	}
	return "", ""
}

// writeFeatureCollection emits the shipped GeoJSON envelope through the
// shared ETag/304 path with the map-layer cache policy (60s, matching
// /api/v1/hazards). A nil features slice serializes as [] — an empty layer is
// still a valid FeatureCollection.
func (s *Service) writeFeatureCollection(w http.ResponseWriter, r *http.Request, features []hazards.Feature, md *hazards.Metadata) {
	if features == nil {
		features = []hazards.Feature{}
	}
	body, err := json.Marshal(hazards.FeatureCollection{Type: "FeatureCollection", Features: features, Metadata: md})
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	writeJSON(w, r, body, "application/geo+json", maxAgeConditions)
}

// timeOrEmpty renders a time as RFC 3339 UTC, "" for zero (the shipped
// metadata omit-when-zero convention).
func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
