package caltrans

import (
	"fmt"
	"strings"
	"time"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// IncidentFromCWWP2LaneClosure adapts one CWWP2 lane-closure window to the
// CaltransIncident shape lcs2way.kml produced, so the roads service's segment
// status (route matching, the Closure-ID alert id, the AI status call) works
// on it unchanged. The caller decides which windows are closures right now
// (cwwp2.LaneClosure.PhaseAt == PhaseActive); this only reshapes one.
//
// The text is built from fields that do not change while a window stands —
// never the fetch time or the file stamp — because title and description are
// the AI enhancement's cache key: unstable text would mean an OpenAI call per
// closure per refresh.
//
// Geometry is the begin point, with both endpoints as the affected polyline.
// The route matcher checks each polyline point against the road, so no
// begin→end chord is ever drawn.
func IncidentFromCWWP2LaneClosure(lc cwwp2.LaneClosure, fetched time.Time) CaltransIncident {
	var pts []*api.Coordinates
	for _, l := range []cwwp2.Location{lc.Begin, lc.End} {
		if l.HasPosition {
			pts = append(pts, &api.Coordinates{Latitude: l.Latitude, Longitude: l.Longitude})
		}
	}
	inc := CaltransIncident{
		FeedType:    LANE_CLOSURE,
		Name:        cwwp2ClosureTitle(lc),
		StyleUrl:    "#lcs",
		LastFetched: fetched,
	}
	if len(pts) > 0 {
		inc.Coordinates = pts[0]
	}
	if len(pts) > 1 {
		inc.AffectedArea = &api.Polyline{Points: pts}
	}
	inc.DescriptionText = cwwp2ClosureText(lc)
	inc.DescriptionHtml = inc.DescriptionText
	return inc
}

// cwwp2ClosureTitle names the closure the way lcs2way.kml titled it
// ("Route 4 One-way Traffic Operation").
func cwwp2ClosureTitle(lc cwwp2.LaneClosure) string {
	route := lc.Begin.Route
	if route == "" {
		route = lc.End.Route
	}
	for _, p := range []string{"SR-", "US-", "I-"} {
		route = strings.TrimPrefix(route, p)
	}
	var what string
	switch strings.ToLower(strings.TrimSpace(lc.ClosureType)) {
	case "full":
		what = "Full Closure"
	case "lane", "":
		what = "Lane Closure"
	case "one-way traffic":
		what = "One-way Traffic Operation"
	case "alternating lanes":
		what = "Alternating Lane Closure"
	case "moving":
		what = "Moving Lane Closure"
	default:
		what = strings.TrimSpace(lc.ClosureType) + " Closure"
	}
	if route == "" {
		return what
	}
	return "Route " + route + " " + what
}

// cwwp2ClosureText is the description the enhancer reads, e.g. "Closure ID:
// C26EA, Log Number: 1. SR-26 (East / West): Full closure from Route 49
// Mokelumne Hill to Main Street (Left), near Mokelumne Hill, Calaveras
// County. Due to Roadway Excavation. Lanes closed: All of 2. ...".
func cwwp2ClosureText(lc cwwp2.LaneClosure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Closure ID: %s, Log Number: %s.", lc.ClosureID, lc.LogNumber)

	route := lc.Begin.Route
	if route == "" {
		route = lc.End.Route
	}
	b.WriteString(" ")
	if route != "" {
		b.WriteString(route)
		if lc.FlowDirection != "" {
			b.WriteString(" (" + lc.FlowDirection + ")")
		}
		b.WriteString(": ")
	}
	kind := strings.TrimSpace(lc.ClosureType)
	if kind == "" {
		kind = "Lane"
	}
	b.WriteString(kind + " closure")
	if lc.Facility != "" {
		b.WriteString(" on " + lc.Facility)
	}
	from, to := strings.TrimSpace(lc.Begin.Name), strings.TrimSpace(lc.End.Name)
	switch {
	case from == "" && to == "":
	case from == "" || to == "" || strings.EqualFold(from, to):
		if from == "" {
			from = to
		}
		b.WriteString(" at " + from)
	default:
		b.WriteString(" from " + from + " to " + to)
	}
	if near := lc.Begin.NearbyPlace; near != "" {
		b.WriteString(", near " + near)
	}
	if county := lc.Begin.County; county != "" {
		b.WriteString(", " + county + " County")
	}
	b.WriteString(".")
	if lc.WorkType != "" {
		b.WriteString(" Due to " + lc.WorkType + ".")
	}
	if lc.LanesClosed != "" {
		b.WriteString(" Lanes closed: " + lc.LanesClosed)
		if lc.TotalLanes > 0 {
			fmt.Fprintf(&b, " of %d", lc.TotalLanes)
		}
		b.WriteString(".")
	}
	if lc.EstimatedDelay > 0 {
		fmt.Fprintf(&b, " Estimated delay: %d min.", lc.EstimatedDelay)
	}
	if lc.SetUp && !lc.SetUpAt.IsZero() {
		b.WriteString(" Closure in place since " + pacificStamp(lc.SetUpAt) + ".")
	}
	switch {
	case lc.EndIndefinite:
		b.WriteString(" No scheduled end.")
	case !lc.EndTime.IsZero():
		b.WriteString(" Expected to end at " + pacificStamp(lc.EndTime) + ".")
	}
	return b.String()
}

func pacificStamp(t time.Time) string {
	return t.In(pacific).Format("3:04pm Jan 2, 2006")
}

// pacific is the zone Caltrans writes local times in (cmd/server blank-imports
// time/tzdata, so it resolves in a minimal container).
var pacific = func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.UTC
}()
