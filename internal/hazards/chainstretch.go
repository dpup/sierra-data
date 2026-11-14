package hazards

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
)

// A chain-control STRETCH is what a driver meets on the road: one run of
// highway where one requirement level is in force. Caltrans reports it sign
// by sign, one row per checkpoint and direction, so a single stretch arrived
// as seven features with one headline. groupChainStretches puts the signs
// back together. It is pure: no clock, no I/O, no area.
//
// The rule. Two signs belong to one stretch when they are on the same
// highway, at the same level, and the road between them is under that
// requirement. How "between" is established depends on what is known:
//
//  1. SIGNED (both signs carry a CWWP2 milepost). A sign speaks for its own
//     direction of travel from where it stands to the next sign facing the
//     same way: an eastbound sign at mile 10 covers eastbound traffic until
//     the eastbound sign at mile 20 says something else. Mileposts rise
//     eastbound and northbound, so an Eastbound/Northbound sign covers
//     [its milepost, the next such sign] and a Westbound/Southbound one
//     [the previous such sign, its milepost]. The last sign in its direction
//     covers to the end of the signed route. Signs whose covered spans
//     overlap, or come within chainTouchMiles of each other, are one stretch.
//
//     This is why the R-0 sign facing the other way at the edge of a stretch
//     (Poison Springs westbound while Poison Springs eastbound reads R-2) is
//     not a gap: it ends the westbound requirement there, and the eastbound
//     one starts at the same spot. A real gap is road that neither direction's
//     signs put under the requirement.
//
//     Pass-closure gates are left out of the walk altogether. A gate reports
//     R-0 or "ROAD CLOSED" whatever the chain sign a metre away says.
//
//  2. INFERRED (either sign has no milepost: every cc.kml entry, so all of
//     Districts 3 and 9, and everything when CWWP2 is down). cc.kml lists
//     active controls only, so nothing reports where a stretch ends. Signs
//     within chainJoinMeters of each other in a straight line are taken as
//     one stretch, and the stretch says so (Inferred).
//
// Different levels are different stretches, even end to end (Hwy 88: R2 from
// Lumber Yard to Red Lake Creek, then R1 at Pickett's Junction). A driver
// with snow tires and no chains may take the R1 part and not the R2 part, so
// one feature covering both would have to either understate the R2 or
// overstate the R1, and could not say where the requirement changes.
//
// Never dropped: every sign handed in comes back in exactly one stretch. A
// sign that cannot be joined to anything (no highway, no milepost and no
// position, no neighbour) is a stretch of one.
type chainStretch struct {
	Highway string
	Level   string // R1 | R2 | R3
	// Signs run from the low end to the high end: by milepost when every sign
	// has one (west to east, south to north, as California counts them), by
	// distance from the western or southern end otherwise.
	Signs []caltrans.ChainControlData
	// Inferred: at least one join came from proximity, not from the signs.
	Inferred bool
}

// chainJoinMeters is how far apart two signs may be and still be taken as one
// stretch when nothing reports the road between them (rule 2). A judgment,
// not a measurement: in the 2026-11-13 storm the widest gap inside a stretch
// CWWP2 confirms was 11.5 km in a straight line (Hwy 108, Clarks Fork Road to
// Kennedy Meadows) and I-80's eastbound signs sit up to 13.6 km apart, while
// the US 395 controls at Crestview and Conway Summit, separate passes, are
// 34 km apart.
const chainJoinMeters = 15000

// chainTouchMiles is how close two signed spans (rule 1) must come to be one
// stretch. The two signs of one checkpoint do not stand at one spot: the
// westbound sign is usually the lower one, up to 1.1 miles below its
// eastbound twin (Hathaway Pines, Hwy 4; Lumber Yard on Hwy 88 is 1.0), and
// the road between the pair belongs to neither span.
const chainTouchMiles = 1.5

