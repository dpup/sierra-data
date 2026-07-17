package meshcore

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/dpup/prefab/logging"
)

// packetTypeAdvert is the MeshCore payload type for node announcements — the
// only packet we ingest (unencrypted; carries identity/role/location/name).
const packetTypeAdvert = 4

// defaultTopic is the map-ecosystem convention: meshcore/{IATA}/{PUBLIC_KEY}/packets.
const defaultTopic = "meshcore/+/+/packets"

// Broker is one MQTT endpoint. Several are configured for resilience; a node
// heard on more than one collapses to a single registry entry (newest advert
// wins, gateways unioned).
type Broker struct {
	URL      string   // tcp:// | ssl:// | ws:// | wss://
	ClientID string   // MQTT client id; defaulted per-broker if empty
	Username string   // optional
	Password string   // optional
	Topics   []string // defaults to [defaultTopic]
	QoS      byte
}

// Config configures a Registry.
type Config struct {
	Brokers []Broker
	// RequireValidSignature drops adverts whose Ed25519 signature doesn't verify.
	// Framing was confirmed against a live capture (2026-07-17): the bridge `raw`
	// is the full frame, stripped by DecodeFrame, after which real adverts verify.
	// Safe (and recommended) to enable to reject spoofed/corrupt adverts.
	RequireValidSignature bool
	// RetainFor bounds registry memory: nodes not heard within this window are
	// pruned. Set to the source's expireAfter so the store's lifecycle, not the
	// buffer, decides when a silent node is gone.
	RetainFor time.Duration
}

// NodeState is a snapshot of one mesh node's presence.
type NodeState struct {
	PubKey      string
	Role        string
	Name        string
	HasLocation bool
	Lat, Lng    float64

	// Brokers are the MQTT server URL(s) this node was heard on — recorded in
	// event provenance (which server delivered the advert). Sorted; unioned
	// across brokers when a node is heard on more than one.
	Brokers []string

	// Volatile telemetry (never mints a store revision).
	SNR          float64
	RSSI         int32
	HopCount     uint32
	Path         []string
	Gateways     []string
	LastAdvertAt time.Time // sender-stamped
	LastHeardAt  time.Time // our receive time
}

type nodeEntry struct {
	NodeState
	gateways map[string]struct{}
	brokers  map[string]struct{}
}

// Registry subscribes to MeshCore MQTT bridges and accumulates node presence in
// memory. It is safe for concurrent use: MQTT callbacks write, the ingest
// normalizer reads via Snapshot on the scheduler's tick.
type Registry struct {
	cfg     Config
	baseCtx context.Context

	mu        sync.Mutex
	nodes     map[string]*nodeEntry
	clients   []mqtt.Client
	lastMsgAt time.Time

	now func() time.Time // injectable clock for tests
}

// NewRegistry builds a Registry. Call Connect to start the MQTT subscriptions.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		cfg:     cfg,
		baseCtx: context.Background(),
		nodes:   make(map[string]*nodeEntry),
		now:     time.Now,
	}
}

// Connect dials every configured broker. It does NOT block on connectivity:
// paho retries in the background, and Health() reflects the live connection
// count. Returns an error only when no brokers are configured.
func (r *Registry) Connect(ctx context.Context) error {
	if len(r.cfg.Brokers) == 0 {
		return fmt.Errorf("meshcore: no brokers configured")
	}
	r.baseCtx = ctx
	for i, b := range r.cfg.Brokers {
		c := r.buildClient(ctx, i, b)
		c.Connect() // fire-and-forget; SetConnectRetry keeps trying
		r.mu.Lock()
		r.clients = append(r.clients, c)
		r.mu.Unlock()
	}
	return nil
}

// buildClient constructs a paho client that (re)subscribes on every connect.
func (r *Registry) buildClient(ctx context.Context, idx int, b Broker) mqtt.Client {
	topics := b.Topics
	if len(topics) == 0 {
		topics = []string{defaultTopic}
	}
	clientID := b.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("sierra-grid-meshcore-%d", idx)
	}

	opts := mqtt.NewClientOptions().
		AddBroker(b.URL).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(30 * time.Second).
		SetConnectTimeout(15 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetCleanSession(true)
	if b.Username != "" {
		opts.SetUsername(b.Username)
	}
	if b.Password != "" {
		opts.SetPassword(b.Password)
	}
	if isTLSURL(b.URL) {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}

	handler := r.onMessage(b.URL)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		logging.Infow(ctx, "MeshCore broker connected", "broker", b.URL)
		for _, t := range topics {
			if tok := c.Subscribe(t, b.QoS, handler); tok.Wait() && tok.Error() != nil {
				logging.Warnw(ctx, "MeshCore subscribe failed", "broker", b.URL, "topic", t, "error", tok.Error())
			}
		}
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		logging.Warnw(ctx, "MeshCore broker connection lost", "broker", b.URL, "error", err)
	})
	return mqtt.NewClient(opts)
}

// onMessage returns a handler bound to a broker id (used as a gateway fallback).
func (r *Registry) onMessage(brokerID string) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		r.ingestRaw(m.Payload(), brokerID)
	}
}

// ingestRaw parses the bridge's JSON envelope and, if it's an advert, updates
// the registry. Malformed/unrelated messages are dropped quietly (the feed is a
// firehose of every packet type; adverts are a small slice).
func (r *Registry) ingestRaw(payload []byte, brokerID string) {
	var env packetEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		logging.Debugw(r.baseCtx, "MeshCore: bad envelope JSON", "error", err)
		return
	}
	r.ingestPacket(&env, brokerID)
}

