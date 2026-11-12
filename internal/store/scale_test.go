package store

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// Opt-in measurements on a production-sized database (P5 performance pass).
//
//	GRID_SCALE_DATABASE_URL=postgres://...  go test ./internal/store -run 'AtScale' -v
//
// The database is WRITTEN TO: point it at a throwaway clone of an imported
// fixture (gridctl import of legacyfixture.ProductionScale, see
// docs/import.md and the P5 note), never at anything real. GRID_SCALE_RTT adds
// a simulated network round-trip time to every client write (for example 1ms,
// roughly a Fargate task and an RDS instance in one availability zone).
const scaleEnv = "GRID_SCALE_DATABASE_URL"

func scaleURL(t *testing.T) string {
	url := os.Getenv(scaleEnv)
	if url == "" {
		t.Skipf("SKIP: set %s to a throwaway production-sized database to run the scale measurements", scaleEnv)
	}
	return url
}

// roundTrips counts, per wrapped connection, the client writes: pgx sends one
// write per round trip (a pipelined Parse/Bind/Execute/Sync is one write), so
// the count is the number of times a statement waited for the server. With
// rtt set, each write also waits that long, standing in for the network.
type roundTrips struct {
	n   atomic.Int64
	rtt time.Duration
}

func (r *roundTrips) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, rt: r}, nil
}

type countingConn struct {
	net.Conn
	rt *roundTrips
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.rt.n.Add(1)
	if c.rt.rtt > 0 {
		// Spin: time.Sleep rounds a 1ms request up to about 2ms on a virtualized
		// timer, which would double the simulated network.
		for start := time.Now(); time.Since(start) < c.rt.rtt; {
		}
	}
	return c.Conn.Write(b)
}

type scaleRow struct {
	name   string
	dur    time.Duration
	trips  int64
	detail string
}

