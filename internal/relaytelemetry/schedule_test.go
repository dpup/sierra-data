package relaytelemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-meshrelay/meshops"
	meshrelay "github.com/dpup/sierra-meshrelay/relay"
)

// fakeReps is a scripted repeater registry and mesh: each gateway answers
// checks from its queue of results (the last one repeats).
type fakeReps struct {
	mu      sync.Mutex
	now     *time.Time
	cred    meshrelay.RepeaterCredential
	script  map[string][]error // by gateway; nil error = a reading
	calls   []call
	state   *meshrelay.RepeaterPollState
	records []string // "gateway:error text" per RecordRepeaterPoll
}

type call struct {
	gw         string
	at         time.Duration // since t0
	floodFirst bool
	addContact bool
}

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func (f *fakeReps) RepeaterCredentials() ([]meshrelay.RepeaterCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cred
	if f.state != nil {
		c.Poll = *f.state
	}
	return []meshrelay.RepeaterCredential{c}, nil
}

func (f *fakeReps) RecordRepeaterPoll(_, gateway string, err error) {
	f.records = append(f.records, gateway+":"+meshrelay.RepeaterErrorText(err))
}

func (f *fakeReps) SetRepeaterPollState(_ string, st meshrelay.RepeaterPollState) { f.state = &st }

func (f *fakeReps) CheckRepeater(_ context.Context, req meshrelay.CheckRequest) (meshrelay.RepeaterCheck, error) {
	f.calls = append(f.calls, call{gw: req.Gateway, at: f.now.Sub(t0), floodFirst: req.FloodFirst, addContact: !req.OnlyIfKnown})
	q := f.script[req.Gateway]
	if len(q) == 0 {
		return meshrelay.RepeaterCheck{}, meshrelay.ErrNotKnown
	}
	err := q[0]
	if len(q) > 1 {
		f.script[req.Gateway] = q[1:]
	}
	if err != nil {
		return meshrelay.RepeaterCheck{}, err
	}
	mv := 4000
	return meshrelay.RepeaterCheck{Gateway: req.Gateway, Access: "guest", Status: &meshops.Status{BatteryMilliVolts: &mv}}, nil
}

type fakeGateways []string

func (g fakeGateways) Gateways() ([]*meshrelay.GatewayRecord, error) {
	var out []*meshrelay.GatewayRecord
	for _, n := range g {
		out = append(out, &meshrelay.GatewayRecord{Name: n})
	}
	return out, nil
}

var (
	noReply = fmt.Errorf("check: %w", meshops.ErrNoMeshReply)
	refused = fmt.Errorf("check: %w", meshops.ErrLoginFailed)
	busy    = meshrelay.ErrBusy
)

// schedHarness is a poller on a fake clock with a scheduled repeater due now.
type schedHarness struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	reps  *fakeReps
	p     *Poller
	alive bool
}

func newSched(t *testing.T, gateways []string, script map[string][]error, st meshrelay.RepeaterPollState) *schedHarness {
	h := &schedHarness{t: t, ctx: logging.EnsureLogger(context.Background()), now: t0}
	if st.NextCycle.IsZero() {
		st.NextCycle = t0
	}
	h.reps = &fakeReps{now: &h.now, script: script, state: &st,
		cred: meshrelay.RepeaterCredential{RepeaterRecord: meshrelay.RepeaterRecord{Pubkey: strings.Repeat("a", 64),
			Name: "Ridge", UpdatedAt: t0.Add(-time.Hour)}, Password: "g"}}
	h.p = New(Config{GridURL: "http://grid.invalid", IngestToken: "x", Now: func() time.Time { return h.now }},
		nil, fakeGateways(gateways), h.reps)
	h.p.alive = func(context.Context, string, time.Duration) (bool, error) { return h.alive, nil }
	return h
}

// at advances the clock to t0+d and ticks.
func (h *schedHarness) at(d time.Duration) {
	h.now = t0.Add(d)
	h.p.RepeaterTick(h.ctx)
}

func (h *schedHarness) calls() []call { return h.reps.calls }

