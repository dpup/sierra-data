package meshcore

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// Cadence-aware presence (docs/design/mesh-topology-design.md §9): a node stays in
	// Snapshot for CadenceK × its own measured inter-advert interval, clamped to
	// [GraceFloor, GraceCeil]. A node with no cadence yet (one-shot / brand-new)
	// gets GraceFloor, so drive-through transients evaporate while a slow backbone
	// repeater is protected in proportion to its rhythm. Zero values default
	// (k=3, floor=3h, ceil=72h). RetainFor should be ≥ GraceCeil so a node lives
	// in memory for its whole presence window; it defaults to GraceCeil if unset.
	CadenceK   float64
	GraceFloor time.Duration
	GraceCeil  time.Duration
	// SilenceAfter is how long an open broker session may go without carrying a
	// single message before Brokers reports it as not delivering. A session can
	// be up and still deliver nothing (a stalled subscription, a broker-side ACL
	// change, a session another client keeps taking over), and only the message
	// clock can see that. <= 0 disables the check, leaving "session open" as the
	// only test.
	SilenceAfter time.Duration
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

	// instance is a random per-process suffix appended to every MQTT client id.
	// A broker allows one session per client id and closes the older one when a
	// second connects, so two processes sharing an id (a local dev server run
	// with production credentials, an overlapping deploy) would knock each other
	// off in a loop, each receiving a fraction of the feed. Generated once, so
	// this process's own reconnects keep their id.
	instance string

	mu      sync.Mutex
	nodes   map[string]*nodeEntry
	clients []mqtt.Client
	// links is parallel to clients: per-broker delivery clocks for Brokers.
	links []*brokerLink

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
		cfg:      cfg,
		baseCtx:  context.Background(),
		instance: newInstanceSuffix(),
		nodes:    make(map[string]*nodeEntry),
		obsGate:  make(map[string]time.Time),
		now:      time.Now,
	}
}

// newInstanceSuffix returns 8 random hex characters. A crypto/rand failure is
// effectively impossible on the platforms we run on; the clock fallback still
// differs between two processes started at different nanoseconds.
func newInstanceSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(b[:])
}

// clientID is the MQTT client id this process presents to a broker: the
// configured (or default) base, which names us to the broker operator, plus the
// per-process instance suffix, which keeps two of our processes from sharing a
// session.
func (r *Registry) clientID(idx int, b Broker) string {
	base := b.ClientID
	if base == "" {
		base = fmt.Sprintf("sierra-grid-meshcore-%d", idx)
	}
	return base + "-" + r.instance
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
// paho retries in the background, and Brokers reflects each one's live state.
// Returns an error only when no brokers are configured.
func (r *Registry) Connect(ctx context.Context) error {
	if len(r.cfg.Brokers) == 0 {
		return fmt.Errorf("meshcore: no brokers configured")
	}
	r.baseCtx = ctx
	for i, b := range r.cfg.Brokers {
		link := &brokerLink{url: b.URL, startedAt: r.now()}
		c := r.buildClient(ctx, i, b, link)
		c.Connect() // fire-and-forget; SetConnectRetry keeps trying
		r.mu.Lock()
		r.clients = append(r.clients, c)
		r.links = append(r.links, link)
		r.mu.Unlock()
	}
	return nil
}

// buildClient constructs a paho client that (re)subscribes on every connect.
func (r *Registry) buildClient(ctx context.Context, idx int, b Broker, link *brokerLink) mqtt.Client {
	topics := b.Topics
	if len(topics) == 0 {
		topics = []string{defaultTopic}
	}
	clientID := r.clientID(idx, b)

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

	handler := r.onMessage(link)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		logging.Infow(ctx, "MeshCore broker connected", "broker", b.URL, "clientId", clientID)
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

// onMessage returns a handler bound to one broker. Every message stamps the
// broker's delivery clock before anything else, whatever its packet type: the
// feed is a firehose of every type, and "is this subscription carrying
// anything" is a question about the pipe, not about adverts.
func (r *Registry) onMessage(link *brokerLink) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		link.lastMsg.Store(r.now().UnixNano())
		r.ingestRaw(m.Payload(), link.url)
	}
}

// brokerLink is one broker's delivery clock. Written from paho's callback
// goroutines and read by Brokers, so the clock is atomic rather than under mu
// (which every advert already contends for).
type brokerLink struct {
	url       string
	startedAt time.Time    // when Connect first dialed it; immutable
	lastMsg   atomic.Int64 // UnixNano of the last message of any type; 0 = none
}

// BrokerHealth is one configured broker's delivery state, judged by Brokers.
type BrokerHealth struct {
	URL string
	// Open reports whether an MQTT session is up NOW. It is deliberately not
	// paho's IsConnected, which with auto-reconnect on also reports true while
	// the client is reconnecting: a broker that had dropped us read as connected
	// indefinitely, so the fail-loud guard downstream could never fire.
	Open bool
	// LastMsgAt is when the broker last carried a message of any packet type.
	// Zero means none since this process started.
	LastMsgAt time.Time
	// Problem is empty when the broker is delivering, and otherwise says why
	// not, in words fit for a log line and a source's last_error.
	Problem string
}

