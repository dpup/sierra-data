// Command test-mesh-relay is the CLI diagnostic for the SIERRA Mesh Relay
// (peer of test-meshcore/test-pge). It talks to a RUNNING relay from the
// outside, over the wire protocol (PROTOCOL.md in
// github.com/sierragridteam/sierra-meshrelay), so it
// exercises the same path a real bridge and client do — including whatever
// reverse proxy sits in front.
//
// It deliberately does not import the relay's protocol types: it is a second,
// independent implementation of the wire protocol, and doubles as a reference
// for bridge authors.
//
// Modes:
//
//	e2e     (default) full scenario against a throwaway gateway "e2e-<hex>":
//	        bridge hello/welcome, status (admin and public), lease, frame round trip, busy (4029),
//	        renew, relink, audit, session replacement (4009), pubkey mismatch +
//	        accept, token rotation (4001), delete. Needs an ADMIN token. It
//	        cleans up its gateway, and makes exactly two failed auth attempts,
//	        well under the relay's lockout threshold.
//	bridge  a long-lived fake bridge + fake node (echoes every frame back with a
//	        0x80 prefix) for an existing gateway. Needs the gateway's smb_ token.
//	client  take a lease and drive a node by hand: type hex frames on stdin
//	        ("01 03 ..."), see replies as hex; "renew" renews. -app-start sends
//	        a companion APP_START and decodes the SELF_INFO reply, which is the
//	        quickest check that a REAL node behind a real bridge is reachable.
//
// Examples:
//
//	./bin/test-mesh-relay -token "$TOKEN"                                # e2e on localhost:8090
//	./bin/test-mesh-relay -url https://data.sierragridteam.org -token "$TOKEN"
//	./bin/test-mesh-relay bridge -gateway test-gw -token "$BRIDGE"
//	./bin/test-mesh-relay client -gateway avery-ridge -token "$TOKEN" -app-start
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	bridgeProto = "sierra-mesh-bridge.v1"
	clientProto = "sierra-mesh-client.v1"
	prefix      = "/mesh/relay"
	timeout     = 5 * time.Second
)

func main() {
	mode := "e2e"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	base := fs.String("url", envOr("MESH_RELAY_URL", "http://localhost:8090"), "relay base URL (env MESH_RELAY_URL)")
	token := fs.String("token", os.Getenv("MESH_RELAY_TOKEN"), "token: admin smc_ for e2e/client, smb_ for bridge (env MESH_RELAY_TOKEN)")
	gateway := fs.String("gateway", "", "gateway name (bridge, client)")
	pubkey := fs.String("pubkey", "", "node pubkey the fake bridge reports (bridge; default random)")
	appStart := fs.Bool("app-start", false, "send APP_START after the lease and decode SELF_INFO (client)")
	preempt := fs.Bool("preempt", false, "take the lease from its holder (client; admin only)")
	keep := fs.Bool("keep", false, "don't delete the e2e gateway afterwards (e2e)")
	_ = fs.Parse(args)
	*base = strings.TrimRight(*base, "/")
	if *token == "" {
		fatal("a token is required (-token or MESH_RELAY_TOKEN)")
	}

	switch mode {
	case "e2e":
		os.Exit(runE2E(*base, *token, *keep))
	case "bridge":
		requireGateway(*gateway)
		runBridge(*base, *gateway, *token, *pubkey)
	case "client":
		requireGateway(*gateway)
		runClient(*base, *gateway, *token, *appStart, *preempt)
	default:
		fatal("unknown mode %q (want e2e, bridge or client)", mode)
	}
}

// --- HTTP admin API -----------------------------------------------------------

type api struct{ base, token string }

func (a api) call(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+prefix+"/api"+path, rd)
	if err != nil {
		return 0, err
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding %s %s: %w (body %.200q)", method, path, err, b)
		}
	}
	return resp.StatusCode, nil
}

// --- websocket peer -------------------------------------------------------------

// peer is one websocket with its inbound messages fanned into channels.
type peer struct {
	ws     *websocket.Conn
	ctl    chan map[string]any
	bin    chan []byte
	closed chan error
}

func wsURL(base, path string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + prefix + path
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://") + prefix + path
	}
	return base + prefix + path
}

// dial connects; echo makes it a fake node that answers each frame with 0x80+frame.
func dial(base, path, token, proto string, echo bool) (*peer, int, error) {
	d := websocket.Dialer{HandshakeTimeout: timeout, Subprotocols: []string{proto}}
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	ws, resp, err := d.Dial(wsURL(base, path), hdr)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return nil, status, err
	}
	p := &peer{ws: ws, ctl: make(chan map[string]any, 64), bin: make(chan []byte, 256), closed: make(chan error, 1)}
	go p.loop(echo)
	return p, status, nil
}

