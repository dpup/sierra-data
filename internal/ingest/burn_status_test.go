package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/calfireburn"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/pushingest"
	"github.com/dpup/sierra-data/internal/store"
)

var burnNow = time.Date(2026, 9, 10, 20, 0, 0, 0, time.UTC)

type fakeCalfireBurn struct {
	rows map[string]calfireburn.CountyStatus
	err  error
}

func (f *fakeCalfireBurn) GetBurnStatus(context.Context) (map[string]calfireburn.CountyStatus, error) {
	return f.rows, f.err
}

// fakeReadings stands in for the burn_readings staging table the push endpoint
// writes.
type fakeReadings struct {
	rows map[string]store.BurnReading
	err  error
}

func (f *fakeReadings) BurnReadings(context.Context) (map[string]store.BurnReading, error) {
	return f.rows, f.err
}

// staged builds a staging table holding one reading for a line.
func staged(lineID, status string, observed time.Time) *fakeReadings {
	return &fakeReadings{rows: map[string]store.BurnReading{
		lineID: {
			LineID: lineID, Status: status, Message: "msg",
			Transcript: "transcript", Confidence: 95, ObservedAt: observed,
		},
	}}
}

// withReading adds another line's reading to a staging table.
func (f *fakeReadings) withReading(lineID, status string, observed time.Time) *fakeReadings {
	f.rows[lineID] = store.BurnReading{
		LineID: lineID, Status: status, Message: "msg",
		Transcript: "transcript", Confidence: 95, ObservedAt: observed,
	}
	return f
}

// freshLine is a staging table holding a Calaveras reading taken 3h ago.
func freshLine(status string) *fakeReadings {
	return staged(calaverasLine.ID, status, burnNow.Add(-3*time.Hour))
}

// noReadings is an empty staging table — nothing has ever been pushed.
func noReadings() *fakeReadings { return &fakeReadings{rows: map[string]store.BurnReading{}} }

type fakePlaceIndex struct{ err error }

func (f fakePlaceIndex) PlaceIDsForCounty(_ context.Context, slug string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []string{"county:" + slug, "area:ebbetts-pass"}, nil
}

// calaverasLine is dialed; tuolumneLine is published but not dialed.
var calaverasLine = config.BurnLine{
	ID: "calaveras-apcd", Name: "Calaveras County burn line",
	Phone: "+12097546600", Counties: []string{"calaveras-county"}, Enabled: true,
}
var tuolumneLine = config.BurnLine{
	ID: "tuolumne-apcd", Name: "Tuolumne County APCD burn line",
	Phone: "+12095335598", Counties: []string{"tuolumne-county"}, Enabled: false,
}

var calaveras = config.BurnCounty{Place: "calaveras-county", Name: "Calaveras County"}
var tuolumne = config.BurnCounty{Place: "tuolumne-county", Name: "Tuolumne County"}

func burnCfg(counties []config.BurnCounty, lines ...config.BurnLine) *config.Config {
	return &config.Config{Grid: config.GridConfig{Burn: config.BurnConfig{
		Counties: counties, Lines: lines,
	}}}
}

// stdCfg is the shipped shape: both counties, Calaveras dialed.
func stdCfg() *config.Config {
	return burnCfg([]config.BurnCounty{calaveras, tuolumne}, calaverasLine, tuolumneLine)
}

// calOnly is just Calaveras with its dialed line.
func calOnly() *config.Config {
	return burnCfg([]config.BurnCounty{calaveras}, calaverasLine)
}

func suspendedRows() map[string]calfireburn.CountyStatus {
	eff := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	return map[string]calfireburn.CountyStatus{
		"calaveras": {County: "Calaveras", Status: calfireburn.StatusBurningSuspended, Area: "All SRA", Effective: eff},
		"tuolumne":  {County: "Tuolumne", Status: calfireburn.StatusBurningSuspended, Area: "All SRA", Effective: eff},
	}
}

func newBurnNormalizer(cfg *config.Config, cf CalfireBurnClient, readings BurnReadingSource) *BurnStatusNormalizer {
	n := NewBurnStatusNormalizer(cfg, cf, readings, fakePlaceIndex{}, nil)
	n.now = func() time.Time { return burnNow }
	return n
}

func byID(events []*gridv1.Event, id string) *gridv1.Event {
	for _, ev := range events {
		if ev.GetId() == id {
			return ev
		}
	}
	return nil
}

