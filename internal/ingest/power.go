package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/pge"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
)

// Source registry ids this poller writes. Two rows, one poller: the outage
// feed and the PSPS feed are separate ArcGIS services that fail independently,
// so conflating their health would let an outage-service outage silently
// present as "no PSPS" (and vice versa).
const (
	powerSourceOutages = "pge"
	powerSourcePSPS    = "psps"
)

// Provenance constants — must match cmd/server's gridSourceInfo rows so
// /api/v1/sources and event provenance agree.
const (
	pgeSourceName     = "PG&E"
	pspsSourceName    = "PG&E PSPS"
	pgeAttribution    = "Pacific Gas and Electric"
	powerCatUnplanned = "unplanned"
	powerCatPlanned   = "planned"
	powerCatPSPS      = "psps"
)

// PowerNormalizer ingests PG&E electric outages (id namespace "pge:") and
// Public Safety Power Shutoffs (id namespace "psps:") over the union bbox of
// the configured hazard areas.
//
// Unlike wildfire, power gets NO widened geography: a shutoff or outage outside
// the coverage footprint is not a threat to it — the grid does not move.
type PowerNormalizer struct {
	cfg    *config.Config
	client *pge.Client
	now    func() time.Time // injectable so the PSPS scheduled/active split is testable
}

// NewPowerNormalizer wires the normalizer to a PG&E client (tests inject one
// built with pge.NewClientWithHTTPDoer).
func NewPowerNormalizer(cfg *config.Config, client *pge.Client) *PowerNormalizer {
	return &PowerNormalizer{cfg: cfg, client: client, now: time.Now}
}

// SourceIDs implements Normalizer.
func (n *PowerNormalizer) SourceIDs() []string {
	return []string{powerSourceOutages, powerSourcePSPS}
}

// Poll implements Normalizer. The two feeds are polled independently: one
// failing records a PerSource error (so its disappearance sweep is skipped)
// while the other's events still land. Both failing is a hard error.
func (n *PowerNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return nil, errEmptyScope("hazard areas")
	}
	bounds := pge.Bounds{
		MinLatitude:  minLat,
		MaxLatitude:  maxLat,
		MinLongitude: minLng,
		MaxLongitude: maxLng,
	}

	res := &PollResult{PerSource: map[string]error{}}

	outages, oerr := n.client.GetOutages(ctx, bounds)
	if oerr != nil {
		res.PerSource[powerSourceOutages] = oerr
		logging.Errorw(ctx, "PG&E outage fetch failed", "error", oerr)
	} else {
		if stale := n.freshnessError(ctx); stale != nil {
			// The fetch worked, but PG&E's own ETL stamp says the data behind it
			// is frozen. Recorded as a source FAILURE, not a success: it skips the
			// disappearance sweep (an outage missing from a frozen feed proves
			// nothing) and skips TouchSeen, and it degrades the source's health so
			// /api/v1/sources and the layer's sourceStatus say so out loud. The
			// events themselves still upsert — last-known data is the best
			// available, and DegradeStoreStatus serves it as STALE rather than
			// disowning it.
			res.PerSource[powerSourceOutages] = stale
		}
		for _, o := range outages {
			res.Events = append(res.Events, n.buildOutageEvent(ctx, o))
		}
	}

	areas, perr := n.client.GetPSPSAreas(ctx, bounds)
	if perr != nil {
		res.PerSource[powerSourcePSPS] = perr
		logging.Errorw(ctx, "PG&E PSPS fetch failed", "error", perr)
	} else {
		res.Events = append(res.Events, n.buildPSPSEvents(ctx, areas)...)
	}

	// Neither feed produced a usable view: fail the tick outright rather than
	// returning a success-empty result that the sweep would read as "the power
	// is on everywhere".
	if oerr != nil && perr != nil {
		return nil, fmt.Errorf("ingest: both PG&E feeds failed: outages: %w; psps: %v", oerr, perr)
	}
	if len(res.PerSource) == 0 {
		res.PerSource = nil
	}
	return res, nil
}

