package gridapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
)

const defaultRoadHistoryWindow = 7 * 24 * time.Hour

// GetRoadHistory returns one monitored segment's archived samples.
func (g *GridServer) GetRoadHistory(ctx context.Context, req *gridv1.GetRoadHistoryRequest) (*gridv1.RoadHistory, error) {
	road := strings.TrimPrefix(req.GetRoad(), "road:")
	known := false
	if g.svc.Cfg != nil {
		for _, r := range g.svc.Cfg.Roads.MonitoredRoads {
			known = known || r.ID == road
		}
	}
	if !known {
		return nil, notFoundErr("unknown road segment: %q", req.GetRoad())
	}
	to := g.svc.Now()
	if req.GetTo() != nil {
		to = req.GetTo().AsTime()
	}
	from := to.Add(-defaultRoadHistoryWindow)
	if req.GetFrom() != nil {
		from = req.GetFrom().AsTime()
	}
	if !from.Before(to) {
		return nil, invalidErr(errors.New("from must be before to"))
	}
	if to.Sub(from) > maxTelemetryWindow {
		from = to.Add(-maxTelemetryWindow)
	}
	samples, err := g.svc.Store.RoadSamples(ctx, road, from, to)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	out := &gridv1.RoadHistory{Road: road, From: timestamppb.New(from), To: timestamppb.New(to)}
	for _, s := range samples {
		rs := &gridv1.RoadSample{
			At:         timestamppb.New(s.At),
			Status:     enumName(api.RoadStatus(s.Status).String()),
			Congestion: enumName(api.CongestionLevel(s.Congestion).String()),
		}
		if s.DurationMinutes != nil {
			rs.DurationMinutes = wrapperspb.Int32(*s.DurationMinutes)
		}
		if s.DelayMinutes != nil {
			rs.DelayMinutes = wrapperspb.Int32(*s.DelayMinutes)
		}
		out.Samples = append(out.Samples, rs)
	}
	return out, nil
}

// enumName is an enum value's name, "" for UNSPECIFIED.
func enumName(s string) string {
	if strings.HasSuffix(s, "_UNSPECIFIED") {
		return ""
	}
	return s
}
