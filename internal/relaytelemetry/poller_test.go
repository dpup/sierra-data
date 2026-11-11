package relaytelemetry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/sierragridteam/sierra-data/internal/clients/meshcore"
	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

const pubkeyA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// --- a real relay with a fake bridge whose node answers companion commands ---

type harness struct {
	t     *testing.T
	ctx   context.Context
	relay *meshrelay.Relay
	store *meshrelay.Store
	base  string

	mu      sync.Mutex
	bridges map[string]*fakeBridgeConn // by gateway
}

// fakeBridgeConn is a connected fake bridge's socket, for sending control
// messages from a test.
type fakeBridgeConn struct {
	mu   sync.Mutex
	ws   *websocket.Conn
	node *fakeNode
}

// setNodeState sends node_status, as a bridge does when its host pauses it.
func (h *harness) setNodeState(gw, state string) {
	h.t.Helper()
	h.mu.Lock()
	b := h.bridges[gw]
	h.mu.Unlock()
	require.NotNil(h.t, b, "no bridge for %s", gw)
	b.mu.Lock()
	defer b.mu.Unlock()
	// The node's fields sit beside "type" (meshproto.NodeStatus embeds Node).
	require.NoError(h.t, b.ws.WriteJSON(map[string]any{"type": "node_status",
		"state": state, "pubkey": b.node.pubkey, "name": b.node.name}))
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
// With signer set, it answers CMD_EXPORT_CONTACT (no key) with its own
// freshly stamped, signed advert, as MeshCore companion firmware does.
type fakeNode struct {
	pubkey   string
	name     string
	firmware byte
	signer   ed25519.PrivateKey
	mu       sync.Mutex
	commands []byte // command bytes received, in order
	clock    uint32 // advert timestamps: each export is a new one
	// lat/lon in micro-degrees, advertised when hasLoc.
	lat, lon int32
	hasLoc   bool
}

func (n *fakeNode) setLocation(lat, lon int32) {
	n.mu.Lock()
	n.lat, n.lon, n.hasLoc = lat, lon, true
	n.mu.Unlock()
}

// newSignedNode is a fakeNode with a real Ed25519 identity, so its adverts
// verify.
func newSignedNode(t *testing.T, name string) *fakeNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return &fakeNode{pubkey: hex.EncodeToString(pub), name: name, firmware: 9, signer: priv, clock: 1_700_000_000}
}

func (n *fakeNode) setName(name string) {
	n.mu.Lock()
	n.name = name
	n.mu.Unlock()
}

