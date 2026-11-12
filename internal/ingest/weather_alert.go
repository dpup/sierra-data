package ingest

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/services"
)

// Provenance constants — the attribution must match cmd/server's gridSourceInfo
// row for `nws` and the hazards Source blocks, so /api/v1/sources, the geojson
// layer metadata and event provenance all credit NOAA identically.
//
// This shipped EMPTY until 2026-09-02: NewProvenance was called with "" and NWS
// was the only source whose events carried no attribution at all, against the
// standing obligation that attribution renders wherever data does.
//
// source_url stays empty deliberately, and that is NOT the same oversight. The
// only per-alert link NWS offers is https://api.weather.gov/alerts/{id}, which
// is JSON rather than a public page (the product's own `web` field is just the
// NWS homepage), so there is nothing honest to deep-link. Surveyed and declined
// — do not add one.
const (
	nwsAttribution = "NOAA / National Weather Service"
	// Fallback when a product carries no senderName; source_name is otherwise
	// the ISSUING OFFICE ("NWS Sacramento CA"), which is more specific than the
	// registry row's name and is meant to be.
	nwsSourceName = "National Weather Service"
)

// weatherAlertsAPI is the slice of WeatherService this normalizer consumes
// (decision 6: ingest reuses the cached NWS fetch, no duplicate parsing).
// RawNWSAlerts returns the full nws.Alert records — certainty, urgency,
// instruction, areaDesc, and the unmapped NWS severity vocabulary, all of
// which the api.WeatherAlert projection drops — plus the fetch time and,
// when serving a last-good stale list, the fetch error alongside the data.
type weatherAlertsAPI interface {
	RawNWSAlerts(ctx context.Context) (alerts []nws.Alert, fetchedAt time.Time, err error)
	// AlertZoneGeometry is the combined, simplified area of an alert's zones
	// (nws.Alert.ZoneRefs), all or nothing.
	AlertZoneGeometry(ctx context.Context, refs []string) ([]byte, error)
	// NWSAlertReplacedBy looks one alert up at api.weather.gov and returns the
	// id of the message that replaced it, or "" if none has.
	NWSAlertReplacedBy(ctx context.Context, id string) (string, error)
}

// WeatherAlertNormalizer ingests NWS zone alerts (id namespace "wx:") via the
// weather service. Alerts carry NWS zones, not geometry: place_ids are
// precomputed from each configured area's zones, and the geometry is the
// zones' own boundaries (fetched once, simplified, combined).
type WeatherAlertNormalizer struct {
	cfg     *config.Config
	weather weatherAlertsAPI
	now     func() time.Time // injectable for the SCHEDULED/ACTIVE cutover tests

	// notReplaced remembers held alerts that NWS said it had NOT replaced
	// (see replacedHeld), so an alert that merely lapsed is looked up once an
	// hour rather than every tick until its grace ends. Poll runs on one
	// goroutine, but the mutex keeps the normalizer safe to share.
	mu          sync.Mutex
	notReplaced map[string]time.Time
}

const (
	// replacedLookupTTL is how long a "not replaced" answer is trusted. An
	// Update that appears later is caught from its references without a
	// lookup, so this only bounds how late a Cancel of a lapsed alert is seen.
	replacedLookupTTL = time.Hour
	// maxReplacedLookups bounds the per-tick /alerts/{id} calls.
	maxReplacedLookups = 8
)

// NewWeatherAlertNormalizer wires the normalizer to the weather service (the
// concrete *services.WeatherService satisfies weatherAlertsAPI).
func NewWeatherAlertNormalizer(cfg *config.Config, weather weatherAlertsAPI) *WeatherAlertNormalizer {
	return &WeatherAlertNormalizer{cfg: cfg, weather: weather, now: time.Now, notReplaced: map[string]time.Time{}}
}

// SourceIDs implements Normalizer.
func (n *WeatherAlertNormalizer) SourceIDs() []string { return []string{"nws"} }

