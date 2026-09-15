package ingest

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/pushingest"
	"github.com/dpup/sierra-data/internal/store"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// MeshCore presence source constants. Nodes deep-link to the community map
// (there is no authoritative per-node page); see the source-url-deeplinks memo.
const (
	meshSourceID    = "meshcore"
	meshSourceName  = "MeshCore Mesh"
	meshAttribution = "MeshCore community mesh"
	meshMapURL      = "https://map.meshcore.io"

	// meshLocationDecimals rounds a stored coordinate to ~4 dp ≈ 11 m, matching
	// the repo-wide GeoJSON precision convention. It does NOT damp GPS jitter,
	// though it was written hoping to — see meshPositionEpsilonMeters.
	meshLocationDecimals = 1e4

	// meshPositionEpsilonMeters is how far an advert must fall from a node's
	// STORED position before we believe the node moved.
	//
	// Quantization alone cannot do this job, for two reasons measured on live
	// data. The jitter is bigger than any sane grid: one companion's adverts
	// spanned 245 m across 18 distinct positions while the node sat still, so a
	// 11 m grid rounds noise to a different cell almost every time. And rounding
	// has no HYSTERESIS — a node parked near a cell boundary flips between two
	// cells forever no matter how coarse the grid is, because the boundary is
	// what it is sitting on.
	//
	// A threshold measured from the last stored position has hysteresis built
	// in, the same shape as the reachability window's: noise changes nothing, a
	// real move registers exactly once. 150 m sits well above the observed noise
	// (p95 ≈ 70 m from a node's median position) and far below any movement a
	// coverage map cares about — a genuinely mobile node still tracked 23 km of
	// travel in the same sample.
	meshPositionEpsilonMeters = 150.0

	// meshPubKeyHex is the hex length of a full MeshCore Ed25519 public key. An
	// event id native part shorter than this is a PREFIX — a node an operator
	// monitor reported that we have not yet matched to a full key.
	meshPubKeyHex = 64

	// meshPushNodeType is the role assumed for a node known only from the
	// mesh.repeater push stream. It is not a guess: the stream's contract is
	// literally "repeaters an operator monitors". An MQTT advert, which carries
	// the node's own self-declared role, always wins over it.
	meshPushNodeType = "repeater"
)

// MeshRegistry is the read side of the MeshCore MQTT subscriber the normalizer
// depends on (satisfied by *meshcore.Registry; a fake is used in tests).
type MeshRegistry interface {
	// Snapshot returns the nodes currently present (each within its own
	// cadence-derived window; see meshcore.Registry.Snapshot).
	Snapshot() []meshcore.NodeState
	// Health reports connected broker count and last-message time.
	Health() (connected int, lastMsg time.Time)
	// DrainObservations returns the receptions buffered since the last drain
	// (cleared), for the append-only relay-observation store (Tier 0).
	DrainObservations() []meshcore.Observation
	// ResolvePrefix maps a pubkey prefix to a full key on a unique match.
	ResolvePrefix(prefix string) (string, bool)
}

// MeshPushSource is the read side of the authenticated push-ingest buffer
// (satisfied by *pushingest.Registry). Nil when no reporter is configured.
type MeshPushSource interface {
	MeshReports() pushingest.MeshSnapshot
	ReporterIDs(stream string) []string
}

// NetworkNormalizer projects mesh-node presence into MESH events from TWO kinds
// of input, and the interesting part of this file is how they are combined.
//
//   - The MQTT advert firehose (internal/clients/meshcore): broadcast, signed,
//     carries identity and location, and only ever offers positive evidence — a
//     node that says nothing is indistinguishable from a node that is gone.
//   - Operator monitors pushing to /api/v1/ingest/mesh.repeater
//     (internal/pushingest): they log into a node's admin interface, so they
//     carry battery/airtime/counters nothing broadcasts, they can see nodes no
//     bridge covers, and — uniquely — they can report a FAILED attempt.
//
// Both are push sources wrapped in the pull-based Normalizer contract: each
// accumulates in memory and Poll takes a snapshot on the scheduler's tick, which
// keeps single-writer discipline, tick-based health and the disappearance sweep
// unchanged.
//
// A node seen by both is ONE event, because both inputs resolve to the same
// event id (a monitor's pubkey prefix is resolved against the node catalog).
// That is the whole dedupe story — there is no merge of two stored events,
// because two were never created.
type NetworkNormalizer struct {
	cfg      *config.Config
	registry MeshRegistry   // nil when MeshCore MQTT is not configured
	push     MeshPushSource // nil when no reporter is configured
	// brokerOps maps a broker URL to its config (operator name + https page),
	// used to attribute a node's provenance to the MQTT server's operator.
	brokerOps map[string]config.MeshcoreBroker
	now       func() time.Time
}

