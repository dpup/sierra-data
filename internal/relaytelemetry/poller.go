// Package relaytelemetry is the relay's in-process telemetry poller: it reads
// the companion node behind every paired gateway, the way an operator monitor
// (alanpi) reads repeaters, and reports to the Grid as a mesh.repeater
// reporter. Pairing with SIERRA is the host's consent; there is no per-gateway
// opt-in (meshRelay.telemetry.excludeGateways is the exception list).
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
//   - Each report is the complete set of gateway nodes (radios), once per
//     pinned key: several gateways can pin the same radio (a host moving it to
//     a new computer), and each radio is read through the first of them that
//     answers. A gateway with no pinned key cannot be named and is left out,
//     as is one excluded or not seen for GatewayStaleAfter.
//   - A gateway node's own signed advert (CMD_EXPORT_CONTACT with no key; it
//     transmits nothing) is forwarded on mesh.packet so the Grid can establish
//     the node, which a report alone never does. It is forwarded only when its
//     content changes or the last one is SelfAdvertResend old: each export is
//     freshly signed, and one per poll would be noise.
package relaytelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
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
	// ExcludeGateways' radios are never read or published as gateway nodes,
	// through them or through any other gateway pinning the same radio.
	// Every other paired gateway is: pairing with SIERRA is the consent.
	// Exclusion is about publishing the gateway's own radio; the
	// shared-repeater checks may still use an excluded gateway. Published telemetry can
	// let someone match a node to its gateway, which the relay's public status
	// alone does not reveal.
	ExcludeGateways []string
	// GatewayStaleAfter skips a gateway whose bridge hasn't been seen (or,
	// never connected, that was created) for this long (7 days), so a
	// long-gone gateway isn't reported as failing forever. A radio with only
	// such gateways is dropped from the report.
	GatewayStaleAfter time.Duration
	// SelfAdvertResend re-forwards an unchanged gateway self-advert this often
	// (12h), inside the Grid's 14h advert presence window.
	SelfAdvertResend time.Duration
	Interval         time.Duration // between polls (5m)
	// Timeout bounds one reply from the node (5s). A whole poll is a handful
	// of round trips.
	Timeout     time.Duration
	GridURL     string // e.g. https://data.sierragridteam.org
	IngestToken string // the Grid mesh.repeater reporter token
	HTTPClient  *http.Client
	Now         func() time.Time
	// RepeaterInterval is how often shared repeaters are read (30m). They're
	// read over the mesh, whose airtime everyone shares; see schedule.go.
	RepeaterInterval time.Duration
	// RepeaterRetries are the waits before each retry on one gateway when a
	// repeater doesn't answer (DefaultRepeaterRetries: 30s, 60s, 120s).
	RepeaterRetries []time.Duration
	// HibernateAfter is how many cycles a repeater can go unread, with
	// nothing else reporting it alive, before it's polled daily (6).
	HibernateAfter int
	// HibernateInterval is the hibernating repeater's cycle (24h).
	HibernateInterval time.Duration
	// RepeaterTick is how often repeater work is checked (5s).
	RepeaterTick time.Duration
	// ReporterID is this poller's id on the Grid, so its own reports aren't
	// taken as another monitor's ("relay").
	ReporterID string
}

// Poller polls gateway nodes and pushes their telemetry to the Grid.
type Poller struct {
	cfg   Config
	relay Relay
	gws   Gateways

	reps Repeaters // nil: no shared repeaters

	mu        sync.Mutex
	nodes     map[string]*nodeState // gateway nodes, by pinned pubkey
	repeaters map[string]*nodeState // shared repeaters, by pubkey

	// Shared-repeater schedule, owned by the RepeaterTick goroutine.
	jobs  map[string]*repeaterJob
	alive func(ctx context.Context, pubkey string, window time.Duration) (bool, error)
}

type nodeState struct {
	pubkey      string
	name        string
	lastAttempt time.Time
	lastSuccess time.Time
	sample      *Sample // last successful reading; nil if never reached
	outcome     string  // last poll outcome, to log changes only
	// Shared repeaters: the last signed advert a gateway's node stored for
	// it, and the one the Grid last accepted (forwarded only on change).
	// Gateway nodes: the node's own advert from this poll's read (nil after a
	// failed read: an old one would claim fresh presence), the one last
	// accepted, and when.
	advert, advertSent []byte
	advertSentAt       time.Time
	// via is the gateway last tried for a gateway node, for the log; okVia
	// the one that last read it.
	via, okVia string
}

