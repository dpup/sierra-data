// Package relaytelemetry is the relay's in-process telemetry poller: it reads
// the companion node behind each opted-in gateway, the way an operator monitor
// (alanpi) reads repeaters, and reports to the Grid as a mesh.repeater
// reporter.
//
// It runs inside cmd/mesh-relay because the relay already has the node: it
// holds a lease like any client (relay.Acquire at PriorityBackground, so any
// person or agent connecting takes the node from it), sends frames with
// relay.Send and receives them through Deliver. It needs no relay client
// token. The one credential is the Grid ingest token, which can only post
// telemetry.
//
// Rules it keeps:
//   - Never log a frame payload. The node pushes messages and adverts while
//     the poller holds the lease; they are dropped unread.
//   - Busy (someone holds the lease), paused (the host's right) and preempted
//     polls record NO attempt: none of them says anything about the node.
//     An offline bridge or disconnected node is a failed attempt.
//   - A value the node did not answer is null, never zero.
//   - Each report is the complete set of opted-in gateways whose node key is
//     pinned; a gateway with no pinned key cannot be named and is left out.
package relaytelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	meshrelay "github.com/dpup/sierra-meshrelay/relay"
)

// ClientName is the lease holder's name in the relay's lease audit and the
// bridge's attach message.
const ClientName = "telemetry"

// Relay is what the poller needs from the relay: *meshrelay.Relay.
type Relay interface {
	Acquire(meshrelay.LeaseRequest) (*meshrelay.Lease, error)
	Send(*meshrelay.Lease, []byte) error
	Release(*meshrelay.Lease)
}

// Gateways lists the relay's gateways: *meshrelay.Store.
type Gateways interface {
	Gateways() ([]*meshrelay.GatewayRecord, error)
}

// Config configures a Poller.
type Config struct {
	// Gateways whose node is polled and published. Opt-in per gateway: the
	// relay's public status deliberately does not say which node sits behind
	// which gateway, and published telemetry can let someone match them up.
	Gateways []string
	Interval time.Duration // between polls (5m)
	// Timeout bounds one reply from the node (5s). A whole poll is a handful
	// of round trips.
	Timeout     time.Duration
	GridURL     string // e.g. https://data.sierragridteam.org
	IngestToken string // the Grid mesh.repeater reporter token
	HTTPClient  *http.Client
	Now         func() time.Time
}

// Poller polls gateway nodes and pushes their telemetry to the Grid.
type Poller struct {
	cfg   Config
	relay Relay
	gws   Gateways

	mu    sync.Mutex
	nodes map[string]*nodeState // by gateway
}

type nodeState struct {
	pubkey      string
	name        string
	lastAttempt time.Time
	lastSuccess time.Time
	sample      *Sample // last successful reading; nil if never reached
	outcome     string  // last poll outcome, to log changes only
}

// New returns a Poller.
func New(cfg Config, relay Relay, gws Gateways) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.GridURL = strings.TrimRight(cfg.GridURL, "/")
	return &Poller{cfg: cfg, relay: relay, gws: gws, nodes: map[string]*nodeState{}}
}

// Run polls every Interval until ctx ends. The first cycle waits one minute,
// so bridges that reconnected after a relay restart are back first.
func (p *Poller) Run(ctx context.Context, firstDelay time.Duration) {
	logging.Infow(ctx, "Relay telemetry poller started", "gateways", p.cfg.Gateways,
		"interval", p.cfg.Interval, "grid", p.cfg.GridURL)
	t := time.NewTimer(firstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.Cycle(ctx)
		t.Reset(p.cfg.Interval)
	}
}

// Cycle polls every opted-in gateway once and pushes the report.
func (p *Poller) Cycle(ctx context.Context) {
	recs, err := p.gws.Gateways()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing gateways failed; skipping this cycle", "error", err)
		return
	}
	pinned := map[string]string{}
	for _, r := range recs {
		pinned[r.Name] = r.PinnedPubkey
	}
	for _, gw := range p.cfg.Gateways {
		if ctx.Err() != nil {
			return
		}
		key := pinned[gw]
		if key == "" {
			p.forget(ctx, gw)
			continue
		}
		p.pollGateway(ctx, gw, key)
	}
	if ctx.Err() != nil {
		return
	}
	p.push(ctx)
}