// NewNetworkNormalizer wires the normalizer to its inputs. Either may be nil,
// but not both: a poller with no input has nothing to say and the server does
// not register it.
func NewNetworkNormalizer(cfg *config.Config, registry MeshRegistry, push MeshPushSource) *NetworkNormalizer {
	ops := make(map[string]config.MeshcoreBroker, len(cfg.Grid.Meshcore.Brokers))
	for _, b := range cfg.Grid.Meshcore.Brokers {
		ops[b.URL] = b
	}
	return &NetworkNormalizer{cfg: cfg, registry: registry, push: push, brokerOps: ops, now: time.Now}
}

// SourceIDs implements Normalizer: the mesh source itself, plus one row per
// configured reporter.
//
// A reporter gets its own source row so /api/v1/sources answers "is Alan's
// monitor still reporting?" the same way it answers that question for every
// other feed. Those rows are HEALTH ONLY — no event is ever stored with a
// reporter as its source. Keeping every mesh event on the `meshcore` source,
// whichever input heard it, is deliberate: a source_id that flipped as inputs
// came and went would both mint a revision and confuse the per-source sweep,
// which diffs each source's polled set against its stored set.
func (n *NetworkNormalizer) SourceIDs() []string {
	ids := []string{meshSourceID}
	if n.push != nil {
		ids = append(ids, n.push.ReporterIDs(pushingest.MeshStream)...)
	}
	return ids
}

// mergedNode accumulates every input's view of one node before it becomes an
// event. Keyed by the event's native id: the full public key when known, the
// reported prefix when not.
type mergedNode struct {
	key    string // full pubkey, or the prefix when unresolved
	mqtt   *meshcore.NodeState
	pushed []pushingest.MeshNodeReport
}