// --- the core safety property -------------------------------------------------

// derivePermission takes the MORE RESTRICTIVE of the two facets, and ALLOWED
// requires both to be known and permissive. Someone acts on this holding a
// match: an unreadable authority must never render as a green light.
func TestDerivePermission(t *testing.T) {
	const (
		unknownDay = gridv1.BurnDay_BURN_DAY_UNKNOWN
		yesDay     = gridv1.BurnDay_BURN_DAY_YES
		noDay      = gridv1.BurnDay_BURN_DAY_NO
		marginal   = gridv1.BurnDay_BURN_DAY_MARGINAL

		cfUnknown   = gridv1.CalfireBurnStatus_CALFIRE_BURN_STATUS_UNKNOWN
		cfSuspended = gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED
		cfPermit    = gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED
		cfNoPermit  = gridv1.CalfireBurnStatus_CALFIRE_NO_PERMIT_REQUIRED

		allowed    = gridv1.BurnPermission_BURN_PERMISSION_ALLOWED
		prohibited = gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED
		unknown    = gridv1.BurnPermission_BURN_PERMISSION_UNKNOWN
	)
	cases := []struct {
		name    string
		day     gridv1.BurnDay
		calfire gridv1.CalfireBurnStatus
		want    gridv1.BurnPermission
	}{
		{"both permit", yesDay, cfPermit, allowed},
		{"both permit, no permit needed", yesDay, cfNoPermit, allowed},
		{"suspension overrides a burn day", yesDay, cfSuspended, prohibited},
		{"no-burn day overrides a CAL FIRE permit", noDay, cfPermit, prohibited},
		{"both prohibit", noDay, cfSuspended, prohibited},
		// The asymmetry: PROHIBITED is conclusive alone, ALLOWED never is.
		{"prohibited is conclusive even if the other is unknown", noDay, cfUnknown, prohibited},
		{"suspended is conclusive even if the other is unknown", unknownDay, cfSuspended, prohibited},
		{"unknown burn day is NOT allowed", unknownDay, cfPermit, unknown},
		{"unknown calfire is NOT allowed", yesDay, cfUnknown, unknown},
		{"both unknown", unknownDay, cfUnknown, unknown},
		// "orange" is an ELEVATION-RESTRICTED burn day; the restriction is the
		// part that matters and only observation.message carries it.
		{"marginal is never allowed", marginal, cfPermit, unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, derivePermission(tc.day, tc.calfire))
		})
	}
}

func TestBurnDayProto(t *testing.T) {
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_YES, burnDayProto("green"))
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_NO, burnDayProto("red"))
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_MARGINAL, burnDayProto("orange"))
	// Never a permissive default.
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, burnDayProto("chartreuse"))
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, burnDayProto(""))
}

// --- happy path ---------------------------------------------------------------

func TestBurnPoll_BothFacets(t *testing.T) {
	n := newBurnNormalizer(stdCfg(),
		&fakeCalfireBurn{rows: suspendedRows()}, freshLine("red"))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 2)
	assert.Empty(t, res.PerSource)

	cal := byID(res.Events, "burn:calaveras-county")
	require.NotNil(t, cal)
	d := cal.GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_NO, d.GetBurnDay())
	assert.Equal(t, gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED, d.GetCalfireStatus())
	assert.Equal(t, gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED, d.GetPermission())
	assert.Equal(t, "All SRA", d.GetCalfireArea())
	require.Len(t, d.GetBurnLines(), 1)
	assert.Equal(t, calaverasLine.ID, d.GetBurnLines()[0].GetId())
	assert.Equal(t, "+12097546600", d.GetBurnLines()[0].GetPhone())
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_NO, d.GetBurnLines()[0].GetBurnDay())
	assert.Equal(t, int32(95), d.GetBurnLines()[0].GetObservation().GetConfidence())
	assert.Equal(t, gridv1.Severity_INFO, cal.GetSeverity(), "ambient state is always INFO")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, cal.GetStatus())
	assert.Equal(t, []string{"county:calaveras-county", "area:ebbetts-pass"}, cal.GetPlaceIds())
}

