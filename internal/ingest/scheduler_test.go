package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// fakeNormalizer returns a canned PollResult (or error), mutable between
// ticks, and captures the Prior each Poll received.
type fakeNormalizer struct {
	ids       []string
	result    *PollResult
	err       error
	polls     int
	lastPrior Prior
}

func (f *fakeNormalizer) SourceIDs() []string { return f.ids }

func (f *fakeNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	f.polls++
	f.lastPrior = prior
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeNWSEnhancer struct {
	calls      int
	lastPlaces []string
	summary    string
	err        error
}

func (f *fakeNWSEnhancer) Enhance(ctx context.Context, headline, description string, placeNames []string) (NWSEnhancement, error) {
	f.calls++
	f.lastPlaces = placeNames
	if f.err != nil {
		return NWSEnhancement{}, f.err
	}
	return NWSEnhancement{Summary: f.summary, Request: "req:" + headline, Response: `{"summary":"` + f.summary + `"}`}, nil
}

func newSchedStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// seedSchedSources seeds registry rows so RecordAttempt and the events
// source_id foreign key work.
func seedSchedSources(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()
	seeds := make([]store.SourceSeed, 0, len(ids))
	for _, id := range ids {
		seeds = append(seeds, store.SourceSeed{ID: id, Name: id, PollInterval: time.Minute})
	}
	require.NoError(t, s.SeedSources(context.Background(), seeds))
}

func schedEvent(id, src string, layer gridv1.Layer) *gridv1.Event {
	return &gridv1.Event{
		Id:         id,
		Layer:      layer,
		Severity:   gridv1.Severity_MODERATE,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "headline " + id,
		Provenance: &gridv1.Provenance{SourceId: src},
	}
}

func sourceByID(t *testing.T, s *store.Store, id string) *gridv1.Source {
	t.Helper()
	srcs, err := s.ListSources(context.Background())
	require.NoError(t, err)
	for _, src := range srcs {
		if src.GetId() == id {
			return src
		}
	}
	t.Fatalf("source %s not found", id)
	return nil
}

func eventStatus(t *testing.T, s *store.Store, id string) gridv1.EventStatus {
	t.Helper()
	ev, err := s.GetEvent(context.Background(), id)
	require.NoError(t, err)
	return ev.GetStatus()
}

// A failed poll must record the error for every covered source and must NOT
// resolve or expire anything — an error never becomes an all-clear.
func TestTickFailedPollNeverTransitions(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "s1")

	fn := &fakeNormalizer{
		ids:    []string{"s1"},
		result: &PollResult{Events: []*gridv1.Event{schedEvent("s1:a", "s1", gridv1.Layer_EARTHQUAKE)}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"s1": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:a"))
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "s1").GetStatus())

	// Now the fetch fails: the stored event must stay ACTIVE at revision 1
	// (no lifecycle transition, no new revision), and health must degrade.
	fn.err = assert.AnError
	sched.tick(ctx, spec, ps)

	ev, err := st.GetEvent(ctx, "s1:a")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, uint32(1), ev.GetRevision())

	src := sourceByID(t, st, "s1")
	assert.Equal(t, gridv1.SourceStatus_STALE, src.GetStatus()) // last success moments ago
	assert.NotEmpty(t, src.GetLastError())
}

// A successful poll upserts the returned set and resolves what disappeared
// (resolve policy: the feed is authoritatively active-only).
func TestTickResolvesDisappeared(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "s1")

	fn := &fakeNormalizer{
		ids: []string{"s1"},
		result: &PollResult{Events: []*gridv1.Event{
			schedEvent("s1:keep", "s1", gridv1.Layer_ROAD_INCIDENT),
			schedEvent("s1:gone", "s1", gridv1.Layer_ROAD_INCIDENT),
		}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"s1": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:keep"))
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:gone"))

	fn.result = &PollResult{Events: []*gridv1.Event{schedEvent("s1:keep", "s1", gridv1.Layer_ROAD_INCIDENT)}}
	sched.tick(ctx, spec, ps)

	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:keep"))
	gone, err := st.GetEvent(ctx, "s1:gone")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, gone.GetStatus())
	assert.Equal(t, uint32(2), gone.GetRevision(), "the all-clear is a recorded revision")
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "s1").GetStatus())
}

