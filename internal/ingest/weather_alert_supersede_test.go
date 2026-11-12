package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The live case of 2026-11-11, captured from api.weather.gov: the CAZ139
// Winter Storm Watch issued 11-10 (an Alert) was re-issued 11-11 12:03 PST as
// an Update that references it. The old one now carries replacedBy and is
// gone from /alerts/active; the active list holds only the Update.
const (
	wswOldID = "urn:oid:2.49.0.1.840.0.d42764c572d5df8b3ae4c07fd1308e13a2f725b2.001.1"
	wswNewID = "urn:oid:2.49.0.1.840.0.f6c56adff45810747c6f90ae2eed1598b8c4a58b.001.1"
	// The Cancel fixture (a Severe Thunderstorm Warning, Gibson County IN)
	// references two earlier messages.
	svrCancelID = "urn:oid:2.49.0.1.840.0.a9a451444669c0d7d768485ef6e3dbafab15969c.001.1"
	svrRefA     = "urn:oid:2.49.0.1.840.0.13322e6258d0a69330fae37f731d7e542dc5976c.001.1"
	svrRefB     = "urn:oid:2.49.0.1.840.0.ab00c7e553dc4d24c60c7c33fc18ffb037202175.001.1"
)

// The deploy tick: after the Update (11-11 20:03Z), before the watch begins
// (11-13 18:00Z), so both copies are SCHEDULED, as in production.
var wswDeployTime = time.Date(2026, 11, 11, 23, 30, 0, 0, time.UTC)

func nwsFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "nws", name))
	require.NoError(t, err)
	return b
}

// nwsAlertFixture parses a saved /alerts/{id} response through the real client.
func nwsAlertFixture(t *testing.T, name string) nws.Alert {
	t.Helper()
	c := nws.NewClientWithHTTPDoer("test", "https://nws.test", &fakeDoer{resp: string(nwsFixture(t, name))})
	a, err := c.GetAlert(testCtx(), "fixture")
	require.NoError(t, err)
	return a
}

// nwsActiveFixture parses a saved /alerts/active response through the real client.
func nwsActiveFixture(t *testing.T, name string) []nws.Alert {
	t.Helper()
	c := nws.NewClientWithHTTPDoer("test", "https://nws.test", &fakeDoer{resp: string(nwsFixture(t, name))})
	alerts, err := c.GetActiveZoneAlerts(testCtx(), []string{"CAZ137", "CAZ138", "CAZ139"})
	require.NoError(t, err)
	return alerts
}

func wswConfig() *config.Config {
	cfg := testConfig()
	cfg.Weather.NWS.Zones = []string{"CAZ137", "CAZ138", "CAZ139"}
	cfg.Hazards.Areas = []config.HazardArea{{ID: "high-country", Zones: []string{"CAZ139"}}}
	cfg.Grid.Sources = map[string]config.SourceTuning{
		"nws": {Disappearance: store.DisappearanceExpire, ExpireAfter: 24 * time.Hour},
	}
	return cfg
}

func newWSWNormalizer(api *fakeWeatherAPI) *WeatherAlertNormalizer {
	n := NewWeatherAlertNormalizer(wswConfig(), api)
	n.now = func() time.Time { return wswDeployTime }
	return n
}

// heldEvent is a stored ACTIVE/SCHEDULED nws event as the Prior hands it over.
func heldEvent(t *testing.T, a nws.Alert) *gridv1.Event {
	t.Helper()
	res, err := newWSWNormalizer(&fakeWeatherAPI{alerts: []nws.Alert{a}}).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	return res.Events[0]
}

