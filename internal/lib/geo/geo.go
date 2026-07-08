package geo

import (
	"errors"
	"math"

	"github.com/twpayne/go-polyline"
)

// geoUtils implements the GeoUtils interface
type geoUtils struct{}

// NewGeoUtils creates a new GeoUtils implementation
func NewGeoUtils() GeoUtils {
	return &geoUtils{}
}

// PointToPoint calculates great-circle distance between two points using Haversine formula
func (g *geoUtils) PointToPoint(p1, p2 Point) (float64, error) {
	// Validate coordinates
	if !isValidCoordinate(p1) || !isValidCoordinate(p2) {
		return 0, errors.New("invalid coordinates: latitude must be [-90, 90], longitude must be [-180, 180]")
	}

	// If points are the same, distance is 0
	if p1.Latitude == p2.Latitude && p1.Longitude == p2.Longitude {
		return 0, nil
	}

	// Convert degrees to radians
	lat1 := p1.Latitude * math.Pi / 180
	lon1 := p1.Longitude * math.Pi / 180
	lat2 := p2.Latitude * math.Pi / 180
	lon2 := p2.Longitude * math.Pi / 180

	// Haversine formula
	dlat := lat2 - lat1
	dlon := lon2 - lon1

	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dlon/2)*math.Sin(dlon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	// Earth's radius in meters
	const earthRadius = 6371000
	distance := earthRadius * c

	return distance, nil
}

// PointToPolyline calculates minimum distance from point to polyline
func (g *geoUtils) PointToPolyline(point Point, polyline Polyline) (float64, error) {
	if !isValidCoordinate(point) {
		return 0, errors.New("invalid point coordinates")
	}

	if len(polyline.Points) == 0 {
		return 0, errors.New("polyline has no points")
	}

	if len(polyline.Points) == 1 {
		// Single point polyline - return point to point distance
		return g.PointToPoint(point, polyline.Points[0])
	}

	minDistance := math.Inf(1)

	// Check distance to each segment of the polyline
	for i := 0; i < len(polyline.Points)-1; i++ {
		segmentStart := polyline.Points[i]
		segmentEnd := polyline.Points[i+1]

		distance := g.pointToSegmentDistance(point, segmentStart, segmentEnd)
		if distance < minDistance {
			minDistance = distance
		}
	}

	return minDistance, nil
}

// pointToSegmentDistance calculates perpendicular distance from point to line segment
func (g *geoUtils) pointToSegmentDistance(point, segmentStart, segmentEnd Point) float64 {
	// If segment start and end are the same, return point to point distance
	if segmentStart.Latitude == segmentEnd.Latitude && segmentStart.Longitude == segmentEnd.Longitude {
		distance, _ := g.PointToPoint(point, segmentStart)
		return distance
	}

	// Use cross-track distance formula for point to great circle segment
	// This is an approximation suitable for relatively short distances

	// Calculate distances
	distanceToStart, _ := g.PointToPoint(point, segmentStart)
	distanceToEnd, _ := g.PointToPoint(point, segmentEnd)
	segmentLength, _ := g.PointToPoint(segmentStart, segmentEnd)

	// If segment length is very small, use point-to-point distance
	if segmentLength < 1 {
		return math.Min(distanceToStart, distanceToEnd)
	}

	// Calculate cross-track distance using spherical trigonometry approximation
	// For small distances, this provides reasonable accuracy
	const earthRadius = 6371000

	// Convert to radians
	lat1 := segmentStart.Latitude * math.Pi / 180
	lon1 := segmentStart.Longitude * math.Pi / 180
	lat2 := segmentEnd.Latitude * math.Pi / 180
	lon2 := segmentEnd.Longitude * math.Pi / 180
	lat3 := point.Latitude * math.Pi / 180
	lon3 := point.Longitude * math.Pi / 180

	// Calculate angular distances
	d13 := distanceToStart / earthRadius // Angular distance from start to point

	// Calculate initial bearing from start to end
	y := math.Sin(lon2-lon1) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(lon2-lon1)
	bearing13 := math.Atan2(y, x)

	// Calculate bearing from start to point
	y = math.Sin(lon3-lon1) * math.Cos(lat3)
	x = math.Cos(lat1)*math.Sin(lat3) - math.Sin(lat1)*math.Cos(lat3)*math.Cos(lon3-lon1)
	bearing12 := math.Atan2(y, x)

	// Cross-track distance
	dxt := math.Asin(math.Sin(d13) * math.Sin(bearing12-bearing13))
	crossTrackDistance := math.Abs(dxt) * earthRadius

	// Along-track distance to find if point is between segment endpoints
	dat := math.Acos(math.Cos(d13) / math.Cos(dxt))
	alongTrackDistance := dat * earthRadius

	// If the point's projection lies beyond the segment, use distance to nearest endpoint
	if alongTrackDistance > segmentLength {
		return distanceToEnd
	}

	return crossTrackDistance
}

