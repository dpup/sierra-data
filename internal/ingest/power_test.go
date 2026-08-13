package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/pge"
	"github.com/dpup/sierra-data/internal/config"
)

// pgeDoer routes by URL path, because one PG&E client fetches four endpoints
// (outage points, outage polygons, PSPS coverage, the ETL stamp) — the shared
// single-body fakeDoer can't distinguish them. An unrouted path 404s, which is
// what the tests that don't care about the stamp rely on.
type pgeDoer struct {
	points    string
	polygons  string
	psps      string
	stamp     string
	pointsErr error
	pspsErr   error
	urls      []string
}

func (d *pgeDoer) Do(req *http.Request) (*http.Response, error) {
	d.urls = append(d.urls, req.URL.String())
	body := ""
	switch {
	case strings.Contains(req.URL.Path, "/outages/MapServer/4/"):
		if d.pointsErr != nil {
			return nil, d.pointsErr
		}
		body = d.points
	case strings.Contains(req.URL.Path, "/outages/MapServer/8/"):
		body = d.polygons
	case strings.Contains(req.URL.Path, "/psps_public/"):
		if d.pspsErr != nil {
			return nil, d.pspsErr
		}
		body = d.psps
	case strings.Contains(req.URL.Path, "/lastupdate_time/"):
		body = d.stamp
	}
	if body == "" {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// --- fixture builders (the evacuation_test.go convention) ---

func pgeOutageRow(id, cause, crew string, customers int, lat, lng float64) string {
	return fmt.Sprintf(`{"type":"Feature","properties":{
      "OUTAGE_ID":%q,"OUTAGE_CAUSE":%q,"CREW_CURRENT_STATUS":%q,"EST_CUSTOMERS":%d,
      "OUTAGE_START":1786556836000,"LAST_UPDATE":1786570817000,"CURRENT_ETOR":1786561200000},
     "geometry":{"type":"Point","coordinates":[%v,%v]}}`, id, cause, crew, customers, lng, lat)
}

func pgePolygonRow(id string, lng, lat float64) string {
	return fmt.Sprintf(`{"type":"Feature","properties":{
      "OUTAGE_ID":%q,"OUTAGE_CAUSE":"","CREW_CURRENT_STATUS":"","EST_CUSTOMERS":0,
      "OUTAGE_START":1786556836000,"LAST_UPDATE":1786570817000,"CURRENT_ETOR":0},
     "geometry":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
		id, lng, lat, lng+0.01, lat, lng+0.01, lat+0.01, lng, lat)
}

func pspsRow(eventID, timePeriod, stage string, cust, mbl int, deEngStart string, lng, lat float64) string {
	return fmt.Sprintf(`{"type":"Feature","properties":{
      "EventID":%q,"EventName":"PSPS_05172026","TimePeriod":%q,"Stage":%q,
      "TotCustAff":"%d","TotMBLAff":"%d","DeEngStart":%q,"DeEngEnd":"2026-05-19T15:00:00Z",
      "AllClear":"","ETOR":"2026-05-20T16:00:00Z","LstUpdated":"2026-05-16T07:43:13Z"},
     "geometry":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
		eventID, timePeriod, stage, cust, mbl, deEngStart, lng, lat, lng+0.05, lat, lng+0.05, lat+0.05, lng, lat)
}

func collection(rows ...string) string {
	return `{"type":"FeatureCollection","features":[` + strings.Join(rows, ",") + `]}`
}

func emptyCollection() string { return collection() }

// stampAt renders the ETL stamp table's zone-less "2006-01-02 15:04:05" body.
func stampAt(t time.Time) string {
	return fmt.Sprintf(`{"features":[{"attributes":{"OBJECTID":1,"LAST_UPDATE":%q}}]}`,
		t.UTC().Format("2006-01-02 15:04:05"))
}

// newPowerNormalizer wires a normalizer over a routing doer with a frozen
// clock, so the PSPS scheduled/active split and the freshness gate are
// deterministic. Mirrors newWildfireNormalizer's multi-endpoint shape.
func newPowerNormalizer(d *pgeDoer, now time.Time) *PowerNormalizer {
	n := NewPowerNormalizer(testConfig(), pge.NewClientWithHTTPDoer("https://pge.test/43", d))
	n.now = func() time.Time { return now }
	return n
}

var testNow = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

func freshDoer() *pgeDoer {
	return &pgeDoer{
		points:   emptyCollection(),
		polygons: emptyCollection(),
		psps:     emptyCollection(),
		stamp:    stampAt(testNow.Add(-3 * time.Minute)),
	}
}

func TestPowerSourceIDs(t *testing.T) {
	n := newPowerNormalizer(freshDoer(), testNow)
	// Two rows, one poller: the outage and PSPS services fail independently, so
	// conflating their health would let one outage hide the other feed's state.
	assert.Equal(t, []string{"pge", "psps"}, n.SourceIDs())
}

func TestPowerPoll(t *testing.T) {
	d := freshDoer()
	d.points = collection(
		pgeOutageRow("330042", "PLNND SHUTDOWN", "Awaiting T-Man", 1, 38.43, -120.07),
		pgeOutageRow("330217", "TREE CONTACT", "Crew Enroute", 180, 38.2, -120.3),
	)
	d.polygons = collection(pgePolygonRow("330217", -120.3, 38.2))
	d.psps = collection(pspsRow("20725", "TP02_05172026", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.5, 38.1))

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource, "a healthy poll of both feeds records no per-source failure")
	require.Len(t, res.Events, 3)

	// --- unplanned outage ---
	out := eventByID(t, res.Events, "pge:330217")
	assert.Equal(t, gridv1.Layer_POWER, out.GetLayer())
	assert.Equal(t, "unplanned", out.GetCategory())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, out.GetStatus())
	// 180 customers -> MODERATE (>=100), unplanned so no demotion.
	assert.Equal(t, gridv1.Severity_MODERATE, out.GetSeverity())
	assert.Equal(t, "Power outage — 180 customers (tree contact)", out.GetHeadline())
	assert.Equal(t, "pge", out.GetProvenance().GetSourceId())
	assert.Equal(t, "PG&E", out.GetProvenance().GetSourceName())
	assert.Equal(t, pge.OutageMapURL, out.GetCanonicalUrl())
	d1 := out.GetPower()
	assert.Equal(t, "330217", d1.GetOutageId())
	assert.Equal(t, "TREE CONTACT", d1.GetCause(), "the raw upstream code is preserved alongside the humanized headline")
	assert.Equal(t, int32(180), d1.GetCustomersAffected())
	assert.Equal(t, "Crew Enroute", d1.GetCrewStatus())
	// ETOR is an estimate, so it lives in the detail and never becomes `expires`
	// (which is what shouldExpire retires an event on).
	assert.NotNil(t, d1.GetEstimatedRestoration())
	assert.Nil(t, out.GetExpires())
	// The polygon layer's affected area wins over the reported point.
	assert.Contains(t, string(out.GetGeometry().GetGeojson()), "Polygon")

	// --- planned outage ---
	planned := eventByID(t, res.Events, "pge:330042")
	assert.Equal(t, "planned", planned.GetCategory())
	// 1 customer is INFO before the planned demotion, and INFO is the floor.
	assert.Equal(t, gridv1.Severity_INFO, planned.GetSeverity())
	// The cause of a planned outage is "it was planned" — the prefix already says so.
	assert.Equal(t, "Planned outage — 1 customer", planned.GetHeadline())
	assert.Contains(t, string(planned.GetGeometry().GetGeojson()), "Point")

	// --- PSPS ---
	psps := eventByID(t, res.Events, "psps:20725:TP02_05172026")
	assert.Equal(t, gridv1.Layer_POWER, psps.GetLayer())
	assert.Equal(t, "psps", psps.GetCategory())
	assert.Equal(t, gridv1.Severity_SEVERE, psps.GetSeverity(), "a Warning is a committed de-energization")
	// De-energization began at 09:00, the clock is 12:00.
	assert.Equal(t, gridv1.EventStatus_ACTIVE, psps.GetStatus())
	assert.Equal(t, "PSPS Warning — 74,786 customers, 5,623 medical baseline", psps.GetHeadline())
	assert.Equal(t, "psps", psps.GetProvenance().GetSourceId())
	d2 := psps.GetPower()
	assert.Equal(t, "20725", d2.GetEventId())
	assert.Equal(t, "Warning", d2.GetStage())
	// Stringified upstream counts must survive as numbers.
	assert.Equal(t, int32(74786), d2.GetCustomersAffected())
	assert.Equal(t, int32(5623), d2.GetMedicalBaselineAffected())
	assert.NotNil(t, d2.GetDeEnergizationEnd())
	assert.Nil(t, psps.GetExpires(), "the planned end is an estimate, not an expiry")

	// Spatial scoping is the union bbox of the configured hazard areas. (The
	// ETL-stamp table is a single row with no geometry, so it is exempt.)
	var spatial int
	for _, u := range d.urls {
		if strings.Contains(u, "/lastupdate_time/") {
			continue
		}
		spatial++
		assert.Contains(t, u, "esriGeometryEnvelope", u)
		assert.Contains(t, u, "37.7", "the union bbox of the configured hazard areas")
	}
	assert.Equal(t, 3, spatial, "outage points + outage polygons + PSPS coverage")
}

// TestPowerPoll_PSPSGroupsByWindow: PG&E publishes one de-energization window
// as MANY polygon rows sharing every attribute (12 rows for one real window).
// Emitting one event per row would draw the same shutoff a dozen times and give
// it a dozen ids for the sweep to track.
func TestPowerPoll_PSPSGroupsByWindow(t *testing.T) {
	d := freshDoer()
	d.psps = collection(
		pspsRow("20725", "TP02_05172026", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.5, 38.1),
		pspsRow("20725", "TP02_05172026", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.4, 38.2),
		pspsRow("20725", "TP02_05172026", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.3, 38.3),
		// A second window of the same event is a separate de-energization with
		// its own footprint and times, so it stays a separate event.
		pspsRow("20725", "TP03_05172026", "Watch", 1200, 40, "2026-05-19T09:00:00Z", -120.2, 38.4),
	)

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"psps:20725:TP02_05172026", "psps:20725:TP03_05172026"}, eventIDs(res.Events))

	// The three rows of one window merge into a single MultiPolygon.
	merged := eventByID(t, res.Events, "psps:20725:TP02_05172026")
	var g struct {
		Type        string            `json:"type"`
		Coordinates []json.RawMessage `json:"coordinates"`
	}
	require.NoError(t, json.Unmarshal(merged.GetGeometry().GetGeojson(), &g))
	assert.Equal(t, "MultiPolygon", g.Type)
	assert.Len(t, g.Coordinates, 3)
}

