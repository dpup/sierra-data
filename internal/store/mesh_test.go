package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertMeshObservations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	obs := []MeshObservation{
		{
			PubKey: "abcd", HeardAt: time.Unix(1_700_000_000, 0), Broker: "wss://b", Gateway: "gw1",
			SNR: 4.5, RSSI: -93, HopCount: 2,
			Path: []string{"c2", "e2"}, PathNodes: []string{"abcd", ""},
		},
		{PubKey: "ef01", HeardAt: time.Unix(1_700_000_060, 0), Gateway: "gw2"},
	}
	require.NoError(t, s.InsertMeshObservations(ctx, obs))

	n, err := s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Path/PathNodes are stored comma-joined, parallel — an unresolved hop stays a
	// blank slot ("abcd," has a trailing empty), and HeardAt is our-clock seconds.
	var heard int64
	var path, pathNodes string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT heard_at, path, path_nodes FROM mesh_observations WHERE pubkey = 'abcd'`).
		Scan(&heard, &path, &pathNodes))
	assert.Equal(t, int64(1_700_000_000), heard)
	assert.Equal(t, "c2,e2", path)
	assert.Equal(t, "abcd,", pathNodes)

	// An empty batch is a no-op (not an error), leaving the count unchanged.
	require.NoError(t, s.InsertMeshObservations(ctx, nil))
	n, err = s.CountMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}
