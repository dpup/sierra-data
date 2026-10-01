package ingest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/lib/geojson"
)

// laneClosureAPI is the slice of *cwwp2.Client this normalizer consumes.
type laneClosureAPI interface {
	LaneClosures(ctx context.Context, district int) ([]cwwp2.LaneClosure, error)
}

// Provenance constants for CWWP2 lane closures. The attribution names the feed
// the rows come from; the source registry row (cmd/server) says the same.
const (
	laneClosureSourceID    = "caltrans"
	laneClosureSourceName  = "Caltrans"
	laneClosureAttribution = "cwwp2.dot.ca.gov"
)

// LaneClosureNormalizer ingests Caltrans planned lane closures from the CWWP2
// data portal into the road_incident layer (source "caltrans"). It replaces
// the lcs2way.kml path, which listed only closures set up right now. CWWP2
// lists every planned WINDOW, scheduled ones included: on 2026-10-01 the
// incident box held 234 windows against the 23 closures lcs2way.kml showed.
//
// One event per window, keyed on the upstream index. There is no coarser unit
// to key on: Caltrans files a multi-day job as one row per day, and neither the
// closure id nor the log number identifies a job. A log number can span windows
// at different places (C50KB log 8 moves from Sly Park Rd to Point View Dr), or
// be re-issued daily at the same place (C88EA logs 4 and 5 are consecutive days
// at Dew Drop Road).
//
// Nothing here is AI-generated. The rows are structured, so headline, area
// label, description and severity are composed deterministically. That keeps
// ~10x the closure volume out of the incidents pipeline's 5-calls-per-refresh
// enhancement budget, and removes the headline churn that a re-worded AI
// headline mints as a revision.
type LaneClosureNormalizer struct {
	cfg       *config.Config
	client    laneClosureAPI
	districts []int
	now       func() time.Time // injectable: phase is a function of the clock
}

// NewLaneClosureNormalizer reads the districts in
// roads.caltransFeeds.cwwp2.laneClosureDistricts and scopes to
// roads.incidentAreas, the same box the CHP incidents use.
func NewLaneClosureNormalizer(cfg *config.Config, client laneClosureAPI) *LaneClosureNormalizer {
	return &LaneClosureNormalizer{
		cfg:       cfg,
		client:    client,
		districts: cfg.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts,
		now:       time.Now,
	}
}

// SourceIDs implements Normalizer.
func (n *LaneClosureNormalizer) SourceIDs() []string { return []string{laneClosureSourceID} }

