package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// These tests drive the REAL gridApp through the handoff controller, the way
// main does, with the MeshCore path enabled. The controller's own tests use a
// fake service, which is how a logger-less activation context shipped: the
// fake never logged through it, and every local smoke test ran with mesh off,
// so nothing reached seedMeshRegistry's first log line until production did.
//
// Outbound HTTP is refused (blockOutbound) and the MQTT broker is a dead
// local port, so nothing here touches a real upstream.

const integrationToken = "integration-deploy-token"

// The coordinator remap (route B): STANDBY is a reader that does not
// campaign, /activate takes the lease, /quiesce releases it while reads
// continue, SIGTERM exits.
func TestGridHandoffLifecycleWithMeshEnabled(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	h := runGrid(t, app, true)

	// STANDBY: serving reads, holding nothing; the next owner could take the
	// lease right now.
	assert.Equal(t, handoff.StateStandby, h.state())
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"), "a standby serves reads")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/readyz"))
	st := h.status()
	assert.True(t, st.Ready)
	assert.False(t, st.DatabaseHeld)
	assertLockFree(t, dbURL)

	h.command("/activate", "it-activate")
	st = h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "activation failed: %+v", st.Operation)
	assert.True(t, st.DatabaseHeld, "ACTIVE under a coordinator means the lease is held")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/mesh/links"),
		"a gated hand-mounted route")
	assertLockHeld(t, dbURL)

	h.command("/quiesce", "it-release")
	st = h.waitSettled()
	require.Equal(t, handoff.StateQuiesced, st.State, "quiescence failed: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"),
		"released: the lease is gone but reads continue until SIGTERM")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/mesh/links"))
	assertLockFree(t, dbURL)

	assert.Equal(t, 0, h.terminate(), "SIGTERM after release exits cleanly")
	assert.Equal(t, http.StatusServiceUnavailable, httpCode(t, port, "/api/v1/sources"),
		"after exit the admission gate refuses (503, not the 499 a cancelled gateway gives)")
}

// The native path: no --standby, so the process campaigns at boot and holds
// the lease once it is free; SIGTERM releases it.
func TestGridBootActivationWithMeshEnabled(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	h := runGrid(t, app, false)

	require.Eventually(t, func() bool { return h.state() == handoff.StateActive && app.DatabaseHeld() },
		20*time.Second, 20*time.Millisecond, "boot activation never took the lease (state %s)", h.state())
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	assert.Equal(t, 0, h.terminate(), "SIGTERM while ACTIVE quiesces and exits 0")
	assertLockFree(t, dbURL)
}

// Boot activation while another process holds the lease: ACTIVE at once
// (campaigning, not holding), serving reads; it takes over as soon as the
// holder lets go.
func TestGridCampaignsWhileAnotherHolds(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	holder, err := store.Open(dbURL)
	require.NoError(t, err)
	app, port := preparedGrid(t, dbURL)
	h := runGrid(t, app, false)

	require.Eventually(t, func() bool { return h.state() == handoff.StateActive }, 20*time.Second, 10*time.Millisecond)
	assert.False(t, app.DatabaseHeld())
	assert.False(t, h.status().DatabaseHeld, "ACTIVE but between leases: /status says so")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/readyz"), "a reader is ready")

	released := time.Now()
	require.NoError(t, holder.Close())
	require.Eventually(t, app.DatabaseHeld, 20*time.Second, 10*time.Millisecond)
	t.Logf("took the lease %s after the holder released it", time.Since(released))
	assert.True(t, h.status().DatabaseHeld)
}

// yield releases the lease and stops campaigning; campaign resumes it.
func TestGridYieldAndCampaign(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	h := runGrid(t, app, false)
	require.Eventually(t, app.DatabaseHeld, 20*time.Second, 20*time.Millisecond)

	assert.Equal(t, http.StatusOK, h.post("/yield"))
	assert.False(t, app.DatabaseHeld())
	assertLockFree(t, dbURL)
	time.Sleep(1500 * time.Millisecond)
	assert.False(t, app.DatabaseHeld(), "a yielded process does not campaign")
	assertLockFree(t, dbURL)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"), "and keeps serving reads")
	assert.Equal(t, handoff.StateActive, h.state())

	assert.Equal(t, http.StatusOK, h.post("/campaign"))
	require.Eventually(t, app.DatabaseHeld, 5*time.Second, 10*time.Millisecond)

	h.command("/quiesce", "it-release")
	require.Equal(t, handoff.StateQuiesced, h.waitSettled().State)
	assert.Equal(t, http.StatusConflict, h.post("/campaign"), "a quiesced process never campaigns again")
}