// A county we publish a number for but do NOT dial still gets an event, honestly
// reporting the gap — and must not degrade the source for a reading never asked
// for.
func TestBurnPoll_UndialedCountyIsPublishedAndDoesNotFailTheSource(t *testing.T) {
	n := newBurnNormalizer(stdCfg(),
		&fakeCalfireBurn{rows: suspendedRows()}, freshLine("red"))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource, "an undialed county is not a burn-line failure")

	tuo := byID(res.Events, "burn:tuolumne-county")
	require.NotNil(t, tuo)
	d := tuo.GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, d.GetBurnDay())
	assert.Equal(t, gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED, d.GetCalfireStatus())
	// Still conclusive: the suspension alone prohibits burning.
	assert.Equal(t, gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED, d.GetPermission())
	require.Len(t, d.GetBurnLines(), 1, "an undialed line is still PUBLISHED")
	assert.Equal(t, "+12095335598", d.GetBurnLines()[0].GetPhone(), "the real authority is always reachable")
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, d.GetBurnLines()[0].GetBurnDay())
}

// --- the freeze gate ----------------------------------------------------------

// A push pipeline that silently stops looks exactly like one that has not run
// yet, so age is the ONLY signal. A stale "burn day" would tell someone today is
// fine when the district may since have said otherwise.
func TestBurnPoll_StaleReadingFailsTheSourceAndBlanksTheFacet(t *testing.T) {
	n := newBurnNormalizer(calOnly(),
		&fakeCalfireBurn{rows: suspendedRows()},
		staged(calaverasLine.ID, "green", burnNow.Add(-48*time.Hour))) // past the 36h default

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)

	require.Error(t, res.PerSource[burnLineSourceID], "a stopped pipeline is a FAILED source")
	assert.Contains(t, res.PerSource[burnLineSourceID].Error(), "stale")

	d := byID(res.Events, "burn:calaveras-county").GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, d.GetBurnDay(),
		"a stale reading must not be published as today's answer")
}

// A dialed county with nothing ever pushed is a failure, not silence.
func TestBurnPoll_NoReadingEverPushedFailsTheSource(t *testing.T) {
	n := newBurnNormalizer(calOnly(),
		&fakeCalfireBurn{rows: suspendedRows()}, noReadings())

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	require.Error(t, res.PerSource[burnLineSourceID])
	assert.Contains(t, res.PerSource[burnLineSourceID].Error(), "no reading")
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN,
		byID(res.Events, "burn:calaveras-county").GetBurnStatus().GetBurnDay())
}

func TestBurnPoll_FreshnessGateCanBeDisabled(t *testing.T) {
	cfg := calOnly()
	cfg.Grid.Burn.BurnDayStaleAfter = -1 // explicit operator opt-out
	n := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: suspendedRows()},
		staged(calaverasLine.ID, "green", burnNow.Add(-100*time.Hour)))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_YES,
		byID(res.Events, "burn:calaveras-county").GetBurnStatus().GetBurnDay())
}

// --- partial and total failure ------------------------------------------------

// CAL FIRE moves about twice a year and its page being down is no evidence the
// suspension lifted, so that facet carries forward from the stored event.
func TestBurnPoll_CalfireDownCarriesPriorSuspensionForward(t *testing.T) {
	eff := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	prior := &scriptedPrior{events: []*gridv1.Event{{
		Id: "burn:calaveras-county",
		Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
			CalfireStatus:    gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED,
			CalfireArea:      "All SRA",
			CalfireEffective: tsProto(eff),
		}},
	}}}
	n := newBurnNormalizer(calOnly(),
		&fakeCalfireBurn{err: errors.New("403 forbidden")}, freshLine("red"))

	res, err := n.Poll(context.Background(), prior)
	require.NoError(t, err, "one source down must not fail the whole poll")
	require.Error(t, res.PerSource[calfireBurnSourceID])

	d := byID(res.Events, "burn:calaveras-county").GetBurnStatus()
	assert.Equal(t, gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED, d.GetCalfireStatus())
	assert.Equal(t, "All SRA", d.GetCalfireArea())
	assert.Equal(t, eff, d.GetCalfireEffective().AsTime())
}

// The burn-day facet is NOT carried forward from the stored event: it is a
// statement about TODAY. Durability comes from the staging row (which keeps its
// own observedAt and is re-judged by the freshness gate), never from resurrecting
// the last published answer.
func TestBurnPoll_BurnDayIsNeverCarriedForwardFromPrior(t *testing.T) {
	prior := &scriptedPrior{events: []*gridv1.Event{{
		Id: "burn:calaveras-county",
		Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
			BurnDay: gridv1.BurnDay_BURN_DAY_YES,
		}},
	}}}
	n := newBurnNormalizer(calOnly(),
		&fakeCalfireBurn{rows: suspendedRows()}, noReadings())

	res, err := n.Poll(context.Background(), prior)
	require.NoError(t, err)
	require.Error(t, res.PerSource[burnLineSourceID])
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN,
		byID(res.Events, "burn:calaveras-county").GetBurnStatus().GetBurnDay())
}