// Poll implements Normalizer.
//
// The fail-loud rules, all of which keep the `resolve` sweep from reading our
// problem as closures ending:
//   - every district failing is a hard error;
//   - one district failing (HTTP error, frozen file, EMPTY file — see
//     cwwp2.Client.LaneClosures) degrades the source for the tick, so its
//     sweep is skipped while the healthy districts' events still land;
//   - an in-scope row the client could not read (cwwp2.LaneClosure.Unrecognized)
//     degrades the source the same way. Its phase is unknown, and silently
//     dropping it would resolve its event. A row we cannot place at all
//     counts as in scope, because it might be.
func (n *LaneClosureNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	areas := n.cfg.Roads.IncidentAreas
	if len(areas) == 0 {
		return nil, errEmptyScope("incident areas")
	}
	if len(n.districts) == 0 {
		return nil, errEmptyScope("lane-closure districts")
	}

	var (
		rows    []cwwp2.LaneClosure
		errs    []error
		fetched int
	)
	for _, d := range n.districts {
		got, err := n.client.LaneClosures(ctx, d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fetched++
		rows = append(rows, got...)
	}
	if fetched == 0 {
		return nil, fmt.Errorf("all lane-closure districts failed: %w", errors.Join(errs...))
	}

	now := n.now()
	var (
		events       []*gridv1.Event
		windows      = make(map[string]cwwp2.LaneClosure)
		unrecognized []string
	)
	for _, lc := range rows {
		in, placeable := inIncidentAreas(lc, areas)
		if lc.Unrecognized != "" {
			if in || !placeable {
				unrecognized = append(unrecognized, fmt.Sprintf("d%d %s (%s %s): %s",
					lc.District, lc.ID, lc.Begin.Route, lc.Begin.Name, lc.Unrecognized))
			}
			continue
		}
		if !in {
			continue
		}
		var status gridv1.EventStatus
		switch lc.PhaseAt(now) {
		case cwwp2.PhaseScheduled:
			status = gridv1.EventStatus_SCHEDULED
		case cwwp2.PhaseActive:
			status = gridv1.EventStatus_ACTIVE
		default:
			// Picked up, cancelled, or past its window with no set-up call. The
			// row lingers in the file for a while; the event's absence from this
			// (successful) poll is what resolves it.
			continue
		}
		ev := buildLaneClosureEvent(lc, status)
		if _, dup := windows[ev.Id]; dup {
			continue // the index is unique per district file; keep the first
		}
		windows[ev.Id] = lc
		events = append(events, ev)
	}

	adoptLegacyClosureIDs(ctx, events, windows, prior)

	var perSource map[string]error
	if len(unrecognized) > 0 {
		sort.Strings(unrecognized)
		logging.Warnw(ctx, "CWWP2 lane closures: unreadable rows in scope; caltrans degraded, sweep skipped",
			"count", len(unrecognized), "rows", unrecognized)
		errs = append(errs, fmt.Errorf("%d unreadable lane-closure row(s) in scope: %s",
			len(unrecognized), strings.Join(unrecognized, "; ")))
	}
	if len(errs) > 0 {
		perSource = map[string]error{laneClosureSourceID: fmt.Errorf("partial lane-closure coverage: %w", errors.Join(errs...))}
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}

// inIncidentAreas reports whether either endpoint of the closure lies in a
// configured incident area. placeable is false when neither endpoint has a
// position, which the caller must not read as "out of scope".
func inIncidentAreas(lc cwwp2.LaneClosure, areas []config.IncidentArea) (in, placeable bool) {
	for _, ep := range []cwwp2.Location{lc.Begin, lc.End} {
		if !ep.HasPosition {
			continue
		}
		placeable = true
		for _, a := range areas {
			if a.Bounds.Contains(ep.Latitude, ep.Longitude) {
				return true, true
			}
		}
	}
	return false, placeable
}

// laneClosureEventID is "caltrans:d{district}-{index}" with the index's colons
// dropped ("caltrans:d10-C4QB-0004-2026-10-02-070100"). Every part is
// immutable for the window, as an id under `resolve` must be (see "an event id
// may only be built from IMMUTABLE fields" in CLAUDE.md). The district prefix
// is there because the index is only known to be unique within one district's
// file, and the closure ids it starts with are per-route codes that two
// districts sharing a route could both issue.
func laneClosureEventID(lc cwwp2.LaneClosure) string {
	return fmt.Sprintf("caltrans:d%d-%s", lc.District, strings.ReplaceAll(lc.ID, ":", ""))
}

func buildLaneClosureEvent(lc cwwp2.LaneClosure, status gridv1.EventStatus) *gridv1.Event {
	sev := hazards.SeverityFromLaneClosure(lc.Facility, lc.ClosureType, lc.LanesClosed, lc.TotalLanes)
	ev := NewEvent(laneClosureEventID(lc), gridv1.Layer_ROAD_INCIDENT, SeverityFromLabel(sev), status, laneClosureHeadline(lc))
	ev.Category = "closure" // the category road_incident closures have always carried
	ev.Description = laneClosureDescription(lc)
	ev.AreaLabel = laneClosureAreaLabel(lc)

	// A point, as before. A begin-to-end LineString would be a straight chord
	// across a winding mountain road: 10 km of it can cut through places the
	// road never touches, and place attachment would believe it.
	at := lc.Begin
	if !at.HasPosition {
		at = lc.End
	}
	ev.Geometry = GeometryFromPoint(at.Latitude, at.Longitude)

	// effective is when it actually took effect where we know that (the 10-97
	// set-up call), else the planned start. The planned END is deliberately
	// not `expires`: crews overrun, and a set-up closure stays ACTIVE until it
	// is picked up (see grid.proto LaneClosureDetail).
	ev.Effective = tsProto(lc.Start)
	if status == gridv1.EventStatus_ACTIVE && !lc.SetUpAt.IsZero() {
		ev.Effective = tsProto(lc.SetUpAt)
	}
	// The file's generation stamp. Hash-excluded, so its every-few-minutes
	// advance mints nothing; it lands when the content actually changes.
	ev.ObservedAt = tsProto(lc.RecordedAt)
	ev.Provenance = NewProvenance(laneClosureSourceID, laneClosureSourceName, laneClosureAttribution, "")

	d := &gridv1.LaneClosureDetail{
		WindowId:              lc.ID,
		District:              int32(lc.District),
		ClosureId:             lc.ClosureID,
		LogNumber:             lc.LogNumber,
		Route:                 closureRoute(lc),
		Direction:             lc.FlowDirection,
		Facility:              lc.Facility,
		ClosureType:           lc.ClosureType,
		WorkType:              lc.WorkType,
		LanesClosed:           lc.LanesClosed,
		TotalLanes:            int32(lc.TotalLanes),
		EstimatedDelayMinutes: int32(lc.EstimatedDelay),
		ClosureDuration:       lc.Duration,
		PlannedStart:          tsProto(lc.Start),
		EndIndefinite:         lc.EndIndefinite,
		SetUpAt:               tsProto(lc.SetUpAt),
		BeginLocation:         endpointName(lc.Begin),
		EndLocation:           endpointName(lc.End),
		Begin:                 latLng(lc.Begin),
		End:                   latLng(lc.End),
	}
	if !lc.EndIndefinite {
		d.PlannedEnd = tsProto(lc.EndTime)
	}
	ev.Detail = &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
		// log_number has always carried a closure's CLOSURE id (quickmap's
		// markup surfaced it first); keep that so the GeoJSON's
		// incident.logNumber reads the same as before.
		LogNumber: lc.ClosureID,
		Closure:   d,
	}}
	return ev
}

