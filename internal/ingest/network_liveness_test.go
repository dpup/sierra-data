package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/pushingest"
)

// expiredNode is the stored, EXPIRED event of a repeater a signed advert once
// placed inside the geofence. Built through a real tick so its geometry is
// whatever the normalizer writes.
func expiredNode(t *testing.T, lat, lng float64) *gridv1.Event {
	t.Helper()
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "BA1D Wildwood",
		HasLocation: true, Lat: lat, Lng: lng, LastHeardAt: pollNow.Add(-80 * time.Hour),
	}}}
	res, err := pushNormalizer(t, reg, pushingest.MeshSnapshot{}).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	ev := proto.Clone(res.Events[0]).(*gridv1.Event)
	ev.Status = gridv1.EventStatus_EXPIRED
	return ev
}

func catalogOf(evs ...*gridv1.Event) MeshCatalog {
	return func(context.Context) ([]*gridv1.Event, error) { return evs, nil }
}

// A repeater that adverts every few days but relays all day must not read as
// gone. CoreScope showed 204 such nodes on 2026-11-03 that we had EXPIRED, some
// relaying thousands of packets a day: relay credit could not reach them once
// the sweep expired them, because only ACTIVE events were candidates.
func TestRelayRevivesAnExpiredNode(t *testing.T) {
	stored := expiredNode(t, 38.2, -120.3)
	reg := &fakeMeshRegistry{connected: 1, relays: map[string]time.Time{fullKey: pollNow.Add(-time.Minute)}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{}).WithCatalog(catalogOf(stored))

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Contains(t, reg.knownSeen, meshcore.KnownNode{PubKey: fullKey, Role: meshcore.RoleRepeater},
		"an expired node is a resolution candidate, or its hops never resolve")
	require.Len(t, res.Events, 1)
	ev := res.Events[0]
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, "BA1D Wildwood", ev.GetMesh().GetName(), "identity is the stored advert's")
	assert.True(t, proto.Equal(stored.GetGeometry(), ev.GetGeometry()), "and so is the position")
	assert.Equal(t, pollNow.Add(-time.Minute), ev.GetObservedAt().AsTime())

	// The credit outlives the drain that produced it: no new relay this tick.
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Len(t, res.Events, 1, "present for relayPresence after the relay, not one tick")

	// ...and ends with the window.
	n.now = func() time.Time { return pollNow.Add(testConfig().Grid.Meshcore.RelayPresenceWindow()) }
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events)
}

func TestRelayDoesNotReviveOutsideTheGeofence(t *testing.T) {
	stored := expiredNode(t, 38.2, -120.3)
	stored.Geometry = expiredNode(t, 38.2, -120.3).GetGeometry()
	stored.GetGeometry().GetCentroid().Lat = 45.0
	reg := &fakeMeshRegistry{connected: 1, relays: map[string]time.Time{fullKey: pollNow}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{}).WithCatalog(catalogOf(stored))
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events)
}

func TestRelayCreditsAnUnknownKeyNothing(t *testing.T) {
	reg := &fakeMeshRegistry{connected: 1, relays: map[string]time.Time{fullKey: pollNow}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{}).WithCatalog(catalogOf())
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events, "a relay never introduces a node, it only extends one an advert established")
}
