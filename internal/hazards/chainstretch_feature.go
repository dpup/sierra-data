package hazards

import (
	"fmt"
	"strings"
	"time"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
)

// isChainLevel reports a chain requirement level. cc.kml's truck and hold
// levels (MAX, MIN, TS, HT, TTA) parse to no level at all.
func isChainLevel(level string) bool {
	return level == "R1" || level == "R2" || level == "R3"
}

func anySign(signs []caltrans.ChainControlData, pred func(caltrans.ChainControlData) bool) bool {
	for _, c := range signs {
		if pred(c) {
			return true
		}
	}
	return false
}

// chainStretchID is the feature id of stretches[i]: "cc:<route>:<level>",
// e.g. "cc:hwy4:r2".
//
// The id names the requirement, not the signs, so it holds while the stretch
// grows or shrinks by a checkpoint, and while a sign's own status flaps. Its
// limits:
//
//   - A level change is a new id (cc:hwy4:r2 -> cc:hwy4:r3): it is a
//     different requirement.
//   - A route with two separate stretches at one level numbers them along the
//     road from the west or south: the first keeps the bare id, the next is
//     "cc:hwy88:r2:2". A stretch that splits, merges, or gains a neighbour
//     before it on the road therefore renumbers.
//   - The stretches are numbered over everything Caltrans reported, not per
//     area, so an id means the same stretch in every place.
//
// A sign with no highway cannot be grouped and keeps its own sign id.
func chainStretchID(stretches []chainStretch, i int) string {
	st := stretches[i]
	if strings.TrimSpace(st.Highway) == "" {
		return chainControlID(st.Signs[0])
	}
	n := 1
	for _, other := range stretches[:i] {
		if other.Level == st.Level && sameHighway(other.Highway, st.Highway) {
			n++
		}
	}
	id := "cc:" + highwayKey(st.Highway) + ":" + strings.ToLower(st.Level)
	if n > 1 {
		id += fmt.Sprintf(":%d", n)
	}
	return id
}

// chainStretchFeature projects a stretch as one feature. ok is false only
// when no sign in it has a position, so there is nowhere to put it.
//
// The geometry is a Point at the stretch's low end (From). Not a LineString
// through the signs: straight lines between checkpoints are not the road
// (Hwy 4 climbs 600 m between Poison Springs and Bear Valley in switchbacks),
// and a drawn line reads as a measured extent. Not a MultiPoint either: every
// reader of this layer takes a Point, and one that skips other geometry types
// would show "no chain controls" in a storm. The signs' own positions are in
// chainControl.signs for a client that wants to draw each checkpoint.
func chainStretchFeature(st chainStretch, id string) (Feature, bool) {
	var at *caltrans.ChainControlData
	for i := range st.Signs {
		if st.Signs[i].Coordinates != nil {
			at = &st.Signs[i]
			break
		}
	}
	if at == nil {
		return Feature{}, false
	}
	first, last := st.Signs[0], st.Signs[len(st.Signs)-1]
	from, to := chainPlace(first), chainPlace(last)

	cc := &ChainControlProps{Level: st.Level, Highway: st.Highway, From: &from, To: &to, Inferred: st.Inferred}
	direction, oneWay := first.Direction, true
	var description, effective string
	var earliest time.Time
	var sources []string
	for _, c := range st.Signs {
		oneWay = oneWay && strings.EqualFold(c.Direction, direction)
		if description == "" {
			description = c.Description
		}
		if t, err := time.Parse(time.RFC3339, c.EffectiveTime); err == nil && (earliest.IsZero() || t.Before(earliest)) {
			earliest, effective = t, c.EffectiveTime
		}
		source := c.Source
		if source == "" {
			source = caltrans.SourceQuickMap
		}
		if !containsString(sources, source) {
			sources = append(sources, source)
		}
		sign := ChainSign{
			ID: chainControlID(c), Place: c.LocationName, Direction: c.Direction, Level: c.Level,
			Since: c.EffectiveTime, Source: source, County: c.County, Postmile: c.Postmile,
		}
		if c.Coordinates != nil {
			sign.Coordinates = lonLat(c.Coordinates.Latitude, c.Coordinates.Longitude)
		}
		cc.Signs = append(cc.Signs, sign)
	}
	if oneWay {
		cc.Direction = direction
	}

	where, label := ", "+from.Place+" to "+to.Place, from.Place+" to "+to.Place
	if strings.EqualFold(from.Place, to.Place) {
		where, label = " at "+from.Place, from.Place
		if oneWay && direction != "" {
			where += ", " + strings.ToLower(direction)
		}
	}
	p := Properties{
		ID:           id,
		Layer:        strings.ToUpper(LayerChainControl),
		Kind:         "Chain control",
		Category:     strings.ToLower(st.Level),
		Headline:     strings.TrimSpace(fmt.Sprintf("%s %s%s (%s)", shortHighway(st.Highway), chainRequirement(st.Level), where, st.Level)),
		Description:  description,
		AreaLabel:    label,
		Effective:    effective,
		Source:       Source{ID: "caltrans", Name: "Caltrans", Attribution: strings.Join(sources, " · ")},
		ChainControl: cc,
	}
	p.setSeverity(fromChainLevelStr(st.Level))
	return Feature{Type: "Feature", Geometry: PointGeom(at.Coordinates.Latitude, at.Coordinates.Longitude), Properties: p}, true
}

func chainPlace(c caltrans.ChainControlData) ChainPlace {
	p := ChainPlace{Place: placeName(c.LocationName)}
	if c.Coordinates != nil {
		p.Coordinates = lonLat(c.Coordinates.Latitude, c.Coordinates.Longitude)
	}
	return p
}

// chainRequirement is the level in a driver's words. R1 lets snow tires
// through; R3 lets nothing through without chains.
func chainRequirement(level string) string {
	switch level {
	case "R1":
		return "chains or snow tires required"
	case "R3":
		return "chains required on all vehicles"
	default:
		return "chains required"
	}
}

// shortHighway is the highway as the road_segment layer writes it:
// "Highway 4" -> "Hwy 4", "Interstate 80" -> "I-80".
func shortHighway(highway string) string {
	h := strings.TrimSpace(highway)
	switch lower := strings.ToLower(h); {
	case strings.HasPrefix(lower, "highway "):
		return "Hwy " + strings.TrimSpace(h[len("highway "):])
	case strings.HasPrefix(lower, "interstate "):
		return "I-" + strings.TrimSpace(h[len("interstate "):])
	}
	return h
}

// placeName title-cases a checkpoint name CWWP2 wrote in capitals ("POISON
// SPRINGS" -> "Poison Springs", "PICKETT'S JUNCTION" -> "Pickett's
// Junction"). A name with any lower case in it (cc.kml's Districts 3 and 9)
// is already written for reading and is left alone, less the sign-face
// layout District 9 appends ("Bodie Flats / 8 Mi. S of Bridgeport_ _ _ (20)6").
func placeName(name string) string {
	if i := strings.Index(name, "_"); i > 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	if name != strings.ToUpper(name) {
		return name
	}
	var b strings.Builder
	start := true
	for _, r := range strings.ToLower(name) {
		if start {
			b.WriteString(strings.ToUpper(string(r)))
		} else {
			b.WriteRune(r)
		}
		start = r == ' ' || r == '(' || r == '/' || r == '-' || r == '.'
	}
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
