package gridapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dpup/prefab/plugins/etag"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// timelineRevisionCap bounds the revisions a timeline reads: a busy mesh
// node or a long fire can have hundreds, each a full snapshot.
const timelineRevisionCap = 500

// GetEventTimeline returns one event's timeline (see buildTimeline).
func (g *GridServer) GetEventTimeline(ctx context.Context, req *gridv1.GetEventTimelineRequest) (*gridv1.EventTimeline, error) {
	stamp, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	tag := fmt.Sprintf("%s:timeline:%s:%d.%d", g.placesVersion, req.GetId(), stamp.Revision, stamp.Refreshes)
	var activity map[string]store.MeshActivity
	if native, ok := strings.CutPrefix(req.GetId(), meshActivityPrefix); ok {
		if activity, err = g.svc.Store.MeshActivityFor(ctx, []string{native}); err != nil {
			logError(ctx, "gridapi: reading mesh activity", err)
			activity = nil
		}
		tag += ":" + meshActivityTag(activity[strings.ToLower(native)])
	}
	if err := etag.Guard(ctx, etag.Weak(tag)); err != nil {
		return nil, err
	}
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	applyMeshActivity([]*gridv1.Event{ev}, activity)

	var revs []*gridv1.EventRevision
	token, truncated := "", false
	for {
		page, err := g.svc.Store.EventHistoryPage(ctx, req.GetId(), store.MaxPageSize, token)
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		revs = append(revs, page.Items...)
		token = page.Next(len(page.Items))
		if token == "" {
			break
		}
		if len(revs) >= timelineRevisionCap {
			truncated = true
			break
		}
	}
	var first *gridv1.EventRevision
	if truncated {
		// FIRST_RECORDED always comes from revision 1, even past the cap.
		if p, err := g.svc.Store.EventRevisionAt(ctx, req.GetId(), 1); err == nil {
			first = p
		}
	}
	return buildTimeline(ev, revs, first, truncated), nil
}

// buildTimeline derives the timeline from the current event and its
// revisions (any order). first, when non-nil, is revision 1 fetched
// separately because revs was truncated.
//
// The rule that makes it worth having: SOURCE entries only ever come from
// fields the upstream stated, and GRID entries only from revision ingest
// times. A client used to guess a report time from whichever timestamp was
// earliest, which mixed the two clocks.
func buildTimeline(ev *gridv1.Event, revs []*gridv1.EventRevision, first *gridv1.EventRevision, truncated bool) *gridv1.EventTimeline {
	t := &gridv1.EventTimeline{EventId: ev.GetId(), Truncated: truncated}
	src := func(at *timestamppb.Timestamp, kind gridv1.TimelineKind, label, detail string) {
		if at == nil || at.AsTime().IsZero() || at.AsTime().Unix() <= 0 {
			return
		}
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{At: at, Kind: kind, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, Label: label, Detail: detail})
	}
	who := ev.GetProvenance().GetSourceName()

	switch ev.GetLayer() {
	case gridv1.Layer_ROAD_INCIDENT:
		ri := ev.GetRoadIncident()
		if c := ri.GetClosure(); c != nil {
			src(c.GetPlannedStart(), gridv1.TimelineKind_TIMELINE_PLANNED_START, "Planned start", who)
			src(c.GetSetUpAt(), gridv1.TimelineKind_TIMELINE_SET_UP, "Set up on the road", who)
			src(c.GetPlannedEnd(), gridv1.TimelineKind_TIMELINE_PLANNED_END, "Planned end", who)
		} else {
			src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Reported", who)
		}
		for _, l := range ri.GetLog() {
			src(l.GetAt(), gridv1.TimelineKind_TIMELINE_LOG, l.GetText(), "Log line "+strconv.Itoa(int(l.GetNumber())))
		}
	case gridv1.Layer_WILDFIRE:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Started", who)
	case gridv1.Layer_EARTHQUAKE:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Occurred", who)
	case gridv1.Layer_EVACUATION, gridv1.Layer_WEATHER_ALERT:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "In effect", who)
		src(ev.GetExpires(), gridv1.TimelineKind_TIMELINE_ENDS, "Ends", who)
	case gridv1.Layer_POWER:
		p := ev.GetPower()
		if p.GetDeEnergizationStart() != nil || p.GetDeEnergizationEnd() != nil {
			src(p.GetDeEnergizationStart(), gridv1.TimelineKind_TIMELINE_PLANNED_START, "De-energization starts", who)
			src(p.GetDeEnergizationEnd(), gridv1.TimelineKind_TIMELINE_PLANNED_END, "De-energization ends", who)
			src(p.GetAllClear(), gridv1.TimelineKind_TIMELINE_ALL_CLEAR, "All clear", who)
		} else {
			src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Outage began", who)
		}
		src(p.GetEstimatedRestoration(), gridv1.TimelineKind_TIMELINE_ESTIMATE, "Estimated restoration", who+" estimate; often revised")
	case gridv1.Layer_MESH:
		tel := ev.GetMesh().GetTelemetry()
		src(tel.GetLastAdvertAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last advert", "")
		src(tel.GetLastHeardAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last heard", "")
		src(tel.GetLastRelayedAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last relayed", "")
		a := tel.GetAdmin()
		src(a.GetLastSuccessAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Monitor last reached it", a.GetReporterId())
		if a.GetLastAttemptAt() != nil && !a.GetLastAttemptAt().AsTime().Equal(a.GetLastSuccessAt().AsTime()) {
			src(a.GetLastAttemptAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Monitor last tried", a.GetReporterId())
		}
	default:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "In effect", who)
	}

	ordered := append([]*gridv1.EventRevision(nil), revs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GetRevision() < ordered[j].GetRevision() })
	if first == nil && len(ordered) > 0 && ordered[0].GetRevision() == 1 {
		first = ordered[0]
	}
	if first != nil {
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{
			At: first.GetIngestedAt(), Kind: gridv1.TimelineKind_TIMELINE_FIRST_RECORDED, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_GRID,
			Label: "First recorded by the Grid", Revision: 1,
			Detail: plural(int(ev.GetRevision()), "revision") + " since",
		})
	}
	for i := 1; i < len(ordered); i++ {
		prev, cur := ordered[i-1], ordered[i]
		if cur.GetRevision() != prev.GetRevision()+1 {
			continue // a gap (truncation): no honest before value
		}
		for _, c := range revisionChanges(prev.GetEvent(), cur.GetEvent()) {
			c.At, c.Clock, c.Revision = cur.GetIngestedAt(), gridv1.TimelineClock_TIMELINE_CLOCK_GRID, cur.GetRevision()
			t.Entries = append(t.Entries, c)
		}
	}

	sort.SliceStable(t.Entries, func(i, j int) bool {
		a, b := t.Entries[i].GetAt().AsTime(), t.Entries[j].GetAt().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return t.Entries[i].GetClock() < t.Entries[j].GetClock() // SOURCE before GRID
	})
	return t
}