// ingestPacket applies one decoded envelope to the registry. Split out from the
// MQTT plumbing so it is unit-testable without a broker.
func (r *Registry) ingestPacket(env *packetEnvelope, brokerID string) {
	if env.PacketType.int() != packetTypeAdvert {
		return
	}
	raw, err := hex.DecodeString(strings.TrimSpace(env.Raw))
	if err != nil {
		logging.Debugw(r.baseCtx, "MeshCore: undecodable raw hex", "error", err)
		return
	}
	// The bridge `raw` is the full over-the-air frame (header + path + payload),
	// so strip the transport framing before decoding the advert payload.
	adv, err := DecodeFrame(raw)
	if err != nil {
		logging.Debugw(r.baseCtx, "MeshCore: advert decode failed", "error", err)
		return
	}
	if r.cfg.RequireValidSignature && !adv.SignatureValid {
		logging.Warnw(r.baseCtx, "MeshCore: dropping advert with invalid signature", "pubkey", adv.PubKey)
		return
	}

	now := r.now()
	gw := firstNonEmpty(env.OriginID, env.Origin, brokerID)
	path := parsePath(env.Path)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastMsgAt = now

	e := r.nodes[adv.PubKey]
	if e == nil {
		e = &nodeEntry{gateways: make(map[string]struct{}), brokers: make(map[string]struct{})}
		r.nodes[adv.PubKey] = e
	}
	e.PubKey = adv.PubKey
	e.Role = adv.Role
	e.Name = adv.Name
	// Keep last-known location: a later location-less advert (e.g. a zero-hop
	// beacon) must not erase a node's position.
	if adv.HasLocation {
		e.HasLocation = true
		e.Lat, e.Lng = adv.Lat, adv.Lng
	}
	e.SNR = env.SNR.float()
	e.RSSI = int32(env.RSSI.int())
	e.Path = path
	e.HopCount = uint32(len(path))
	e.LastAdvertAt = adv.Timestamp
	e.LastHeardAt = now
	if gw != "" {
		e.gateways[gw] = struct{}{}
	}
	// brokerID is the MQTT server URL this advert arrived on (→ provenance).
	if brokerID != "" {
		e.brokers[brokerID] = struct{}{}
	}

	r.pruneLocked(now)
}

// Snapshot returns nodes heard within activeWindow (0 = no window). Gateways are
// materialized as a sorted slice. This is what the normalizer's Poll returns.
func (r *Registry) Snapshot(activeWindow time.Duration) []NodeState {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)

	out := make([]NodeState, 0, len(r.nodes))
	for _, e := range r.nodes {
		if activeWindow > 0 && now.Sub(e.LastHeardAt) > activeWindow {
			continue
		}
		ns := e.NodeState
		ns.Gateways = sortedKeys(e.gateways)
		ns.Brokers = sortedKeys(e.brokers)
		out = append(out, ns)
	}
	return out
}

// Health reports how many brokers are currently connected and when the last
// message arrived. Zero connected → the normalizer hard-errors its Poll so the
// disappearance sweep is skipped (our outage must not expire live nodes).
func (r *Registry) Health() (connected int, lastMsg time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		if c != nil && c.IsConnected() {
			connected++
		}
	}
	return connected, r.lastMsgAt
}

// Close disconnects every broker.
func (r *Registry) Close() {
	r.mu.Lock()
	clients := r.clients
	r.mu.Unlock()
	for _, c := range clients {
		if c != nil && c.IsConnected() {
			c.Disconnect(250)
		}
	}
}

// pruneLocked drops nodes not heard within RetainFor. Caller holds r.mu.
func (r *Registry) pruneLocked(now time.Time) {
	if r.cfg.RetainFor <= 0 {
		return
	}
	for k, e := range r.nodes {
		if now.Sub(e.LastHeardAt) > r.cfg.RetainFor {
			delete(r.nodes, k)
		}
	}
}

// --- JSON envelope + flexible number parsing -------------------------------

// packetEnvelope is the bridge's per-packet JSON. Numeric fields arrive as
// either JSON numbers or quoted strings depending on the bridge, so they use
// flex types.
type packetEnvelope struct {
	Origin     string    `json:"origin"`
	OriginID   string    `json:"origin_id"`
	Timestamp  string    `json:"timestamp"`
	PacketType flexInt   `json:"packet_type"`
	Route      string    `json:"route"`
	Raw        string    `json:"raw"`
	SNR        flexFloat `json:"SNR"`
	RSSI       flexInt   `json:"RSSI"`
	Hash       string    `json:"hash"`
	Path       string    `json:"path"`
}

type flexInt struct{ v int64 }
type flexFloat struct{ v float64 }

func (f *flexInt) int() int64       { return f.v }
func (f *flexFloat) float() float64 { return f.v }

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		f.v = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// tolerate a float-looking int ("4.0")
		fl, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		n = int64(fl)
	}
	f.v = n
	return nil
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		f.v = 0
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	f.v = fl
	return nil
}

// --- small helpers ---------------------------------------------------------

// parsePath splits a bridge path like "C2 -> E2" into ["C2","E2"].
func parsePath(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "->")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isTLSURL(u string) bool {
	u = strings.ToLower(u)
	return strings.HasPrefix(u, "ssl://") || strings.HasPrefix(u, "tls://") ||
		strings.HasPrefix(u, "wss://") || strings.HasPrefix(u, "https://")
}
