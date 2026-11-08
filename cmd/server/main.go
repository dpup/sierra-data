package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Embed the IANA tz database so America/Los_Angeles resolves in minimal containers

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/caloes"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/clients/meshcore"
	"github.com/sierragridteam/sierra-data/internal/clients/pge"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	"github.com/sierragridteam/sierra-data/internal/ingest"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The Grid's lifecycle is internal/handoff's: prepare (no database), then
// ACTIVE either at boot or, with --standby, when a deployment coordinator
// says so; SIGTERM in any state quiesces before exit.
//
//	sierra-server             # activate at boot (the default, as always)
//	sierra-server --standby   # prepare, then wait on the control port for POST /activate
func main() {
	standby := flag.Bool("standby", false,
		"start in STANDBY: prepare without opening grid.db and wait for POST /activate on the control port")
	flag.Parse()

	// Initialize structured logging
	logger := logging.NewProdLogger()
	ctx := logging.With(context.Background(), logger)

	logging.Infow(ctx, "Starting The Grid (S.I.E.R.R.A data service)", "standby", *standby)

	// Load configuration using Prefab's config system
	appConfig := config.LoadConfig()

	// Tell prefab's config validator that the app's own config namespaces are
	// known, so it doesn't warn about every roads/weather/grid/... key as a
	// "potential typo". Prefab's own server.* keys stay validated, so real typos
	// there still surface. Must run before prefab.New (which validates on build).
	config.RegisterPrefabNamespaces()

	app, err := prepareGrid(ctx, appConfig)
	if err != nil {
		logging.Errorw(ctx, "Grid preparation failed", "error", err)
		log.Fatalf("Grid preparation failed: %v", err)
	}

	opts := handoff.Options{Service: "grid", Standby: *standby}
	if ctl := appConfig.Grid.Control; ctl.Enabled() {
		opts.Addr = fmt.Sprintf(":%d", ctl.Port)
		opts.TokenSha256 = ctl.TokenSha256
	}
	ctrl, err := handoff.New(ctx, app, opts)
	if err != nil {
		logging.Errorw(ctx, "Invalid deployment control configuration", "error", err)
		log.Fatalf("Invalid deployment control configuration: %v", err)
	}
	app.onFatal = ctrl.Fail
	os.Exit(ctrl.Run(ctx))
}

// deferredHandler is an http.Handler whose delegate is set after construction.
// MCP needs the gRPC-Gateway mux, but that mux only exists once prefab.New wires
// the gateway (in the WithGRPCGateway callback) — later than mcp.NewHandler runs.
// Until the delegate is set, requests fail loud rather than silently 404.
type deferredHandler struct{ h http.Handler }

func (d *deferredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.h == nil {
		http.Error(w, "gateway not ready", http.StatusServiceUnavailable)
		return
	}
	d.h.ServeHTTP(w, r)
}

// retiredSourceIDs are source ids no poller declares anymore. The disappearance
// sweep only runs for a live poller's SourceIDs(), so ACTIVE events left behind by
// a removed/renamed source (e.g. the standalone "wfigs:" perimeters after the
// wfigs→firis swap) would otherwise persist ACTIVE forever — stale duplicates on
// the fire map beside the fresh "firis:" events. Retire them once at boot.
// Entries may be dropped after a deploy has drained them (the transition is
// idempotent — once expired, ActiveEventsBySource returns none).
var retiredSourceIDs = []string{"wfigs"}

// retireOrphanedSources expires any ACTIVE/SCHEDULED events still attached to a
// retired source (a proper EXPIRED revision, history preserved) and then removes
// the defunct source registry row so /api/v1/sources doesn't list it forever.
// Best effort: a failure logs and continues — it must never block startup.
// Idempotent: once drained, ActiveEventsBySource returns none and the delete is a
// no-op.
func retireOrphanedSources(ctx context.Context, st *store.Store) {
	for _, src := range retiredSourceIDs {
		evs, err := st.ActiveEventsBySource(ctx, src)
		if err != nil {
			logging.Warnw(ctx, "Could not scan retired source for orphaned events", "source", src, "error", err)
			continue
		}
		if len(evs) > 0 {
			ids := make([]string, len(evs))
			for i, e := range evs {
				ids[i] = e.Event.GetId()
			}
			if err := st.TransitionEvents(ctx, ids, gridv1.EventStatus_EXPIRED, time.Now()); err != nil {
				logging.Warnw(ctx, "Could not retire orphaned events for retired source", "source", src, "count", len(ids), "error", err)
				continue
			}
			logging.Infow(ctx, "Retired orphaned events for a removed source", "source", src, "count", len(ids))
		}
		if err := st.DeleteSource(ctx, src); err != nil {
			logging.Warnw(ctx, "Could not remove retired source registry row", "source", src, "error", err)
		}
	}
}