// Expire policy: a disappeared event only becomes EXPIRED once past its own
// expires time, or once the expire_after grace has elapsed since the LAST
// SUCCESSFUL POLL that included it. observed_at is the time of the last
// content change and must NOT anchor the grace — a stable event that misses
// one poll would be expired instantly.
func TestTickExpirePolicy(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")
	t0 := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	now := t0

	past := schedEvent("wx:past", "nws", gridv1.Layer_WEATHER_ALERT)
	past.Expires = timestamppb.New(t0.Add(-time.Hour))
	future := schedEvent("wx:future", "nws", gridv1.Layer_WEATHER_ALERT)
	future.Expires = timestamppb.New(t0.Add(48 * time.Hour))
	stale := schedEvent("wx:stale", "nws", gridv1.Layer_WEATHER_ALERT)
	stale.ObservedAt = timestamppb.New(t0.Add(-48 * time.Hour)) // no expires; content 48h old

	fn := &fakeNormalizer{
		ids:    []string{"nws"},
		result: &PollResult{Events: []*gridv1.Event{past, future, stale}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"nws": {
			Disappearance: store.DisappearanceExpire,
			ExpireAfter:   24 * time.Hour,
		}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec, ps) // all present: last seen = t0

	// Everything disappears from the feed.
	fn.result = &PollResult{}
	sched.tick(ctx, spec, ps)

	assert.Equal(t, gridv1.EventStatus_EXPIRED, eventStatus(t, st, "wx:past"),
		"missing and past its own expires: expired")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:future"),
		"missing from feed but not yet past expires: stays active")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stale"),
		"content is 48h old but the feed listed it moments ago: the grace runs from last seen, not observed_at")

	// Missing continuously: expired once the grace since last seen elapses.
	now = t0.Add(23 * time.Hour)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stale"),
		"still within the grace since last seen")

	now = t0.Add(25 * time.Hour)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_EXPIRED, eventStatus(t, st, "wx:stale"),
		"missing continuously past the grace since last seen: expired")
}

// A stable event (unchanged content => hash-equal no-op upserts) present in
// every poll for LONGER than the expire grace, then missing once, must not
// expire until the grace elapses after its last successful appearance.
func TestTickExpireAnchorsToLastSeen(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")

	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	now := t0
	ev := schedEvent("wx:stable", "nws", gridv1.Layer_WEATHER_ALERT)
	ev.ObservedAt = timestamppb.New(t0) // content never changes after this

	fn := &fakeNormalizer{ids: []string{"nws"}, result: &PollResult{Events: []*gridv1.Event{ev}}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"nws": {
			Disappearance: store.DisappearanceExpire,
			ExpireAfter:   24 * time.Hour,
		}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	// Present in every poll across 30h (> the 24h grace). Each tick after
	// the first is a hash-equal no-op upsert; only TouchSeen moves.
	for _, offset := range []time.Duration{0, 10 * time.Hour, 20 * time.Hour, 30 * time.Hour} {
		now = t0.Add(offset)
		sched.tick(ctx, spec, ps)
		require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"))
	}
	got, err := st.GetEvent(ctx, "wx:stable")
	require.NoError(t, err)
	require.Equal(t, uint32(1), got.GetRevision(), "sanity: re-polls were no-op upserts")

	// Missing one poll, 1h after its last appearance (t0+30h): observed_at
	// is 31h stale, but the grace runs from last seen — stays active.
	fn.result = &PollResult{}
	now = t0.Add(31 * time.Hour)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"),
		"one missed poll must not expire a stable event")

	// Still missing just inside the grace since last seen: still active.
	now = t0.Add(53 * time.Hour)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"))

	// Missing continuously past the grace since last seen (t0+30h+24h): expired.
	now = t0.Add(55 * time.Hour)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_EXPIRED, eventStatus(t, st, "wx:stable"))
}

