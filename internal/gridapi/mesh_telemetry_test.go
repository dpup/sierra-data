package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

func f64(v float64) *float64 { return &v }
func i64p(v int64) *int64    { return &v }

// telemetryAt builds one archived sample: a battery the monitor READ, a humidity
// it could not, and counters at `uptime`.
func telemetryAt(node string, at time.Time, pct float64, uptime int64) store.MeshTelemetrySample {
	return store.MeshTelemetrySample{
		PubKey: node, ReportedAt: at, ReceivedAt: at.Add(20 * time.Second), Reporter: "alanpi",
		LastSuccessAt: at, LastAttemptAt: at,
		BatteryVolts: f64(4.14), BatteryPct: f64(pct), BatteryPctSource: "estimated",
		TemperatureC: f64(21.5), NoiseFloorDBm: i64p(-116),
		UptimeS: uptime, PacketsSent: 150305, PacketsRecv: 649331,
	}
}

func TestGetMeshTelemetry(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	ctx := context.Background()
	node := "de0715314cfa9b5e"

	// Four 15-minute samples; the node reboots before the last one.
	t0 := base.Add(-time.Hour)
	require.NoError(t, s.Store.InsertMeshTelemetry(ctx, []store.MeshTelemetrySample{
		telemetryAt(node, t0, 97, 2615083),
		telemetryAt(node, t0.Add(15*time.Minute), 96.5, 2615983),
		telemetryAt(node, t0.Add(30*time.Minute), 96, 2616883),
		telemetryAt(node, t0.Add(45*time.Minute), 95.5, 120), // rebooted
	}))

	out, err := g.GetMeshTelemetry(ctx, &gridv1.GetMeshTelemetryRequest{Node: node})
	require.NoError(t, err)
	require.Len(t, out.GetSamples(), 4)

	// The reading IS MeshAdminTelemetry — the same message the event carries.
	first := out.GetSamples()[0].GetReading()
	assert.Equal(t, "alanpi", first.GetReporterId())
	assert.InDelta(t, 97, first.GetBatteryPercent().GetValue(), 1e-9)
	assert.Equal(t, "estimated", first.GetBatteryPercentSource())
	assert.Equal(t, int64(150305), first.GetPacketsSent())
	// A gauge the monitor could not read stays NULL on the wire. This is the
	// distinction the whole chain exists to carry: an unreadable battery must
	// never reach a chart as a flat one.
	assert.Nil(t, first.Humidity)
	assert.Nil(t, first.Pressure)
	assert.Nil(t, first.TxQueueLen)

	// Cadence is reported so a client knows which gaps are abnormal.
	require.NotNil(t, out.GetCadenceSeconds())
	assert.Equal(t, int64(900), out.GetCadenceSeconds().GetValue())

	// The reboot is named, not smoothed: every counter restarts there.
	require.Len(t, out.GetReboots(), 1)
	assert.Equal(t, t0.Add(45*time.Minute).Unix(), out.GetReboots()[0].AsTime().Unix())

	// Coverage says what we HOLD, so an empty window is distinguishable from a
	// quiet node.
	assert.Equal(t, int32(4), out.GetCoverage().GetSamples())
	assert.Equal(t, t0.Unix(), out.GetCoverage().GetFrom().AsTime().Unix())
	assert.False(t, out.GetTruncated())
}

func TestGetMeshTelemetryWindowAndEmptyCoverage(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	ctx := context.Background()
	node := "de0715314cfa9b5e"

	old := base.Add(-72 * time.Hour)
	require.NoError(t, s.Store.InsertMeshTelemetry(ctx, []store.MeshTelemetrySample{
		telemetryAt(node, old, 90, 100),
	}))

	// The default window is a day, so a three-day-old sample is outside it —
	// and coverage still reports that we hold one, which is what stops a client
	// rendering "no data" as "the node was silent".
	out, err := g.GetMeshTelemetry(ctx, &gridv1.GetMeshTelemetryRequest{Node: node})
	require.NoError(t, err)
	assert.Empty(t, out.GetSamples())
	assert.Equal(t, int32(1), out.GetCoverage().GetSamples())
	assert.Nil(t, out.GetCadenceSeconds(), "one sample has no cadence to report")

	// A node we hold nothing for is empty coverage, not an error.
	out, err = g.GetMeshTelemetry(ctx, &gridv1.GetMeshTelemetryRequest{Node: "nobody"})
	require.NoError(t, err)
	assert.Empty(t, out.GetSamples())
	assert.Equal(t, int32(0), out.GetCoverage().GetSamples())
	assert.Nil(t, out.GetCoverage().GetFrom())
}

func TestGetMeshTelemetryRejectsMissingNode(t *testing.T) {
	g := NewGridServer(newTestService(t))
	_, err := g.GetMeshTelemetry(context.Background(), &gridv1.GetMeshTelemetryRequest{})
	require.Error(t, err, "a cross-node dump is a different product; node is required")
}
