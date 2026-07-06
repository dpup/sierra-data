package gridapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// GET /v1/places/{place}/summary (plan §2.3/§2.4, task T12b-2): the one-call
// place rollup — mode (QUIET/WATCH/ACTIVE), a severity summary over the
// place's live events, per-domain rollups (event layers merged with the live
// condition layers), the evacuation invariant (int MAY be 0; UNAVAILABLE is an
// explicit null — an error never becomes a 0), and a source-health sidecar.
//
// The response is a hand-built snake_case JSON document (NOT protojson): the
// evac null must be an explicit JSON null, exactly like /api/v1/situation.

// Mode values (plan §2.4). Ordered by escalation; the string is the wire value.
const (
	ModeQuiet  = "QUIET"
	ModeWatch  = "WATCH"
	ModeActive = "ACTIVE"
)

// summaryDomains is the fixed domain order on the wire (plan §2.4).
var summaryDomains = []string{"fire", "evacuation", "weather", "roads", "seismic"}

// summaryResponse is the exact plan §2.3 JSON shape the site codes against.
type summaryResponse struct {
	Place       string         `json:"place"`
	PlaceID     string         `json:"place_id"`
	PlaceName   string         `json:"place_name"`
	GeneratedAt string         `json:"generated_at"`
	Mode        string         `json:"mode"`
	Summary     summaryBlock   `json:"summary"`
	Domains     []domainBlock  `json:"domains"`
	Sources     []sourceHealth `json:"sources"`
}

type summaryBlock struct {
	HighestSeverity     string         `json:"highest_severity"`
	HighestSeverityRank int            `json:"highest_severity_rank"`
	SeverityCounts      map[string]int `json:"severity_counts"`
	TotalActive         int            `json:"total_active"`
	// ActiveEvacuations is the count of ACTIVE evacuation events in this place
	// while the Cal OES source is OK/STALE (MAY be 0 — a caveated
	// confirmed-empty), and an explicit null while UNAVAILABLE: a client MUST
	// render null as "unknown — check Genasys", never as zero.
	ActiveEvacuations *int       `json:"active_evacuations"`
	EvacuationStatus  string     `json:"evacuation_status"`
	TopEvents         []topEvent `json:"top_events"`
}

type topEvent struct {
	ID           string `json:"id"`
	Layer        string `json:"layer"` // lowercase layer slug ("weather_alert")
	Severity     string `json:"severity"`
	SeverityRank int    `json:"severity_rank"`
	Headline     string `json:"headline"`
	Source       string `json:"source"` // provenance source id ("usgs")
}

type domainBlock struct {
	Domain          string           `json:"domain"`
	Status          string           `json:"status"` // worst source_status across the domain's layers
	HighestSeverity string           `json:"highest_severity"`
	ActiveCount     int              `json:"active_count"`
	Headlines       []domainHeadline `json:"headlines"` // top 3 by severity
}

type domainHeadline struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Headline string `json:"headline"`
}

// sourceHealth is one registry row's health (id, status, last_success_at).
type sourceHealth struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	LastSuccessAt string `json:"last_success_at,omitempty"` // omitted when never succeeded
}

// ModeInputs distills a place's live signals for ComputeMode. Severities are
// over ACTIVE events only — SCHEDULED watches inform the domains/summary but
// never escalate the mode (the plan's rules say "active").
type ModeInputs struct {
	// ActiveEvacLevels are the coded levels (ORDER|WARNING|ADVISORY|
	// SHELTER_IN_PLACE) of the place's ACTIVE evacuation events.
	ActiveEvacLevels []string
	// EvacUnavailable is true when the Cal OES source is UNAVAILABLE — unknown
	// evacuation state is never quiet.
	EvacUnavailable bool
	// MaxActiveSeverity is the highest severity across ALL of the place's
	// ACTIVE events (any layer). Zero value INFO when there are none.
	MaxActiveSeverity gridv1.Severity
	// MaxWildfireSeverity is the highest severity across ACTIVE wildfire
	// events only (wildfire SEVERE escalates to ACTIVE, unlike other layers).
	MaxWildfireSeverity gridv1.Severity
	// FireWeatherState is the fire_weather condition state
	// (normal|elevated|red-flag), "" when unknown.
	FireWeatherState string
	// AnyLayerUnavailable is true when any layer's source_status is
	// UNAVAILABLE (event layers via source rows, condition layers via their
	// builders).
	AnyLayerUnavailable bool
}

