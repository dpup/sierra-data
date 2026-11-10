package gridapi

import (
	"testing"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// geometryFormat=NONE on /api/v1/events serves bbox and centroid without the
// shape; the other formats still carry it. Other RPCs refuse NONE.
func TestListEventsGeometryNone(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)
	all := []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}

	full, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: all, PageSize: 200})
	require.NoError(t, err)
	none, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: all, PageSize: 200, GeometryFormat: "none"})
	require.NoError(t, err)
	require.Equal(t, len(full.GetEvents()), len(none.GetEvents()))

	shapes := 0
	for i, ev := range none.GetEvents() {
		assert.Equal(t, full.GetEvents()[i].GetId(), ev.GetId(), "same order")
		assert.Empty(t, ev.GetGeometry().GetGeojson())
		assert.Nil(t, ev.GetGeometry().GetObject())
		if len(full.GetEvents()[i].GetGeometry().GetGeojson()) > 0 {
			shapes++
			assert.NotNil(t, ev.GetGeometry().GetBbox(), ev.GetId())
			assert.NotNil(t, ev.GetGeometry().GetCentroid(), ev.GetId())
		}
	}
	assert.Positive(t, shapes, "the fixtures include shaped events")

	_, err = g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: full.GetEvents()[0].GetId(), GeometryFormat: "NONE"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