func latLng(l cwwp2.Location) *gridv1.LatLng {
	if !l.HasPosition {
		return nil
	}
	return &gridv1.LatLng{Lat: l.Latitude, Lng: l.Longitude}
}

// endpointName is the location's name, or its free-form description when the
// name is blank (C49FB carries "North Deloro Ln" only there), with the
// hand-typed spacing tidied: "Lake Kirkwood ( Left)" reads "Lake Kirkwood
// (Left)", "Turn Out Lane  Eastbound" loses its double space.
func endpointName(l cwwp2.Location) string {
	name := l.Name
	if name == "" {
		name = l.FreeFormDesc
	}
	name = strings.Join(strings.Fields(name), " ")
	return strings.ReplaceAll(strings.ReplaceAll(name, "( ", "("), " )", ")")
}

// bareName drops a leading connective a free-form description brings with it
// ("from 17.78" / "to 17.78"), because the area label supplies its own.
func bareName(name string) string {
	lower := strings.ToLower(name)
	for _, p := range []string{"from ", "to ", "at "} {
		if strings.HasPrefix(lower, p) {
			return strings.TrimSpace(name[len(p):])
		}
	}
	return name
}

func closureRoute(lc cwwp2.LaneClosure) string {
	if lc.Begin.Route != "" {
		return lc.Begin.Route
	}
	return lc.End.Route
}

// routeLabel renders a CWWP2 route the way the site names roads: "SR-4" is
// "Hwy 4", "US-50" is "US 50", interstates stay "I-80".
func routeLabel(route string) string {
	switch {
	case strings.HasPrefix(route, "SR-"):
		return "Hwy " + strings.TrimPrefix(route, "SR-")
	case strings.HasPrefix(route, "US-"):
		return "US " + strings.TrimPrefix(route, "US-")
	}
	return route
}

// directionLabel is "eastbound" etc. for a one-way flow, and "" for both ways
// ("East / West") or none: a two-way closure needs no qualifier.
func directionLabel(flow string) string {
	switch strings.ToLower(strings.TrimSpace(flow)) {
	case "east":
		return "eastbound"
	case "west":
		return "westbound"
	case "north":
		return "northbound"
	case "south":
		return "southbound"
	}
	return ""
}

