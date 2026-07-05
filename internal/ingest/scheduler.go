package ingest

import (
	"context"
	"math/rand/v2"
	"runtime/debug"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/store"
)

// maxStartJitter spreads poller start times so a restart doesn't hit every
// upstream at once.
const maxStartJitter = 15 * time.Second

// PollerSpec pairs a normalizer with its poll interval. A normalizer spanning
// several sources (wildfire, road incidents) runs at one interval: the
// fastest of its sources' configured cadences.
type PollerSpec struct {
	Normalizer Normalizer
	Interval   time.Duration
}

// SchedulerConfig wires a Scheduler. Tuning keys are source ids; missing
// entries default to the resolve policy with no expire grace. A nil Enhancer
// (or a zero BudgetPerTick) disables weather-alert enhancement.
type SchedulerConfig struct {
	Pollers       []PollerSpec
	Tuning        map[string]config.SourceTuning
	Enhancer      NWSEnhancer
	EnhancerModel string // stamped into Enhancement.model on enhanced events
	BudgetPerTick int    // max Enhance calls (attempts, not successes) per tick
}

// Scheduler drives the ingest pollers: one goroutine per PollerSpec, each
// tick polls the upstream, upserts the returned events, applies the
// per-source disappearance policy, and records source health.
type Scheduler struct {
	store         *store.Store
	pollers       []PollerSpec
	tuning        map[string]config.SourceTuning
	enhancer      NWSEnhancer
	enhancerModel string
	budgetPerTick int
	now           func() time.Time // injectable for lifecycle tests
}

// NewScheduler builds a scheduler over the given store.
func NewScheduler(st *store.Store, cfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		store:         st,
		pollers:       cfg.Pollers,
		tuning:        cfg.Tuning,
		enhancer:      cfg.Enhancer,
		enhancerModel: cfg.EnhancerModel,
		budgetPerTick: cfg.BudgetPerTick,
		now:           time.Now,
	}
}

// Start launches one polling goroutine per spec. Each takes a jittered
// initial delay (0..maxStartJitter), ticks immediately, then on its interval
// until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	for _, spec := range s.pollers {
		go s.run(ctx, spec)
	}
	logging.Infow(ctx, "Ingest scheduler started", "pollers", len(s.pollers))
}

func (s *Scheduler) run(ctx context.Context, spec PollerSpec) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(rand.N(maxStartJitter)):
	}
	s.safeTick(ctx, spec)

	ticker := time.NewTicker(spec.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logging.Infow(ctx, "Ingest poller stopping", "sources", spec.Normalizer.SourceIDs())
			return
		case <-ticker.C:
			s.safeTick(ctx, spec)
		}
	}
}

// safeTick isolates a panicking tick so one bad poll can't kill the poller
// goroutine (mirrors periodic_refresh.go's recovery).
func (s *Scheduler) safeTick(ctx context.Context, spec PollerSpec) {
	defer func() {
		if r := recover(); r != nil {
			err, _ := errors.ParseStack(debug.Stack())
			skipFrames := 3
			numFrames := 5
			logging.Errorw(ctx, "Ingest tick: recovered from panic",
				"sources", spec.Normalizer.SourceIDs(), "error", r,
				"error.stack_trace", err.MinimalStack(skipFrames, numFrames))
		}
	}()
	s.tick(ctx, spec)
}

// tick is one poll cycle. The critical lifecycle invariant lives here:
//
//	a failed fetch must NEVER resolve or expire events.
//
// Disappearance ("missing from the feed") is only meaningful against a
// successful poll of that source; on error we record the failure for every
// source the poller covers and stop — an error never becomes an all-clear
// (the same fail-loud contract as the evacuation layer).
func (s *Scheduler) tick(ctx context.Context, spec PollerSpec) {
	n := spec.Normalizer
	sourceIDs := n.SourceIDs()

	// (a) Poll. A hard error fails every covered source and ends the tick.
	result, err := n.Poll(ctx)
	if err != nil {
		logging.Errorw(ctx, "Ingest tick: poll failed", "sources", sourceIDs, "error", err)
		for _, src := range sourceIDs {
			s.recordAttempt(ctx, src, err)
		}
		return
	}

	// (b) Upsert every polled event (enhancing changed weather alerts within
	// budget), tracking polled ids per source for the disappearance diff.
	now := s.now()
	budget := s.budgetPerTick
	polled := make(map[string]map[string]bool, len(sourceIDs))
	for _, src := range sourceIDs {
		polled[src] = make(map[string]bool)
	}
	for _, ev := range result.Events {
		if ids, ok := polled[ev.GetProvenance().GetSourceId()]; ok {
			ids[ev.GetId()] = true
		}
		s.maybeEnhance(ctx, ev, &budget)
		if _, err := s.store.UpsertEvent(ctx, ev); err != nil {
			logging.Errorw(ctx, "Ingest tick: upsert failed", "event", ev.GetId(), "error", err)
		}
	}

	// (c) Disappearance sweep — ONLY for sources whose fetch succeeded this
	// tick (PerSource errors mean that source's absence proves nothing).
	for _, src := range sourceIDs {
		if result.PerSource[src] != nil {
			continue
		}
		s.sweepDisappeared(ctx, src, polled[src], now)
	}

	// (d) Per-source health: nil for success, the partial error otherwise.
	for _, src := range sourceIDs {
		s.recordAttempt(ctx, src, result.PerSource[src])
	}
}