// revisionChanges lists the tracked values that differ between consecutive
// revisions: status, severity, and per layer the figures a reader follows.
func revisionChanges(a, b *gridv1.Event) []*gridv1.TimelineEntry {
	var out []*gridv1.TimelineEntry
	diff := func(kind gridv1.TimelineKind, field, label, before, after string) {
		if before == after {
			return
		}
		out = append(out, &gridv1.TimelineEntry{Kind: kind, Field: field, Label: label, Before: before, After: after, Detail: "was " + nonEmptyOr(before, "not given")})
	}
	if a.GetStatus() != b.GetStatus() {
		diff(gridv1.TimelineKind_TIMELINE_STATUS, "status", statusLabel(a.GetStatus(), b.GetStatus()), enumWord(a.GetStatus().String()), enumWord(b.GetStatus().String()))
	}
	diff(gridv1.TimelineKind_TIMELINE_CHANGED, "severity", "Severity "+enumWord(b.GetSeverity().String()), enumWord(a.GetSeverity().String()), enumWord(b.GetSeverity().String()))
	switch b.GetLayer() {
	case gridv1.Layer_WILDFIRE:
		fa, fb := a.GetWildfire(), b.GetWildfire()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "acres", "Size "+acresText(fb.GetAcres()), acresText(fa.GetAcres()), acresText(fb.GetAcres()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "containment", "Containment "+pct(fb.GetContainment()), pct(fa.GetContainment()), pct(fb.GetContainment()))
	case gridv1.Layer_EVACUATION:
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "level", "Level "+enumWord(b.GetEvacuation().GetLevel()), enumWord(a.GetEvacuation().GetLevel()), enumWord(b.GetEvacuation().GetLevel()))
	case gridv1.Layer_POWER:
		ca, cb := a.GetPower().GetCustomersAffected(), b.GetPower().GetCustomersAffected()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "customersAffected", countText(cb)+" customers out", countText(ca), countText(cb))
	}
	return out
}

func statusLabel(from, to gridv1.EventStatus) string {
	switch to {
	case gridv1.EventStatus_RESOLVED:
		return "Resolved"
	case gridv1.EventStatus_EXPIRED:
		return "Expired"
	case gridv1.EventStatus_ACTIVE:
		if from == gridv1.EventStatus_SCHEDULED {
			return "Now in effect"
		}
		return "Active again"
	}
	return enumWord(to.String())
}

// enumWord renders an enum name for display: "RESOLVED" -> "Resolved",
// "BURN_DAY_YES" stays readable as "Burn day yes".
func enumWord(s string) string {
	if s == "" || strings.HasSuffix(s, "UNSPECIFIED") {
		return ""
	}
	w := strings.ToLower(strings.ReplaceAll(s, "_", " "))
	return strings.ToUpper(w[:1]) + w[1:]
}

func nonEmptyOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func acresText(v float64) string {
	if v <= 0 {
		return ""
	}
	return strconv.FormatFloat(roundTo(v, 1), 'f', -1, 64) + " acres"
}

func pct(v int32) string { return strconv.Itoa(int(v)) + "%" }

func countText(v int32) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(int(v))
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}
