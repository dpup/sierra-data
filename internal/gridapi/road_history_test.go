package gridapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

func seedRoadSamples(t *testing.T, s *Service, road string) {
	t.Helper()
	d := int32(42)
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{
		{RoadID: road, At: base.Add(-2 * time.Hour), Status: int32(api.RoadStatus_OPEN), Congestion: int32(api.CongestionLevel_CLEAR), DurationMinutes: &d, DelayMinutes: new(int32)},
		{RoadID: road, At: base.Add(-time.Hour), Status: int32(api.RoadStatus_CLOSED)},
	}))
}

func TestGetRoadHistory(t *testing.T) {
	s := newTestService(t)
	road := s.Cfg.Roads.MonitoredRoads[0].ID
	seedRoadSamples(t, s, road)
	g := NewGridServer(s)

	h, err := g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: "road:" + road})
	require.NoError(t, err)
	require.Len(t, h.GetSamples(), 2)
	assert.Equal(t, "OPEN", h.GetSamples()[0].GetStatus())
	assert.Equal(t, "CLEAR", h.GetSamples()[0].GetCongestion())
	assert.Equal(t, int32(42), h.GetSamples()[0].GetDurationMinutes().GetValue())
	assert.Equal(t, "CLOSED", h.GetSamples()[1].GetStatus())
	assert.Nil(t, h.GetSamples()[1].GetDurationMinutes(), "no travel time is null")
	assert.Equal(t, "", h.GetSamples()[1].GetCongestion())

	_, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: "nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: road, From: timestamppb.New(base), To: timestamppb.New(base)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