// gridSourceInfo is the static registry of source display names, attributions
// and upstream homepages.
//
// These describe THE FEED, which is a different question from an event's
// provenance, and conflating the two is what made the sources board read as
// duplicated. An event's Provenance credits every source that contributed to
// THAT event — a `calfire:` incident carrying an adopted FIRIS perimeter is
// legitimately attributed "CAL FIRE / FIRIS" — whereas the row below describes
// only the CAL FIRE active-incidents feed, which is not FIRIS (that has its own
// row). The two must agree on WHO a source is; they need not carry the same
// string, and where they differ the divergence is deliberate.
//
// Names disambiguate feeds that share an upstream. calfire/firis, pge/psps and
// chp/caltrans are each one poller covering two source rows, so every other cell
// on the board — poll interval, thresholds, last success to the second — is
// identical within a pair; the name is the only thing that can tell them apart.
// Keep the organization recognizable and put the feed's subject in parentheses.
var gridSourceInfo = map[string]struct{ name, attribution, homepage string }{
	"usgs":     {"USGS Earthquakes", "U.S. Geological Survey", "https://earthquake.usgs.gov/earthquakes/map/"},
	"calfire":  {"CAL FIRE (active incidents)", "CAL FIRE", "https://incidents.fire.ca.gov"},
	"firis":    {"FIRIS (fire perimeters)", "CAL FIRE / FIRIS / NIFC", firisProgramURL},
	"caloes":   {"Cal OES (evacuation zones)", "Cal OES — reference only", caloes.SourceURL},
	"nws":      {"National Weather Service (Sacramento)", "NOAA / National Weather Service", "https://www.weather.gov/sto"},
	"chp":      {"CHP (traffic incidents)", "quickmap.dot.ca.gov", quickMapURL},
	"caltrans": {"Caltrans (chain control + lane closures)", caltrans.SourceQuickMap, quickMapURL}, // attribution: see caltransAttribution
	"meshcore": {"MeshCore Mesh", "MeshCore community mesh", "https://map.meshcore.io"},
	"pge":      {"PG&E (electric outages)", "Pacific Gas and Electric", pge.OutageMapURL},
	"psps":     {"PG&E (public safety power shutoffs)", "Pacific Gas and Electric", pge.PSPSUpdatesURL},
	// Two authorities gate a legal burn and both are carried; see
	// internal/ingest/burn_status.go. The burn line row covers every configured
	// county's line, so its name stays generic.
	"burnline":     {"County burn line (burn day)", "County air district burn information line", burnDayHomepage},
	"calfire-burn": {"CAL FIRE (burn permit suspension)", "CAL FIRE", calfireBurnHomepage},
}

// caltransAttribution names the feeds actually behind the caltrans row, which
// depend on config. Any CWWP2 use means both feeds: CWWP2 chain-control levels
// are MERGED with cc.kml (never replace it), and CWWP2 lane closures leave
// chain control on cc.kml when its own districts are unset. The row's
// attribution is also the chain_control layer's metadata.attribution (via
// gridapi.conditionLayerSourceIDs), which said quickmap alone after CWWP2
// started supplying the levels.
func caltransAttribution(cfg *config.Config) string {
	cw := cfg.Roads.CaltransFeeds.CWWP2
	if len(cw.ChainControlDistricts) == 0 && len(cw.LaneClosureDistricts) == 0 {
		return caltrans.SourceQuickMap
	}
	return caltrans.SourceCWWP2 + " · " + caltrans.SourceQuickMap
}

