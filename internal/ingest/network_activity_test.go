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
	assert.ElementsMatch(t, []string{heard, seeded, quiet}, reg.knownSeen,
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