// Delivering reports whether the broker is open and carrying traffic.
func (h BrokerHealth) Delivering() bool { return h.Problem == "" }

// Brokers reports every configured broker's delivery state, in config order.
// Empty before Connect, and always empty for a registry fed only by push
// forwarders.
//
// A broker is delivering when its session is open AND it has carried a message
// within SilenceAfter. The silence is measured from the last message, falling
// back to when Connect first dialed the broker, and never from the latest
// reconnect: a session that keeps being dropped and re-established while
// carrying nothing must not look fresh each time it comes back.
func (r *Registry) Brokers() []BrokerHealth {
	r.mu.Lock()
	clients := slices.Clone(r.clients)
	links := slices.Clone(r.links)
	r.mu.Unlock()

	now := r.now()
	out := make([]BrokerHealth, 0, len(links))
	for i, l := range links {
		h := BrokerHealth{URL: l.url, Open: clients[i] != nil && clients[i].IsConnectionOpen()}
		if ns := l.lastMsg.Load(); ns != 0 {
			h.LastMsgAt = time.Unix(0, ns)
		}
		h.Problem = deliveryProblem(h.Open, l.startedAt, h.LastMsgAt, now, r.cfg.SilenceAfter)
		out = append(out, h)
	}
	return out
}

// deliveryProblem is the judgment behind Brokers, split out so it is testable
// without a broker. It returns "" for a delivering broker.
func deliveryProblem(open bool, startedAt, lastMsgAt, now time.Time, silenceAfter time.Duration) string {
	heard := "no message since " + startedAt.UTC().Format(time.RFC3339)
	if !lastMsgAt.IsZero() {
		heard = "last message " + now.Sub(lastMsgAt).Round(time.Second).String() + " ago"
	}
	if !open {
		return "not connected (" + heard + ")"
	}
	if silenceAfter <= 0 {
		return ""
	}
	ref := lastMsgAt
	if ref.IsZero() {
		ref = startedAt
	}
	if now.Sub(ref) <= silenceAfter {
		return ""
	}
	return "connected but silent (" + heard + ")"
}

// PacketOutcome classifies what became of one packet envelope handed to
// IngestEnvelope. The MQTT path only logs it; the push-ingest path reports it
// back to the forwarder, which is why it is a value and not a log line.
type PacketOutcome int

const (
	// PacketAccepted: an ADVERT that decoded (and verified, when required) and
	// updated the registry.
	PacketAccepted PacketOutcome = iota
	// PacketIgnored: a well-formed envelope for a packet type we do not ingest.
	// The bridges are a firehose of every packet type; this is the common case.
	PacketIgnored
	// PacketMalformed: the envelope was not JSON, `raw` was not hex, or the frame
	// did not parse as an ADVERT.
	PacketMalformed
	// PacketRejected: an ADVERT whose Ed25519 signature did not verify, dropped
	// under RequireValidSignature. The forwarder relayed it faithfully; the
	// node (or whoever minted the frame) is the problem.
	PacketRejected
)

func (o PacketOutcome) String() string {
	switch o {
	case PacketAccepted:
		return "accepted"
	case PacketIgnored:
		return "ignored"
	case PacketMalformed:
		return "malformed"
	case PacketRejected:
		return "rejected"
	default:
		return fmt.Sprintf("PacketOutcome(%d)", int(o))
	}
}

// ingestRaw is the MQTT door: it parses the bridge's JSON envelope and, if it's
// an advert, updates the registry. Malformed/unrelated messages are dropped
// quietly (the feed is a firehose of every packet type; adverts are a small
// slice), with a Debug line for the malformed and a Warn for a bad signature.
func (r *Registry) ingestRaw(payload []byte, brokerID string) {
	outcome, err := r.IngestEnvelope(payload, brokerID, "")
	switch outcome {
	case PacketMalformed:
		logging.Debugw(r.baseCtx, "MeshCore: dropping malformed packet", "broker", brokerID, "error", err)
	case PacketRejected:
		logging.Warnw(r.baseCtx, "MeshCore: dropping advert with invalid signature", "broker", brokerID, "error", err)
	}
}