// selfAdvert builds a flood ADVERT frame: header, path_len 0, pubkey,
// timestamp, signature over pubkey||timestamp||appdata, appdata (companion
// role with a name).
func (n *fakeNode) selfAdvert() []byte {
	n.mu.Lock()
	n.clock++
	ts, name, lat, lon, hasLoc := n.clock, n.name, n.lat, n.lon, n.hasLoc
	n.mu.Unlock()
	pub := n.signer.Public().(ed25519.PublicKey)
	app := []byte{0x81} // companion, name follows
	if hasLoc {
		app[0] |= advertFlagLocation
		app = binary.LittleEndian.AppendUint32(app, uint32(lat))
		app = binary.LittleEndian.AppendUint32(app, uint32(lon))
	}
	app = append(app, name...)
	msg := append(append([]byte{}, pub...), binary.LittleEndian.AppendUint32(nil, ts)...)
	sig := ed25519.Sign(n.signer, append(append([]byte{}, msg...), app...))
	f := []byte{payloadAdvert<<2 | 0x01, 0x00} // flood ADVERT, no path
	f = append(append(append(f, msg...), sig...), app...)
	return f
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
	case cmdExportContact:
		if n.signer == nil || len(cmd) != 1 {
			return [][]byte{{respErr, 1}}
		}
		return [][]byte{push, append([]byte{respExportContact}, n.selfAdvert()...)}
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
	conn := &fakeBridgeConn{ws: ws, node: node}
	h.mu.Lock()
	if h.bridges == nil {
		h.bridges = map[string]*fakeBridgeConn{}
	}
	h.bridges[gw] = conn
	h.mu.Unlock()
	mu := &conn.mu
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

// last is the last mesh.repeater report's entries.
func (g *fakeGrid) last(t *testing.T) []map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	var m map[string]any
	for i := len(g.reports) - 1; i >= 0; i-- {
		if strings.HasPrefix(g.auth[i], "/api/v1/ingest/mesh.repeater ") {
			m = g.reports[i]
			break
		}
	}
	require.NotNil(t, m, "no report pushed")
	assert.EqualValues(t, 1, m["schema_version"])
	var out []map[string]any
	for _, r := range m["repeaters"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// poller polls every paired gateway except exclude.
func (h *harness) poller(grid *fakeGrid, exclude ...string) *Poller {
	return New(Config{ExcludeGateways: exclude, Timeout: time.Second, GridURL: grid.srv.URL + "/",
		IngestToken: "ingest-secret"}, h.relay, h.store, nil)
}

// --- tests ---

func TestPollReadsTheNodeAndPushesAReport(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := &fakeNode{pubkey: pubkeyA, name: "SIERRA Test Box", firmware: 9}
	h.bridge("gw-a", node)

	h.poller(grid).Cycle(h.ctx)

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

	h.poller(grid).Cycle(h.ctx)

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

	p := h.poller(grid)
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
	p := h.poller(grid)
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
	// Not pinned: it cannot be named, so it is not reported.
	require.NoError(t, h.store.CreateGateway("gw-c", meshrelay.TokenHash("smb_y"), "", "", time.Now()))

	h.poller(grid).Cycle(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1)
	r := reps[0]
	assert.Equal(t, pubkeyA, r["id"])
	assert.NotNil(t, r["last_attempt"])
	assert.Nil(t, r["last_success"])
	assert.Nil(t, r["battery_voltage"])
	assert.Nil(t, r["nb_sent"])
}

func TestEveryPairedGatewayIsPolledUnlessExcluded(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	a := &fakeNode{pubkey: pubkeyA, name: "A", firmware: 9}
	b := &fakeNode{pubkey: key("b"), name: "B", firmware: 9}
	private := &fakeNode{pubkey: key("c"), name: "private", firmware: 9}
	h.bridge("gw-a", a)
	h.bridge("gw-b", b)
	h.bridge("gw-private", private)

	h.poller(grid, "gw-private").Cycle(h.ctx)

	var ids []string
	for _, r := range grid.last(t) {
		ids = append(ids, r["id"].(string))
	}
	assert.ElementsMatch(t, []string{pubkeyA, key("b")}, ids, "no opt-in: every paired gateway but the excluded one")
	private.mu.Lock()
	defer private.mu.Unlock()
	assert.Empty(t, private.commands, "an excluded gateway gets no lease and no frames")
}

func TestARadioOnTwoGatewaysIsReportedOnceThroughWhicheverAnswers(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	// The same radio pinned by two gateways (a host moved it to a new
	// computer). gw-new connected last, so it is tried first.
	old := &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9}
	h.bridge("gw-old", old)
	time.Sleep(5 * time.Millisecond)
	cur := &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9}
	h.bridge("gw-new", cur)

	p := h.poller(grid)
	p.Cycle(h.ctx)
	reps := grid.last(t)
	require.Len(t, reps, 1, "one radio, one entry")
	assert.Equal(t, true, reps[0]["online"])
	old.mu.Lock()
	assert.Empty(t, old.commands, "read through the most recently seen gateway; the other isn't touched")
	old.mu.Unlock()

	// gw-new busy: the radio is read through gw-old instead.
	human, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "gw-new", Client: "op",
		Role: meshrelay.RoleOperator, Holder: newHolder()})
	require.NoError(t, err)
	defer h.relay.Release(human)
	p.Cycle(h.ctx)
	reps = grid.last(t)
	require.Len(t, reps, 1)
	assert.Equal(t, true, reps[0]["online"])
	old.mu.Lock()
	assert.Contains(t, old.commands, cmdAppStart, "fell back to the other gateway")
	old.mu.Unlock()
}

func TestStaleGatewaysAreNotPolled(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	// Paired a month ago and never seen since: a long-gone dev gateway.
	require.NoError(t, h.store.CreateGateway("gw-gone", meshrelay.TokenHash("smb_x"), key("d"), "",
		time.Now().Add(-30*24*time.Hour)))
	h.bridge("gw-a", &fakeNode{pubkey: pubkeyA, name: "A", firmware: 9})

	p := h.poller(grid)
	p.Cycle(h.ctx)
	reps := grid.last(t)
	require.Len(t, reps, 1, "the stale gateway's radio isn't reported as failing")
	assert.Equal(t, pubkeyA, reps[0]["id"])

	// Eight days on, gw-a is stale too: its radio leaves the report.
	p.cfg.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	p.Cycle(h.ctx)
	assert.Empty(t, grid.last(t))
}