// TestPowerPoll_PSPSScheduledIsNotActive: grid.proto names SCHEDULED as the
// planned-PSPS status. It matters beyond labelling — SCHEDULED events never
// escalate a place's summary mode, so a shutoff announced for Tuesday must not
// read as one happening now.
func TestPowerPoll_PSPSScheduledIsNotActive(t *testing.T) {
	d := freshDoer()
	d.psps = collection(
		pspsRow("20725", "TP09", "Watch", 500, 10, "2026-05-19T09:00:00Z", -120.5, 38.1),   // future
		pspsRow("20725", "TP01", "Warning", 500, 10, "2026-05-17T09:00:00Z", -120.4, 38.2), // started
		pspsRow("20725", "TP00", "Warning", 500, 10, "", -120.3, 38.3),                     // no start time
	)
	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)

	assert.Equal(t, gridv1.EventStatus_SCHEDULED, eventByID(t, res.Events, "psps:20725:TP09").GetStatus())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventByID(t, res.Events, "psps:20725:TP01").GetStatus())
	// A listed shutoff we cannot place in the future is happening as far as we
	// can tell; the layer is active-only, so ACTIVE is the honest default.
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventByID(t, res.Events, "psps:20725:TP00").GetStatus())
	// Watch is real enough to prepare for, not yet committed.
	assert.Equal(t, gridv1.Severity_MODERATE, eventByID(t, res.Events, "psps:20725:TP09").GetSeverity())
}