// IngestEnvelope applies one bridge-style JSON packet envelope (the per-packet
// document the map-ecosystem bridges publish: `packet_type`, hex `raw`, `SNR`,
// `RSSI`, `origin_id`, ...) to the registry, whichever door it arrived through.
//
// `source` names the delivery path for provenance — an MQTT broker URL, or a
// push reporter's id — and is recorded on the node and on the observation
// exactly as a broker URL is. `defaultGateway` names the receiving radio when
// the envelope carries no `origin_id`/`origin` of its own (a forwarder that
// knows which companion heard the packet but does not stamp every envelope);
// empty falls back to `source`, as the MQTT path always has.
//
// `raw` may be bare hex or a `meshcore://<hex>` link, the form the map's own
// uploader produces for the same frame, so a forwarder built from that code can
// hand its packets over unchanged.
//
// The returned error carries detail for a Malformed or Rejected outcome and is
// nil otherwise.
func (r *Registry) IngestEnvelope(payload []byte, source, defaultGateway string) (PacketOutcome, error) {
	var env packetEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return PacketMalformed, fmt.Errorf("bad envelope JSON: %w", err)
	}
	return r.ingestPacket(&env, source, defaultGateway)
}

// ingestPacket applies one decoded envelope to the registry. Split out from the
// transport plumbing so it is unit-testable without a broker.
func (r *Registry) ingestPacket(env *packetEnvelope, source, defaultGateway string) (PacketOutcome, error) {
	if env.PacketType.int() != packetTypeAdvert {
		return PacketIgnored, nil
	}
	rawHex := strings.TrimPrefix(strings.TrimSpace(env.Raw), "meshcore://")
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return PacketMalformed, fmt.Errorf("raw is not hex: %w", err)
	}
	// The bridge `raw` is the full over-the-air frame (header + path + payload),
	// so strip the transport framing before decoding the advert payload.
	adv, err := DecodeFrame(raw)
	if err != nil {
		return PacketMalformed, err
	}
	if r.cfg.RequireValidSignature && !adv.SignatureValid {
		return PacketRejected, fmt.Errorf("advert signature does not verify (pubkey %s)", adv.PubKey)
	}

	now := r.now()
	brokerID := source
	gw := firstNonEmpty(env.OriginID, env.Origin, defaultGateway, brokerID)

	r.mu.Lock()
	defer r.mu.Unlock()

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
	// Keep last-known telemetry, for the same reason as the location above: an
	// advert that omits SNR/RSSI must not erase what we already measured. Absent
	// and zero are different facts, and `present()` is what tells them apart —
	// before it existed, every bridge that left these keys out reset the node's
	// signal to 0 and the UI dutifully reported "0 dB".
	if env.SNR.present() {
		e.SNR = env.SNR.float()
	}
	// RSSI additionally rejects a literal 0: received signal strength is
	// negative dBm, so 0 is not a reading any radio produces — a bridge sending
	// it means "unknown", whatever the JSON says.
	if env.RSSI.present() && env.RSSI.int() != 0 {
		e.RSSI = int32(env.RSSI.int())
	}
	// Hop count is the last-heard advert's path length, decoded from the
	// authoritative over-the-air frame rather than the bridge's envelope — so it
	// is always present, and 0 is MEANINGFUL (a zero-hop beacon heard directly).
	// It must not get the absent-vs-zero treatment above. The path itself is
	// per-reception — captured in the observation below, not on node presence.
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
	return PacketAccepted, nil
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

// ResolvePrefix maps a pubkey PREFIX (lowercase hex, any even length) to the
// full public key of the node it identifies, on the same UNIQUE-match rule
// resolvePath uses for relay hops: a prefix carried by two known nodes resolves
// to neither.
//
// Operator monitors identify a node by a prefix — Alan's reports use 8 bytes of
// the key — while our event ids are built from the full key. Resolution is what
// makes a pushed report and an MQTT advert converge on ONE event instead of two,
// so the rule has to be strict in the same direction as everything else here:
// guessing wrong would attach a repeater's battery reading to a different
// repeater, which is worse than leaving it unresolved.
//
// A full-length prefix still goes through the catalog, so an id for a node we
// have never heard of stays unresolved rather than being minted as fact.
func (r *Registry) ResolvePrefix(prefix string) (string, bool) {
	if len(prefix) == 0 {
		return "", false
	}
	prefix = strings.ToLower(prefix)
	r.mu.Lock()
	defer r.mu.Unlock()
	var match string
	for pk := range r.nodes {
		if !strings.HasPrefix(pk, prefix) {
			continue
		}
		if match != "" {
			return "", false // ambiguous — two known nodes share this prefix
		}
		match = pk
	}
	return match, match != ""
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

// flexInt / flexFloat tolerate the several shapes community bridges send a
// number in ("-91", -91, "-91.0", "", null, or the key omitted entirely).
//
// `set` records whether a REAL value arrived, which absence and an explicit
// null do not provide. Without it a missing SNR is indistinguishable from a
// measured 0, and the node-state merge wiped a known reading every time a
// bridge left the field out — the same class of bug the location merge already
// guards against a few lines above it.
type flexInt struct {
	v   int64
	set bool
}
type flexFloat struct {
	v   float64
	set bool
}

func (f *flexInt) int() int64       { return f.v }
func (f *flexInt) present() bool    { return f.set }
func (f *flexFloat) float() float64 { return f.v }
func (f *flexFloat) present() bool  { return f.set }

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
	f.set = true
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
	f.set = true
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
