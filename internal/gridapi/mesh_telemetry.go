package gridapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

const (
	// defaultTelemetryWindow is what you get with no from/to: a day, the range a
	// battery or temperature curve is read at.
	defaultTelemetryWindow = 24 * time.Hour
	// maxTelemetryWindow clamps a request to the retention ceiling.
	maxTelemetryWindow = 400 * 24 * time.Hour
)

// GetMeshTelemetry returns one node's archived monitor samples.
//
// The reading on each sample is grid.v1.MeshAdminTelemetry — the same message
// the node's event carries — so the archive cannot describe a reading
// differently from the live record, and an unread gauge stays null in both.
func (g *GridServer) GetMeshTelemetry(ctx context.Context, req *gridv1.GetMeshTelemetryRequest) (*gridv1.MeshTelemetryArchive, error) {
	node := strings.ToLower(strings.TrimSpace(req.GetNode()))
	if node == "" {
		return nil, invalidErr(errors.New("node is required: the full public key of one mesh node"))
	}

	now := g.svc.Now()
	to := now
	if req.GetTo() != nil {
		to = req.GetTo().AsTime()
	}
	from := to.Add(-defaultTelemetryWindow)
	if req.GetFrom() != nil {
		from = req.GetFrom().AsTime()
	}
	if !from.Before(to) {
		return nil, invalidErr(errors.New("from must be before to"))
	}
	if to.Sub(from) > maxTelemetryWindow {
		from = to.Add(-maxTelemetryWindow)
	}

	samples, truncated, err := g.svc.Store.MeshTelemetry(ctx, node, from, to)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	covFrom, covTo, covN, err := g.svc.Store.MeshTelemetryCoverage(ctx, node)
	if err != nil {
		return nil, internalErr(ctx, err)
	}

	out := &gridv1.MeshTelemetryArchive{
		Node:      node,
		From:      timestamppb.New(from.UTC()),
		To:        timestamppb.New(to.UTC()),
		Truncated: truncated,
		Coverage: &gridv1.MeshTelemetryCoverage{
			From:    tsOrNil(covFrom),
			To:      tsOrNil(covTo),
			Samples: int32(covN),
		},
		Reboots: rebootTimes(samples),
		Samples: make([]*gridv1.MeshTelemetrySample, 0, len(samples)),
	}
	if c := medianCadence(samples); c > 0 {
		out.CadenceSeconds = wrapperspb.Int64(c)
	}
	for _, t := range samples {
		out.Samples = append(out.Samples, &gridv1.MeshTelemetrySample{
			ReceivedAt: tsOrNil(t.ReceivedAt),
			Reading:    adminReading(t),
		})
	}
	return out, nil
}

// adminReading rebuilds the proto reading from an archived row. Every gauge goes
// back through its wrapper type: a column that is NULL is a gauge the monitor
// could not read, and it must reach the client as null rather than as 0.
func adminReading(t store.MeshTelemetrySample) *gridv1.MeshAdminTelemetry {
	return &gridv1.MeshAdminTelemetry{
		ReporterId:    t.Reporter,
		ReportedAt:    tsOrNil(t.ReportedAt),
		LastSuccessAt: tsOrNil(t.LastSuccessAt),
		LastAttemptAt: tsOrNil(t.LastAttemptAt),

		BatteryVolts:         doubleOrNil(t.BatteryVolts),
		BatteryPercent:       doubleOrNil(t.BatteryPct),
		BatteryPercentSource: t.BatteryPctSource,
		TemperatureC:         doubleOrNil(t.TemperatureC),
		Humidity:             doubleOrNil(t.Humidity),
		Pressure:             doubleOrNil(t.Pressure),
		NoiseFloorDbm:        int32OrNil(t.NoiseFloorDBm),
		LastSnrDb:            doubleOrNil(t.LastSNRdB),
		LastRssiDbm:          int32OrNil(t.LastRSSIdBm),
		TxQueueLen:           int32OrNil(t.TxQueueLen),

		UptimeSeconds:   t.UptimeS,
		AirtimeMs:       t.AirtimeMs,
		RxAirtimeMs:     t.RxAirtimeMs,
		PacketsSent:     t.PacketsSent,
		PacketsReceived: t.PacketsRecv,
		SentFlood:       t.SentFlood,
		SentDirect:      t.SentDirect,
		RecvFlood:       t.RecvFlood,
		RecvDirect:      t.RecvDirect,
		DirectDups:      t.DirectDups,
		FloodDups:       t.FloodDups,
		FullEvents:      t.FullEvts,
		RecvErrors:      t.RecvErrors,
	}
}

// rebootTimes returns the sample times where uptime went backwards.
//
// Reported rather than smoothed away: every lifetime counter restarts at a
// reboot, so this is both the instruction a chart needs in order to break its
// counter lines AND a fact about the node worth seeing on its own.
func rebootTimes(samples []store.MeshTelemetrySample) []*timestamppb.Timestamp {
	var out []*timestamppb.Timestamp
	for i := 1; i < len(samples); i++ {
		if samples[i].UptimeS < samples[i-1].UptimeS {
			out = append(out, timestamppb.New(samples[i].ReportedAt.UTC()))
		}
	}
	return out
}

// medianCadence is the median gap between consecutive samples, in seconds.
//
// Median, not mean: one multi-hour outage in a day of 15-minute reports drags a
// mean far past anything the feed actually does, and this number's whole job is
// to tell a client which gaps are abnormal.
func medianCadence(samples []store.MeshTelemetrySample) int64 {
	if len(samples) < 2 {
		return 0
	}
	gaps := make([]int64, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		if g := int64(samples[i].ReportedAt.Sub(samples[i-1].ReportedAt).Seconds()); g > 0 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

func doubleOrNil(v *float64) *wrapperspb.DoubleValue {
	if v == nil {
		return nil
	}
	return wrapperspb.Double(*v)
}

func int32OrNil(v *int64) *wrapperspb.Int32Value {
	if v == nil {
		return nil
	}
	return wrapperspb.Int32(int32(*v))
}
