package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/calfire"
	"github.com/sierragridteam/sierra-data/internal/clients/calfireburn"
	"github.com/sierragridteam/sierra-data/internal/clients/caloes"
	"github.com/sierragridteam/sierra-data/internal/clients/firis"
	"github.com/sierragridteam/sierra-data/internal/clients/meshcore"
	"github.com/sierragridteam/sierra-data/internal/clients/pge"
	"github.com/sierragridteam/sierra-data/internal/clients/usgs"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	"github.com/sierragridteam/sierra-data/internal/ingest"
	"github.com/sierragridteam/sierra-data/internal/places"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The writer role. At most one process per database holds the writer lease
// (internal/store/lease.go); while it does (a "term") it seeds sources and
// places, runs the ingest scheduler, the MeshCore subscriber, push ingest's
// buffers and upstream cache warming. Every other process is a reader that
// campaigns for the lease (polling the advisory lock) and serves reads.
//
// A term ends when the lease is lost (its session died, or another process
// bumped the epoch: inTx then refuses every write), or when the process
// yields or quiesces. Either way the term's workers are stopped and waited
// for, the lease is released, and, after a loss, the process campaigns again
// with backoff.
//
// How the handoff controller's states map on it (docs/deploy-handoff.md):
// STANDBY = reader, not campaigning; ACTIVATING/ACTIVE = campaigning or
// holding (databaseHeld says which); QUIESCED = released and never
// campaigning again, still serving reads until SIGTERM.

const (
	campaignBackoffMin = time.Second
	campaignBackoffMax = 15 * time.Second
)

type writerRole struct {
	mu       sync.Mutex
	cancel   context.CancelFunc // ends the campaign loop
	loopDone chan struct{}      // closed when the loop has returned
	term     *writerTerm        // the term being run or stopped; nil between terms
}

// writerTerm is what one held lease runs. stopped tracks which teardown
// steps finished, so a failed stop resumes where it failed.
type writerTerm struct {
	lease     *store.Lease
	runCancel context.CancelFunc
	scheduler *ingest.Scheduler
	meshReg   *meshcore.Registry
	bg        sync.WaitGroup // background goroutines that read the store
	stopped   struct{ ingest, workers, mesh, bg, lease bool }
}

// DatabaseHeld reports whether this process holds the writer lease, or is
// still stopping a term that did (handoff.HoldReporter).
func (a *gridApp) DatabaseHeld() bool {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	return a.w.term != nil
}

// Activate is the controller's activation: start campaigning for the writer
// lease. At boot (no --standby) it returns at once and the campaign runs in
// the background for as long as it takes. Under a coordinator
// (strictActivate) it waits up to grid.database.lockTimeout for the lease and
// starts the term before returning, so ACTIVE means "holds the lease", and a
// failure leaves nothing running (back to STANDBY). With grid.writer.enabled
// false there is nothing to do: the process serves reads only.
func (a *gridApp) Activate(actx context.Context) (err error) {
	ctx := a.ctx
	// Everything below logs through actx too; carry the process logger on it
	// whatever the caller passed (a logger-less context panics in prefab's
	// logging, which once crash-looped production at the first mesh log line).
	actx = handoff.WithLoggerOf(actx, ctx)
	if err := a.Serve(actx); err != nil {
		return err
	}
	if !a.cfg.Grid.Writer.IsEnabled() {
		logging.Infow(ctx, "Writer disabled (grid.writer.enabled=false): serving reads only, never campaigning")
		return nil
	}
	a.w.mu.Lock()
	if a.w.cancel != nil {
		a.w.mu.Unlock()
		return nil // already campaigning or holding
	}
	a.w.mu.Unlock()

	var first *writerTerm
	if a.strictActivate {
		defer func() {
			// A panic is a failed activation like any other: release what was
			// opened rather than leave it running behind a "held" status.
			if r := recover(); r != nil {
				err = fmt.Errorf("panic during activation: %v\n%s", r, debug.Stack())
			}
			if err == nil {
				return
			}
			logging.Warnw(ctx, "Activation failed; releasing what it opened", "error", err)
			if qerr := a.Quiesce(ctx); qerr != nil {
				err = fmt.Errorf("%w (and releasing it failed: %w: %w)", err, handoff.ErrStillHeld, qerr)
			}
		}()
		lease, err := a.acquire(actx, a.cfg.Grid.Database.LockTimeout)
		if err != nil {
			return err
		}
		if first, err = a.startTerm(actx, lease); err != nil {
			return err
		}
	}
	a.startCampaign(first)
	return nil
}