func TestRetriesOnTheSameGatewayThenSucceeds(t *testing.T) {
	h := newSched(t, []string{"a", "b"}, map[string][]error{"a": {noReply, noReply, nil}, "b": {nil}}, meshrelay.RepeaterPollState{})
	h.at(0)
	h.at(29 * time.Second) // not yet
	h.at(30 * time.Second)
	h.at(89 * time.Second)
	h.at(90 * time.Second)
	assert.Equal(t, []call{{gw: "a"}, {gw: "a", at: 30 * time.Second, floodFirst: true}, {gw: "a", at: 90 * time.Second, floodFirst: true}},
		h.calls(), "retries after 30s and 60s, flooding once the route went unanswered")
	assert.Equal(t, "a", h.reps.state.Primary)
	assert.Zero(t, h.reps.state.FailedCycles)
	assert.Equal(t, t0.Add(30*time.Minute), h.reps.state.NextCycle, "the next cycle keeps the cadence")
	assert.Equal(t, []string{"a:"}, h.reps.records)
}

func TestFailsOverAfterThreeRetries(t *testing.T) {
	h := newSched(t, []string{"a", "b"}, map[string][]error{"a": {noReply}, "b": {nil}}, meshrelay.RepeaterPollState{})
	for _, d := range []time.Duration{0, 30, 90, 210} {
		h.at(d * time.Second)
	}
	var got []string
	for _, c := range h.calls() {
		got = append(got, fmt.Sprintf("%s@%s", c.gw, c.at))
	}
	assert.Equal(t, []string{"a@0s", "a@30s", "a@1m30s", "a@3m30s", "b@3m30s"}, got)
	assert.Equal(t, "b", h.reps.state.Primary, "the gateway that reached it becomes primary")
}

func TestARepeaterThatMissedItsLastCycleGetsOneTryPerGateway(t *testing.T) {
	h := newSched(t, []string{"a", "b", "c", "d"}, map[string][]error{"a": {noReply}, "b": {noReply}, "c": {noReply}, "d": {nil}},
		meshrelay.RepeaterPollState{FailedCycles: 1})
	h.at(0)
	assert.Len(t, h.calls(), 3, "one try each, at most three gateways: no retries, d never tried")
	assert.Equal(t, 2, h.reps.state.FailedCycles)
	assert.Nil(t, h.reps.state.HibernatingSince)
	assert.Equal(t, []string{"c:no answer from the repeater over the mesh"}, h.reps.records)
}

func TestUnavailableGatewaysAreNotFailures(t *testing.T) {
	h := newSched(t, []string{"a", "b", "c"}, map[string][]error{"a": {busy}, "c": {nil}}, meshrelay.RepeaterPollState{Primary: "a"})
	h.at(0)
	assert.Equal(t, []string{"a", "b", "c"}, gws(h.calls()), "busy and not-a-contact move on at once")
	assert.Equal(t, "c", h.reps.state.Primary)

	// Every gateway busy: a skipped cycle, which doesn't count against it.
	h2 := newSched(t, []string{"a"}, map[string][]error{"a": {busy}}, meshrelay.RepeaterPollState{FailedCycles: 2})
	h2.at(0)
	assert.Equal(t, 2, h2.reps.state.FailedCycles)
	assert.Empty(t, h2.reps.records)
}

func TestNoNodeHasItTriesTheLastGatewayAddingTheContact(t *testing.T) {
	h := newSched(t, []string{"a", "b"}, map[string][]error{}, meshrelay.RepeaterPollState{})
	h.reps.cred.LastGateway = "b"
	h.reps.script["b"] = []error{meshrelay.ErrNotKnown, nil}
	h.at(0)
	require.Len(t, h.calls(), 3)
	assert.Equal(t, call{gw: "b", addContact: true}, h.calls()[2])
	assert.Equal(t, "b", h.reps.state.Primary)
}

func TestRefusedPasswordStopsUntilTheShareIsUpdated(t *testing.T) {
	h := newSched(t, []string{"a", "b"}, map[string][]error{"a": {refused}, "b": {nil}}, meshrelay.RepeaterPollState{})
	h.at(0)
	assert.Equal(t, []string{"a"}, gws(h.calls()), "not retried, not tried elsewhere")
	require.NotNil(t, h.reps.state.RefusedAt)
	h.at(2 * time.Hour)
	assert.Len(t, h.calls(), 1, "stopped")

	// The host shares it again with the new password.
	h.reps.script["a"] = []error{nil}
	h.reps.cred.UpdatedAt = t0.Add(3 * time.Hour)
	h.at(3*time.Hour + time.Second)
	assert.Len(t, h.calls(), 2)
	assert.Nil(t, h.reps.state.RefusedAt)
}