// New returns a Poller. reps, when not nil, adds the relay's shared
// repeaters.
func New(cfg Config, relay Relay, gws Gateways, reps Repeaters) *Poller {
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
	if cfg.RepeaterInterval <= 0 {
		cfg.RepeaterInterval = 30 * time.Minute
	}
	if cfg.RepeaterRetries == nil {
		cfg.RepeaterRetries = DefaultRepeaterRetries
	}
	if cfg.HibernateAfter <= 0 {
		cfg.HibernateAfter = 6
	}
	if cfg.HibernateInterval <= 0 {
		cfg.HibernateInterval = 24 * time.Hour
	}
	if cfg.RepeaterTick <= 0 {
		cfg.RepeaterTick = 5 * time.Second
	}
	if cfg.ReporterID == "" {
		cfg.ReporterID = "relay"
	}
	if cfg.GatewayStaleAfter <= 0 {
		cfg.GatewayStaleAfter = 7 * 24 * time.Hour
	}
	if cfg.SelfAdvertResend <= 0 {
		cfg.SelfAdvertResend = 12 * time.Hour
	}
	cfg.GridURL = strings.TrimRight(cfg.GridURL, "/")
	p := &Poller{cfg: cfg, relay: relay, gws: gws, reps: reps, nodes: map[string]*nodeState{},
		repeaters: map[string]*nodeState{}, jobs: map[string]*repeaterJob{}}
	p.alive = p.gridAlive
	return p
}

// Run polls every Interval until ctx ends. The first cycle waits one minute,
// so bridges that reconnected after a relay restart are back first.
func (p *Poller) Run(ctx context.Context, firstDelay time.Duration) {
	logging.Infow(ctx, "Relay telemetry poller started", "excludeGateways", p.cfg.ExcludeGateways,
		"interval", p.cfg.Interval, "repeaters", p.reps != nil, "repeaterInterval", p.cfg.RepeaterInterval,
		"grid", p.cfg.GridURL)
	if p.reps != nil {
		go p.runRepeaters(ctx, firstDelay)
	}
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

// runRepeaters runs the shared-repeater schedule (schedule.go) until ctx
// ends. Its readings go out with each Cycle's report.
func (p *Poller) runRepeaters(ctx context.Context, firstDelay time.Duration) {
	t := time.NewTimer(firstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.RepeaterTick(ctx)
		t.Reset(p.cfg.RepeaterTick)
	}
}

// Cycle polls every paired gateway's radio once and pushes the report, with
// the shared repeaters' latest readings, then any adverts the Grid needs.
func (p *Poller) Cycle(ctx context.Context) {
	recs, err := p.gws.Gateways()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing gateways failed; skipping this cycle", "error", err)
		return
	}
	radios := p.radios(ctx, recs)
	p.mu.Lock()
	var gone []string
	for pk := range p.nodes {
		if _, ok := radios[pk]; !ok {
			gone = append(gone, pk)
		}
	}
	p.mu.Unlock()
	for _, pk := range gone {
		p.forget(ctx, pk)
	}
	keys := make([]string, 0, len(radios))
	for pk := range radios {
		keys = append(keys, pk)
	}
	sort.Strings(keys)
	for _, pk := range keys {
		if ctx.Err() != nil {
			return
		}
		p.pollRadio(ctx, pk, radios[pk])
	}
	if ctx.Err() != nil {
		return
	}
	p.push(ctx)
	p.pushAdverts(ctx)
}

// radios groups the gateways to poll by pinned key, each group ordered most
// recently seen first (then by name), so the gateway a radio is on now is
// tried before one it left.
//
// Exclusion is by radio: excluding a gateway also stops reading its pinned
// radio through any other gateway that pins the same one. The host asked
// about their radio, not about a gateway name.
func (p *Poller) radios(ctx context.Context, recs []*meshrelay.GatewayRecord) map[string][]string {
	excluded := map[string]bool{}
	for _, gw := range p.cfg.ExcludeGateways {
		excluded[gw] = true
	}
	excludedRadio := map[string]bool{}
	for _, r := range recs {
		if excluded[r.Name] && r.PinnedPubkey != "" {
			excludedRadio[r.PinnedPubkey] = true
		}
	}
	now := p.cfg.Now()
	type cand struct {
		name string
		seen time.Time
	}
	groups := map[string][]cand{}
	for _, r := range recs {
		if r.PinnedPubkey == "" || excluded[r.Name] || excludedRadio[r.PinnedPubkey] {
			continue
		}
		seen := r.LastSeen
		if seen.IsZero() {
			seen = r.CreatedAt // paired, bridge not connected yet
		}
		if now.Sub(seen) > p.cfg.GatewayStaleAfter {
			logging.Debugw(ctx, "Relay telemetry: gateway not seen recently; not polled", "gateway", r.Name, "lastSeen", seen)
			continue
		}
		groups[r.PinnedPubkey] = append(groups[r.PinnedPubkey], cand{r.Name, seen})
	}
	out := make(map[string][]string, len(groups))
	for pk, cs := range groups {
		sort.Slice(cs, func(i, j int) bool {
			if !cs[i].seen.Equal(cs[j].seen) {
				return cs[i].seen.After(cs[j].seen)
			}
			return cs[i].name < cs[j].name
		})
		for _, c := range cs {
			out[pk] = append(out[pk], c.name)
		}
	}
	return out
}

