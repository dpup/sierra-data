package hazards

import (
	"math"
	"strings"

	api "github.com/sierragridteam/sierra-data/api/v1"
)

// causeMaxOffsetMeters is how far from the segment line a cause may sit and
// still be placed on it. The roads service only applies ON_ROUTE alerts
// (within ~100 m), so this is generous slack for the begin/end of a closure.
const causeMaxOffsetMeters = 1000

// roadStatusCauses projects the roads service's status causes onto the
// segment's path (internal lat/lng order, origin first).
func roadStatusCauses(rd *api.Road, path []LatLng) []RoadStatusCauseProps {
	if rd.GetStatus() == api.RoadStatus_OPEN {
		return nil
	}
	var out []RoadStatusCauseProps
	for _, c := range rd.GetStatusCauses() {
		p := RoadStatusCauseProps{
			Status:      strings.TrimPrefix(c.GetStatus().String(), "ROAD_STATUS_"),
			Source:      c.GetSource(),
			SourceID:    c.GetSourceId(),
			Title:       c.GetTitle(),
			Description: c.GetDescription(),
			Extent:      "UNKNOWN",
		}
		if c.GetSource() == "incident" && c.GetSourceId() != "" {
			p.EventID = "chp:" + c.GetSourceId()
		}
		var from, to *float64
		if b := c.GetBegin(); b != nil {
			p.Begin = []float64{b.GetLongitude(), b.GetLatitude()}
			if km, off := alongPath(path, b.GetLatitude(), b.GetLongitude()); off <= causeMaxOffsetMeters {
				from = &km
			}
		}
		if e := c.GetEnd(); e != nil {
			p.End = []float64{e.GetLongitude(), e.GetLatitude()}
			if km, off := alongPath(path, e.GetLatitude(), e.GetLongitude()); off <= causeMaxOffsetMeters {
				to = &km
			}
		}
		// A closure with one end on the segment and the other off it runs
		// past the segment: it covers the segment from the placed end to
		// whichever segment end is nearer the other point. The roads service
		// applies a closure when either end is on the route, so this is the
		// usual shape of a multi-km closure, not an edge case.
		if from != nil && to == nil && c.GetEnd() != nil {
			to = nearerEndKm(path, c.GetEnd().GetLatitude(), c.GetEnd().GetLongitude())
		} else if to != nil && from == nil && c.GetBegin() != nil {
			from = nearerEndKm(path, c.GetBegin().GetLatitude(), c.GetBegin().GetLongitude())
		}
		switch {
		case from != nil && to != nil:
			lo, hi := math.Min(*from, *to), math.Max(*from, *to)
			p.FromKm, p.ToKm, p.Extent = &lo, &hi, "SPAN"
		case from != nil && c.GetEnd() == nil:
			p.FromKm, p.ToKm, p.Extent = from, from, "POINT"
		}
		out = append(out, p)
	}
	return out
}

// roadBeyond projects the closures and restrictions past the segment's ends.
func roadBeyond(rd *api.Road) []RoadBeyondProps {
	var out []RoadBeyondProps
	for _, b := range rd.GetBeyond() {
		out = append(out, RoadBeyondProps{
			Status:      strings.TrimPrefix(b.GetStatus().String(), "ROAD_STATUS_"),
			Side:        b.GetSide(),
			Source:      b.GetSource(),
			Description: b.GetDescription(),
			Reason:      b.GetReason(),
			From:        b.GetFrom(),
			To:          b.GetTo(),
		})
	}
	return out
}

// nearerEndKm is the km position (0 or the path's length) of whichever end
// of path is nearer (lat, lng); nil for a path too short to have two.
func nearerEndKm(path []LatLng, lat, lng float64) *float64 {
	if len(path) < 2 {
		return nil
	}
	first, last := path[0], path[len(path)-1]
	_, dFirst := alongPath([]LatLng{first, first}, lat, lng)
	_, dLast := alongPath([]LatLng{last, last}, lat, lng)
	if dFirst <= dLast {
		v := 0.0
		return &v
	}
	v, _ := alongPath(path, last.Lat, last.Lng)
	return &v
}

// alongPath returns how far along path (km from its first point) the point
// nearest (lat, lng) is, and how far off the path that point is (metres).
// Equirectangular per segment: segments are short and the service area small.
func alongPath(path []LatLng, lat, lng float64) (km, offsetM float64) {
	if len(path) < 2 {
		return 0, math.Inf(1)
	}
	const r = 6371000.0
	kx := math.Cos(lat * math.Pi / 180)
	xy := func(p LatLng) (float64, float64) {
		return p.Lng * math.Pi / 180 * r * kx, p.Lat * math.Pi / 180 * r
	}
	px, py := xy(LatLng{Lat: lat, Lng: lng})
	best, bestAlong, walked := math.Inf(1), 0.0, 0.0
	for i := 0; i+1 < len(path); i++ {
		ax, ay := xy(path[i])
		bx, by := xy(path[i+1])
		dx, dy := bx-ax, by-ay
		l2 := dx*dx + dy*dy
		t := 0.0
		if l2 > 0 {
			t = math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/l2))
		}
		cx, cy := ax+t*dx, ay+t*dy
		if d := math.Hypot(px-cx, py-cy); d < best {
			best, bestAlong = d, walked+t*math.Sqrt(l2)
		}
		walked += math.Sqrt(l2)
	}
	return math.Round(bestAlong/100) / 10, best
}
