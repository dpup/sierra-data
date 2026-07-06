package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/store"
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
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:a"))
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, "s1").GetStatus())

	// Now the fetch fails: the stored event must stay ACTIVE at revision 1
	// (no lifecycle transition, no new revision), and health must degrade.
	fn.err = assert.AnError
	sched.tick(ctx, spec)

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
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:keep"))
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "s1:gone"))

	fn.result = &PollResult{Events: []*gridv1.Event{schedEvent("s1:keep", "s1", gridv1.Layer_ROAD_INCIDENT)}}
	sched.tick(ctx, spec)

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
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec) // all present: last seen = t0

	// Everything disappears from the feed.
	fn.result = &PollResult{}
	sched.tick(ctx, spec)

	assert.Equal(t, gridv1.EventStatus_EXPIRED, eventStatus(t, st, "wx:past"),
		"missing and past its own expires: expired")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:future"),
		"missing from feed but not yet past expires: stays active")
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stale"),
		"content is 48h old but the feed listed it moments ago: the grace runs from last seen, not observed_at")

	// Missing continuously: expired once the grace since last seen elapses.
	now = t0.Add(23 * time.Hour)
	sched.tick(ctx, spec)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stale"),
		"still within the grace since last seen")

	now = t0.Add(25 * time.Hour)
	sched.tick(ctx, spec)
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
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	// Present in every poll across 30h (> the 24h grace). Each tick after
	// the first is a hash-equal no-op upsert; only TouchSeen moves.
	for _, offset := range []time.Duration{0, 10 * time.Hour, 20 * time.Hour, 30 * time.Hour} {
		now = t0.Add(offset)
		sched.tick(ctx, spec)
		require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"))
	}
	got, err := st.GetEvent(ctx, "wx:stable")
	require.NoError(t, err)
	require.Equal(t, uint32(1), got.GetRevision(), "sanity: re-polls were no-op upserts")

	// Missing one poll, 1h after its last appearance (t0+30h): observed_at
	// is 31h stale, but the grace runs from last seen — stays active.
	fn.result = &PollResult{}
	now = t0.Add(31 * time.Hour)
	sched.tick(ctx, spec)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"),
		"one missed poll must not expire a stable event")

	// Still missing just inside the grace since last seen: still active.
	now = t0.Add(53 * time.Hour)
	sched.tick(ctx, spec)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wx:stable"))

	// Missing continuously past the grace since last seen (t0+30h+24h): expired.
	now = t0.Add(55 * time.Hour)
	sched.tick(ctx, spec)
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
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec)
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
	sched.tick(ctx, spec)
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

	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute})
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

	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute})
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
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec)

	// Next poll: both events disappear, but caltrans failed — only chp's
	// disappearance is meaningful.
	fn.result = &PollResult{PerSource: map[string]error{"caltrans": assert.AnError}}
	sched.tick(ctx, spec)

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
	seedSchedSources(t, st, "wfigs")

	fn := &fakeNormalizer{
		ids:    []string{"wfigs"},
		result: &PollResult{Events: []*gridv1.Event{schedEvent("wfigs:fire", "wfigs", gridv1.Layer_WILDFIRE)}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"wfigs": {Disappearance: store.DisappearanceResolve}},
	})
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec)
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "wfigs:fire"))

	// The event disappears but the poller suppresses the sweep: fetch OK,
	// disappearance evidence incomplete.
	fn.result = &PollResult{SweepSuppress: []string{"wfigs"}}
	sched.tick(ctx, spec)

	ev, err := st.GetEvent(ctx, "wfigs:fire")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus(),
		"suppressed sweep must not transition disappeared events")
	assert.Equal(t, uint32(1), ev.GetRevision())
	src := sourceByID(t, st, "wfigs")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus(), "suppression still records success")
	assert.Empty(t, src.GetLastError())

	// Suppression lifted, still missing: the disappearance now resolves.
	fn.result = &PollResult{}
	sched.tick(ctx, spec)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "wfigs:fire"))
}

// Poll receives a Prior populated from the store's active/scheduled sets for
// exactly the poller's sources.
func TestTickPriorPopulatedFromStore(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "calfire", "wfigs")

	fn := &fakeNormalizer{
		ids: []string{"calfire", "wfigs"},
		result: &PollResult{Events: []*gridv1.Event{
			schedEvent("calfire:one", "calfire", gridv1.Layer_WILDFIRE),
			schedEvent("wfigs:two", "wfigs", gridv1.Layer_WILDFIRE),
		}},
	}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{
			"calfire": {Disappearance: store.DisappearanceResolve},
			"wfigs":   {Disappearance: store.DisappearanceResolve},
		},
	})
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}

	sched.tick(ctx, spec)
	require.NotNil(t, fn.lastPrior, "Poll always receives a non-nil Prior")
	assert.Nil(t, fn.lastPrior.ByID("calfire:one"), "first tick: store is empty")
	assert.Empty(t, fn.lastPrior.ForSource("calfire"))

	sched.tick(ctx, spec)
	prior := fn.lastPrior
	require.NotNil(t, prior.ByID("calfire:one"))
	assert.Equal(t, "headline calfire:one", prior.ByID("calfire:one").GetHeadline())
	assert.Nil(t, prior.ByID("calfire:nope"))

	forCalfire := prior.ForSource("calfire")
	require.Len(t, forCalfire, 1, "ForSource splits by source id")
	assert.Equal(t, "calfire:one", forCalfire[0].GetId())
	forWfigs := prior.ForSource("wfigs")
	require.Len(t, forWfigs, 1)
	assert.Equal(t, "wfigs:two", forWfigs[0].GetId())
	assert.Empty(t, prior.ForSource("caloes"), "sources outside the poller are not loaded")
}