// outcome of one poll.
const (
	outcomeOK      = "ok"
	outcomeSkipped = "skipped" // busy, paused, preempted, relay stopping: no attempt
	outcomeFailed  = "failed"  // tried and could not read the node
)

// pollRadio reads one radio through its gateways in order, stopping at the
// first that reads it. If none does:
//   - a gateway where someone holds the node (busy, preempted) or the relay
//     is stopping means no attempt is recorded: the radio is there, in use;
//   - a paused gateway does the same only if it is the gateway that last
//     read this radio, or no other gateway failed. A pause says the host
//     wants the node left alone, not that the radio is on that gateway, so it
//     mustn't hide a radio that is really offline elsewhere;
//   - otherwise it is a failed attempt.
func (p *Poller) pollRadio(ctx context.Context, pubkey string, gws []string) {
	start := p.cfg.Now()
	p.mu.Lock()
	var lastOK string
	if st := p.nodes[pubkey]; st != nil {
		lastOK = st.okVia
	}
	p.mu.Unlock()
	var (
		name, via, why string
		sample         *Sample
		advert         []byte
		outcome        = outcomeFailed
		inUse, failed  bool
		pausedHere     []string
	)
	for _, gw := range gws {
		if ctx.Err() != nil {
			return
		}
		n, s, adv, o, w := p.read(ctx, gw, pubkey)
		via, why = gw, w
		switch {
		case o == outcomeOK:
			name, sample, advert, outcome = n, s, adv, o
		case o == outcomeSkipped && pausedCode(w):
			pausedHere = append(pausedHere, gw)
		case o == outcomeSkipped:
			inUse = true
		default:
			failed = true
		}
		if outcome == outcomeOK {
			break
		}
	}
	if outcome != outcomeOK && (inUse || len(pausedHere) > 0 && (!failed || slices.Contains(pausedHere, lastOK))) {
		outcome = outcomeSkipped
	}

	p.mu.Lock()
	st := p.nodes[pubkey]
	if st == nil {
		st = &nodeState{pubkey: pubkey}
		p.nodes[pubkey] = st
	}
	switch outcome {
	case outcomeOK:
		st.lastAttempt, st.lastSuccess, st.sample = start, start, sample
		st.okVia = via
		st.advert = advert
		if name != "" {
			st.name = name
		}
	case outcomeFailed:
		st.lastAttempt = start
		st.advert = nil
	default:
		st.advert = nil
	}
	changed := st.outcome != outcome || st.via != via
	st.outcome, st.via = outcome, via
	p.mu.Unlock()

	kv := []any{"node", shortKey(pubkey), "gateway", via, "gateways", len(gws), "outcome", outcome, "reason", why,
		"duration", p.cfg.Now().Sub(start)}
	if changed {
		logging.Infow(ctx, "Relay telemetry poll", kv...)
	} else {
		logging.Debugw(ctx, "Relay telemetry poll", kv...)
	}
}

// forget drops a radio no gateway can be polled for any more (removed,
// re-pinned, excluded or stale).
func (p *Poller) forget(ctx context.Context, pubkey string) {
	p.mu.Lock()
	delete(p.nodes, pubkey)
	p.mu.Unlock()
	logging.Infow(ctx, "Relay telemetry: gateway node no longer polled; dropped from the report", "node", shortKey(pubkey))
}

func shortKey(pk string) string {
	if len(pk) > 8 {
		return pk[:8]
	}
	return pk
}

