package geojson

import "math"

// SimplifiedMultiPolygon combines the areal parts of geoms into one
// MultiPolygon, each ring simplified with Douglas-Peucker at tolerance
// degrees and repaired so it does not cross itself (simplifyRingValid), and
// encodes it. Non-areal geometries are skipped; a ring that simplifies below
// a triangle is dropped, and a polygon whose shell is dropped is dropped with
// its holes. Returns nil when nothing areal remains.
//
// The output depends only on the inputs and their order, so a caller that
// passes the same shapes in the same order gets the same bytes (geometry is
// content-hashed in the store).
//
// The parts are not unioned: adjacent zones keep their shared edge. That is
// fine for drawing and for the store's place matching, which tests each part.
func SimplifiedMultiPolygon(tolerance float64, geoms ...*Geom) []byte {
	var out [][][][]float64
	for _, g := range geoms {
		if g == nil {
			continue
		}
		var polys [][][]Position
		switch g.Type {
		case "Polygon":
			polys = [][][]Position{g.Rings}
		case "MultiPolygon":
			polys = g.Polygons
		default:
			continue
		}
		for _, rings := range polys {
			var poly [][][]float64
			for i, ring := range rings {
				s := simplifyRingValid(ring, tolerance)
				if len(s) < 4 { // a closed triangle is four positions
					if i == 0 {
						poly = nil
						break
					}
					continue
				}
				coords := make([][]float64, len(s))
				for j, p := range s {
					coords[j] = []float64{trim(p.Lng()), trim(p.Lat())}
				}
				poly = append(poly, coords)
			}
			if len(poly) > 0 {
				out = append(out, poly)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return marshal(geometryJSON{Type: "MultiPolygon", Coordinates: out})
}

// simplifyRingValid simplifies ring with Douglas-Peucker at tol, then
// repairs it: Douglas-Peucker does not preserve topology, and on the long,
// narrow STO elevation-band zones it left self-crossing rings at every
// tolerance tried. While two simplified edges cross, each takes back the
// original vertex farthest from it. The result is the original ring when
// repair runs out of rounds (or the original itself is not simple).
func simplifyRingValid(ring []Position, tol float64) []Position {
	closed := closeRing(ring)
	if len(closed) <= 4 {
		return closed
	}
	keep := dpKeep(closed, tol)
	for round := 0; round < simplifyRepairRounds; round++ {
		idx := keptIndices(keep)
		bad := crossingEdges(closed, idx)
		if len(bad) == 0 {
			return pick(closed, idx)
		}
		added := false
		for e := range bad {
			a, b := idx[e], idx[e+1]
			if far := farthestBetween(closed, a, b); far >= 0 {
				keep[far] = true
				added = true
			}
		}
		if !added {
			break
		}
	}
	return closed
}

// simplifyRepairRounds bounds the repair loop; each round re-tests the ring.
const simplifyRepairRounds = 64

func closeRing(ring []Position) []Position {
	if len(ring) == 0 || ring[0] == ring[len(ring)-1] {
		return ring
	}
	return append(append([]Position(nil), ring...), ring[0])
}

// dpKeep marks the vertices of a closed ring Douglas-Peucker keeps. A closed
// ring's endpoints coincide, so it splits at the vertex farthest from the
// start and simplifies the two halves; one chord would be degenerate.
func dpKeep(ring []Position, tol float64) []bool {
	keep := make([]bool, len(ring))
	far, best := 0, -1.0
	for i, p := range ring {
		if d := sqDist(p, ring[0]); d > best {
			far, best = i, d
		}
	}
	keep[0], keep[far], keep[len(ring)-1] = true, true, true
	tol2 := tol * tol
	type span struct{ a, b int }
	stack := []span{{0, far}, {far, len(ring) - 1}}
	for len(stack) > 0 {
		sp := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		idx, max := -1, tol2
		for i := sp.a + 1; i < sp.b; i++ {
			if d := segDistSq(ring[i], ring[sp.a], ring[sp.b]); d > max {
				idx, max = i, d
			}
		}
		if idx >= 0 {
			keep[idx] = true
			stack = append(stack, span{sp.a, idx}, span{idx, sp.b})
		}
	}
	return keep
}

func keptIndices(keep []bool) []int {
	var idx []int
	for i, k := range keep {
		if k {
			idx = append(idx, i)
		}
	}
	return idx
}

func pick(ring []Position, idx []int) []Position {
	out := make([]Position, len(idx))
	for i, j := range idx {
		out[i] = ring[j]
	}
	return out
}

// crossingEdges returns the simplified edges (by position in idx) that touch
// or cross a non-adjacent edge.
func crossingEdges(ring []Position, idx []int) map[int]bool {
	n := len(idx) - 1
	bad := map[int]bool{}
	for i := 0; i < n; i++ {
		for j := i + 2; j < n; j++ {
			if i == 0 && j == n-1 {
				continue // the first and last edges share the ring's start
			}
			if segmentsIntersect(ring[idx[i]], ring[idx[i+1]], ring[idx[j]], ring[idx[j+1]]) {
				bad[i], bad[j] = true, true
			}
		}
	}
	return bad
}

// farthestBetween is the vertex strictly between a and b farthest from the
// chord a-b, or -1 when there is none.
func farthestBetween(ring []Position, a, b int) int {
	idx, max := -1, -1.0
	for i := a + 1; i < b; i++ {
		if d := segDistSq(ring[i], ring[a], ring[b]); d > max {
			idx, max = i, d
		}
	}
	return idx
}

// ringSelfCrosses reports whether any two non-adjacent edges of a closed ring
// touch or cross.
func ringSelfCrosses(r []Position) bool {
	idx := make([]int, len(r))
	for i := range idx {
		idx[i] = i
	}
	return len(crossingEdges(r, idx)) > 0
}

// ringArea is the shoelace area of a closed ring, in square degrees (signed).
func ringArea(r []Position) float64 {
	a := 0.0
	for i := 0; i+1 < len(r); i++ {
		a += r[i][0]*r[i+1][1] - r[i+1][0]*r[i][1]
	}
	return a / 2
}

func sqDist(a, b Position) float64 {
	dx, dy := a[0]-b[0], a[1]-b[1]
	return dx*dx + dy*dy
}

// segDistSq is the squared distance from p to segment ab, in degrees. Fine at
// this tolerance and latitude: it only decides which vertices to keep.
func segDistSq(p, a, b Position) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return sqDist(p, a)
	}
	t := math.Max(0, math.Min(1, ((p[0]-a[0])*dx+(p[1]-a[1])*dy)/l2))
	return sqDist(p, Position{a[0] + t*dx, a[1] + t*dy})
}