// shouldExpire falls back to observed_at then ingested_at only for rows with
// a zero LastSeenAt (pre-migration rows from an old-schema DB).
func TestShouldExpireLastSeenFallback(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	grace := 24 * time.Hour

	observedOld := &gridv1.Event{ObservedAt: timestamppb.New(now.Add(-48 * time.Hour))}
	assert.True(t, shouldExpire(store.StoredEvent{Event: observedOld}, grace, now),
		"zero last-seen falls back to observed_at")
	assert.False(t, shouldExpire(store.StoredEvent{Event: observedOld, LastSeenAt: now.Add(-time.Hour)}, grace, now),
		"a recent last-seen wins over stale observed_at")

	ingestedOld := &gridv1.Event{IngestedAt: timestamppb.New(now.Add(-48 * time.Hour))}
	assert.True(t, shouldExpire(store.StoredEvent{Event: ingestedOld}, grace, now),
		"no observed_at falls back to ingested_at")

	assert.False(t, shouldExpire(store.StoredEvent{Event: &gridv1.Event{}}, grace, now),
		"no reference time at all: never grace-expired")
}

// The enhancement budget caps Enhance calls per tick; unenhanced events are
// still upserted raw, and unchanged content on a later tick spends nothing.
func TestTickEnhancementBudget(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")

	a := schedEvent("wx:a", "nws", gridv1.Layer_WEATHER_ALERT)
	b := schedEvent("wx:b", "nws", gridv1.Layer_WEATHER_ALERT)
	fn := &fakeNormalizer{ids: []string{"nws"}, result: &PollResult{Events: []*gridv1.Event{a, b}}}
	fe := &fakeNWSEnhancer{summary: "Two plain sentences."}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning:        map[string]config.SourceTuning{"nws": {Disappearance: store.DisappearanceExpire}},
		Enhancer:      fe,
		EnhancerModel: "gpt-5-mini",
		BudgetPerTick: 1,
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	assert.Equal(t, 1, fe.calls, "budget of 1 caps enhancement to one call")

	enhanced, err := st.GetEvent(ctx, "wx:a")
	require.NoError(t, err)
	assert.Equal(t, "Two plain sentences.", enhanced.GetSummary())
	require.NotNil(t, enhanced.GetEnhancement())
	assert.Equal(t, "gpt-5-mini", enhanced.GetEnhancement().GetModel())
	assert.Equal(t, []string{"summary"}, enhanced.GetEnhancement().GetFields())
	// Model I/O captured for transparency.
	assert.NotEmpty(t, enhanced.GetEnhancement().GetRequest())
	assert.NotEmpty(t, enhanced.GetEnhancement().GetResponse())

	raw, err := st.GetEvent(ctx, "wx:b")
	require.NoError(t, err)
	assert.Empty(t, raw.GetSummary(), "over-budget event is served raw")
	assert.Nil(t, raw.GetEnhancement())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, raw.GetStatus(), "budget never blocks ingest")

	// Unchanged content on the next tick: budget resets but nothing is
	// spent (NeedsUpdate gates the spend), and the stored summary survives
	// the no-op upsert.
	sched.tick(ctx, spec, ps)
	assert.Equal(t, 1, fe.calls)
	enhanced, err = st.GetEvent(ctx, "wx:a")
	require.NoError(t, err)
	assert.Equal(t, "Two plain sentences.", enhanced.GetSummary())
}

// Enhancement failure is log-and-continue: the alert is upserted raw.
func TestTickEnhancementFailureStillUpserts(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")

	fn := &fakeNormalizer{
		ids:    []string{"nws"},
		result: &PollResult{Events: []*gridv1.Event{schedEvent("wx:a", "nws", gridv1.Layer_WEATHER_ALERT)}},
	}
	fe := &fakeNWSEnhancer{err: assert.AnError}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning:        map[string]config.SourceTuning{"nws": {Disappearance: store.DisappearanceExpire}},
		Enhancer:      fe,
		BudgetPerTick: 5,
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine

	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute}, ps)
	assert.Equal(t, 1, fe.calls)

	ev, err := st.GetEvent(ctx, "wx:a")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Empty(t, ev.GetSummary())
	assert.Nil(t, ev.GetEnhancement())
}