// freshnessError reports a frozen upstream: PG&E publishes its own ETL stamp
// for the outage service, and when that stamp stops advancing the layers keep
// serving the last set indefinitely. Without this check "still listed" and
// "still out" are indistinguishable — which is exactly how the Cal OES mirror
// of this same data came to report day-old outages as Active.
//
// It returns nil (fail-OPEN) when the stamp itself can't be read. The gate is
// an EXTRA honesty signal layered on an already-successful fetch; losing it
// leaves us exactly where every other source in this repo already sits (none of
// them publish an ETL stamp at all), whereas failing the source on a flaky
// metadata table would flap the layer for no gain in truth.
func (n *PowerNormalizer) freshnessError(ctx context.Context) error {
	maxAge := n.cfg.Grid.Power.OutageStale()
	if maxAge <= 0 {
		return nil
	}
	stamp, err := n.client.GetOutagesLastUpdate(ctx)
	if err != nil {
		logging.Warnw(ctx, "PG&E outage freshness stamp unreadable; skipping the staleness gate", "error", err)
		return nil
	}
	if age := n.now().Sub(stamp); age > maxAge {
		return fmt.Errorf("PG&E outage feed is frozen: upstream ETL stamp %s is %s old (max %s)",
			stamp.UTC().Format(time.RFC3339), age.Round(time.Minute), maxAge)
	}
	return nil
}

// buildOutageEvent converts one PG&E outage into an event.
func (n *PowerNormalizer) buildOutageEvent(ctx context.Context, o pge.Outage) *gridv1.Event {
	planned := o.Planned()
	category := powerCatUnplanned
	if planned {
		category = powerCatPlanned
	}
	ev := NewEvent(
		"pge:"+o.ID,
		gridv1.Layer_POWER,
		SeverityFromLabel(hazards.SeverityFromPowerOutage(o.CustomersAffected, planned)),
		gridv1.EventStatus_ACTIVE,
		powerOutageHeadline(o, planned),
	)
	ev.Category = category
	// PG&E publishes no per-outage page, so the canonical link is the outage map
	// itself — the authoritative place a reader checks, which is what the
	// fail-loud rule asks for.
	ev.CanonicalUrl = pge.OutageMapURL
	ev.Effective = tsProto(o.Start)
	// observed_at is the upstream update stamp, falling back to the start time
	// for an outage PG&E has not revised (same rule as earthquakes).
	if o.LastUpdate.IsZero() {
		ev.ObservedAt = tsProto(o.Start)
	} else {
		ev.ObservedAt = tsProto(o.LastUpdate)
	}
	ev.Provenance = NewProvenance(powerSourceOutages, pgeSourceName, pgeAttribution, pge.OutageMapURL)
	ev.Detail = &gridv1.Event_Power{Power: &gridv1.PowerDetail{
		OutageId:          o.ID,
		Cause:             o.Cause,
		CustomersAffected: o.CustomersAffected,
		CrewStatus:        o.CrewStatus,
		// Deliberately NOT the envelope `expires`: ETOR is an estimate PG&E
		// routinely overruns (observed: a 19:00Z ETOR on an outage still active
		// at 22:00Z), and `expires` is what shouldExpire retires an event on.
		EstimatedRestoration: tsProto(o.EstimatedRestoration),
	}}
	n.attachGeometry(ctx, ev, o.GeometryType, o.GeometryCoords, "outage "+o.ID)
	return ev
}

// buildPSPSEvents groups the PSPS coverage rows into one event per
// de-energization window.
//
// PG&E publishes a window as MANY polygon rows that share every attribute (a
// real footprint measured 12 rows for one window), so one event per row would
// render the same shutoff a dozen times and give it a dozen ids to track.
// Grouping on (EventID, TimePeriod) is the natural grain: TimePeriod IS the
// de-energization window, so it is the level at which DeEngStart/DeEngEnd and
// the customer counts actually mean something.
func (n *PowerNormalizer) buildPSPSEvents(ctx context.Context, areas []pge.PSPSArea) []*gridv1.Event {
	groups := map[string][]pge.PSPSArea{}
	var order []string
	for _, a := range areas {
		key := PSPSGroupKey(a)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], a)
	}

	events := make([]*gridv1.Event, 0, len(order))
	for _, key := range order {
		events = append(events, n.buildPSPSEvent(ctx, key, groups[key]))
	}
	return events
}