// Both authorities unreadable: emitting all-UNKNOWN events would overwrite real
// stored values with blanks, so the tick must fail instead.
func TestBurnPoll_AllSourcesDownIsAHardError(t *testing.T) {
	n := newBurnNormalizer(calOnly(),
		&fakeCalfireBurn{err: errors.New("403")}, noReadings())
	_, err := n.Poll(context.Background(), &scriptedPrior{})
	require.Error(t, err)
}

// The staging table being unreadable is OUR failure, not the pipeline's, and
// must fail the tick rather than blank every facet.
func TestBurnPoll_UnreadableStagingTableIsAHardError(t *testing.T) {
	n := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()},
		&fakeReadings{err: errors.New("database is locked")})
	_, err := n.Poll(context.Background(), &scriptedPrior{})
	require.Error(t, err)
}

// An empty configured scope must be a hard error, never a success-empty result:
// the sweep would RESOLVE every stored burn status off a config regression, with
// no fetch ever made.
func TestBurnPoll_EmptyScopeIsAHardError(t *testing.T) {
	n := newBurnNormalizer(burnCfg(nil), &fakeCalfireBurn{rows: suspendedRows()}, noReadings())
	_, err := n.Poll(context.Background(), &scriptedPrior{})
	require.Error(t, err)
}

// --- identity + revision hygiene ----------------------------------------------

// THE id trap from the package guide: under a `resolve` policy the id IS the
// lifecycle handle. Deriving it from a status field would mint a new id on the
// very transition this layer exists to record, and the sweep would RESOLVE the
// old one — a fabricated history restart.
func TestBurnEventIDIsStableAcrossStatusChanges(t *testing.T) {
	rows := suspendedRows()
	n := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: rows}, freshLine("red"))
	first, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)

	// Season turns: suspension lifts and it becomes a burn day.
	rows["calaveras"] = calfireburn.CountyStatus{
		County: "Calaveras", Status: calfireburn.StatusPermitRequired, Area: "All SRA",
	}
	n2 := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: rows}, freshLine("green"))
	second, err := n2.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)

	assert.Equal(t, first.Events[0].GetId(), second.Events[0].GetId())
	assert.Equal(t, gridv1.BurnPermission_BURN_PERMISSION_ALLOWED,
		second.Events[0].GetBurnStatus().GetPermission())
}

// Provenance must key off CONFIGURATION, not on whether a reading was available.
// It is part of the content hash, so a source_id that flips with availability
// would mint a spurious revision pair every time the pipeline missed a run.
func TestBurnPoll_ProvenanceIsStableAcrossAMissedPush(t *testing.T) {
	up := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()}, freshLine("red"))
	okRes, err := up.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)

	down := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()}, noReadings())
	downRes, err := down.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)

	assert.Equal(t, burnLineSourceID, byID(okRes.Events, "burn:calaveras-county").GetProvenance().GetSourceId())
	assert.Equal(t, burnLineSourceID, byID(downRes.Events, "burn:calaveras-county").GetProvenance().GetSourceId(),
		"provenance must not flip just because no reading had been pushed")

	// A county we do not dial is attributed to CAL FIRE, always.
	noLine := newBurnNormalizer(burnCfg([]config.BurnCounty{tuolumne}, tuolumneLine), &fakeCalfireBurn{rows: suspendedRows()}, noReadings())
	res, err := noLine.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Equal(t, calfireBurnSourceID,
		byID(res.Events, "burn:tuolumne-county").GetProvenance().GetSourceId())
}

