package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
)

// fakeMeshRegistry is a canned MeshRegistry for the normalizer tests.
type fakeMeshRegistry struct {
	nodes     []meshcore.NodeState
	connected int
	obs       []meshcore.Observation
}

func (f *fakeMeshRegistry) Snapshot(time.Duration) []meshcore.NodeState { return f.nodes }
func (f *fakeMeshRegistry) Health() (int, time.Time)                    { return f.connected, time.Time{} }
func (f *fakeMeshRegistry) DrainObservations() []meshcore.Observation   { return f.obs }

func TestNetworkPollBuildsEvents(t *testing.T) {
	reg := &fakeMeshRegistry{
		connected: 1,
		nodes: []meshcore.NodeState{
			{ // in-region repeater with location
				PubKey: "aa11bb22cc33", Role: meshcore.RoleRepeater, Name: "Murphys Ridge",
				HasLocation: true, Lat: 38.137412, Lng: -120.457934,
				SNR: 4.5, RSSI: -93, HopCount: 2, Path: []string{"c2", "e2"},
				Gateways:     []string{"ag loft rpt"},
				LastAdvertAt: time.Unix(1_782_400_000, 0).UTC(),            // node clock (skewed/untrusted)
				LastHeardAt:  time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC), // our receive time
			},
			{ // out-of-region: dropped
				PubKey: "dd44", Role: meshcore.RoleRepeater, Name: "Reno", HasLocation: true, Lat: 39.5, Lng: -119.8,
			},
			{ // locationless: dropped
				PubKey: "ee55", Role: meshcore.RoleCompanion, Name: "Handheld", HasLocation: false,
			},
		},
	}
	n := NewNetworkNormalizer(testConfig(), reg)
	assert.Equal(t, []string{"meshcore"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "only the in-region located node")

	ev := res.Events[0]
	assert.Equal(t, "meshcore:aa11bb22cc33", ev.Id)
	assert.Equal(t, gridv1.Layer_NETWORK, ev.Layer)
	assert.Equal(t, gridv1.Severity_INFO, ev.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.Status)
	assert.Equal(t, "Murphys Ridge (repeater)", ev.Headline)
	assert.Equal(t, "repeater", ev.Category)
	assert.Equal(t, "Murphys Ridge", ev.AreaLabel)
	assert.Equal(t, meshMapURL, ev.CanonicalUrl)

	// Location is quantized to ~4dp (~11m) to damp jitter into the hash.
	require.NotNil(t, ev.Geometry)
	assert.InDelta(t, 38.1374, ev.Geometry.Centroid.Lat, 1e-9)
	assert.InDelta(t, -120.4579, ev.Geometry.Centroid.Lng, 1e-9)

	det := ev.GetNetwork()
	require.NotNil(t, det)
	assert.Equal(t, "aa11bb22cc33", det.PublicKey)
	assert.Equal(t, "repeater", det.NodeType)
	require.NotNil(t, det.Telemetry)
	assert.InDelta(t, 4.5, det.Telemetry.Snr, 1e-9)
	assert.EqualValues(t, -93, det.Telemetry.Rssi)
	assert.EqualValues(t, 2, det.Telemetry.HopCount)
	assert.Equal(t, []string{"c2", "e2"}, det.Telemetry.Path)

	// Event's observed time is OUR receive time (LastHeardAt), never the node's
	// skewed clock — the feed orders/`since`-filters on this.
	assert.True(t, ev.GetObservedAt().AsTime().Equal(time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)),
		"observed_at should be our receive time, got %s", ev.GetObservedAt().AsTime())
	// The node's stamp survives only in telemetry (diagnostic).
	assert.True(t, det.Telemetry.LastAdvertAt.AsTime().Equal(time.Unix(1_782_400_000, 0).UTC()))
	assert.Equal(t, []string{"ag loft rpt"}, det.Telemetry.Gateways)
}

