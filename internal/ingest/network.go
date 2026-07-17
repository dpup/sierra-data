package ingest

import (
	"context"
	"fmt"
	"math"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
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
}

// NewNetworkNormalizer wires the normalizer to a MeshCore registry.
func NewNetworkNormalizer(cfg *config.Config, registry MeshRegistry) *NetworkNormalizer {
	aw := cfg.Grid.Meshcore.ActiveWindow
	if aw <= 0 {
		aw = defaultMeshActiveWindow
	}
	return &NetworkNormalizer{cfg: cfg, registry: registry, activeWindow: aw}
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
		ev.ObservedAt = tsProto(nd.LastAdvertAt)
		ev.Provenance = NewProvenance(meshSourceID, meshSourceName, meshAttribution, meshMapURL)
		ev.Detail = &gridv1.Event_Network{Network: &gridv1.NetworkDetail{
			PublicKey: nd.PubKey,
			NodeType:  nd.Role,
			Name:      nd.Name,
			Telemetry: &gridv1.NetworkTelemetry{
				Snr:          nd.SNR,
				Rssi:         nd.RSSI,
				HopCount:     nd.HopCount,
				Path:         nd.Path,
				Gateways:     nd.Gateways,
				LastAdvertAt: tsProto(nd.LastAdvertAt),
			},
		}}
		events = append(events, ev)
	}
	return &PollResult{Events: events}, nil
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
