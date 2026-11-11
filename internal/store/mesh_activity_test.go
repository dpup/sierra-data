package store

import (
	"context"
	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeshActivityMergesByMax(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	v0 := meshActivityVersion(t, s)

	require.NoError(t, s.RecordMeshTick(ctx, nil, []MeshActivity{
		{PubKey: "AB07CD", LastHeardAt: base, LastRelayedAt: base.Add(time.Minute)},
		{PubKey: "2cfe00", LastRelayedAt: base},
	}))
	assert.Greater(t, meshActivityVersion(t, s), v0, "a write moves the validator")

	// Older times and zeros never move a clock backwards; newer ones advance it.
	require.NoError(t, s.RecordMeshTick(ctx, nil, []MeshActivity{
		{PubKey: "ab07cd", LastHeardAt: base.Add(-time.Hour), LastRelayedAt: base.Add(5 * time.Minute)},
		{PubKey: "2cfe00", LastHeardAt: base.Add(2 * time.Minute)},
	}))

	got, err := s.MeshActivityFor(ctx, []string{"AB07CD", "2cfe00", "ffff00", "ab07cd"})
	require.NoError(t, err)
	assert.Equal(t, map[string]MeshActivity{
		"ab07cd": {PubKey: "ab07cd", LastHeardAt: base, LastRelayedAt: base.Add(5 * time.Minute)},
		"2cfe00": {PubKey: "2cfe00", LastHeardAt: base.Add(2 * time.Minute), LastRelayedAt: base},
	}, got, "keys are case-insensitive; unknown keys are absent, not zero rows")

	empty, err := s.MeshActivityFor(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestRecordMeshTickWritesBothInOneCall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	v0 := meshActivityVersion(t, s)

	require.NoError(t, s.RecordMeshTick(ctx, nil, nil), "an empty tick is a no-op")
	assert.Equal(t, v0, meshActivityVersion(t, s))

	require.NoError(t, s.RecordMeshTick(ctx,
		[]MeshObservation{{PubKey: "ab07cd", HeardAt: at, Gateway: "gw", Path: []string{"2cfe"}, PathNodes: []string{""}}},
		[]MeshActivity{{PubKey: "ab07cd", LastHeardAt: at}}))
	n, err := s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, err := s.MeshActivityFor(ctx, []string{"ab07cd"})
	require.NoError(t, err)
	assert.Equal(t, at, got["ab07cd"].LastHeardAt)

	// The old entry point still writes observations and leaves activity alone.
	v1 := meshActivityVersion(t, s)
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{{PubKey: "ab07cd", HeardAt: at.Add(time.Minute)}}))
	assert.Equal(t, v1, meshActivityVersion(t, s))
}

// The boot seed re-resolves stored paths itself, so it gets hops, never the
// PathNodes some older resolver wrote.
func TestMeshObservationPaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	require.NoError(t, s.InsertMeshObservations(ctx, []MeshObservation{
		{PubKey: "old", HeardAt: at.Add(-time.Hour), Path: []string{"aa"}},
		{PubKey: "direct", HeardAt: at.Add(time.Minute)},
		{PubKey: "ab07cd", HeardAt: at, Broker: "wss://b", Gateway: "gw",
			Path: []string{"2c", "fe"}, PathNodes: []string{"", "fe99"}},
	}))
	got, err := s.MeshObservationPaths(ctx, at)
	require.NoError(t, err)
	assert.Equal(t, []MeshObservation{{PubKey: "ab07cd", HeardAt: at, Broker: "wss://b", Gateway: "gw",
		Path: []string{"2c", "fe"}}}, got)
}

func TestEventsBySourceAndStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSource(t, s, "meshcore")
	for _, id := range []string{"meshcore:a", "meshcore:b"} {
		ev := &gridv1.Event{Id: id, Layer: gridv1.Layer_MESH, Status: gridv1.EventStatus_ACTIVE,
			Provenance: &gridv1.Provenance{SourceId: "meshcore"}}
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	require.NoError(t, s.TransitionEvents(ctx, []string{"meshcore:b"}, gridv1.EventStatus_EXPIRED, time.Now()))
	got, err := s.EventsBySourceAndStatus(ctx, "meshcore", gridv1.EventStatus_EXPIRED)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "meshcore:b", got[0].GetId())
}