// Homepages used by more than one source row, or that have no constant of their
// own in a client package. Human-facing pages — the site links the source name
// at them — not the machine endpoints the clients fetch.
const (
	quickMapURL = "https://quickmap.dot.ca.gov/"
	// FIRIS data reaches us through an ArcGIS feature service with no landing
	// page; Cal OES runs the program, so its page is the honest upstream.
	firisProgramURL = "https://www.caloes.ca.gov/office-of-the-director/operations/response-operations/fire-rescue/firis/"
	// The public page for the burn-day readings we consume. The AUTHORITY is the
	// county's recorded phone line, which has no URL — each event carries the
	// number in burn_status.burnLinePhone so a reader can always reach it.
	burnDayHomepage = "https://burnday.ersn.net"
	// CAL FIRE's own burn status table.
	calfireBurnHomepage = "https://burnpermit.fire.ca.gov/current-burn-status"
)

// meshcoreClientConfig maps the grid.meshcore config onto the meshcore client
// Config. RetainFor is anchored to the source's expireAfter so the store's
// lifecycle — not the in-memory buffer — decides when a silent node is gone.
func meshcoreClientConfig(cfg *config.Config) meshcore.Config {
	mc := cfg.Grid.Meshcore
	brokers := make([]meshcore.Broker, 0, len(mc.Brokers))
	for _, b := range mc.Brokers {
		// Per-broker creds win; otherwise fall back to the shared subscriber
		// credentials (env-injected via PF__GRID__MESHCORE__USERNAME/PASSWORD).
		user, pass := b.Username, b.Password
		if user == "" {
			user = mc.Username
		}
		if pass == "" {
			pass = mc.Password
		}
		brokers = append(brokers, meshcore.Broker{
			URL:      b.URL,
			ClientID: b.ClientID,
			Username: user,
			Password: pass,
			Topics:   b.Topics,
			QoS:      b.QoS,
		})
	}
	// SpamFloor guards the relay-observation store from a fast-adverting node;
	// default 30s when unset (0 in yaml is treated as the default, not "disable" —
	// direct Config construction in tests can still disable it with a negative).
	spamFloor := mc.SpamFloor
	if spamFloor <= 0 {
		spamFloor = 30 * time.Second
	}
	graceCeil := mc.GraceCeil
	if graceCeil <= 0 {
		graceCeil = 72 * time.Hour
	}
	// RetainFor (in-memory node retention) is anchored to the presence ceiling, NOT
	// the source's expireAfter: expireAfter is now the short disappearance-sweep
	// safety net, while a node must live in memory for its whole cadence-derived
	// presence window (up to GraceCeil). NewRegistry defaults the remaining knobs.
	return meshcore.Config{
		Brokers:               brokers,
		RequireValidSignature: mc.RequireValidSignature,
		RetainFor:             graceCeil,
		SpamFloor:             spamFloor,
		CadenceK:              mc.CadenceK,
		GraceFloor:            mc.GraceFloor,
		GraceCeil:             graceCeil,
		SilenceAfter:          mc.Silence(),
		RelayPresence:         mc.RelayPresenceWindow(),
	}
}

// meshMaintenanceConfig maps the grid.meshcore config onto the scheduler's
// relay-topology maintenance tick (compaction + prune). A disabled meshcore
// source returns a zero config (Interval 0), which turns the tick off entirely.
// Cadence/retention default when unset (docs/design/mesh-topology-design.md §10).
func meshMaintenanceConfig(cfg *config.Config) ingest.MeshMaintenance {
	mc := cfg.Grid.Meshcore
	// The tick also owns telemetry-archive pruning, and telemetry arrives by
	// PUSH — which works with MQTT off entirely. Gating the whole tick on the
	// MQTT subscriber would leave a push-only deployment archiving samples that
	// nothing ever prunes. Compaction over an empty observation table is a no-op,
	// so running it for a push-only deployment costs nothing.
	if (!mc.Enabled || len(mc.Brokers) == 0) && len(cfg.Grid.Ingest.Reporters) == 0 {
		return ingest.MeshMaintenance{}
	}
	interval := mc.CompactionInterval
	if interval <= 0 {
		interval = time.Hour
	}
	obsRetention := mc.ObservationRetention
	if obsRetention <= 0 {
		obsRetention = 48 * time.Hour
	}
	rollupRetention := mc.RollupRetention
	if rollupRetention <= 0 {
		rollupRetention = 2 * 365 * 24 * time.Hour
	}
	telemetryRetention := mc.TelemetryRetention
	if telemetryRetention <= 0 {
		telemetryRetention = 365 * 24 * time.Hour
	}
	return ingest.MeshMaintenance{
		Interval:             interval,
		ObservationRetention: obsRetention,
		RollupRetention:      rollupRetention,
		TelemetryRetention:   telemetryRetention,
	}
}