// Quiesce stops the writer role for good (the controller never reactivates
// a QUIESCED process): stop campaigning, stop the term's workers, release
// the lease. Reads keep being served; the admission gate closes only at
// Exit. Each step runs once; a retry after a failure resumes where it failed.
func (a *gridApp) Quiesce(_ context.Context) error {
	start := time.Now()
	if err := a.stopWriter(); err != nil {
		return err
	}
	logging.Infow(a.ctx, "Quiesce: writer lease released; still serving reads", "duration", time.Since(start))
	return nil
}

// yield is the control API's "yield" command: release the lease and stop
// campaigning until "campaign" (or a restart). Reads continue. Useful to
// hand the writer to another task set ahead of its predecessor's SIGTERM
// (a release with a migration: the new code can only migrate as the writer).
func (a *gridApp) yield(_ context.Context, st handoff.State) error {
	start := time.Now()
	if err := a.stopWriter(); err != nil {
		return err
	}
	logging.Infow(a.ctx, "Yielded the writer role; not campaigning until told to", "state", st,
		"duration", time.Since(start))
	return nil
}

// campaign is the control API's "campaign" command: resume campaigning after
// a yield. Only in ACTIVE: a standby is activated with /activate, and a
// quiesced process never writes again.
func (a *gridApp) campaign(_ context.Context, st handoff.State) error {
	if st != handoff.StateActive {
		return fmt.Errorf("%w: campaign needs state ACTIVE (this process is %s; a standby takes POST /activate)",
			handoff.ErrCommandRefused, st)
	}
	if !a.cfg.Grid.Writer.IsEnabled() {
		return fmt.Errorf("%w: grid.writer.enabled is false", handoff.ErrCommandRefused)
	}
	a.startCampaign(nil)
	return nil
}

// startCampaign runs the campaign loop, beginning with first when the caller
// already holds a term. A no-op while a loop runs.
func (a *gridApp) startCampaign(first *writerTerm) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.w.cancel, a.w.loopDone = cancel, make(chan struct{})
	go a.campaignLoop(ctx, first, a.w.loopDone)
}

// campaignLoop holds the lease or campaigns for it until ctx ends. After a
// lost lease or a failed term start it stops the term and backs off before
// the next attempt.
func (a *gridApp) campaignLoop(ctx context.Context, t *writerTerm, done chan struct{}) {
	defer close(done)
	backoff := campaignBackoffMin
	for {
		if t == nil {
			logging.Infow(a.ctx, "Campaigning for the writer lease")
			lease, err := a.acquire(ctx, 0)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				t, err = a.safeStartTerm(ctx, lease)
			}
			if err != nil {
				logging.Errorw(a.ctx, "Writer term failed to start; will campaign again", "error", err, "retryIn", backoff)
				if serr := a.stopTerm(); serr != nil {
					logging.Errorw(a.ctx, "Stopping the failed term", "error", serr)
				}
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, campaignBackoffMax)
				continue
			}
		}
		began := time.Now()
		select {
		case <-ctx.Done():
			return // stopWriter tears the term down
		case <-t.lease.Lost():
		}
		logging.Errorw(a.ctx, "Writer lease LOST; stopping the writers and serving reads only",
			"epoch", t.lease.Epoch(), "error", t.lease.Err(), "heldFor", time.Since(began))
		if err := a.stopTerm(); err != nil {
			logging.Errorw(a.ctx, "Stopping the writers after a lost lease", "error", err)
		}
		t = nil
		if time.Since(began) > time.Minute {
			backoff = campaignBackoffMin
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, campaignBackoffMax)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// stopWriter ends the campaign and the current term, if any.
func (a *gridApp) stopWriter() error {
	a.w.mu.Lock()
	cancel, done := a.w.cancel, a.w.loopDone
	a.w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
		a.w.mu.Lock()
		a.w.cancel, a.w.loopDone = nil, nil
		a.w.mu.Unlock()
	}
	return a.stopTerm()
}