// PolylinesOverlap checks if two polylines overlap within threshold distance
func (g *geoUtils) PolylinesOverlap(polyline1, polyline2 Polyline, thresholdMeters float64) (bool, []OverlapSegment, error) {
	if len(polyline1.Points) < 2 || len(polyline2.Points) < 2 {
		return false, nil, errors.New("both polylines must have at least 2 points")
	}

	var overlapSegments []OverlapSegment

	// Check each segment of polyline1 against each segment of polyline2
	for i := 0; i < len(polyline1.Points)-1; i++ {
		seg1Start := polyline1.Points[i]
		seg1End := polyline1.Points[i+1]

		for j := 0; j < len(polyline2.Points)-1; j++ {
			seg2Start := polyline2.Points[j]
			seg2End := polyline2.Points[j+1]

			// Check if segments are close enough to be considered overlapping
			if g.segmentsOverlap(seg1Start, seg1End, seg2Start, seg2End, thresholdMeters) {
				// Calculate overlap segment
				overlapStart := g.findCloserPoint(seg1Start, seg2Start, seg2End)
				overlapEnd := g.findCloserPoint(seg1End, seg2Start, seg2End)

				length, _ := g.PointToPoint(overlapStart, overlapEnd)

				overlapSegments = append(overlapSegments, OverlapSegment{
					StartPoint: overlapStart,
					EndPoint:   overlapEnd,
					Length:     length,
				})
			}
		}
	}

	hasOverlap := len(overlapSegments) > 0
	return hasOverlap, overlapSegments, nil
}

// segmentsOverlap checks if two line segments are within threshold distance using proper interpolation
func (g *geoUtils) segmentsOverlap(seg1Start, seg1End, seg2Start, seg2End Point, threshold float64) bool {
	// Calculate segment lengths to determine sampling frequency
	seg1Length, _ := g.PointToPoint(seg1Start, seg1End)
	seg2Length, _ := g.PointToPoint(seg2Start, seg2End)

	// Use adaptive sampling based on segment length and threshold
	// Sample every 50 meters or threshold/2, whichever is smaller, but at least 3 samples per segment
	maxSampleDistance := math.Min(50.0, threshold/2)

	// Sample segment 1 against segment 2
	if g.sampleSegmentAgainstSegment(seg1Start, seg1End, seg2Start, seg2End, seg1Length, maxSampleDistance, threshold) {
		return true
	}

	// Sample segment 2 against segment 1 (to catch cases where seg2 is much longer)
	if g.sampleSegmentAgainstSegment(seg2Start, seg2End, seg1Start, seg1End, seg2Length, maxSampleDistance, threshold) {
		return true
	}

	return false
}

// sampleSegmentAgainstSegment samples points along segment1 and checks distance to segment2
func (g *geoUtils) sampleSegmentAgainstSegment(seg1Start, seg1End, seg2Start, seg2End Point, seg1Length, maxSampleDistance, threshold float64) bool {
	// Determine number of samples (minimum 3: start, middle, end)
	numSamples := int(math.Max(3, math.Ceil(seg1Length/maxSampleDistance)))

	for i := 0; i < numSamples; i++ {
		// Calculate interpolated point along segment 1
		t := float64(i) / float64(numSamples-1) // 0.0 to 1.0
		samplePoint := g.interpolatePoint(seg1Start, seg1End, t)

		// Check distance from sample point to segment 2
		distance := g.pointToSegmentDistance(samplePoint, seg2Start, seg2End)

		if distance <= threshold {
			return true
		}
	}

	return false
}

// interpolatePoint calculates a point along the great circle between two points
// t=0 returns start, t=1 returns end, t=0.5 returns midpoint
func (g *geoUtils) interpolatePoint(start, end Point, t float64) Point {
	// For short distances, linear interpolation is sufficient
	// For longer distances, we should use spherical interpolation, but for road segments
	// (typically < 10km), linear interpolation provides adequate accuracy

	lat := start.Latitude + t*(end.Latitude-start.Latitude)
	lon := start.Longitude + t*(end.Longitude-start.Longitude)

	return Point{Latitude: lat, Longitude: lon}
}

// findCloserPoint finds the point closer to the given segment
func (g *geoUtils) findCloserPoint(point, segStart, segEnd Point) Point {
	distToStart, _ := g.PointToPoint(point, segStart)
	distToEnd, _ := g.PointToPoint(point, segEnd)

	if distToStart <= distToEnd {
		return segStart
	}
	return segEnd
}

