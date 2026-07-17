package gridapi

import (
	"context"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// This file holds the place-summary projection (buildPlaceSummary) and the pure
// ComputeMode rule table. GetPlaceSummary (the RPC) lives in grpc.go.

// Mode values (plan §2.4). Ordered by escalation; the string is the wire value.
const (
	ModeQuiet  = "QUIET"
	ModeWatch  = "WATCH"
	ModeActive = "ACTIVE"
)

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

// buildPlaceSummary computes the place rollup as a proto PlaceSummary. It
// returns store.ErrNotFound for an unknown place (the RPC maps it to NotFound);
// the caller passes the concrete hazards builder (or a nil interface, which
// makes the condition layers UNAVAILABLE — fail loud).
func (s *Service) buildPlaceSummary(ctx context.Context, hb hazardsBuilder, placeKey string) (*gridv1.PlaceSummary, error) {
	place, err := s.Store.GetPlace(ctx, placeKey)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		events = append(events, page...)
		if next == "" {
			break
		}
		q.PageToken = next
	}

	sources, err := s.Store.ListSources(ctx)
	if err != nil {
		return nil, err
	}

	// Condition layers (live projections of the roads/weather services): fetch
	// once each, feeding the roads/fire domain merges and the fire-weather mode
	// signal. The three builds do independent upstream I/O, so fan them out
	// concurrently rather than paying the sum of their latencies.
	area, covered := s.resolveHazardArea(place)
	// fire_weather is zone-scoped: an out-of-coverage place inherits no region's
	// product — a confirmed-empty OK (not "" which worstStatus ranks UNAVAILABLE).
	condChain, condSegment := conditionResult{}, conditionResult{}
	condFireWx := conditionResult{status: "OK"}
	var cwg sync.WaitGroup
	cwg.Add(2)
	go func() { defer cwg.Done(); condChain = buildCondition(ctx, hb, area, hazards.LayerChainControl) }()
	go func() { defer cwg.Done(); condSegment = buildCondition(ctx, hb, area, hazards.LayerRoadSegment) }()
	if covered {
		cwg.Add(1)
		go func() { defer cwg.Done(); condFireWx = buildCondition(ctx, hb, area, hazards.LayerFireWeather) }()
	}
	cwg.Wait()

	out := &gridv1.PlaceSummary{
		Place:       place.GetSlug(),
		PlaceId:     place.GetId(),
		PlaceName:   place.GetName(),
		GeneratedAt: timestamppb.New(s.Now().UTC()),
	}

	// Mesh-node presence (NETWORK) is ambient INFO infrastructure state, surfaced
	// in its own `comms` domain below. It must NOT inflate the top-level hazard
	// rollup (total_active, severity counts, top events, mode) — the same
	// "ambient state is monitoring, not an active hazard" rule that excludes
	// baseline conditions (commit d278e43). hazardEvents is the set that drives
	// those; byLayer (built from the full set) still feeds the comms domain.
	hazardEvents := make([]*gridv1.Event, 0, len(events))
	for _, ev := range events {
		if ev.GetLayer() != gridv1.Layer_NETWORK {
			hazardEvents = append(hazardEvents, ev)
		}
	}

	// --- summary block (store events only; conditions live in domains) ---
	stats := &gridv1.SummaryStats{
		HighestSeverity: gridv1.Severity_INFO.String(), // default when no events
		SeverityCounts:  map[string]int32{},
		TotalActive:     int32(len(hazardEvents)),
		TopEvents:       []*gridv1.SummaryTopEvent{},
	}
	for _, ev := range hazardEvents {
		sev := ev.GetSeverity()
		stats.SeverityCounts[sev.String()]++
		if int32(sev.Number()) >= stats.HighestSeverityRank {
			stats.HighestSeverityRank = int32(sev.Number())
			stats.HighestSeverity = sev.String()
		}
	}
	stats.TopEvents = topEventsFrom(hazardEvents, 5)

	byLayer := map[gridv1.Layer][]*gridv1.Event{}
	for _, ev := range events {
		byLayer[ev.GetLayer()] = append(byLayer[ev.GetLayer()], ev)
	}
	// eventLayerStatus is the SERVED status of an event-backed layer: raw source
	// health degraded through hazards.DegradeStoreStatus. Stored events below ARE
	// served, so a down source with stored data must read STALE, vouching for
	// them as last-good — never UNAVAILABLE disowning data this response carries.
	eventLayerStatus := func(layer string) string {
		st, last := LayerSourceStatus(sources, layer)
		st, _ = hazards.DegradeStoreStatus(st, len(byLayer[eventLayers[layer]]) > 0, last)
		return st
	}

	// --- evacuation invariant (an error never becomes a 0) ---
	// With stored active zones and Cal OES down, the status degrades to STALE and
	// the stored count is served; null + UNAVAILABLE is reserved for "down with
	// nothing stored", where unknown really is unknown.
	evacStatus := eventLayerStatus(hazards.LayerEvacuation)
	stats.EvacuationStatus = evacStatus
	if evacStatus == "OK" || evacStatus == "STALE" {
		var n int32
		for _, ev := range events {
			if ev.GetLayer() == gridv1.Layer_EVACUATION && ev.GetStatus() == gridv1.EventStatus_ACTIVE {
				n++
			}
		}
		stats.ActiveEvacuations = wrapperspb.Int32(n) // MAY be 0: a caveated confirmed-empty
	}
	out.Summary = stats

	// --- domains (plan §2.4 mapping; fire and roads merge condition layers) ---
	out.Domains = []*gridv1.SummaryDomain{
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
	// comms (MeshCore mesh presence) is only reported when the source is enabled:
	// a deliberately-off source must not surface as a dark/UNAVAILABLE domain.
	if s.Cfg.Grid.Meshcore.Enabled {
		out.Domains = append(out.Domains, buildDomain("comms",
			[]string{eventLayerStatus(hazards.LayerNetwork)},
			eventItems(byLayer[gridv1.Layer_NETWORK])))
	}

	// --- mode --- (event layers use the same served/degraded statuses the
	// domains report: a down source with stored data is STALE, not a dark layer)
	anyUnavailable := condChain.status == "UNAVAILABLE" ||
		condSegment.status == "UNAVAILABLE" || condFireWx.status == "UNAVAILABLE"
	for layer := range eventLayers {
		// A deliberately-off comms source is not a data gap in the hazard picture.
		if layer == hazards.LayerNetwork && !s.Cfg.Grid.Meshcore.Enabled {
			continue
		}
		if eventLayerStatus(layer) == "UNAVAILABLE" {
			anyUnavailable = true
		}
	}
	in := ModeInputs{
		EvacUnavailable:     evacStatus == "UNAVAILABLE",
		FireWeatherState:    fireWeatherState(condFireWx.features),
		AnyLayerUnavailable: anyUnavailable,
	}
	for _, ev := range hazardEvents {
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
	out.Mode = ComputeMode(in)

	// --- source-health sidecar (the registry rows feeding this place's layers) ---
	out.Sources = sourceRows(sources)
	return out, nil
}

// conditionResult is one condition layer's contribution to the summary.
type conditionResult struct {
	features []hazards.Feature
	status   string // OK | STALE | UNAVAILABLE
}

// buildCondition fetches one condition layer through the hazards delegation
// seam. An unwired builder (nil interface) or an unknown layer is UNAVAILABLE
// with no features — fail loud, never a fabricated clear state.
func buildCondition(ctx context.Context, hb hazardsBuilder, area config.HazardArea, layer string) conditionResult {
	if hb == nil {
		return conditionResult{status: "UNAVAILABLE"}
	}
	features, status, _, _, _, ok := hb.BuildLayer(ctx, area, layer)
	if !ok {
		return conditionResult{status: "UNAVAILABLE"}
	}
	return conditionResult{features: features, status: status}
}

// topEventsFrom returns the n most urgent events (severity_rank desc, then
// observed_at desc — the canonical client sort; the store already yields this
// order, the stable re-sort makes the contract explicit).
func topEventsFrom(events []*gridv1.Event, n int) []*gridv1.SummaryTopEvent {
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
	out := make([]*gridv1.SummaryTopEvent, 0, len(sorted))
	for _, ev := range sorted {
		out = append(out, &gridv1.SummaryTopEvent{
			Id:           ev.GetId(),
			Layer:        ev.GetLayer().String(),
			Severity:     ev.GetSeverity().String(),
			SeverityRank: int32(ev.GetSeverity().Number()),
			Headline:     ev.GetHeadline(),
			Source:       ev.GetProvenance().GetSourceId(),
		})
	}
	return out
}

// mergedItem is the domain-rollup view of an event or condition feature.
// isCondition marks the item as a condition-layer projection (road_segment,
// chain_control, fire_weather) rather than a discrete event — condition layers
// are always present (a monitored road, the region's fire-weather state), so a
// baseline INFO one is monitoring, not an active hazard (see buildDomain).
type mergedItem struct {
	id          string
	severity    string
	rank        int
	headline    string
	isCondition bool
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
			id:          f.Properties.ID,
			severity:    f.Properties.Severity,
			rank:        f.Properties.SeverityRank,
			headline:    f.Properties.Headline,
			isCondition: true,
		})
	}
	return out
}