// ComputeMode implements the plan §2.4 mode rules exactly. Pure — no I/O, no
// clock — so every branch is unit-testable.
//
//   - ACTIVE: any active evacuation (ORDER/WARNING/SHELTER_IN_PLACE), or any
//     active event EXTREME, or wildfire SEVERE.
//   - WATCH: any active SEVERE, or evac ADVISORY, or fire_weather
//     elevated/red-flag, or any layer UNAVAILABLE while another signal is
//     >= MODERATE. An UNAVAILABLE evac source forces mode >= WATCH — unknown
//     is never quiet.
//   - QUIET: otherwise.
func ComputeMode(in ModeInputs) string {
	// Life-safety bias: ingest normalizes levels to the four coded values, but
	// if an unrecognized ACTIVE level ever reaches here it escalates (only
	// ADVISORY is explicitly the lesser signal).
	for _, lvl := range in.ActiveEvacLevels {
		if lvl != "ADVISORY" {
			return ModeActive
		}
	}
	if in.MaxActiveSeverity >= gridv1.Severity_EXTREME {
		return ModeActive
	}
	if in.MaxWildfireSeverity >= gridv1.Severity_SEVERE {
		return ModeActive
	}

	if in.MaxActiveSeverity >= gridv1.Severity_SEVERE {
		return ModeWatch
	}
	for _, lvl := range in.ActiveEvacLevels {
		if lvl == "ADVISORY" {
			return ModeWatch
		}
	}
	switch strings.ToLower(in.FireWeatherState) {
	case "elevated", "red-flag":
		return ModeWatch
	}
	if in.EvacUnavailable {
		return ModeWatch
	}
	// Any dark layer while something >= MODERATE is going on: the picture is
	// incomplete exactly when it matters. (An elevated/red-flag fire-weather
	// companion signal already returned WATCH above, so the remaining signal
	// is the active-event severity.)
	if in.AnyLayerUnavailable && in.MaxActiveSeverity >= gridv1.Severity_MODERATE {
		return ModeWatch
	}
	return ModeQuiet
}

// serveSummary handles GET /v1/places/{place}/summary, binding the concrete
// hazards service; serveSummaryWith carries the fakeable seam (the
// serveConditionLayer convention).
func (s *Service) serveSummary(w http.ResponseWriter, r *http.Request, placeKey string) {
	var hb hazardsBuilder
	if s.Hazards != nil {
		// Keep a nil *hazards.Service a nil INTERFACE (see serveMapLayer).
		hb = s.Hazards
	}
	s.serveSummaryWith(w, r, hb, placeKey)
}

