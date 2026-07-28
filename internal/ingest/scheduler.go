package ingest

import (
	"context"
	"math/rand/v2"
	"runtime/debug"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// maxStartJitter spreads each poller's first tick over this window so a restart
// (or a rolling deploy's overlapping old+new tasks) doesn't hit every upstream
// at once — notably ArcGIS feature endpoints that 429 under a post-deploy burst.
// Wider than the old 15s so a fresh instance's first fetch is less likely to
// coincide with the draining instance's poll; still well under the poll
// intervals, so first data lands within ~90s of boot.
const maxStartJitter = 90 * time.Second

// PollerSpec pairs a normalizer with its poll interval. A normalizer spanning
// several sources (wildfire, road incidents) runs at one interval: the
// fastest of its sources' configured cadences.
type PollerSpec struct {
	Normalizer Normalizer
	Interval   time.Duration
}

// MeshMaintenance configures the periodic compaction + prune of the MeshCore
// relay-observation store (docs/mesh-topology-design.md): Tier 0 receptions are
// rolled into the Tier 1 per-link-per-day history, then aged rows are pruned. A
// zero Interval disables the whole tick (meshcore off / no topology store).
type MeshMaintenance struct {
	Interval             time.Duration // compaction + prune cadence (0 disables)
	ObservationRetention time.Duration // Tier 0 raw age cap
	RollupRetention      time.Duration // Tier 1 rollup age cap
}

// SchedulerConfig wires a Scheduler. Tuning keys are source ids; missing
// entries default to the resolve policy with no expire grace. A nil Enhancer
// (or a zero BudgetPerTick) disables weather-alert enhancement.
type SchedulerConfig struct {
	Pollers         []PollerSpec
	Tuning          map[string]config.SourceTuning
	Enhancer        NWSEnhancer
	EnhancerModel   string          // stamped into Enhancement.model on enhanced events
	BudgetPerTick   int             // max Enhance calls (attempts, not successes) per tick
	MeshMaintenance MeshMaintenance // relay-observation compaction + prune (Interval 0 = off)
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
	meshMaint     MeshMaintenance
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
		meshMaint:     cfg.MeshMaintenance,
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
	if s.meshMaint.Interval > 0 {
		go s.runMeshMaintenance(ctx)
	}
	logging.Infow(ctx, "Ingest scheduler started", "pollers", len(s.pollers),
		"meshMaintenance", s.meshMaint.Interval > 0)
}

// runMeshMaintenance ticks the relay-observation compaction + prune on its own
// interval. It shares the store's single writer (inTx takes the mutex), so it
// serializes with the pollers rather than being a second writer. First run is
// after one interval, keeping it off the boot burst.
func (s *Scheduler) runMeshMaintenance(ctx context.Context) {
	ticker := time.NewTicker(s.meshMaint.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.safeMeshMaintenance(ctx)
		}
	}
}

// safeMeshMaintenance runs one compaction + prune cycle, panic-isolated (a bad
// cycle must not kill the goroutine) and fully log-and-continue: topology
// maintenance is best-effort and never blocks presence ingest.
func (s *Scheduler) safeMeshMaintenance(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logging.Errorw(ctx, "Mesh maintenance: recovered from panic", "error", r)
		}
	}()
	now := s.now()
	compacted, err := s.store.CompactMeshObservations(ctx)
	if err != nil {
		logging.Errorw(ctx, "Mesh maintenance: compaction failed", "error", err)
	}
	prunedObs, err := s.store.PruneMeshObservations(ctx, now.Add(-s.meshMaint.ObservationRetention))
	if err != nil {
		logging.Errorw(ctx, "Mesh maintenance: pruning observations failed", "error", err)
	}
	prunedRollup, err := s.store.PruneMeshLinkRollup(ctx, now.Add(-s.meshMaint.RollupRetention))
	if err != nil {
		logging.Errorw(ctx, "Mesh maintenance: pruning rollup failed", "error", err)
	}
	logging.Infow(ctx, "Mesh maintenance tick", "compacted", compacted,
		"prunedObservations", prunedObs, "prunedRollup", prunedRollup)
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

	// (a) Poll, handing the normalizer the store's current active set so it
	// can keep identity/state stable across ticks. A hard error fails every
	// covered source and ends the tick.
	result, err := n.Poll(ctx, s.loadPrior(ctx, sourceIDs))
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
	var polledIDs []string
	for _, ev := range result.Events {
		src := ev.GetProvenance().GetSourceId()
		if ids, ok := polled[src]; ok {
			ids[ev.GetId()] = true
		}
		// Only ids from a source that fetched cleanly count as "confirmed seen".
		// A stale-served event (its source has a PerSource error, e.g. NWS down
		// and RawNWSAlerts returned the last-good list) was NOT confirmed this
		// tick — bumping its last_seen_at would reset the expire grace off a poll
		// that never reached the source, over-retaining an ended alert.
		if result.PerSource[src] == nil {
			polledIDs = append(polledIDs, ev.GetId())
		}
		s.maybeEnhance(ctx, ev, &budget)
		if _, err := s.store.UpsertEvent(ctx, ev); err != nil {
			logging.Errorw(ctx, "Ingest tick: upsert failed", "event", ev.GetId(), "error", err)
		}
	}

	// Every id a clean poll returned was just confirmed by its source —
	// including hash-equal no-op upserts, which write nothing. TouchSeen anchors
	// the expire grace to this confirmation, so a stable event that later drops
	// out of one poll is not expired instantly.
	if err := s.store.TouchSeen(ctx, polledIDs, now); err != nil {
		logging.Errorw(ctx, "Ingest tick: touch seen failed", "sources", sourceIDs, "error", err)
	}

	// Flush the mesh reception firehose (nil for every non-mesh poller). These are
	// measurements written to the append-only observation store, outside the
	// revisioned event path; a failure logs and continues — losing a tick of raw
	// telemetry never blocks presence ingest. Reached only on a successful poll
	// (a hard Poll error returned above), so our outage never writes stale rows.
	if len(result.MeshObservations) > 0 {
		if err := s.store.InsertMeshObservations(ctx, result.MeshObservations); err != nil {
			logging.Errorw(ctx, "Ingest tick: mesh observation insert failed",
				"count", len(result.MeshObservations), "error", err)
		}
	}

	// (c) Disappearance sweep — ONLY for sources whose fetch succeeded this
	// tick (PerSource errors mean that source's absence proves nothing) AND
	// that the poller did not suppress. Suppression is life-safety honesty:
	// a source can fetch cleanly yet be unable to prove disappearance (e.g.
	// wildfire can't compute the standalone-perimeter set while its sibling
	// feed is down) — sweeping anyway would turn a partial view into a false
	// all-clear, resolving hazards that are still burning.
	suppressed := make(map[string]bool, len(result.SweepSuppress))
	for _, src := range result.SweepSuppress {
		suppressed[src] = true
	}
	for _, src := range sourceIDs {
		if result.PerSource[src] != nil || suppressed[src] {
			continue
		}
		s.sweepDisappeared(ctx, src, polled[src], now)
	}

	// (d) Per-source health: nil for success, the partial error otherwise.
	// Sweep-suppressed sources still record success — their fetch worked;
	// only their lifecycle evidence was incomplete.
	for _, src := range sourceIDs {
		s.recordAttempt(ctx, src, result.PerSource[src])
	}
}