// Poll implements Normalizer.
//
// THE FAIL-LOUD RULE, generalized to several inputs: the poll fails hard when
// EVERY input is unavailable, and suppresses the sweep when SOME are. The
// original rule ("no broker connected ⇒ hard error") exists because an empty
// snapshot produced by OUR outage must never read as "every node left the mesh".
// With two kinds of input that same reasoning splits in two:
//
//   - no broker AND no live reporter ⇒ hard error, exactly as before;
//   - one input healthy, another silent ⇒ emit what we actually have, but set
//     SweepSuppress, because the nodes missing from this snapshot are missing
//     for our reason, not theirs.
//
// Suppression is bounded on the reporter side (pushingest.reporterDeadMultiple):
// a monitor that never comes back must not freeze the layer's lifecycle forever.
func (n *NetworkNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	now := n.now()

	boxes := n.geofence()
	if n.registry != nil && len(boxes) == 0 {
		return nil, errEmptyScope("meshcore geofence")
	}

	connected := 0
	var nodes []meshcore.NodeState
	if n.registry != nil {
		connected, _ = n.registry.Health()
		if connected > 0 {
			nodes = n.registry.Snapshot()
		}
	}

	var snap pushingest.MeshSnapshot
	if n.push != nil {
		snap = n.push.MeshReports()
	}

	if connected == 0 && snap.Live == 0 {
		return nil, fmt.Errorf(
			"meshcore: no brokers connected and no reporter live; not asserting node disappearance")
	}

	merged := make(map[string]*mergedNode, len(nodes)+len(snap.Reports))
	for i := range nodes {
		nd := nodes[i]
		// Scope: an ADVERTISED node must have a location inside the geofence.
		// Locationless adverts can't be geofenced and are dropped. Pushed nodes
		// are exempt from both tests (below) — a reporter is an authenticated
		// operator asserting its own scope, not an anonymous broadcast.
		if !nd.HasLocation || !inAnyBounds(nd.Lat, nd.Lng, boxes) {
			continue
		}
		merged[nd.PubKey] = &mergedNode{key: nd.PubKey, mqtt: &nd}
	}

	// Resolve each reported prefix against the catalog of full keys we know:
	// this tick's adverts, the store's existing events, and the MQTT registry's
	// retained nodes. Resolution is what collapses "the node Alan monitors" and
	// "the node the bridge hears" into one event id.
	catalog := n.fullKeyCatalog(merged, prior)
	for _, rep := range snap.Reports {
		key := rep.NodeID
		if len(key) < meshPubKeyHex {
			if full, ok := resolveUnique(catalog, key); ok {
				key = full
			} else if n.registry != nil {
				if full, ok := n.registry.ResolvePrefix(key); ok {
					key = full
				}
			}
		}
		m := merged[key]
		if m == nil {
			m = &mergedNode{key: key}
			merged[key] = m
		}
		m.pushed = append(m.pushed, rep)
	}

	events := make([]*gridv1.Event, 0, len(merged))
	var forceWrite []string
	for _, m := range merged {
		ev := n.buildEvent(m, now, prior)
		events = append(events, ev)
		if n.shouldPersistTelemetry(ev, prior, now) {
			forceWrite = append(forceWrite, ev.GetId())
		}
	}
	// Deterministic order so a diff of two ticks is readable and tests are stable.
	sort.Slice(events, func(i, j int) bool { return events[i].GetId() < events[j].GetId() })

	promotions := promotedPrefixKeys(merged, prior)
	result := &PollResult{
		Events:               events,
		ForceWrite:           forceWrite,
		Superseded:           supersededIDs(promotions),
		MeshObservations:     n.drainObservations(),
		MeshTelemetry:        meshTelemetrySamples(events, now),
		MeshTelemetryRenames: promotions,
		PerSource:            n.reporterHealth(snap),
	}
	if snap.SuppressSweep {
		result.SweepSuppress = append(result.SweepSuppress, meshSourceID)
	}
	return result, nil
}

// drainObservations drains the MQTT reception firehose into the append-only
// observation store (Tier 0). Unlike the presence events above, observations are
// NOT geofenced here — a relay hop just outside the fence can still connect two
// in-region nodes, and the raw path is retained for re-resolution; the
// edge-build/projection step applies the located + in-region filter. These are
// measurements, not events.
func (n *NetworkNormalizer) drainObservations() []store.MeshObservation {
	if n.registry == nil {
		return nil
	}
	obs := n.registry.DrainObservations()
	if len(obs) == 0 {
		return nil
	}
	storeObs := make([]store.MeshObservation, 0, len(obs))
	for _, o := range obs {
		storeObs = append(storeObs, store.MeshObservation{
			PubKey:    o.PubKey,
			HeardAt:   o.HeardAt,
			Broker:    o.Broker,
			Gateway:   o.Gateway,
			SNR:       o.SNR,
			RSSI:      o.RSSI,
			HopCount:  o.HopCount,
			Path:      o.Path,
			PathNodes: o.PathNodes,
		})
	}
	return storeObs
}

// reporterHealth maps each reporter's state onto its source row. A reporter that
// has never reported is an error, not a success: RecordAttempt(nil) would paint
// a monitor that was never set up as a healthy feed.
func (n *NetworkNormalizer) reporterHealth(snap pushingest.MeshSnapshot) map[string]error {
	if len(snap.Reporters) == 0 {
		return nil
	}
	out := make(map[string]error, len(snap.Reporters))
	for _, h := range snap.Reporters {
		switch h.State {
		case pushingest.ReporterOK:
			out[h.ID] = nil
		case pushingest.ReporterUnknown:
			out[h.ID] = fmt.Errorf("no report received yet")
		default:
			err := fmt.Errorf("no report since %s (%s)",
				h.LastAcceptedAt.UTC().Format(time.RFC3339), h.State)
			if h.LastError != "" {
				err = fmt.Errorf("%w; last error: %s", err, h.LastError)
			}
			out[h.ID] = err
		}
	}
	return out
}

