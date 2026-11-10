package gridapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

func at(min int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute))
}

func rev(n uint32, ingested int, ev *gridv1.Event) *gridv1.EventRevision {
	c := proto.Clone(ev).(*gridv1.Event)
	c.Revision = n
	return &gridv1.EventRevision{Revision: n, IngestedAt: at(ingested), ObservedAt: at(ingested), Event: c}
}

func labels(t *gridv1.EventTimeline) []string {
	var out []string
	for _, e := range t.GetEntries() {
		out = append(out, e.GetLabel())
	}
	return out
}

// A CHP incident: the report time and each log line are the source's clock;
// first stored and resolved are the Grid's. Same instant: source first.
func TestTimelineCHPIncident(t *testing.T) {
	ev := &gridv1.Event{
		Id: "chp:1", Layer: gridv1.Layer_ROAD_INCIDENT, Status: gridv1.EventStatus_ACTIVE, Severity: gridv1.Severity_MINOR,
		Effective:  at(21),
		Provenance: &gridv1.Provenance{SourceName: "CHP / Caltrans"},
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{Log: []*gridv1.IncidentLogEntry{
			{Number: 1, At: at(21), Text: "1183-Trfc Collision-Unkn Inj"},
			{Number: 2, At: at(28), Text: "TOW ENRT"},
		}}},
	}
	resolved := proto.Clone(ev).(*gridv1.Event)
	resolved.Status = gridv1.EventStatus_RESOLVED
	resolved.Revision = 3
	tl := buildTimeline(resolved, []*gridv1.EventRevision{rev(3, 70, resolved), rev(1, 28, ev), rev(2, 41, ev)}, nil, false)

	assert.Equal(t, []string{"Reported", "1183-Trfc Collision-Unkn Inj", "TOW ENRT", "First recorded by the Grid", "Resolved"}, labels(tl))
	e := tl.GetEntries()
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, e[0].GetClock())
	assert.Equal(t, "CHP / Caltrans", e[0].GetDetail())
	assert.Equal(t, "Log line 2", e[2].GetDetail())
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, e[2].GetClock(), "at 19:28 the log line precedes the Grid's first record")
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_GRID, e[3].GetClock())
	assert.Equal(t, "3 revisions since", e[3].GetDetail())
	last := e[4]
	assert.Equal(t, gridv1.TimelineKind_TIMELINE_STATUS, last.GetKind())
	assert.Equal(t, uint32(3), last.GetRevision())
	assert.Equal(t, "Active", last.GetBefore())
	assert.Equal(t, "Resolved", last.GetAfter())
}

func TestTimelineLaneClosureAndFire(t *testing.T) {
	closure := &gridv1.Event{
		Id: "caltrans:d10-1", Layer: gridv1.Layer_ROAD_INCIDENT, Effective: at(0),
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{Closure: &gridv1.LaneClosureDetail{
			PlannedStart: at(0), SetUpAt: at(12), PlannedEnd: at(600),
		}}},
	}
	assert.Equal(t, []string{"Planned start", "Set up on the road", "Planned end"}, labels(buildTimeline(closure, nil, nil, false)))

	fire := &gridv1.Event{Id: "calfire:1", Layer: gridv1.Layer_WILDFIRE, Effective: at(0), Severity: gridv1.Severity_SEVERE,
		Detail: &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{Acres: 120, Containment: 0}}}
	grown := proto.Clone(fire).(*gridv1.Event)
	grown.GetWildfire().Acres, grown.GetWildfire().Containment = 450.25, 15
	tl := buildTimeline(grown, []*gridv1.EventRevision{rev(1, 5, fire), rev(2, 90, grown)}, nil, false)
	assert.Equal(t, []string{"Started", "First recorded by the Grid", "Size 450.3 acres", "Containment 15%"}, labels(tl))
	assert.Equal(t, "120 acres", tl.GetEntries()[2].GetBefore())
}

// Truncated history: no change entry across the gap, and FIRST_RECORDED
// still comes from revision 1 when the caller supplies it.
func TestTimelineTruncated(t *testing.T) {
	ev := &gridv1.Event{Id: "x", Layer: gridv1.Layer_EARTHQUAKE, Effective: at(0), Revision: 900}
	other := proto.Clone(ev).(*gridv1.Event)
	other.Severity = gridv1.Severity_SEVERE
	tl := buildTimeline(ev, []*gridv1.EventRevision{rev(899, 50, other), rev(700, 40, ev)}, rev(1, 1, ev), true)
	assert.True(t, tl.GetTruncated())
	assert.Equal(t, []string{"Occurred", "First recorded by the Grid"}, labels(tl))
}

func TestGetEventTimelineRPC(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)
	tl, err := g.GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "caltrans:d10-1"})
	require.NoError(t, err)
	require.NotEmpty(t, tl.GetEntries())
	assert.Equal(t, "caltrans:d10-1", tl.GetEventId())
	_, err = g.GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "nope:1"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