// Enhancement passes the event's attached places (resolved to names) as the
// localization grounding, and non-weather-alert layers are never enhanced.
func TestTickEnhancementPlaceNamesAndLayerGate(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws", "usgs")
	require.NoError(t, st.UpsertPlace(ctx, &gridv1.Place{
		Id: "area:calaveras", Kind: gridv1.PlaceKind_AREA, Name: "Calaveras County", Slug: "calaveras",
	}))

	wx := schedEvent("wx:a", "nws", gridv1.Layer_WEATHER_ALERT)
	wx.PlaceIds = []string{"area:calaveras", "area:unknown"}
	quake := schedEvent("usgs:q", "usgs", gridv1.Layer_EARTHQUAKE)
	fn := &fakeNormalizer{
		ids:    []string{"nws", "usgs"},
		result: &PollResult{Events: []*gridv1.Event{quake, wx}},
	}
	fe := &fakeNWSEnhancer{summary: "Localized summary."}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning:        map[string]config.SourceTuning{"nws": {Disappearance: store.DisappearanceExpire}},
		Enhancer:      fe,
		BudgetPerTick: 5,
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine

	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute}, ps)
	assert.Equal(t, 1, fe.calls, "earthquake layer must not be enhanced")
	assert.Equal(t, []string{"Calaveras County"}, fe.lastPlaces, "unknown place ids are skipped")
}

// PerSource partial failure: disappearance only sweeps the healthy source;
// the failed source's events are untouched and its health degrades.
func TestTickPerSourcePartialFailure(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "chp", "caltrans")

	fn := &fakeNormalizer{
		ids: []string{"chp", "caltrans"},
		result: &PollResult{Events: []*gridv1.Event{
			schedEvent("chp:gone", "chp", gridv1.Layer_ROAD_INCIDENT),
			schedEvent("caltrans:gone", "caltrans", gridv1.Layer_ROAD_INCIDENT),
		}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{
			"chp":      {Disappearance: store.DisappearanceResolve},
			"caltrans": {Disappearance: store.DisappearanceResolve},
		},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec, ps)

	// Next poll: both events disappear, but caltrans failed — only chp's
	// disappearance is meaningful.
	fn.result = &PollResult{PerSource: map[string]error{"caltrans": assert.AnError}}
	sched.tick(ctx, spec, ps)

	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "chp:gone"))
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "caltrans:gone"),
		"a failed source's missing events must not transition")

	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "chp").GetStatus())
	caltrans := sourceByID(t, st, "caltrans")
	assert.Equal(t, gridv1.SourceStatus_STALE, caltrans.GetStatus())
	assert.NotEmpty(t, caltrans.GetLastError())
}

// SweepSuppress: a source whose fetch succeeded but whose full current set
// could not be computed (e.g. wildfire's standalone-perimeter set while the
// sibling feed is down) skips the disappearance sweep — a partial view must
// never become an all-clear — while its health still records the success.
func TestTickSweepSuppression(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "firis")

	fn := &fakeNormalizer{
		ids:    []string{"firis"},
		result: &PollResult{Events: []*gridv1.Event{schedEvent("firis:fire", "firis", gridv1.Layer_WILDFIRE)}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"firis": {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec, ps)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "firis:fire"))

	// The event disappears but the poller suppresses the sweep: fetch OK,
	// disappearance evidence incomplete.
	fn.result = &PollResult{SweepSuppress: []string{"firis"}}
	sched.tick(ctx, spec, ps)

	ev, err := st.GetEvent(ctx, "firis:fire")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus(),
		"suppressed sweep must not transition disappeared events")
	assert.Equal(t, uint32(1), ev.GetRevision())
	src := sourceByID(t, st, "firis")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus(), "suppression still records success")
	assert.Empty(t, src.GetLastError())

	// Suppression lifted, still missing: the disappearance now resolves.
	fn.result = &PollResult{}
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "firis:fire"))
}