// maybeEnhance summarizes a changed weather alert while budget remains.
// NeedsUpdate gates the spend: unchanged content keeps its stored summary (a
// no-op upsert) and costs nothing. Enhancement failure is log-and-continue —
// the alert is served raw, never blocked (spec §3.1 policy 5).
func (s *Scheduler) maybeEnhance(ctx context.Context, ev *gridv1.Event, budget *int) {
	if s.enhancer == nil || ev.GetLayer() != gridv1.Layer_WEATHER_ALERT || *budget <= 0 {
		return
	}
	changed, err := s.store.NeedsUpdate(ctx, ev)
	if err != nil {
		logging.Warnw(ctx, "Ingest tick: needs-update check failed; skipping enhancement",
			"event", ev.GetId(), "error", err)
		return
	}
	if !changed {
		return
	}
	*budget-- // attempts spend budget, so failures can't loop the API
	summary, err := s.enhancer.Enhance(ctx, ev.GetHeadline(), ev.GetDescription(), s.placeNames(ctx, ev))
	if err != nil {
		logging.Warnw(ctx, "Ingest tick: enhancement failed; serving raw alert",
			"event", ev.GetId(), "error", err)
		return
	}
	ev.Summary = summary
	ev.Enhancement = &gridv1.Enhancement{
		Model:      s.enhancerModel,
		EnhancedAt: timestamppb.New(s.now()),
		Fields:     []string{"summary"},
	}
}

// placeNames resolves the event's attached place ids to display names — the
// grounding list the enhancer may localize against (spec §3.1 policy 1).
// Unknown ids are skipped: the list is grounding, not a requirement.
func (s *Scheduler) placeNames(ctx context.Context, ev *gridv1.Event) []string {
	var names []string
	for _, id := range ev.GetPlaceIds() {
		p, err := s.store.GetPlace(ctx, id)
		if err != nil {
			continue
		}
		if p.GetName() != "" {
			names = append(names, p.GetName())
		}
	}
	return names
}

// sweepDisappeared applies the source's disappearance policy to active
// events missing from this (successful) poll:
//
//	resolve — the feed is authoritatively active-only: missing => RESOLVED.
//	expire  — missing proves nothing by itself: EXPIRED only once past the
//	          event's own expires time, or past the expireAfter grace since
//	          it was last observed; otherwise it stays active.
func (s *Scheduler) sweepDisappeared(ctx context.Context, src string, polledIDs map[string]bool, now time.Time) {
	active, err := s.store.ActiveEventsBySource(ctx, src)
	if err != nil {
		logging.Errorw(ctx, "Ingest tick: listing active events failed; skipping sweep",
			"source", src, "error", err)
		return
	}
	var disappeared []*gridv1.Event
	for _, ev := range active {
		if !polledIDs[ev.GetId()] {
			disappeared = append(disappeared, ev)
		}
	}
	if len(disappeared) == 0 {
		return
	}

	tuning := s.tuning[src]
	var ids []string
	to := gridv1.EventStatus_RESOLVED
	if tuning.Disappearance == store.DisappearanceExpire {
		to = gridv1.EventStatus_EXPIRED
		for _, ev := range disappeared {
			if shouldExpire(ev, tuning.ExpireAfter, now) {
				ids = append(ids, ev.GetId())
			}
		}
	} else {
		for _, ev := range disappeared {
			ids = append(ids, ev.GetId())
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := s.store.TransitionEvents(ctx, ids, to, now); err != nil {
		logging.Errorw(ctx, "Ingest tick: lifecycle transition failed",
			"source", src, "to", to.String(), "error", err)
		return
	}
	logging.Infow(ctx, "Ingest tick: transitioned disappeared events",
		"source", src, "to", to.String(), "count", len(ids))
}

// shouldExpire implements the expire policy's time test. Events without an
// upstream observed_at stamp (e.g. standalone WFIGS perimeters) age from
// their last ingest instead, so the grace still terminates them.
func shouldExpire(ev *gridv1.Event, expireAfter time.Duration, now time.Time) bool {
	if exp := ev.GetExpires(); exp != nil && now.After(exp.AsTime()) {
		return true
	}
	if expireAfter <= 0 {
		return false
	}
	ref := ev.GetObservedAt()
	if ref == nil {
		ref = ev.GetIngestedAt()
	}
	return ref != nil && now.Sub(ref.AsTime()) > expireAfter
}

// recordAttempt writes source health, logging (not failing the tick) on
// store errors.
func (s *Scheduler) recordAttempt(ctx context.Context, src string, attemptErr error) {
	if err := s.store.RecordAttempt(ctx, src, attemptErr); err != nil {
		logging.Errorw(ctx, "Ingest tick: recording attempt failed", "source", src, "error", err)
	}
}