func TestNetworkPollWiderBoundsOverrideHazardAreas(t *testing.T) {
	cfg := testConfig()
	// Explicit meshcore geofence covering the Bay Area → Sierra, wider than the
	// hazard region. A San Francisco node is outside hazards.areas but inside this.
	cfg.Grid.Meshcore.Bounds = []config.GeoBounds{
		{MinLatitude: 36.0, MaxLatitude: 39.0, MinLongitude: -123.0, MaxLongitude: -119.5},
	}
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{
		{PubKey: "bay01", Role: meshcore.RoleRepeater, Name: "SF Node",
			HasLocation: true, Lat: 37.7888, Lng: -122.4188}, // Bay Area: in wider box, out of hazard area
		{PubKey: "far01", Role: meshcore.RoleRepeater, Name: "Reno",
			HasLocation: true, Lat: 39.5, Lng: -119.0}, // outside even the wider box
	}}
	n := NewNetworkNormalizer(cfg, reg)

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "the SF node is included by the wider meshcore bounds; Reno stays out")
	assert.Equal(t, "meshcore:bay01", res.Events[0].Id)
}

func TestNetworkProvenanceAttributesBrokerOperator(t *testing.T) {
	cfg := testConfig()
	cfg.Grid.Meshcore.Brokers = []config.MeshcoreBroker{{
		URL: "wss://mqtt.gomesh.dev:443/mqtt", Operator: "LetsMesh",
		OperatorURL: "https://analyzer.letsmesh.net/about",
	}}
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: "aa11bb22", Role: meshcore.RoleRepeater, Name: "Ridge",
		HasLocation: true, Lat: 38.14, Lng: -120.45,
		Brokers: []string{"wss://mqtt.gomesh.dev:443/mqtt"},
	}}}
	n := NewNetworkNormalizer(cfg, reg)

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := res.Events[0]

	prov := ev.GetProvenance()
	// provenance points at the operator's https page (not the wss:// broker URL)…
	assert.Equal(t, "https://analyzer.letsmesh.net/about", prov.GetSourceUrl())
	assert.Equal(t, "MeshCore community mesh via LetsMesh", prov.GetAttribution())
	// …while the node's map deep-link stays on canonical_url.
	assert.Equal(t, meshMapURL, ev.GetCanonicalUrl())
}

func TestNetworkProvenanceFallsBackWithoutOperator(t *testing.T) {
	// A node heard on a broker with no configured operator falls back to the map.
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: "cc33", Role: meshcore.RoleRepeater, HasLocation: true, Lat: 38.14, Lng: -120.45,
		Brokers: []string{"wss://unknown-broker"},
	}}}
	n := NewNetworkNormalizer(testConfig(), reg)
	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, meshMapURL, res.Events[0].GetProvenance().GetSourceUrl())
	assert.Equal(t, "MeshCore community mesh", res.Events[0].GetProvenance().GetAttribution())
}

func TestNetworkPollHardErrorsWhenNoBrokers(t *testing.T) {
	reg := &fakeMeshRegistry{connected: 0, nodes: []meshcore.NodeState{
		{PubKey: "aa", Role: meshcore.RoleRepeater, HasLocation: true, Lat: 38.1, Lng: -120.4},
	}}
	n := NewNetworkNormalizer(testConfig(), reg)

	_, err := n.Poll(testCtx(), nil)
	require.Error(t, err, "an all-brokers-down poll must fail so the sweep is skipped")
}

func TestNetworkPollEmptyScope(t *testing.T) {
	cfg := testConfig()
	cfg.Hazards.Areas = nil
	n := NewNetworkNormalizer(cfg, &fakeMeshRegistry{connected: 1})
	_, err := n.Poll(testCtx(), nil)
	require.Error(t, err)
}

func TestNetworkHeadlineFallback(t *testing.T) {
	nd := meshcore.NodeState{PubKey: "abcdef1234567890", Role: meshcore.RoleCompanion}
	assert.Equal(t, "node abcdef12 (companion)", meshHeadline(nd))
}