func TestNWSFixturesCarryReplacementFields(t *testing.T) {
	old := nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json")
	assert.Equal(t, wswOldID, old.ID)
	assert.Equal(t, "Alert", old.MessageType)
	assert.Empty(t, old.References)
	assert.Equal(t, wswNewID, old.ReplacedBy, "replacedBy is a URL on the wire; the client keeps the id")

	upd := nwsAlertFixture(t, "alert_wsw_caz139_20261111_update.json")
	assert.Equal(t, "Update", upd.MessageType)
	assert.Equal(t, []string{wswOldID}, upd.References)
	assert.Empty(t, upd.ReplacedBy, "null replacedBy")

	active := nwsActiveFixture(t, "active_caz137-139_20261111.json")
	require.Len(t, active, 1)
	assert.Equal(t, wswNewID, active[0].ID)
	assert.Equal(t, []string{wswOldID}, active[0].References)

	cancel := nwsAlertFixture(t, "alert_svr_20261111_cancel.json")
	assert.Equal(t, "Cancel", cancel.MessageType)
	assert.Equal(t, []string{svrRefA, svrRefB}, cancel.References)
}

// (a) Production: the old watch is held SCHEDULED and absent from the feed;
// the Update is in the feed (and, as in production, already stored too).
func TestWeatherAlertPoll_UpdateRetiresHeldPredecessor(t *testing.T) {
	old := heldEvent(t, nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json"))
	require.Equal(t, gridv1.EventStatus_SCHEDULED, old.Status)
	feed := nwsActiveFixture(t, "active_caz137-139_20261111.json")
	stored := heldEvent(t, feed[0])

	api := &fakeWeatherAPI{alerts: feed}
	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{old, stored}})
	require.NoError(t, err)

	assert.Equal(t, []string{"wx:" + wswOldID}, res.Superseded)
	assert.Equal(t, map[string]string{"wx:" + wswOldID: "wx:" + wswNewID}, res.ReplacedBy, "the retired alert points at its replacement")
	require.Len(t, res.Events, 1)
	assert.Equal(t, "wx:"+wswNewID, res.Events[0].Id, "the replacement keeps its own id and continues")
	assert.Empty(t, res.Events[0].GetWeatherAlert().GetReplacedByEventId(), "the replacement itself points at nothing")
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, res.Events[0].Status)
	assert.Empty(t, api.lookups, "a reference in the feed is evidence enough; no lookup")
}

// A referenced alert that is still listed (NWS normally drops it at once) is
// not emitted, or it would be written back to ACTIVE after being retired.
func TestWeatherAlertPoll_ReferencedAlertStillListedIsNotEmitted(t *testing.T) {
	oldAlert := nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json")
	old := heldEvent(t, oldAlert)
	feed := append(nwsActiveFixture(t, "active_caz137-139_20261111.json"), oldAlert)

	res, err := newWSWNormalizer(&fakeWeatherAPI{alerts: feed}).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{old}})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "wx:"+wswNewID, res.Events[0].Id)
	assert.Equal(t, []string{"wx:" + wswOldID}, res.Superseded)
}

// (b) A Cancel ends what it references, and is not a hazard of its own.
func TestWeatherAlertPoll_CancelRetiresHeldAlert(t *testing.T) {
	cancel := nwsAlertFixture(t, "alert_svr_20261111_cancel.json")
	held := heldEvent(t, nws.Alert{ID: svrRefB, Event: "Severe Thunderstorm Warning", Severity: "Severe"})
	other := heldEvent(t, nws.Alert{ID: "urn:oid:2.49.0.1.840.0.unrelated", Event: "Wind Advisory"})
	api := &fakeWeatherAPI{alerts: []nws.Alert{cancel, {ID: "urn:oid:2.49.0.1.840.0.unrelated", Event: "Wind Advisory"}}}

	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{held, other}})
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:" + svrRefB}, res.Superseded, "only the held, referenced alert; svrRefA is not held")
	assert.Empty(t, res.ReplacedBy, "a Cancel is not an event, so there is nothing to point at")
	require.Len(t, res.Events, 1, "the Cancel message itself is not emitted")
	assert.Equal(t, "wx:urn:oid:2.49.0.1.840.0.unrelated", res.Events[0].Id)
}