func TestWritePhaseAtScale(t *testing.T) {
	url := scaleURL(t)
	ctx := context.Background()
	var rtt time.Duration
	if v := os.Getenv("GRID_SCALE_RTT"); v != "" {
		var err error
		rtt, err = time.ParseDuration(v)
		require.NoError(t, err)
	}
	rt := &roundTrips{}
	s, err := Open(url, WithDialFunc(rt.dial))
	require.NoError(t, err)
	defer s.Close()
	rt.rtt = rtt // after Open: the lease and migrations are not under test

	// Realistic inputs come from the database itself.
	load := func(layer gridv1.Layer, status gridv1.EventStatus, n int) []*gridv1.Event {
		var out []*gridv1.Event
		q := EventQuery{Layers: []gridv1.Layer{layer}, Statuses: []gridv1.EventStatus{status}, PageSize: 200}
		for len(out) < n {
			page, next, err := s.QueryEvents(ctx, q)
			require.NoError(t, err)
			out = append(out, page...)
			if next == "" {
				break
			}
			q.PageToken = next
		}
		require.GreaterOrEqual(t, len(out), n, "layer %v status %v", layer, status)
		return out[:n]
	}
	mesh := load(gridv1.Layer_MESH, gridv1.EventStatus_ACTIVE, 250)
	roads := load(gridv1.Layer_ROAD_INCIDENT, gridv1.EventStatus_ACTIVE, 150)
	// QueryEvents returns events with a stored ingest stamp; a real poll's
	// events do not carry the previous revision's.
	var rows []scaleRow
	measure := func(name, detail string, fn func()) {
		before := rt.n.Load()
		start := time.Now()
		fn()
		rows = append(rows, scaleRow{name, time.Since(start), rt.n.Load() - before, detail})
	}
	now := time.Now()

	// Mesh poller tick (60 s cadence): 250 nodes polled, telemetry changed on 60
	// of them (hash-equal refresh, ForceWrite), a coalescing-window tick for
	// TouchSeen, 250 receptions, activity for every node, 60 telemetry samples.
	changed := make([]*gridv1.Event, 0, 60)
	for i, ev := range mesh[:60] {
		c := ev
		if c.GetMesh().GetTelemetry() == nil {
			c.GetMesh().Telemetry = &gridv1.MeshTelemetry{}
		}
		c.GetMesh().GetTelemetry().Snr = wrapperspb.Double(float64(i) + 0.5)
		changed = append(changed, c)
	}
	measure("mesh: hash pre-check (ContentHashes, 250 ids)", "read", func() {
		ids := make([]string, len(mesh))
		for i, ev := range mesh {
			ids[i] = ev.GetId()
		}
		_, err := s.ContentHashes(ctx, ids)
		require.NoError(t, err)
	})
	measure("mesh: telemetry refresh (UpsertEvent hash-equal x60)", "60 txs", func() {
		for _, ev := range changed {
			_, err := s.UpsertEvent(ctx, ev)
			require.NoError(t, err)
		}
	})
	measure("mesh: TouchSeen (250 ids, all stale)", "1 tx", func() {
		ids := make([]string, len(mesh))
		for i, ev := range mesh {
			ids[i] = ev.GetId()
		}
		n, err := s.TouchSeen(ctx, ids, now, now.Add(-time.Minute))
		require.NoError(t, err)
		require.EqualValues(t, len(ids), n)
	})
	measure("mesh: RecordMeshTick (250 observations + 250 activity)", "1 tx", func() {
		obs := make([]MeshObservation, 250)
		act := make([]MeshActivity, 250)
		for i := range obs {
			pk := mesh[i].GetId()[len("meshcore:"):]
			obs[i] = MeshObservation{PubKey: pk, HeardAt: now.Add(time.Duration(i) * time.Millisecond), Broker: "b", Gateway: "g",
				SNR: 7.5, RSSI: -80, HopCount: 2, Path: []string{"aa", "bb"}, PathNodes: []string{pk, ""}}
			act[i] = MeshActivity{PubKey: pk, LastHeardAt: now}
		}
		require.NoError(t, s.RecordMeshTick(ctx, obs, act))
	})
	measure("mesh: InsertMeshTelemetry (60 samples)", "1 tx", func() {
		samples := make([]MeshTelemetrySample, 60)
		for i := range samples {
			pk := mesh[i].GetId()[len("meshcore:"):]
			samples[i] = MeshTelemetrySample{PubKey: pk, ReportedAt: now.Add(time.Hour + time.Duration(i)*time.Second),
				ReceivedAt: now, Reporter: "monitor", UptimeS: int64(1_000_000 + i), PacketsSent: int64(5000 + i), PacketsRecv: int64(9000 + i)}
		}
		require.NoError(t, s.InsertMeshTelemetry(ctx, samples))
	})

	// First tick after a restart, or after a place change: every event takes
	// the full path once (place attachments are derived state).
	measure("full reconcile (UpsertEvent hash-equal x250)", "300 txs", func() {
		for _, ev := range mesh {
			_, err := s.UpsertEvent(ctx, ev)
			require.NoError(t, err)
		}
	})
	// Content changes: new revisions.
	measure("content change (UpsertEvent new revision x100)", "100 txs", func() {
		for _, ev := range roads[:100] {
			c := ev
			c.Headline += " (updated)"
			c.ObservedAt = timestamppb.New(now)
			_, err := s.UpsertEvent(ctx, c)
			require.NoError(t, err)
		}
	})
	// Lifecycle sweep: a source dropped 100 active events.
	measure("sweep (TransitionEvents x100 -> RESOLVED)", "1 tx", func() {
		ids := make([]string, 100)
		for i := range ids {
			ids[i] = roads[i].GetId()
		}
		require.NoError(t, s.TransitionEvents(ctx, ids, gridv1.EventStatus_RESOLVED, now))
	})
	measure("road samples (InsertRoadSamples x150)", "1 tx", func() {
		samples := make([]RoadSample, 150)
		for i := range samples {
			samples[i] = RoadSample{RoadID: fmt.Sprintf("road-%d", i%30), At: now.Add(time.Duration(i) * time.Second), Status: 1}
		}
		require.NoError(t, s.InsertRoadSamples(ctx, samples))
	})

	var total time.Duration
	var trips int64
	t.Logf("rtt %s", rtt)
	t.Logf("%-62s %10s %8s", "operation", "ms", "writes")
	for _, r := range rows {
		t.Logf("%-62s %10.1f %8d", r.name, float64(r.dur.Microseconds())/1000, r.trips)
		total += r.dur
		trips += r.trips
	}
	t.Logf("%-62s %10.1f %8d", "TOTAL", float64(total.Microseconds())/1000, trips)
}