// bootStatsRefreshDelay is when the first background ANALYZE runs after a boot
// that kept the persisted statistics: late enough to stay off the first ticks'
// full reconcile, soon enough that frequent deploys never leave them stale for
// long. The refresh holds the store's writer mutex for its whole run.
const bootStatsRefreshDelay = 10 * time.Minute

// processStart anchors the boot-duration log line.
var processStart = time.Now()

// seedMeshRegistry rehydrates the MeshCore Registry from the persisted store so
// node presence survives a restart. It seeds every ACTIVE/SCHEDULED network node
// (identity + location + last-seen) and replays each node's recent advert times
// (from the observation store) to reconstruct its cadence, so the per-node
// presence window is right immediately after boot. Best-effort: a store read
// failure logs and leaves the Registry to refill from live adverts (the prior
// behavior), never blocks startup. It reports whether any node was seeded.
//
// This half stays synchronous, before Connect and before the scheduler's first
// mesh tick: a snapshot missing the seeded nodes would let the sweep start
// expiring the slow SIERRA repeaters.
func seedMeshRegistry(ctx context.Context, reg *meshcore.Registry, st *store.Store) bool {
	start := time.Now()
	events, err := st.ActiveEventsBySource(ctx, "meshcore")
	if err != nil {
		logging.Warnw(ctx, "MeshCore: presence rehydration skipped — loading active nodes failed", "error", err)
		return false
	}
	if len(events) == 0 {
		return false
	}
	heard, err := st.MeshNodeHeardTimes(ctx, 40)
	if err != nil {
		logging.Warnw(ctx, "MeshCore: cadence rehydration skipped — reading heard times failed", "error", err)
		heard = nil
	}
	// The activity clocks, so relay presence and "last heard" pick up where the
	// last process left off rather than re-accumulating from nothing.
	keys := make([]string, 0, len(events))
	for _, se := range events {
		if pk := se.Event.GetMesh().GetPublicKey(); pk != "" {
			keys = append(keys, pk)
		}
	}
	activity, err := st.MeshActivityFor(ctx, keys)
	if err != nil {
		logging.Warnw(ctx, "MeshCore: activity rehydration skipped — reading mesh activity failed", "error", err)
		activity = nil
	}
	seeds := make([]meshcore.SeedNode, 0, len(events))
	for _, se := range events {
		d := se.Event.GetMesh()
		pk := d.GetPublicKey()
		if pk == "" {
			continue
		}
		sn := meshcore.SeedNode{
			PubKey:     pk,
			Role:       d.GetNodeType(),
			Name:       d.GetName(),
			HeardTimes: heard[pk],
			LastHeard:  se.LastSeenAt,
		}
		if a, ok := activity[strings.ToLower(pk)]; ok {
			sn.LastReceived, sn.LastRelayed = a.LastHeardAt, a.LastRelayedAt
		}
		if c := se.Event.GetGeometry().GetCentroid(); c != nil {
			sn.HasLocation = true
			sn.Lat, sn.Lng = c.GetLat(), c.GetLng()
		}
		if sn.LastHeard.IsZero() {
			if ts := se.Event.GetObservedAt(); ts != nil {
				sn.LastHeard = ts.AsTime()
			}
		}
		seeds = append(seeds, sn)
	}
	reg.Seed(seeds)
	logging.Infow(ctx, "MeshCore: rehydrated node presence from store",
		"nodes", len(seeds), "duration", time.Since(start))
	return len(seeds) > 0
}