// read takes a background lease and reads the node. why is a short reason
// for the log, never node data.
func (p *Poller) read(ctx context.Context, gw, pubkey string) (name string, s *Sample, advert []byte, outcome, why string) {
	h := newHolder()
	lease, err := p.relay.Acquire(meshrelay.LeaseRequest{Gateway: gw, Client: ClientName,
		Role: meshrelay.RoleAgent, Priority: meshrelay.PriorityBackground, Holder: h})
	var unavailable *meshrelay.UnavailableError
	switch {
	case errors.Is(err, meshrelay.ErrBusy):
		return "", nil, nil, outcomeSkipped, "busy"
	case errors.As(err, &unavailable):
		if skipCode(unavailable.Code) {
			return "", nil, nil, outcomeSkipped, unavailable.Code
		}
		return "", nil, nil, outcomeFailed, unavailable.Code
	case err != nil:
		return "", nil, nil, outcomeFailed, err.Error()
	}
	defer p.relay.Release(lease)

	x := &exchange{ctx: ctx, relay: p.relay, lease: lease, h: h, timeout: p.cfg.Timeout}
	name, s, advert, err = x.readNode(pubkey)
	if err != nil {
		if reason, ended := h.endReason(); ended && skipCode(reason) {
			return "", nil, nil, outcomeSkipped, reason
		}
		return "", nil, nil, outcomeFailed, err.Error()
	}
	return name, s, advert, outcomeOK, ""
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

// pausedCode is a skip because the host paused sharing (rather than someone
// holding the node, or the relay stopping).
func pausedCode(code string) bool {
	return code == meshrelay.ReasonHostPaused || code == "node_"+meshrelay.NodePaused
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
// it will answer, including its own signed advert. Only APP_START and the key
// check are required; a stats command the firmware does not support leaves
// its fields null, and a refused advert export leaves advert nil.
func (x *exchange) readNode(pubkey string) (string, *Sample, []byte, error) {
	f, err := x.ask(appStartFrame(), isCode(respSelfInfo))
	if err != nil {
		return "", nil, nil, fmt.Errorf("APP_START: %w", err)
	}
	got, name, err := parseSelfInfo(f)
	if err != nil {
		return "", nil, nil, err
	}
	if got != pubkey {
		// The relay only leases a node reporting its pinned key, so this is
		// a node swapped mid-lease. Report nothing under the pinned key.
		return "", nil, nil, errors.New("node key does not match the pinned key")
	}

	s := &Sample{}
	fw := 0
	if f, err := x.ask(deviceQueryFrame(), isCode(respDeviceInfo)); err == nil {
		fw, _ = parseFirmwareVersion(f)
	} else if x.fatal(err) {
		return "", nil, nil, err
	}
	if f, err := x.ask(battStorageFrame(), isCode(respBattStorage)); err == nil {
		if v, err := parseBattery(f); err == nil {
			s.BatteryVoltage = &v
		}
	} else if x.fatal(err) {
		return "", nil, nil, err
	}
	if fw >= statsMinFirmware {
		for _, st := range []struct {
			kind  byte
			apply func([]byte, *Sample) error
		}{{statsCore, applyCoreStats}, {statsRadio, applyRadioStats}, {statsPackets, applyPacketStats}} {
			f, err := x.ask(statsFrame(st.kind), isStats(st.kind))
			if err != nil {
				if x.fatal(err) {
					return "", nil, nil, err
				}
				continue
			}
			_ = st.apply(f, s) // a malformed reply leaves its fields null
		}
	}
	var advert []byte
	if f, err := x.ask(exportSelfFrame(), isCode(respExportContact)); err == nil {
		if raw := f[1:]; selfAdvertOf(raw, pubkey) != nil {
			advert = bytes.Clone(raw)
		}
	} else if x.fatal(err) {
		return "", nil, nil, err
	}
	return name, s, advert, nil
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
func (p *Poller) push(ctx context.Context) { p.post(ctx, "mesh.repeater", p.Report()) }

// post sends one document to a Grid ingest stream, reporting whether the
// Grid accepted it (202; warnings are logged, not retried).
func (p *Poller) post(ctx context.Context, stream string, v any) bool {
	body, err := json.Marshal(v)
	if err != nil {
		logging.Errorw(ctx, "Relay telemetry: encoding the report failed", "stream", stream, "error", err)
		return false
	}
	url := p.cfg.GridURL + "/api/v1/ingest/" + stream
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logging.Errorw(ctx, "Relay telemetry: building the ingest request failed", "stream", stream, "error", err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.IngestToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: push to the Grid failed", "stream", stream, "error", err)
		return false
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusAccepted {
		logging.Warnw(ctx, "Relay telemetry: the Grid refused the report", "stream", stream, "status", resp.StatusCode,
			"retryAfter", resp.Header.Get("Retry-After"), "body", string(out))
		return false
	}
	var ack struct {
		Accepted int      `json:"accepted"`
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(out, &ack)
	if len(ack.Warnings) > 0 {
		logging.Warnw(ctx, "Relay telemetry: report accepted with warnings", "stream", stream,
			"accepted", ack.Accepted, "warnings", ack.Warnings)
		return true
	}
	logging.Debugw(ctx, "Relay telemetry: report accepted", "stream", stream, "accepted", ack.Accepted)
	return true
}