// Poll implements Normalizer.
func (n *WeatherAlertNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	// Empty scope is a hard error, never a success-empty poll. With no configured
	// NWS zones, RawNWSAlerts short-circuits to (nil, now, nil) with no fetch; a
	// success-empty PollResult would mark nws OK and let the sweep EXPIRE every
	// stored active alert with no fetch ever made (fail-loud mechanism 4, matching
	// the other pollers' errEmptyScope guard).
	if len(n.cfg.Weather.NWS.Zones) == 0 {
		return nil, errEmptyScope("nws zones")
	}
	alerts, _, err := n.weather.RawNWSAlerts(ctx)
	if err != nil && alerts == nil {
		// Fetch failed with no usable last-good list: hard error, every
		// covered source records the failure and nothing sweeps.
		return nil, err
	}
	var perSource map[string]error
	if err != nil {
		// Stale serve: RawNWSAlerts returned the last-good list alongside the
		// fetch error. Emit the events (availability) AND report the source
		// degraded — the scheduler skips the disappearance sweep for a source
		// with a PerSource error, so a stale list can never expire alerts it
		// merely failed to refresh.
		perSource = map[string]error{"nws": err}
	}
	now := n.now()

	// Every id an Update or Cancel in the list names in its CAP references:
	// NWS has replaced those messages (see replacedHeld).
	referenced := referencedAlerts(alerts)

	events := make([]*gridv1.Event, 0, len(alerts))
	for _, a := range alerts {
		// A referenced alert still in the list (NWS normally drops it at once)
		// is not emitted: the scheduler retires it this tick, and emitting it
		// would write it back to ACTIVE on the next one. A Cancel message is
		// an all-clear for the alerts it references, not a hazard of its own.
		// (/alerts/active does not list Cancel messages today; this is a
		// guard, not a path production exercises.)
		if len(referenced[services.NWSAlertID(a)]) > 0 || strings.EqualFold(a.MessageType, "Cancel") {
			continue
		}
		// An NWS watch is SCHEDULED until it takes effect (decision 8). The
		// test is against the HAZARD's start (CAP `onset`), not the product's
		// `effective` — a Watch issued Tuesday morning for Thursday's storms is
		// effective on issue, so keying on `effective` made every such watch
		// ACTIVE the moment it was published and decision 8 never fired.
		status := gridv1.EventStatus_ACTIVE
		if begins := a.Begins(); !begins.IsZero() && begins.After(now) {
			status = gridv1.EventStatus_SCHEDULED
		}

		ev := NewEvent(
			// services.NWSAlertID is the exact derivation behind the shipped
			// /weather/alerts ids, so "wx:"+id stays byte-identical to them.
			"wx:"+services.NWSAlertID(a),
			gridv1.Layer_WEATHER_ALERT,
			// NWS-direct severity mapping: unlike the api.AlertSeverity
			// projection (which collapses Extreme into CRITICAL => SEVERE),
			// this keeps NWS "Extreme" as EXTREME — a deliberate accuracy
			// improvement over the shipped collapse.
			SeverityFromLabel(hazards.SeverityFromNWSSeverity(a.Severity)),
			status,
			// "<Event> — <reason>", composed deterministically from the
			// product name and parameters.NWSheadline. CAP's own
			// properties.headline is issuance boilerplate whose every token is
			// already category/effective/expires/provenance — see
			// nws.Alert.ShortHeadline.
			a.ShortHeadline(),
		)
		ev.Category = a.Event
		ev.Description = a.Description // original NWS text, verbatim
		// The hazard window, not the product's publication window — see
		// nws.Alert.Begins/EndsAt.
		ev.Effective = tsProto(a.Begins())
		ev.Expires = tsProto(a.EndsAt())
		// Attach to every configured area whose zones intersect the alert's
		// (a zoneless alert can't be scoped, so it attaches to all areas).
		for _, area := range n.cfg.Hazards.Areas {
			if zonesMatch(area.Zones, a.Zones) {
				ev.PlaceIds = append(ev.PlaceIds, "area:"+area.ID)
			}
		}
		n.attachArea(ctx, ev, a, prior)
		ev.Provenance = NewProvenance("nws", nonEmpty(a.SenderName, nwsSourceName), nwsAttribution, "")
		// event name is the envelope category; sender is provenance.source_name —
		// neither is repeated in the detail.
		ev.Detail = &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{
			NwsSeverity: a.Severity, // raw NWS vocabulary: Extreme|Severe|Moderate|Minor|Unknown
			Certainty:   a.Certainty,
			Urgency:     a.Urgency,
			Instruction: a.Instruction,
			AreaDesc:    a.AreaDesc,
			Zones:       a.Zones,
		}}
		events = append(events, ev)
	}
	res := &PollResult{Events: events, PerSource: perSource}
	// Only from a clean fetch: a stale-served list proves nothing about
	// what is still current, and the scheduler would not act on it anyway.
	if perSource == nil {
		res.Superseded, res.ReplacedBy = n.replacedHeld(ctx, prior, referenced, events, now)
	}
	return res, nil
}

// referencedAlerts maps each alert id that some alert in the list names in its
// CAP references to the ids of the listed alerts that name it, in list order.
// Every listed message's references count, so a chain whose links are all in
// the list is covered without walking it.
func referencedAlerts(alerts []nws.Alert) map[string][]string {
	out := make(map[string][]string)
	for _, a := range alerts {
		self := services.NWSAlertID(a)
		for _, ref := range a.References {
			if ref != self {
				out[ref] = append(out[ref], self)
			}
		}
	}
	return out
}

