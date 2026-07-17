package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
)

// fakeMeshRegistry is a canned MeshRegistry for the normalizer tests.
type fakeMeshRegistry struct {
	nodes     []meshcore.NodeState
	connected int
}

func (f *fakeMeshRegistry) Snapshot(time.Duration) []meshcore.NodeState { return f.nodes }
func (f *fakeMeshRegistry) Health() (int, time.Time)                    { return f.connected, time.Time{} }

func TestNetworkPollBuildsEvents(t *testing.T) {
	reg := &fakeMeshRegistry{
		connected: 1,
		nodes: []meshcore.NodeState{
			{ // in-region repeater with location
				PubKey: "aa11bb22cc33", Role: meshcore.RoleRepeater, Name: "Murphys Ridge",
				HasLocation: true, Lat: 38.137412, Lng: -120.457934,
				SNR: 4.5, RSSI: -93, HopCount: 2, Path: []string{"C2", "E2"},
				Gateways: []string{"ag loft rpt"}, LastAdvertAt: time.Unix(1_782_400_000, 0).UTC(),
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
	assert.Equal(t, []string{"C2", "E2"}, det.Telemetry.Path)
	assert.Equal(t, []string{"ag loft rpt"}, det.Telemetry.Gateways)
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