// TestPowerPoll_OneFeedFailingKeepsTheOther: a partial failure must not fail
// the tick. The failed source's sweep is skipped (its absence proves nothing)
// while the healthy sibling's events still land.
func TestPowerPoll_OneFeedFailingKeepsTheOther(t *testing.T) {
	t.Run("outages down", func(t *testing.T) {
		d := freshDoer()
		d.pointsErr = assert.AnError
		d.psps = collection(pspsRow("20725", "TP02", "Warning", 100, 2, "2026-05-17T09:00:00Z", -120.5, 38.1))

		res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
		require.NoError(t, err, "one feed failing is a per-source failure, not a hard error")
		assert.Error(t, res.PerSource["pge"])
		assert.NoError(t, res.PerSource["psps"])
		assert.Equal(t, []string{"psps:20725:TP02"}, eventIDs(res.Events))
	})

	t.Run("psps down", func(t *testing.T) {
		d := freshDoer()
		d.pspsErr = assert.AnError
		d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 19, 38.43, -120.07))

		res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
		require.NoError(t, err)
		assert.Error(t, res.PerSource["psps"])
		assert.NoError(t, res.PerSource["pge"])
		assert.Equal(t, []string{"pge:330042"}, eventIDs(res.Events))
	})
}

// TestPowerPoll_BothFeedsFailingIsHardError: with no usable view of either
// feed, a success-empty result would let the sweep RESOLVE every stored outage
// and shutoff — publishing "the power is back on everywhere" from an outage of
// our own.
func TestPowerPoll_BothFeedsFailingIsHardError(t *testing.T) {
	d := freshDoer()
	d.pointsErr = assert.AnError
	d.pspsErr = assert.AnError

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.Error(t, err)
	assert.Nil(t, res)
}

