package geojson

import "math"

// overlapGrid is the sample grid's side for OverlapFraction: 64x64 points
// over the two shapes' common bbox.
const overlapGrid = 64

// OverlapFraction estimates how much of the smaller of two areal geometries
// lies inside the other, from 0 to 1, by sampling a grid over their common
// bounding box. It separates a real overlap from the sliver two
// independently drawn outlines of one border make (an NWS zone and a Census
// county share a line but not its vertices), which Intersects counts as
// touching. Non-areal input returns 0.
func OverlapFraction(a, b *Geom) float64 {
	if !isAreal(a) || !isAreal(b) {
		return 0
	}
	aMinLat, aMinLng, aMaxLat, aMaxLng := a.Bbox()
	bMinLat, bMinLng, bMaxLat, bMaxLng := b.Bbox()
	minLat, maxLat := math.Max(aMinLat, bMinLat), math.Min(aMaxLat, bMaxLat)
	minLng, maxLng := math.Max(aMinLng, bMinLng), math.Min(aMaxLng, bMaxLng)
	if minLat >= maxLat || minLng >= maxLng {
		return 0
	}
	both := sampleCount(minLat, minLng, maxLat, maxLng, a, b)
	if both == 0 {
		return 0
	}
	// Each shape's own area in the same units: grid cells of its own bbox,
	// scaled to the common grid's cell size.
	common := (maxLat - minLat) * (maxLng - minLng)
	areaOf := func(g *Geom, lo1, lo2, hi1, hi2 float64) float64 {
		n := sampleCount(lo1, lo2, hi1, hi2, g, nil)
		return float64(n) * (hi1 - lo1) * (hi2 - lo2) / common
	}
	smaller := math.Min(areaOf(a, aMinLat, aMinLng, aMaxLat, aMaxLng), areaOf(b, bMinLat, bMinLng, bMaxLat, bMaxLng))
	if smaller <= 0 {
		return 0
	}
	return math.Min(1, float64(both)/smaller)
}

// sampleCount counts the cell-centre points of an overlapGrid grid over the
// box that lie inside a (and inside b, when b is not nil).
func sampleCount(minLat, minLng, maxLat, maxLng float64, a, b *Geom) int {
	dLat, dLng := (maxLat-minLat)/overlapGrid, (maxLng-minLng)/overlapGrid
	n := 0
	for i := 0; i < overlapGrid; i++ {
		lat := minLat + (float64(i)+0.5)*dLat
		for j := 0; j < overlapGrid; j++ {
			lng := minLng + (float64(j)+0.5)*dLng
			if PointInGeometry(lat, lng, a) && (b == nil || PointInGeometry(lat, lng, b)) {
				n++
			}
		}
	}
	return n
}