// acquire takes the writer lease (wait 0: until ctx ends).
func (a *gridApp) acquire(ctx context.Context, wait time.Duration) (*store.Lease, error) {
	a.mu.Lock()
	st := a.store
	a.mu.Unlock()
	if st == nil {
		return nil, errors.New("the grid store is not open")
	}
	start := time.Now()
	lease, err := st.AcquireLease(ctx, store.LeaseOptions{
		Holder:  a.holder(),
		Address: a.advertiseAddr(),
		Wait:    wait,
	})
	if err != nil {
		return nil, err
	}
	logging.Infow(a.ctx, "Writer lease acquired", "epoch", lease.Epoch(), "address", a.advertiseAddr(),
		"waited", time.Since(start))
	return lease, nil
}

func (a *gridApp) holder() string {
	h, _ := os.Hostname()
	return a.instanceID + "@" + h
}

// advertiseAddr is where other processes reach this one's production
// listener (fixed by Serve): grid.writer.advertiseAddr, else server.host (the
// hostname when it is a wildcard) and the port Serve bound.
func (a *gridApp) advertiseAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.advertise
}

func advertiseAddrFor(cfg *config.Config, port int) string {
	if addr := cfg.Grid.Writer.AdvertiseAddr; addr != "" {
		return addr
	}
	host := prefab.ConfigString("server.host")
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host, _ = os.Hostname()
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// safeStartTerm is startTerm with a panic turned into an error (the loop
// then stops whatever it started).
func (a *gridApp) safeStartTerm(ctx context.Context, lease *store.Lease) (t *writerTerm, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic starting the writer term: %v\n%s", r, debug.Stack())
		}
	}()
	return a.startTerm(ctx, lease)
}

