package gridapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dpup/prefab/plugins/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// seedMeshNode stores one located MESH event inside calaveras whose blob
// carries an OLD observed_at and no activity, the shape a quiet repeater has
// in production.
func seedMeshNode(t *testing.T, s *Service, pk string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
		{ID: "meshcore", Name: "MeshCore Mesh", PollInterval: time.Minute},
	}))
	require.NoError(t, s.Store.RecordAttempt(ctx, "meshcore", nil))
	_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
		Id: "meshcore:" + pk, Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: gridv1.EventStatus_ACTIVE, Headline: "SIERRA Lilac Park (repeater)",
		Geometry:   pointGeom(38.1, -120.5),
		ObservedAt: timestamppb.New(base.Add(-72 * time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "meshcore", FetchedAt: timestamppb.New(base)},
		Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
			PublicKey: pk, NodeType: "repeater", Name: "SIERRA Lilac Park",
			Telemetry: &gridv1.MeshTelemetry{Gateways: []string{"gw"}},
		}},
	})
	require.NoError(t, err)
}

// rpcBG is a context an RPC's ETag guard can set headers on.
func rpcBG() context.Context {
	ctx, _ := rpcCtx()
	return ctx
}

func recordActivity(t *testing.T, s *Service, a store.MeshActivity) {
	t.Helper()
	require.NoError(t, s.Store.RecordMeshTick(context.Background(), nil, []store.MeshActivity{a}))
}

// The fix for "the mesh looks days old": every read of a current mesh event
// carries the activity clocks, from outside the blob.
func TestMeshActivityOverlaidOnEventReads(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	pk := "a2d649457de4481a"
	seedMeshNode(t, s, pk)
	heard, relayed := base.Add(-6*time.Hour), base.Add(-time.Minute)
	recordActivity(t, s, store.MeshActivity{PubKey: pk, LastHeardAt: heard, LastRelayedAt: relayed})

	list, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Layer: []string{"mesh"}})
	require.NoError(t, err)
	require.Len(t, list.GetEvents(), 1)
	tel := list.GetEvents()[0].GetMesh().GetTelemetry()
	assert.Equal(t, heard, tel.GetLastHeardAt().AsTime())
	assert.Equal(t, relayed, tel.GetLastRelayedAt().AsTime())
	assert.Equal(t, []string{"gw"}, tel.GetGateways(), "the stored telemetry is kept, not replaced")

	// Unfiltered lists overlay too; other layers are untouched.
	all, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{})
	require.NoError(t, err)
	for _, ev := range all.GetEvents() {
		if ev.GetLayer() == gridv1.Layer_MESH {
			assert.NotNil(t, ev.GetMesh().GetTelemetry().GetLastRelayedAt())
		}
	}

	ev, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "meshcore:" + pk})
	require.NoError(t, err)
	assert.Equal(t, relayed, ev.GetMesh().GetTelemetry().GetLastRelayedAt().AsTime())

	// Revision history is what was stored at the time: no overlay.
	hist, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "meshcore:" + pk})
	require.NoError(t, err)
	require.NotEmpty(t, hist.GetRevisions())
	assert.Nil(t, hist.GetRevisions()[0].GetEvent().GetMesh().GetTelemetry().GetLastRelayedAt())
}

func TestMeshActivityAbsentStaysNull(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	seedMeshNode(t, s, "a2d649457de4481a")
	ev, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "meshcore:a2d649457de4481a"})
	require.NoError(t, err)
	assert.Nil(t, ev.GetMesh().GetTelemetry().GetLastHeardAt(), "no activity on record is unknown, not a time")
	assert.Nil(t, ev.GetMesh().GetTelemetry().GetLastRelayedAt())
}

// The activity clocks move without a revision, so a validator that ignored
// them would answer 304 with a stale "last relayed".
func TestMeshActivityInvalidatesValidators(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	pk := "a2d649457de4481a"
	seedMeshNode(t, s, pk)

	tagOf := func(call func(ctx context.Context) error) string {
		ctx, fs := rpcCtx()
		require.NoError(t, call(ctx))
		tags := fs.hdr.Get("grpc-metadata-etag")
		require.Len(t, tags, 1)
		return tags[0]
	}
	notModified := func(tag string, call func(ctx context.Context) error) bool {
		ctx, _ := rpcCtx()
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("if-none-match", tag))
		err := call(ctx)
		if err == nil {
			return false
		}
		require.ErrorIs(t, err, etag.ErrNotModified)
		return true
	}
	listMesh := func(ctx context.Context) error {
		_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{Layer: []string{"mesh"}})
		return err
	}
	listQuakes := func(ctx context.Context) error {
		_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{Layer: []string{"earthquake"}})
		return err
	}
	getNode := func(ctx context.Context) error {
		_, err := g.GetEvent(ctx, &gridv1.GetEventRequest{Id: "meshcore:" + pk})
		return err
	}

	meshTag, quakeTag, nodeTag := tagOf(listMesh), tagOf(listQuakes), tagOf(getNode)
	require.True(t, notModified(meshTag, listMesh))
	require.True(t, notModified(nodeTag, getNode))

	recordActivity(t, s, store.MeshActivity{PubKey: pk, LastRelayedAt: base})

	assert.False(t, notModified(meshTag, listMesh), "a mesh list revalidates after activity moves")
	assert.False(t, notModified(nodeTag, getNode), "a node's detail revalidates after its activity moves")
	assert.True(t, notModified(quakeTag, listQuakes), "a list that cannot hold mesh events keeps its 304")
}

func TestMeshActivityOnMapLayers(t *testing.T) {
	s := newTestService(t)
	pk := "a2d649457de4481a"
	seedMeshNode(t, s, pk)
	heard, relayed := base.Add(-6*time.Hour), base.Add(-time.Minute)
	recordActivity(t, s, store.MeshActivity{PubKey: pk, LastHeardAt: heard, LastRelayedAt: relayed})

	// The link layer draws a node only at the end of a link, so give it one.
	ctx := context.Background()
	_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
		Id: "meshcore:bb", Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: gridv1.EventStatus_ACTIVE, Headline: "neighbour", Geometry: pointGeom(38.2, -120.4),
		Provenance: &gridv1.Provenance{SourceId: "meshcore", FetchedAt: timestamppb.New(base)},
		Detail:     &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: "bb", NodeType: "repeater"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.Store.InsertMeshObservations(ctx, []store.MeshObservation{
		{PubKey: pk, HeardAt: base, PathNodes: []string{"bb"}},
	}))
	_, err = s.Store.CompactMeshObservations(ctx)
	require.NoError(t, err)

	for _, layer := range []string{"mesh_node", "mesh_link"} {
		t.Run(layer, func(t *testing.T) {
			rec := get(t, s, "/v1/places/calaveras/map/"+layer+".geojson")
			require.Equal(t, http.StatusOK, rec.Code)
			var fc hazards.FeatureCollection
			decode(t, rec, &fc)
			var found bool
			for _, f := range fc.Features {
				if m := f.Properties.Mesh; m != nil && m.PublicKey == pk {
					found = true
					assert.Equal(t, heard.Format(time.RFC3339), m.LastHeardAt)
					assert.Equal(t, relayed.Format(time.RFC3339), m.LastRelayedAt)
					assert.Equal(t, base.Add(-72*time.Hour).Format(time.RFC3339), f.Properties.UpdatedAt,
						"updatedAt stays the last revision; the new props say when it talked")
				}
			}
			assert.True(t, found)
		})
	}
}
