package geojson

// PointInGeometry reports whether the internal (lat, lng) point falls inside
// the geometry. Only Polygon and MultiPolygon can contain a point: Point,
// MultiPoint, LineString and MultiLineString always return false, as does a
// nil geometry. A bounding-box check rejects far-away points before any ring
// walk.
//
// Containment uses the even-odd rule via ray casting across every ring of a
// polygon, so holes are respected: a point inside the shell AND inside a hole
// crosses an even number of edges and is outside. A MultiPolygon contains a
// point when any of its parts does.
//
// Boundary tolerance: a point exactly on an edge or vertex follows the
// ray-cast half-open convention — deterministic for a given geometry (roughly:
// min-lat/min-lng edges count inside, max-lat/max-lng edges outside, so two
// polygons sharing an edge claim a boundary point exactly once), but callers
// must treat on-boundary results as unspecified. Anything needing edge
// tolerance should buffer the geometry; at the package's 5-decimal encoding
// precision an edge is ~1 m wide.
func PointInGeometry(lat, lng float64, g *Geom) bool {
	if g == nil {
		return false
	}
	switch g.Type {
	case "Polygon", "MultiPolygon":
	default:
		return false
	}

	minLat, minLng, maxLat, maxLng := g.Bbox()
	if lat < minLat || lat > maxLat || lng < minLng || lng > maxLng {
		return false
	}

	if g.Type == "Polygon" {
		return ringsContain(g.Rings, lat, lng)
	}
	for _, poly := range g.Polygons {
		if ringsContain(poly, lat, lng) {
			return true
		}
	}
	return false
}

// ringsContain applies the even-odd rule across all rings of one polygon
// (PNPOLY ray cast, eastward ray). Counting crossings over shell and holes
// together makes holes subtract naturally. Rings are treated as implicitly
// closed via the last->first wraparound edge, so unclosed upstream rings
// work; an explicitly closed ring's duplicate vertex yields a degenerate
// wraparound edge that self-skips (yi == yj).
func ringsContain(rings [][]Position, lat, lng float64) bool {
	inside := false
	for _, ring := range rings {
		n := len(ring)
		if n < 3 {
			continue // degenerate ring cannot contain anything
		}
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			xi, yi := ring[i].Lng(), ring[i].Lat()
			xj, yj := ring[j].Lng(), ring[j].Lat()
			// Edge straddles the point's latitude, and the crossing with the
			// eastward ray from (lng, lat) is strictly east of the point.
			if (yi > lat) != (yj > lat) && lng < (xj-xi)*(lat-yi)/(yj-yi)+xi {
				inside = !inside
			}
		}
	}
	return inside
}
