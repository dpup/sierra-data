package geo

// Point represents a geographic coordinate
type Point struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lng"`
}

// Polyline represents an encoded polyline with optional decoded points
type Polyline struct {
	EncodedPolyline string  `json:"encoded_polyline"`
	Points          []Point `json:"points"`
}

// OverlapSegment represents a segment where two polylines overlap
type OverlapSegment struct {
	StartPoint Point   `json:"start_point"`
	EndPoint   Point   `json:"end_point"`
	Length     float64 `json:"length_meters"`
}

// GeoUtils interface defines geographic calculation utilities
type GeoUtils interface {
	// Calculate great-circle distance between two points in meters
	PointToPoint(p1, p2 Point) (float64, error)

	// Calculate minimum distance from point to polyline in meters
	PointToPolyline(point Point, polyline Polyline) (float64, error)

	// Check if two polylines overlap (for closure vs route matching)
	PolylinesOverlap(polyline1, polyline2 Polyline, thresholdMeters float64) (bool, []OverlapSegment, error)

	// Calculate percentage of polyline1 that overlaps with polyline2
	PolylineOverlapPercentage(polyline1, polyline2 Polyline, thresholdMeters float64) (float64, error)

	// Decode Google polyline string to point sequence
	DecodePolyline(encoded string) ([]Point, error)

	// Find closest point on polyline to given point
	ClosestPointOnPolyline(point Point, polyline Polyline) (Point, error)

	// Filter points to those within specified distance of center point
	FilterPointsByDistance(points []Point, center Point, maxDistanceMeters float64) ([]Point, error)

	// Calculate distance between coordinate pairs (convenience method)
	DistanceFromCoords(lat1, lon1, lat2, lon2 float64) (float64, error)
}

// NewGeoUtils is implemented in geo.go
