package relaytelemetry

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/dpup/prefab/logging"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	meshrelay "github.com/dpup/sierra-meshrelay/relay"
)

const pubkeyA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// --- a real relay with a fake bridge whose node answers companion commands ---

type harness struct {
	t     *testing.T
	ctx   context.Context
	relay *meshrelay.Relay
	store *meshrelay.Store
	base  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := logging.EnsureLogger(context.Background())
	st, err := meshrelay.OpenStore(ctx, filepath.Join(t.TempDir(), "relay.db"), time.Second)
	require.NoError(t, err)
	r := meshrelay.New(ctx, meshrelay.Config{TickInterval: 20 * time.Millisecond}, st)
	port := freePort(t)
	server := prefab.New(prefab.WithContext(ctx), prefab.WithHost("127.0.0.1"), prefab.WithPort(port),
		prefab.WithPlugin(r))
	go func() { _ = server.Start() }()
	h := &harness{t: t, ctx: ctx, relay: r, store: st, base: fmt.Sprintf("127.0.0.1:%d", port)}
	eventually(t, func() bool {
		resp, err := http.Get("http://" + h.base + meshrelay.PathPrefix + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "relay never came up")
	t.Cleanup(func() {
		_ = r.Stop(context.Background())
		_ = server.Shutdown()
		_ = st.Close()
	})
	return h
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// fakeNode is a companion's answers. Firmware below 8 has no GET_STATS.
type fakeNode struct {
	pubkey   string
	name     string
	firmware byte
	mu       sync.Mutex
	commands []byte // command bytes received, in order
}

func (n *fakeNode) answer(cmd []byte) [][]byte {
	n.mu.Lock()
	n.commands = append(n.commands, cmd[0])
	n.mu.Unlock()
	// Every reply is preceded by an unsolicited push (an advert), which the
	// poller must ignore.
	push := []byte{0x80, 0xde, 0xad}
	switch cmd[0] {
	case cmdAppStart:
		f := make([]byte, selfInfoNameOff)
		f[0] = respSelfInfo
		key, _ := hex.DecodeString(n.pubkey)
		copy(f[selfInfoPubkeyOff:], key)
		return [][]byte{push, append(f, n.name...)}
	case cmdDeviceQuery:
		f := make([]byte, 80)
		f[0], f[1] = respDeviceInfo, n.firmware
		return [][]byte{push, f}
	case cmdGetBattStorage:
		f := make([]byte, 11)
		f[0] = respBattStorage
		binary.LittleEndian.PutUint16(f[1:], 4140)
		return [][]byte{f}
	case cmdGetStats:
		if n.firmware < statsMinFirmware {
			return [][]byte{{respErr, 1}}
		}
		switch cmd[1] {
		case statsCore:
			f := make([]byte, 11)
			f[0], f[1] = respStats, statsCore
			binary.LittleEndian.PutUint16(f[2:], 4100)
			binary.LittleEndian.PutUint32(f[4:], 2615083)
			f[10] = 2
			return [][]byte{push, f}
		case statsRadio:
			f := make([]byte, 14)
			f[0], f[1] = respStats, statsRadio
			binary.LittleEndian.PutUint16(f[2:], uint16(0xffff-116+1)) // -116
			f[4] = byte(0xff - 88 + 1)                                 // -88
			f[5] = byte(0xff - 10 + 1)                                 // -10 quarter dB = -2.5
			binary.LittleEndian.PutUint32(f[6:], 49)
			binary.LittleEndian.PutUint32(f[10:], 211)
			return [][]byte{f}
		case statsPackets:
			f := make([]byte, 30)
			f[0], f[1] = respStats, statsPackets
			for i, v := range []uint32{649331, 150305, 149984, 321, 642043, 6702, 240197} {
				binary.LittleEndian.PutUint32(f[2+4*i:], v)
			}
			return [][]byte{f}
		}
	}
	return [][]byte{{respErr, 1}}
}

// bridge connects a fake bridge for gw, fronting node. It returns a func that
// disconnects it.
func (h *harness) bridge(gw string, node *fakeNode) func() {
	h.t.Helper()
	tok, err := meshrelay.MintToken(meshrelay.BridgeTokenPrefix)
	require.NoError(h.t, err)
	require.NoError(h.t, h.store.CreateGateway(gw, meshrelay.TokenHash(tok), node.pubkey, "", time.Now()))
	d := websocket.Dialer{Subprotocols: []string{meshrelay.BridgeSubprotocol}}
	ws, _, err := d.Dial("ws://"+h.base+meshrelay.PathPrefix+"/"+gw+"/dial",
		http.Header{"Authorization": {"Bearer " + tok}})
	require.NoError(h.t, err)
	require.NoError(h.t, ws.WriteJSON(map[string]any{
		"type": "hello", "protocol": 1, "gateway": gw,
		"agent": map[string]any{"name": "meshbridge", "version": "test"},
		"link":  map[string]any{"kind": "tcp"},
		"node":  map[string]any{"state": "connected", "pubkey": node.pubkey, "name": node.name},
	}))
	var mu sync.Mutex
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			for _, f := range node.answer(data) {
				mu.Lock()
				_ = ws.WriteMessage(websocket.BinaryMessage, f)
				mu.Unlock()
			}
		}
	}()
	eventually(h.t, func() bool {
		l, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: gw, Client: "probe",
			Role: meshrelay.RoleAgent, Holder: newHolder()})
		if err == nil {
			h.relay.Release(l)
		}
		return err == nil
	}, "bridge never came online")
	return func() { ws.Close() }
}