// /alerts/active never lists Cancel messages, so a cancelled alert simply
// drops out. The held alert is looked up, and NWS's replacedBy retires it.
// The same path retires the middle of a chain whose Update we never saw.
func TestWeatherAlertPoll_AbsentHeldAlertReplacedByLookup(t *testing.T) {
	held := heldEvent(t, nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json"))
	lapsed := heldEvent(t, nws.Alert{ID: "urn:oid:2.49.0.1.840.0.lapsed", Event: "Frost Advisory"})
	api := &fakeWeatherAPI{replacedBy: map[string]string{wswOldID: wswNewID}}
	n := newWSWNormalizer(api)
	prior := &fakePrior{events: []*gridv1.Event{held, lapsed}}

	// An empty feed is a valid answer (no zone has an alert).
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Equal(t, []string{"wx:" + wswOldID}, res.Superseded)
	assert.Empty(t, res.ReplacedBy, "the replacement is not in this poll, so there is no event to name")
	assert.ElementsMatch(t, []string{wswOldID, "urn:oid:2.49.0.1.840.0.lapsed"}, api.lookups)

	// The lapsed alert NWS did not replace keeps its grace and is not asked
	// about again within the hour.
	api.lookups = nil
	res, err = n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{lapsed}})
	require.NoError(t, err)
	assert.Empty(t, res.Superseded)
	assert.Empty(t, api.lookups)

	n.now = func() time.Time { return wswDeployTime.Add(replacedLookupTTL + time.Minute) }
	_, err = n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{lapsed}})
	require.NoError(t, err)
	assert.Equal(t, []string{"urn:oid:2.49.0.1.840.0.lapsed"}, api.lookups)
}

// A lookup that fails is no evidence: nothing is retired.
func TestWeatherAlertPoll_LookupFailureRetiresNothing(t *testing.T) {
	held := heldEvent(t, nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json"))
	api := &fakeWeatherAPI{replacedErr: assert.AnError, replacedBy: map[string]string{wswOldID: wswNewID}}
	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{held}})
	require.NoError(t, err)
	assert.Empty(t, res.Superseded)
}

// A stale-served list proves nothing: no references acted on, no lookups.
func TestWeatherAlertPoll_StaleServeSupersedesNothing(t *testing.T) {
	old := heldEvent(t, nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json"))
	api := &fakeWeatherAPI{alerts: nwsActiveFixture(t, "active_caz137-139_20261111.json"), err: assert.AnError}
	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{old}})
	require.NoError(t, err)
	assert.NotNil(t, res.PerSource["nws"])
	assert.Empty(t, res.Superseded)
	assert.Empty(t, api.lookups)
}

// (c) A reference to an alert the Grid does not hold is ignored.
func TestWeatherAlertPoll_ReferenceToUnknownIDIgnored(t *testing.T) {
	feed := nwsActiveFixture(t, "active_caz137-139_20261111.json")
	stored := heldEvent(t, feed[0])
	api := &fakeWeatherAPI{alerts: feed}
	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{stored}})
	require.NoError(t, err)
	assert.Empty(t, res.Superseded)
	require.Len(t, res.Events, 1)
	assert.Empty(t, api.lookups, "the held alert is still listed")
}

// (d) A fresh alert with no references is emitted as before and retires nothing.
func TestWeatherAlertPoll_FreshAlertUnchanged(t *testing.T) {
	old := nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json")
	old.ReplacedBy = "" // as /alerts/active listed it on 11-10
	api := &fakeWeatherAPI{alerts: []nws.Alert{old}}
	held := heldEvent(t, old)
	res, err := newWSWNormalizer(api).Poll(testCtx(), &fakePrior{events: []*gridv1.Event{held}})
	require.NoError(t, err)
	assert.Empty(t, res.Superseded)
	require.Len(t, res.Events, 1)
	assert.Equal(t, store.ContentHash(held), store.ContentHash(res.Events[0]), "content unchanged")
	assert.Empty(t, api.lookups)
}

