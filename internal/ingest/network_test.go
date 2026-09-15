package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/pushingest"
	"github.com/dpup/sierra-data/internal/store"
)

// fakeMeshRegistry is a canned MeshRegistry for the normalizer tests.
type fakeMeshRegistry struct {
	nodes     []meshcore.NodeState
	connected int
	obs       []meshcore.Observation
}

func (f *fakeMeshRegistry) Snapshot() []meshcore.NodeState            { return f.nodes }
func (f *fakeMeshRegistry) Health() (int, time.Time)                  { return f.connected, time.Time{} }
func (f *fakeMeshRegistry) DrainObservations() []meshcore.Observation { return f.obs }

// ResolvePrefix matches the production rule: a unique prefix match, or nothing.
func (f *fakeMeshRegistry) ResolvePrefix(prefix string) (string, bool) {
	var match string
	for _, n := range f.nodes {
		if !strings.HasPrefix(n.PubKey, prefix) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = n.PubKey
	}
	return match, match != ""
}

func TestNetworkPollBuildsEvents(t *testing.T) {
	reg := &fakeMeshRegistry{
		connected: 1,
		nodes: []meshcore.NodeState{
			{ // in-region repeater with location
				PubKey: "aa11bb22cc33", Role: meshcore.RoleRepeater, Name: "Murphys Ridge",
				HasLocation: true, Lat: 38.137412, Lng: -120.457934,
				SNR: 4.5, RSSI: -93, HopCount: 2,
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
	n := NewNetworkNormalizer(testConfig(), reg, nil)
	assert.Equal(t, []string{"meshcore"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "only the in-region located node")

	ev := res.Events[0]
	assert.Equal(t, "meshcore:aa11bb22cc33", ev.Id)
	assert.Equal(t, gridv1.Layer_MESH, ev.Layer)
	assert.Equal(t, gridv1.Severity_INFO, ev.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.Status)
	assert.Equal(t, "Murphys Ridge (repeater)", ev.Headline)
	assert.Equal(t, "repeater", ev.Category)
	// AreaLabel is a description of WHERE, and a mesh node has no such
	// description — only coordinates. It used to be set to the node's name,
	// which made the event claim a location it does not have.
	assert.Empty(t, ev.AreaLabel, "a mesh node has no area description")
	assert.Equal(t, "Murphys Ridge", ev.GetMesh().GetName(), "the name lives on the detail block")
	assert.Equal(t, meshMapURL, ev.CanonicalUrl)

	// Location is quantized to ~4dp (~11m) to damp jitter into the hash.
	require.NotNil(t, ev.Geometry)
	assert.InDelta(t, 38.1374, ev.Geometry.Centroid.Lat, 1e-9)
	assert.InDelta(t, -120.4579, ev.Geometry.Centroid.Lng, 1e-9)

	det := ev.GetMesh()
	require.NotNil(t, det)
	assert.Equal(t, "aa11bb22cc33", det.PublicKey)
	assert.Equal(t, "repeater", det.NodeType)
	require.NotNil(t, det.Telemetry)
	assert.InDelta(t, 4.5, det.Telemetry.GetSnr().GetValue(), 1e-9)
	assert.EqualValues(t, -93, det.Telemetry.GetRssi().GetValue())
	assert.EqualValues(t, 2, det.Telemetry.GetHopCount().GetValue())

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
	n := NewNetworkNormalizer(cfg, reg, nil)

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
	n := NewNetworkNormalizer(cfg, reg, nil)

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
	n := NewNetworkNormalizer(testConfig(), reg, nil)
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
	n := NewNetworkNormalizer(testConfig(), reg, nil)

	_, err := n.Poll(testCtx(), nil)
	require.Error(t, err, "an all-brokers-down poll must fail so the sweep is skipped")
}

func TestNetworkPollEmptyScope(t *testing.T) {
	cfg := testConfig()
	cfg.Hazards.Areas = nil
	n := NewNetworkNormalizer(cfg, &fakeMeshRegistry{connected: 1}, nil)
	_, err := n.Poll(testCtx(), nil)
	require.Error(t, err)
}

func TestNetworkHeadlineFallback(t *testing.T) {
	assert.Equal(t, "node abcdef12 (companion)",
		meshHeadline("", meshcore.RoleCompanion, "abcdef1234567890"))
}

// GPS noise is not movement.
//
// A node's self-reported fix wanders tens of metres between adverts while the
// node sits still, and geometry is hashed — so before this, every wobble was a
// revision. One stationary companion reached revision 75 that way, 18 distinct
// positions inside a 245 m circle, each snapshot recording that it had not moved.
func TestMeshPositionJitterDoesNotChurn(t *testing.T) {
	const lat, lng = 38.137412, -120.457934

	stored := func() *gridv1.Event {
		reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
			PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "Arnold Summit",
			HasLocation: true, Lat: lat, Lng: lng, LastHeardAt: pollNow.Add(-time.Minute),
		}}}
		n := pushNormalizer(t, reg, pushingest.MeshSnapshot{})
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		return res.Events[0]
	}()
	require.NotNil(t, stored.GetGeometry())
	baseHash := store.ContentHash(stored)

	// A later advert from the same stationary node, ~60 m away — inside the
	// noise floor. The stored geometry rides forward BYTE FOR BYTE, which is
	// what keeps the content hash equal.
	jittered := reheardAt(t, lat+0.0005, lng+0.0002, stored)
	assert.Equal(t, baseHash, store.ContentHash(jittered),
		"a wobble inside the epsilon must not mint a revision")
	assert.Equal(t, stored.GetGeometry().GetGeojson(), jittered.GetGeometry().GetGeojson())

	// A real move — ~700 m — updates, exactly once. A threshold that damped this
	// would be hiding the one thing a node's geometry is for.
	moved := reheardAt(t, lat+0.0063, lng, stored)
	assert.NotEqual(t, baseHash, store.ContentHash(moved), "a real move is a real revision")
	assert.NotEqual(t, stored.GetGeometry().GetGeojson(), moved.GetGeometry().GetGeojson())

	// A node with nothing stored takes the fix it was given: we damp change,
	// never the first sighting.
	fresh := reheardAt(t, lat, lng, nil)
	require.NotNil(t, fresh.GetGeometry())
}

// reheardAt polls one node at the given position, with `prev` (or nothing) in
// the store.
func reheardAt(t *testing.T, lat, lng float64, prev *gridv1.Event) *gridv1.Event {
	t.Helper()
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "Arnold Summit",
		HasLocation: true, Lat: lat, Lng: lng, LastHeardAt: pollNow,
	}}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{})
	p := &fakePrior{}
	if prev != nil {
		p.events = []*gridv1.Event{prev}
	}
	res, err := n.Poll(testCtx(), p)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	return res.Events[0]
}