// --- a fake Grid ingest endpoint ---

type fakeGrid struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reports []map[string]any
	auth    []string
}

func newFakeGrid(t *testing.T) *fakeGrid {
	g := &fakeGrid{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		g.mu.Lock()
		g.reports = append(g.reports, m)
		g.auth = append(g.auth, r.URL.Path+" "+r.Header.Get("Authorization"))
		g.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":1}`))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGrid) last(t *testing.T) []map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	require.NotEmpty(t, g.reports, "no report pushed")
	m := g.reports[len(g.reports)-1]
	assert.EqualValues(t, 1, m["schema_version"])
	var out []map[string]any
	for _, r := range m["repeaters"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func (h *harness) poller(grid *fakeGrid, gateways ...string) *Poller {
	return New(Config{Gateways: gateways, Timeout: time.Second, GridURL: grid.srv.URL + "/",
		IngestToken: "ingest-secret"}, h.relay, h.store)
}

// --- tests ---

func TestPollReadsTheNodeAndPushesAReport(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := &fakeNode{pubkey: pubkeyA, name: "SIERRA Test Box", firmware: 9}
	h.bridge("gw-a", node)

	h.poller(grid, "gw-a").Cycle(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1)
	r := reps[0]
	assert.Equal(t, pubkeyA, r["id"], "the full key, read from the node itself")
	assert.Equal(t, "SIERRA Test Box", r["name"])
	assert.NotNil(t, r["last_attempt"])
	assert.Equal(t, r["last_attempt"], r["last_success"])
	assert.Equal(t, true, r["online"])
	assert.InDelta(t, 4.14, r["battery_voltage"], 1e-9, "BATT_AND_STORAGE wins over core stats")
	assert.EqualValues(t, 2615083, r["uptime_s"])
	assert.EqualValues(t, 2, r["tx_queue_len"])
	assert.EqualValues(t, -116, r["noise_floor_dbm"])
	assert.EqualValues(t, -88, r["last_rssi_dbm"])
	assert.InDelta(t, -2.5, r["last_snr_db"], 1e-9)
	assert.EqualValues(t, 49000, r["airtime_ms"])
	assert.EqualValues(t, 211000, r["rx_airtime_ms"])
	assert.EqualValues(t, 649331, r["nb_recv"])
	assert.EqualValues(t, 150305, r["nb_sent"])
	assert.EqualValues(t, 149984, r["sent_flood"])
	assert.EqualValues(t, 321, r["sent_direct"])
	assert.EqualValues(t, 642043, r["recv_flood"])
	assert.EqualValues(t, 6702, r["recv_direct"])
	assert.EqualValues(t, 240197, r["recv_errors"])

	grid.mu.Lock()
	assert.Equal(t, "/api/v1/ingest/mesh.repeater Bearer ingest-secret", grid.auth[0])
	grid.mu.Unlock()

	// The lease was released: a person can connect right after.
	l, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "gw-a", Client: "op",
		Role: meshrelay.RoleOperator, Holder: newHolder()})
	require.NoError(t, err)
	h.relay.Release(l)
}

func TestOldFirmwareReportsNullStats(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := &fakeNode{pubkey: pubkeyA, name: "old", firmware: 7}
	h.bridge("gw-a", node)

	h.poller(grid, "gw-a").Cycle(h.ctx)

	r := grid.last(t)[0]
	assert.InDelta(t, 4.14, r["battery_voltage"], 1e-9)
	for _, k := range []string{"uptime_s", "noise_floor_dbm", "nb_sent", "airtime_ms", "recv_errors"} {
		v, present := r[k]
		assert.True(t, present, "%s must be sent, as null", k)
		assert.Nil(t, v, "%s: an unread value is null, never zero", k)
	}
	assert.NotContains(t, node.commands, cmdGetStats, "GET_STATS is not sent below firmware 8")
}

func TestBusyGatewayRecordsNoAttempt(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	h.bridge("gw-a", &fakeNode{pubkey: pubkeyA, name: "n", firmware: 9})
	human, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "gw-a", Client: "op",
		Role: meshrelay.RoleOperator, Holder: newHolder()})
	require.NoError(t, err)

	p := h.poller(grid, "gw-a")
	p.Cycle(h.ctx)
	assert.Empty(t, grid.last(t), "someone using the node says nothing about it: no attempt")

	h.relay.Release(human)
	p.Cycle(h.ctx)
	require.Len(t, grid.last(t), 1)
	assert.NotNil(t, grid.last(t)[0]["last_success"])
}