// groupChainStretches groups active chain-control signs into stretches.
//
// active must hold only chain requirements: a recognized R1..R3 level, not a
// closure. quiet is every other CWWP2 checkpoint that reported a status
// (R-0, or one we could not read): they are where a direction's requirement
// ends. The result is ordered by highway, then level, then along the road.
func groupChainStretches(active, quiet []caltrans.ChainControlData) []chainStretch {
	active = dedupeChainSigns(active)
	n := len(active)
	uf := newUnionFind(n)
	inferred := make([]bool, n) // by sign; folded onto the stretch below

	spans := signedSpans(active, quiet)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := active[i], active[j]
			if a.Highway == "" || !sameHighway(a.Highway, b.Highway) || a.Level != b.Level {
				continue
			}
			si, iSigned := spans[i]
			sj, jSigned := spans[j]
			switch {
			case iSigned && jSigned:
				if si.lo <= sj.hi+chainTouchMiles && sj.lo <= si.hi+chainTouchMiles {
					uf.union(i, j)
				}
			case a.Coordinates != nil && b.Coordinates != nil &&
				metersBetween(a, b) <= chainJoinMeters:
				uf.union(i, j)
				inferred[i], inferred[j] = true, true
			}
		}
	}

	groups := map[int]*chainStretch{}
	var order []int
	for i, c := range active {
		root := uf.find(i)
		g := groups[root]
		if g == nil {
			g = &chainStretch{Highway: c.Highway, Level: c.Level}
			groups[root] = g
			order = append(order, root)
		}
		g.Signs = append(g.Signs, c)
		g.Inferred = g.Inferred || inferred[i]
	}
	out := make([]chainStretch, 0, len(order))
	for _, root := range order {
		orderChainSigns(groups[root].Signs)
		out = append(out, *groups[root])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ka, kb := highwayKey(a.Highway), highwayKey(b.Highway); ka != kb {
			return naturalLess(ka, kb)
		}
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		return stretchStartsBefore(a, b)
	})
	return out
}

type span struct{ lo, hi float64 }

// signedSpans is rule 1: for each active sign that can be placed on its
// route's milepost axis, the span of road its requirement covers. A sign
// missing from the result (no milepost, or a direction we cannot read) is
// joined by proximity instead.
func signedSpans(active, quiet []caltrans.ChainControlData) map[int]span {
	type mark struct {
		at     float64
		active int // index into active, or -1 for a quiet checkpoint
	}
	type route struct {
		up, down []mark // Eastbound/Northbound, Westbound/Southbound
		lo, hi   float64
	}
	routes := map[string]*route{}
	add := func(c caltrans.ChainControlData, idx int) {
		// A gate is not a chain sign. One that reports a requirement is still
		// shown (it is in active); it just takes no part in the walk.
		if !c.HasMilepost || c.Gate || c.Highway == "" {
			return
		}
		rising, ok := risesWithMilepost(c.Direction)
		if !ok {
			return
		}
		key := highwayKey(c.Highway)
		r := routes[key]
		if r == nil {
			r = &route{lo: math.Inf(1), hi: math.Inf(-1)}
			routes[key] = r
		}
		r.lo, r.hi = math.Min(r.lo, c.Milepost), math.Max(r.hi, c.Milepost)
		if rising {
			r.up = append(r.up, mark{c.Milepost, idx})
		} else {
			r.down = append(r.down, mark{c.Milepost, idx})
		}
	}
	for i, c := range active {
		add(c, i)
	}
	for _, c := range quiet {
		add(c, -1)
	}

	out := map[int]span{}
	for _, r := range routes {
		sort.SliceStable(r.up, func(i, j int) bool { return r.up[i].at < r.up[j].at })
		sort.SliceStable(r.down, func(i, j int) bool { return r.down[i].at < r.down[j].at })
		for i, m := range r.up {
			if m.active < 0 {
				continue
			}
			end := r.hi
			if i+1 < len(r.up) {
				end = r.up[i+1].at
			}
			out[m.active] = span{m.at, end}
		}
		for i, m := range r.down {
			if m.active < 0 {
				continue
			}
			start := r.lo
			if i > 0 {
				start = r.down[i-1].at
			}
			out[m.active] = span{start, m.at}
		}
	}
	return out
}

// risesWithMilepost reports whether traffic in the direction meets rising
// mileposts. California counts them west to east and south to north.
func risesWithMilepost(direction string) (rising, ok bool) {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "eastbound", "northbound":
		return true, true
	case "westbound", "southbound":
		return false, true
	}
	return false, false
}