// closurePhrase names what the closure does to traffic.
func closurePhrase(lc cwwp2.LaneClosure) string {
	var phrase string
	switch strings.ToLower(strings.TrimSpace(lc.ClosureType)) {
	case "full":
		phrase = "full closure"
	case "one-way traffic":
		phrase = "one-way traffic control"
	case "alternating lanes":
		phrase = "alternating lane closure"
	case "moving":
		phrase = "moving lane closure"
	case "traffic break":
		phrase = "traffic break"
	case "lane", "":
		phrase = "lane closure"
		if shoulderOnly(lc.LanesClosed) {
			phrase = "shoulder closure" // Caltrans files these as "Lane"; no lane is closed
		}
	default:
		phrase = strings.ToLower(lc.ClosureType) + " closure"
	}
	switch strings.ToLower(strings.TrimSpace(lc.Facility)) {
	case "on ramp":
		phrase = "on-ramp " + phrase
	case "off ramp":
		phrase = "off-ramp " + phrase
	}
	return phrase
}

func shoulderOnly(lanes string) bool {
	found := false
	for _, tok := range strings.Split(lanes, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(tok), "shoulder") {
			return false
		}
		found = true
	}
	return found
}

// laneClosureHeadline is what and why, e.g. "Hwy 88 one-way traffic control
// (Tree Work)". Where is the area label; when is `effective` and the detail's
// planned window, so neither is repeated here.
func laneClosureHeadline(lc cwwp2.LaneClosure) string {
	parts := []string{}
	for _, p := range []string{routeLabel(closureRoute(lc)), directionLabel(lc.FlowDirection), closurePhrase(lc)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	h := strings.Join(parts, " ")
	if lc.WorkType != "" {
		h += " (" + lc.WorkType + ")"
	}
	return strings.ToUpper(h[:1]) + h[1:]
}

// laneClosureAreaLabel is where: "Hwy 88 at Schneidr Road (Left), near
// Markleeville" or "US 50 from Sly Park Rd to Bedford Ave, near Pollock Pines".
func laneClosureAreaLabel(lc cwwp2.LaneClosure) string {
	from, to := bareName(endpointName(lc.Begin)), bareName(endpointName(lc.End))
	var where string
	switch {
	case from == "" && to == "":
	case from == "" || to == "" || strings.EqualFold(from, to):
		where = "at " + firstNonEmpty(from, to)
	default:
		where = "from " + from + " to " + to
	}
	label := strings.TrimSpace(routeLabel(closureRoute(lc)) + " " + where)
	if near := lc.Begin.NearbyPlace; near != "" && !strings.Contains(strings.ToLower(label), strings.ToLower(near)) {
		label += ", near " + near
	}
	return label
}

// laneClosureDescription spells out the closed lanes and any delay estimate:
// "Closed: lane 1, right shoulder (of 2 lanes). Estimated delay: 10 min."
func laneClosureDescription(lc cwwp2.LaneClosure) string {
	var b strings.Builder
	if lanes := humanLanes(lc.LanesClosed); lanes != "" {
		b.WriteString("Closed: " + lanes)
		if lc.TotalLanes > 0 {
			fmt.Fprintf(&b, " (of %d %s)", lc.TotalLanes, plural(lc.TotalLanes, "lane", "lanes"))
		}
		b.WriteString(".")
	}
	if lc.EstimatedDelay > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "Estimated delay: %d min.", lc.EstimatedDelay)
	}
	return b.String()
}

// laneNames expands CWWP2's lane tokens; anything unlisted passes through.
var laneNames = map[string]string{
	"all":         "all lanes",
	"rshoulder":   "right shoulder",
	"lshoulder":   "left shoulder",
	"rt turn ln":  "right-turn lane",
	"lt turn ln":  "left-turn lane",
	"median":      "median",
	"auxiliary":   "auxiliary lane",
	"bike lane":   "bike lane",
	"sidewalk":    "sidewalk",
	"center turn": "center turn lane",
}

func humanLanes(list string) string {
	var out []string
	for _, tok := range strings.Split(list, ",") {
		tok = strings.TrimSpace(tok)
		switch {
		case tok == "":
		case isLaneNumber(tok):
			out = append(out, "lane "+tok)
		case laneNames[strings.ToLower(tok)] != "":
			out = append(out, laneNames[strings.ToLower(tok)])
		default:
			out = append(out, tok)
		}
	}
	return strings.Join(out, ", ")
}

func isLaneNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- the one-time move off lcs2way.kml ---------------------------------------

// legacyClosureIDRe matches the ids closures carried while they came from
// quickmap's lcs2way.kml: caltrans:{closureID}-{logNumber}-{6-hex hash of the
// location text} (services.incidentID). New ids never match: they carry the
// district and the window index, so they have more dash-separated parts.
var legacyClosureIDRe = regexp.MustCompile(`^caltrans:([A-Za-z0-9]+)-([A-Za-z0-9]+)-[0-9a-f]{6}$`)