// outcome of one poll.
const (
	outcomeOK      = "ok"
	outcomeSkipped = "skipped" // busy, paused, preempted, relay stopping: no attempt
	outcomeFailed  = "failed"  // tried and could not read the node
)

func (p *Poller) pollGateway(ctx context.Context, gw, pubkey string) {
	start := p.cfg.Now()
	name, sample, outcome, why := p.read(ctx, gw, pubkey)

	p.mu.Lock()
	st := p.nodes[gw]
	if st == nil || st.pubkey != pubkey {
		// New, or the gateway's pinned node changed: the old readings were
		// someone else's.
		st = &nodeState{pubkey: pubkey}
		p.nodes[gw] = st
	}
	switch outcome {
	case outcomeOK:
		st.lastAttempt, st.lastSuccess, st.sample = start, start, sample
		if name != "" {
			st.name = name
		}
	case outcomeFailed:
		st.lastAttempt = start
	}
	changed := st.outcome != outcome
	st.outcome = outcome
	p.mu.Unlock()

	kv := []any{"gateway", gw, "outcome", outcome, "reason", why, "duration", p.cfg.Now().Sub(start)}
	if changed {
		logging.Infow(ctx, "Relay telemetry poll", kv...)
	} else {
		logging.Debugw(ctx, "Relay telemetry poll", kv...)
	}
}

// forget drops a gateway whose node is no longer pinned (deleted, or reset).
func (p *Poller) forget(ctx context.Context, gw string) {
	p.mu.Lock()
	_, had := p.nodes[gw]
	delete(p.nodes, gw)
	p.mu.Unlock()
	if had {
		logging.Infow(ctx, "Relay telemetry: gateway has no pinned node; dropped from the report", "gateway", gw)
	} else {
		logging.Debugw(ctx, "Relay telemetry: gateway has no pinned node; not reported", "gateway", gw)
	}
}

// read takes a background lease and reads the node. why is a short reason
// for the log, never node data.
func (p *Poller) read(ctx context.Context, gw, pubkey string) (name string, s *Sample, outcome, why string) {
	h := newHolder()
	lease, err := p.relay.Acquire(meshrelay.LeaseRequest{Gateway: gw, Client: ClientName,
		Role: meshrelay.RoleAgent, Priority: meshrelay.PriorityBackground, Holder: h})
	var unavailable *meshrelay.UnavailableError
	switch {
	case errors.Is(err, meshrelay.ErrBusy):
		return "", nil, outcomeSkipped, "busy"
	case errors.As(err, &unavailable):
		if skipCode(unavailable.Code) {
			return "", nil, outcomeSkipped, unavailable.Code
		}
		return "", nil, outcomeFailed, unavailable.Code
	case err != nil:
		return "", nil, outcomeFailed, err.Error()
	}
	defer p.relay.Release(lease)

	x := &exchange{ctx: ctx, relay: p.relay, lease: lease, h: h, timeout: p.cfg.Timeout}
	name, s, err = x.readNode(pubkey)
	if err != nil {
		if reason, ended := h.endReason(); ended && skipCode(reason) {
			return "", nil, outcomeSkipped, reason
		}
		return "", nil, outcomeFailed, err.Error()
	}
	return name, s, outcomeOK, ""
}

// skipCode is true for a refusal or lease end that says nothing about the
// node: someone else is using it, the host paused sharing, or the relay is
// stopping.
func skipCode(code string) bool {
	switch code {
	case meshrelay.ReasonPreempted, meshrelay.ReasonHostPaused, "node_" + meshrelay.NodePaused,
		meshrelay.ReasonShutdown, meshrelay.ReasonRevoked:
		return true
	}
	return false
}

// exchange is one leased conversation with a node.
type exchange struct {
	ctx     context.Context
	relay   Relay
	lease   *meshrelay.Lease
	h       *holder
	timeout time.Duration
}

// ask sends a command and waits for the reply that matches, ignoring every
// other frame (the node's own pushes). An ERR reply is errNodeRefused.
func (x *exchange) ask(cmd []byte, want func([]byte) bool) ([]byte, error) {
	if err := x.relay.Send(x.lease, cmd); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(x.timeout)
	defer deadline.Stop()
	for {
		select {
		case f := <-x.h.frames:
			switch {
			case want(f):
				return f, nil
			case len(f) > 0 && f[0] == respErr:
				return nil, errNodeRefused
			}
		case reason := <-x.h.ended:
			return nil, fmt.Errorf("lease ended: %s", reason)
		case <-deadline.C:
			return nil, fmt.Errorf("no reply to command 0x%02x within %s", cmd[0], x.timeout)
		case <-x.ctx.Done():
			return nil, x.ctx.Err()
		}
	}
}

