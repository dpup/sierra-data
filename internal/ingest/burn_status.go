package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/calfireburn"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// Burn status source constants. Two sources, one poller: they are separate
// authorities that fail independently (see the package guide's poller ≠ source
// rule).
const (
	burnLineSourceID     = "burnline"
	burnLineSourceName   = "County Burn Line"
	burnLineAttribution  = "County air district burn information line"
	calfireBurnSourceID  = "calfire-burn"
	calfireBurnName      = "CAL FIRE Burn Permits"
	calfireBurnAttrib    = "CAL FIRE"
	calfireBurnStatusURL = "https://burnpermit.fire.ca.gov/current-burn-status"
)

// BurnReadingSource is the read side of the burn-line staging table (satisfied
// by *store.Store; a fake is used in tests).
//
// The burn-day facet is the service's only PUSHED source: cmd/burn-line calls
// the county line on its own schedule and posts the reading to /ingest/burn-line,
// which lands it in burn_readings. Poll then reads the latest row on the
// scheduler's tick — the same push-source-wrapped-as-a-poller shape MeshCore
// uses, which keeps single-writer discipline and tick-based health unchanged.
//
// Reading from the store (rather than an in-memory buffer) is what makes the
// reading survive a restart. It carries its own ObservedAt, so a rehydrated
// reading is re-judged by the freshness gate rather than resurrected as current.
type BurnReadingSource interface {
	BurnReadings(ctx context.Context) (map[string]store.BurnReading, error)
}

// CalfireBurnClient is the read side of the CAL FIRE burn status page.
type CalfireBurnClient interface {
	GetBurnStatus(ctx context.Context) (map[string]calfireburn.CountyStatus, error)
}

// PlaceIndex resolves which place ids a county's status attaches to. Burn
// status is an ADMINISTRATIVE fact about a county, not a geographic footprint,
// so it carries no geometry and presets place_ids instead — the same mechanism
// NWS zone alerts use (store.UpsertEvent unions preset ids with geometric
// matches, and never drops the preset ones).
//
// Attaching a county polygon instead would work, but would write 18-32 KB of
// geometry into the event AND into every revision, to express a fact that is
// true of the county by definition.
type PlaceIndex interface {
	// PlaceIDsForCounty returns the county place id plus every place contained
	// by it (towns parented to it, and any area overlapping it), so that a
	// query for a town inside the county sees its burn status.
	PlaceIDsForCounty(ctx context.Context, countySlug string) ([]string, error)
}

// BurnStatusNormalizer ingests per-county residential burning status (id
// namespace "burn:") from two independent authorities.
//
// The events are AMBIENT: one per configured county, ACTIVE at all times,
// severity INFO, excluded from the summary hazard rollup exactly like mesh-node
// presence. Their value is the revision history — "when did it change" is the
// question this layer exists to answer, which is why the volatile per-reading
// fields live in BurnObservation (zeroed by store.ContentHash) and only a real
// transition mints a revision.
type BurnStatusNormalizer struct {
	cfg     *config.Config
	calfire CalfireBurnClient
	// readings is the staging table of pushed burn-day readings. A county with
	// no row still gets an event, with the burn-day facet UNKNOWN.
	readings BurnReadingSource
	places   PlaceIndex
	now      func() time.Time
}

// NewBurnStatusNormalizer wires the normalizer to the CAL FIRE scrape and the
// pushed-reading staging table.
func NewBurnStatusNormalizer(cfg *config.Config, calfire CalfireBurnClient, readings BurnReadingSource, places PlaceIndex) *BurnStatusNormalizer {
	return &BurnStatusNormalizer{cfg: cfg, calfire: calfire, readings: readings, places: places, now: time.Now}
}

// SourceIDs implements Normalizer. The burn line row is only claimed when at
// least one county actually has one — otherwise the registry would show a
// permanently-unattempted source.
func (n *BurnStatusNormalizer) SourceIDs() []string {
	ids := []string{calfireBurnSourceID}
	if len(n.cfg.Grid.Burn.EnabledLines()) > 0 {
		ids = append(ids, burnLineSourceID)
	}
	return ids
}