func (p *peer) loop(echo bool) {
	for {
		kind, data, err := p.ws.ReadMessage()
		if err != nil {
			p.closed <- err
			return
		}
		if kind == websocket.BinaryMessage {
			if echo {
				_ = p.ws.WriteMessage(websocket.BinaryMessage, append([]byte{0x80}, data...))
				continue
			}
			p.bin <- data
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			p.ctl <- m
		}
	}
}

func (p *peer) send(v any) error { return p.ws.WriteJSON(v) }

// expect waits for a control message of type typ, skipping others.
func (p *peer) expect(typ string) (map[string]any, error) {
	deadline := time.After(timeout)
	for {
		select {
		case m := <-p.ctl:
			if m["type"] == typ {
				return m, nil
			}
		case err := <-p.closed:
			p.closed <- err
			// The reader queues a final message (e.g. an error) before it sees
			// the close that follows it; drain what arrived first.
			for {
				select {
				case m := <-p.ctl:
					if m["type"] == typ {
						return m, nil
					}
					continue
				default:
				}
				break
			}
			return nil, fmt.Errorf("closed while waiting for %q: %v", typ, err)
		case <-deadline:
			return nil, fmt.Errorf("no %q message within %s", typ, timeout)
		}
	}
}

func (p *peer) expectFrame() ([]byte, error) {
	select {
	case b := <-p.bin:
		return b, nil
	case err := <-p.closed:
		p.closed <- err
		select {
		case b := <-p.bin:
			return b, nil
		default:
		}
		return nil, fmt.Errorf("closed while waiting for a frame: %v", err)
	case <-time.After(timeout):
		return nil, fmt.Errorf("no frame within %s", timeout)
	}
}

func (p *peer) expectClose(code int) error {
	select {
	case err := <-p.closed:
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			if ce.Code == code {
				return nil
			}
			return fmt.Errorf("closed with %d %q, want %d", ce.Code, ce.Text, code)
		}
		return fmt.Errorf("connection ended without a close frame (%v), want %d", err, code)
	case <-time.After(timeout):
		return fmt.Errorf("not closed within %s (want %d)", timeout, code)
	}
}

func (p *peer) close() {
	_ = p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
	_ = p.ws.Close()
}

func hello(gateway, pubkey string) map[string]any {
	return map[string]any{
		"type": "hello", "protocol": 1, "gateway": gateway,
		"agent": map[string]any{"name": "test-mesh-relay", "version": "0", "os": "test", "arch": "test"},
		"link":  map[string]any{"kind": "tcp", "detail": "fake-node"},
		"node":  map[string]any{"state": "connected", "pubkey": pubkey, "name": "fake-" + gateway, "fw": "fake"},
	}
}

// connectBridge dials as a bridge and completes hello/welcome.
func connectBridge(base, gateway, token, pubkey string) (*peer, map[string]any, error) {
	p, status, err := dial(base, "/"+gateway+"/dial", token, bridgeProto, true)
	if err != nil {
		return nil, nil, fmt.Errorf("dial: HTTP %d: %v", status, err)
	}
	if err := p.send(hello(gateway, pubkey)); err != nil {
		return nil, nil, err
	}
	w, err := p.expect("welcome")
	if err != nil {
		p.close()
		return nil, nil, err
	}
	return p, w, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- e2e ------------------------------------------------------------------------

type runner struct{ failed, passed int }

func (r *runner) step(name string, fn func() (string, error)) bool {
	start := time.Now()
	detail, err := fn()
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		r.failed++
		fmt.Printf("FAIL  %-44s %v\n", name, err)
		return false
	}
	r.passed++
	if detail != "" {
		detail = "  " + detail
	}
	fmt.Printf("PASS  %-44s %6s%s\n", name, took, detail)
	return true
}

type gatewayStatus struct {
	Online         bool   `json:"online"`
	PubkeyStatus   string `json:"pubkey_status"`
	PinnedPubkey   string `json:"pinned_pubkey"`
	MismatchPubkey string `json:"mismatch_pubkey"`
}

