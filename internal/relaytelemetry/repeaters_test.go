package relaytelemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

	"github.com/sierragridteam/sierra-meshrelay/meshops/meshsim"
	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// newRepeaterHarness is newHarness with a secret key, so repeaters can be
// shared.
func newRepeaterHarness(t *testing.T) *harness {
	t.Helper()
	ctx := logging.EnsureLogger(context.Background())
	st, err := meshrelay.OpenStore(ctx, filepath.Join(t.TempDir(), "relay.db"), time.Second)
	require.NoError(t, err)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	r := meshrelay.New(ctx, meshrelay.Config{TickInterval: 20 * time.Millisecond, SecretKey: key}, st)
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

// meshBridge connects a fake bridge for gw whose node is a meshsim node
// (contacts, repeater logins, status). It returns the bridge token.
func (h *harness) meshBridge(gw, pubkey string, mesh *meshsim.Node) string {
	h.t.Helper()
	tok, err := meshrelay.MintToken(meshrelay.BridgeTokenPrefix)
	require.NoError(h.t, err)
	require.NoError(h.t, h.store.CreateGateway(gw, meshrelay.TokenHash(tok), pubkey, "", time.Now()))
	d := websocket.Dialer{Subprotocols: []string{meshrelay.BridgeSubprotocol}}
	ws, _, err := d.Dial("ws://"+h.base+meshrelay.PathPrefix+"/"+gw+"/dial",
		http.Header{"Authorization": {"Bearer " + tok}})
	require.NoError(h.t, err)
	require.NoError(h.t, ws.WriteJSON(map[string]any{
		"type": "hello", "protocol": 1, "gateway": gw,
		"agent": map[string]any{"name": "meshbridge", "version": "test"},
		"link":  map[string]any{"kind": "tcp"},
		"node":  map[string]any{"state": "connected", "pubkey": pubkey, "name": gw},
	}))
	var mu sync.Mutex
	write := func(f []byte) {
		mu.Lock()
		defer mu.Unlock()
		_ = ws.WriteMessage(websocket.BinaryMessage, f)
	}
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage || len(data) == 0 {
				continue
			}
			if data[0] == cmdAppStart {
				f := make([]byte, selfInfoNameOff)
				f[0] = respSelfInfo
				k, _ := hex.DecodeString(pubkey)
				copy(f[selfInfoPubkeyOff:], k)
				write(append(f, gw...))
				continue
			}
			if !mesh.Handle(data, write) {
				write([]byte{respErr, 1})
			}
		}
	}()
	h.t.Cleanup(func() { ws.Close() })
	eventually(h.t, func() bool {
		l, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: gw, Client: "probe",
			Role: meshrelay.RoleAgent, Holder: newHolder()})
		if err == nil {
			h.relay.Release(l)
		}
		return err == nil
	}, "bridge never came online")
	return tok
}

// share shares a repeater as the host behind tok would (`meshbridge repeater add`).
func (h *harness) share(tok, pubkey, password string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"pubkey": pubkey, "password": password})
	req, _ := http.NewRequest(http.MethodPost, "http://"+h.base+meshrelay.PathPrefix+"/api/gateway/repeaters", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	require.Equal(h.t, http.StatusOK, resp.StatusCode, "sharing the repeater")
}

func key(c string) string { return strings.Repeat(c, 64) }

// cycleAll runs due repeater work, then a gateway cycle (which pushes the
// report).
func (p *Poller) cycleAll(ctx context.Context) {
	p.RepeaterTick(ctx)
	p.Cycle(ctx)
}

// dueNow schedules every shared repeater's first cycle for now, instead of
// at its offset.
func (p *Poller) dueNow(ctx context.Context) {
	p.RepeaterTick(ctx)
	for _, j := range p.jobs {
		j.state.NextCycle = p.cfg.Now()
	}
}