// dedupeChainSigns drops a sign reported twice with nothing to tell the two
// apart but the message id (cc.kml District 3, messages 9013 and 9014 on
// 2026-11-13). The first is kept. Road closures are left exactly as reported.
func dedupeChainSigns(signs []caltrans.ChainControlData) []caltrans.ChainControlData {
	seen := map[string]bool{}
	out := make([]caltrans.ChainControlData, 0, len(signs))
	for _, c := range signs {
		if c.Coordinates == nil || c.Closed {
			out = append(out, c) // nothing to prove it is the same sign
			continue
		}
		key := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%.5f,%.5f", highwayKey(c.Highway), strings.ToLower(c.Direction),
			c.Level, strings.ToLower(c.LocationName), c.Description, c.EffectiveTime,
			c.Coordinates.Latitude, c.Coordinates.Longitude)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// orderChainSigns sorts a stretch's signs from its low end to its high end.
func orderChainSigns(signs []caltrans.ChainControlData) {
	allSigned := true
	for _, c := range signs {
		allSigned = allSigned && c.HasMilepost
	}
	if allSigned {
		sort.SliceStable(signs, func(i, j int) bool {
			if signs[i].Milepost != signs[j].Milepost {
				return signs[i].Milepost < signs[j].Milepost
			}
			return signs[i].MessageID < signs[j].MessageID
		})
		return
	}
	// By position: the two signs farthest apart are the ends, and the western
	// one (the southern one on a north-south highway) is the start, which is
	// the end mileposts count from.
	from, far := -1, -1.0
	for i := range signs {
		for j := i + 1; j < len(signs); j++ {
			if signs[i].Coordinates == nil || signs[j].Coordinates == nil {
				continue
			}
			if d := metersBetween(signs[i], signs[j]); d > far {
				far, from = d, i
				if lowerEnd(signs[j], signs[i]) {
					from = j
				}
			}
		}
	}
	if from < 0 {
		return // one sign, or none with a position: the order given stands
	}
	start := signs[from]
	dist := func(c caltrans.ChainControlData) float64 {
		if c.Coordinates == nil {
			return math.Inf(1) // unplaced signs go last
		}
		return metersBetween(start, c)
	}
	sort.SliceStable(signs, func(i, j int) bool {
		if di, dj := dist(signs[i]), dist(signs[j]); di != dj {
			return di < dj
		}
		return signs[i].MessageID < signs[j].MessageID
	})
}

// lowerEnd reports whether a sits before b the way mileposts count: further
// south on a highway signed north/south, further west otherwise. Where the
// road runs across its signed direction (Hwy 49 is signed north/south and
// runs due east over Yuba Pass) the signed axis says nothing, and the other
// one decides.
func lowerEnd(a, b caltrans.ChainControlData) bool {
	north := (b.Coordinates.Latitude - a.Coordinates.Latitude) * 111320
	east := (b.Coordinates.Longitude - a.Coordinates.Longitude) * 111320 * math.Cos(a.Coordinates.Latitude*math.Pi/180)
	bySouth := northSouth(a.Direction) || northSouth(b.Direction)
	if along, across := math.Abs(north), math.Abs(east); bySouth && along < across/4 {
		bySouth = false
	} else if !bySouth && across < along/4 {
		bySouth = true
	}
	if bySouth {
		return north > 0
	}
	return east > 0
}

func northSouth(direction string) bool {
	d := strings.ToLower(strings.TrimSpace(direction))
	return d == "northbound" || d == "southbound"
}

// stretchStartsBefore orders two stretches of one highway and level along the
// road, by where each starts.
func stretchStartsBefore(a, b chainStretch) bool {
	sa, sb := a.Signs[0], b.Signs[0]
	switch {
	case sa.HasMilepost && sb.HasMilepost && sa.Milepost != sb.Milepost:
		return sa.Milepost < sb.Milepost
	case sa.Coordinates != nil && sb.Coordinates != nil &&
		(sa.Coordinates.Latitude != sb.Coordinates.Latitude || sa.Coordinates.Longitude != sb.Coordinates.Longitude):
		return lowerEnd(sa, sb)
	}
	return sa.MessageID < sb.MessageID
}

func metersBetween(a, b caltrans.ChainControlData) float64 {
	const r = 6371000.0
	p1, p2 := a.Coordinates.Latitude*math.Pi/180, b.Coordinates.Latitude*math.Pi/180
	dp, dl := p2-p1, (b.Coordinates.Longitude-a.Coordinates.Longitude)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

var highwayNumberRe = regexp.MustCompile(`(\d+[A-Za-z]?)\s*$`)

// highwayKey is a highway's identity across the two feeds' spellings, and the
// route part of a stretch id: "Highway 4" -> "hwy4", "US 50" -> "us50",
// "Interstate 80" and "I-80" -> "i80". A label it cannot read is slugged
// whole, so it still only ever matches itself.
func highwayKey(highway string) string {
	h := strings.ToLower(strings.TrimSpace(highway))
	m := highwayNumberRe.FindStringSubmatch(h)
	if m == nil {
		return strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(h, "-"), "-")
	}
	switch {
	case strings.HasPrefix(h, "us"):
		return "us" + m[1]
	case strings.HasPrefix(h, "i-"), strings.HasPrefix(h, "i "), strings.HasPrefix(h, "interstate"):
		return "i" + m[1]
	default:
		return "hwy" + m[1]
	}
}

func sameHighway(a, b string) bool { return highwayKey(a) == highwayKey(b) }

// naturalLess orders "hwy4" before "hwy88" before "hwy108".
func naturalLess(a, b string) bool {
	split := func(s string) (string, int) {
		i := strings.IndexAny(s, "0123456789")
		if i < 0 {
			return s, 0
		}
		n := 0
		fmt.Sscanf(s[i:], "%d", &n)
		return s[:i], n
	}
	pa, na := split(a)
	pb, nb := split(b)
	if pa != pb {
		return pa < pb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

// union keeps the lower index as the root, so a stretch's first-listed sign
// decides nothing but the order groups are discovered in.
func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
}