// The county line's message NAMES THE DATE, so its text differs daily even when
// the answer has not changed. If that were hashed, every day would mint a
// revision and bury the handful of real transitions.
func TestBurnStatus_ObservationChangeDoesNotMintARevision(t *testing.T) {
	mk := func(msg string, ts time.Time, conf int32) *gridv1.Event {
		return &gridv1.Event{
			Id:    "burn:calaveras-county",
			Layer: gridv1.Layer_BURN_STATUS,
			Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
				BurnDay:           gridv1.BurnDay_BURN_DAY_NO,
				CalfireStatus:     gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED,
				CalfireObservedAt: tsProto(ts),
				BurnLines: []*gridv1.BurnLineReading{{
					Id: "calaveras-apcd", Phone: "+12097546600",
					BurnDay: gridv1.BurnDay_BURN_DAY_NO,
					Observation: &gridv1.BurnObservation{
						Message: msg, Confidence: conf, ObservedAt: tsProto(ts),
					},
				}},
			}},
		}
	}
	day1 := mk("Today, September 10th, is not a burn day", burnNow, 95)
	day2 := mk("Today, September 11th, is not a burn day", burnNow.Add(24*time.Hour), 91)
	assert.Equal(t, store.ContentHash(day1), store.ContentHash(day2),
		"a new reading of the SAME answer must not mint a revision")

	// A real transition still does.
	changed := mk("Today, September 11th, is a burn day", burnNow.Add(24*time.Hour), 95)
	changed.GetBurnStatus().BurnDay = gridv1.BurnDay_BURN_DAY_YES
	changed.GetBurnStatus().GetBurnLines()[0].BurnDay = gridv1.BurnDay_BURN_DAY_YES
	assert.NotEqual(t, store.ContentHash(day1), store.ContentHash(changed))
}

// The headline must be composed deterministically. ContentHash does NOT zero
// Headline, so a generated/reworded one would differ every tick and mint a
// revision each time.
func TestBurnHeadlineIsDeterministicAndNamesBothFacets(t *testing.T) {
	d := &gridv1.BurnStatusDetail{
		BurnDay:       gridv1.BurnDay_BURN_DAY_NO,
		CalfireStatus: gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED,
	}
	got := burnHeadline(calaveras, d)
	assert.Equal(t, "Calaveras County — no-burn day; CAL FIRE burning suspended", got)
	assert.Equal(t, got, burnHeadline(calaveras, d), "must be stable across calls")

	unknown := burnHeadline(tuolumne, &gridv1.BurnStatusDetail{})
	assert.Equal(t, "Tuolumne County — burn day unknown; CAL FIRE status unknown", unknown)
}

// SourceIDs must not claim the burn line row when no county is dialed, or the
// registry would show a permanently-unattempted source.
func TestBurnSourceIDs(t *testing.T) {
	dialed := newBurnNormalizer(calOnly(), &fakeCalfireBurn{}, noReadings())
	assert.ElementsMatch(t, []string{calfireBurnSourceID, burnLineSourceID}, dialed.SourceIDs())

	undialed := newBurnNormalizer(burnCfg([]config.BurnCounty{tuolumne}, tuolumneLine), &fakeCalfireBurn{}, noReadings())
	assert.Equal(t, []string{calfireBurnSourceID}, undialed.SourceIDs())
}

// --- multiple lines ------------------------------------------------------------

// A county with TWO lines merges them taking the MOST RESTRICTIVE answer, the
// same asymmetry derivePermission applies across facets.
func TestBurnPoll_MultipleLinesPerCountyMergeMostRestrictive(t *testing.T) {
	upper := config.BurnLine{ID: "cal-upper", Name: "Upper Calaveras", Phone: "+12097546601",
		Counties: []string{"calaveras-county"}, Enabled: true}
	lower := config.BurnLine{ID: "cal-lower", Name: "Lower Calaveras", Phone: "+12097546602",
		Counties: []string{"calaveras-county"}, Enabled: true}
	cfg := burnCfg([]config.BurnCounty{calaveras}, upper, lower)

	cases := []struct {
		name string
		a, b string
		want gridv1.BurnDay
	}{
		{"both yes", "green", "green", gridv1.BurnDay_BURN_DAY_YES},
		{"one no wins", "green", "red", gridv1.BurnDay_BURN_DAY_NO},
		{"no wins regardless of order", "red", "green", gridv1.BurnDay_BURN_DAY_NO},
		{"marginal beats yes", "green", "orange", gridv1.BurnDay_BURN_DAY_MARGINAL},
		{"no beats marginal", "orange", "red", gridv1.BurnDay_BURN_DAY_NO},
		{"both marginal", "orange", "orange", gridv1.BurnDay_BURN_DAY_MARGINAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readings := staged(upper.ID, tc.a, burnNow.Add(-2*time.Hour)).
				withReading(lower.ID, tc.b, burnNow.Add(-2*time.Hour))
			n := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: suspendedRows()}, readings)

			res, err := n.Poll(context.Background(), &scriptedPrior{})
			require.NoError(t, err)
			d := byID(res.Events, "burn:calaveras-county").GetBurnStatus()
			assert.Equal(t, tc.want, d.GetBurnDay())
			assert.Len(t, d.GetBurnLines(), 2, "both lines are published")
		})
	}
}

