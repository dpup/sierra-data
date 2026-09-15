package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/pushingest"
	"github.com/dpup/sierra-data/internal/store"
)

// A full public key and the 8-byte prefix a monitor would identify it by.
const (
	fullKey   = "de0715314cfa9b5e" + "0011223344556677" + "8899aabbccddeeff" + "0123456789abcdef"
	keyPrefix = "de0715314cfa9b5e"
)

var pollNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// fakePush is a canned MeshPushSource.
type fakePush struct{ snap pushingest.MeshSnapshot }

func (f *fakePush) MeshReports() pushingest.MeshSnapshot { return f.snap }
func (f *fakePush) ReporterIDs(string) []string          { return []string{"alan-pi"} }

// fakePrior is a canned Prior over a fixed event set.
type fakePrior struct{ events []*gridv1.Event }

func (p *fakePrior) ByID(id string) *gridv1.Event {
	for _, ev := range p.events {
		if ev.GetId() == id {
			return ev
		}
	}
	return nil
}

func (p *fakePrior) ForSource(src string) []*gridv1.Event {
	var out []*gridv1.Event
	for _, ev := range p.events {
		if ev.GetProvenance().GetSourceId() == src {
			out = append(out, ev)
		}
	}
	return out
}

func report(nodeID, name string, success time.Time, mutate ...func(*pushingest.MeshNodeReport)) pushingest.MeshNodeReport {
	r := pushingest.MeshNodeReport{
		ReporterID:  "alan-pi",
		NodeID:      nodeID,
		Name:        name,
		ReceivedAt:  pollNow,
		ReportedAt:  pollNow,
		LastAttempt: pollNow,
		LastSuccess: success,
	}
	if !success.IsZero() {
		r.Telemetry = &gridv1.MeshAdminTelemetry{
			ReporterId:   "alan-pi",
			ReportedAt:   tsProto(pollNow),
			BatteryVolts: wrapperspb.Double(4.14),
			PacketsSent:  150305,
		}
	}
	for _, m := range mutate {
		m(&r)
	}
	return r
}

func pushNormalizer(t *testing.T, reg MeshRegistry, snap pushingest.MeshSnapshot) *NetworkNormalizer {
	t.Helper()
	n := NewNetworkNormalizer(testConfig(), reg, &fakePush{snap: snap})
	n.now = func() time.Time { return pollNow }
	return n
}

// TestPushOnlyNodeBecomesAnEvent covers the case the feature exists for: a quiet
// backbone repeater that no MQTT bridge hears, which only an operator's monitor
// can see.
func TestPushOnlyNodeBecomesAnEvent(t *testing.T) {
	n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "SIERRA Arnold Summit", pollNow)},
	})
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)

	ev := res.Events[0]
	assert.Equal(t, "meshcore:"+keyPrefix, ev.GetId(), "unresolved prefix keys the event")
	assert.Equal(t, gridv1.Layer_MESH, ev.GetLayer())
	assert.Equal(t, gridv1.Severity_INFO, ev.GetSeverity())
	assert.Equal(t, "SIERRA Arnold Summit (repeater)", ev.GetHeadline())
	// Every mesh event stays on the `meshcore` source whichever input heard it: a
	// source_id that flipped as inputs came and went would mint a revision and
	// confuse the per-source sweep.
	assert.Equal(t, "meshcore", ev.GetProvenance().GetSourceId())
	assert.Nil(t, ev.GetGeometry(), "a report carries no coordinates and we do not invent them")

	admin := ev.GetMesh().GetTelemetry().GetAdmin()
	require.NotNil(t, admin)
	assert.InDelta(t, 4.14, admin.GetBatteryVolts().GetValue(), 0.001)
	assert.Equal(t, int64(150305), admin.GetPacketsSent())
	assert.Equal(t, gridv1.MeshReachability_REACHABLE, ev.GetMesh().GetReachability())
}