// buildDomain rolls merged items into one domain block: status is the WORST
// source_status across the domain's layers (partial data must not present as
// complete), active_count is the count of *active* items, headlines are the top
// 3 of those by severity (stable on ties, preserving events-before-conditions
// order).
//
// "Active" excludes baseline conditions. An always-present condition feature at
// INFO severity — an OPEN road segment, a "normal" fire-weather banner — is
// monitoring, not a hazard, so counting it made a genuinely QUIET area report
// e.g. roads active_count 4 for four clear segments (and fire 1 for normal fire
// weather) alongside total_active 0. Events always count (matching
// total_active); condition features count only above INFO. The unfiltered set
// still drives the map layers and source health — this filter is the summary
// rollup only.
func buildDomain(name string, statuses []string, items []mergedItem) *gridv1.SummaryDomain {
	active := make([]mergedItem, 0, len(items))
	for _, it := range items {
		if it.isCondition && it.rank <= int(gridv1.Severity_INFO.Number()) {
			continue
		}
		active = append(active, it)
	}
	d := &gridv1.SummaryDomain{
		Domain:          name,
		Status:          worstStatus(statuses),
		HighestSeverity: gridv1.Severity_INFO.String(),
		ActiveCount:     int32(len(active)),
		Headlines:       []*gridv1.SummaryDomainHeadline{},
	}
	topRank := -1
	for _, it := range active {
		if it.rank > topRank {
			topRank = it.rank
			d.HighestSeverity = it.severity
		}
	}
	sorted := make([]mergedItem, len(active))
	copy(sorted, active)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].rank > sorted[j].rank })
	if len(sorted) > 3 {
		sorted = sorted[:3]
	}
	for _, it := range sorted {
		d.Headlines = append(d.Headlines, &gridv1.SummaryDomainHeadline{Id: it.id, Severity: it.severity, Headline: it.headline})
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
func sourceRows(sources []*gridv1.Source) []*gridv1.SummarySourceHealth {
	allowed := summarySourceIDs()
	out := []*gridv1.SummarySourceHealth{}
	for _, src := range sources {
		if !allowed[src.GetId()] {
			continue
		}
		status := src.GetStatus()
		if status == gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED {
			status = gridv1.SourceStatus_UNAVAILABLE
		}
		out = append(out, &gridv1.SummarySourceHealth{
			Id:            src.GetId(),
			Status:        status.String(),
			LastSuccessAt: src.GetLastSuccessAt(),
		})
	}
	return out
}