// readNode runs APP_START, checks the node is the pinned one, and reads what
// it will answer. Only APP_START and the key check are required; a stats
// command the firmware does not support leaves its fields null.
func (x *exchange) readNode(pubkey string) (string, *Sample, error) {
	f, err := x.ask(appStartFrame(), isCode(respSelfInfo))
	if err != nil {
		return "", nil, fmt.Errorf("APP_START: %w", err)
	}
	got, name, err := parseSelfInfo(f)
	if err != nil {
		return "", nil, err
	}
	if got != pubkey {
		// The relay only leases a node reporting its pinned key, so this is
		// a node swapped mid-lease. Report nothing under the pinned key.
		return "", nil, errors.New("node key does not match the pinned key")
	}

	s := &Sample{}
	fw := 0
	if f, err := x.ask(deviceQueryFrame(), isCode(respDeviceInfo)); err == nil {
		fw, _ = parseFirmwareVersion(f)
	} else if x.fatal(err) {
		return "", nil, err
	}
	if f, err := x.ask(battStorageFrame(), isCode(respBattStorage)); err == nil {
		if v, err := parseBattery(f); err == nil {
			s.BatteryVoltage = &v
		}
	} else if x.fatal(err) {
		return "", nil, err
	}
	if fw >= statsMinFirmware {
		for _, st := range []struct {
			kind  byte
			apply func([]byte, *Sample) error
		}{{statsCore, applyCoreStats}, {statsRadio, applyRadioStats}, {statsPackets, applyPacketStats}} {
			f, err := x.ask(statsFrame(st.kind), isStats(st.kind))
			if err != nil {
				if x.fatal(err) {
					return "", nil, err
				}
				continue
			}
			_ = st.apply(f, s) // a malformed reply leaves its fields null
		}
	}
	return name, s, nil
}

// fatal is true when the lease or context is gone, so the rest of the poll
// cannot run. A refused or unanswered command is not fatal.
func (x *exchange) fatal(err error) bool {
	if x.ctx.Err() != nil {
		return true
	}
	_, ended := x.h.endReason()
	return ended || errors.Is(err, meshrelay.ErrLeaseEnded)
}

// holder is the poller's LeaseHolder. The relay calls it with its mutex held,
// so nothing here blocks: frames beyond the buffer are dropped (they are the
// node's own pushes; replies are read as they arrive).
type holder struct {
	frames chan []byte
	ended  chan string

	mu     sync.Mutex
	reason string
	done   bool
}

func newHolder() *holder {
	return &holder{frames: make(chan []byte, 64), ended: make(chan string, 1)}
}

func (h *holder) Deliver(f []byte) {
	select {
	case h.frames <- f:
	default:
	}
}

func (h *holder) Notify(any) {}

func (h *holder) End(reason string) {
	h.mu.Lock()
	h.reason, h.done = reason, true
	h.mu.Unlock()
	select {
	case h.ended <- reason:
	default:
	}
}

func (h *holder) endReason() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason, h.done
}

// push sends the complete report to the Grid's mesh.repeater stream.
func (p *Poller) push(ctx context.Context) {
	body, err := json.Marshal(p.Report())
	if err != nil {
		logging.Errorw(ctx, "Relay telemetry: encoding the report failed", "error", err)
		return
	}
	url := p.cfg.GridURL + "/api/v1/ingest/mesh.repeater"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logging.Errorw(ctx, "Relay telemetry: building the ingest request failed", "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.IngestToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: push to the Grid failed", "error", err)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusAccepted {
		logging.Warnw(ctx, "Relay telemetry: the Grid refused the report", "status", resp.StatusCode,
			"retryAfter", resp.Header.Get("Retry-After"), "body", string(out))
		return
	}
	var ack struct {
		Accepted int      `json:"accepted"`
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(out, &ack)
	if len(ack.Warnings) > 0 {
		logging.Warnw(ctx, "Relay telemetry: report accepted with warnings", "accepted", ack.Accepted,
			"warnings", ack.Warnings)
		return
	}
	logging.Debugw(ctx, "Relay telemetry: report accepted", "accepted", ack.Accepted)
}