// startTerm runs one held lease: seeding, then every background writer. The
// term is registered before anything starts, so stopTerm can always undo a
// partial start.
func (a *gridApp) startTerm(actx context.Context, lease *store.Lease) (*writerTerm, error) {
	ctx := a.ctx
	appConfig := a.cfg
	a.mu.Lock()
	gridStore := a.store
	a.mu.Unlock()
	runCtx, runCancel := context.WithCancel(ctx)
	t := &writerTerm{lease: lease, runCancel: runCancel}
	a.w.mu.Lock()
	a.w.term = t
	a.w.mu.Unlock()
	a.hook("lease-acquired")
	if err := actx.Err(); err != nil {
		return nil, err
	}

	seedStart := time.Now()
	if err := gridStore.SeedSources(actx, gridSourceSeeds(appConfig)); err != nil {
		return nil, fmt.Errorf("seed grid sources: %w", err)
	}
	retireOrphanedSources(actx, gridStore)
	if err := places.Seed(actx, gridStore, appConfig); err != nil {
		return nil, fmt.Errorf("seed grid places: %w", err)
	}
	logging.Infow(ctx, "Seeded grid sources and places", "duration", time.Since(seedStart))

	// One poller per upstream scope; weather_alert and road_incident reuse the
	// services' cached, budgeted pipelines (plan decision 6).
	roadIncidents := ingest.NewRoadIncidentNormalizer(appConfig, a.roadsService)
	wildfire := ingest.NewWildfireNormalizer(appConfig, calfire.NewClient(), firis.NewClient())
	wildfire.UseEndedFires(endedFires(gridStore))
	pollers := []ingest.PollerSpec{
		{Normalizer: ingest.NewEarthquakeNormalizer(appConfig, usgs.NewClient()), Interval: gridPollInterval(appConfig, "usgs")},
		{Normalizer: wildfire, Interval: gridPollInterval(appConfig, "calfire", "firis")},
		{Normalizer: ingest.NewEvacuationNormalizer(appConfig, caloes.NewClient()), Interval: gridPollInterval(appConfig, "caloes")},
		{Normalizer: ingest.NewWeatherAlertNormalizer(appConfig, a.weatherService), Interval: gridPollInterval(appConfig, "nws")},
		// chp alone, or chp + caltrans when lane closures still come from
		// lcs2way.kml (see RoadIncidentNormalizer.SourceIDs).
		{Normalizer: roadIncidents, Interval: gridPollInterval(appConfig, roadIncidents.SourceIDs()...)},
		{Normalizer: ingest.NewPowerNormalizer(appConfig, pge.NewClient()), Interval: gridPollInterval(appConfig, "pge", "psps")},
	}
	// Caltrans lane closures from CWWP2, scheduled windows included. Its own
	// poller on the caltrans interval rather than the CHP one: the two district
	// files are ~5 MB together, uncompressed, and the portal does not gzip.
	if len(appConfig.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts) > 0 {
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewLaneClosureNormalizer(appConfig, a.cwClient),
			Interval:   gridPollInterval(appConfig, "caltrans"),
		})
	}

	// Authenticated push ingest (optional): operator-run monitors POST to
	// /api/v1/ingest/{stream}. The registry (its buffers, rate limits and
	// reporter health) lives for one term; while it does, the ingest route
	// serves it here and every other process forwards to this one.
	burnLineIDs := make([]string, 0, len(appConfig.Grid.Burn.Lines))
	for _, l := range appConfig.Grid.Burn.Lines {
		burnLineIDs = append(burnLineIDs, l.ID)
	}
	// An accepted burn reading wakes the burn poller, so the event shows it
	// right away instead of on the next hourly tick.
	burnWake, wakeBurn := ingest.NewWake()
	pushRegistry, err := pushingest.NewRegistry(appConfig.Grid.Ingest,
		pushingest.WithBurnLines(gridStore, burnLineIDs),
		pushingest.WithBurnAccepted(wakeBurn))
	if err != nil {
		return nil, fmt.Errorf("invalid grid.ingest configuration: %w", err)
	}
	for _, id := range pushRegistry.MissingTokens() {
		logging.Warnw(ctx, "Push reporter has no token hash; nothing can authenticate as it",
			"reporter", id, "env", "PF__GRID__INGEST__REPORTER_TOKEN_SHA256__"+strings.ToUpper(id))
	}
	// Reporter health is in-memory; without this every term start reports the
	// once-a-day burn-line reporter as never having reported until its next run.
	if err := pushRegistry.RestoreBurnHealth(actx); err != nil {
		logging.Warnw(ctx, "Could not restore burn reporter health", "error", err)
	}

	// Burn status (optional): per-county residential burning status from two
	// independent authorities — the county air district's burn line and CAL
	// FIRE's seasonal suspension. Ambient state, polled a couple of times a day.
	if len(appConfig.Grid.Burn.Counties) > 0 {
		// The burn-day facet is PUSHED (the `burn.line` stream on
		// /api/v1/ingest): the normalizer reads the staging table the push
		// handler writes, rather than fetching an upstream. CAL FIRE is still a
		// real fetch.
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewBurnStatusNormalizer(appConfig, calfireburn.NewClient(), gridStore,
				storePlaceIndex{gridStore}, burnReporters(pushRegistry)),
			Interval: gridPollInterval(appConfig, "burnline", "calfire-burn"),
			Wake:     burnWake,
		})
	}

	// Mesh-node presence (optional). TWO possible inputs, and the poller runs if
	// EITHER is configured: the MeshCore MQTT subscriber (community bridges,
	// broadcast adverts) and the push-ingest buffer (operator monitors reading
	// repeater admin interfaces). The NetworkNormalizer merges them into one event
	// per node on the scheduler's tick.
	//
	// The MeshCore registry itself has two doors: the MQTT brokers, and the
	// mesh.packet push stream, through which an operator's companion radio
	// forwards the raw adverts it hears (a companion in Arnold hears SIERRA
	// nodes no MQTT observer does). Either door justifies the registry; only
	// the brokers need a connection.
	var meshcoreReg ingest.MeshRegistry
	mqttEnabled := appConfig.Grid.Meshcore.Enabled && len(appConfig.Grid.Meshcore.Brokers) > 0
	packetReporters := pushRegistry.ReporterIDs(pushingest.MeshPacketStream)
	if mqttEnabled || len(packetReporters) > 0 {
		reg := meshcore.NewRegistry(meshcoreClientConfig(appConfig))
		// Rehydrate presence from the persisted store BEFORE connecting, so a
		// new term doesn't drop the whole mesh to "unknown" (and let the sweep
		// expire the slow SIERRA repeaters) until every node re-adverts.
		if seedMeshRegistry(actx, reg, gridStore) {
			// The neighbour graph is rebuilt in the background: replaying 48h
			// of receptions measured 14.4s over EFS, and nothing needs it
			// first. It reads the store, so it is a background database user:
			// stopTerm cancels it and waits.
			t.bg.Add(1)
			go func() {
				defer t.bg.Done()
				seedMeshNeighbours(runCtx, reg, gridStore)
			}()
		}
		a.w.mu.Lock()
		t.meshReg = reg
		a.w.mu.Unlock()
		if mqttEnabled {
			if err := reg.Connect(runCtx); err != nil {
				logging.Errorw(ctx, "Failed to start MeshCore subscriber", "error", err)
			} else {
				logging.Infow(ctx, "MeshCore subscriber started", "brokers", len(appConfig.Grid.Meshcore.Brokers))
			}
		}
		if len(packetReporters) > 0 {
			pushRegistry.SetPacketSink(reg)
			logging.Infow(ctx, "MeshCore packet forwarding enabled", "reporters", packetReporters)
		}
		meshcoreReg = reg
	}
	var meshPush ingest.MeshPushSource
	if pushRegistry.Enabled() {
		meshPush = pushRegistry
		logging.Infow(ctx, "Push ingest enabled",
			"reporters", pushRegistry.ReporterIDs(pushingest.MeshStream),
			"packetReporters", packetReporters)
	}
	if meshcoreReg != nil || meshPush != nil {
		meshSources := []string{"meshcore"}
		seen := map[string]bool{"meshcore": true}
		for _, id := range append(pushRegistry.ReporterIDs(pushingest.MeshStream), packetReporters...) {
			if !seen[id] {
				seen[id] = true
				meshSources = append(meshSources, id)
			}
		}
		pollers = append(pollers, ingest.PollerSpec{
			Normalizer: ingest.NewNetworkNormalizer(appConfig, meshcoreReg, meshPush).
				WithCatalog(func(ctx context.Context) ([]*gridv1.Event, error) {
					return gridStore.EventsBySourceAndStatus(ctx, "meshcore", gridv1.EventStatus_EXPIRED)
				}),
			Interval: gridPollInterval(appConfig, meshSources...),
		})
	}
	if err := actx.Err(); err != nil {
		return nil, err
	}

	// The scheduler (and its per-poller reconcile memory) is built per term:
	// what it remembers is only valid while this process is the writer.
	scheduler := ingest.NewScheduler(gridStore, ingest.SchedulerConfig{
		Pollers:         pollers,
		Tuning:          appConfig.Grid.Sources,
		Enhancer:        a.nwsEnhancer,
		EnhancerModel:   a.model,
		BudgetPerTick:   appConfig.Grid.Enhancement.BudgetPerTick,
		MeshMaintenance: meshMaintenanceConfig(appConfig),
		RoadSampling:    roadSamplingConfig(a.roadsService),
	})
	scheduler.Start(runCtx)
	a.w.mu.Lock()
	t.scheduler = scheduler
	a.w.mu.Unlock()

	// Upstream cache warming belongs to the writer: N readers warming their
	// own caches would multiply the Google Routes / OpenWeather spend. A
	// reader's caches fill on demand.
	periodicRefresh := services.NewPeriodicRefreshService(a.roadsService, a.weatherService, appConfig)
	if err := periodicRefresh.StartPeriodicRefresh(runCtx); err != nil {
		logging.Errorw(ctx, "Failed to start periodic refresh", "error", err)
	}

	if a.ingest != nil && pushRegistry.Enabled() {
		a.ingest.setLocal(pushRegistry)
	}
	logging.Infow(ctx, "Writer term started", "epoch", lease.Epoch(),
		"sinceProcessStart", time.Since(processStart))
	return t, nil
}