func TestGatewayNodeAdvertIsForwardedOnChangeOrWhenOld(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := newSignedNode(t, "SIERRA L2PRO")
	disconnect := h.bridge("gw-a", node)
	now := time.Now()
	p := h.poller(grid)
	p.cfg.Now = func() time.Time { return now }

	p.Cycle(h.ctx)
	pk := grid.packets()
	require.Len(t, pk, 1, "the first read forwards the node's own advert")
	pkts := pk[0]["packets"].([]any)
	require.Len(t, pkts, 1)
	raw, err := hex.DecodeString(pkts[0].(map[string]any)["raw"].(string))
	require.NoError(t, err)
	adv, err := meshcore.DecodeFrame(raw)
	require.NoError(t, err, "the Grid's own decoder reads it")
	assert.Equal(t, node.pubkey, adv.PubKey)
	assert.True(t, adv.SignatureValid, "forwarded byte for byte, so the node's signature holds")
	assert.Equal(t, meshcore.RoleCompanion, adv.Role)
	assert.Equal(t, "SIERRA L2PRO", adv.Name)
	assert.False(t, adv.HasLocation, "no location is invented")

	now = now.Add(5 * time.Minute)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 1, "a re-signed advert saying the same thing isn't sent every poll")

	node.setName("SIERRA L2PRO 2")
	now = now.Add(5 * time.Minute)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 2, "a changed advert is forwarded")

	now = now.Add(12 * time.Hour)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 3, "an unchanged one is re-sent when the last is 12h old")

	// Offline: no read, so no advert, however old the last one is.
	disconnect()
	eventually(t, func() bool {
		_, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "gw-a", Client: "probe",
			Role: meshrelay.RoleAgent, Holder: newHolder()})
		return err != nil && !strings.Contains(err.Error(), "busy")
	}, "bridge never went offline")
	now = now.Add(13 * time.Hour)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 3, "an old advert never stands in for a read that failed")
	assert.Equal(t, false, grid.last(t)[0]["online"])
}

func TestFirmwareThatRefusesTheExportStillReports(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	h.bridge("gw-a", &fakeNode{pubkey: pubkeyA, name: "n", firmware: 9}) // answers ERR to the export

	h.poller(grid).Cycle(h.ctx)

	require.Len(t, grid.last(t), 1)
	assert.Equal(t, true, grid.last(t)[0]["online"])
	assert.Empty(t, grid.packets(), "no advert to forward")
}

func TestSelfAdvertOfChecksTheKeyAndLayout(t *testing.T) {
	node := newSignedNode(t, "x")
	raw := node.selfAdvert()
	assert.Equal(t, []byte{0x81, 'x'}, selfAdvertOf(raw, node.pubkey))
	assert.Nil(t, selfAdvertOf(raw, pubkeyA), "another node's advert")
	assert.Nil(t, selfAdvertOf(raw[:50], node.pubkey), "truncated")
	assert.Nil(t, selfAdvertOf([]byte{0x80, 0x00}, node.pubkey), "not an advert")
	// A transport-flood route carries 4 bytes of transport codes before path_len.
	transport := append([]byte{payloadAdvert << 2, 1, 2, 3, 4}, raw[1:]...)
	assert.Equal(t, []byte{0x81, 'x'}, selfAdvertOf(transport, node.pubkey))
}

func TestReportJSONSendsNullsExplicitly(t *testing.T) {
	b, err := json.Marshal(ReportEntry{ID: pubkeyA})
	require.NoError(t, err)
	assert.True(t, bytes.Contains(b, []byte(`"battery_voltage":null`)), string(b))
	assert.True(t, bytes.Contains(b, []byte(`"last_success":null`)), string(b))
}

func TestExcludingAGatewayExcludesItsRadioEverywhere(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	// One radio, two gateways (bridge-dev and mission-gateway in production).
	h.bridge("gw-old", &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9})
	other := &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9}
	h.bridge("gw-new", other)
	h.bridge("gw-b", &fakeNode{pubkey: key("b"), name: "B", firmware: 9})

	h.poller(grid, "gw-old").Cycle(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1)
	assert.Equal(t, key("b"), reps[0]["id"], "the excluded radio isn't read through the other gateway")
	other.mu.Lock()
	defer other.mu.Unlock()
	assert.Empty(t, other.commands)
}