// If one of a county's DIALED lines is unreadable, the merge must not claim YES
// off the other one — "we do not know" never renders as a green light.
func TestBurnPoll_MissingDialedLineBlocksAYes(t *testing.T) {
	upper := config.BurnLine{ID: "cal-upper", Phone: "+1", Counties: []string{"calaveras-county"}, Enabled: true}
	lower := config.BurnLine{ID: "cal-lower", Phone: "+2", Counties: []string{"calaveras-county"}, Enabled: true}
	cfg := burnCfg([]config.BurnCounty{calaveras}, upper, lower)

	// Only the upper line reported, and it says burn day.
	readings := staged(upper.ID, "green", burnNow.Add(-2*time.Hour))
	n := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: suspendedRows()}, readings)

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	require.Error(t, res.PerSource[burnLineSourceID], "the silent line degrades the source")

	d := byID(res.Events, "burn:calaveras-county").GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN, d.GetBurnDay(),
		"a yes from one line cannot stand while a dialed sibling is unread")

	// A NO from the readable line would still be conclusive, though.
	readings2 := staged(upper.ID, "red", burnNow.Add(-2*time.Hour))
	n2 := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: suspendedRows()}, readings2)
	res2, err := n2.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_NO,
		byID(res2.Events, "burn:calaveras-county").GetBurnStatus().GetBurnDay())
}

// A line we merely PUBLISH and never dial must not block a YES — we never asked
// it anything, so its silence proves nothing.
func TestBurnPoll_UndialedLineDoesNotBlockAYes(t *testing.T) {
	dialed := config.BurnLine{ID: "cal-main", Phone: "+1", Counties: []string{"calaveras-county"}, Enabled: true}
	published := config.BurnLine{ID: "cal-extra", Phone: "+2", Counties: []string{"calaveras-county"}, Enabled: false}
	cfg := burnCfg([]config.BurnCounty{calaveras}, dialed, published)

	n := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: permitRows()},
		staged(dialed.ID, "green", burnNow.Add(-2*time.Hour)))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource, "an undialed line is not a failure")

	d := byID(res.Events, "burn:calaveras-county").GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_YES, d.GetBurnDay())
	assert.Equal(t, gridv1.BurnPermission_BURN_PERMISSION_ALLOWED, d.GetPermission())
	assert.Len(t, d.GetBurnLines(), 2, "the undialed line is still published, with its number")
}

// ONE air-district line speaking for SEVERAL counties is read once and attached
// to each — the inverse of multiple lines per county, and the reason a line is
// its own config entity.
func TestBurnPoll_OneLineCanServeSeveralCounties(t *testing.T) {
	shared := config.BurnLine{
		ID: "mother-lode-apcd", Name: "Mother Lode district line", Phone: "+12095550000",
		Counties: []string{"calaveras-county", "tuolumne-county"}, Enabled: true,
	}
	cfg := burnCfg([]config.BurnCounty{calaveras, tuolumne}, shared)

	n := newBurnNormalizer(cfg, &fakeCalfireBurn{rows: permitRows()},
		staged(shared.ID, "green", burnNow.Add(-2*time.Hour)))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)
	require.Len(t, res.Events, 2)

	for _, id := range []string{"burn:calaveras-county", "burn:tuolumne-county"} {
		d := byID(res.Events, id).GetBurnStatus()
		require.Len(t, d.GetBurnLines(), 1, id)
		assert.Equal(t, shared.ID, d.GetBurnLines()[0].GetId(), id)
		assert.Equal(t, gridv1.BurnDay_BURN_DAY_YES, d.GetBurnDay(), id)
	}

	// And it is only ever EXPECTED once, not once per county.
	assert.ElementsMatch(t, []string{calfireBurnSourceID, burnLineSourceID}, n.SourceIDs())
}

// permitRows is the off-season CAL FIRE state (no suspension), so a burn day can
// actually resolve to ALLOWED.
func permitRows() map[string]calfireburn.CountyStatus {
	return map[string]calfireburn.CountyStatus{
		"calaveras": {County: "Calaveras", Status: calfireburn.StatusPermitRequired, Area: "All SRA"},
		"tuolumne":  {County: "Tuolumne", Status: calfireburn.StatusPermitRequired, Area: "All SRA"},
	}
}

