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
	// SpamFloor is the minimum gap between BUFFERED receptions from the same node
	// on the same gateway — a guard so a pathological fast-adverting node can't
	// flood the observation store. Multi-gateway copies of one advert (different
	// gateway) are each kept: hearing a link on N gateways is real resilience
	// signal. 0 disables the floor (keep every reception).
	SpamFloor time.Duration
	// Cadence-aware presence (docs/mesh-topology-design.md §9): a node stays in
	// Snapshot for CadenceK × its own measured inter-advert interval, clamped to
	// [GraceFloor, GraceCeil]. A node with no cadence yet (one-shot / brand-new)
	// gets GraceFloor, so drive-through transients evaporate while a slow backbone
	// repeater is protected in proportion to its rhythm. Zero values default
	// (k=3, floor=3h, ceil=72h). RetainFor should be ≥ GraceCeil so a node lives
	// in memory for its whole presence window; it defaults to GraceCeil if unset.
	CadenceK   float64
	GraceFloor time.Duration
	GraceCeil  time.Duration
}

// Observation is one received advert, captured for the relay-topology store
// (Tier 0). Unlike NodeState (the latest-per-node presence), every reception is
// kept — the raw firehose the topology rollup is derived from. HeardAt is our
// receive time. PathNodes is resolved at DrainObservations time against the
// current node catalog (empty until then), parallel to Path.
type Observation struct {
	PubKey    string
	HeardAt   time.Time
	Broker    string
	Gateway   string
	SNR       float64
	RSSI      int32
	HopCount  uint32
	Path      []string
	PathNodes []string
}

// maxObsBuffer caps the reception buffer between drains. The scheduler drains
// every tick (minutes), so this only trips on a pathological burst; past it the
// oldest receptions are dropped to bound memory.
const maxObsBuffer = 100000

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

	// Volatile last-heard telemetry (never mints a store revision). The relay path
	// is NOT here — it is per-reception, captured in the observation firehose
	// (see Observation) and served as derived topology, not per-node presence.
	SNR          float64
	RSSI         int32
	HopCount     uint32
	Gateways     []string
	LastAdvertAt time.Time // sender-stamped
	LastHeardAt  time.Time // our receive time
}

type nodeEntry struct {
	NodeState
	gateways map[string]struct{}
	brokers  map[string]struct{}

	// Cadence estimate for presence: lastAdvertAt is the heard time of the last
	// DISTINCT advert (multi-gateway echoes of one advert are collapsed), and
	// ewmaInterval is the smoothed inter-advert interval. advertCount < 2 means
	// no interval measured yet (treated as unknown cadence).
	lastAdvertAt time.Time
	ewmaInterval time.Duration
	advertCount  int
}

// Cadence-aware presence tuning defaults + smoothing.
const (
	defaultCadenceK   = 3
	defaultGraceFloor = 3 * time.Hour
	defaultGraceCeil  = 72 * time.Hour
	// minCadenceSample ignores same-advert echoes (one advert arrives off several
	// gateways within seconds) when measuring the inter-advert interval.
	minCadenceSample = 30 * time.Second
	// cadenceAlpha weights the newest interval in the EWMA (responsive but stable).
	cadenceAlpha = 0.3
)

// recordCadence folds one advert reception time into the node's cadence estimate,
// collapsing multi-gateway echoes (< minCadenceSample apart) into a single advert.
// Shared by live ingest and Seed's rehydration replay. Times must arrive in
// ascending order.
func (e *nodeEntry) recordCadence(t time.Time) {
	if e.lastAdvertAt.IsZero() {
		e.lastAdvertAt = t
		e.advertCount = 1
		return
	}
	gap := t.Sub(e.lastAdvertAt)
	if gap < minCadenceSample {
		return // same advert echoed off another gateway — not a new interval
	}
	if e.ewmaInterval <= 0 {
		e.ewmaInterval = gap
	} else {
		e.ewmaInterval = time.Duration(cadenceAlpha*float64(gap) + (1-cadenceAlpha)*float64(e.ewmaInterval))
	}
	e.lastAdvertAt = t
	e.advertCount++
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

	// obsBuf accumulates receptions between scheduler drains; obsGate tracks the
	// last buffered time per (pubkey,gateway) for the SpamFloor. Both guarded by mu.
	obsBuf  []Observation
	obsGate map[string]time.Time

	now func() time.Time // injectable clock for tests
}