// TestPowerPoll_FrozenFeedIsRecordedAsFailure: PG&E's ETL can stall while the
// layers keep serving the last set, so a restored outage stays listed and a new
// one never appears. The fetch alone cannot see this — only the upstream stamp
// can. (The Cal OES mirror of this same data was measured 26 h stale while
// reporting every row as Active.)
func TestPowerPoll_FrozenFeedIsRecordedAsFailure(t *testing.T) {
	d := freshDoer()
	d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 19, 38.43, -120.07))
	d.stamp = stampAt(testNow.Add(-26 * time.Hour))

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Error(t, res.PerSource["pge"], "a frozen feed must degrade the source")
	assert.Contains(t, res.PerSource["pge"].Error(), "frozen")
	// The events still upsert: last-known data is the best available, and the
	// source status is what tells the reader not to trust its freshness.
	assert.Equal(t, []string{"pge:330042"}, eventIDs(res.Events))
	// PSPS is a separate service and is unaffected — its own stamp legitimately
	// sits idle for weeks between events, so the gate must not touch it.
	assert.NoError(t, res.PerSource["psps"])
}

func TestPowerPoll_FreshFeedIsHealthy(t *testing.T) {
	d := freshDoer()
	d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 19, 38.43, -120.07))
	d.stamp = stampAt(testNow.Add(-3 * time.Minute)) // the observed live lag

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
}

// TestPowerPoll_UnreadableStampFailsOpen: the gate is an EXTRA signal layered
// on an already-successful fetch. Losing it puts us exactly where every other
// source in this repo sits (none publish an ETL stamp), so a flaky metadata
// table must not flap the layer.
func TestPowerPoll_UnreadableStampFailsOpen(t *testing.T) {
	d := freshDoer()
	d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 19, 38.43, -120.07))
	d.stamp = "" // 404s

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	assert.Equal(t, []string{"pge:330042"}, eventIDs(res.Events))
}

func TestPowerPoll_FreshnessGateDisabled(t *testing.T) {
	d := freshDoer()
	d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 19, 38.43, -120.07))
	d.stamp = stampAt(testNow.Add(-100 * time.Hour))

	cfg := testConfig()
	cfg.Grid.Power.OutageStaleAfter = -1 // explicit opt-out
	n := NewPowerNormalizer(cfg, pge.NewClientWithHTTPDoer("https://pge.test/43", d))
	n.now = func() time.Time { return testNow }

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource, "a negative value disables the gate outright")
}

// TestPowerSeverity: the thresholds exist because the statewide MEDIAN PG&E
// outage affects ONE customer. If the bottom of the scale did not absorb those,
// the layer would bury a real event under service calls.
func TestPowerSeverity(t *testing.T) {
	cases := []struct {
		name      string
		cause     string
		customers int
		want      gridv1.Severity
	}{
		{"single premise", "TREE CONTACT", 1, gridv1.Severity_INFO},
		{"small cluster", "TREE CONTACT", 9, gridv1.Severity_INFO},
		{"neighbourhood", "TREE CONTACT", 10, gridv1.Severity_MINOR},
		{"town block", "TREE CONTACT", 100, gridv1.Severity_MODERATE},
		{"community", "TREE CONTACT", 1000, gridv1.Severity_SEVERE},
		// A pre-notified shutdown is materially less urgent than the same
		// unplanned outage — demoted one rank, never below INFO.
		{"planned community", "PLNND SHUTDOWN", 1000, gridv1.Severity_MODERATE},
		{"planned block", "PLNND SHUTDOWN", 100, gridv1.Severity_MINOR},
		{"planned single", "PLNND SHUTDOWN", 1, gridv1.Severity_INFO},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := freshDoer()
			d.points = collection(pgeOutageRow("1", tc.cause, "Awaiting Crew", tc.customers, 38.2, -120.3))
			res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
			require.NoError(t, err)
			require.Len(t, res.Events, 1)
			assert.Equal(t, tc.want, res.Events[0].GetSeverity())
		})
	}
}