func (s *Service) serveSummaryWith(w http.ResponseWriter, r *http.Request, hb hazardsBuilder, placeKey string) {
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

	// The place's live event set: ACTIVE+SCHEDULED (the "what's happening now"
	// read the map layers serve), drained across pages in canonical store
	// order (severity DESC, observed_at DESC, id ASC).
	q := store.EventQuery{
		PlaceID:  place.GetId(),
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED},
		PageSize: 200, // the store max; the keyset loop drains any overflow
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

	// Condition layers (live projections of the roads/weather services): fetch
	// once each, feeding the roads/fire domain merges and the fire-weather mode
	// signal. The three builds do independent upstream I/O, so fan them out
	// concurrently (mirroring the /situation handler) rather than paying the sum
	// of their latencies on the request path.
	area, covered := s.resolveHazardArea(place)
	// fire_weather is zone-scoped: an out-of-coverage place inherits no region's
	// product — a confirmed-empty OK (not "" which worstStatus ranks UNAVAILABLE),
	// contributing nothing to the fire domain or the mode signal.
	condChain, condSegment := conditionResult{}, conditionResult{}
	condFireWx := conditionResult{status: "OK"}
	var cwg sync.WaitGroup
	cwg.Add(2)
	go func() { defer cwg.Done(); condChain = buildCondition(r, hb, area, hazards.LayerChainControl) }()
	go func() { defer cwg.Done(); condSegment = buildCondition(r, hb, area, hazards.LayerRoadSegment) }()
	if covered {
		cwg.Add(1)
		go func() { defer cwg.Done(); condFireWx = buildCondition(r, hb, area, hazards.LayerFireWeather) }()
	}
	cwg.Wait()

	resp := summaryResponse{
		Place:       place.GetSlug(),
		PlaceID:     place.GetId(),
		PlaceName:   place.GetName(),
		GeneratedAt: s.Now().UTC().Format(time.RFC3339),
	}

	// --- summary block (store events only; conditions live in domains) ---
	sb := summaryBlock{
		HighestSeverity: gridv1.Severity_INFO.String(), // default when no events
		SeverityCounts:  map[string]int{},
		TotalActive:     len(events),
		TopEvents:       []topEvent{},
	}
	for _, ev := range events {
		sev := ev.GetSeverity()
		sb.SeverityCounts[sev.String()]++
		if int(sev.Number()) >= sb.HighestSeverityRank {
			sb.HighestSeverityRank = int(sev.Number())
			sb.HighestSeverity = sev.String()
		}
	}
	sb.TopEvents = topEventsFrom(events, 5)

	byLayer := map[gridv1.Layer][]*gridv1.Event{}
	for _, ev := range events {
		byLayer[ev.GetLayer()] = append(byLayer[ev.GetLayer()], ev)
	}
	// eventLayerStatus is the SERVED status of an event-backed layer: raw
	// source health degraded through hazards.DegradeStoreStatus — the exact
	// mapping the store-backed /api/v1 hazards/situation path applies. The
	// stored events below ARE served (domains, top_events, map layers), so a
	// down source with stored data must read STALE, vouching for them as
	// last-good — never UNAVAILABLE disowning data this same response carries.
	eventLayerStatus := func(layer string) string {
		st, last := LayerSourceStatus(sources, layer)
		st, _ = hazards.DegradeStoreStatus(st, len(byLayer[eventLayers[layer]]) > 0, last)
		return st
	}

	// --- evacuation invariant (an error never becomes a 0) ---
	// With stored active zones and Cal OES down, the status degrades to STALE
	// and the stored count is served (the store is the persisted last-good
	// cache — same answer as /api/v1/situation); null + UNAVAILABLE is
	// reserved for "down with nothing stored", where unknown really is
	// unknown.
	evacStatus := eventLayerStatus(hazards.LayerEvacuation)
	sb.EvacuationStatus = evacStatus
	if evacStatus == "OK" || evacStatus == "STALE" {
		n := 0
		for _, ev := range events {
			if ev.GetLayer() == gridv1.Layer_EVACUATION && ev.GetStatus() == gridv1.EventStatus_ACTIVE {
				n++
			}
		}
		sb.ActiveEvacuations = &n // MAY be 0: a caveated confirmed-empty
	}
	resp.Summary = sb

	// --- domains (plan §2.4 mapping; fire and roads merge condition layers) ---
	resp.Domains = []domainBlock{
		buildDomain("fire",
			[]string{eventLayerStatus(hazards.LayerWildfire), condFireWx.status},
			append(eventItems(byLayer[gridv1.Layer_WILDFIRE]), featureItems(condFireWx.features)...)),
		buildDomain("evacuation",
			[]string{evacStatus},
			eventItems(byLayer[gridv1.Layer_EVACUATION])),
		buildDomain("weather",
			[]string{eventLayerStatus(hazards.LayerWeatherAlert)},
			eventItems(byLayer[gridv1.Layer_WEATHER_ALERT])),
		buildDomain("roads",
			[]string{eventLayerStatus(hazards.LayerRoadIncident), condChain.status, condSegment.status},
			append(eventItems(byLayer[gridv1.Layer_ROAD_INCIDENT]),
				featureItems(append(condChain.features, condSegment.features...))...)),
		buildDomain("seismic",
			[]string{eventLayerStatus(hazards.LayerEarthquake)},
			eventItems(byLayer[gridv1.Layer_EARTHQUAKE])),
	}

	// --- mode --- (event layers use the same served/degraded statuses the
	// domains report: a down source with stored data is STALE, not a dark
	// layer)
	anyUnavailable := condChain.status == "UNAVAILABLE" ||
		condSegment.status == "UNAVAILABLE" || condFireWx.status == "UNAVAILABLE"
	for layer := range eventLayers {
		if eventLayerStatus(layer) == "UNAVAILABLE" {
			anyUnavailable = true
		}
	}
	in := ModeInputs{
		EvacUnavailable:     evacStatus == "UNAVAILABLE",
		FireWeatherState:    fireWeatherState(condFireWx.features),
		AnyLayerUnavailable: anyUnavailable,
	}
	for _, ev := range events {
		if ev.GetStatus() != gridv1.EventStatus_ACTIVE {
			continue // SCHEDULED never escalates the mode
		}
		if ev.GetSeverity() > in.MaxActiveSeverity {
			in.MaxActiveSeverity = ev.GetSeverity()
		}
		if ev.GetLayer() == gridv1.Layer_WILDFIRE && ev.GetSeverity() > in.MaxWildfireSeverity {
			in.MaxWildfireSeverity = ev.GetSeverity()
		}
		if ev.GetLayer() == gridv1.Layer_EVACUATION {
			in.ActiveEvacLevels = append(in.ActiveEvacLevels, ev.GetEvacuation().GetLevel())
		}
	}
	resp.Mode = ComputeMode(in)

	// --- source-health sidecar (the registry rows feeding this place's layers) ---
	resp.Sources = sourceRows(sources)

	body, err := json.Marshal(resp)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	writeJSON(w, r, body, contentTypeJSON, maxAgeEntities)
}