// A node only an operator's monitor knows publishes NO signal readings — not
// zeroed ones.
//
// This shipped wrong: the signal fields were bare scalars, so the gateway's
// EmitUnpopulated marshaler rendered an unheard node as `snr: 0, rssi: 0,
// hopCount: 0` — 0 dB, heard DIRECT, for a node no bridge has ever heard. Three
// of nine monitored repeaters were in exactly that state on 2026-09-15. The
// fields are wrapper types now, and this is the test that keeps them that way:
// the assertion is on NIL, not on zero.
func TestMonitorOnlyNodePublishesNoSignalReadings(t *testing.T) {
	// No MQTT state at all for this node: the registry is live but has not heard it.
	reg := &fakeMeshRegistry{connected: 1}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "SIERRA BigPratherMeadow", pollNow)},
	})

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)

	tel := res.Events[0].GetMesh().GetTelemetry()
	require.NotNil(t, tel)
	assert.Nil(t, tel.Snr, "no bridge heard it: SNR is unknown, not 0 dB")
	assert.Nil(t, tel.Rssi, "no bridge heard it: RSSI is unknown, not 0 dBm")
	assert.Nil(t, tel.HopCount, "no bridge heard it: hop count is unknown, not heard-direct")
	assert.Nil(t, tel.LastAdvertAt)
	assert.Empty(t, tel.GetGateways())
	// ...while what the monitor DID read is present and unaffected.
	assert.InDelta(t, 4.14, tel.GetAdmin().GetBatteryVolts().GetValue(), 0.001)
}

// TestPushAndMQTTDeduplicateToOneEvent is the headline requirement: the same
// node seen both ways must be ONE event, not two.
func TestPushAndMQTTDeduplicateToOneEvent(t *testing.T) {
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "Arnold Summit",
		HasLocation: true, Lat: 38.137412, Lng: -120.457934,
		SNR: 4.5, RSSI: -93, LastHeardAt: pollNow.Add(-time.Minute),
	}}}
	// The monitor knows the node only by an 8-byte prefix of its public key.
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "SIERRA Arnold Summit", pollNow)},
	})

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "prefix must resolve against the advert catalog, not mint a second event")

	ev := res.Events[0]
	assert.Equal(t, "meshcore:"+fullKey, ev.GetId())
	// Identity precedence is by input class: the signed advert wins the name.
	assert.Equal(t, "Arnold Summit", ev.GetMesh().GetName())
	assert.NotNil(t, ev.GetGeometry(), "the advert supplies the location the report lacks")
	// Both inputs' telemetry rides on the one event.
	assert.InDelta(t, 4.5, ev.GetMesh().GetTelemetry().GetSnr().GetValue(), 0.001)
	assert.InDelta(t, 4.14, ev.GetMesh().GetTelemetry().GetAdmin().GetBatteryVolts().GetValue(), 0.001)
}

func TestAmbiguousPrefixDoesNotMerge(t *testing.T) {
	// Two known nodes share the reported prefix. Attaching a repeater's battery
	// reading to the WRONG repeater is worse than leaving it unattached, so the
	// report stays on its own prefix-keyed event.
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{
		{PubKey: keyPrefix + "1111111111111111111111111111111111111111111111aa", Role: meshcore.RoleRepeater,
			HasLocation: true, Lat: 38.1, Lng: -120.4, LastHeardAt: pollNow},
		{PubKey: keyPrefix + "2222222222222222222222222222222222222222222222bb", Role: meshcore.RoleRepeater,
			HasLocation: true, Lat: 38.2, Lng: -120.5, LastHeardAt: pollNow},
	}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Ambiguous", pollNow)},
	})
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Len(t, res.Events, 3, "two adverts plus the unresolved report")

	ids := map[string]bool{}
	for _, ev := range res.Events {
		ids[ev.GetId()] = true
	}
	assert.True(t, ids["meshcore:"+keyPrefix], "the report keeps its own prefix id")
}

// TestPrefixResolvesAgainstStoredEvents covers the MQTT-down / MQTT-disabled
// case: a full key, once learned, stays known through the store.
func TestPrefixResolvesAgainstStoredEvents(t *testing.T) {
	prior := &fakePrior{events: []*gridv1.Event{storedMeshEvent(fullKey, nil)}}
	n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
	})
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "meshcore:"+fullKey, res.Events[0].GetId())
}

// TestPromotionSupersedesPrefixEvent covers the handover: a node known only by
// prefix acquires its proper id once a bridge finally hears it.
func TestPromotionSupersedesPrefixEvent(t *testing.T) {
	prior := &fakePrior{events: []*gridv1.Event{storedMeshEvent(keyPrefix, nil)}}
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "Arnold Summit",
		HasLocation: true, Lat: 38.137412, Lng: -120.457934, LastHeardAt: pollNow,
	}}}
	n := pushNormalizer(t, reg, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
	})

	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "meshcore:"+fullKey, res.Events[0].GetId())
	// Positive evidence — we can name the successor — so the old id retires
	// immediately instead of drawing the same node twice for the whole grace.
	assert.Equal(t, []string{"meshcore:" + keyPrefix}, res.Superseded)
}

