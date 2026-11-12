package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The batched writes (observations, activity, telemetry, road samples,
// transitions) replaced per-row loops. These tests pin what the loops
// guaranteed: input order, in-batch repeats, chunk boundaries, and, for
// telemetry, the exact rows a sequential reference produces.

func TestInsertMeshObservationsKeepsInputOrderAcrossChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	n := obsChunk + 17 // one full chunk and a short one
	obs := make([]MeshObservation, n)
	for i := range obs {
		obs[i] = MeshObservation{PubKey: fmt.Sprintf("pk%05d", i), HeardAt: t0.Add(time.Duration(i) * time.Second),
			Broker: "b", Gateway: "g", SNR: float64(i%20) - 5, RSSI: int32(-60 - i%40), HopCount: uint32(i % 4),
			Path: []string{"aa", "bb"}[:i%3%3], PathNodes: []string{"x", ""}[:i%2]}
	}
	require.NoError(t, s.InsertMeshObservations(ctx, obs))

	rows, err := s.db.QueryContext(ctx, `SELECT id, pubkey, heard_at, snr, rssi, hop_count, path, path_nodes
		FROM mesh_observations ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	i := 0
	var prev int64
	for rows.Next() {
		var id, heard, hops int64
		var pk, path, pathNodes string
		var snr float64
		var rssi int32
		require.NoError(t, rows.Scan(&id, &pk, &heard, &snr, &rssi, &hops, &path, &pathNodes))
		require.Less(t, i, n)
		assert.Greater(t, id, prev, "ids ascend")
		prev = id
		o := obs[i]
		assert.Equal(t, o.PubKey, pk, "row %d is input row %d", i, i)
		assert.Equal(t, o.HeardAt.Unix(), heard)
		assert.Equal(t, o.SNR, snr)
		assert.Equal(t, o.RSSI, rssi)
		assert.EqualValues(t, o.HopCount, hops)
		assert.Equal(t, joinHops(o.Path), path)
		assert.Equal(t, joinHops(o.PathNodes), pathNodes)
		i++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, n, i)
}

func joinHops(h []string) string {
	out := ""
	for i, x := range h {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func TestMeshActivityBatchFoldsRepeatedKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	require.NoError(t, s.RecordMeshTick(ctx, nil, []MeshActivity{
		{PubKey: "ABCD", LastHeardAt: base.Add(10 * time.Minute)},
	}))
	// One batch names a node three times, in two cases, and carries an empty
	// key: per clock the greatest wins, and nothing already stored goes back.
	require.NoError(t, s.RecordMeshTick(ctx, nil, []MeshActivity{
		{PubKey: "abcd", LastHeardAt: base, LastRelayedAt: base.Add(3 * time.Minute)},
		{PubKey: "ABCD", LastHeardAt: base.Add(5 * time.Minute), LastRelayedAt: base.Add(time.Minute)},
		{PubKey: "", LastHeardAt: base},
		{PubKey: "abcd", LastHeardAt: base.Add(2 * time.Minute)},
		{PubKey: "ef01", LastRelayedAt: base},
	}))
	got, err := s.MeshActivityFor(ctx, []string{"abcd", "ef01", ""})
	require.NoError(t, err)
	assert.Equal(t, map[string]MeshActivity{
		"abcd": {PubKey: "abcd", LastHeardAt: base.Add(10 * time.Minute), LastRelayedAt: base.Add(3 * time.Minute)},
		"ef01": {PubKey: "ef01", LastRelayedAt: base},
	}, got)
}

func TestInsertRoadSamplesBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	require.NoError(t, s.InsertRoadSamples(ctx, []RoadSample{
		{RoadID: "hwy4", At: t0, Status: 1, Congestion: 2, DurationMinutes: i32(20), DelayMinutes: i32(3)},
		{RoadID: "hwy4", At: t0, Status: 3},                  // same key in one batch: the first wins
		{RoadID: "", At: t0, Status: 1},                      // no road: dropped
		{RoadID: "hwy4", Status: 1},                          // no time: dropped
		{RoadID: "hwy4", At: t0.Add(time.Minute), Status: 1}, // no durations: NULL
	}))
	require.NoError(t, s.InsertRoadSamples(ctx, []RoadSample{{RoadID: "hwy4", At: t0, Status: 3}})) // stored: kept
	got, err := s.RoadSamples(ctx, "hwy4", t0, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.EqualValues(t, 1, got[0].Status)
	assert.EqualValues(t, 2, got[0].Congestion)
	require.NotNil(t, got[0].DurationMinutes)
	assert.EqualValues(t, 20, *got[0].DurationMinutes)
	require.NotNil(t, got[0].DelayMinutes)
	assert.EqualValues(t, 3, *got[0].DelayMinutes)
	assert.Nil(t, got[1].DurationMinutes)
	assert.Nil(t, got[1].DelayMinutes)
}

// sequentialTelemetry is the per-sample loop the batch replaced, kept as the
// reference: one lookup then one insert per sample, each insert visible to the
// next lookup.
func sequentialTelemetry(ctx context.Context, tx *sql.Tx, samples []MeshTelemetrySample) error {
	for _, t := range samples {
		if t.PubKey == "" || t.ReportedAt.IsZero() {
			continue
		}
		var last sql.NullInt64
		var uptime, sent, recv int64
		err := tx.QueryRowContext(ctx, `
			SELECT last_success_at, uptime_s, packets_sent, packets_recv FROM mesh_telemetry
			WHERE pubkey = $1 AND reporter = $2 AND reported_at < $3
			ORDER BY reported_at DESC LIMIT 1`, t.PubKey, t.Reporter, t.ReportedAt.Unix()).Scan(&last, &uptime, &sent, &recv)
		switch {
		case err == nil && !t.LastSuccessAt.IsZero() && last.Valid && last.Int64 == t.LastSuccessAt.Unix():
			continue
		case err == nil && t.UptimeS > 0 && t.UptimeS == uptime && t.PacketsSent == sent && t.PacketsRecv == recv:
			continue
		case err != nil && err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mesh_telemetry (`+telemetryColumns+`)
			VALUES (`+params(1, 29)+`) ON CONFLICT (pubkey, reported_at) DO NOTHING`,
			t.PubKey, t.ReportedAt.Unix(), t.ReceivedAt.Unix(), t.Reporter,
			unixTimeOrNil(t.LastSuccessAt), unixTimeOrNil(t.LastAttemptAt),
			t.BatteryVolts, t.BatteryPct, t.BatteryPctSource, t.TemperatureC, t.Humidity, t.Pressure,
			t.NoiseFloorDBm, t.LastSNRdB, t.LastRSSIdBm, t.TxQueueLen,
			t.UptimeS, t.AirtimeMs, t.RxAirtimeMs, t.PacketsSent, t.PacketsRecv,
			t.SentFlood, t.SentDirect, t.RecvFlood, t.RecvDirect,
			t.DirectDups, t.FloodDups, t.FullEvts, t.RecvErrors); err != nil {
			return err
		}
	}
	return nil
}