// buildEvent renders one merged node. Field precedence is by INPUT CLASS, never
// by recency: identity fields are hashed, so a rule like "most recent wins"
// would let two inputs that disagree flip the winner every tick and mint a
// revision each time. A signed advert beats a monitor's reading; among monitors,
// configured priority then reporter id.
func (n *NetworkNormalizer) buildEvent(m *mergedNode, now time.Time, prior Prior) *gridv1.Event {
	reports := slices.Clone(m.pushed)
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].Priority != reports[j].Priority {
			return reports[i].Priority > reports[j].Priority
		}
		return reports[i].ReporterID < reports[j].ReporterID
	})

	name, nodeType := "", ""
	if m.mqtt != nil {
		name, nodeType = m.mqtt.Name, m.mqtt.Role
	}
	for _, r := range reports {
		if name == "" {
			name = r.Name
		}
		if nodeType == "" {
			nodeType = meshPushNodeType
		}
	}

	ev := NewEvent(
		meshSourceID+":"+m.key,
		gridv1.Layer_MESH,
		gridv1.Severity_INFO,
		gridv1.EventStatus_ACTIVE,
		meshHeadline(name, nodeType, m.key),
	)
	ev.Category = nodeType
	// NO AreaLabel. It is the human description of WHERE something is — "Hwy 4
	// at Avery", "10km NE of Murphys" — and this was setting it to the node's
	// own NAME, which is neither a place nor new information: the name is
	// already the headline and `mesh.name`. It made a mesh event claim a
	// location it does not have.
	//
	// A mesh node has coordinates and nothing else; the Grid does not reverse
	// geocode, and inventing a description from lat/lng would be worse than
	// leaving it empty. Empty is the honest answer, and the geometry carries
	// the position for anyone who needs it.
	ev.CanonicalUrl = meshMapURL

	var brokers []string
	if m.mqtt != nil {
		ev.Geometry = stablePosition(prior, ev.GetId(), m.mqtt.Lat, m.mqtt.Lng)
		brokers = m.mqtt.Brokers
	}
	// A node known only from a monitor has NO geometry: a report carries no
	// coordinates, and the Grid does not invent them. Its place attachment
	// therefore comes from the reporter's configured placeIds, which UpsertEvent
	// unions with (absent) geometric matches. Where a reporter configures none,
	// the node is honestly place-less until an advert supplies a location.
	ev.PlaceIds = reporterPlaceIDs(reports)

	// observed_at is when this node's content was observed; it's zeroed in the
	// content hash, so it only persists on an actual identity/location change.
	// Mesh node clocks are frequently skewed (we've seen adverts stamped
	// months in the future), so the event's observed time — which the feed
	// orders and `since`-filters on — is OUR receive time, never the
	// node-reported advert timestamp. The node's stamp is kept only in
	// telemetry.lastAdvertAt (diagnostic; reveals the skew).
	observed := time.Time{}
	if m.mqtt != nil {
		observed = m.mqtt.LastHeardAt
	}
	for _, r := range reports {
		// A successful admin read proves the node is alive; where a monitor has
		// never reached it, the report time still stands as "this is current
		// information about the node" — an actively-monitored, unreachable
		// repeater is a live and useful fact, not a stale record.
		if r.LastSuccess.After(observed) {
			observed = r.LastSuccess
		} else if r.LastSuccess.IsZero() && r.ReportedAt.After(observed) {
			observed = r.ReportedAt
		}
	}
	if !observed.IsZero() {
		ev.ObservedAt = tsProto(observed)
	}

	ev.Provenance = n.meshProvenance(brokers)
	if len(brokers) == 0 {
		keepPriorAttribution(ev, prior)
	}
	ev.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
		PublicKey:    m.key,
		NodeType:     nodeType,
		Name:         name,
		Reachability: n.reachability(reports, now),
		Telemetry:    meshTelemetry(m.mqtt, reports),
	}}
	return ev
}