// loadPrior snapshots the store's active/scheduled events for the poller's
// sources. Store errors degrade to an empty view (logged): a normalizer can
// always call the Prior it is handed.
func (s *Scheduler) loadPrior(ctx context.Context, sourceIDs []string) Prior {
	p := &priorEvents{
		byID:     make(map[string]*gridv1.Event),
		bySource: make(map[string][]*gridv1.Event, len(sourceIDs)),
	}
	for _, src := range sourceIDs {
		stored, err := s.store.ActiveEventsBySource(ctx, src)
		if err != nil {
			logging.Warnw(ctx, "Ingest tick: loading prior events failed", "source", src, "error", err)
			continue
		}
		for _, se := range stored {
			p.byID[se.Event.GetId()] = se.Event
			p.bySource[src] = append(p.bySource[src], se.Event)
		}
	}
	return p
}

// priorEvents is the scheduler's Prior implementation. Nil-safe so tests
// (and any direct caller) can pass a zero value.
type priorEvents struct {
	byID     map[string]*gridv1.Event
	bySource map[string][]*gridv1.Event
}

func (p *priorEvents) ByID(id string) *gridv1.Event {
	if p == nil {
		return nil
	}
	return p.byID[id]
}

func (p *priorEvents) ForSource(sourceID string) []*gridv1.Event {
	if p == nil {
		return nil
	}
	return p.bySource[sourceID]
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
	enh, err := s.enhancer.Enhance(ctx, ev.GetHeadline(), ev.GetDescription(), s.placeNames(ctx, ev))
	if err != nil {
		logging.Warnw(ctx, "Ingest tick: enhancement failed; serving raw alert",
			"event", ev.GetId(), "error", err)
		return
	}
	ev.Summary = enh.Summary
	ev.Enhancement = &gridv1.Enhancement{
		Model:      s.enhancerModel,
		EnhancedAt: timestamppb.New(s.now()),
		Fields:     []string{"summary"},
		Request:    enh.Request,
		Response:   enh.Response,
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
	var disappeared []store.StoredEvent
	for _, se := range active {
		if !polledIDs[se.Event.GetId()] {
			disappeared = append(disappeared, se)
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
		for _, se := range disappeared {
			if shouldExpire(se, tuning.ExpireAfter, now) {
				ids = append(ids, se.Event.GetId())
			}
		}
	} else {
		for _, se := range disappeared {
			ids = append(ids, se.Event.GetId())
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

// shouldExpire implements the expire policy's time test. The grace is
// anchored to LastSeenAt — the last successful poll that included the event
// — NOT to observed/ingested times, which only move on content changes: a
// stable long-lived event dropping out of a single poll must not expire
// instantly. Rows that predate last-seen tracking (zero LastSeenAt) fall
// back to observed_at, then ingested_at, so the grace still terminates them.
func shouldExpire(se store.StoredEvent, expireAfter time.Duration, now time.Time) bool {
	if exp := se.Event.GetExpires(); exp != nil && now.After(exp.AsTime()) {
		return true
	}
	if expireAfter <= 0 {
		return false
	}
	ref := se.LastSeenAt
	if ref.IsZero() {
		if ts := se.Event.GetObservedAt(); ts != nil {
			ref = ts.AsTime()
		} else if ts := se.Event.GetIngestedAt(); ts != nil {
			ref = ts.AsTime()
		}
	}
	return !ref.IsZero() && now.Sub(ref) > expireAfter
}

// recordAttempt writes source health, logging (not failing the tick) on
// store errors.
func (s *Scheduler) recordAttempt(ctx context.Context, src string, attemptErr error) {
	if err := s.store.RecordAttempt(ctx, src, attemptErr); err != nil {
		logging.Errorw(ctx, "Ingest tick: recording attempt failed", "source", src, "error", err)
	}
}