// seedMeshNeighbours rebuilds the neighbour graph that resolves 1-byte relay
// hops (meshcore/relay.go), so a restart does not stop crediting half the
// traffic until it re-learns. Rebuilt from the raw receptions (Tier 0,
// observationRetention) through the live rule, NOT from the link rollup: rollup
// edges were resolved by the older rule that matches 1-byte hops on prefix,
// which is exactly the misattribution the graph exists to avoid.
//
// It runs in the background, after the listener is up. That is safe because
// SeedPaths and live learning share adjMu and an edge keeps its latest time,
// so the order they interleave in does not matter. Until it finishes, a 1-byte
// hop that only the stored graph could resolve goes uncredited: relay credit
// for those nodes resumes a few seconds late, and their presence is already
// held by the synchronous seed above. Mesh presence is ambient INFO.
func seedMeshNeighbours(ctx context.Context, reg *meshcore.Registry, st *store.Store) {
	start := time.Now()
	obs, err := st.MeshObservationPaths(ctx, time.Now().Add(-meshcore.AdjacencyRetention))
	if err != nil {
		logging.Warnw(ctx, "MeshCore: neighbour-graph rehydration skipped — reading receptions failed", "error", err)
		return
	}
	paths := seedPaths(obs)
	reg.SeedPaths(paths, nil)
	logging.Infow(ctx, "MeshCore: rehydrated neighbour graph from store",
		"paths", len(paths), "duration", time.Since(start))
}

// seedPaths turns stored advert receptions into neighbour-graph seed paths.
// A stored gateway equal to the broker is the broker-URL fallback, which every
// radio on that broker shares, so it is no anchor (see meshcore.relayObserver).
func seedPaths(obs []store.MeshObservation) []meshcore.SeedPath {
	paths := make([]meshcore.SeedPath, 0, len(obs))
	for _, o := range obs {
		sp := meshcore.SeedPath{Hops: o.Path, Observer: o.Gateway, Sender: o.PubKey, At: o.HeardAt}
		if o.Gateway == o.Broker {
			sp.Observer = ""
		}
		paths = append(paths, sp)
	}
	return paths
}

// gridSourceSeeds builds the source registry rows: ids + tuning from
// grid.sources config, names/attributions from the static registry (a
// config id without a registry entry is seeded with its id as the name so it
// still shows up in /api/v1/sources rather than failing silently).
func gridSourceSeeds(cfg *config.Config) []store.SourceSeed {
	ids := make([]string, 0, len(cfg.Grid.Sources))
	for id := range cfg.Grid.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic seeding order

	seeds := make([]store.SourceSeed, 0, len(ids)+len(cfg.Grid.Ingest.Reporters))
	for _, id := range ids {
		tuning := cfg.Grid.Sources[id]
		info, ok := gridSourceInfo[id]
		if !ok {
			info.name = id
		}
		if id == "caltrans" {
			info.attribution = caltransAttribution(cfg)
		}
		seeds = append(seeds, store.SourceSeed{
			ID:            id,
			Name:          info.name,
			Attribution:   info.attribution,
			HomepageURL:   info.homepage,
			PollInterval:  tuning.PollInterval,
			StaleAfter:    tuning.StaleAfter,
			ExpireAfter:   tuning.ExpireAfter,
			Disappearance: tuning.Disappearance,
		})
	}

	// One row per push reporter, so /api/v1/sources answers "is Alan's monitor
	// still reporting?" exactly the way it answers that question for CAL FIRE or
	// PG&E. These rows carry HEALTH ONLY — no event is ever stored with a reporter
	// as its source (mesh events all stay on `meshcore`), so their disappearance
	// sweep has nothing to act on.
	//
	// PollInterval is the cadence of the poller that re-evaluates this row's
	// health (reporterCheckInterval), not how often the monitor reports — a push
	// source has no poll interval of ours to state. StaleAfter is the reporter's
	// own silence threshold rather than the store's 3x default.
	for _, rep := range cfg.Grid.Ingest.Reporters {
		name := rep.Name
		if name == "" {
			name = rep.ID
		}
		seeds = append(seeds, store.SourceSeed{
			ID:            rep.ID,
			Name:          name,
			Attribution:   "Operator-reported (" + rep.ID + ")",
			Kind:          gridv1.SourceKind_REPORTER,
			PollInterval:  reporterCheckInterval(cfg, rep),
			StaleAfter:    rep.StaleAfterOrDefault(),
			Disappearance: store.DisappearanceExpire,
		})
	}
	return seeds
}