// TestPowerPoll_UnrecognizedPSPSStageIsSevere: this layer is active-only, so
// any row present is a live shutoff. Under-rating one PG&E is publishing is the
// failure that matters, so an unknown stage classifies conservatively (the same
// bias as the evacuation WARNING default).
func TestPowerPoll_UnrecognizedPSPSStageIsSevere(t *testing.T) {
	d := freshDoer()
	d.psps = collection(pspsRow("20725", "TP02", "Imminent De-energization", 500, 10, "2026-05-17T09:00:00Z", -120.5, 38.1))

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, gridv1.Severity_SEVERE, res.Events[0].GetSeverity())
}

// TestPowerPoll_BadGeometryIsNotDropped: both power sources use the `resolve`
// policy, so an event missing from a successful poll transitions to RESOLVED.
// Dropping a row over unusable geometry would therefore publish "power
// restored" for an outage PG&E is still reporting.
func TestPowerPoll_BadGeometryIsNotDropped(t *testing.T) {
	d := freshDoer()
	d.points = `{"type":"FeatureCollection","features":[
      {"type":"Feature","properties":{"OUTAGE_ID":"330042","OUTAGE_CAUSE":"FIRE",
       "CREW_CURRENT_STATUS":"No Access","EST_CUSTOMERS":19,"OUTAGE_START":1786556836000,
       "LAST_UPDATE":1786570817000,"CURRENT_ETOR":0},
       "geometry":{"type":"Nonsense","coordinates":[1,2]}}]}`

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Nil(t, ev.GetGeometry())
	// A geometry-less event gets no geometric place matches, which would make it
	// invisible to every place-scoped read. The poll is already bbox-scoped to
	// the configured areas, so attach it to them.
	assert.ElementsMatch(t, []string{"area:calaveras", "area:tuolumne"}, ev.GetPlaceIds())
}

func TestPowerHeadlines(t *testing.T) {
	cases := []struct {
		name      string
		cause     string
		customers int
		want      string
	}{
		{"known cause humanized", "BRKN UG EQUIPMNT", 42, "Power outage — 42 customers (broken underground equipment)"},
		// An unknown code is still information: surface it rather than dropping it.
		{"unknown cause passes through", "WEIRD NEW CODE", 42, "Power outage — 42 customers (weird new code)"},
		// PG&E leaves the cause null on roughly half of all rows.
		{"no cause", "", 42, "Power outage — 42 customers"},
		{"singular", "", 1, "Power outage — 1 customer"},
		// Distinguish "not reported" from a real zero rather than printing "0 customers".
		{"count missing", "", 0, "Power outage — customer count not reported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := freshDoer()
			d.points = collection(pgeOutageRow("1", tc.cause, "", tc.customers, 38.2, -120.3))
			res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
			require.NoError(t, err)
			require.Len(t, res.Events, 1)
			assert.Equal(t, tc.want, res.Events[0].GetHeadline())
		})
	}
}