// Poll implements Normalizer.
//
// Both upstreams are optional-but-not-silently: a failure of either degrades
// that SOURCE (PerSource) while the other's facet still publishes, and the
// failed facet reads UNKNOWN rather than carrying forward a value we can no
// longer vouch for. A total failure of both is a hard error, because then the
// event set we would emit says nothing at all.
func (n *BurnStatusNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	counties := n.cfg.Grid.Burn.Counties
	if len(counties) == 0 {
		// An empty scope is a hard error, never a success-empty result: the
		// disappearance sweep would RESOLVE every stored burn status event on
		// the strength of a config regression, with no fetch ever made.
		return nil, errEmptyScope("burn counties")
	}

	perSource := map[string]error{}

	// --- CAL FIRE facet (statewide table, one fetch for every county) ---
	calfireRows, calfireErr := n.calfire.GetBurnStatus(ctx)
	if calfireErr != nil {
		perSource[calfireBurnSourceID] = calfireErr
	}

	// --- burn-day facet (the staging table of pushed readings) ---
	//
	// There is no fetch here. cmd/burn-line pushes a reading when it takes one;
	// this reads whatever has landed. "Nothing has landed recently" is therefore
	// the ONLY failure mode, and the freshness gate is what detects it — a push
	// pipeline that silently stops looks exactly like one that has not run yet.
	staged, readErr := n.readings.BurnReadings(ctx)
	if readErr != nil {
		// The staging table being unreadable is OUR failure, not the pipeline's.
		return nil, fmt.Errorf("burn status: read staged readings: %w", readErr)
	}

	// Judge every CONFIGURED line once, regardless of how many counties it speaks
	// for — a shared air-district line is one reading, not one per county.
	//
	// Note the asymmetry, which is deliberate: a fresh reading is USED for any
	// configured line, but only a DIALED one is EXPECTED. `enabled` governs what
	// we call and what we hold the source accountable for, not what we are
	// willing to believe. The push endpoint already accepts a reading for any
	// configured line, so accepting one and then silently ignoring it would be
	// the surprising behaviour — and it would make a manual one-off push useless.
	enabled := n.cfg.Grid.Burn.EnabledLines()
	usable := make(map[string]*store.BurnReading, len(staged))
	var lineErrs []string
	for _, l := range n.cfg.Grid.Burn.Lines {
		r, ok := staged[strings.ToLower(l.ID)]
		if !ok {
			if l.Dialed() {
				lineErrs = append(lineErrs, fmt.Sprintf("%s: no reading has been pushed", l.ID))
			}
			continue
		}
		if err := n.freshnessError(r.ObservedAt); err != nil {
			// A push pipeline that stopped is a FAILED source, not a healthy one
			// serving old data: /api/v1/sources must say so, and the facet must
			// read UNKNOWN rather than assert a day-old answer as today's.
			if l.Dialed() {
				lineErrs = append(lineErrs, fmt.Sprintf("%s: %v", l.ID, err))
			}
			continue
		}
		usable[l.ID] = &r
	}
	expected := len(enabled)
	if len(lineErrs) > 0 {
		sort.Strings(lineErrs)
		perSource[burnLineSourceID] = fmt.Errorf("burn line: %s", strings.Join(lineErrs, "; "))
	}

	if calfireErr != nil && expected > 0 && len(lineErrs) == expected {
		// Nothing was readable from either authority. Emitting events whose every
		// facet is UNKNOWN would overwrite real stored values with blanks.
		return nil, fmt.Errorf("burn status: all sources failed (calfire: %v; %s)",
			calfireErr, strings.Join(lineErrs, "; "))
	}
	if calfireErr != nil && expected == 0 {
		return nil, calfireErr
	}

	now := n.now().UTC()
	events := make([]*gridv1.Event, 0, len(counties))
	for _, c := range counties {
		ev, err := n.buildEvent(ctx, c, calfireRows, usable, now, prior)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}

// freshnessError reports a burn-line reading that is too old to trust. See
// config.BurnConfig.BurnDayStaleAfter for why this gate exists.
func (n *BurnStatusNormalizer) freshnessError(observed time.Time) error {
	max := n.cfg.Grid.Burn.BurnDayStale()
	if max <= 0 {
		return nil // explicitly disabled by the operator
	}
	if observed.IsZero() {
		return fmt.Errorf("burn-day reading has no timestamp")
	}
	if age := n.now().UTC().Sub(observed); age > max {
		return fmt.Errorf("burn-day reading is stale (%s old, limit %s)",
			age.Round(time.Minute), max)
	}
	return nil
}

// buildEvent composes one county's ambient burn status event.
func (n *BurnStatusNormalizer) buildEvent(
	ctx context.Context,
	c config.BurnCounty,
	calfireRows map[string]calfireburn.CountyStatus,
	usable map[string]*store.BurnReading,
	now time.Time,
	prior Prior,
) (*gridv1.Event, error) {
	detail := &gridv1.BurnStatusDetail{}

	// CAL FIRE facet. Absent from a SUCCESSFUL fetch is meaningful (the table
	// lists every county), so it stays UNKNOWN rather than inventing a value.
	if calfireRows != nil {
		if row, ok := calfireRows[countyKey(c)]; ok {
			detail.CalfireStatus = calfireStatusProto(row.Status)
			detail.CalfireArea = row.Area
			detail.CalfireEffective = tsProto(row.Effective)
			detail.CalfireObservedAt = tsProto(now)
		}
	}

	// Burn-day facet: every line that speaks for this county, each with its own
	// answer. A line with no usable reading is still PUBLISHED, with burn_day
	// UNKNOWN and its phone populated — "we could not read it, here is the
	// number" is more useful than omitting it.
	lines := n.cfg.Grid.Burn.LinesForCounty(c.Place)
	for _, l := range lines {
		lr := &gridv1.BurnLineReading{Id: l.ID, Name: l.Name, Phone: l.Phone}
		if r := usable[l.ID]; r != nil {
			lr.BurnDay = burnDayProto(r.Status)
			lr.Observation = &gridv1.BurnObservation{
				Message:    r.Message,
				Transcript: r.Transcript,
				Confidence: r.Confidence,
				ObservedAt: tsProto(r.ObservedAt),
			}
		}
		detail.BurnLines = append(detail.BurnLines, lr)
	}
	detail.BurnDay = mergeBurnDays(lines, usable)

	// A facet we could not read this tick carries FORWARD the stored value only
	// for CAL FIRE, whose truth changes about twice a year and whose page being
	// down is no evidence the suspension lifted. The burn-day facet is
	// deliberately NOT carried forward: it is a statement about TODAY, and a
	// yesterday's-answer is exactly the failure the freshness gate exists to
	// catch. Durability for it lives in the staging row, which keeps a real
	// timestamp the gate can judge.
	if detail.CalfireStatus == gridv1.CalfireBurnStatus_CALFIRE_BURN_STATUS_UNKNOWN {
		if p := prior.ByID(burnEventID(c)); p != nil {
			if pd := p.GetBurnStatus(); pd != nil && pd.GetCalfireStatus() != gridv1.CalfireBurnStatus_CALFIRE_BURN_STATUS_UNKNOWN {
				detail.CalfireStatus = pd.GetCalfireStatus()
				detail.CalfireArea = pd.GetCalfireArea()
				detail.CalfireEffective = pd.GetCalfireEffective()
				detail.CalfireObservedAt = pd.GetCalfireObservedAt()
			}
		}
	}

	detail.Permission = derivePermission(detail.BurnDay, detail.CalfireStatus)

	ev := NewEvent(
		burnEventID(c),
		gridv1.Layer_BURN_STATUS,
		gridv1.Severity_INFO, // ambient advisory state, never a hazard severity
		gridv1.EventStatus_ACTIVE,
		burnHeadline(c, detail),
	)
	ev.Category = "burn_status"
	ev.AreaLabel = c.Name
	ev.CanonicalUrl = calfireBurnStatusURL
	ev.ObservedAt = tsProto(now)
	// Provenance names the source whose facet a reader is most likely acting on:
	// the burn line is the operative daily answer for a county we dial, CAL FIRE
	// otherwise.
	//
	// It keys off CONFIGURATION, never off whether a reading was available.
	// Provenance is part of the content hash (only fetched_at is zeroed), so
	// deriving it from availability would flip source_id on every missed run and
	// mint a spurious revision pair on an event whose status never changed —
	// burying real transitions in exactly the history this layer exists to keep.
	// It also keeps an event's id stable within one source for the disappearance
	// sweep, which scopes by provenance.
	if countyIsDialed(lines) {
		// No source URL: the authority is a recorded PHONE LINE, which has no
		// page. The numbers themselves ride on detail.burn_lines, which is the
		// thing a reader can actually act on.
		ev.Provenance = NewProvenance(burnLineSourceID, burnLineSourceName, burnLineAttribution, "")
	} else {
		ev.Provenance = NewProvenance(calfireBurnSourceID, calfireBurnName, calfireBurnAttrib, calfireBurnStatusURL)
	}
	ev.Detail = &gridv1.Event_BurnStatus{BurnStatus: detail}

	ids, err := n.places.PlaceIDsForCounty(ctx, c.Place)
	if err != nil {
		return nil, fmt.Errorf("burn status: place ids for %s: %w", c.Place, err)
	}
	ev.PlaceIds = ids
	return ev, nil
}

// countyIsDialed reports whether any of a county's lines is one we call.
func countyIsDialed(lines []config.BurnLine) bool {
	for _, l := range lines {
		if l.Dialed() {
			return true
		}
	}
	return false
}

// mergeBurnDays collapses a county's lines into one answer, taking the MOST
// RESTRICTIVE — the same asymmetry derivePermission applies across facets, for
// the same reason.
//
//   - Any line saying NO wins outright: a prohibition from any authority that
//     speaks for this county is conclusive.
//   - Otherwise any MARGINAL (an elevation-restricted burn day) wins, because it
//     is a restriction and YES is not.
//   - YES requires that EVERY DIALED line produced a usable answer and all of
//     them said yes. If a line we expected to read is missing or stale, we do
//     not know, and "do not know" must never render as a green light.
//   - A line we merely publish and never dial cannot BLOCK a YES — we never
//     asked it anything — but a fresh reading that arrives for it is still used.
func mergeBurnDays(lines []config.BurnLine, usable map[string]*store.BurnReading) gridv1.BurnDay {
	var sawYes, sawMarginal, missingDialed bool
	for _, l := range lines {
		r := usable[l.ID]
		if r == nil {
			if l.Dialed() {
				missingDialed = true
			}
			continue
		}
		switch burnDayProto(r.Status) {
		case gridv1.BurnDay_BURN_DAY_NO:
			return gridv1.BurnDay_BURN_DAY_NO
		case gridv1.BurnDay_BURN_DAY_MARGINAL:
			sawMarginal = true
		case gridv1.BurnDay_BURN_DAY_YES:
			sawYes = true
		default:
			// An unreadable status from a line we dialed is a gap, not a yes.
			if l.Dialed() {
				missingDialed = true
			}
		}
	}
	if sawMarginal {
		return gridv1.BurnDay_BURN_DAY_MARGINAL
	}
	if sawYes && !missingDialed {
		return gridv1.BurnDay_BURN_DAY_YES
	}
	return gridv1.BurnDay_BURN_DAY_UNKNOWN
}

// burnEventID is built only from the county place slug — an immutable
// identifier. Deriving it from any status field would mint a new id whenever the
// status changed, and the sweep would then RESOLVE the old one: a fabricated
// history restart on exactly the transition this layer exists to record.
func burnEventID(c config.BurnCounty) string { return "burn:" + c.Place }

// countyKey maps a county place slug onto the CAL FIRE table's key
// ("calaveras-county" -> "calaveras").
func countyKey(c config.BurnCounty) string {
	return strings.TrimSuffix(strings.ToLower(c.Place), "-county")
}

// burnDayProto maps the extraction vocabulary onto the proto enum.
//
// "orange" is specifically an ELEVATION-RESTRICTED burn day ("permissive burn
// days at 3500 feet elevation or more"), not a vague middle. It maps to
// MARGINAL, which derivePermission deliberately refuses to call ALLOWED — the
// restriction is the part that matters and only observation.message carries it.
//
// An unrecognized value maps to UNKNOWN, never to a permissive default. The
// push endpoint already rejects unknown vocabulary, so reaching this default
// means a row predates a vocabulary change.
func burnDayProto(status string) gridv1.BurnDay {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "green":
		return gridv1.BurnDay_BURN_DAY_YES
	case "red":
		return gridv1.BurnDay_BURN_DAY_NO
	case "orange":
		return gridv1.BurnDay_BURN_DAY_MARGINAL
	default:
		return gridv1.BurnDay_BURN_DAY_UNKNOWN
	}
}