func dumpTelemetry(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT pubkey, reporter, reported_at, received_at, last_success_at, uptime_s,
		packets_sent, packets_recv, battery_volts FROM mesh_telemetry ORDER BY pubkey, reporter, reported_at`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pk, rep string
		var rpt, rcv, up, sent, recv int64
		var ls sql.NullInt64
		var bv sql.NullFloat64
		require.NoError(t, rows.Scan(&pk, &rep, &rpt, &rcv, &ls, &up, &sent, &recv, &bv))
		out = append(out, fmt.Sprintf("%s/%s rep=%d rcv=%d ls=%v up=%d s=%d r=%d bv=%v", pk, rep, rpt, rcv, ls, up, sent, recv, bv))
	}
	require.NoError(t, rows.Err())
	return out
}

// Random batches, with repeats, out-of-order stamps, cached re-sends and
// samples colliding with stored rows, give the same rows through the batch as
// through the sequential reference.
func TestMeshTelemetryBatchMatchesSequentialReference(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			batched, ref := newTestStore(t), newTestStore(t)
			t0 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
			gen := func() MeshTelemetrySample {
				// Few keys and few stamps: collisions and repeats are the point.
				rep := t0.Add(time.Duration(rng.Intn(12)) * 5 * time.Minute)
				s := MeshTelemetrySample{
					PubKey:     []string{"aa", "bb", "cc"}[rng.Intn(3)],
					Reporter:   []string{"r1", "r2"}[rng.Intn(2)],
					ReportedAt: rep, ReceivedAt: rep.Add(time.Duration(rng.Intn(3)) * time.Second),
					UptimeS: int64(rng.Intn(5)), PacketsSent: int64(rng.Intn(3)), PacketsRecv: int64(rng.Intn(3)),
				}
				if rng.Intn(4) > 0 {
					s.LastSuccessAt = t0.Add(time.Duration(rng.Intn(6)) * 15 * time.Minute)
				}
				if rng.Intn(2) == 0 {
					s.BatteryVolts = f64(3.5 + float64(rng.Intn(10))/10)
				}
				if rng.Intn(15) == 0 {
					s.PubKey = ""
				}
				if rng.Intn(15) == 0 {
					s.ReportedAt = time.Time{}
				}
				return s
			}
			for batch := 0; batch < 12; batch++ {
				in := make([]MeshTelemetrySample, rng.Intn(25))
				for i := range in {
					in[i] = gen()
				}
				require.NoError(t, batched.InsertMeshTelemetry(ctx, in))
				require.NoError(t, ref.inTx(ctx, func(tx *sql.Tx) error { return sequentialTelemetry(ctx, tx, in) }))
				require.Equal(t, dumpTelemetry(t, ref), dumpTelemetry(t, batched), "after batch %d", batch)
			}
		})
	}
}