// replacedHeld returns the stored ACTIVE/SCHEDULED alert events that NWS has
// replaced, for PollResult.Superseded, which RESOLVES them this tick instead
// of leaving them for the 24h expire grace (2026-11-11: an Update of the
// CAZ139 Winter Storm Watch left both copies SCHEDULED, and /live showed two
// identical cards).
//
// Two kinds of positive evidence, both NWS's own replacement relation:
//
//   - A listed alert references the held one (CAP `references`). This is the
//     common case (an Update) and needs no extra request. It is evaluated
//     against the store on EVERY tick, not only when the Update is first
//     seen, so an Update ingested before this rule existed still retires
//     its predecessor.
//   - A held alert has dropped out of the active list and api.weather.gov
//     says it was replaced (`replacedBy`). This is the only way a Cancel is
//     seen, because /alerts/active never lists Cancel messages, and it
//     catches the links of a chain whose middle we never saw (an Update that
//     references only its immediate predecessor). It costs one request per
//     such alert, remembered for replacedLookupTTL when the answer is "not
//     replaced", and none once the alert is retired.
//
// RESOLVED, not EXPIRED, for both: EXPIRED means aged out with nobody saying
// so, while here the issuing office has said it. A Cancel ends the hazard;
// an Update ends THIS message, and the hazard continues on the replacing
// alert, which is its own ACTIVE/SCHEDULED event. That is the Superseded
// contract the wildfire adoption already uses. Event ids are unchanged. The
// second result maps a retired id to the Grid event id of the alert that
// replaced it (WeatherAlertDetail.replaced_by_event_id), for the ids whose
// replacement is itself an event in this poll: a Cancel is not emitted, so a
// cancelled alert gets no pointer.
//
// Ids we do not hold are ignored, which also ignores alerts in zones we do
// not track: we only ever hold alerts from our zones' feed.
func (n *WeatherAlertNormalizer) replacedHeld(ctx context.Context, prior Prior, referenced map[string][]string, events []*gridv1.Event, now time.Time) ([]string, map[string]string) {
	held := priorForSource(prior, "nws")
	if len(held) == 0 {
		return nil, nil
	}
	listed := make(map[string]bool, len(events))
	for _, ev := range events {
		listed[ev.GetId()] = true
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notReplaced == nil {
		n.notReplaced = map[string]time.Time{}
	}
	stillHeld := make(map[string]bool, len(held))
	var out []string
	replacedBy := map[string]string{}
	lookups := 0
	for _, ev := range held {
		id := strings.TrimPrefix(ev.GetId(), "wx:")
		stillHeld[id] = true
		if refs := referenced[id]; len(refs) > 0 {
			out = append(out, ev.GetId())
			for _, r := range refs {
				if listed["wx:"+r] {
					replacedBy[ev.GetId()] = "wx:" + r
					break
				}
			}
			continue
		}
		// Still listed means still current; a synthesized id (no NWS id) has
		// nothing to look up.
		if listed[ev.GetId()] || !strings.HasPrefix(id, "urn:") {
			continue
		}
		if at, ok := n.notReplaced[id]; ok && now.Sub(at) < replacedLookupTTL {
			continue
		}
		if lookups >= maxReplacedLookups {
			continue
		}
		lookups++
		by, err := n.weather.NWSAlertReplacedBy(ctx, id)
		if err != nil {
			// No evidence either way: the alert keeps its grace.
			logging.Warnw(ctx, "Weather alert: replacement lookup failed", "id", ev.GetId(), "error", err)
			continue
		}
		if by == "" {
			n.notReplaced[id] = now
			continue
		}
		logging.Infow(ctx, "Weather alert: NWS replaced a held alert", "id", ev.GetId(), "replacedBy", by)
		out = append(out, ev.GetId())
		if listed["wx:"+by] {
			replacedBy[ev.GetId()] = "wx:" + by
		}
	}
	for id := range n.notReplaced {
		if !stillHeld[id] {
			delete(n.notReplaced, id)
		}
	}
	return out, replacedBy
}

// attachArea sets the alert's geometry to its zones' area. Geometry is
// hashed, so when a zone cannot be read the stored shape rides forward
// rather than vanishing for a tick and coming back (two revisions of nothing),
// and an alert first seen during an NWS zone outage simply has no shape yet.
// An alert with no typed zone refs has no shape: guessing the zone type from
// the UGC code would draw a public zone for a fire-weather product.
func (n *WeatherAlertNormalizer) attachArea(ctx context.Context, ev *gridv1.Event, a nws.Alert, prior Prior) {
	if len(a.ZoneRefs) == 0 {
		return
	}
	raw, err := n.weather.AlertZoneGeometry(ctx, a.ZoneRefs)
	if err == nil && len(raw) > 0 {
		var g *gridv1.Geometry
		if g, err = GeometryFromGeoJSON(raw); err == nil {
			ev.Geometry = g
			return
		}
	}
	if err != nil {
		logging.Warnw(ctx, "Weather alert: zone area unavailable; keeping the stored shape", "id", ev.GetId(), "error", err)
	}
	if pe := priorByID(prior, ev.GetId()); pe != nil {
		ev.Geometry = pe.GetGeometry()
	}
}