func (n *PowerNormalizer) buildPSPSEvent(ctx context.Context, key string, group []pge.PSPSArea) *gridv1.Event {
	// Attributes are constant across a group's rows (verified against a real
	// coverage set), so one row speaks for all of them — but pick it
	// deterministically, and escalate the stage, so the event's content is
	// stable across polls and never under-reports a group's worst member.
	a := PSPSRepresentative(group)

	if !hazards.PSPSStageRecognized(a.Stage) {
		// Conservatively classified SEVERE by the fail-loud default. Log so the
		// stage can be classified explicitly — this layer is active-only, so any
		// row present is a live shutoff and under-rating it is the failure mode.
		logging.Warnw(ctx, "Unrecognized PG&E PSPS stage; defaulted to SEVERE",
			"stage", a.Stage, "event", a.EventName, "timePeriod", a.TimePeriod)
	}

	ev := NewEvent(
		"psps:"+key,
		gridv1.Layer_POWER,
		SeverityFromLabel(hazards.SeverityFromPSPSStage(a.Stage)),
		pspsStatus(a, n.now()),
		pspsHeadline(a),
	)
	ev.Category = powerCatPSPS
	ev.CanonicalUrl = pge.PSPSUpdatesURL
	ev.Effective = tsProto(a.DeEnergizationStart)
	ev.ObservedAt = tsProto(a.LastUpdated)
	ev.Provenance = NewProvenance(powerSourcePSPS, pspsSourceName, pgeAttribution, pge.PSPSUpdatesURL)
	ev.Detail = &gridv1.Event_Power{Power: &gridv1.PowerDetail{
		EventId:                 a.EventID,
		EventName:               a.EventName,
		TimePeriod:              a.TimePeriod,
		Stage:                   a.Stage,
		CustomersAffected:       a.CustomersAffected,
		MedicalBaselineAffected: a.MedicalBaselineAffected,
		DeEnergizationStart:     tsProto(a.DeEnergizationStart),
		// Like ETOR, the planned end is an estimate PSPS restoration routinely
		// overruns, so it stays in the detail rather than becoming `expires`.
		DeEnergizationEnd:    tsProto(a.DeEnergizationEnd),
		EstimatedRestoration: tsProto(a.EstimatedRestoration),
		// Carried, but deliberately NOT used to resolve the event: PG&E
		// populates AllClear on rows still at stage `Watch` (where it just
		// mirrors DeEngEnd), so it is a PLANNED time, not proof the shutoff
		// ended. A shutoff ends by leaving this active-only feed, which the
		// disappearance sweep already handles.
		AllClear: tsProto(a.AllClear),
	}}

	geomType, coords, err := pge.CombineAreaGeometry(group)
	if err != nil {
		n.attachGeometry(ctx, ev, "", nil, "PSPS "+key)
		return ev
	}
	n.attachGeometry(ctx, ev, geomType, coords, "PSPS "+key)
	return ev
}

// attachGeometry sets the event geometry, or — when the upstream geometry is
// unusable — emits the event without geometry attached to every configured
// area.
//
// Dropping the event instead is not an option: both power sources use the
// `resolve` policy, so an event missing from a successful poll is transitioned
// to RESOLVED. A parse failure would therefore publish "power restored" for an
// outage PG&E is still reporting. Over-attachment is the same bias the
// evacuation normalizer takes, and it is bounded here because the poll is
// already bbox-scoped to the configured areas.
func (n *PowerNormalizer) attachGeometry(ctx context.Context, ev *gridv1.Event, geomType string, coords []byte, what string) {
	if geomType != "" && len(coords) > 0 {
		if geom, err := geometryFromTyped(geomType, coords); err == nil {
			ev.Geometry = geom
			return
		} else {
			logging.Errorw(ctx, "PG&E geometry unusable; emitting without geometry",
				"what", what, "type", geomType, "error", err)
		}
	}
	for _, area := range n.cfg.Hazards.Areas {
		ev.PlaceIds = append(ev.PlaceIds, "area:"+area.ID)
	}
}