func TestSharedRepeaterIsPolledThroughAGatewayThatHeardIt(t *testing.T) {
	h := newRepeaterHarness(t)
	grid := newFakeGrid(t)
	rpt := &meshsim.Repeater{Pubkey: key("e"), Name: "Ridge RPT", GuestPassword: "look", BatteryMV: 3950, UptimeS: 86400, TempC: 11.5}
	cabin := meshsim.New(rpt)
	deaf := meshsim.New()
	deaf.AddInRange(rpt)
	tok := h.meshBridge("cabin", key("1"), cabin)
	h.meshBridge("deaf", key("2"), deaf)
	h.share(tok, rpt.Pubkey, "look")

	now := time.Now()
	// The gateways' own nodes are excluded here: this test is about the repeater.
	p := New(Config{Timeout: time.Second, GridURL: grid.srv.URL, IngestToken: "ingest-secret",
		ExcludeGateways: []string{"cabin", "deaf"}, Now: func() time.Time { return now }}, h.relay, h.store, h.relay)
	p.dueNow(h.ctx)
	p.cycleAll(h.ctx)

	reps := grid.last(t)
	require.Len(t, reps, 1, "only the shared repeater")
	r := reps[0]
	assert.Equal(t, rpt.Pubkey, r["id"])
	assert.Equal(t, "Ridge RPT", r["name"])
	assert.Equal(t, true, r["online"])
	assert.InDelta(t, 3.95, r["battery_voltage"], 1e-9)
	assert.InDelta(t, 11.5, r["temperature_c"], 1e-9)
	assert.EqualValues(t, 86400, r["uptime_s"])
	assert.EqualValues(t, -110, r["noise_floor_dbm"])
	assert.EqualValues(t, 120000, r["airtime_ms"])
	assert.InDelta(t, 5.5, r["last_snr_db"], 1e-9)
	assert.EqualValues(t, 0, r["flood_dups"], "read, so zero is a real zero")
	assert.False(t, deaf.HasContact(rpt.Pubkey), "the poller never adds contacts to a node that hasn't heard it")

	rec, err := h.store.Repeater(rpt.Pubkey)
	require.NoError(t, err)
	assert.Equal(t, "cabin", rec.LastGateway)
	assert.Empty(t, rec.LastError)

	// Within the repeater interval, the next cycle doesn't transmit again.
	statusReqs := cabin.StatusReqs
	now = now.Add(5 * time.Minute)
	p.cycleAll(h.ctx)
	assert.Equal(t, statusReqs, cabin.StatusReqs)
	require.Len(t, grid.last(t), 1, "but the repeater stays in the complete set")

	// The password changes: a failed attempt, last readings kept, and the
	// host sees why.
	cabin.Update(rpt.Pubkey, func(r *meshsim.Repeater) { r.GuestPassword = "new" })
	first := grid.last(t)[0]
	now = now.Add(30 * time.Minute)
	p.cycleAll(h.ctx)
	r = grid.last(t)[0]
	assert.Equal(t, false, r["online"])
	assert.Greater(t, r["last_attempt"], first["last_attempt"])
	assert.Equal(t, first["last_success"], r["last_success"])
	assert.Equal(t, first["uptime_s"], r["uptime_s"])
	rec, _ = h.store.Repeater(rpt.Pubkey)
	eventually(t, func() bool {
		rec, _ = h.store.Repeater(rpt.Pubkey)
		return rec.LastError != ""
	}, "the registry never recorded the failure")
	assert.Contains(t, rec.LastError, "refused the password")
	assert.Equal(t, 1, cabin.Logins-2, "a refused password isn't retried through other gateways")

	// Unshared: it leaves the report.
	require.NoError(t, h.store.DeleteRepeater(rpt.Pubkey))
	now = now.Add(30 * time.Minute)
	p.cycleAll(h.ctx)
	assert.Empty(t, grid.last(t))
}

func TestRepeaterNobodyHeardIsNotAnAttempt(t *testing.T) {
	h := newRepeaterHarness(t)
	grid := newFakeGrid(t)
	rpt := &meshsim.Repeater{Pubkey: key("f"), Name: "Hidden", GuestPassword: "g", BatteryMV: 4000}
	cabin := meshsim.New(rpt)
	tok := h.meshBridge("cabin", key("3"), cabin)
	h.share(tok, rpt.Pubkey, "g")

	// Someone is using the only gateway that heard it: busy, so no attempt.
	human, err := h.relay.Acquire(meshrelay.LeaseRequest{Gateway: "cabin", Client: "op",
		Role: meshrelay.RoleOperator, Holder: newHolder()})
	require.NoError(t, err)
	p := New(Config{Timeout: time.Second, GridURL: grid.srv.URL, IngestToken: "x"}, h.relay, h.store, h.relay)
	p.dueNow(h.ctx)
	p.cycleAll(h.ctx)
	assert.Empty(t, grid.last(t), "never attempted: not reported yet")
	h.relay.Release(human)
}

// packets returns the mesh.packet documents the fake Grid received.
func (g *fakeGrid) packets() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for i, a := range g.auth {
		if strings.HasPrefix(a, "/api/v1/ingest/mesh.packet ") {
			out = append(out, g.reports[i])
		}
	}
	return out
}

// An island repeater's advert never reaches a broker; the poller forwards
// the signed advert the gateway's node stored, once per new advert.
func TestRepeaterAdvertIsForwardedOncePerChange(t *testing.T) {
	h := newRepeaterHarness(t)
	grid := newFakeGrid(t)
	advert := []byte{0x11, 0x00, 0xaa, 0xbb, 0xcc} // header: flood ADVERT
	rpt := &meshsim.Repeater{Pubkey: key("9"), Name: "Island", GuestPassword: "g", BatteryMV: 4000, Advert: advert}
	island := meshsim.New(rpt)
	tok := h.meshBridge("island", key("4"), island)
	h.share(tok, rpt.Pubkey, "g")

	now := time.Now()
	p := New(Config{Timeout: time.Second, GridURL: grid.srv.URL, IngestToken: "x",
		Now: func() time.Time { return now }}, h.relay, h.store, h.relay)
	p.dueNow(h.ctx)
	p.cycleAll(h.ctx)
	pk := grid.packets()
	require.Len(t, pk, 1)
	pkts := pk[0]["packets"].([]any)
	require.Len(t, pkts, 1)
	assert.Equal(t, "1100aabbcc", pkts[0].(map[string]any)["raw"])
	assert.EqualValues(t, 4, pkts[0].(map[string]any)["packet_type"])
	assert.NotContains(t, pk[0], "observer", "the gateway's node stays unnamed")

	now = now.Add(31 * time.Minute)
	p.cycleAll(h.ctx)
	assert.Len(t, grid.packets(), 1, "the same advert isn't re-sent: the Grid would read it as fresh presence")

	island.Update(rpt.Pubkey, func(r *meshsim.Repeater) { r.Advert = []byte{0x11, 0x00, 0xdd} })
	now = now.Add(31 * time.Minute)
	p.cycleAll(h.ctx)
	require.Len(t, grid.packets(), 2, "a new advert is forwarded")
}