// `enabled` governs what we DIAL and what we hold the source accountable for —
// not what we are willing to believe. A fresh reading pushed for a configured
// but undialed line is used; accepting it at the endpoint and then ignoring it
// would be the surprising behaviour.
func TestBurnPoll_FreshReadingForAnUndialedLineIsStillUsed(t *testing.T) {
	n := newBurnNormalizer(stdCfg(), &fakeCalfireBurn{rows: permitRows()},
		staged(calaverasLine.ID, "green", burnNow.Add(-2*time.Hour)).
			withReading(tuolumneLine.ID, "green", burnNow.Add(-2*time.Hour)))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)

	d := byID(res.Events, "burn:tuolumne-county").GetBurnStatus()
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_YES, d.GetBurnDay(),
		"a pushed reading for an undialed line is still believed")
	assert.Equal(t, gridv1.BurnPermission_BURN_PERMISSION_ALLOWED, d.GetPermission())
}

// ...but an undialed line with NO reading must not degrade the source, because
// we never asked it for one.
func TestBurnPoll_UndialedLineWithNoReadingIsNotAFailure(t *testing.T) {
	n := newBurnNormalizer(stdCfg(), &fakeCalfireBurn{rows: permitRows()}, freshLine("green"))

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource, "only DIALED lines are expected to report")
	assert.Equal(t, gridv1.BurnDay_BURN_DAY_UNKNOWN,
		byID(res.Events, "burn:tuolumne-county").GetBurnStatus().GetBurnDay())
}

// --- push-reporter health ------------------------------------------------------

type fakeReporters struct {
	ids    []string
	health []pushingest.ReporterHealth
}

func (f fakeReporters) ReporterIDs(string) []string { return f.ids }
func (f fakeReporters) Health(string) []pushingest.ReporterHealth {
	return f.health
}

// A burn.line reporter gets its own source row, so /api/v1/sources can answer
// "is the line actually being called?" — which the `burnline` row cannot, since
// that one stays healthy right up until the freshness gate trips.
func TestBurnPoll_ReporterHealthIsReported(t *testing.T) {
	reporters := fakeReporters{
		ids: []string{"burn-line"},
		health: []pushingest.ReporterHealth{
			{ID: "burn-line", State: pushingest.ReporterOK},
		},
	}
	n := NewBurnStatusNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()},
		freshLine("red"), fakePlaceIndex{}, reporters)
	n.now = func() time.Time { return burnNow }

	assert.Contains(t, n.SourceIDs(), "burn-line", "the reporter must claim a source row")

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	// Present with a nil error == recorded as a healthy attempt.
	require.Contains(t, res.PerSource, "burn-line")
	assert.NoError(t, res.PerSource["burn-line"])
}

// A reporter that has NEVER reported is an error, not a success: recording a
// clean attempt would paint a monitor that was never set up as a healthy feed.
func TestBurnPoll_SilentReporterIsUnhealthy(t *testing.T) {
	for name, st := range map[string]pushingest.ReporterState{
		"never reported": pushingest.ReporterUnknown,
		"gone stale":     pushingest.ReporterStale,
		"dead":           pushingest.ReporterDead,
	} {
		t.Run(name, func(t *testing.T) {
			reporters := fakeReporters{
				ids:    []string{"burn-line"},
				health: []pushingest.ReporterHealth{{ID: "burn-line", State: st}},
			}
			n := NewBurnStatusNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()},
				freshLine("red"), fakePlaceIndex{}, reporters)
			n.now = func() time.Time { return burnNow }

			res, err := n.Poll(context.Background(), &scriptedPrior{})
			require.NoError(t, err)
			assert.Error(t, res.PerSource["burn-line"])
		})
	}
}

// With push ingest off, no reporter rows are claimed — a source row nothing can
// ever report for would sit permanently UNSPECIFIED on the public endpoint.
func TestBurnPoll_NoReporterRowsWhenPushDisabled(t *testing.T) {
	n := newBurnNormalizer(calOnly(), &fakeCalfireBurn{rows: suspendedRows()}, freshLine("red"))
	assert.ElementsMatch(t, []string{calfireBurnSourceID, burnLineSourceID}, n.SourceIDs())

	res, err := n.Poll(context.Background(), &scriptedPrior{})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)
}