// NewRegistry builds a Registry. Call Connect to start the MQTT subscriptions.
// Zero cadence-presence knobs default; RetainFor defaults to GraceCeil so a node
// lives in memory for its whole presence window.
func NewRegistry(cfg Config) *Registry {
	if cfg.CadenceK <= 0 {
		cfg.CadenceK = defaultCadenceK
	}
	if cfg.GraceFloor <= 0 {
		cfg.GraceFloor = defaultGraceFloor
	}
	if cfg.GraceCeil <= 0 {
		cfg.GraceCeil = defaultGraceCeil
	}
	if cfg.RetainFor <= 0 {
		cfg.RetainFor = cfg.GraceCeil
	}
	return &Registry{
		cfg:     cfg,
		baseCtx: context.Background(),
		nodes:   make(map[string]*nodeEntry),
		obsGate: make(map[string]time.Time),
		now:     time.Now,
	}
}

// SeedNode is one node's persisted presence, used to rehydrate the Registry on
// startup. The grid store survives a restart but the in-memory Registry does not,
// so without this a deploy drops the whole mesh to "unknown" until every node
// re-adverts — and the disappearance sweep expires the slow ones first.
type SeedNode struct {
	PubKey      string
	Role        string
	Name        string
	HasLocation bool
	Lat, Lng    float64
	// LastHeard is the fallback last-heard time (the store's last_seen_at) used
	// when HeardTimes is empty. HeardTimes are recent advert receptions (ascending)
	// replayed to reconstruct cadence so the per-node presence window is accurate
	// immediately after boot, not just the GraceFloor.
	LastHeard  time.Time
	HeardTimes []time.Time
}

// Seed rehydrates node presence from persisted state. Call ONCE before Connect
// (single-threaded — no MQTT callbacks yet). Live adverts then update seeded
// nodes in place. Nodes seeded here appear in the next Snapshot within their
// (reconstructed or GraceFloor) window, so a restart no longer looks like a
// mesh-wide disappearance to the sweep.
func (r *Registry) Seed(nodes []SeedNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sn := range nodes {
		if sn.PubKey == "" {
			continue
		}
		e := r.nodes[sn.PubKey]
		if e == nil {
			e = &nodeEntry{gateways: make(map[string]struct{}), brokers: make(map[string]struct{})}
			r.nodes[sn.PubKey] = e
		}
		e.PubKey = sn.PubKey
		e.Role = sn.Role
		e.Name = sn.Name
		if sn.HasLocation {
			e.HasLocation = true
			e.Lat, e.Lng = sn.Lat, sn.Lng
		}
		last := sn.LastHeard
		for _, t := range sn.HeardTimes {
			e.recordCadence(t)
			if t.After(last) {
				last = t
			}
		}
		e.LastHeardAt = last
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
	// Hop count is the last-heard advert's path length (from the authoritative
	// over-the-air frame). The path itself is per-reception — captured in the
	// observation below, not on node presence.
	e.HopCount = uint32(adv.HopCount)
	e.LastAdvertAt = adv.Timestamp // node-reported; unreliable clock, diagnostic only
	e.LastHeardAt = now            // our receive time — the trustworthy clock
	e.recordCadence(now)           // per-node advert-interval estimate (drives Snapshot's window)
	if gw != "" {
		e.gateways[gw] = struct{}{}
	}
	// brokerID is the MQTT server URL this advert arrived on (→ provenance).
	if brokerID != "" {
		e.brokers[brokerID] = struct{}{}
	}

	// Buffer this reception for the topology store (Tier 0) unless it trips the
	// per-(node,gateway) spam floor. The rollup weight derives from these rows, so
	// a capped spammer is correctly down-weighted too; PathNodes is left empty here
	// and resolved at drain time against the full catalog.
	if r.allowObservationLocked(adv.PubKey, gw, now) {
		r.obsBuf = append(r.obsBuf, Observation{
			PubKey:   adv.PubKey,
			HeardAt:  now,
			Broker:   brokerID,
			Gateway:  gw,
			SNR:      env.SNR.float(),
			RSSI:     int32(env.RSSI.int()),
			HopCount: uint32(adv.HopCount),
			Path:     adv.Path,
		})
		if over := len(r.obsBuf) - maxObsBuffer; over > 0 {
			// Drop oldest — the drain interval is short, so this only trips on a
			// pathological burst; keep the most recent receptions.
			r.obsBuf = append(r.obsBuf[:0], r.obsBuf[over:]...)
		}
	}

	r.pruneLocked(now)
}

// allowObservationLocked reports whether a reception should be buffered, applying
// the per-(pubkey,gateway) SpamFloor. Caller holds r.mu.
func (r *Registry) allowObservationLocked(pubkey, gateway string, now time.Time) bool {
	if r.cfg.SpamFloor <= 0 {
		return true
	}
	key := pubkey + "\x00" + gateway
	if last, ok := r.obsGate[key]; ok && now.Sub(last) < r.cfg.SpamFloor {
		return false
	}
	r.obsGate[key] = now
	return true
}

// DrainObservations returns the receptions buffered since the last drain and
// clears the buffer. Called once per scheduler tick (single reader) alongside
// Snapshot; the scheduler flushes the result to the append-only observation
// store. Each observation's relay path is resolved to node pubkeys against the
// CURRENT catalog — same unique-prefix-match rule as Snapshot.
func (r *Registry) DrainObservations() []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.obsBuf) == 0 {
		return nil
	}
	idx := r.prefixIndexLocked()
	out := r.obsBuf
	r.obsBuf = nil
	for i := range out {
		out[i].PathNodes = resolvePath(out[i].Path, idx)
	}
	return out
}