// Poll receives a Prior populated from the store's active/scheduled sets for
// exactly the poller's sources.
func TestTickPriorPopulatedFromStore(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "calfire", "firis")

	fn := &fakeNormalizer{
		ids: []string{"calfire", "firis"},
		result: &PollResult{Events: []*gridv1.Event{
			schedEvent("calfire:one", "calfire", gridv1.Layer_WILDFIRE),
			schedEvent("firis:two", "firis", gridv1.Layer_WILDFIRE),
		}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{
			"calfire": {Disappearance: store.DisappearanceResolve},
			"firis":   {Disappearance: store.DisappearanceResolve},
		},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec, ps)
	require.NotNil(t, fn.lastPrior, "Poll always receives a non-nil Prior")
	assert.Nil(t, fn.lastPrior.ByID("calfire:one"), "first tick: store is empty")
	assert.Empty(t, fn.lastPrior.ForSource("calfire"))

	sched.tick(ctx, spec, ps)
	prior := fn.lastPrior
	require.NotNil(t, prior.ByID("calfire:one"))
	assert.Equal(t, "headline calfire:one", prior.ByID("calfire:one").GetHeadline())
	assert.Nil(t, prior.ByID("calfire:nope"))

	forCalfire := prior.ForSource("calfire")
	require.Len(t, forCalfire, 1, "ForSource splits by source id")
	assert.Equal(t, "calfire:one", forCalfire[0].GetId())
	forWfigs := prior.ForSource("firis")
	require.Len(t, forWfigs, 1)
	assert.Equal(t, "firis:two", forWfigs[0].GetId())
	assert.Empty(t, prior.ForSource("caloes"), "sources outside the poller are not loaded")
}

// --- Supersession (PollResult.Superseded) -----------------------------------
//
// The inverse of SweepSuppress: an event the poller proves is gone because it
// knows its successor. It must resolve NOW rather than sit out the expire
// grace, which exists only for ambiguous absence.

func TestTickSupersededResolvesImmediatelyDespiteExpireGrace(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")
	t0 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	now := t0

	old := schedEvent("wx:old", "nws", gridv1.Layer_WEATHER_ALERT)
	other := schedEvent("wx:other", "nws", gridv1.Layer_WEATHER_ALERT)
	fn := &fakeNormalizer{
		ids:    []string{"nws"},
		result: &PollResult{Events: []*gridv1.Event{old, other}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"nws": {
			Disappearance: store.DisappearanceExpire,
			ExpireAfter:   24 * time.Hour,
		}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec, ps)

	// Next tick: both drop out of Events, but only wx:old has a named successor.
	successor := schedEvent("wx:new", "nws", gridv1.Layer_WEATHER_ALERT)
	fn.result = &PollResult{Events: []*gridv1.Event{successor}, Superseded: []string{"wx:old"}}
	now = t0.Add(time.Minute)
	sched.tick(ctx, spec, ps)

	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "wx:old"),
		"a named successor makes absence unambiguous: resolve now, don't wait out the 24h grace")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:other"),
		"merely absent: the expire grace still protects it")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:new"))

	// The transition is a recorded revision, not a delete — history is kept.
	hist, _, err := st.EventHistory(ctx, "wx:old", 50, "")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(hist), 2, "supersession writes a revision")

	// Idempotent: re-superseding an already-resolved id is a no-op.
	now = t0.Add(2 * time.Minute)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "wx:old"))
}

