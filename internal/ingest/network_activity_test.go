package ingest

import (
	"strings"
	"testing"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func activityByKey(acts []store.MeshActivity) map[string]store.MeshActivity {
	out := make(map[string]store.MeshActivity, len(acts))
	for _, a := range acts {
		out[a.PubKey] = a
	}
	return out
}

// The activity clocks are what make a quiet repeater look as alive as it is:
// last_heard_at from a REAL advert reception, last_relayed_at from other
// nodes' traffic, for every node the layer covers and nothing else.
func TestPollEmitsMeshActivity(t *testing.T) {
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	heard := strings.Repeat("a1", 32)  // in the snapshot, advert on record
	seeded := strings.Repeat("b2", 32) // in the snapshot on a seed fallback only
	quiet := strings.Repeat("c3", 32)  // stored event, out of the snapshot, relaying
	bayArea := strings.Repeat("d4", 32)

	reg := &fakeMeshRegistry{
		connected: 1,
		nodes: []meshcore.NodeState{
			{PubKey: heard, Role: meshcore.RoleRepeater, Name: "Camp Connell", HasLocation: true,
				Lat: 38.137, Lng: -120.457, LastHeardAt: at.Add(-time.Hour), ReceivedAt: at.Add(-time.Hour)},
			{PubKey: seeded, Role: meshcore.RoleRepeater, Name: "Seeded", HasLocation: true,
				Lat: 38.138, Lng: -120.458, LastHeardAt: at.Add(-2 * time.Hour)},
		},
		relays: map[string]time.Time{
			heard:   at.Add(-time.Minute),
			quiet:   at.Add(-2 * time.Minute),
			bayArea: at, // resolved, but not a node this layer covers
		},
	}
	n := NewNetworkNormalizer(testConfig(), reg, nil)
	prior := &fakePrior{events: []*gridv1.Event{storedMeshEvent(quiet, nil)}}

	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Equal(t, []string{quiet}, reg.knownSeen,
		"the store's keys are offered for resolution, so a pruned repeater still resolves")

	got := activityByKey(res.MeshActivity)
	assert.Equal(t, map[string]store.MeshActivity{
		heard: {PubKey: heard, LastHeardAt: at.Add(-time.Hour), LastRelayedAt: at.Add(-time.Minute)},
		quiet: {PubKey: quiet, LastRelayedAt: at.Add(-2 * time.Minute)},
	}, got, "no row for the seed fallback (not a reception) or the uncovered node")

	// Nothing moved: nothing to write.
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Empty(t, res.MeshActivity, "an unchanged tick writes no activity")

	// One clock moves: only that node is written, carrying both clocks.
	reg.relays = map[string]time.Time{heard: at}
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Equal(t, []store.MeshActivity{{PubKey: heard, LastHeardAt: at.Add(-time.Hour), LastRelayedAt: at}}, res.MeshActivity)

	// An OLDER relay (a late drain) never moves the clock back.
	reg.relays = map[string]time.Time{heard: at.Add(-time.Hour)}
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Empty(t, res.MeshActivity)
}

// On a hard error the relay evidence is not drained: it waits for a tick
// that can write it.
func TestPollKeepsRelaysOnHardError(t *testing.T) {
	key := strings.Repeat("a1", 32)
	reg := &fakeMeshRegistry{connected: 0, down: 1, relays: map[string]time.Time{key: time.Now()}}
	n := NewNetworkNormalizer(testConfig(), reg, nil)
	_, err := n.Poll(testCtx(), &fakePrior{})
	require.Error(t, err)
	assert.NotNil(t, reg.relays, "not drained")
}

// A node in the snapshot without a reading of its own (seeded at boot, or
// kept present by relays) must not publish 0 dB / 0 hops. Until 2026-11 the
// first tick after every restart wrote those zeros over the stored reading.
func TestUnheardNodeCarriesStoredSignal(t *testing.T) {
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	key := strings.Repeat("a2", 32)
	node := meshcore.NodeState{
		PubKey: key, Role: meshcore.RoleRepeater, Name: "SIERRA Lilac Park",
		HasLocation: true, Lat: 38.25, Lng: -120.34,
		LastHeardAt: at.Add(-20 * time.Hour), LastRelayedAt: at.Add(-time.Minute),
		// HasSignal false: SNR/RSSI/HopCount are zero values, not a reading.
	}
	stored := func(tel *gridv1.MeshTelemetry) *fakePrior {
		ev := storedMeshEvent(key, nil)
		ev.GetMesh().Telemetry = tel
		return &fakePrior{events: []*gridv1.Event{ev}}
	}
	poll := func(prior Prior) *gridv1.Event {
		n := NewNetworkNormalizer(testConfig(), &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{node}}, nil)
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		require.Len(t, res.Events, 1)
		return res.Events[0]
	}

	t.Run("the stored reading rides forward", func(t *testing.T) {
		ev := poll(stored(&gridv1.MeshTelemetry{
			Snr: wrapperspb.Double(6.5), Rssi: wrapperspb.Int32(-101), HopCount: wrapperspb.UInt32(3),
			Gateways: []string{"gw"}, LastAdvertAt: timestamppb.New(at.Add(-20 * time.Hour)),
		}))
		tel := ev.GetMesh().GetTelemetry()
		assert.InDelta(t, 6.5, tel.GetSnr().GetValue(), 1e-9)
		assert.EqualValues(t, 3, tel.GetHopCount().GetValue())
		assert.Equal(t, []string{"gw"}, tel.GetGateways())
		assert.Equal(t, at.Add(-time.Minute), ev.GetObservedAt().AsTime(), "observed is the latest evidence: the relay")
	})

	t.Run("a zeroed block with no advert stamp is the old bug, and is dropped", func(t *testing.T) {
		ev := poll(stored(&gridv1.MeshTelemetry{
			Snr: wrapperspb.Double(0), Rssi: wrapperspb.Int32(0), HopCount: wrapperspb.UInt32(0),
		}))
		tel := ev.GetMesh().GetTelemetry()
		assert.Nil(t, tel.GetSnr())
		assert.Nil(t, tel.GetHopCount(), "not 0 hops: never heard direct")
	})

	t.Run("nothing stored, nothing claimed", func(t *testing.T) {
		tel := poll(&fakePrior{}).GetMesh().GetTelemetry()
		assert.Nil(t, tel.GetSnr())
		assert.Nil(t, tel.GetRssi())
		assert.Nil(t, tel.GetHopCount())
	})

	t.Run("a live reading always wins", func(t *testing.T) {
		live := node
		live.HasSignal, live.SNR, live.HopCount = true, 9, 1
		n := NewNetworkNormalizer(testConfig(), &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{live}}, nil)
		res, err := n.Poll(testCtx(), stored(&gridv1.MeshTelemetry{
			Snr: wrapperspb.Double(6.5), LastAdvertAt: timestamppb.New(at.Add(-20 * time.Hour)),
		}))
		require.NoError(t, err)
		assert.InDelta(t, 9, res.Events[0].GetMesh().GetTelemetry().GetSnr().GetValue(), 1e-9)
	})
}
