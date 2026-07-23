package ingest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// MeshCore presence source constants. Nodes deep-link to the community map
// (there is no authoritative per-node page); see the source-url-deeplinks memo.
const (
	meshSourceID    = "meshcore"
	meshSourceName  = "MeshCore Mesh"
	meshAttribution = "MeshCore community mesh"
	meshMapURL      = "https://map.meshcore.io"

	defaultMeshActiveWindow = 30 * time.Minute
	// meshLocationDecimals damps GPS jitter: ~4 dp ≈ 11 m. Location is hashed
	// content, so un-quantized jitter would mint spurious revisions.
	meshLocationDecimals = 1e4
)

// MeshRegistry is the read side of the MeshCore MQTT subscriber the normalizer
// depends on (satisfied by *meshcore.Registry; a fake is used in tests).
type MeshRegistry interface {
	// Snapshot returns nodes heard within activeWindow.
	Snapshot(activeWindow time.Duration) []meshcore.NodeState
	// Health reports connected broker count and last-message time.
	Health() (connected int, lastMsg time.Time)
	// DrainObservations returns the receptions buffered since the last drain
	// (cleared), for the append-only relay-observation store (Tier 0).
	DrainObservations() []meshcore.Observation
}

// NetworkNormalizer projects MeshCore node presence into NETWORK events. It
// wraps the long-lived MQTT subscriber (a push source) behind the pull-based
// Normalizer contract: Poll returns a snapshot of the in-region nodes heard
// recently. Per-packet signal metrics ride in NetworkTelemetry, which the
// store excludes from the content hash — so the advert firehose refreshes
// liveness without minting a revision.
type NetworkNormalizer struct {
	cfg          *config.Config
	registry     MeshRegistry
	activeWindow time.Duration
	// brokerOps maps a broker URL to its config (operator name + https page),
	// used to attribute a node's provenance to the MQTT server's operator.
	brokerOps map[string]config.MeshcoreBroker
}

// NewNetworkNormalizer wires the normalizer to a MeshCore registry.
func NewNetworkNormalizer(cfg *config.Config, registry MeshRegistry) *NetworkNormalizer {
	aw := cfg.Grid.Meshcore.ActiveWindow
	if aw <= 0 {
		aw = defaultMeshActiveWindow
	}
	ops := make(map[string]config.MeshcoreBroker, len(cfg.Grid.Meshcore.Brokers))
	for _, b := range cfg.Grid.Meshcore.Brokers {
		ops[b.URL] = b
	}
	return &NetworkNormalizer{cfg: cfg, registry: registry, activeWindow: aw, brokerOps: ops}
}

// SourceIDs implements Normalizer.
func (n *NetworkNormalizer) SourceIDs() []string { return []string{meshSourceID} }

// Poll implements Normalizer. It fails loud when no broker is connected: an
// empty snapshot from OUR outage must never look like "every node left the
// mesh" to the disappearance sweep. A hard Poll error skips the sweep entirely
// (scheduler invariant), so nodes stay ACTIVE until genuinely silent past the
// source's expireAfter.
func (n *NetworkNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	boxes := n.geofence()
	if len(boxes) == 0 {
		return nil, errEmptyScope("meshcore geofence")
	}
	connected, _ := n.registry.Health()
	if connected == 0 {
		return nil, fmt.Errorf("meshcore: no brokers connected; not asserting node disappearance")
	}

	nodes := n.registry.Snapshot(n.activeWindow)
	events := make([]*gridv1.Event, 0, len(nodes))
	for _, nd := range nodes {
		// Scope: any node with a location inside the geofence. Locationless nodes
		// can't be geofenced and are dropped.
		if !nd.HasLocation || !inAnyBounds(nd.Lat, nd.Lng, boxes) {
			continue
		}
		lat := quantizeCoord(nd.Lat)
		lng := quantizeCoord(nd.Lng)

		ev := NewEvent(
			meshSourceID+":"+nd.PubKey,
			gridv1.Layer_NETWORK,
			gridv1.Severity_INFO,
			gridv1.EventStatus_ACTIVE,
			meshHeadline(nd),
		)
		ev.Category = nd.Role
		ev.AreaLabel = nd.Name
		ev.CanonicalUrl = meshMapURL
		ev.Geometry = GeometryFromPoint(lat, lng)
		// observed_at is when this node's content was observed; it's zeroed in the
		// content hash, so it only persists on an actual identity/location change.
		// Mesh node clocks are frequently skewed (we've seen adverts stamped
		// months in the future), so the event's observed time — which the feed
		// orders and `since`-filters on — is OUR receive time, never the
		// node-reported advert timestamp. The node's stamp is kept only in
		// telemetry.lastAdvertAt (diagnostic; reveals the skew).
		ev.ObservedAt = tsProto(nd.LastHeardAt)
		ev.Provenance = n.meshProvenance(nd.Brokers)
		ev.Detail = &gridv1.Event_Network{Network: &gridv1.NetworkDetail{
			PublicKey: nd.PubKey,
			NodeType:  nd.Role,
			Name:      nd.Name,
			Telemetry: &gridv1.NetworkTelemetry{
				Snr:          nd.SNR,
				Rssi:         nd.RSSI,
				HopCount:     nd.HopCount,
				Path:         nd.Path,
				PathNodes:    nd.PathNodes,
				Gateways:     nd.Gateways,
				LastAdvertAt: tsProto(nd.LastAdvertAt),
			},
		}}
		events = append(events, ev)
	}

	// Drain the reception firehose accumulated since the last tick into the
	// append-only observation store (Tier 0). Unlike the presence events above,
	// observations are NOT geofenced here — a relay hop just outside the fence can
	// still connect two in-region nodes, and the raw path is retained for
	// re-resolution; the edge-build/projection step applies the located +
	// in-region filter. These are measurements, not events.
	obs := n.registry.DrainObservations()
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
	return &PollResult{Events: events, MeshObservations: storeObs}, nil
}

// meshProvenance attributes a node to the operator(s) of the MQTT broker(s) it
// was heard on: provenance source_url is the primary operator's https page (not
// the wss:// broker URL), and attribution names the operator(s). Falls back to
// the community map when a broker has no configured operator. The node's map
// deep-link stays on Event.canonical_url. Brokers are sorted, so a stable
// broker set yields a stable provenance (no per-poll revision churn).
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

// geofence returns the bboxes a node's location must fall within to be ingested.
// MeshCore presence is monitored over a deliberately WIDER area than the hazard
// region (config meshcore.bounds — e.g. to include the Bay Area for liveness
// confidence); when unset it falls back to the union of hazard-area bounds.
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
// to a short pubkey when a node advertises no name.
func meshHeadline(nd meshcore.NodeState) string {
	name := nd.Name
	if name == "" {
		short := nd.PubKey
		if len(short) > 8 {
			short = short[:8]
		}
		name = "node " + short
	}
	return fmt.Sprintf("%s (%s)", name, nd.Role)
}

func quantizeCoord(v float64) float64 {
	return math.Round(v*meshLocationDecimals) / meshLocationDecimals
}
