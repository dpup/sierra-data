// Package geojson is a small dependency-free RFC 7946 geometry library shared
// by the grid store (bbox/centroid at ingest), the ingest normalizers, and
// place resolution (point-in-polygon).
//
// Conventions (mirroring internal/hazards/geojson.go):
//   - On the wire, coordinates are [longitude, latitude] per RFC 7946 §3.1.1.
//   - Every lat/lng parameter and return value in this package's API is in the
//     service's internal (lat, lng) order; the encoders/parser own the swap.
//   - Encoders trim coordinates to 5 decimals (~1.1 m) to cut payload.
//   - Antimeridian-crossing geometries are not handled (service area is
//     California).
package geojson

import (
	"encoding/json"
	"fmt"
	"math"
)

// Position is a single coordinate in RFC 7946 [lng, lat] order. A third
// (elevation) element, when present upstream, is dropped at parse time.
type Position [2]float64

// Lng returns the longitude (first element per RFC 7946).
func (p Position) Lng() float64 { return p[0] }

// Lat returns the latitude (second element per RFC 7946).
func (p Position) Lat() float64 { return p[1] }

// Geom is a parsed GeoJSON geometry with typed coordinates retained in
// RFC 7946 [lng, lat] order. Exactly one coordinate field is populated for a
// given Type:
//
//	Point                    -> Point
//	MultiPoint, LineString   -> Points
//	MultiLineString, Polygon -> Rings (Polygon: Rings[0] is the exterior
//	                            shell, Rings[1:] are holes)
//	MultiPolygon             -> Polygons
type Geom struct {
	Type     string
	Point    Position
	Points   []Position
	Rings    [][]Position
	Polygons [][][]Position
}

// Parse decodes a GeoJSON geometry object (not a Feature). Point, LineString,
// Polygon and MultiPolygon are first-class; MultiPoint and MultiLineString are
// tolerated so upstream feeds don't hard-fail. GeometryCollection and unknown
// types are errors. Rings are accepted unclosed (>= 3 positions) — real feeds
// omit the closing vertex; containment treats rings as implicitly closed.
func Parse(raw []byte) (*Geom, error) {
	var env struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("geojson: invalid geometry object: %w", err)
	}
	if len(env.Coordinates) == 0 || string(env.Coordinates) == "null" {
		return nil, fmt.Errorf("geojson: %q geometry has no coordinates", env.Type)
	}

	g := &Geom{Type: env.Type}
	switch env.Type {
	case "Point":
		var c []float64
		if err := json.Unmarshal(env.Coordinates, &c); err != nil {
			return nil, fmt.Errorf("geojson: Point coordinates: %w", err)
		}
		p, err := toPosition(c)
		if err != nil {
			return nil, err
		}
		g.Point = p

	case "MultiPoint", "LineString":
		pts, err := parsePositions(env.Coordinates)
		if err != nil {
			return nil, fmt.Errorf("geojson: %s coordinates: %w", env.Type, err)
		}
		min := 1
		if env.Type == "LineString" {
			min = 2
		}
		if len(pts) < min {
			return nil, fmt.Errorf("geojson: %s needs at least %d positions, got %d", env.Type, min, len(pts))
		}
		g.Points = pts

	case "MultiLineString", "Polygon":
		rings, err := parseRings(env.Coordinates, env.Type)
		if err != nil {
			return nil, err
		}
		g.Rings = rings

	case "MultiPolygon":
		var raws []json.RawMessage
		if err := json.Unmarshal(env.Coordinates, &raws); err != nil {
			return nil, fmt.Errorf("geojson: MultiPolygon coordinates: %w", err)
		}
		if len(raws) == 0 {
			return nil, fmt.Errorf("geojson: MultiPolygon has no polygons")
		}
		g.Polygons = make([][][]Position, len(raws))
		for i, pr := range raws {
			rings, err := parseRings(pr, "Polygon")
			if err != nil {
				return nil, fmt.Errorf("geojson: MultiPolygon part %d: %w", i, err)
			}
			g.Polygons[i] = rings
		}

	case "GeometryCollection":
		return nil, fmt.Errorf("geojson: GeometryCollection is not supported")
	default:
		return nil, fmt.Errorf("geojson: unsupported geometry type %q", env.Type)
	}
	return g, nil
}

// parsePositions decodes a JSON array of positions, dropping elevation.
func parsePositions(raw json.RawMessage) ([]Position, error) {
	var cs [][]float64
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, err
	}
	pts := make([]Position, len(cs))
	for i, c := range cs {
		p, err := toPosition(c)
		if err != nil {
			return nil, err
		}
		pts[i] = p
	}
	return pts, nil
}

// parseRings decodes a Polygon or MultiLineString coordinate array. Polygon
// rings need >= 3 positions (unclosed tolerated); MultiLineString lines >= 2.
func parseRings(raw json.RawMessage, geomType string) ([][]Position, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, fmt.Errorf("geojson: %s coordinates: %w", geomType, err)
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("geojson: %s has no rings", geomType)
	}
	min, unit := 3, "ring"
	if geomType == "MultiLineString" {
		min, unit = 2, "line"
	}
	rings := make([][]Position, len(raws))
	for i, rr := range raws {
		pts, err := parsePositions(rr)
		if err != nil {
			return nil, fmt.Errorf("geojson: %s %s %d: %w", geomType, unit, i, err)
		}
		if len(pts) < min {
			return nil, fmt.Errorf("geojson: %s %s %d needs at least %d positions, got %d", geomType, unit, i, min, len(pts))
		}
		rings[i] = pts
	}
	return rings, nil
}

