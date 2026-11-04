// Command mesh-relay is the SIERRA Mesh Relay: the service MeshCore gateway
// bridges dial out to, and that authenticated clients reach a node through.
// See internal/meshrelay and docs/mesh-relay-server.md.
//
// It is a separate process from cmd/server on purpose — a relay crash must not
// touch the hazard feeds. It still runs on prefab like the Grid: prefab owns
// the HTTP server, config (prefab.yaml's meshRelay section, PF__MESH_RELAY__*
// overrides), logging, and the relay's lifecycle (it is a prefab plugin).
//
//	mesh-relay                    # serve
//	mesh-relay token -name dpup   # mint a bootstrap admin token + its hash
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/meshrelay"
)

const defaultPort = 8090

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		mintAdmin(os.Args[2:])
		return
	}

	ctx := logging.With(context.Background(), logging.NewProdLogger())
	logging.Info(ctx, "Starting the SIERRA Mesh Relay")

	relayCfg := config.LoadMeshRelayConfig()
	config.RegisterPrefabNamespaces()

	cfg, err := meshrelay.ConfigFrom(*relayCfg)
	if err != nil {
		log.Fatalf("Invalid meshRelay config: %v", err)
	}
	if len(cfg.BootstrapAdmins) == 0 {
		logging.Warn(ctx, "No bootstrap admins configured (meshRelay.admins); the admin API is unusable "+
			"until the clients table has an admin")
	}
	dbPath := relayCfg.DBPath
	if dbPath == "" {
		dbPath = "./data/relay.db"
	}
	lockTimeout := relayCfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = 90 * time.Second
	}
	port := relayCfg.Port
	if port == 0 {
		port = defaultPort
	}

	// Opened before the server so the listener only opens once the lock is
	// held — the same rolling-deploy behaviour as the grid store.
	st, err := meshrelay.OpenStore(ctx, dbPath, lockTimeout)
	if err != nil {
		logging.Errorw(ctx, "Failed to open relay store", "path", dbPath, "error", err)
		log.Fatalf("Failed to open relay store: %v", err)
	}
	defer st.Close()
	logging.Infow(ctx, "Relay store opened", "path", dbPath)

	relay := meshrelay.New(ctx, cfg, st)
	server := prefab.New(
		prefab.WithContext(ctx),
		// meshRelay.port, not server.port: both processes read the same
		// prefab.yaml, and server.port is the Grid's.
		prefab.WithPort(port),
		// The relay is a plugin: it mounts its own routes (OptionProvider) and
		// prefab drives its Init/Shutdown.
		prefab.WithPlugin(relay),
	)

	if err := server.Start(); err != nil {
		logging.Errorw(ctx, "Relay server failed", "error", err)
		log.Fatalf("Relay server failed: %v", err)
	}
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
