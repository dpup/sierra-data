package geojson

import "testing"

func TestIntersects(t *testing.T) {
	// A right triangle covering the SW half of the 0..10 box: the point (9,9) and
	// the whole SE corner (x+y>10) are OUTSIDE it.
	triangle := `{"type":"Polygon","coordinates":[[[0,0],[10,0],[0,10],[0,0]]]}`
	bigBox := `{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}`

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{
			// The Dove case: a small box in the triangle's SE corner. Its bbox is
			// inside the triangle's bbox (overlap), but it lies entirely outside the
			// triangle itself → must NOT intersect.
			name: "bbox overlaps but polygons disjoint",
			a:    `{"type":"Polygon","coordinates":[[[8,8],[9,8],[9,9],[8,9],[8,8]]]}`,
			b:    triangle,
			want: false,
		},
		{
			name: "small box fully inside big box",
			a:    `{"type":"Polygon","coordinates":[[[2,2],[3,2],[3,3],[2,3],[2,2]]]}`,
			b:    bigBox,
			want: true,
		},
		{
			name: "boxes share a corner region (partial overlap)",
			a:    `{"type":"Polygon","coordinates":[[[8,8],[12,8],[12,12],[8,12],[8,8]]]}`,
			b:    bigBox,
			want: true,
		},
		{
			// A plus/cross: horizontal bar vs vertical bar. No vertex of either lies
			// inside the other, so only edge-crossing detects it.
			name: "edge crossing with no vertex inside (plus shape)",
			a:    `{"type":"Polygon","coordinates":[[[1,4],[5,4],[5,6],[1,6],[1,4]]]}`,
			b:    `{"type":"Polygon","coordinates":[[[3,1],[4,1],[4,9],[3,9],[3,1]]]}`,
			want: true,
		},
		{
			name: "fully disjoint (non-overlapping bboxes)",
			a:    `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}`,
			b:    `{"type":"Polygon","coordinates":[[[5,5],[6,5],[6,6],[5,6],[5,5]]]}`,
			want: false,
		},
		{
			name: "multipolygon: one part overlaps",
			a:    `{"type":"MultiPolygon","coordinates":[[[[20,20],[21,20],[21,21],[20,21],[20,20]]],[[[2,2],[3,2],[3,3],[2,3],[2,2]]]]}`,
			b:    bigBox,
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := mustParse(t, c.a), mustParse(t, c.b)
			if got := Intersects(a, b); got != c.want {
				t.Errorf("Intersects = %v, want %v", got, c.want)
			}
			// Symmetric.
			if got := Intersects(b, a); got != c.want {
				t.Errorf("Intersects (swapped) = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIntersects_NonAreal(t *testing.T) {
	poly := mustParse(t, `{"type":"Polygon","coordinates":[[[0,0],[10,0],[10,10],[0,10],[0,0]]]}`)
	point := mustParse(t, `{"type":"Point","coordinates":[5,5]}`)
	line := mustParse(t, `{"type":"LineString","coordinates":[[0,0],[10,10]]}`)
	if Intersects(poly, point) || Intersects(point, poly) {
		t.Error("point is not areal; Intersects must be false")
	}
	if Intersects(poly, line) || Intersects(line, poly) {
		t.Error("linestring is not areal; Intersects must be false")
	}
	if Intersects(nil, poly) || Intersects(poly, nil) {
		t.Error("nil geometry must be false")
	}
}