func calfireStatusProto(s calfireburn.Status) gridv1.CalfireBurnStatus {
	switch s {
	case calfireburn.StatusBurningSuspended:
		return gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED
	case calfireburn.StatusPermitRequired:
		return gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED
	case calfireburn.StatusNoPermitRequired:
		return gridv1.CalfireBurnStatus_CALFIRE_NO_PERMIT_REQUIRED
	default:
		return gridv1.CalfireBurnStatus_CALFIRE_BURN_STATUS_UNKNOWN
	}
}

// derivePermission resolves the two facets into one answer, taking the MORE
// RESTRICTIVE of the two and never the more permissive.
//
// The asymmetry is deliberate and is the whole point of the function: a
// PROHIBITED facet is conclusive on its own (either authority can forbid
// burning), while ALLOWED requires BOTH facets to be known and permissive.
// Anything else is UNKNOWN. Someone acts on this holding a match, so "we could
// not read one of the authorities" must never render as a green light.
func derivePermission(day gridv1.BurnDay, calfire gridv1.CalfireBurnStatus) gridv1.BurnPermission {
	if day == gridv1.BurnDay_BURN_DAY_NO || calfire == gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED {
		return gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED
	}
	calfireOK := calfire == gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED ||
		calfire == gridv1.CalfireBurnStatus_CALFIRE_NO_PERMIT_REQUIRED
	if day == gridv1.BurnDay_BURN_DAY_YES && calfireOK {
		return gridv1.BurnPermission_BURN_PERMISSION_ALLOWED
	}
	// MARGINAL, or either facet unknown.
	return gridv1.BurnPermission_BURN_PERMISSION_UNKNOWN
}