// legacyAdoptMeters bounds how far a legacy event's point may sit from the
// window's nearest endpoint. The KML placemark IS one of the CWWP2 endpoints:
// all 23 live closures on 2026-10-01 matched at 0 m.
const legacyAdoptMeters = 250

// adoptLegacyClosureIDs gives a CWWP2 window the id of the KML-era event that
// was tracking the same closure, so the switch of feed does not resolve every
// closure that is physically in place and open a second event for it. Without
// this, the first tick after deploy writes a RESOLVED revision into the
// history of ~20 closures that never ended. That is a fabricated all-clear,
// the thing the sweep invariant exists to prevent.
//
// lcs2way.kml listed only closures that were set up, so a legacy event can
// only correspond to an ACTIVE window. The match is same closure id, same log
// number, and an endpoint within legacyAdoptMeters, and it must be unique in
// BOTH directions. On 2026-10-01 that matched all 23 live closures to exactly
// one window each. Anything ambiguous is left alone: the legacy event resolves
// and the window keeps its own id, which is the outcome without this shim.
//
// The adoption is stable: next tick the stored event still has the legacy id
// and the same window, so the same match recurs until the window is picked up
// and the event resolves. No new legacy ids are ever minted, so this retires
// itself once the last KML-era closure ends (the longest live one, C26EA, is
// planned to 2026-11-09). It can be deleted after that.
func adoptLegacyClosureIDs(ctx context.Context, events []*gridv1.Event, windows map[string]cwwp2.LaneClosure, prior Prior) {
	type legacy struct {
		id, closureID, logNumber string
		lat, lng                 float64
	}
	var olds []legacy
	for _, ev := range priorForSource(prior, laneClosureSourceID) {
		m := legacyClosureIDRe.FindStringSubmatch(ev.GetId())
		c := ev.GetGeometry().GetCentroid()
		if m == nil || c == nil {
			continue
		}
		olds = append(olds, legacy{id: ev.GetId(), closureID: m[1], logNumber: m[2], lat: c.GetLat(), lng: c.GetLng()})
	}
	if len(olds) == 0 {
		return
	}

	matches := make(map[string][]*gridv1.Event) // legacy id -> candidate events
	claims := make(map[string]int)              // event id -> legacy events claiming it
	for _, old := range olds {
		for _, ev := range events {
			if ev.GetStatus() != gridv1.EventStatus_ACTIVE {
				continue
			}
			lc := windows[ev.GetId()]
			if !strings.EqualFold(lc.ClosureID, old.closureID) || !sameLogNumber(lc.LogNumber, old.logNumber) {
				continue
			}
			if nearestEndpointMeters(lc, old.lat, old.lng) > legacyAdoptMeters {
				continue
			}
			matches[old.id] = append(matches[old.id], ev)
			claims[ev.GetId()]++
		}
	}
	for _, old := range olds {
		cands := matches[old.id]
		if len(cands) != 1 || claims[cands[0].GetId()] != 1 {
			logging.Infow(ctx, "CWWP2 lane closures: KML-era closure has no unique CWWP2 window; the sweep will resolve it",
				"id", old.id, "candidates", len(cands))
			continue
		}
		cands[0].Id = old.id
	}
}

// sameLogNumber compares log numbers across the two feeds: the KML printed
// "4" where the CWWP2 index pads "0004".
func sameLogNumber(a, b string) bool {
	trim := func(s string) string {
		if t := strings.TrimLeft(strings.TrimSpace(s), "0"); t != "" {
			return t
		}
		return "0"
	}
	return strings.EqualFold(trim(a), trim(b))
}

func nearestEndpointMeters(lc cwwp2.LaneClosure, lat, lng float64) float64 {
	best := -1.0
	for _, ep := range []cwwp2.Location{lc.Begin, lc.End} {
		if !ep.HasPosition {
			continue
		}
		if d := geojson.MetersBetween(lat, lng, ep.Latitude, ep.Longitude); best < 0 || d < best {
			best = d
		}
	}
	if best < 0 {
		return legacyAdoptMeters + 1
	}
	return best
}