func TestReachabilityHysteresis(t *testing.T) {
	window := config.DefaultIngestUnreachableAfter
	cases := []struct {
		name    string
		success time.Time
		attempt time.Time
		want    gridv1.MeshReachability
	}{
		{"fresh success", pollNow.Add(-time.Minute), pollNow, gridv1.MeshReachability_REACHABLE},
		{"one missed poll", pollNow.Add(-window + time.Minute), pollNow, gridv1.MeshReachability_REACHABLE},
		{"sustained outage", pollNow.Add(-window - time.Minute), pollNow, gridv1.MeshReachability_UNREACHABLE},
		{"never reached", time.Time{}, pollNow, gridv1.MeshReachability_UNREACHABLE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
				Live: 1,
				Reports: []pushingest.MeshNodeReport{report(keyPrefix, "N", tc.success, func(r *pushingest.MeshNodeReport) {
					r.LastAttempt = tc.attempt
				})},
			})
			res, err := n.Poll(testCtx(), &fakePrior{})
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Events[0].GetMesh().GetReachability())
		})
	}

	t.Run("no monitor is not unreachable", func(t *testing.T) {
		// A node nobody checks must not render as one that is down.
		reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
			PubKey: fullKey, Role: meshcore.RoleRepeater,
			HasLocation: true, Lat: 38.1, Lng: -120.4, LastHeardAt: pollNow,
		}}}
		n := pushNormalizer(t, reg, pushingest.MeshSnapshot{Live: 1})
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		assert.Equal(t, gridv1.MeshReachability_MESH_REACHABILITY_UNSPECIFIED,
			res.Events[0].GetMesh().GetReachability())
	})
}

// TestTelemetryChurnMintsNoRevision is the property that makes a 5-minute
// reporting cadence affordable: moving counters must not change the content
// hash.
func TestTelemetryChurnMintsNoRevision(t *testing.T) {
	first := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
	})
	res1, err := first.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	// Same node, a later report: every counter has moved and the battery drifted.
	second := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live: 1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow, func(r *pushingest.MeshNodeReport) {
			r.Telemetry.BatteryVolts = wrapperspb.Double(3.97)
			r.Telemetry.PacketsSent = 150999
			r.ReportedAt = pollNow.Add(5 * time.Minute)
		})},
	})
	res2, err := second.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	assert.Equal(t, store.ContentHash(res1.Events[0]), store.ContentHash(res2.Events[0]),
		"telemetry is hash-excluded; a report that only moves counters must not mint a revision")
}

// TestReachabilityIsHashed is the deliberate exception to the rule above: a
// repeater going down IS history.
func TestReachabilityIsHashed(t *testing.T) {
	up := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
	})
	resUp, err := up.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	down := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live: 1,
		Reports: []pushingest.MeshNodeReport{
			report(keyPrefix, "Arnold", pollNow.Add(-2*config.DefaultIngestUnreachableAfter))},
	})
	resDown, err := down.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	assert.NotEqual(t, store.ContentHash(resUp.Events[0]), store.ContentHash(resDown.Events[0]),
		"a repeater going unreachable must be a revision, not silent state")
}

func TestForceWriteCoalescesTelemetryPersistence(t *testing.T) {
	interval := config.DefaultIngestTelemetryPersistInterval

	t.Run("new event is not forced", func(t *testing.T) {
		n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
			Live:    1,
			Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
		})
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		assert.Empty(t, res.ForceWrite, "a new event takes the normal write path anyway")
	})

	t.Run("fresh stored sample is not rewritten", func(t *testing.T) {
		stored := storedMeshEvent(keyPrefix, tsProto(pollNow.Add(-interval/2)))
		n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
			Live:    1,
			Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
		})
		res, err := n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{stored}})
		require.NoError(t, err)
		assert.Empty(t, res.ForceWrite, "coalescing is what keeps this from being a write per tick")
	})

	t.Run("stale stored sample is rewritten", func(t *testing.T) {
		stored := storedMeshEvent(keyPrefix, tsProto(pollNow.Add(-interval-time.Minute)))
		n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
			Live:    1,
			Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
		})
		res, err := n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{stored}})
		require.NoError(t, err)
		// Without this the hash-equal event would be skipped by shouldUpsert and
		// the battery reading would never reach the store at all.
		assert.Equal(t, []string{"meshcore:" + keyPrefix}, res.ForceWrite)
	})

	t.Run("first sample for an existing node is rewritten", func(t *testing.T) {
		stored := storedMeshEvent(keyPrefix, nil)
		n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
			Live:    1,
			Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
		})
		res, err := n.Poll(testCtx(), &fakePrior{events: []*gridv1.Event{stored}})
		require.NoError(t, err)
		assert.Equal(t, []string{"meshcore:" + keyPrefix}, res.ForceWrite)
	})
}