// Fail-loud: supersession obeys the same guard as the sweep. A source whose
// fetch failed transitions NOTHING, even if the poller named ids.
func TestTickSupersededSkippedWhenSourceFailed(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "nws")
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	old := schedEvent("wx:old", "nws", gridv1.Layer_WEATHER_ALERT)
	fn := &fakeNormalizer{ids: []string{"nws"}, result: &PollResult{Events: []*gridv1.Event{old}}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"nws": {
			Disappearance: store.DisappearanceExpire, ExpireAfter: 24 * time.Hour,
		}},
	})
	ps := &pollerState{} // shared across this test's ticks, like a real poller goroutine
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec, ps)

	fn.result = &PollResult{
		PerSource:  map[string]error{"nws": assert.AnError},
		Superseded: []string{"wx:old"},
	}
	now = now.Add(time.Minute)
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:old"),
		"a failed fetch must never transition events, supersession included")
}

// TestTickIDRenameNeedsNoMigration answers "can a programmatic migration be
// applied?" for the Caltrans id change — by showing none is needed.
//
// The old ids (chp:<ClosureID>, provenance caltrans) are simply absent from the
// first successful poll under the new scheme. `caltrans` is a `resolve` source,
// so the disappearance sweep retires them with a RECORDED revision — a proper
// closing entry, not a silent delete or a rename. New closures start clean at
// revision 1.
//
// A rename migration would be strictly worse, and not merely unnecessary. The
// mapping is 1:N, not 1:1: one stored chp:C99CB row conflated 16 real closures,
// and its history interleaves them (33 distinct centroids, 30 km apart). There
// is no SQL that splits that into 16 correct histories, and attaching the whole
// mashup to whichever closure "won" the rename would give one real ramp closure
// a fabricated multi-week history.
func TestTickIDRenameNeedsNoMigration(t *testing.T) {
	st := newSchedStore(t)
	seedSchedSources(t, st, "caltrans")

	// A pre-existing event under the OLD scheme.
	_, err := st.UpsertEvent(testCtx(), schedEvent("chp:C99CB", "caltrans", gridv1.Layer_ROAD_INCIDENT))
	require.NoError(t, err)

	// The first poll after deploy emits the NEW ids for the same closures.
	n := &fakeNormalizer{ids: []string{"caltrans"}, result: &PollResult{Events: []*gridv1.Event{
		schedEvent("caltrans:C99CB-14-d65d75", "caltrans", gridv1.Layer_ROAD_INCIDENT),
		schedEvent("caltrans:C99CB-19-d47db1", "caltrans", gridv1.Layer_ROAD_INCIDENT),
	}}}
	s := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"caltrans": {Disappearance: store.DisappearanceResolve}},
	})
	s.tick(testCtx(), PollerSpec{Normalizer: n, Interval: time.Minute}, &pollerState{})

	old, err := st.GetEvent(testCtx(), "chp:C99CB")
	require.NoError(t, err, "the old id is retired, not deleted — its history stays queryable")
	assert.Equal(t, gridv1.EventStatus_RESOLVED, old.GetStatus(),
		"the sweep closes the old id on its own; no migration required")

	for _, id := range []string{"caltrans:C99CB-14-d65d75", "caltrans:C99CB-19-d47db1"} {
		got, err := st.GetEvent(testCtx(), id)
		require.NoError(t, err)
		assert.Equal(t, gridv1.EventStatus_ACTIVE, got.GetStatus())
		assert.Equal(t, uint32(1), got.GetRevision(), "a new closure starts a clean history")
	}
}

