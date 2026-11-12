package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// UpsertEvent is one transaction per event and, from a Fargate task, every
// statement in it is a network round trip. The hash-equal path is the most
// frequent one (every reconcile after a restart, every mesh telemetry
// refresh), so its statement count is pinned: begin, the lease fence, ONE
// lookup that also carries the stored blob and place set when the hash
// matches, commit; plus the three writes of a refresh when something
// hash-excluded changed.
func TestUpsertEventHashEqualStatementCount(t *testing.T) {
	ctx := context.Background()
	rec := &statementRecorder{}
	s := newTestStore(t, WithQueryTracer(rec))
	seedSource(t, s, "usgs")

	ev := testEvent("usgs:rt", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	_, err = s.UpsertEvent(ctx, ev) // warm: the place geometry cache is per term
	require.NoError(t, err)

	rec.take()
	res, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	assert.False(t, res.Changed)
	assert.Len(t, rec.take(), 4, "hash-equal, nothing to refresh: begin, fence, lookup, commit")

	// A hash-excluded change (mesh telemetry) is refreshed in place.
	mesh := testEvent("meshcore:rt", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "node")
	mesh.Layer = gridv1.Layer_MESH
	mesh.Provenance.SourceId = "usgs"
	mesh.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{Telemetry: &gridv1.MeshTelemetry{Snr: wrapperspb.Double(1)}}}
	_, err = s.UpsertEvent(ctx, mesh)
	require.NoError(t, err)
	mesh.GetMesh().GetTelemetry().Snr = wrapperspb.Double(2)
	rec.take()
	res, err = s.UpsertEvent(ctx, mesh)
	require.NoError(t, err)
	assert.False(t, res.Changed)
	assert.Len(t, rec.take(), 7, "refresh: begin, fence, lookup, lite copy, update, version bump, commit")
	got, err := s.GetEvent(ctx, "meshcore:rt")
	require.NoError(t, err)
	assert.Equal(t, 2.0, got.GetMesh().GetTelemetry().GetSnr().GetValue(), "the refresh landed")
}