// TestFailLoudAcrossInputs pins the generalized invariant: hard-fail only when
// EVERY input is gone; suppress the sweep when some are.
func TestFailLoudAcrossInputs(t *testing.T) {
	t.Run("no broker and no live reporter is a hard error", func(t *testing.T) {
		n := pushNormalizer(t, &fakeMeshRegistry{connected: 0}, pushingest.MeshSnapshot{Live: 0})
		_, err := n.Poll(testCtx(), &fakePrior{})
		require.Error(t, err, "an empty snapshot from OUR outage must never read as an empty mesh")
	})

	t.Run("MQTT down but a reporter live still reports", func(t *testing.T) {
		n := pushNormalizer(t, &fakeMeshRegistry{connected: 0}, pushingest.MeshSnapshot{
			Live:          1,
			SuppressSweep: true,
			Reports:       []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
		})
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		assert.Len(t, res.Events, 1)
		// The nodes missing from this snapshot are missing for our reason.
		assert.Equal(t, []string{"meshcore"}, res.SweepSuppress)
	})

	t.Run("a stale reporter suppresses the sweep", func(t *testing.T) {
		reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
			PubKey: fullKey, Role: meshcore.RoleRepeater,
			HasLocation: true, Lat: 38.1, Lng: -120.4, LastHeardAt: pollNow,
		}}}
		n := pushNormalizer(t, reg, pushingest.MeshSnapshot{Live: 1, SuppressSweep: true})
		res, err := n.Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		assert.Equal(t, []string{"meshcore"}, res.SweepSuppress)
	})
}

func TestReporterHealthMapsToSourceRows(t *testing.T) {
	n := pushNormalizer(t, nil, pushingest.MeshSnapshot{
		Live: 1,
		Reporters: []pushingest.ReporterHealth{
			{ID: "ok-one", State: pushingest.ReporterOK},
			{ID: "never", State: pushingest.ReporterUnknown},
			{ID: "silent", State: pushingest.ReporterStale, LastAcceptedAt: pollNow.Add(-time.Hour)},
		},
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "Arnold", pollNow)},
	})
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)

	assert.NoError(t, res.PerSource["ok-one"])
	// A monitor that was never set up is not a healthy feed — RecordAttempt(nil)
	// would paint it green on /api/v1/sources.
	assert.Error(t, res.PerSource["never"])
	assert.Error(t, res.PerSource["silent"])
}

func TestSourceIDsIncludeReporters(t *testing.T) {
	n := pushNormalizer(t, nil, pushingest.MeshSnapshot{})
	assert.Equal(t, []string{"meshcore", "alan-pi"}, n.SourceIDs())
}

// storedMeshEvent fabricates what the store would hold for a node, optionally
// carrying a previously persisted admin sample stamped at reportedAt.
func storedMeshEvent(key string, reportedAt *timestamppb.Timestamp) *gridv1.Event {
	ev := NewEvent("meshcore:"+key, gridv1.Layer_MESH, gridv1.Severity_INFO,
		gridv1.EventStatus_ACTIVE, "stored")
	ev.Provenance = NewProvenance("meshcore", "MeshCore Mesh", "", "")
	mesh := &gridv1.MeshDetail{PublicKey: key, Telemetry: &gridv1.MeshTelemetry{}}
	if reportedAt != nil {
		mesh.Telemetry.Admin = &gridv1.MeshAdminTelemetry{ReporterId: "alan-pi", ReportedAt: reportedAt}
	}
	ev.Detail = &gridv1.Event_Mesh{Mesh: mesh}
	return ev
}