func TestHibernatesWhenNothingElseHearsItAndWakesWhenSomethingDoes(t *testing.T) {
	h := newSched(t, []string{"a"}, map[string][]error{"a": {noReply}}, meshrelay.RepeaterPollState{FailedCycles: 5})
	h.at(0)
	st := h.reps.state
	require.NotNil(t, st.HibernatingSince, "the sixth missed cycle, and the Grid has nothing newer")
	assert.Equal(t, t0.Add(24*time.Hour), st.NextCycle)

	// Half-hourly, only the Grid is asked; nothing goes over the mesh.
	for d := 30 * time.Minute; d <= 3*time.Hour; d += 30 * time.Minute {
		h.at(d)
	}
	assert.Len(t, h.calls(), 1)

	// Something else hears it: polled again at once, retries still off
	// until a read.
	h.alive = true
	h.reps.script["a"] = []error{nil}
	h.at(3*time.Hour + 30*time.Minute)
	assert.Len(t, h.calls(), 2)
	assert.Nil(t, h.reps.state.HibernatingSince)
	assert.Zero(t, h.reps.state.FailedCycles)
}

func TestHibernatingRepeaterIsTriedDaily(t *testing.T) {
	h := newSched(t, []string{"a", "b"}, map[string][]error{"a": {noReply}, "b": {noReply}}, meshrelay.RepeaterPollState{FailedCycles: 5})
	h.at(0)
	require.NotNil(t, h.reps.state.HibernatingSince)
	h.at(24 * time.Hour)
	assert.Len(t, h.calls(), 4, "one try per gateway each day")
	assert.Equal(t, t0.Add(48*time.Hour), h.reps.state.NextCycle)
}

func TestAliveElsewhereKeepsItOnTheNormalCycle(t *testing.T) {
	h := newSched(t, []string{"a"}, map[string][]error{"a": {noReply}}, meshrelay.RepeaterPollState{FailedCycles: 5})
	h.alive = true // heard on the mesh, just not by us
	h.at(0)
	assert.Nil(t, h.reps.state.HibernatingSince)
	assert.Equal(t, 6, h.reps.state.FailedCycles)
	assert.Equal(t, t0.Add(30*time.Minute), h.reps.state.NextCycle)
}

func TestScheduleResumesAfterARestart(t *testing.T) {
	h := newSched(t, []string{"a"}, map[string][]error{"a": {nil}}, meshrelay.RepeaterPollState{NextCycle: t0.Add(20 * time.Minute)})
	h.at(0)
	h.at(19 * time.Minute)
	assert.Empty(t, h.calls(), "not polled on start: the saved schedule holds")
	h.at(20 * time.Minute)
	assert.Len(t, h.calls(), 1)

	// A repeater never scheduled starts at its offset within the interval.
	p := New(Config{}, nil, nil, nil)
	off := p.offset(strings.Repeat("a", 64))
	assert.True(t, off >= 0 && off < 30*time.Minute)
	assert.NotEqual(t, off, p.offset(strings.Repeat("b", 64)))
}

func gws(cs []call) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.gw)
	}
	return out
}

func TestGridAlive(t *testing.T) {
	now := t0
	events := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := events[strings.TrimPrefix(r.URL.Path, "/api/v1/events/meshcore:")]
		switch {
		case !ok:
			http.NotFound(w, r)
		case body == "500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	defer srv.Close()
	p := New(Config{GridURL: srv.URL, Now: func() time.Time { return now }}, nil, nil, nil)
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	events["advert"] = `{"mesh":{"telemetry":{"lastAdvertAt":"` + ts(time.Hour) + `"}}}`
	events["oldadvert"] = `{"mesh":{"telemetry":{"lastAdvertAt":"` + ts(4*time.Hour) + `"}}}`
	events["us"] = `{"mesh":{"telemetry":{"admin":{"reporterId":"relay","lastSuccessAt":"` + ts(time.Minute) + `"}}}}`
	events["alanpi"] = `{"mesh":{"telemetry":{"admin":{"reporterId":"alan-pi","lastSuccessAt":"` + ts(time.Minute) + `"}}}}`
	events["broken"] = "500"

	for pk, want := range map[string]bool{"advert": true, "oldadvert": false, "us": false, "alanpi": true, "unknown": false} {
		got, err := p.gridAlive(context.Background(), pk, 3*time.Hour)
		require.NoError(t, err, pk)
		assert.Equal(t, want, got, pk)
	}
	_, err := p.gridAlive(context.Background(), "broken", 3*time.Hour)
	assert.Error(t, err, "an error is not 'dead': the caller doesn't hibernate on it")
}