// burnHeadline composes the display line deterministically from the two facets.
//
// Deterministic on purpose, for the same reason as the NWS alert headline:
// store.ContentHash does NOT zero Headline, so a generated or reworded headline
// would differ on every tick, mint a revision each time, and bury the real
// transitions. It also makes it structurally impossible to render a suspension
// as permission.
func burnHeadline(c config.BurnCounty, d *gridv1.BurnStatusDetail) string {
	var parts []string
	switch d.GetBurnDay() {
	case gridv1.BurnDay_BURN_DAY_YES:
		parts = append(parts, "burn day")
	case gridv1.BurnDay_BURN_DAY_NO:
		parts = append(parts, "no-burn day")
	case gridv1.BurnDay_BURN_DAY_MARGINAL:
		parts = append(parts, "restricted burn day")
	default:
		parts = append(parts, "burn day unknown")
	}
	switch d.GetCalfireStatus() {
	case gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED:
		parts = append(parts, "CAL FIRE burning suspended")
	case gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED:
		parts = append(parts, "CAL FIRE permit required")
	case gridv1.CalfireBurnStatus_CALFIRE_NO_PERMIT_REQUIRED:
		parts = append(parts, "CAL FIRE permit not required")
	default:
		parts = append(parts, "CAL FIRE status unknown")
	}
	return fmt.Sprintf("%s — %s", c.Name, strings.Join(parts, "; "))
}
