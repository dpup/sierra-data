package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func sample(pubkey string, at time.Time) MeshTelemetrySample {
	return MeshTelemetrySample{
		PubKey: pubkey, ReportedAt: at, ReceivedAt: at.Add(30 * time.Second), Reporter: "alanpi",
		BatteryVolts: f64(4.14), BatteryPct: f64(97), BatteryPctSource: "estimated",
		TemperatureC: f64(37), NoiseFloorDBm: i64(-116), LastSNRdB: f64(12.5),
		UptimeS: 2615083, PacketsSent: 150305, PacketsRecv: 649331,
	}
}

func TestMeshTelemetryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	require.NoError(t, s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{
		sample("aa11", t0),
		sample("aa11", t0.Add(15*time.Minute)),
	}))

	got, truncated, err := s.MeshTelemetry(ctx, "aa11", t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, got, 2)
	assert.True(t, got[0].ReportedAt.Before(got[1].ReportedAt), "oldest first")

	// A gauge the monitor READ survives as a value...
	require.NotNil(t, got[0].BatteryPct)
	assert.InDelta(t, 97, *got[0].BatteryPct, 1e-9)
	assert.Equal(t, "estimated", got[0].BatteryPctSource)
	// ...and one it could not stays ABSENT. This is the whole reason the columns
	// are nullable: humidity nil must not read back as 0% humidity.
	assert.Nil(t, got[0].Humidity)
	assert.Nil(t, got[0].Pressure)
	assert.Nil(t, got[0].TxQueueLen)
	// Counters are plain — they are only written alongside a successful read.
	assert.Equal(t, int64(150305), got[0].PacketsSent)
}

// The mesh poller ticks every 60s while reports arrive every ~15 minutes, so it
// offers the same sample over and over. Keying on the monitor's own timestamp is
// what makes that free — without it one reading becomes fifteen points and every
// chart shows a flat step per report.
func TestMeshTelemetryInsertIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 15; i++ {
		// Our receive clock moves every tick; the monitor's stamp does not.
		s2 := sample("aa11", t0)
		s2.ReceivedAt = t0.Add(time.Duration(i) * time.Minute)
		require.NoError(t, s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{s2}))
	}
	got, _, err := s.MeshTelemetry(ctx, "aa11", t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "one reading is one row however many times it is offered")
	assert.Equal(t, t0.Unix(), got[0].ReceivedAt.Unix(),
		"receivedAt records when we FIRST held it, not the latest re-offer")
}

func TestMeshTelemetryCoverageAndPrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	var batch []MeshTelemetrySample
	for i := 0; i < 5; i++ {
		batch = append(batch, sample("aa11", t0.Add(time.Duration(i)*time.Hour)))
	}
	require.NoError(t, s.InsertMeshTelemetry(ctx, batch))

	first, last, n, err := s.MeshTelemetryCoverage(ctx, "aa11")
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, t0.Unix(), first.Unix())
	assert.Equal(t, t0.Add(4*time.Hour).Unix(), last.Unix())

	removed, err := s.PruneMeshTelemetry(ctx, t0.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), removed)

	_, _, n, err = s.MeshTelemetryCoverage(ctx, "aa11")
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// A node we hold nothing for reports empty coverage, not an error — the
	// caller renders "we have no samples", which is not "the node was quiet".
	f, l, n, err := s.MeshTelemetryCoverage(ctx, "nobody")
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, f.IsZero() && l.IsZero())
}

// A node known only by a key prefix gets promoted when an advert supplies the
// full key. Its archived samples have to follow, or one node's battery history
// splits in two — silently, once, with nothing left pointing at the old half.
func TestMeshTelemetryFollowsAPromotedKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	require.NoError(t, s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{
		sample("de0715314cfa9b5e", t0),
		sample("de0715314cfa9b5e", t0.Add(15*time.Minute)),
	}))
	full := "de0715314cfa9b5e" + "8f2c1d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d"[:48]

	moved, err := s.RenameMeshTelemetryPubKey(ctx, "de0715314cfa9b5e", full)
	require.NoError(t, err)
	assert.Equal(t, int64(2), moved)

	got, _, err := s.MeshTelemetry(ctx, full, t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, got, 2, "the history moved with the node")

	got, _, err = s.MeshTelemetry(ctx, "de0715314cfa9b5e", t0.Add(-time.Hour), t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, got, "nothing is left under the retired provisional key")
}