// PolylineOverlapPercentage calculates percentage of polyline1 that overlaps with polyline2 using detailed sampling
func (g *geoUtils) PolylineOverlapPercentage(polyline1, polyline2 Polyline, thresholdMeters float64) (float64, error) {
	if len(polyline1.Points) < 2 || len(polyline2.Points) < 2 {
		return 0, errors.New("both polylines must have at least 2 points")
	}

	// Calculate total length of polyline1
	totalLength := 0.0
	for i := 0; i < len(polyline1.Points)-1; i++ {
		segmentLength, _ := g.PointToPoint(polyline1.Points[i], polyline1.Points[i+1])
		totalLength += segmentLength
	}

	if totalLength == 0 {
		return 0, nil
	}

	// Calculate overlapping length using fine-grained sampling
	overlappingLength := 0.0
	sampleDistance := 25.0 // Sample every 25 meters for accuracy

	for i := 0; i < len(polyline1.Points)-1; i++ {
		seg1Start := polyline1.Points[i]
		seg1End := polyline1.Points[i+1]
		segmentLength, _ := g.PointToPoint(seg1Start, seg1End)

		// Sample this segment to determine what portion overlaps
		numSamples := int(math.Max(2, math.Ceil(segmentLength/sampleDistance)))
		overlappingSamples := 0

		for s := 0; s < numSamples; s++ {
			t := float64(s) / float64(numSamples-1)
			samplePoint := g.interpolatePoint(seg1Start, seg1End, t)

			// Check if this sample point is close to polyline2
			distance, _ := g.PointToPolyline(samplePoint, polyline2)
			if distance <= thresholdMeters {
				overlappingSamples++
			}
		}

		// Calculate the proportion of this segment that overlaps
		overlapProportion := float64(overlappingSamples) / float64(numSamples)
		overlappingLength += segmentLength * overlapProportion
	}

	percentage := (overlappingLength / totalLength) * 100
	return percentage, nil
}

// DecodePolyline decodes Google polyline string to point sequence
func (g *geoUtils) DecodePolyline(encoded string) ([]Point, error) {
	if encoded == "" {
		return nil, errors.New("encoded polyline string is empty")
	}

	// Use go-polyline library to decode
	coords, _, err := polyline.DecodeCoords([]byte(encoded))
	if err != nil {
		return nil, errors.New("failed to decode polyline: " + err.Error())
	}

	points := make([]Point, len(coords))
	for i, coord := range coords {
		points[i] = Point{
			Latitude:  coord[0],
			Longitude: coord[1],
		}

		// Validate decoded coordinates
		if !isValidCoordinate(points[i]) {
			return nil, errors.New("decoded polyline contains invalid coordinates")
		}
	}

	return points, nil
}

// ClosestPointOnPolyline finds closest point on polyline to given point
func (g *geoUtils) ClosestPointOnPolyline(point Point, polyline Polyline) (Point, error) {
	if !isValidCoordinate(point) {
		return Point{}, errors.New("invalid point coordinates")
	}

	if len(polyline.Points) == 0 {
		return Point{}, errors.New("polyline has no points")
	}

	if len(polyline.Points) == 1 {
		return polyline.Points[0], nil
	}

	var closestPoint Point
	minDistance := math.Inf(1)

	// Check closest point on each segment
	for i := 0; i < len(polyline.Points)-1; i++ {
		segmentStart := polyline.Points[i]
		segmentEnd := polyline.Points[i+1]

		closestOnSegment := g.closestPointOnSegment(point, segmentStart, segmentEnd)
		distance, _ := g.PointToPoint(point, closestOnSegment)

		if distance < minDistance {
			minDistance = distance
			closestPoint = closestOnSegment
		}
	}

	return closestPoint, nil
}

// closestPointOnSegment finds the closest point on a line segment to a given point
func (g *geoUtils) closestPointOnSegment(point, segmentStart, segmentEnd Point) Point {
	// If segment is just a point
	if segmentStart.Latitude == segmentEnd.Latitude && segmentStart.Longitude == segmentEnd.Longitude {
		return segmentStart
	}

	// For simplicity with geographic coordinates, find the closest endpoint
	// A more accurate implementation would project the point onto the great circle
	distToStart, _ := g.PointToPoint(point, segmentStart)
	distToEnd, _ := g.PointToPoint(point, segmentEnd)

	if distToStart <= distToEnd {
		return segmentStart
	}
	return segmentEnd
}

// FilterPointsByDistance filters points to those within specified distance of center point
func (g *geoUtils) FilterPointsByDistance(points []Point, center Point, maxDistanceMeters float64) ([]Point, error) {
	if !isValidCoordinate(center) {
		return nil, errors.New("invalid center point coordinates")
	}

	var filteredPoints []Point

	for _, point := range points {
		if !isValidCoordinate(point) {
			continue // Skip invalid points
		}

		distance, err := g.PointToPoint(center, point)
		if err != nil {
			continue // Skip points that cause calculation errors
		}

		if distance <= maxDistanceMeters {
			filteredPoints = append(filteredPoints, point)
		}
	}

	return filteredPoints, nil
}

// DistanceFromCoords calculates distance between two coordinate pairs
// Convenience method for raw latitude/longitude values
func (g *geoUtils) DistanceFromCoords(lat1, lon1, lat2, lon2 float64) (float64, error) {
	point1 := Point{Latitude: lat1, Longitude: lon1}
	point2 := Point{Latitude: lat2, Longitude: lon2}

	return g.PointToPoint(point1, point2)
}

// isValidCoordinate validates latitude and longitude values
func isValidCoordinate(point Point) bool {
	return point.Latitude >= -90 && point.Latitude <= 90 &&
		point.Longitude >= -180 && point.Longitude <= 180
}