func (a *gridApp) hook(stage string) {
	if a.activateHook != nil {
		a.activateHook(stage)
	}
}

// stopTerm tears the current term down: push ingest stops landing here,
// the scheduler stops, MQTT disconnects, the remaining background work is
// cancelled and awaited, and the lease is released (the store stays open as
// a reader). One budget for every worker (grid.control.workerStopTimeout), so
// a SIGTERM's quiescence stays inside ECS's stop timeout. A failure leaves the
// term registered (DatabaseHeld true); a retry resumes at the failed step.
func (a *gridApp) stopTerm() error {
	a.w.mu.Lock()
	t := a.w.term
	a.w.mu.Unlock()
	if t == nil {
		return nil
	}
	ctx := a.ctx
	ctl := a.cfg.Grid.Control
	workersDeadline := time.Now().Add(ctl.WorkerStop())

	if !t.stopped.ingest {
		if a.ingest != nil {
			a.ingest.setLocal(nil)
		}
		t.stopped.ingest = true
	}
	if !t.stopped.workers {
		if t.scheduler != nil {
			start := time.Now()
			if err := t.scheduler.Stop(2*time.Second, max(time.Until(workersDeadline)-2*time.Second, 0)); err != nil {
				logging.Errorw(ctx, "Writer stop: ingest scheduler did not stop", "error", err, "duration", time.Since(start))
				return err
			}
			logging.Infow(ctx, "Writer stop: ingest scheduler stopped", "duration", time.Since(start))
		}
		t.stopped.workers = true
	}
	if !t.stopped.mesh {
		if t.meshReg != nil {
			start := time.Now()
			t.meshReg.Close()
			logging.Infow(ctx, "Writer stop: MeshCore subscriber closed", "duration", time.Since(start))
		}
		t.stopped.mesh = true
	}
	if !t.stopped.bg {
		start := time.Now()
		t.runCancel()
		bgDone := make(chan struct{})
		go func() { t.bg.Wait(); close(bgDone) }()
		if !handoff.WaitClosed(bgDone, max(time.Until(workersDeadline), 100*time.Millisecond)) {
			err := fmt.Errorf("background store readers still running %s after cancellation", ctl.WorkerStop())
			logging.Errorw(ctx, "Writer stop: background work did not stop", "error", err)
			return err
		}
		logging.Infow(ctx, "Writer stop: background work stopped", "duration", time.Since(start))
		t.stopped.bg = true
	}
	if !t.stopped.lease {
		a.mu.Lock()
		st := a.store
		a.mu.Unlock()
		start := time.Now()
		if err := st.ReleaseLease(); err != nil {
			// The lease connection is closed either way (closing it is what
			// releases the lock server-side); report, and count it released.
			logging.Warnw(ctx, "Writer stop: releasing the lease reported an error", "error", err)
		}
		logging.Infow(ctx, "Writer stop: lease released", "epoch", t.lease.Epoch(), "duration", time.Since(start))
		t.stopped.lease = true
	}
	a.w.mu.Lock()
	if a.w.term == t {
		a.w.term = nil
	}
	a.w.mu.Unlock()
	return nil
}