// reachability derives the HASHED reachability field.
//
// It deliberately ignores the monitor's own `online` boolean and uses the age of
// its last SUCCESS instead. The boolean is a per-poll verdict that flips on any
// marginal repeater, and because this field is hashed, every flip would mint a
// revision — the history would fill with noise and the signal ("Lilac Park has
// been down since Tuesday") would be buried in it. An age threshold has
// hysteresis built in: one missed poll changes nothing, a sustained outage
// transitions exactly once.
//
// Any monitor reaching the node makes it reachable; UNSPECIFIED means no monitor
// has even attempted it, which is not the same as unreachable and must not
// render as one.
func (n *NetworkNormalizer) reachability(reports []pushingest.MeshNodeReport, now time.Time) gridv1.MeshReachability {
	window := n.cfg.Grid.Ingest.UnreachableAfterOrDefault()
	var bestSuccess, bestAttempt time.Time
	for _, r := range reports {
		if r.LastSuccess.After(bestSuccess) {
			bestSuccess = r.LastSuccess
		}
		if r.LastAttempt.After(bestAttempt) {
			bestAttempt = r.LastAttempt
		}
		// A report with no attempt stamps at all still means the monitor listed
		// this node, so the report time itself counts as an attempt.
		if r.LastAttempt.IsZero() && r.ReportedAt.After(bestAttempt) {
			bestAttempt = r.ReportedAt
		}
	}
	if bestAttempt.IsZero() {
		return gridv1.MeshReachability_MESH_REACHABILITY_UNSPECIFIED
	}
	if !bestSuccess.IsZero() && now.Sub(bestSuccess) <= window {
		return gridv1.MeshReachability_REACHABLE
	}
	return gridv1.MeshReachability_UNREACHABLE
}

// meshTelemetry assembles the volatile block. Everything here is zeroed by
// store.ContentHash, so none of it mints a revision — which is exactly why the
// counters and gauges an operator monitor reports belong in it, and exactly why
// persisting it needs PollResult.ForceWrite (see shouldPersistTelemetry).
func meshTelemetry(mqtt *meshcore.NodeState, reports []pushingest.MeshNodeReport) *gridv1.MeshTelemetry {
	t := &gridv1.MeshTelemetry{}
	// The signal block is set ONLY where an MQTT bridge actually heard the node.
	// These are wrapper types precisely so that this `if` is visible on the wire:
	// a node an operator monitors but the mesh has never heard reports no SNR,
	// not 0 dB, and no hop count, not "heard direct".
	if mqtt != nil {
		t.Snr = wrapperspb.Double(mqtt.SNR)
		t.Rssi = wrapperspb.Int32(mqtt.RSSI)
		t.HopCount = wrapperspb.UInt32(mqtt.HopCount)
		t.Gateways = mqtt.Gateways
		t.LastAdvertAt = tsProto(mqtt.LastAdvertAt)
	}
	// The freshest successful sample wins; reports arrive pre-sorted by
	// precedence, so an equal timestamp resolves to the higher-priority monitor.
	var best *pushingest.MeshNodeReport
	for i := range reports {
		r := &reports[i]
		if r.Telemetry == nil {
			continue
		}
		if best == nil || r.LastSuccess.After(best.LastSuccess) {
			best = r
		}
	}
	if best != nil {
		t.Admin = best.Telemetry
	}
	return t
}

// shouldPersistTelemetry decides whether this tick must write an otherwise
// hash-equal event so a fresh telemetry sample reaches the store.
//
// Without this the sample would essentially never be written: telemetry is
// excluded from the content hash (so it mints no revision — the point), and the
// scheduler skips the write path entirely for hash-equal events (so nothing
// persists it). The result would be a battery reading frozen at whatever was
// current the last time the node was renamed or moved.
//
// It is COALESCED rather than per-tick for the reason the write-skip exists in
// the first place: every commit on a network filesystem invalidates every
// reader's page cache, and the mesh poller was the layer that made that
// measurable. One write per node per telemetryPersistInterval bounds the cost
// independently of how fast monitors report, and the staleness it trades away is
// bounded by the same interval.
func (n *NetworkNormalizer) shouldPersistTelemetry(ev *gridv1.Event, prior Prior, now time.Time) bool {
	admin := ev.GetMesh().GetTelemetry().GetAdmin()
	if admin == nil {
		return false
	}
	if prior == nil {
		return false
	}
	stored := prior.ByID(ev.GetId())
	if stored == nil {
		return false // new event: the normal write path handles it
	}
	prev := stored.GetMesh().GetTelemetry().GetAdmin()
	if prev == nil {
		return true // first sample for a node that already existed
	}
	if prev.GetReportedAt() == nil {
		return true
	}
	return now.Sub(prev.GetReportedAt().AsTime()) >= n.cfg.Grid.Ingest.TelemetryPersistIntervalOrDefault()
}

