// Package hazards aggregates the service's hazard/situation data sources into a
// single standardized, map-ready GeoJSON interface (see
// docs/hazard-aggregation-design.md). Every hazard, from every source, is
// normalized into an RFC 7946 Feature with a common properties envelope so an
// open maps client (MapLibre GL, Leaflet, OpenLayers) can layer it directly.
package hazards

import (
	"encoding/json"
	"math"
)

// FeatureCollection is an RFC 7946 FeatureCollection. The non-standard
// `metadata` member is a foreign member (allowed by RFC 7946 §6.1) carrying
// provenance/freshness; map libraries ignore it.
type FeatureCollection struct {
	Type     string    `json:"type"` // always "FeatureCollection"
	Features []Feature `json:"features"`
	Metadata *Metadata `json:"metadata,omitempty"`
}

// Feature is an RFC 7946 Feature. Geometry is a pointer so it can be null for
// non-located items (a county-wide advisory renders as a banner, not a shape).
type Feature struct {
	Type       string     `json:"type"` // always "Feature"
	Geometry   *Geometry  `json:"geometry"`
	Properties Properties `json:"properties"`
}

// Geometry is an RFC 7946 geometry. Coordinates use [longitude, latitude] order
// (RFC 7946 §3.1.1) — the inverse of the service's internal {latitude,longitude}.
type Geometry struct {
	Type        string `json:"type"`
	Coordinates any    `json:"coordinates"`
}

// Metadata is the foreign-member provenance/freshness block on a collection.
type Metadata struct {
	Layer            string `json:"layer"`
	Area             string `json:"area"`
	GeneratedAt      string `json:"generated_at"`
	SourceStatus     string `json:"source_status"` // OK | STALE | UNAVAILABLE
	LastSourceUpdate string `json:"last_source_update,omitempty"`
	Attribution      string `json:"attribution,omitempty"`
	SourceURL        string `json:"source_url,omitempty"`
	SchemaVersion    int    `json:"schema_version"`
}

// --- Geometry constructors (handle the lat/lng -> lon,lat swap + precision) ---

// coordPrecision trims coordinates to ~1.1 m (5 decimals) to cut payload.
const coordPrecision = 5

func trim(v float64) float64 {
	p := math.Pow(10, coordPrecision)
	return math.Round(v*p) / p
}

// lonLat returns a coordinate pair in RFC 7946 [lon, lat] order, trimmed.
func lonLat(lat, lng float64) []float64 {
	return []float64{trim(lng), trim(lat)}
}

// PointGeom builds a Point from internal {lat,lng}.
func PointGeom(lat, lng float64) *Geometry {
	return &Geometry{Type: "Point", Coordinates: lonLat(lat, lng)}
}

// LatLng is an internal {lat,lng} pair used by the line/polygon constructors.
type LatLng struct{ Lat, Lng float64 }

// LineStringGeom builds a LineString from internal {lat,lng} points. Returns nil
// if fewer than two points (an invalid LineString).
func LineStringGeom(points []LatLng) *Geometry {
	if len(points) < 2 {
		return nil
	}
	coords := make([][]float64, len(points))
	for i, p := range points {
		coords[i] = lonLat(p.Lat, p.Lng)
	}
	return &Geometry{Type: "LineString", Coordinates: coords}
}

// RawGeom wraps an already-GeoJSON geometry (type + raw [lon,lat] coordinates
// straight from an upstream feed, e.g. ArcGIS f=geojson) without re-encoding.
// Returns nil if either part is empty.
func RawGeom(geomType string, coords json.RawMessage) *Geometry {
	if geomType == "" || len(coords) == 0 {
		return nil
	}
	return &Geometry{Type: geomType, Coordinates: coords}
}