// Losing the lease's session (an RDS failover looks like this) stops the
// writers; the process keeps serving reads and campaigns again, coming back
// with a newer epoch.
func TestGridLeaseLossDropsToReaderAndRecampaigns(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	h := runGrid(t, app, false)
	require.Eventually(t, app.DatabaseHeld, 20*time.Second, 20*time.Millisecond)
	_ = h

	admin, err := store.OpenReader(dbURL)
	require.NoError(t, err)
	defer admin.Close()
	before, err := admin.WriterLease(context.Background())
	require.NoError(t, err)

	db, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer db.Close()
	var killed int
	require.NoError(t, db.QueryRow(`SELECT COUNT(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&killed))
	require.Equal(t, 1, killed, "the lease connection")

	require.Eventually(t, func() bool {
		w, err := admin.WriterLease(context.Background())
		return err == nil && w.Epoch > before.Epoch && w.Live && app.DatabaseHeld()
	}, 15*time.Second, 50*time.Millisecond, "the process came back as the writer with a newer epoch")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
}

// --- harness ----------------------------------------------------------------

// blockOutbound refuses every non-loopback HTTP request for the test's
// duration. Every upstream client uses http.DefaultTransport.
func blockOutbound(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = loopbackOnly{next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); (ip == nil || !ip.IsLoopback()) && r.URL.Hostname() != "localhost" {
		return nil, fmt.Errorf("test: outbound request to %s blocked", r.URL.Host)
	}
	return l.next.RoundTrip(r)
}

// seededGridDB creates a grid store holding one ACTIVE mesh node, so
// activation's presence rehydration has something to log about. Returns the
// database URL (a fresh database per test, internal/storetest).
func seededGridDB(t *testing.T) string {
	t.Helper()
	url := storetest.URL(t)
	st, err := store.Open(url)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{{ID: "meshcore", Name: "MeshCore", PollInterval: time.Minute}}))
	pk := "aa" + string(bytes.Repeat([]byte("0"), 62))
	_, err = st.UpsertEvent(ctx, &gridv1.Event{
		Id: "meshcore:" + pk, Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: gridv1.EventStatus_ACTIVE, Headline: "Test node",
		Provenance: &gridv1.Provenance{SourceId: "meshcore"},
		Geometry:   &gridv1.Geometry{Centroid: &gridv1.LatLng{Lat: 38.2, Lng: -120.4}},
		Detail:     &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: pk, Name: "Test node"}},
	})
	require.NoError(t, err)
	require.NoError(t, st.Close())
	return url
}

// preparedGrid is prepareGrid on a minimal config: mesh enabled against a
// dead broker, a free production port, a short lock wait. Serve is the
// caller's (runGrid), as main does.
func preparedGrid(t *testing.T, dbURL string) (*gridApp, int) {
	t.Helper()
	port := freePort(t)
	prefab.LoadConfigDefaults(map[string]any{"server.port": port, "server.host": "127.0.0.1"})
	cfg := &config.Config{}
	cfg.OpenAI.APIKey = "test"
	cfg.Grid.Database.URL = dbURL
	cfg.Grid.Database.LockTimeout = 500 * time.Millisecond
	cfg.Grid.Meshcore.Enabled = true
	cfg.Grid.Meshcore.Brokers = []config.MeshcoreBroker{{URL: "ws://127.0.0.1:1/mqtt"}}
	cfg.Grid.Control.DrainTimeout = 500 * time.Millisecond
	cfg.Grid.Control.WorkerStopTimeout = 5 * time.Second
	app, err := prepareGrid(testCtx(), cfg)
	require.NoError(t, err)
	app.serverStopWait = 10 * time.Millisecond
	return app, port
}

// runGrid starts app the way main does: the controller with the Grid's
// options, then Serve (reads), then Run.
func runGrid(t *testing.T, app *gridApp, standby bool) *ctrlHarness {
	t.Helper()
	app.strictActivate = standby
	h := runControllerWith(t, app, gridControlOptions(app, app.cfg, standby), func(c *handoff.Controller) {
		app.onFatal = c.Fail
		app.instanceID = c.InstanceID()
		require.NoError(t, app.Serve(testCtx()))
	})
	return h
}

type ctrlHarness struct {
	t      *testing.T
	c      *handoff.Controller
	base   string
	cancel context.CancelFunc
	done   chan struct{}
	code   int
}

// runControllerWith runs svc under a controller exactly as main does: a
// logger on the process context, nothing else. The service must get
// everything else it needs from the controller. before runs between New and
// Run (main's Serve).
func runControllerWith(t *testing.T, svc handoff.Service, opts handoff.Options, before func(*handoff.Controller)) *ctrlHarness {
	t.Helper()
	sum := sha256.Sum256([]byte(integrationToken))
	opts.Service, opts.Addr, opts.TokenSha256 = "test", "127.0.0.1:0", hex.EncodeToString(sum[:])
	c, err := handoff.New(testCtx(), svc, opts)
	require.NoError(t, err)
	require.NoError(t, c.Listen())
	if before != nil {
		before(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &ctrlHarness{t: t, c: c, base: "http://" + c.ControlAddr(), cancel: cancel, done: make(chan struct{})}
	go func() { h.code = c.Run(ctx); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
		}
	})
	return h
}

func (h *ctrlHarness) state() handoff.State { return h.c.State() }

func (h *ctrlHarness) command(path, op string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.base+path, bytes.NewBufferString(`{"operationId":"`+op+`"}`))
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	resp.Body.Close()
	require.Contains(h.t, []int{http.StatusOK, http.StatusAccepted}, resp.StatusCode, "%s %s", path, op)
}

// post sends an authenticated control command with no body and returns the
// HTTP status.
func (h *ctrlHarness) post(path string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func (h *ctrlHarness) status() handoff.StatusDoc {
	h.t.Helper()
	resp, err := http.Get(h.base + "/status")
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var d handoff.StatusDoc
	require.NoError(h.t, json.NewDecoder(resp.Body).Decode(&d))
	return d
}

// waitSettled waits for the current operation to finish and returns /status.
func (h *ctrlHarness) waitSettled() handoff.StatusDoc {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(h.base + "/status")
		require.NoError(h.t, err)
		var d handoff.StatusDoc
		require.NoError(h.t, json.NewDecoder(resp.Body).Decode(&d))
		resp.Body.Close()
		if d.Operation != nil && d.Operation.Status != "running" {
			return d
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("operation never settled: %+v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *ctrlHarness) terminate() int {
	h.t.Helper()
	h.cancel()
	select {
	case <-h.done:
		return h.code
	case <-time.After(15 * time.Second):
		h.t.Fatal("controller did not exit")
		return -1
	}
}

func httpCode(t *testing.T, port int, path string) int {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// The writer lease is a PostgreSQL advisory lock per database: a second
// store on the same database is the contender the next deploy would be.
func assertLockFree(t *testing.T, url string) {
	t.Helper()
	st, err := store.Open(url, store.WithLockTimeout(100*time.Millisecond))
	require.NoError(t, err, "the database lock should be free")
	require.NoError(t, st.Close())
}

func assertLockHeld(t *testing.T, url string) {
	t.Helper()
	st, err := store.Open(url, store.WithLockTimeout(100*time.Millisecond))
	if err == nil {
		st.Close()
	}
	assert.ErrorIs(t, err, store.ErrLockHeld, "the active owner must hold the database lock")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Fixes from the post-incident review, each against the real Grid.

// The production port already bound (in a shared network namespace a
// predecessor keeps it): Serve must fail, not report serving on the strength
// of someone else's listener.
func TestGridServeRefusesABoundPort(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	squatter, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	require.NoError(t, err)
	defer squatter.Close()

	err = app.Serve(testCtx())
	assert.ErrorContains(t, err, "already in use")
	assert.Nil(t, app.store, "a failed Serve closes what it opened")
	assertLockFree(t, dbURL)
}

// SIGTERM during the lock wait ends it at once (WithLockContext), instead of
// holding shutdown for lockTimeout, past ECS's stop timeout.
func TestGridTerminateDuringLockWaitIsPrompt(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, _ := preparedGrid(t, dbURL)
	app.cfg.Grid.Database.LockTimeout = time.Minute
	holder, err := store.Open(dbURL)
	require.NoError(t, err)
	defer holder.Close()

	h := runGrid(t, app, true)
	h.command("/activate", "it-activate")
	require.Eventually(t, func() bool { return h.state() == handoff.StateActivating }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // inside the lock wait
	start := time.Now()
	assert.Equal(t, 0, h.terminate())
	assert.Less(t, time.Since(start), 5*time.Second, "shutdown must not wait out the minute-long lock timeout")
}

// A panic mid-activation (coordinator mode) releases what was opened (the
// deferred cleanup runs) and returns to STANDBY; a retry then succeeds.
func TestGridPanicDuringActivationReleasesAndRetries(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	app, port := preparedGrid(t, dbURL)
	panicked := false
	app.activateHook = func(stage string) {
		if stage == "lease-acquired" && !panicked {
			panicked = true
			panic("injected")
		}
	}
	h := runGrid(t, app, true)
	h.command("/activate", "it-activate-1")
	st := h.waitSettled()
	assert.Equal(t, handoff.StateStandby, st.State, "cleanup ran, so the database is free: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Contains(t, st.Operation.Error, "panic during activation: injected")
	assertLockFree(t, dbURL)

	h.command("/activate", "it-activate-2")
	st = h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "%+v", st.Operation)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
}