// TestPowerPoll_EmptyScopeIsError mirrors TestPollEmptyScopeIsError for the
// power poller specifically, pinning that the scope check runs BEFORE any HTTP
// request.
func TestPowerPoll_EmptyScopeIsError(t *testing.T) {
	d := freshDoer()
	n := NewPowerNormalizer(&config.Config{}, pge.NewClientWithHTTPDoer("https://pge.test/43", d))

	res, err := n.Poll(testCtx(), nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Empty(t, d.urls, "an empty scope must not fetch anything")
}

// pspsRowNoTimePeriod is a coverage row missing PG&E's TimePeriod identifier —
// the degenerate case the group-key fallbacks exist for.
func pspsRowNoTimePeriod(eventID, stage, deEngStart, deEngEnd string, lng, lat float64) string {
	return fmt.Sprintf(`{"type":"Feature","properties":{
      "EventID":%q,"EventName":"PSPS_05172026","TimePeriod":"","Stage":%q,
      "TotCustAff":"500","TotMBLAff":"10","DeEngStart":%q,"DeEngEnd":%q,
      "AllClear":"","ETOR":"","LstUpdated":"2026-05-16T07:43:13Z"},
     "geometry":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
		eventID, stage, deEngStart, deEngEnd, lng, lat, lng+0.05, lat, lng+0.05, lat+0.05, lng, lat)
}

// TestPowerPoll_PSPSIDSurvivesStageEscalation is the single most important
// property of this layer's identity.
//
// Watch -> Warning is the escalation this feed exists to report. `psps` uses the
// `resolve` policy, so if that escalation CHANGED the event id, the old id would
// be missing from a successful poll and the sweep would RESOLVE it — publishing
// "shutoff cancelled" at the exact moment PG&E committed to cutting power, and
// restarting the history under a new id. The key must therefore be built only
// from immutable fields; Stage is not one.
func TestPowerPoll_PSPSIDSurvivesStageEscalation(t *testing.T) {
	poll := func(stage string) *gridv1.Event {
		d := freshDoer()
		d.psps = collection(pspsRowNoTimePeriod("20725", stage, "2026-05-19T09:00:00Z", "2026-05-20T09:00:00Z", -120.5, 38.1))
		res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		return res.Events[0]
	}

	watch, warning := poll("Watch"), poll("Warning")
	assert.Equal(t, watch.GetId(), warning.GetId(),
		"a Watch->Warning escalation must not change the id — the sweep would read the old id as cancelled")
	// The escalation itself must still be visible, as severity and detail.
	assert.Equal(t, gridv1.Severity_MODERATE, watch.GetSeverity())
	assert.Equal(t, gridv1.Severity_SEVERE, warning.GetSeverity())
	assert.Equal(t, "Warning", warning.GetPower().GetStage())
}

// TestPowerPoll_PSPSIDSurvivesEndTimeRevision: PG&E revises the planned end as a
// shutoff runs long. That is an ESTIMATE changing, not a different shutoff, so
// it must not move the id (which would resolve the live event).
func TestPowerPoll_PSPSIDSurvivesEndTimeRevision(t *testing.T) {
	poll := func(end string) string {
		d := freshDoer()
		d.psps = collection(pspsRowNoTimePeriod("20725", "Warning", "2026-05-19T09:00:00Z", end, -120.5, 38.1))
		res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		return res.Events[0].GetId()
	}
	assert.Equal(t, poll("2026-05-20T09:00:00Z"), poll("2026-05-20T21:00:00Z"),
		"revising the planned end must not mint a new event id")
}

// TestPowerPoll_PSPSCollapsedGroupTakesWorstStage: on the TimePeriod-less
// fallback, rows from different windows share one id. Summarizing that group as
// the LESS urgent of its members would under-report a committed de-energization,
// so the representative carries the worst stage and the earliest start.
func TestPowerPoll_PSPSCollapsedGroupTakesWorstStage(t *testing.T) {
	d := freshDoer()
	d.psps = collection(
		pspsRowNoTimePeriod("20725", "Watch", "2026-05-22T09:00:00Z", "2026-05-23T09:00:00Z", -120.2, 38.4),
		pspsRowNoTimePeriod("20725", "Warning", "2026-05-19T09:00:00Z", "2026-05-20T09:00:00Z", -120.5, 38.1),
	)
	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "with no TimePeriod, one id per event — stable beats granular")

	ev := res.Events[0]
	assert.Equal(t, gridv1.Severity_SEVERE, ev.GetSeverity(), "a group holding a Warning must not report as a Watch")
	assert.Equal(t, "Warning", ev.GetPower().GetStage())
	// Earliest start: the shutoff begins when its first window does.
	assert.Equal(t, "2026-05-19T09:00:00Z", ev.GetEffective().AsTime().UTC().Format(time.RFC3339))
	// Both footprints are still drawn.
	var g struct {
		Type        string            `json:"type"`
		Coordinates []json.RawMessage `json:"coordinates"`
	}
	require.NoError(t, json.Unmarshal(ev.GetGeometry().GetGeojson(), &g))
	assert.Equal(t, "MultiPolygon", g.Type)
	assert.Len(t, g.Coordinates, 2)
}

// TestPowerPoll_GeometryStableAcrossFeedOrder: ArcGIS promises no row ordering,
// and geometry IS part of the store's content hash. If a reordered response
// produced different geometry bytes, an outage that never changed would mint a
// revision on every reorder.
func TestPowerPoll_GeometryStableAcrossFeedOrder(t *testing.T) {
	a := pgePolygonRow("330217", -120.3, 38.2)
	b := pgePolygonRow("330217", -120.6, 38.5)

	geomFor := func(polygons string) string {
		d := freshDoer()
		d.points = collection(pgeOutageRow("330217", "TREE CONTACT", "Crew Enroute", 180, 38.2, -120.3))
		d.polygons = polygons
		res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		return string(res.Events[0].GetGeometry().GetGeojson())
	}
	assert.Equal(t, geomFor(collection(a, b)), geomFor(collection(b, a)),
		"upstream row order must not change the stored geometry bytes")
}

// TestPowerHeadlineSeparatesThousands: these headlines render in the events
// detail pane, whose screenshot contract (web/screenshots/events-contract.mjs
// check 9) fails the build on unseparated 4+ digit numbers — and PSPS customer
// counts are the largest numbers this service emits.
func TestPowerHeadlineSeparatesThousands(t *testing.T) {
	bare := regexp.MustCompile(`(?:^|[^\d,])\d{4,}`)

	d := freshDoer()
	d.points = collection(pgeOutageRow("1", "TREE CONTACT", "", 43210, 38.2, -120.3))
	d.psps = collection(pspsRow("20725", "TP02", "Warning", 74786, 5623, "2026-05-17T09:00:00Z", -120.5, 38.1))
	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 2)

	for _, ev := range res.Events {
		assert.NotRegexp(t, bare, ev.GetHeadline(), "unseparated 4+ digit number in %q", ev.GetHeadline())
	}
	assert.Equal(t, "Power outage — 43,210 customers (tree contact)",
		eventByID(t, res.Events, "pge:1").GetHeadline())
}

// TestPSPSRepresentativeIsSafeForExternalCallers: the helper is exported so
// cmd/test-pge can report the same stage the poller stores. An internal-only
// helper could assume a non-empty group and reorder its argument; an exported
// one cannot — a nil slice from a future caller would panic inside the ingest
// goroutine, and reordering the caller's slice is a surprise side effect.
func TestPSPSRepresentativeIsSafeForExternalCallers(t *testing.T) {
	assert.NotPanics(t, func() { PSPSRepresentative(nil) })
	assert.NotPanics(t, func() { PSPSRepresentative([]pge.PSPSArea{}) })
	assert.Equal(t, pge.PSPSArea{}, PSPSRepresentative(nil))

	// The caller's slice keeps its order.
	group := []pge.PSPSArea{
		{Stage: "Watch", DeEnergizationStart: testNow.Add(48 * time.Hour), GeometryCoords: []byte(`[2]`)},
		{Stage: "Warning", DeEnergizationStart: testNow, GeometryCoords: []byte(`[1]`)},
	}
	rep := PSPSRepresentative(group)
	assert.Equal(t, "Watch", group[0].Stage, "the caller's slice must not be reordered in place")
	// Earliest window, worst stage.
	assert.Equal(t, testNow, rep.DeEnergizationStart)
	assert.Equal(t, "Warning", rep.Stage)
}

// TestPowerPoll_UnknownCustomerCountIsNotSuppressed: EST_CUSTOMERS null
// unmarshals to 0 with no error. Since the place summary drops INFO-severity
// power events from its rollup, rating an unsized outage INFO would make a
// possibly community-scale one vanish from the summary while its own headline
// says the count is unknown.
func TestPowerPoll_UnknownCustomerCountIsNotSuppressed(t *testing.T) {
	d := freshDoer()
	d.points = collection(pgeOutageRow("330042", "FIRE", "No Access", 0, 38.2, -120.3))

	res, err := newPowerNormalizer(d, testNow).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, gridv1.Severity_MINOR, ev.GetSeverity(),
		"an unknown count is not evidence of a small outage")
	// The headline still says plainly that we don't know.
	assert.Equal(t, "Power outage — customer count not reported (fire)", ev.GetHeadline())
}