// End to end through the scheduler and a real SQLite store, reproducing the
// production state: the old watch at its pre-deploy revision and the Update
// stored beside it by the old code. The first tick after deploy must retire
// the old one as a revision, and later ticks must write nothing.
func TestTickRetiresNWSAlertReplacedByUpdate(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")
	cfg := wswConfig()
	oldAlert := nwsAlertFixture(t, "alert_wsw_caz139_20261110_replaced.json")
	oldAlert.ReplacedBy = ""
	feed := nwsActiveFixture(t, "active_caz137-139_20261111.json")

	// 11-10: the watch is listed and stored.
	clock := time.Date(2026, 11, 10, 19, 32, 0, 0, time.UTC)
	api := &fakeWeatherAPI{alerts: []nws.Alert{oldAlert}}
	n := NewWeatherAlertNormalizer(cfg, api)
	n.now = func() time.Time { return clock }
	spec := PollerSpec{Normalizer: n, Interval: 5 * time.Minute}
	sched := NewScheduler(st, SchedulerConfig{Tuning: cfg.Grid.Sources})
	sched.now = func() time.Time { return clock }
	sched.tick(ctx, spec, &pollerState{})

	// 11-11 22:38Z: the old code stored the Update beside it and retired nothing.
	pre, err := NewWeatherAlertNormalizer(cfg, &fakeWeatherAPI{alerts: feed}).Poll(ctx, nil)
	require.NoError(t, err)
	for _, ev := range pre.Events {
		_, err := st.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	oldEv, err := st.GetEvent(ctx, "wx:"+wswOldID)
	require.NoError(t, err)
	require.Equal(t, gridv1.EventStatus_SCHEDULED, oldEv.GetStatus())
	oldRev := oldEv.GetRevision()

	// The deploy: a new scheduler and normalizer, the feed as it is now.
	clock = wswDeployTime
	api = &fakeWeatherAPI{alerts: feed}
	n = NewWeatherAlertNormalizer(cfg, api)
	n.now = func() time.Time { return clock }
	spec = PollerSpec{Normalizer: n, Interval: 5 * time.Minute}
	sched = NewScheduler(st, SchedulerConfig{Tuning: cfg.Grid.Sources})
	sched.now = func() time.Time { return clock }
	ps := &pollerState{}
	sched.tick(ctx, spec, ps)

	oldEv, err = st.GetEvent(ctx, "wx:"+wswOldID)
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, oldEv.GetStatus())
	assert.Equal(t, oldRev+1, oldEv.GetRevision(), "the retirement, with its replacement pointer, is ONE recorded revision")
	assert.Equal(t, "wx:"+wswNewID, oldEv.GetWeatherAlert().GetReplacedByEventId())
	assert.Equal(t, "Winter Storm Watch", oldEv.GetCategory(), "the rest of the event is untouched")
	rev, err := st.EventRevisionAt(ctx, "wx:"+wswOldID, oldEv.GetRevision())
	require.NoError(t, err)
	assert.Equal(t, "wx:"+wswNewID, rev.GetEvent().GetWeatherAlert().GetReplacedByEventId(), "the pointer is in the revision history too")
	newEv, err := st.GetEvent(ctx, "wx:"+wswNewID)
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, newEv.GetStatus())
	assert.Equal(t, uint32(1), newEv.GetRevision())
	assert.Empty(t, newEv.GetWeatherAlert().GetReplacedByEventId())
	active, err := st.ActiveEventsBySource(ctx, "nws")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "wx:"+wswNewID, active[0].Event.GetId())

	// Later ticks: no churn on either event, and nothing looked up.
	for i := 0; i < 2; i++ {
		clock = clock.Add(5 * time.Minute)
		sched.tick(ctx, spec, ps)
	}
	oldEv, err = st.GetEvent(ctx, "wx:"+wswOldID)
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, oldEv.GetStatus())
	assert.Equal(t, oldRev+1, oldEv.GetRevision())
	newEv, err = st.GetEvent(ctx, "wx:"+wswNewID)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), newEv.GetRevision())
	assert.Empty(t, api.lookups)
}