func TestAPausedGatewayDoesNotHideAnOfflineRadio(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	// The radio's own gateway is offline (paired, bridge never connected);
	// another gateway still pins it, online but paused by its host.
	require.NoError(t, h.store.CreateGateway("gw-offline", meshrelay.TokenHash("smb_x"), pubkeyA, "",
		time.Now().Add(-time.Hour)))
	h.bridge("gw-paused", &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9})
	h.setNodeState("gw-paused", "paused")
	h.waitPaused("gw-paused")

	h.poller(grid).Cycle(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1, "a failed attempt, not a silent skip")
	assert.Equal(t, false, reps[0]["online"])
	assert.NotNil(t, reps[0]["last_attempt"])
}

func TestPausingTheGatewayThatHasTheRadioRecordsNoAttempt(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	require.NoError(t, h.store.CreateGateway("gw-offline", meshrelay.TokenHash("smb_x"), pubkeyA, "",
		time.Now().Add(-time.Hour)))
	h.bridge("gw-home", &fakeNode{pubkey: pubkeyA, name: "Elmer", firmware: 9})
	p := h.poller(grid)
	p.Cycle(h.ctx)
	first := grid.last(t)
	require.Len(t, first, 1)
	require.Equal(t, true, first[0]["online"])

	// The host pauses the gateway that last read it: the host's right, not an
	// outage, even though the other gateway pinning it is offline.
	h.setNodeState("gw-home", "paused")
	h.waitPaused("gw-home")
	later := time.Now().Add(10 * time.Minute) // a recorded attempt would show
	p.cfg.Now = func() time.Time { return later }
	p.Cycle(h.ctx)
	again := grid.last(t)
	require.Len(t, again, 1)
	require.NotNil(t, first[0]["last_attempt"])
	assert.Equal(t, first[0]["last_attempt"], again[0]["last_attempt"], "no attempt recorded")
	assert.Equal(t, first[0]["last_success"], again[0]["last_success"])
}

func TestGPSWanderIsNotAChangedAdvert(t *testing.T) {
	h := newHarness(t)
	grid := newFakeGrid(t)
	node := newSignedNode(t, "SIERRA L2PRO")
	node.setLocation(38_166_983, -120_405_619)
	h.bridge("gw-a", node)
	now := time.Now()
	p := h.poller(grid)
	p.cfg.Now = func() time.Time { return now }

	p.Cycle(h.ctx)
	require.Len(t, grid.packets(), 1)

	node.setLocation(38_167_012, -120_405_588) // a few metres
	now = now.Add(5 * time.Minute)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 1, "GPS jitter isn't forwarded")

	node.setLocation(38_180_000, -120_405_588) // about 1.5 km
	now = now.Add(5 * time.Minute)
	p.Cycle(h.ctx)
	assert.Len(t, grid.packets(), 2, "a real move is")
}

func TestSelfAdvertKey(t *testing.T) {
	loc := func(lat, lon int32, rest string) []byte {
		b := []byte{0x91}
		b = binary.LittleEndian.AppendUint32(b, uint32(lat))
		b = binary.LittleEndian.AppendUint32(b, uint32(lon))
		return append(b, rest...)
	}
	assert.Equal(t, selfAdvertKey(loc(38_166_983, -120_405_619, "n")), selfAdvertKey(loc(38_167_100, -120_405_550, "n")))
	assert.NotEqual(t, selfAdvertKey(loc(38_166_983, -120_405_619, "n")), selfAdvertKey(loc(38_166_983, -120_405_619, "m")))
	assert.NotEqual(t, selfAdvertKey(loc(38_166_983, -120_405_619, "n")), selfAdvertKey(loc(38_170_000, -120_405_619, "n")))
	assert.Equal(t, selfAdvertKey([]byte{0x81, 'n'}), "\x81n")
	assert.Equal(t, "", selfAdvertKey(nil))
}

// waitPaused waits until the relay refuses gw's node as paused.
func (h *harness) waitPaused(gw string) {
	h.t.Helper()
	eventually(h.t, func() bool {
		l, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: gw, Client: "probe",
			Role: meshrelay.RoleAgent, Holder: newHolder()})
		if err == nil {
			h.relay.Release(l)
			return false
		}
		var u *meshrelay.UnavailableError
		return errors.As(err, &u) && pausedCode(u.Code)
	}, "gateway never paused")
}