func TestOfflineBridgeIsAFailedAttemptThatKeepsTheLastReading(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	disconnect := h.bridge("gw-a", &fakeNode{pubkey: pubkeyA, name: "n", firmware: 9})
	p := h.poller(grid, "gw-a")
	p.Cycle(h.ctx)
	first := grid.last(t)[0]

	disconnect()
	eventually(t, func() bool {
		_, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "gw-a", Client: "probe",
			Role: meshrelay.RoleAgent, Holder: newHolder()})
		return err != nil && !strings.Contains(err.Error(), "busy")
	}, "bridge never went offline")
	p.cfg.Now = func() time.Time { return time.Now().Add(time.Minute) }
	p.Cycle(h.ctx)

	r := grid.last(t)[0]
	assert.Equal(t, false, r["online"])
	assert.Greater(t, r["last_attempt"], first["last_attempt"], "the failed attempt is recorded")
	assert.Equal(t, first["last_success"], r["last_success"], "last_success stays at the last real read")
	assert.Equal(t, first["uptime_s"], r["uptime_s"], "readings ride forward, dated by last_success")
}

func TestNeverReachedNodeCarriesNoReadings(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	// Pinned but never connected: the relay refuses with gateway_offline.
	require.NoError(t, h.store.CreateGateway("gw-b", meshrelay.TokenHash("smb_x"), pubkeyA, "", time.Now()))
	// Opted in but not pinned: it cannot be named, so it is not reported.
	require.NoError(t, h.store.CreateGateway("gw-c", meshrelay.TokenHash("smb_y"), "", "", time.Now()))

	h.poller(grid, "gw-b", "gw-c", "gw-unknown").Cycle(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1)
	r := reps[0]
	assert.Equal(t, pubkeyA, r["id"])
	assert.NotNil(t, r["last_attempt"])
	assert.Nil(t, r["last_success"])
	assert.Nil(t, r["battery_voltage"])
	assert.Nil(t, r["nb_sent"])
}

func TestGatewaysNotOptedInAreNeverPolled(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := &fakeNode{pubkey: pubkeyA, name: "private", firmware: 9}
	h.bridge("gw-a", node)

	h.poller(grid /* no gateways */).Cycle(h.ctx)

	assert.Empty(t, grid.last(t))
	node.mu.Lock()
	defer node.mu.Unlock()
	assert.Empty(t, node.commands, "no lease, no frames")
}

func TestReportJSONSendsNullsExplicitly(t *testing.T) {
	b, err := json.Marshal(ReportEntry{ID: pubkeyA})
	require.NoError(t, err)
	assert.True(t, bytes.Contains(b, []byte(`"battery_voltage":null`)), string(b))
	assert.True(t, bytes.Contains(b, []byte(`"last_success":null`)), string(b))
}