// reporterCheckInterval is how often a reporter's health is re-evaluated: the
// cadence of the poller that reads its stream. Mesh streams are merged by the
// mesh poller and burn.line by the burn-status poller, which ticks hourly — so
// stating the mesh tick for burn-line told clients its row was checked every
// minute. A reporter on several streams is checked by whichever ticks first.
func reporterCheckInterval(cfg *config.Config, rep config.Reporter) time.Duration {
	var best time.Duration
	for _, stream := range rep.Streams {
		var d time.Duration
		switch stream {
		case pushingest.BurnStream:
			d = gridPollInterval(cfg, "burnline", "calfire-burn")
		default: // mesh.repeater, mesh.packet
			d = gridPollInterval(cfg, "meshcore")
		}
		if best == 0 || d < best {
			best = d
		}
	}
	if best == 0 {
		return gridPollInterval(cfg, "meshcore")
	}
	return best
}

// gridPollInterval is a poller's cadence: the fastest configured interval
// among the sources it covers (a poller may span several source rows),
// defaulting to 5m when none is configured.
func gridPollInterval(cfg *config.Config, sourceIDs ...string) time.Duration {
	var best time.Duration
	for _, id := range sourceIDs {
		if t, ok := cfg.Grid.Sources[id]; ok && t.PollInterval > 0 {
			if best == 0 || t.PollInterval < best {
				best = t.PollInterval
			}
		}
	}
	if best == 0 {
		return 5 * time.Minute
	}
	return best
}

// storePlaceIndex resolves a county's place ids from the seeded place directory,
// satisfying ingest.PlaceIndex.
type storePlaceIndex struct{ st *store.Store }

// PlaceIDsForCounty returns the county place plus every place the county's
// status is true of: towns (parented to their containing county by the seeder)
// and any AREA whose coverage overlaps it.
//
// This is what makes a burn status attach without geometry. A query for a town
// ("?place=arnold") must see its county's burn status — the status is true of
// the whole county by definition, so resolving membership once at ingest is
// both cheaper and more accurate than writing an 18-32 KB county polygon into
// every event and every revision of it.
//
// AREAs need the geometric test because they are deliberately NOT parented to a
// county: the coverage footprint spans several (that is why AREA is its own
// place kind). Without this the deployment's own place — the one
// /places/ebbetts-pass/summary reports on — would show an empty burn domain
// while both its counties had a status.
//
// CORRIDORs are excluded: a corridor is a road, and burn status is a fact about
// land, not about the highway crossing it.
func (s storePlaceIndex) PlaceIDsForCounty(ctx context.Context, countySlug string) ([]string, error) {
	county, err := s.st.GetPlace(ctx, countySlug)
	if err != nil {
		return nil, err
	}
	ids := []string{county.GetId()}

	var countyGeom *geojson.Geom
	if raw := county.GetGeometry().GetGeojson(); len(raw) > 0 {
		countyGeom, _ = geojson.Parse(raw)
	}

	all, err := s.st.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.GetId() == county.GetId() {
			continue
		}
		switch {
		case p.GetParentId() == county.GetId():
			ids = append(ids, p.GetId())
		case p.GetKind() == gridv1.PlaceKind_AREA && countyGeom != nil:
			raw := p.GetGeometry().GetGeojson()
			if len(raw) == 0 {
				continue
			}
			g, err := geojson.Parse(raw)
			if err != nil {
				continue
			}
			if geojson.Intersects(countyGeom, g) {
				ids = append(ids, p.GetId())
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// burnReporters passes the push registry to the burn normalizer only when push
// ingest is actually on. A typed nil would satisfy the interface and then claim
// reporter source rows that can never report.
func burnReporters(reg *pushingest.Registry) ingest.BurnReporters {
	if reg == nil || !reg.Enabled() {
		return nil
	}
	return reg
}