// Snapshot returns the nodes currently PRESENT — each one heard within its own
// cadence-derived window (CadenceK × measured inter-advert interval, clamped to
// [GraceFloor, GraceCeil]; GraceFloor for a node with no cadence yet). A slow
// backbone repeater is protected in proportion to its rhythm while a dead chatty
// node or a one-shot transient drops out quickly. This is the "current set" the
// normalizer hands the disappearance sweep, so the sweep's own grace can stay a
// short uniform safety net. Gateways are materialized as a sorted slice.
func (r *Registry) Snapshot() []NodeState {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)

	out := make([]NodeState, 0, len(r.nodes))
	for _, e := range r.nodes {
		if now.Sub(e.LastHeardAt) > r.presenceWindow(e) {
			continue
		}
		ns := e.NodeState
		ns.Gateways = sortedKeys(e.gateways)
		ns.Brokers = sortedKeys(e.brokers)
		out = append(out, ns)
	}
	return out
}

// presenceWindow is how long a node stays present after its last advert:
// CadenceK × its measured cadence, clamped to [GraceFloor, GraceCeil]. A node
// with no interval measured yet (one advert, or echoes only) gets GraceFloor, so
// drive-through transients evaporate. Caller holds r.mu.
func (r *Registry) presenceWindow(e *nodeEntry) time.Duration {
	if e.advertCount < 2 || e.ewmaInterval <= 0 {
		return r.cfg.GraceFloor
	}
	w := time.Duration(r.cfg.CadenceK * float64(e.ewmaInterval))
	if w < r.cfg.GraceFloor {
		return r.cfg.GraceFloor
	}
	if w > r.cfg.GraceCeil {
		return r.cfg.GraceCeil
	}
	return w
}

// prefixLens are the hex lengths of the 1-, 2-, and 3-byte path-hash modes.
var prefixLens = []int{2, 4, 6}

// prefixIndexLocked maps every known node's pubkey prefixes (the 1/2/3-byte hex
// lengths) to the pubkeys carrying them, for relay-hop resolution. A hop resolves
// only on a UNIQUE match (see resolvePath). Caller holds r.mu.
func (r *Registry) prefixIndexLocked() map[string][]string {
	idx := make(map[string][]string, len(r.nodes)*len(prefixLens))
	for pk := range r.nodes {
		for _, n := range prefixLens {
			if len(pk) >= n {
				idx[pk[:n]] = append(idx[pk[:n]], pk)
			}
		}
	}
	return idx
}

// resolvePath maps each relay-path hop (a pubkey-prefix hash, hex) to the full
// public key of the node it identifies, using a prefix→pubkeys index. A hop
// resolves only when EXACTLY ONE known node carries that prefix; ambiguous
// (collision) or unknown hops stay empty. Result is parallel to hops.
func resolvePath(hops []string, idx map[string][]string) []string {
	if len(hops) == 0 {
		return nil
	}
	res := make([]string, len(hops))
	for i, h := range hops {
		if cands := idx[h]; len(cands) == 1 {
			res[i] = cands[0]
		}
	}
	return res
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
	// Keep the spam-floor map from growing without bound as gateways come and go.
	for k, t := range r.obsGate {
		if now.Sub(t) > r.cfg.RetainFor {
			delete(r.obsGate, k)
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
