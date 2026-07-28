package geojson

// Intersects reports whether two areal geometries (Polygon or MultiPolygon)
// actually overlap — share interior area or have crossing boundaries — not merely
// have overlapping bounding boxes. Anything that is not a Polygon/MultiPolygon
// returns false (point/line containment is PointInGeometry / PointNearLine).
//
// This is the predicate the store uses to decide whether a polygon event (a fire
// perimeter) attaches to a polygonal place (a county/area). Bounding-box overlap
// alone badly over-attaches: county bboxes are large interlocking rectangles that
// overlap each other far from their actual borders, so a small fire near a
// tri-county junction bbox-overlaps all three counties while sitting inside only
// one.
//
// The test is the standard robust one, with a bbox pre-reject and early exits:
// the geometries intersect iff a vertex of one lies inside the other (covers
// containment and partial overlap), OR an edge of one crosses an edge of the
// other (covers the rare case where boundaries cross without any vertex landing
// inside, e.g. a plus/cross shape). Boundary-touching (a shared edge or vertex)
// counts as intersecting — consistent with the "over-attach beats missing a
// perimeter crossing a boundary" posture for a fire straddling a county line.
func Intersects(a, b *Geom) bool {
	if !isAreal(a) || !isAreal(b) {
		return false
	}
	aMinLat, aMinLng, aMaxLat, aMaxLng := a.Bbox()
	bMinLat, bMinLng, bMaxLat, bMaxLng := b.Bbox()
	if !BboxIntersects(aMinLat, aMinLng, aMaxLat, aMaxLng, bMinLat, bMinLng, bMaxLat, bMaxLng) {
		return false
	}
	ra, rb := polygonRings(a), polygonRings(b)
	if anyVertexInside(ra, b) || anyVertexInside(rb, a) {
		return true
	}
	return ringsCross(ra, rb)
}

func isAreal(g *Geom) bool {
	return g != nil && (g.Type == "Polygon" || g.Type == "MultiPolygon")
}

// polygonRings returns every ring (shell + holes) across all parts of an areal
// geometry, so vertex/edge iteration is uniform for Polygon and MultiPolygon.
func polygonRings(g *Geom) [][]Position {
	if g.Type == "Polygon" {
		return g.Rings
	}
	var rings [][]Position
	for _, poly := range g.Polygons {
		rings = append(rings, poly...)
	}
	return rings
}

// anyVertexInside reports whether any vertex of rings falls inside other.
func anyVertexInside(rings [][]Position, other *Geom) bool {
	for _, ring := range rings {
		for _, p := range ring {
			if PointInGeometry(p.Lat(), p.Lng(), other) {
				return true
			}
		}
	}
	return false
}

// ringsCross reports whether any edge of a's rings intersects any edge of b's
// rings. Rings are treated as implicitly closed (last->first wraparound), matching
// the containment convention.
func ringsCross(a, b [][]Position) bool {
	for _, ra := range a {
		na := len(ra)
		if na < 2 {
			continue
		}
		for i, j := 0, na-1; i < na; j, i = i, i+1 {
			a1, a2 := ra[j], ra[i]
			for _, rb := range b {
				nb := len(rb)
				if nb < 2 {
					continue
				}
				for k, l := 0, nb-1; k < nb; l, k = k, k+1 {
					if segmentsIntersect(a1, a2, rb[l], rb[k]) {
						return true
					}
				}
			}
		}
	}
	return false
}

// segmentsIntersect reports whether closed segments p1p2 and p3p4 intersect,
// including collinear overlap and shared endpoints (orientation test + on-segment
// fallback for the collinear cases).
func segmentsIntersect(p1, p2, p3, p4 Position) bool {
	d1 := orient(p3, p4, p1)
	d2 := orient(p3, p4, p2)
	d3 := orient(p1, p2, p3)
	d4 := orient(p1, p2, p4)
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) &&
		((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	if d1 == 0 && onSegment(p3, p4, p1) {
		return true
	}
	if d2 == 0 && onSegment(p3, p4, p2) {
		return true
	}
	if d3 == 0 && onSegment(p1, p2, p3) {
		return true
	}
	if d4 == 0 && onSegment(p1, p2, p4) {
		return true
	}
	return false
}

// orient is the signed cross product of (b-a) x (c-a) in (lng=x, lat=y) space:
// >0 counter-clockwise, <0 clockwise, 0 collinear.
func orient(a, b, c Position) float64 {
	return (b.Lng()-a.Lng())*(c.Lat()-a.Lat()) - (b.Lat()-a.Lat())*(c.Lng()-a.Lng())
}

// onSegment reports whether c (known collinear with a-b) lies within a-b's bbox.
func onSegment(a, b, c Position) bool {
	return min(a.Lng(), b.Lng()) <= c.Lng() && c.Lng() <= max(a.Lng(), b.Lng()) &&
		min(a.Lat(), b.Lat()) <= c.Lat() && c.Lat() <= max(a.Lat(), b.Lat())
}
