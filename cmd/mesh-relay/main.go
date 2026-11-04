// Command mesh-relay is the SIERRA Mesh Relay: the service MeshCore gateway
// bridges dial out to, and that authenticated clients reach a node through.
// See internal/meshrelay and docs/mesh-relay-server.md.
//
// It is a separate process from cmd/server on purpose — a relay crash must not
// touch the hazard feeds. It still runs on prefab like the Grid: prefab owns
// the HTTP server, config (prefab.yaml's meshRelay section, PF__MESH_RELAY__*
// overrides), logging, and the relay's lifecycle (it is a prefab plugin).
//
//	mesh-relay                    # serve (activate at boot)
//	mesh-relay --standby          # prepare; wait on the control port for POST /activate
//	mesh-relay token -name dpup   # mint a bootstrap admin token + its hash
//
// The lifecycle is internal/handoff's, like the Grid's.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/handoff"
	"github.com/dpup/sierra-data/internal/meshrelay"
)

const defaultPort = 8090

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		mintAdmin(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet("mesh-relay", flag.ExitOnError)
	standby := fs.Bool("standby", false,
		"start in STANDBY: prepare without opening relay.db and wait for POST /activate on the control port")
	_ = fs.Parse(os.Args[1:])

	ctx := logging.With(context.Background(), logging.NewProdLogger())
	logging.Infow(ctx, "Starting the SIERRA Mesh Relay", "standby", *standby)

	relayCfg := config.LoadMeshRelayConfig()
	config.RegisterPrefabNamespaces()

	app, err := prepareRelay(ctx, relayCfg)
	if err != nil {
		log.Fatalf("Invalid meshRelay config: %v", err)
	}
	opts := handoff.Options{Service: "mesh-relay", Standby: *standby}
	if ctl := relayCfg.Control; ctl.Enabled() {
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

// relayApp is the relay as internal/handoff drives it: prepareRelay checks
// config and opens nothing; Activate opens relay.db and serves; Quiesce
// closes every session, flushes the audit queue and releases relay.db.
type relayApp struct {
	ctx         context.Context
	cfg         meshrelay.Config
	control     config.ControlConfig
	dbPath      string
	lockTimeout time.Duration
	port        int

	// onFatal is the controller's Fail (main sets it).
	onFatal func(error)

	mu         sync.Mutex
	store      *meshrelay.Store
	relay      *meshrelay.Relay
	serverDone chan struct{}
	quiesced   struct{ gate, sessions, drained, store bool }
}

func prepareRelay(ctx context.Context, relayCfg *config.MeshRelayConfig) (*relayApp, error) {
	cfg, err := meshrelay.ConfigFrom(*relayCfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.BootstrapAdmins) == 0 {
		logging.Warn(ctx, "No bootstrap admins configured (meshRelay.admins); the admin API is unusable "+
			"until the clients table has an admin")
	}
	a := &relayApp{ctx: ctx, cfg: cfg, control: relayCfg.Control,
		dbPath: relayCfg.DBPath, lockTimeout: relayCfg.LockTimeout, port: relayCfg.Port}
	if a.dbPath == "" {
		a.dbPath = "./data/relay.db"
	}
	if a.lockTimeout <= 0 {
		a.lockTimeout = 90 * time.Second
	}
	if a.port == 0 {
		a.port = defaultPort
	}
	logging.Infow(ctx, "Relay prepared (no database opened)", "path", a.dbPath, "port", a.port)
	return a, nil
}

// Activate opens relay.db and starts serving. A failure releases what it
// opened (see gridApp.Activate for the same contract).
func (a *relayApp) Activate(actx context.Context) (err error) {
	// The lock wait logs through actx; see gridApp.Activate.
	actx = handoff.WithLoggerOf(actx, a.ctx)
	start := time.Now()
	a.mu.Lock()
	a.store, a.relay = nil, nil
	a.quiesced = struct{ gate, sessions, drained, store bool }{}
	a.mu.Unlock()
	defer func() {
		// A panic is a failed activation like any other: release what was
		// opened rather than leave it running behind a "held" status.
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during activation: %v\n%s", r, debug.Stack())
		}
		if err == nil {
			return
		}
		logging.Warnw(a.ctx, "Activation failed; releasing what it opened", "error", err)
		if qerr := a.Quiesce(a.ctx); qerr != nil {
			err = fmt.Errorf("%w (and releasing it failed: %w: %w)", err, handoff.ErrStillHeld, qerr)
		}
	}()

	// The listener only opens once the lock is held. The lock is a second
	// line of defence: activation itself is the coordinator's decision.
	st, err := meshrelay.OpenStore(actx, a.dbPath, a.lockTimeout)
	if err != nil {
		return fmt.Errorf("open relay store %s: %w", a.dbPath, err)
	}
	a.mu.Lock()
	a.store = st
	a.mu.Unlock()
	logging.Infow(a.ctx, "Relay store opened", "path", a.dbPath, "duration", time.Since(start))

	relay := meshrelay.New(a.ctx, a.cfg, st)
	a.mu.Lock()
	a.relay = relay
	a.mu.Unlock()
	server := prefab.New(
		prefab.WithContext(a.ctx),
		// meshRelay.port, not server.port: both processes read the same
		// prefab.yaml, and server.port is the Grid's.
		prefab.WithPort(a.port),
		// The relay is a plugin: it mounts its own routes (OptionProvider) and
		// prefab drives its Init/Shutdown.
		prefab.WithPlugin(relay),
	)
	// Our listener must be the one WaitListening finds.
	if err := handoff.PortFree(a.port); err != nil {
		return err
	}
	done := make(chan struct{})
	var serverErr error
	a.mu.Lock()
	a.serverDone = done
	a.mu.Unlock()
	go func() {
		serverErr = server.Start()
		if serverErr != nil {
			logging.Errorw(a.ctx, "Relay server failed", "error", serverErr)
		}
		close(done)
	}()
	if err := handoff.WaitListening(actx, a.port, done, 15*time.Second); err != nil {
		return fmt.Errorf("relay listener: %w", err)
	}
	// prefab's Start returns nil only after its own SIGTERM shutdown; an error
	// later means the relay stopped serving while holding relay.db, which the
	// pre-handoff main treated as fatal.
	go func() {
		<-done
		if serverErr != nil && a.onFatal != nil {
			a.onFatal(fmt.Errorf("relay server stopped: %w", serverErr))
		}
	}()
	logging.Infow(a.ctx, "Relay serving", "port", a.port, "activation", time.Since(start))
	return nil
}

// Quiesce releases relay.db: stop admitting, close every session (leases end
// with relay_shutdown, bridges get 1001) and flush the audit queue, drain the
// admin API, then close the store.
func (a *relayApp) Quiesce(_ context.Context) error {
	start := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.relay != nil && !a.quiesced.gate {
		a.relay.Gate().Close()
	}
	a.quiesced.gate = true

	if a.relay != nil && !a.quiesced.sessions {
		t := time.Now()
		ctx, cancel := context.WithTimeout(a.ctx, a.control.WorkerStop())
		err := a.relay.Stop(ctx)
		cancel()
		if err != nil {
			logging.Errorw(a.ctx, "Quiesce: relay sessions did not close", "error", err,
				"liveSessions", a.relay.LiveSessions(), "duration", time.Since(t))
			return err
		}
		logging.Infow(a.ctx, "Quiesce: relay sessions closed, audit queue flushed", "duration", time.Since(t))
	}
	a.quiesced.sessions = true

	if a.relay != nil && !a.quiesced.drained {
		res := a.relay.Gate().Drain(a.control.Drain(), 2*time.Second)
		logging.Infow(a.ctx, "Quiesce: request drain complete",
			"inFlight", res.InFlight, "cancelled", res.Cancelled, "remaining", res.Remaining,
			"duration", res.Duration)
	}
	a.quiesced.drained = true

	if a.store != nil && !a.quiesced.store {
		t := time.Now()
		if err := a.store.Close(); err != nil {
			logging.Errorw(a.ctx, "Quiesce: relay store close failed", "error", err)
			return fmt.Errorf("close relay store: %w", err)
		}
		logging.Infow(a.ctx, "Quiesce: relay store closed and lock released", "duration", time.Since(t))
	}
	a.quiesced.store = true
	logging.Infow(a.ctx, "Quiesce: relay.db ownership released", "duration", time.Since(start))
	return nil
}

// Exit waits for prefab's own SIGTERM shutdown to finish.
func (a *relayApp) Exit(ctx context.Context) {
	a.mu.Lock()
	done := a.serverDone
	a.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			logging.Warnw(a.ctx, "Relay HTTP server did not stop before exit")
		}
	}
	logging.Info(a.ctx, "Shutdown complete")
}

// mintAdmin prints a bootstrap admin token and the config entry for its hash.
func mintAdmin(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	name := fs.String("name", "", "admin name (lowercase slug)")
	_ = fs.Parse(args)
	if !meshrelay.ValidClientName(*name) {
		fatal("token: -name must be a lowercase slug")
	}
	tok, err := meshrelay.MintToken(meshrelay.ClientTokenPrefix)
	if err != nil {
		fatal("token: %v", err)
	}
	fmt.Println()
	fmt.Println("  Bootstrap admin token — shown ONCE, stored nowhere. Keep it on your machine:")
	fmt.Println()
	fmt.Println("    " + tok)
	fmt.Println()
	fmt.Println("  Add this to prefab.yaml under meshRelay.admins (the hash is safe to commit):")
	fmt.Println()
	fmt.Printf("    - name: %s\n      tokenSha256: %q\n", *name, meshrelay.TokenHash(tok))
	fmt.Println()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mesh-relay: "+format+"\n", args...)
	os.Exit(1)
}
