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
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/handoff"
	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// The REAL relayApp under the handoff controller, as main runs it. The first
// activation finds relay.db locked by another owner, which is the path that
// logs from inside the activation context ("waiting for it to exit"): a
// logger-less activation context panicked there. It must fail cleanly back
// to STANDBY, released; the retry after the owner lets go must succeed.
func TestRelayHandoffLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	port := freePort(t)
	app, err := prepareRelay(testCtx(), &config.MeshRelayConfig{
		DBPath: dbPath, Port: port, LockTimeout: 600 * time.Millisecond,
		Control: config.ControlConfig{DrainTimeout: 500 * time.Millisecond, WorkerStopTimeout: 5 * time.Second},
	})
	require.NoError(t, err)
	h := runController(t, app, true)

	owner, err := meshrelay.OpenStore(testCtx(), dbPath, time.Second)
	require.NoError(t, err)
	h.command("/activate", "it-activate-1")
	st := h.waitSettled()
	assert.Equal(t, handoff.StateStandby, st.State, "a held lock fails the activation: %+v", st.Operation)
	assert.False(t, st.DatabaseHeld)
	assert.Equal(t, "failed", st.Operation.Status)
	assert.Contains(t, st.Operation.Error, "locked")
	require.NoError(t, owner.Close())

	h.command("/activate", "it-activate-2")
	st = h.waitSettled()
	require.Equal(t, handoff.StateActive, st.State, "activation failed: %+v", st.Operation)
	assert.Equal(t, http.StatusOK, httpCode(t, port, meshrelay.PathPrefix+"/healthz"))
	assert.Equal(t, http.StatusOK, httpCode(t, port, meshrelay.PathPrefix+"/api/status"))

	h.command("/quiesce", "it-release")
	st = h.waitSettled()
	require.Equal(t, handoff.StateQuiesced, st.State, "quiescence failed: %+v", st.Operation)
	assert.Equal(t, http.StatusServiceUnavailable, httpCode(t, port, meshrelay.PathPrefix+"/healthz"))
	next, err := meshrelay.OpenStore(testCtx(), dbPath, 100*time.Millisecond)
	require.NoError(t, err, "released: the next owner takes the lock at once")
	require.NoError(t, next.Close())

	assert.Equal(t, 0, h.terminate())
}

// The telemetry poller runs only while the relay is ACTIVE: it starts with
// activation and quiesce stops it before relay.db is released.
func TestRelayTelemetryPollerFollowsTheLifecycle(t *testing.T) {
	var mu sync.Mutex
	pushes := 0
	grid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pushes++
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer grid.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return pushes }

	prev := telemetryFirstPoll
	telemetryFirstPoll = 0
	defer func() { telemetryFirstPoll = prev }()

	port := freePort(t)
	app, err := prepareRelay(testCtx(), &config.MeshRelayConfig{
		DBPath: filepath.Join(t.TempDir(), "relay.db"), Port: port, LockTimeout: time.Second,
		Control: config.ControlConfig{DrainTimeout: 500 * time.Millisecond, WorkerStopTimeout: 5 * time.Second},
		Telemetry: config.MeshRelayTelemetryConfig{Gateways: []string{"gw-a"}, Interval: 20 * time.Millisecond,
			GridURL: grid.URL, IngestToken: "x"},
	})
	require.NoError(t, err)
	h := runController(t, app, true)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, count(), "no polling in STANDBY")

	h.command("/activate", "tel-activate")
	require.Equal(t, handoff.StateActive, h.waitSettled().State)
	require.Eventually(t, func() bool { return count() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the poller reports on its interval once ACTIVE")

	h.command("/quiesce", "tel-quiesce")
	require.Equal(t, handoff.StateQuiesced, h.waitSettled().State)
	after := count()
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, after, count(), "quiesce stopped the poller")
	assert.Equal(t, 0, h.terminate())
}

// --- harness (mirrors cmd/server's) -------------------------------------------

const integrationToken = "integration-deploy-token"

type ctrlHarness struct {
	t      *testing.T
	c      *handoff.Controller
	base   string
	cancel context.CancelFunc
	done   chan struct{}
	code   int
}

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

func (h *ctrlHarness) command(path, op string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.base+path, bytes.NewBufferString(`{"operationId":"`+op+`"}`))
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	resp.Body.Close()
	require.Contains(h.t, []int{http.StatusOK, http.StatusAccepted}, resp.StatusCode, "%s %s", path, op)
}

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

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
