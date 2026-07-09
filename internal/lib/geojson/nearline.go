package geojson

import "github.com/dpup/sierra-data/internal/lib/geo"

// PointNearLine reports whether (lat, lng) is within meters of a LineString or
// MultiLineString geometry — the minimum great-circle distance from the point to
// any of the line's segments is <= meters. Other geometry types return false
// (Point/Polygon use PointInGeometry).
//
// This is how a point event attaches to a *corridor* place: corridors are
// LineStrings, which have no interior, so a strict point-in-polygon test never
// matches one. A road incident near the road counts as "on the corridor".
func PointNearLine(lat, lng float64, g *Geom, meters float64) bool {
	if g == nil || meters <= 0 {
		return false
	}
	var lines [][]Position
	switch g.Type {
	case "LineString":
		lines = [][]Position{g.Points}
	case "MultiLineString":
		lines = g.Rings
	default:
		return false
	}
	gu := geo.NewGeoUtils()
	pt := geo.Point{Latitude: lat, Longitude: lng}
	for _, line := range lines {
		if len(line) < 2 {
			continue
		}
		pl := geo.Polyline{Points: make([]geo.Point, len(line))}
		for i, p := range line {
			pl.Points[i] = geo.Point{Latitude: p.Lat(), Longitude: p.Lng()}
		}
		if d, err := gu.PointToPolyline(pt, pl); err == nil && d <= meters {
			return true
		}
	}
	return false
}