// TestTickSkipsNoopUpserts pins the write-skip gate. Mesh telemetry is zeroed
// out of the content hash by design, so a node merely re-advertising is
// hash-equal — and before this gate every such event still opened a transaction
// (BEGIN + 3 SELECTs + COMMIT). On EFS each of those is a network round trip,
// which is what stretched the mesh tick's write phase to 7-9s and gave readers
// that many more chances to block on a commit.
func TestTickSkipsNoopUpserts(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "s1")
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"s1": {Disappearance: store.DisappearanceResolve}},
	})
	ev := schedEvent("s1:a", "s1", gridv1.Layer_EARTHQUAKE)

	// The first tick of a poller's life always reconciles fully.
	assert.True(t, sched.shouldUpsert(ctx, ev, true), "full reconcile always writes")

	fn := &fakeNormalizer{ids: []string{"s1"}, result: &PollResult{Events: []*gridv1.Event{ev}}}
	ps := &pollerState{}
	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute}, ps)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:a"))
	assert.True(t, ps.reconciled, "the first tick records that it reconciled")

	// Unchanged content, no enhancement, places unchanged => no write needed.
	assert.False(t, sched.shouldUpsert(ctx, ev, false),
		"an unchanged event must not open a transaction")

	// Changed content still writes.
	changed := schedEvent("s1:a", "s1", gridv1.Layer_EARTHQUAKE)
	changed.Headline = "something new"
	assert.True(t, sched.shouldUpsert(ctx, changed, false), "changed content must write")

	// An event CARRYING an enhancement must always write, even hash-equal.
	// Enhancement and summary are excluded from the content hash, so the
	// hash-equal upsert path is the ONLY thing that persists them — road
	// incidents arrive already enhanced and are routinely hash-equal, so
	// skipping them would silently drop AI text that was just regenerated.
	enhanced := schedEvent("s1:a", "s1", gridv1.Layer_EARTHQUAKE)
	enhanced.Enhancement = &gridv1.Enhancement{Model: "m", Fields: []string{"summary"}}
	assert.True(t, sched.shouldUpsert(ctx, enhanced, false),
		"a carried enhancement must be persisted even when the content hash is equal")
}

// TestTickReconcilesWhenPlacesChange is the correctness guard on the skip gate.
//
// Place attachments are DERIVED state, recomputed by the hash-equal upsert path
// (refreshEventPlaces). Skipping that path for unchanged events is safe only
// because a change to the place set forces one full pass — otherwise an event
// that arrived BEFORE a place was seeded would never attach to it, and would
// silently vanish from that place's map and summary forever.
func TestTickReconcilesWhenPlacesChange(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "s1")
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"s1": {Disappearance: store.DisappearanceResolve}},
	})

	ev := schedEvent("s1:a", "s1", gridv1.Layer_EARTHQUAKE)
	ev.Geometry = &gridv1.Geometry{Geojson: []byte(`{"type":"Point","coordinates":[-120.45,38.2]}`)}
	fn := &fakeNormalizer{ids: []string{"s1"}, result: &PollResult{Events: []*gridv1.Event{ev}}}
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	ps := &pollerState{}

	// Tick 1: no places exist yet, so nothing to attach to.
	sched.tick(ctx, spec, ps)
	got, err := st.GetEvent(ctx, "s1:a")
	require.NoError(t, err)
	require.Empty(t, got.GetPlaceIds(), "no places seeded yet")
	v1 := ps.placesVersion

	// Tick 2 with the place set unchanged: the event is hash-equal, so it is
	// skipped entirely. Still no attachments, and the version has not moved.
	sched.tick(ctx, spec, ps)
	assert.Equal(t, v1, ps.placesVersion, "an unchanged place set must not force a reconcile")

	// A place is seeded that CONTAINS the event — after the event first arrived.
	require.NoError(t, st.UpsertPlace(ctx, &gridv1.Place{
		Id: "county:calaveras", Slug: "calaveras", Name: "Calaveras County",
		Kind: gridv1.PlaceKind_COUNTY,
		Geometry: &gridv1.Geometry{Geojson: []byte(
			`{"type":"Polygon","coordinates":[[[-120.9,38.0],[-120.0,38.0],[-120.0,38.5],[-120.9,38.5],[-120.9,38.0]]]}`)},
	}))

	// Tick 3: the place version changed, so this tick reconciles in full and the
	// pre-existing event attaches retroactively.
	sched.tick(ctx, spec, ps)
	got, err = st.GetEvent(ctx, "s1:a")
	require.NoError(t, err)
	assert.Contains(t, got.GetPlaceIds(), "county:calaveras",
		"an event that predates a place must still attach once that place is seeded")
	assert.NotEqual(t, v1, ps.placesVersion, "the reconcile records the new place version")
}