// supersededPrefixIDs names prefix-keyed events that a full-key event now
// replaces — the promotion case.
//
// A monitor identifies a node by a prefix of its public key. Until an advert
// tells us the full key, that node's event is keyed by the prefix. When the
// bridge finally hears it, the same node acquires its proper id, and the old
// prefix event must go — otherwise the node is drawn twice for the whole expire
// grace.
//
// This is positive evidence, which is the only thing Superseded may ever be
// populated from: we are not saying "the prefix event stopped appearing", we are
// saying "here is the full key it was always a prefix OF, and here is the event
// now carrying it". Same shape as a standalone fire perimeter being adopted by a
// CAL FIRE incident.
func promotedPrefixKeys(merged map[string]*mergedNode, prior Prior) []MeshKeyRename {
	if prior == nil {
		return nil
	}
	var out []MeshKeyRename
	for _, ev := range prior.ForSource(meshSourceID) {
		id := ev.GetId()
		native, ok := strings.CutPrefix(id, meshSourceID+":")
		if !ok || len(native) >= meshPubKeyHex {
			continue // already a full key (or not ours): nothing to promote
		}
		for key := range merged {
			if len(key) == meshPubKeyHex && strings.HasPrefix(key, native) {
				// Both halves are in hand HERE and nowhere else, which is why the
				// telemetry rename is paired with the supersession rather than
				// inferred later: after this tick the provisional id is retired and
				// nothing in the store connects the two keys.
				out = append(out, MeshKeyRename{From: native, To: key})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// supersededIDs names the provisional EVENTS a promotion retires.
func supersededIDs(promotions []MeshKeyRename) []string {
	if len(promotions) == 0 {
		return nil
	}
	out := make([]string, 0, len(promotions))
	for _, p := range promotions {
		out = append(out, meshSourceID+":"+p.From)
	}
	return out
}

// meshTelemetrySamples projects this tick's events into archive rows — one per
// node that carries a monitor sample.
//
// Built from the EVENTS rather than from the raw reports because this is where
// a prefix has already been resolved to the node's full public key: filing a
// sample under the prefix and the same node's later samples under the full key
// would split one node's history in two.
//
// `now` is our receive time. It is written once — the insert ignores a duplicate
// — so it records when we FIRST saw this sample, which is the honest meaning
// against the monitor's own reported_at beside it.
func meshTelemetrySamples(events []*gridv1.Event, now time.Time) []store.MeshTelemetrySample {
	var out []store.MeshTelemetrySample
	for _, ev := range events {
		d := ev.GetMesh()
		admin := d.GetTelemetry().GetAdmin()
		if admin == nil || d.GetPublicKey() == "" || admin.GetReportedAt() == nil {
			continue
		}
		out = append(out, store.MeshTelemetrySample{
			PubKey:     d.GetPublicKey(),
			ReportedAt: admin.GetReportedAt().AsTime(),
			ReceivedAt: now,
			Reporter:   admin.GetReporterId(),

			BatteryVolts:     wrapperFloat(admin.GetBatteryVolts()),
			BatteryPct:       wrapperFloat(admin.GetBatteryPercent()),
			BatteryPctSource: admin.GetBatteryPercentSource(),
			TemperatureC:     wrapperFloat(admin.GetTemperatureC()),
			Humidity:         wrapperFloat(admin.GetHumidity()),
			Pressure:         wrapperFloat(admin.GetPressure()),
			NoiseFloorDBm:    wrapperInt32(admin.GetNoiseFloorDbm()),
			LastSNRdB:        wrapperFloat(admin.GetLastSnrDb()),
			LastRSSIdBm:      wrapperInt32(admin.GetLastRssiDbm()),
			TxQueueLen:       wrapperInt32(admin.GetTxQueueLen()),

			UptimeS:     admin.GetUptimeSeconds(),
			AirtimeMs:   admin.GetAirtimeMs(),
			RxAirtimeMs: admin.GetRxAirtimeMs(),
			PacketsSent: admin.GetPacketsSent(),
			PacketsRecv: admin.GetPacketsReceived(),
			SentFlood:   admin.GetSentFlood(),
			SentDirect:  admin.GetSentDirect(),
			RecvFlood:   admin.GetRecvFlood(),
			RecvDirect:  admin.GetRecvDirect(),
			DirectDups:  admin.GetDirectDups(),
			FloodDups:   admin.GetFloodDups(),
			FullEvts:    admin.GetFullEvents(),
			RecvErrors:  admin.GetRecvErrors(),
		})
	}
	return out
}

// wrapperFloat / wrapperInt32 carry a wrapper's "unset" through to the archive
// as NULL. GetValue() would flatten it to 0 — the one thing the wrappers exist
// to prevent.
func wrapperFloat(v *wrapperspb.DoubleValue) *float64 {
	if v == nil {
		return nil
	}
	f := v.GetValue()
	return &f
}

func wrapperInt32(v *wrapperspb.Int32Value) *int64 {
	if v == nil {
		return nil
	}
	n := int64(v.GetValue())
	return &n
}

// fullKeyCatalog collects every full public key we currently know: this tick's
// adverts plus the store's existing mesh events. The store half matters when
// MQTT is down or disabled — a node's full key, once learned, stays known.
func (n *NetworkNormalizer) fullKeyCatalog(merged map[string]*mergedNode, prior Prior) []string {
	keys := make([]string, 0, len(merged))
	for k := range merged {
		if len(k) == meshPubKeyHex {
			keys = append(keys, k)
		}
	}
	if prior != nil {
		for _, ev := range prior.ForSource(meshSourceID) {
			if native, ok := strings.CutPrefix(ev.GetId(), meshSourceID+":"); ok && len(native) == meshPubKeyHex {
				keys = append(keys, native)
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// resolveUnique returns the single catalog entry carrying a prefix. Two matches
// resolve to none: attaching a repeater's telemetry to the wrong repeater is
// worse than leaving it unattached. Same rule meshcore.resolvePath uses on
// relay hops.
func resolveUnique(catalog []string, prefix string) (string, bool) {
	var match string
	for _, k := range catalog {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = k
	}
	return match, match != ""
}

// reporterPlaceIDs unions the configured place attachments of every monitor that
// reported a node, sorted so the set is stable across ticks (place_ids are
// excluded from the content hash, but a stable order keeps diffs readable).
func reporterPlaceIDs(reports []pushingest.MeshNodeReport) []string {
	var out []string
	for _, r := range reports {
		out = append(out, r.PlaceIDs...)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// meshProvenance attributes a node to the operator(s) of the MQTT broker(s) it
// was heard on: provenance source_url is the primary operator's https page (not
// the wss:// broker URL), and attribution names the operator(s). Falls back to
// the community map when a broker has no configured operator — which is also the
// push-only case, where no broker heard the node at all. Brokers are sorted, so
// a stable broker set yields a stable provenance (no per-poll revision churn).
func (n *NetworkNormalizer) meshProvenance(brokers []string) *gridv1.Provenance {
	sourceURL := ""
	var names []string
	seen := map[string]bool{}
	for _, b := range brokers {
		mb, ok := n.brokerOps[b]
		if !ok {
			continue
		}
		if sourceURL == "" && mb.OperatorURL != "" {
			sourceURL = mb.OperatorURL
		}
		if mb.Operator != "" && !seen[mb.Operator] {
			seen[mb.Operator] = true
			names = append(names, mb.Operator)
		}
	}
	attribution := meshAttribution
	if len(names) > 0 {
		attribution = meshAttribution + " via " + strings.Join(names, ", ")
	}
	if sourceURL == "" {
		sourceURL = meshMapURL
	}
	return NewProvenance(meshSourceID, meshSourceName, attribution, sourceURL)
}

// keepPriorAttribution holds on to the bridge credit when this tick did not
// learn which broker heard the node.
//
// meshProvenance derives `attribution` and `source_url` from the brokers in THIS
// tick's snapshot, and both are hashed. A node rehydrated from the store on boot
// has no broker until its next advert — Seed carries pubkey, role, name,
// location and reconstructed cadence, but not the broker set — so after every
// restart a repeater on a 12-hour cycle sat attributed to nobody for hours and
// then flipped back when it was next heard. Two revisions per node per deploy,
// which means the churn scaled with how often we ship: measured across 45 live
// nodes, 505 attribution flips, 11.2 each, 26 of them inside one minute.
//
// Not knowing which bridge heard a node is missing information, not a change of
// source — the fourth instance of the rule the wildfire perimeter, the PG&E
// footprint and the mesh position all follow. Only the two fields that name the
// bridge are carried: `fetched_at` stays FRESH because we did observe the node
// this tick, we simply cannot say through whom.
func keepPriorAttribution(ev *gridv1.Event, prior Prior) {
	prev := priorByID(prior, ev.GetId()).GetProvenance()
	if prev.GetAttribution() == "" {
		return
	}
	ev.Provenance.Attribution = prev.GetAttribution()
	ev.Provenance.SourceUrl = prev.GetSourceUrl()
}

// geofence returns the bboxes an ADVERTISED node's location must fall within.
// MeshCore presence is monitored over a deliberately WIDER area than the hazard
// region (config meshcore.bounds — e.g. to include the Bay Area for liveness
// confidence); when unset it falls back to the union of hazard-area bounds.
//
// Pushed nodes are not geofenced at all. The fence exists to scope an anonymous
// global broadcast feed; a reporter is an authenticated operator whose whole
// report is an assertion about its own equipment, and the nodes that most need
// this path — quiet backbone repeaters — are precisely the ones that advertise
// no location to test.
func (n *NetworkNormalizer) geofence() []config.GeoBounds {
	if len(n.cfg.Grid.Meshcore.Bounds) > 0 {
		return n.cfg.Grid.Meshcore.Bounds
	}
	boxes := make([]config.GeoBounds, 0, len(n.cfg.Hazards.Areas))
	for _, a := range n.cfg.Hazards.Areas {
		boxes = append(boxes, a.Bounds)
	}
	return boxes
}

// inAnyBounds reports whether a coordinate falls within any bbox. Precise polygon
// attachment happens later in the store's matchPlaces; this is the coarse gate.
func inAnyBounds(lat, lng float64, boxes []config.GeoBounds) bool {
	for _, b := range boxes {
		if b.Contains(lat, lng) {
			return true
		}
	}
	return false
}

// meshHeadline renders a card-safe one-liner: "<name> (<role>)", falling back
// to a short key when a node has no name.
func meshHeadline(name, nodeType, key string) string {
	if name == "" {
		short := key
		if len(short) > 8 {
			short = short[:8]
		}
		name = "node " + short
	}
	if nodeType == "" {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, nodeType)
}

func quantizeCoord(v float64) float64 {
	return math.Round(v*meshLocationDecimals) / meshLocationDecimals
}

// stablePosition returns the node's geometry for this tick: the fresh advert
// position, unless the node is still within meshPositionEpsilonMeters of where
// it is already stored, in which case the STORED geometry rides forward byte for
// byte.
//
// Geometry is hashed — movement is meaningful — so every wobble in a node's
// self-reported GPS fix was minting a revision. One stationary companion reached
// revision 75 that way, 18 distinct positions inside a 245 m circle, each one a
// full-blob snapshot recording that a node had not moved.
//
// Returning the PRIOR geometry rather than a re-rounded fresh one is the point:
// equal bytes are what make the content hash equal. Rounding a new fix to a grid
// produces a new value whenever the fix crosses a cell line, which for a node
// parked on a boundary is most of the time.
//
// Same shape as the wildfire perimeter and PG&E footprint rules — when this tick
// says something we have reason not to believe, keep what we had — but a
// different reason: those are upstream feeds blinking, this is a real
// measurement that is simply less precise than the thing it measures.
func stablePosition(prior Prior, id string, lat, lng float64) *gridv1.Geometry {
	fresh := GeometryFromPoint(quantizeCoord(lat), quantizeCoord(lng))
	prev := priorByID(prior, id).GetGeometry()
	c := prev.GetCentroid()
	if c == nil {
		return fresh // nothing stored (or no location yet): this fix is the truth
	}
	if geojson.MetersBetween(c.GetLat(), c.GetLng(), lat, lng) > meshPositionEpsilonMeters {
		return fresh
	}
	return prev
}
