package services

import (
	"strings"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// segmentPlaces is what locates a text-only road condition relative to one
// monitored road: its own places, and whether anything is monitored past
// each of its ends.
type segmentPlaces struct {
	caltrans.SegmentPlaces
	// firstInChain and lastInChain: no monitored road of the same highway
	// continues before the origin / past the destination. Only such a road
	// reports a condition lying past that end (Road.beyond); otherwise the
	// next road along either carries it or reports it itself.
	firstInChain, lastInChain bool
}

// roadEnds splits "Arnold to Bear Valley" into its two ends.
func roadEnds(section string) (origin, destination string) {
	p := caltrans.SectionPlaces(section, nil, nil, nil)
	return p.Origin, p.Destination
}

// placesFor builds the places for one monitored road. Monitored roads of the
// same highway that chain end to start (Angels Camp to Murphys, Murphys to
// Arnold, Arnold to Bear Valley) tell each other what lies past their ends:
// everything on the roads after this one is past its destination, everything
// on the roads before it is before its origin. So "closed from Arnold to Bear
// Valley" does not close Murphys to Arnold, and nobody has to list the
// neighbours' towns by hand.
func placesFor(road config.MonitoredRoad, all []config.MonitoredRoad) segmentPlaces {
	origin, destination := roadEnds(road.Section)
	p := segmentPlaces{SegmentPlaces: caltrans.SegmentPlaces{
		Origin:      origin,
		Destination: destination,
		On:          road.LocationKeywords,
		BeforeStart: append([]string{}, road.BeforeStartKeywords...),
		PastEnd:     append([]string{}, road.PastEndKeywords...),
	}}

	same := func(a, b string) bool { return a != "" && strings.EqualFold(a, b) }
	neighbour := func(at string, forward bool, seen map[string]bool) *config.MonitoredRoad {
		for i := range all {
			o := &all[i]
			if seen[o.ID] || !strings.EqualFold(strings.TrimSpace(o.Name), strings.TrimSpace(road.Name)) {
				continue
			}
			oOrigin, oDestination := roadEnds(o.Section)
			if (forward && same(oOrigin, at)) || (!forward && same(oDestination, at)) {
				return o
			}
		}
		return nil
	}

	seen := map[string]bool{road.ID: true}
	p.lastInChain = true
	for at := destination; ; {
		next := neighbour(at, true, seen)
		if next == nil {
			break
		}
		seen[next.ID] = true
		p.lastInChain = false
		_, at = roadEnds(next.Section)
		p.PastEnd = append(p.PastEnd, at)
		p.PastEnd = append(p.PastEnd, next.LocationKeywords...)
		p.PastEnd = append(p.PastEnd, next.PastEndKeywords...)
	}
	p.firstInChain = true
	for at := origin; ; {
		prev := neighbour(at, false, seen)
		if prev == nil {
			break
		}
		seen[prev.ID] = true
		p.firstInChain = false
		at, _ = roadEnds(prev.Section)
		p.BeforeStart = append(p.BeforeStart, at)
		p.BeforeStart = append(p.BeforeStart, prev.LocationKeywords...)
		p.BeforeStart = append(p.BeforeStart, prev.BeforeStartKeywords...)
	}
	return p
}