// PSPSGroupKey identifies one de-energization window. Exported so the test-pge
// diagnostic groups exactly the way the poller does — a tool that reports
// different ids than the thing it diagnoses is worse than no tool.
//
// THE KEY MUST BE BUILT ONLY FROM IMMUTABLE FIELDS. It becomes the event id, and
// this source uses the `resolve` policy: if the id changes between polls, the old
// id is missing from a successful poll and the sweep RESOLVES it — publishing
// "shutoff cancelled" for a shutoff that is still on, and starting a fresh
// history for it. That is the worst failure this layer can produce.
//
// So the key is `EventID:TimePeriod` — PG&E's own identifiers, both stable for
// the life of a window. Deliberately NOT part of it:
//
//   - `Stage`, which is mutable by design (Watch escalates to Warning — the
//     single most important transition this layer reports).
//   - `DeEngEnd` / ETOR, which PG&E revises as a shutoff runs long.
//
// The fallbacks below are for rows PG&E has never actually published. They
// prefer a STABLE id over a precise one: with no TimePeriod, every window of an
// event collapses onto the event id, which under-reports the windows but keeps
// the shutoff continuously tracked. Under-reporting granularity is recoverable;
// a fabricated all-clear is not.
func PSPSGroupKey(a pge.PSPSArea) string {
	event := nonEmpty(a.EventID, a.EventName)
	switch {
	case event != "" && a.TimePeriod != "":
		return event + ":" + a.TimePeriod
	case event != "":
		return event
	case a.TimePeriod != "":
		return a.TimePeriod
	default:
		// No upstream identity at all. The start of de-energization is the only
		// remaining thing that names this window; a revision to it would move
		// the id, but there is nothing more stable to key on.
		sum := sha256.Sum256([]byte(a.DeEnergizationStart.String()))
		return "window-" + hex.EncodeToString(sum[:4])
	}
}

// PSPSRepresentative picks the row whose attributes speak for a whole group and
// returns it with the group's WORST stage.
//
// Ordinarily every row in a group is identical, so this is a no-op. It matters
// on the collapsed-key fallbacks above, where rows from different windows can
// share one id: the representative is then the EARLIEST window (the shutoff
// starts when its first window does) carrying the most severe stage present, so
// a group holding both a Watch and a Warning reports the Warning. Same
// life-safety bias as the evacuation normalizer's collision collapse — never
// summarize a group as less urgent than its worst member.
func PSPSRepresentative(group []pge.PSPSArea) pge.PSPSArea {
	// Exported, so it must not panic on an empty group or reorder the caller's
	// slice underneath them — an internal-only helper could assume both.
	if len(group) == 0 {
		return pge.PSPSArea{}
	}
	sorted := make([]pge.PSPSArea, len(group))
	copy(sorted, group)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].DeEnergizationStart.Equal(sorted[j].DeEnergizationStart) {
			return sorted[i].DeEnergizationStart.Before(sorted[j].DeEnergizationStart)
		}
		return string(sorted[i].GeometryCoords) < string(sorted[j].GeometryCoords)
	})
	rep := sorted[0]
	for _, a := range sorted[1:] {
		if severityRank(hazards.SeverityFromPSPSStage(a.Stage)) > severityRank(hazards.SeverityFromPSPSStage(rep.Stage)) {
			rep.Stage = a.Stage
		}
	}
	return rep
}

// severityRank orders the unified severity labels for the comparison above.
func severityRank(label string) int { return int(SeverityFromLabel(label).Number()) }

// pspsStatus splits a shutoff into SCHEDULED (announced, not yet started) and
// ACTIVE (de-energization underway). grid.proto names SCHEDULED as the planned-
// PSPS status.
//
// A row with NO start time is ACTIVE, not SCHEDULED: the layer is active-only,
// so a listed shutoff we cannot place in the future is happening as far as we
// can tell, and SCHEDULED events never escalate a place's summary mode.
func pspsStatus(a pge.PSPSArea, now time.Time) gridv1.EventStatus {
	if !a.DeEnergizationStart.IsZero() && a.DeEnergizationStart.After(now) {
		return gridv1.EventStatus_SCHEDULED
	}
	return gridv1.EventStatus_ACTIVE
}