// conditionResult is one condition layer's contribution to the summary.
type conditionResult struct {
	features []hazards.Feature
	status   string // OK | STALE | UNAVAILABLE
}

// buildCondition fetches one condition layer through the hazards delegation
// seam. An unwired builder (nil interface) or an unknown layer is UNAVAILABLE
// with no features — fail loud, never a fabricated clear state.
func buildCondition(r *http.Request, hb hazardsBuilder, area config.HazardArea, layer string) conditionResult {
	if hb == nil {
		return conditionResult{status: "UNAVAILABLE"}
	}
	features, status, _, _, _, ok := hb.BuildLayer(r.Context(), area, layer)
	if !ok {
		return conditionResult{status: "UNAVAILABLE"}
	}
	return conditionResult{features: features, status: status}
}

// topEventsFrom returns the n most urgent events (severity_rank desc, then
// observed_at desc — the canonical client sort; the store already yields this
// order, the stable re-sort makes the contract explicit).
func topEventsFrom(events []*gridv1.Event, n int) []topEvent {
	sorted := make([]*gridv1.Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := sorted[i].GetSeverity(), sorted[j].GetSeverity()
		if si != sj {
			return si > sj
		}
		return sorted[i].GetObservedAt().AsTime().After(sorted[j].GetObservedAt().AsTime())
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := make([]topEvent, 0, len(sorted))
	for _, ev := range sorted {
		out = append(out, topEvent{
			ID:           ev.GetId(),
			Layer:        strings.ToLower(ev.GetLayer().String()),
			Severity:     ev.GetSeverity().String(),
			SeverityRank: int(ev.GetSeverity().Number()),
			Headline:     ev.GetHeadline(),
			Source:       ev.GetProvenance().GetSourceId(),
		})
	}
	return out
}

// mergedItem is the domain-rollup view of an event or condition feature.
type mergedItem struct {
	id       string
	severity string
	rank     int
	headline string
}

func eventItems(events []*gridv1.Event) []mergedItem {
	out := make([]mergedItem, 0, len(events))
	for _, ev := range events {
		out = append(out, mergedItem{
			id:       ev.GetId(),
			severity: ev.GetSeverity().String(),
			rank:     int(ev.GetSeverity().Number()),
			headline: ev.GetHeadline(),
		})
	}
	return out
}

func featureItems(features []hazards.Feature) []mergedItem {
	out := make([]mergedItem, 0, len(features))
	for _, f := range features {
		out = append(out, mergedItem{
			id:       f.Properties.ID,
			severity: f.Properties.Severity,
			rank:     f.Properties.SeverityRank,
			headline: f.Properties.Headline,
		})
	}
	return out
}

// buildDomain rolls merged items into one domain block: status is the WORST
// source_status across the domain's layers (partial data must not present as
// complete), active_count is the merged item count, headlines are the top 3
// by severity (stable on ties, preserving events-before-conditions order).
func buildDomain(name string, statuses []string, items []mergedItem) domainBlock {
	d := domainBlock{
		Domain:          name,
		Status:          worstStatus(statuses),
		HighestSeverity: gridv1.Severity_INFO.String(),
		ActiveCount:     len(items),
		Headlines:       []domainHeadline{},
	}
	topRank := -1
	for _, it := range items {
		if it.rank > topRank {
			topRank = it.rank
			d.HighestSeverity = it.severity
		}
	}
	sorted := make([]mergedItem, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].rank > sorted[j].rank })
	if len(sorted) > 3 {
		sorted = sorted[:3]
	}
	for _, it := range sorted {
		d.Headlines = append(d.Headlines, domainHeadline{ID: it.id, Severity: it.severity, Headline: it.headline})
	}
	return d
}

