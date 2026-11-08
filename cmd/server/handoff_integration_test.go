package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	"github.com/sierragridteam/sierra-data/internal/store"
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

func TestGridHandoffLifecycleWithMeshEnabled(t *testing.T) {
	blockOutbound(t)
	dbPath := seededGridDB(t)
	app, port := preparedGrid(t, dbPath)
	h := runController(t, app, true)

	// STANDBY: nothing open, the next owner could take the lock right now.
	assert.Equal(t, handoff.StateStandby, h.state())
	assertLockFree(t, dbPath)

	h.command("/activate", "it-activate")
	st := h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "activation failed: %+v", st.Operation)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/mesh/links"),
		"a gated hand-mounted route")
	assertLockHeld(t, dbPath)

	h.command("/quiesce", "it-release")
	st = h.waitSettled()
	require.Equal(t, handoff.StateQuiesced, st.State, "quiescence failed: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Equal(t, http.StatusServiceUnavailable, httpCode(t, port, "/api/v1/sources"),
		"released: the gRPC-routed surface refuses (503, not the 499 a cancelled gateway gives)")
	assert.Equal(t, http.StatusServiceUnavailable, httpCode(t, port, "/api/v1/mesh/links"))
	assertLockFree(t, dbPath)

	assert.Equal(t, 0, h.terminate(), "SIGTERM after release exits cleanly")
}

func TestGridBootActivationWithMeshEnabled(t *testing.T) {
	blockOutbound(t)
	dbPath := seededGridDB(t)
	app, port := preparedGrid(t, dbPath)
	h := runController(t, app, false)

	require.Eventually(t, func() bool { return h.state() == handoff.StateActive }, 20*time.Second, 20*time.Millisecond,
		"boot activation never reached ACTIVE (state %s)", h.state())
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	assert.Equal(t, 0, h.terminate(), "SIGTERM while ACTIVE quiesces and exits 0")
	assertLockFree(t, dbPath)
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
// activation's presence rehydration has something to log about.
func seededGridDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grid.db")
	st, err := store.Open(path)
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
	return path
}

// preparedGrid is prepareGrid on a minimal config: mesh enabled against a
// dead broker, a free production port, a short lock wait.
func preparedGrid(t *testing.T, dbPath string) (*gridApp, int) {
	t.Helper()
	port := freePort(t)
	prefab.LoadConfigDefaults(map[string]any{"server.port": port, "server.host": "127.0.0.1"})
	cfg := &config.Config{}
	cfg.OpenAI.APIKey = "test"
	cfg.Grid.DBPath = dbPath
	cfg.Grid.LockTimeout = 500 * time.Millisecond
	cfg.Grid.Meshcore.Enabled = true
	cfg.Grid.Meshcore.Brokers = []config.MeshcoreBroker{{URL: "ws://127.0.0.1:1/mqtt"}}
	cfg.Grid.Control.DrainTimeout = 500 * time.Millisecond
	cfg.Grid.Control.WorkerStopTimeout = 5 * time.Second
	app, err := prepareGrid(testCtx(), cfg)
	require.NoError(t, err)
	return app, port
}

type ctrlHarness struct {
	t      *testing.T
	c      *handoff.Controller
	base   string
	cancel context.CancelFunc
	done   chan struct{}
	code   int
}

// runController runs svc under a controller exactly as main does: a logger on
// the process context, nothing else. The service must get everything else it
// needs from the controller.
func runController(t *testing.T, svc handoff.Service, standby bool) *ctrlHarness {
	t.Helper()
	sum := sha256.Sum256([]byte(integrationToken))
	c, err := handoff.New(testCtx(), svc, handoff.Options{Service: "test", Addr: "127.0.0.1:0",
		TokenSha256: hex.EncodeToString(sum[:]), Standby: standby})
	require.NoError(t, err)
	require.NoError(t, c.Listen())
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

func assertLockFree(t *testing.T, path string) {
	t.Helper()
	st, err := store.Open(path, store.WithLockTimeout(100*time.Millisecond))
	require.NoError(t, err, "the database lock should be free")
	require.NoError(t, st.Close())
}

func assertLockHeld(t *testing.T, path string) {
	t.Helper()
	st, err := store.Open(path, store.WithLockTimeout(100*time.Millisecond))
	if err == nil {
		st.Close()
	}
	assert.Error(t, err, "the active owner must hold the database lock")
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
// released predecessor keeps it): activation must fail, not report ACTIVE on
// the strength of someone else's listener.
func TestGridActivationRefusesABoundPort(t *testing.T) {
	blockOutbound(t)
	dbPath := seededGridDB(t)
	app, port := preparedGrid(t, dbPath)
	squatter, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	require.NoError(t, err)
	defer squatter.Close()

	h := runController(t, app, true)
	h.command("/activate", "it-activate")
	st := h.waitSettled()
	assert.Equal(t, handoff.StateStandby, st.State)
	assert.False(t, st.DatabaseHeld)
	assert.Contains(t, st.Operation.Error, "already in use")
	assertLockFree(t, dbPath)
}

// SIGTERM during the lock wait ends it at once (WithLockContext), instead of
// holding shutdown for lockTimeout, past ECS's stop timeout.
func TestGridTerminateDuringLockWaitIsPrompt(t *testing.T) {
	blockOutbound(t)
	dbPath := seededGridDB(t)
	app, _ := preparedGrid(t, dbPath)
	app.cfg.Grid.LockTimeout = time.Minute
	holder, err := store.Open(dbPath)
	require.NoError(t, err)
	defer holder.Close()

	h := runController(t, app, true)
	h.command("/activate", "it-activate")
	require.Eventually(t, func() bool { return h.state() == handoff.StateActivating }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // inside the lock wait
	start := time.Now()
	assert.Equal(t, 0, h.terminate())
	assert.Less(t, time.Since(start), 5*time.Second, "shutdown must not wait out the minute-long lock timeout")
}

// A panic mid-activation releases what was opened (the deferred cleanup runs)
// and returns to STANDBY; a retry then succeeds.
func TestGridPanicDuringActivationReleasesAndRetries(t *testing.T) {
	blockOutbound(t)
	dbPath := seededGridDB(t)
	app, port := preparedGrid(t, dbPath)
	panicked := false
	app.activateHook = func(stage string) {
		if stage == "store-opened" && !panicked {
			panicked = true
			panic("injected")
		}
	}
	h := runController(t, app, true)
	h.command("/activate", "it-activate-1")
	st := h.waitSettled()
	assert.Equal(t, handoff.StateStandby, st.State, "cleanup ran, so the database is free: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Contains(t, st.Operation.Error, "panic during activation: injected")
	assertLockFree(t, dbPath)

	h.command("/activate", "it-activate-2")
	st = h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "%+v", st.Operation)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
}