// powerOutageHeadline names what is out and how big it is. It deliberately
// carries no timestamps: effective/updatedAt are typed fields on the same
// record, and restating them is the mistake the NWS headline rework fixed.
func powerOutageHeadline(o pge.Outage, planned bool) string {
	what := "Power outage"
	if planned {
		what = "Planned outage"
	}
	head := fmt.Sprintf("%s — %s", what, customersPhrase(o.CustomersAffected))
	// The cause of a planned outage is "it was planned", which the prefix
	// already says.
	if c := humanPowerCause(o.Cause); c != "" && !planned {
		head += " (" + c + ")"
	}
	return head
}

// pspsHeadline leads with the stage, because Watch and Warning are what a
// reader acts on differently. The medical-baseline count rides along when
// PG&E reports one: it is the number that decides whether neighbours need
// checking on, and no other feed we carry has it.
func pspsHeadline(a pge.PSPSArea) string {
	stage := strings.TrimSpace(a.Stage)
	if stage == "" {
		stage = "Notice"
	}
	head := fmt.Sprintf("PSPS %s — %s", stage, customersPhrase(a.CustomersAffected))
	if a.MedicalBaselineAffected > 0 {
		head += fmt.Sprintf(", %s medical baseline", commaNum(float64(a.MedicalBaselineAffected)))
	}
	return head
}

// customersPhrase renders a customer count, distinguishing "none reported"
// from a real zero-ish number rather than printing a bare "0 customers".
func customersPhrase(n int32) string {
	switch {
	case n <= 0:
		return "customer count not reported"
	case n == 1:
		return "1 customer"
	default:
		// Separated: these render in the detail pane, whose screenshot contract
		// (web/screenshots/events-contract.mjs check 9) rejects unseparated 4+
		// digit numbers — and PSPS customer counts are the largest numbers this
		// service emits. Same helper the wildfire acreage headline uses.
		return fmt.Sprintf("%s customers", commaNum(float64(n)))
	}
}

// powerCauses translates PG&E's abbreviated outage-cause codes into readable
// phrases. This is a static lookup, not an AI enhancement, for the same reason
// chain-control R1/R2/R3 is: the vocabulary is a small closed set of codes, so
// translating it is a table, and a table cannot hallucinate a cause.
//
// Codes observed live across a statewide sample. An unlisted code falls through
// to a lower-cased version of the raw code rather than being dropped — an
// unknown cause is still information, and it surfaces the code so it can be
// added here.
var powerCauses = map[string]string{
	"PLNND SHUTDOWN":         "planned shutdown",
	"PATROLLING":             "crew patrolling the line",
	"AWAITING INVESTIGATION": "cause under investigation",
	"REPLCE TXFMR":           "transformer replacement",
	"EMERG REPAIRS":          "emergency repairs",
	"THRD PARTY":             "third-party damage",
	"LGHTNING":               "lightning",
	"REPAIR WIRE DWN":        "downed wire",
	"BRKN POLE":              "broken pole",
	"BRKN POLE EQUIPMNT":     "broken pole equipment",
	"BRKN UG EQUIPMNT":       "broken underground equipment",
	"DAMGE UG CABLE":         "damaged underground cable",
	"CAR POLE":               "vehicle struck a pole",
	"FOREIGN OBJ":            "foreign object on the line",
	"TREE CONTACT":           "tree contact",
	"FIRE":                   "fire",
	"WEATHER":                "weather",
	"ANIMAL":                 "animal contact",
	"VEHICLE":                "vehicle accident",
	"EQUIPMENT":              "equipment failure",
	"NO ACCESS":              "crew unable to access",
}

// humanPowerCause renders a PG&E cause code, "" when the feed reported none
// (which it does on roughly half of all rows).
func humanPowerCause(code string) string {
	c := strings.TrimSpace(code)
	if c == "" {
		return ""
	}
	if human, ok := powerCauses[strings.ToUpper(c)]; ok {
		return human
	}
	return strings.ToLower(c)
}