func runE2E(base, token string, keep bool) int {
	a := api{base: base, token: token}
	r := &runner{}
	gw := "e2e-" + randomHex(3)
	keyA, keyB := randomHex(32), randomHex(32)
	var bridgeTok, leaseID string
	var bridge, client *peer
	fmt.Printf("Mesh relay E2E against %s (gateway %s)\n\n", base, gw)

	if !r.step("healthz", func() (string, error) {
		resp, err := (&http.Client{Timeout: timeout}).Get(base + prefix + "/healthz")
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return "", nil
	}) {
		return summary(r)
	}
	if !r.step("create gateway (admin token)", func() (string, error) {
		var out struct {
			BridgeToken string `json:"bridge_token"`
			Message     string `json:"message"`
		}
		code, err := a.call("POST", "/gateways", map[string]any{"name": gw, "notes": "test-mesh-relay e2e"}, &out)
		if err != nil {
			return "", err
		}
		if code != 200 || !strings.HasPrefix(out.BridgeToken, "smb_") {
			return "", fmt.Errorf("HTTP %d %s (an admin token is required)", code, out.Message)
		}
		bridgeTok = out.BridgeToken
		return "", nil
	}) {
		return summary(r)
	}
	if !keep {
		defer func() {
			code, err := a.call("DELETE", "/gateways/"+gw, nil, nil)
			if err != nil || (code != 200 && code != 404) {
				fmt.Printf("WARN  could not delete gateway %s (HTTP %d, %v)\n", gw, code, err)
			}
		}()
	}

	r.step("bridge with a bad token gets 401", func() (string, error) {
		_, status, err := dial(base, "/"+gw+"/dial", "smb_not-a-real-token", bridgeProto, false)
		if err == nil || status != 401 {
			return "", fmt.Errorf("HTTP %d, want 401", status)
		}
		return "", nil
	})
	if !r.step("bridge hello -> welcome", func() (string, error) {
		var w map[string]any
		var err error
		bridge, w, err = connectBridge(base, gw, bridgeTok, keyA)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("session %v, ping %vs", w["session"], w["ping_interval_s"]), nil
	}) {
		return summary(r)
	}
	r.step("gateway online, pubkey pinned (within 2s)", func() (string, error) {
		deadline := time.Now().Add(2 * time.Second)
		var st gatewayStatus
		for time.Now().Before(deadline) {
			if _, err := a.call("GET", "/gateways/"+gw, nil, &st); err != nil {
				return "", err
			}
			if st.Online && st.PubkeyStatus == "pinned" && st.PinnedPubkey == keyA {
				return "", nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return "", fmt.Errorf("online=%v pubkey_status=%q", st.Online, st.PubkeyStatus)
	})
	r.step("public status: listed, ready, no pubkey", func() (string, error) {
		resp, err := (&http.Client{Timeout: timeout}).Get(base + prefix + "/api/status")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("HTTP %d without a token", resp.StatusCode)
		}
		if bytes.Contains(raw, []byte(keyA)) {
			return "", errors.New("public status exposes the node pubkey")
		}
		var out struct {
			Gateways []struct {
				Name  string `json:"name"`
				Ready bool   `json:"ready"`
			} `json:"gateways"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		for _, g := range out.Gateways {
			if g.Name == gw {
				if !g.Ready {
					return "", errors.New("listed but not ready")
				}
				return fmt.Sprintf("%d gateway(s) listed", len(out.Gateways)), nil
			}
		}
		return "", fmt.Errorf("%s not listed", gw)
	})
	if !r.step("client lease; bridge gets attach", func() (string, error) {
		var err error
		var status int
		client, status, err = dial(base, "/"+gw, token, clientProto, false)
		if err != nil {
			return "", fmt.Errorf("HTTP %d: %v", status, err)
		}
		l, err := client.expect("lease")
		if err != nil {
			return "", err
		}
		leaseID, _ = l["lease"].(string)
		at, err := bridge.expect("attach")
		if err != nil {
			return "", err
		}
		if at["lease"] != leaseID {
			return "", fmt.Errorf("attach for %v, lease is %s", at["lease"], leaseID)
		}
		return fmt.Sprintf("%s, expires %v", leaseID, l["expires_at"]), nil
	}) {
		return summary(r)
	}
	const frames = 20
	r.step(fmt.Sprintf("%d frames round trip unchanged", frames), func() (string, error) {
		var total time.Duration
		for i := 0; i < frames; i++ {
			f := make([]byte, 1+i*9%176)
			// Unassigned opcodes: the relay refuses some real commands to
			// every lease (sierra-meshrelay nodepolicy), and a random body
			// behind one (e.g. 9, ADD_UPDATE_CONTACT) can trip it.
			f[0] = 0xc0 + byte(i)
			_, _ = rand.Read(f[1:])
			start := time.Now()
			if err := client.ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
				return "", err
			}
			got, err := client.expectFrame()
			if err != nil {
				return "", err
			}
			total += time.Since(start)
			if !bytes.Equal(got, append([]byte{0x80}, f...)) {
				return "", fmt.Errorf("frame %d came back altered", i)
			}
		}
		return fmt.Sprintf("avg RTT %s", (total / frames).Round(100*time.Microsecond)), nil
	})
	r.step("second client gets busy (4029)", func() (string, error) {
		c2, status, err := dial(base, "/"+gw, token, clientProto, false)
		if err != nil {
			return "", fmt.Errorf("HTTP %d: %v", status, err)
		}
		defer c2.close()
		if e, err := c2.expect("error"); err != nil || e["code"] != "busy" {
			return "", fmt.Errorf("want error busy, got %v %v", e, err)
		}
		return "", c2.expectClose(4029)
	})
	r.step("renew -> new lease message", func() (string, error) {
		if err := client.send(map[string]any{"type": "renew", "lease": leaseID}); err != nil {
			return "", err
		}
		l, err := client.expect("lease")
		if err != nil {
			return "", err
		}
		if l["lease"] != leaseID {
			return "", fmt.Errorf("renewed %v, want %s", l["lease"], leaseID)
		}
		return fmt.Sprintf("expires %v", l["expires_at"]), nil
	})
	r.step("relink reaches the bridge", func() (string, error) {
		code, err := a.call("POST", "/gateways/"+gw+"/relink", nil, nil)
		if err != nil || code != 200 {
			return "", fmt.Errorf("HTTP %d %v", code, err)
		}
		_, err = bridge.expect("relink")
		return "", err
	})
	r.step("client hangs up -> detach + audit row", func() (string, error) {
		client.close()
		if _, err := bridge.expect("detach"); err != nil {
			return "", err
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var out struct {
				Recent []struct {
					ID           string `json:"id"`
					EndReason    string `json:"end_reason"`
					FramesToNode int    `json:"frames_to_node"`
				} `json:"recent"`
			}
			if _, err := a.call("GET", "/leases?limit=50", nil, &out); err != nil {
				return "", err
			}
			for _, l := range out.Recent {
				if l.ID == leaseID {
					if l.FramesToNode != frames {
						return "", fmt.Errorf("audit says %d frames to node, want %d", l.FramesToNode, frames)
					}
					return "reason " + l.EndReason, nil
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return "", fmt.Errorf("lease %s not in the audit log", leaseID)
	})
	r.step("second bridge replaces the first (4009)", func() (string, error) {
		b2, _, err := connectBridge(base, gw, bridgeTok, keyA)
		if err != nil {
			return "", err
		}
		old := bridge
		bridge = b2
		return "", old.expectClose(4009)
	})
	r.step("pubkey change -> mismatch, leases blocked", func() (string, error) {
		bridge.close()
		time.Sleep(200 * time.Millisecond)
		b3, _, err := connectBridge(base, gw, bridgeTok, keyB)
		if err != nil {
			return "", err
		}
		bridge = b3
		var st gatewayStatus
		if _, err := a.call("GET", "/gateways/"+gw, nil, &st); err != nil {
			return "", err
		}
		if st.PubkeyStatus != "mismatch" || st.MismatchPubkey != keyB {
			return "", fmt.Errorf("pubkey_status=%q mismatch=%q", st.PubkeyStatus, st.MismatchPubkey)
		}
		c, status, err := dial(base, "/"+gw, token, clientProto, false)
		if err != nil {
			return "", fmt.Errorf("HTTP %d: %v", status, err)
		}
		defer c.close()
		if e, err := c.expect("error"); err != nil || e["code"] != "pubkey_mismatch" {
			return "", fmt.Errorf("want error pubkey_mismatch, got %v %v", e, err)
		}
		return "", c.expectClose(1000)
	})
	r.step("accept-pubkey -> leases work again", func() (string, error) {
		code, err := a.call("POST", "/gateways/"+gw+"/accept-pubkey", nil, nil)
		if err != nil || code != 200 {
			return "", fmt.Errorf("HTTP %d %v", code, err)
		}
		c, status, err := dial(base, "/"+gw, token, clientProto, false)
		if err != nil {
			return "", fmt.Errorf("HTTP %d: %v", status, err)
		}
		defer c.close()
		_, err = c.expect("lease")
		return "", err
	})
	r.step("rotate token closes the bridge (4001)", func() (string, error) {
		var out struct {
			BridgeToken string `json:"bridge_token"`
		}
		code, err := a.call("POST", "/gateways/"+gw+"/rotate-token", nil, &out)
		if err != nil || code != 200 {
			return "", fmt.Errorf("HTTP %d %v", code, err)
		}
		if err := bridge.expectClose(4001); err != nil {
			return "", err
		}
		if _, status, err := dial(base, "/"+gw+"/dial", bridgeTok, bridgeProto, false); err == nil || status != 401 {
			return "", fmt.Errorf("old token: HTTP %d, want 401", status)
		}
		b, _, err := connectBridge(base, gw, out.BridgeToken, keyB)
		if err != nil {
			return "", fmt.Errorf("new token: %v", err)
		}
		bridge = b
		return "", nil
	})
	if !keep {
		r.step("delete gateway closes the bridge (4001)", func() (string, error) {
			code, err := a.call("DELETE", "/gateways/"+gw, nil, nil)
			if err != nil || code != 200 {
				return "", fmt.Errorf("HTTP %d %v", code, err)
			}
			if err := bridge.expectClose(4001); err != nil {
				return "", err
			}
			if code, _ := a.call("GET", "/gateways/"+gw, nil, nil); code != 404 {
				return "", fmt.Errorf("gateway still there (HTTP %d)", code)
			}
			return "", nil
		})
	} else if bridge != nil {
		bridge.close()
	}
	return summary(r)
}

func summary(r *runner) int {
	fmt.Printf("\n%d passed, %d failed\n", r.passed, r.failed)
	if r.failed > 0 {
		return 1
	}
	return 0
}

// --- bridge mode ----------------------------------------------------------------

func runBridge(base, gateway, token, pubkey string) {
	if pubkey == "" {
		pubkey = randomHex(32)
	}
	p, w, err := connectBridge(base, gateway, token, pubkey)
	if err != nil {
		fatal("bridge: %v", err)
	}
	fmt.Printf("connected as bridge for %s (session %v), fake node pubkey %s\n", gateway, w["session"], pubkey)
	fmt.Println("echoing every frame back with a 0x80 prefix; Ctrl-C to stop")
	for {
		select {
		case m := <-p.ctl:
			b, _ := json.Marshal(m)
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), b)
		case err := <-p.closed:
			fatal("bridge: %v", err)
		}
	}
}

// --- client mode ----------------------------------------------------------------

// Companion protocol codes (MeshCore firmware, companion_radio).
const (
	cmdAppStart  = 0x01
	respSelfInfo = 0x05
)

func runClient(base, gateway, token string, appStart, preempt bool) {
	path := "/" + gateway
	if preempt {
		path += "?preempt=1"
	}
	p, status, err := dial(base, path, token, clientProto, false)
	if err != nil {
		fatal("client: HTTP %d: %v", status, err)
	}
	var leaseID string
	select {
	case m := <-p.ctl:
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
		if m["type"] != "lease" {
			if err := <-p.closed; err != nil {
				fatal("no lease: %v", err)
			}
			os.Exit(1)
		}
		leaseID, _ = m["lease"].(string)
	case err := <-p.closed:
		fatal("client: %v", err)
	}
	if appStart {
		// APP_START: code, app_ver, 6 reserved bytes, app name.
		f := append([]byte{cmdAppStart, 1, 0, 0, 0, 0, 0, 0}, "test-mesh-relay"...)
		if err := p.ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
			fatal("client: %v", err)
		}
		fmt.Printf("> %s\n", hex.EncodeToString(f))
	}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			switch {
			case line == "":
			case line == "renew":
				_ = p.send(map[string]any{"type": "renew", "lease": leaseID})
			default:
				b, err := hex.DecodeString(strings.ReplaceAll(line, " ", ""))
				if err != nil {
					fmt.Println("! not hex:", err)
					continue
				}
				_ = p.ws.WriteMessage(websocket.BinaryMessage, b)
			}
		}
	}()
	for {
		select {
		case m := <-p.ctl:
			b, _ := json.Marshal(m)
			fmt.Println(string(b))
		case f := <-p.bin:
			fmt.Printf("< %s\n", hex.EncodeToString(f))
			if len(f) >= 36 && f[0] == respSelfInfo {
				// SELF_INFO: code, adv type, tx power, max tx power, pubkey[32], …, name at the end.
				// Best-effort decode: the hex above is authoritative.
				fmt.Printf("  SELF_INFO pubkey %s\n", hex.EncodeToString(f[4:36]))
			}
		case err := <-p.closed:
			fmt.Println("closed:", err)
			return
		}
	}
}

// --- helpers ----------------------------------------------------------------------

func requireGateway(g string) {
	if g == "" {
		fatal("-gateway is required")
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "test-mesh-relay: "+format+"\n", args...)
	os.Exit(2)
}
