package geojson

import "math"

// metersPerDegreeLat is the length of one degree of latitude. It varies by less
// than 0.3% between the equator and the poles, so a single constant is ample for
// a proximity buffer measured in kilometres.
const metersPerDegreeLat = 111320.0

// WithinDistance reports whether two geometries come within meters of one
// another: they overlap or contain one another (distance 0), or their closest
// boundaries are at most meters apart. Either geometry may be a Point,
// LineString, Polygon or MultiPolygon; nil or a non-positive distance is false.
//
// This is the "near a place" predicate, the areal counterpart to
// PointNearLine's "near a corridor". The store uses it to attach a WILDFIRE
// event to a place it is APPROACHING but has not yet reached — a fire 12 km
// outside the coverage polygon is a threat to it, and waiting for the perimeter
// to cross the line is waiting too long. Every other layer keeps the strict
// containment/overlap rules: only fire moves toward you.
//
// Distances use a local equirectangular projection anchored at the midpoint
// latitude of the two geometries. Over the tens of kilometres a proximity buffer
// spans that is accurate to well under a percent — and it is far cheaper than
// haversine, which matters because this runs inside the ingest write
// transaction, per event, per place.
func WithinDistance(a, b *Geom, meters float64) bool {
	if a == nil || b == nil || meters <= 0 {
		return false
	}
	aMinLat, aMinLng, aMaxLat, aMaxLng := a.Bbox()
	bMinLat, bMinLng, bMaxLat, bMaxLng := b.Bbox()

	// Cheap reject: grow a's bbox by the buffer and require overlap. A degree of
	// longitude shrinks with cos(lat), so convert at the SMALLEST cosine (highest
	// absolute latitude) in play — that yields the widest degree margin, keeping
	// the prefilter inclusive. An over-wide prefilter only costs a few exact
	// tests; a too-narrow one silently drops a real attachment.
	padLat := meters / metersPerDegreeLat
	padLng := padLat / math.Max(minCos(aMinLat, aMaxLat, bMinLat, bMaxLat), 1e-6)
	if !BboxIntersects(
		aMinLat-padLat, aMinLng-padLng, aMaxLat+padLat, aMaxLng+padLng,
		bMinLat, bMinLng, bMaxLat, bMaxLng) {
		return false
	}

	// Distance 0 cases, cheapest first: areal overlap, then a vertex of either
	// geometry inside the other (which covers a point or line inside a polygon,
	// where Intersects is false by definition).
	if Intersects(a, b) || anyPositionInside(a, b) || anyPositionInside(b, a) {
		return true
	}

	lat0 := (aMinLat + aMaxLat + bMinLat + bMaxLat) / 4
	kx := metersPerDegreeLat * math.Cos(lat0*math.Pi/180)
	return separationAtMost(a, b, kx, meters)
}

// minCos is the smallest cos(latitude) among the given latitudes — i.e. the
// shortest longitude degree, which gives the widest (safest) degree margin.
func minCos(lats ...float64) float64 {
	m := 1.0
	for _, lat := range lats {
		if c := math.Cos(lat * math.Pi / 180); c < m {
			m = c
		}
	}
	return m
}

// anyPositionInside reports whether any vertex of g falls inside other.
func anyPositionInside(g, other *Geom) bool {
	found := false
	g.eachPosition(func(p Position) {
		if !found && PointInGeometry(p.Lat(), p.Lng(), other) {
			found = true
		}
	})
	return found
}

// separationAtMost reports whether the closest approach between a's and b's
// vertex/edge sets is at most meters, comparing every vertex of each against
// every edge of the other (and vertex-to-vertex when the other side has no
// edges, i.e. it is a Point). kx is metres per degree of longitude at the local
// latitude. Callers must have ruled out overlap first — this measures boundaries.
func separationAtMost(a, b *Geom, kx, meters float64) bool {
	av, ae := project(a, kx), projectEdges(a, kx)
	bv, be := project(b, kx), projectEdges(b, kx)
	limit := meters * meters
	return sidesWithin(av, be, bv, limit) || sidesWithin(bv, ae, av, limit)
}

// sidesWithin reports whether any vertex in verts is within sqrt(limit) of an
// edge in edges — or, when edges is empty (the other geometry is a Point), of a
// vertex in fallback.
func sidesWithin(verts []xy, edges []edge, fallback []xy, limit float64) bool {
	for _, p := range verts {
		for _, e := range edges {
			if pointSegDistSq(p, e) <= limit {
				return true
			}
		}
		if len(edges) == 0 {
			for _, q := range fallback {
				if dx, dy := p.x-q.x, p.y-q.y; dx*dx+dy*dy <= limit {
					return true
				}
			}
		}
	}
	return false
}

// xy is a position projected to local metres.
type xy struct{ x, y float64 }

// edge is a projected segment.
type edge struct{ a, b xy }

// project returns every vertex of g in local metres.
func project(g *Geom, kx float64) []xy {
	var out []xy
	g.eachPosition(func(p Position) {
		out = append(out, xy{x: p.Lng() * kx, y: p.Lat() * metersPerDegreeLat})
	})
	return out
}

// projectEdges returns every segment of g in local metres: polygon rings are
// implicitly closed (last->first, matching the containment convention),
// LineStrings stay open, and a Point/MultiPoint has no edges.
func projectEdges(g *Geom, kx float64) []edge {
	var lines [][]Position
	closed := false
	switch g.Type {
	case "LineString":
		lines = [][]Position{g.Points}
	case "MultiLineString":
		lines = g.Rings
	case "Polygon":
		lines, closed = g.Rings, true
	case "MultiPolygon":
		for _, poly := range g.Polygons {
			lines = append(lines, poly...)
		}
		closed = true
	default:
		return nil
	}

	var out []edge
	for _, line := range lines {
		n := len(line)
		if n < 2 {
			continue
		}
		pts := make([]xy, n)
		for i, p := range line {
			pts[i] = xy{x: p.Lng() * kx, y: p.Lat() * metersPerDegreeLat}
		}
		for i := 1; i < n; i++ {
			out = append(out, edge{a: pts[i-1], b: pts[i]})
		}
		if closed {
			out = append(out, edge{a: pts[n-1], b: pts[0]})
		}
	}
	return out
}

// pointSegDistSq is the squared distance from p to segment e, in metres².
func pointSegDistSq(p xy, e edge) float64 {
	dx, dy := e.b.x-e.a.x, e.b.y-e.a.y
	if dx == 0 && dy == 0 {
		ux, uy := p.x-e.a.x, p.y-e.a.y
		return ux*ux + uy*uy
	}
	t := ((p.x-e.a.x)*dx + (p.y-e.a.y)*dy) / (dx*dx + dy*dy)
	switch {
	case t < 0:
		t = 0
	case t > 1:
		t = 1
	}
	cx, cy := e.a.x+t*dx, e.a.y+t*dy
	ux, uy := p.x-cx, p.y-cy
	return ux*ux + uy*uy
}
