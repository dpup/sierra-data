package gridapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// meshLinksAt issues GET /api/v1/mesh/links?window=… against the hand-built
// handler and decodes the response.
func meshLinksAt(t *testing.T, s *Service, window string) meshLinksResponse {
	t.Helper()
	u := "/api/v1/mesh/links"
	if window != "" {
		u += "?window=" + window
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	s.serveMeshLinks(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out meshLinksResponse
	decode(t, rec, &out)
	return out
}

func TestServeMeshLinks(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	// Two receptions of node aa relayed via bb, ~2 days ago; compact into the
	// rollup so the endpoint reads real topology. base is the service clock "now".
	heard := base.Add(-48 * time.Hour)
	require.NoError(t, s.Store.InsertMeshObservations(ctx, []store.MeshObservation{
		{PubKey: "aa", HeardAt: heard, SNR: -6, PathNodes: []string{"bb"}},
		{PubKey: "aa", HeardAt: heard.Add(time.Minute), SNR: -4, PathNodes: []string{"bb"}},
	}))
	_, err := s.Store.CompactMeshObservations(ctx)
	require.NoError(t, err)

	// Default window (72h) covers the receptions → one weighted link.
	resp := meshLinksAt(t, s, "")
	assert.Equal(t, "72h", resp.Window)
	require.Len(t, resp.Links, 1)
	l := resp.Links[0]
	assert.Equal(t, "aa", l.A)
	assert.Equal(t, "bb", l.B)
	assert.Equal(t, 2, l.Observations)
	assert.Equal(t, 1, l.DaysActive)
	assert.InDelta(t, -4, l.BestSnr, 1e-9)
	assert.NotEmpty(t, l.LastSeen)

	// A window whose day-bucket floor is after the (2-day-old) rollup bucket
	// excludes it — still 200 + []. (Tier 1 is day-grained; sub-day precision
	// lives only in the raw tail.)
	resp = meshLinksAt(t, s, "1h")
	assert.Equal(t, "1h", resp.Window)
	assert.Empty(t, resp.Links)

	// A garbage window falls back to the default rather than erroring.
	assert.Equal(t, "72h", meshLinksAt(t, s, "not-a-duration").Window)
}

func TestServeMeshLinkLayer(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
		{ID: "meshcore", Name: "MeshCore Mesh", PollInterval: time.Minute},
	}))
	require.NoError(t, s.Store.RecordAttempt(ctx, "meshcore", nil)) // healthy poll → status OK

	// Four located nodes: A inside calaveras (37.8–38.55 lat); B a neighbour just
	// outside; C+D both outside and linked only to each other.
	nodes := []struct {
		id, pk, name string
		lat, lng     float64
	}{
		{"meshcore:aa", "aa", "Ridge A", 38.1, -120.5}, // inside calaveras
		{"meshcore:bb", "bb", "Peak B", 38.7, -119.6},  // outside
		{"meshcore:cc", "cc", "Far C", 38.75, -119.55}, // outside
		{"meshcore:dd", "dd", "Far D", 38.72, -119.52}, // outside
	}
	for _, n := range nodes {
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: n.id, Layer: gridv1.Layer_NETWORK, Severity: gridv1.Severity_INFO,
			Status: gridv1.EventStatus_ACTIVE, Headline: n.name,
			Geometry:   pointGeom(n.lat, n.lng),
			ObservedAt: timestamppb.New(base),
			Provenance: &gridv1.Provenance{SourceId: "meshcore"},
			Detail:     &gridv1.Event_Network{Network: &gridv1.NetworkDetail{PublicKey: n.pk, NodeType: "repeater", Name: n.name}},
		})
		require.NoError(t, err)
	}

	// Edges: A↔B (touches the region) and C↔D (does not).
	require.NoError(t, s.Store.InsertMeshObservations(ctx, []store.MeshObservation{
		{PubKey: "aa", HeardAt: base, SNR: -5, PathNodes: []string{"bb"}},
		{PubKey: "cc", HeardAt: base, SNR: -6, PathNodes: []string{"dd"}},
	}))
	_, err := s.Store.CompactMeshObservations(ctx)
	require.NoError(t, err)

	rec := get(t, s, "/v1/places/calaveras/map/mesh_link.geojson")
	require.Equal(t, http.StatusOK, rec.Code)
	var fc hazards.FeatureCollection
	decode(t, rec, &fc)

	var lines, points []hazards.Feature
	for _, f := range fc.Features {
		if f.Geometry != nil && f.Geometry.Type == "LineString" {
			lines = append(lines, f)
		} else {
			points = append(points, f)
		}
	}

	// Only the A↔B edge touches the region; C↔D is excluded entirely.
	require.Len(t, lines, 1)
	assert.Equal(t, "aa", lines[0].Properties.MeshLink.A)
	assert.Equal(t, "bb", lines[0].Properties.MeshLink.B)
	assert.Equal(t, 1, lines[0].Properties.MeshLink.Observations)

	// Nodes: A in-region + B the pulled-in neighbour; C/D absent.
	inRegion := map[string]*bool{}
	for _, p := range points {
		require.NotNil(t, p.Properties.Network)
		inRegion[p.Properties.Network.PublicKey] = p.Properties.Network.InRegion
	}
	require.Len(t, points, 2)
	require.NotNil(t, inRegion["aa"])
	assert.True(t, *inRegion["aa"], "A is in-region")
	require.NotNil(t, inRegion["bb"])
	assert.False(t, *inRegion["bb"], "B is a neighbour, not in-region")
	assert.NotContains(t, inRegion, "cc")
	assert.NotContains(t, inRegion, "dd")

	// Metadata is the meshcore-scoped envelope.
	require.NotNil(t, fc.Metadata)
	assert.Equal(t, "mesh_link", fc.Metadata.Layer)
	assert.Equal(t, "OK", fc.Metadata.SourceStatus)
}
