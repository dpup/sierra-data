package geojson

import "math"

// SimplifiedMultiPolygon combines the areal parts of geoms into one
// MultiPolygon, each ring simplified with Douglas-Peucker at tolerance
// degrees, and encodes it. Non-areal geometries are skipped; a ring that
// simplifies below a triangle is dropped, and a polygon whose shell is dropped
// is dropped with its holes. Returns nil when nothing areal remains.
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
				s := simplifyRing(ring, tolerance)
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

// simplifyRing closes ring if needed and simplifies it, keeping it closed.
func simplifyRing(ring []Position, tol float64) []Position {
	if len(ring) == 0 {
		return nil
	}
	if ring[0] != ring[len(ring)-1] {
		ring = append(append([]Position(nil), ring...), ring[0])
	}
	if len(ring) <= 4 {
		return ring
	}
	// A closed ring's endpoints coincide, so split at the vertex farthest from
	// the start and simplify the two halves; one chord would be degenerate.
	far, best := 0, -1.0
	for i, p := range ring {
		if d := sqDist(p, ring[0]); d > best {
			far, best = i, d
		}
	}
	a := douglasPeucker(ring[:far+1], tol)
	b := douglasPeucker(ring[far:], tol)
	return append(a[:len(a)-1], b...)
}

func douglasPeucker(pts []Position, tol float64) []Position {
	if len(pts) < 3 {
		return append([]Position(nil), pts...)
	}
	keep := make([]bool, len(pts))
	keep[0], keep[len(pts)-1] = true, true
	type span struct{ a, b int }
	stack := []span{{0, len(pts) - 1}}
	tol2 := tol * tol
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		idx, max := -1, tol2
		for i := s.a + 1; i < s.b; i++ {
			if d := segDistSq(pts[i], pts[s.a], pts[s.b]); d > max {
				idx, max = i, d
			}
		}
		if idx >= 0 {
			keep[idx] = true
			stack = append(stack, span{s.a, idx}, span{idx, s.b})
		}
	}
	out := make([]Position, 0, len(pts))
	for i, k := range keep {
		if k {
			out = append(out, pts[i])
		}
	}
	return out
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