// A restart must not re-attribute the whole mesh.
//
// The registry is rehydrated from the store on boot with everything about a node
// EXCEPT which broker heard it, so a seeded node's provenance used to fall back
// to the generic credit until its next advert — hours, for a 12-hour repeater —
// and flip back afterwards. Both fields are hashed, so that was two revisions
// per node per deploy: 505 attribution flips across 45 live nodes, 26 of them in
// a single minute.
func TestSeededNodeKeepsItsBrokerAttribution(t *testing.T) {
	cfg := testConfig()
	cfg.Grid.Meshcore.Brokers = []config.MeshcoreBroker{{
		URL: "wss://mqtt.gomesh.dev:443/mqtt", Operator: "LetsMesh",
		OperatorURL: "https://analyzer.letsmesh.net/about",
	}}
	node := meshcore.NodeState{
		PubKey: "aa11bb22", Role: meshcore.RoleRepeater, Name: "Ridge",
		HasLocation: true, Lat: 38.14, Lng: -120.45,
		Brokers: []string{"wss://mqtt.gomesh.dev:443/mqtt"},
	}

	// Heard live: the bridge operator is credited.
	heard := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{node}}
	res, err := NewNetworkNormalizer(cfg, heard, nil).Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	stored := res.Events[0]
	assert.Contains(t, stored.GetProvenance().GetAttribution(), "LetsMesh")
	baseHash := store.ContentHash(stored)

	// After a restart the same node is in the snapshot from the seed, with no
	// broker recorded yet. The credit rides forward, so the hash does not move.
	seeded := node
	seeded.Brokers = nil
	reboot := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{seeded}}
	res, err = NewNetworkNormalizer(cfg, reboot, nil).
		Poll(testCtx(), &fakePrior{events: []*gridv1.Event{stored}})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	after := res.Events[0]
	assert.Equal(t, stored.GetProvenance().GetAttribution(), after.GetProvenance().GetAttribution())
	assert.Equal(t, stored.GetProvenance().GetSourceUrl(), after.GetProvenance().GetSourceUrl())
	assert.Equal(t, baseHash, store.ContentHash(after), "a restart is not a change of source")
	// fetchedAt stays fresh: we DID observe the node, we just cannot say through
	// which bridge.
	assert.True(t, after.GetProvenance().GetFetchedAt().AsTime().
		After(stored.GetProvenance().GetFetchedAt().AsTime().Add(-time.Second)))

	// A node nobody has ever credited gets the generic attribution, not a
	// borrowed one.
	fresh := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{seeded}}
	res, err = NewNetworkNormalizer(cfg, fresh, nil).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.NotContains(t, res.Events[0].GetProvenance().GetAttribution(), "LetsMesh")
}