// toPosition validates a raw coordinate and drops any elevation element.
func toPosition(c []float64) (Position, error) {
	if len(c) < 2 {
		return Position{}, fmt.Errorf("geojson: position needs at least 2 elements, got %d", len(c))
	}
	return Position{c[0], c[1]}, nil
}

// eachPosition invokes fn for every coordinate in the geometry.
func (g *Geom) eachPosition(fn func(Position)) {
	switch g.Type {
	case "Point":
		fn(g.Point)
	case "MultiPoint", "LineString":
		for _, p := range g.Points {
			fn(p)
		}
	case "MultiLineString", "Polygon":
		for _, ring := range g.Rings {
			for _, p := range ring {
				fn(p)
			}
		}
	case "MultiPolygon":
		for _, poly := range g.Polygons {
			for _, ring := range poly {
				for _, p := range ring {
					fn(p)
				}
			}
		}
	}
}

// Bbox returns the geometry's bounding box in internal (lat, lng) order.
// Parse never produces an empty geometry; a zero-value Geom returns all zeros.
func (g *Geom) Bbox() (minLat, minLng, maxLat, maxLng float64) {
	first := true
	g.eachPosition(func(p Position) {
		lat, lng := p.Lat(), p.Lng()
		if first {
			minLat, maxLat, minLng, maxLng = lat, lat, lng, lng
			first = false
			return
		}
		minLat = math.Min(minLat, lat)
		maxLat = math.Max(maxLat, lat)
		minLng = math.Min(minLng, lng)
		maxLng = math.Max(maxLng, lng)
	})
	return
}

// Centroid returns the bbox center in internal (lat, lng) order. It is a cheap
// anchor for indexing and display (map pin, R*Tree seed) — NOT an area
// centroid. Do not use it for spatial analysis: for concave or multi-part
// shapes the bbox center can fall outside the geometry entirely.
func (g *Geom) Centroid() (lat, lng float64) {
	minLat, minLng, maxLat, maxLng := g.Bbox()
	return (minLat + maxLat) / 2, (minLng + maxLng) / 2
}

// BboxIntersects reports whether two (lat, lng) bounding boxes overlap.
// Touching edges count as intersecting (bounds are inclusive).
func BboxIntersects(aMinLat, aMinLng, aMaxLat, aMaxLng, bMinLat, bMinLng, bMaxLat, bMaxLng float64) bool {
	return aMinLat <= bMaxLat && bMinLat <= aMaxLat &&
		aMinLng <= bMaxLng && bMinLng <= aMaxLng
}

// --- Encoders ([lng, lat] output order, 5-decimal trim) ---

// coordPrecision trims coordinates to ~1.1 m (5 decimals), matching
// internal/hazards/geojson.go.
const coordPrecision = 5

func trim(v float64) float64 {
	p := math.Pow(10, coordPrecision)
	return math.Round(v*p) / p
}

// lonLat returns a coordinate pair in RFC 7946 [lng, lat] order, trimmed.
func lonLat(lat, lng float64) []float64 {
	return []float64{trim(lng), trim(lat)}
}

// geometryJSON is the wire shape shared by all encoders.
type geometryJSON struct {
	Type        string `json:"type"`
	Coordinates any    `json:"coordinates"`
}

// marshal encodes a geometry, returning nil on non-finite coordinates (the
// only way json.Marshal can fail here); nil is the package's "no geometry".
func marshal(g geometryJSON) []byte {
	b, err := json.Marshal(g)
	if err != nil {
		return nil
	}
	return b
}

// PointGeoJSON encodes an internal (lat, lng) point as a GeoJSON Point.
func PointGeoJSON(lat, lng float64) []byte {
	return marshal(geometryJSON{Type: "Point", Coordinates: lonLat(lat, lng)})
}

// LineStringGeoJSON encodes internal (lat, lng) pairs as a GeoJSON LineString.
// Returns nil for fewer than two points (an invalid LineString).
func LineStringGeoJSON(pts [][2]float64) []byte {
	if len(pts) < 2 {
		return nil
	}
	coords := make([][]float64, len(pts))
	for i, p := range pts {
		coords[i] = lonLat(p[0], p[1])
	}
	return marshal(geometryJSON{Type: "LineString", Coordinates: coords})
}

// BboxPolygonGeoJSON encodes a (lat, lng) bounding box as a closed
// counterclockwise single-ring GeoJSON Polygon (RFC 7946 right-hand rule).
// Bounds are encoded as given; callers pass min <= max.
func BboxPolygonGeoJSON(minLat, minLng, maxLat, maxLng float64) []byte {
	ring := [][]float64{
		lonLat(minLat, minLng),
		lonLat(minLat, maxLng),
		lonLat(maxLat, maxLng),
		lonLat(maxLat, minLng),
		lonLat(minLat, minLng),
	}
	return marshal(geometryJSON{Type: "Polygon", Coordinates: [][][]float64{ring}})
}