// worstStatus returns the most degraded of OK < STALE < UNAVAILABLE. Unknown
// strings rank as UNAVAILABLE (nothing vouches for them).
func worstStatus(statuses []string) string {
	rank := func(s string) int {
		switch s {
		case "OK":
			return 0
		case "STALE":
			return 1
		default:
			return 2
		}
	}
	worst := 0
	for _, s := range statuses {
		if rank(s) > worst {
			worst = rank(s)
		}
	}
	return [...]string{"OK", "STALE", "UNAVAILABLE"}[worst]
}

// fireWeatherState extracts the most escalated fire-weather state from the
// condition layer's features ("" when the layer is empty/unavailable).
func fireWeatherState(features []hazards.Feature) string {
	rank := func(s string) int {
		switch s {
		case "red-flag":
			return 2
		case "elevated":
			return 1
		default:
			return 0
		}
	}
	state := ""
	for _, f := range features {
		fw := f.Properties.FireWeather
		if fw == nil || fw.State == "" {
			continue
		}
		// First known state wins over unknown; escalation wins over normal.
		if state == "" || rank(fw.State) > rank(state) {
			state = fw.State
		}
	}
	return state
}

// summarySourceIDs is the union of the source registry rows feeding the
// summary's layers (event layers via layerSourceIDs; the condition layers'
// only registry-backed feed, caltrans, is already in the set).
func summarySourceIDs() map[string]bool {
	ids := map[string]bool{}
	for _, list := range layerSourceIDs {
		for _, id := range list {
			ids[id] = true
		}
	}
	return ids
}

// sourceRows projects the registry health rows onto the sidecar shape, in the
// store's id order. A never-polled row reads UNAVAILABLE — health unknown is
// not OK (the LayerSourceStatus convention).
func sourceRows(sources []*gridv1.Source) []sourceHealth {
	allowed := summarySourceIDs()
	out := []sourceHealth{}
	for _, src := range sources {
		if !allowed[src.GetId()] {
			continue
		}
		status := src.GetStatus()
		if status == gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED {
			status = gridv1.SourceStatus_UNAVAILABLE
		}
		out = append(out, sourceHealth{
			ID:            src.GetId(),
			Status:        status.String(),
			LastSuccessAt: rfc3339(src.GetLastSuccessAt()),
		})
	}
	return out
}
