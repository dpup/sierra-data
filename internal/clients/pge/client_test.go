package pge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// routeDoer answers by URL path substring, so one fake can serve the two outage
// layers, PSPS, and the ETL stamp table in a single call to GetOutages.
type routeDoer struct {
	routes   map[string]string // path fragment -> body
	status   map[string]int    // path fragment -> status (default 200)
	lastURLs []string
}

func (d *routeDoer) Do(req *http.Request) (*http.Response, error) {
	d.lastURLs = append(d.lastURLs, req.URL.String())
	for frag, body := range d.routes {
		if strings.Contains(req.URL.Path, frag) {
			code := 200
			if c, ok := d.status[frag]; ok {
				code = c
			}
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("no route")), Header: make(http.Header)}, nil
}

func (d *routeDoer) urlsContaining(frag string) []string {
	var out []string
	for _, u := range d.lastURLs {
		if strings.Contains(u, frag) {
			out = append(out, u)
		}
	}
	return out
}

// Field shapes here are copied from live responses: the outage layers publish
// epoch-MILLISECOND integers, the PSPS layer publishes RFC 3339 strings and
// STRINGIFIED counts, and the stamp table publishes a zone-less
// "2006-01-02 15:04:05".
const outagePointsSample = `{
  "type": "FeatureCollection",
  "features": [
    {"type":"Feature","properties":{
      "OUTAGE_ID":"330042","OUTAGE_CAUSE":"PLNND SHUTDOWN","CREW_CURRENT_STATUS":"Awaiting T-Man",
      "EST_CUSTOMERS":1,"OUTAGE_START":1786556836000,"LAST_UPDATE":1786570817000,"CURRENT_ETOR":1786561200000},
     "geometry":{"type":"Point","coordinates":[-120.07586,38.43929]}},
    {"type":"Feature","properties":{
      "OUTAGE_ID":"330217","OUTAGE_CAUSE":"TREE CONTACT","CREW_CURRENT_STATUS":"Crew Enroute",
      "EST_CUSTOMERS":180,"OUTAGE_START":1786568481000,"LAST_UPDATE":1786570000000,"CURRENT_ETOR":0},
     "geometry":{"type":"Point","coordinates":[-120.3,38.2]}},
    {"type":"Feature","properties":{
      "OUTAGE_ID":"","OUTAGE_CAUSE":"","CREW_CURRENT_STATUS":"","EST_CUSTOMERS":3,
      "OUTAGE_START":1786568481000,"LAST_UPDATE":1786570000000,"CURRENT_ETOR":0},
     "geometry":{"type":"Point","coordinates":[-120.4,38.4]}}
  ]
}`

// 330042 has two polygon rows (a multi-part affected area); 330900 exists only
// on the polygon layer.
const outagePolygonsSample = `{
  "type": "FeatureCollection",
  "features": [
    {"type":"Feature","properties":{
      "OUTAGE_ID":"330042","OUTAGE_CAUSE":"PLNND SHUTDOWN","CREW_CURRENT_STATUS":"Awaiting T-Man",
      "EST_CUSTOMERS":1,"OUTAGE_START":1786556836000,"LAST_UPDATE":1786570817000,"CURRENT_ETOR":1786561200000},
     "geometry":{"type":"Polygon","coordinates":[[[-120.075,38.4388],[-120.0766,38.4388],[-120.0766,38.4391],[-120.075,38.4388]]]}},
    {"type":"Feature","properties":{
      "OUTAGE_ID":"330042","OUTAGE_CAUSE":"PLNND SHUTDOWN","CREW_CURRENT_STATUS":"Awaiting T-Man",
      "EST_CUSTOMERS":1,"OUTAGE_START":1786556836000,"LAST_UPDATE":1786570817000,"CURRENT_ETOR":1786561200000},
     "geometry":{"type":"Polygon","coordinates":[[[-120.085,38.4488],[-120.0866,38.4488],[-120.0866,38.4491],[-120.085,38.4488]]]}},
    {"type":"Feature","properties":{
      "OUTAGE_ID":"330900","OUTAGE_CAUSE":"FIRE","CREW_CURRENT_STATUS":"No Access",
      "EST_CUSTOMERS":19,"OUTAGE_START":1786500000000,"LAST_UPDATE":1786570000000,"CURRENT_ETOR":0},
     "geometry":{"type":"Polygon","coordinates":[[[-120.5,38.1],[-120.6,38.1],[-120.6,38.2],[-120.5,38.1]]]}}
  ]
}`

func outageDoer() *routeDoer {
	return &routeDoer{routes: map[string]string{
		"/outages/MapServer/4/query": outagePointsSample,
		"/outages/MapServer/8/query": outagePolygonsSample,
	}}
}

func testBounds() Bounds {
	return Bounds{MinLatitude: 37.87, MaxLatitude: 38.59, MinLongitude: -120.72, MaxLongitude: -119.89}
}

func TestGetOutagesJoinsPolygons(t *testing.T) {
	doer := outageDoer()
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	outages, err := c.GetOutages(context.Background(), testBounds())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Outage{}
	for _, o := range outages {
		byID[o.ID] = o
	}
	// The blank-id row is dropped: without a stable id it cannot be tracked
	// across polls, so it would churn a new event every tick.
	if len(outages) != 3 {
		t.Fatalf("got %d outages (%v), want 3", len(outages), keysOf(byID))
	}

	// Two polygon rows for one outage combine into a single MultiPolygon event.
	got := byID["330042"]
	if !got.HasPolygon || got.GeometryType != "MultiPolygon" {
		t.Errorf("330042 geometry = %q hasPolygon=%v, want MultiPolygon/true", got.GeometryType, got.HasPolygon)
	}
	var multi []json.RawMessage
	if err := json.Unmarshal(got.GeometryCoords, &multi); err != nil {
		t.Fatalf("MultiPolygon coordinates should be an array of polygons: %v", err)
	}
	if len(multi) != 2 {
		t.Errorf("got %d polygon parts, want 2", len(multi))
	}
	if !got.Planned() {
		t.Error("PLNND SHUTDOWN should read as planned")
	}
	if got.CustomersAffected != 1 || got.CrewStatus != "Awaiting T-Man" {
		t.Errorf("330042 = %+v", got)
	}
	if !got.Start.Equal(time.UnixMilli(1786556836000).UTC()) {
		t.Errorf("start = %v", got.Start)
	}
	if got.EstimatedRestoration.IsZero() {
		t.Error("ETOR should parse from epoch ms")
	}

	// An outage with no polygon keeps its point geometry.
	if p := byID["330217"]; p.HasPolygon || p.GeometryType != "Point" {
		t.Errorf("330217 geometry = %q hasPolygon=%v, want Point/false", p.GeometryType, p.HasPolygon)
	}
	if byID["330217"].Planned() {
		t.Error("TREE CONTACT should not read as planned")
	}

	// An outage present ONLY on the polygon layer is still reported — dropping it
	// would under-report an outage PG&E is publishing.
	po := byID["330900"]
	if po.ID == "" || !po.HasPolygon {
		t.Errorf("polygon-only outage missing or ungeometried: %+v", po)
	}
	if po.Cause != "FIRE" || po.CustomersAffected != 19 {
		t.Errorf("polygon-only attrs not carried: %+v", po)
	}

	// Spatial filtering (PG&E leaves COUNTY null on most rows, so the envelope is
	// the only scoping that works) and the 5-dp coordinate convention.
	for _, u := range doer.urlsContaining("/outages/") {
		if !strings.Contains(u, "esriGeometryEnvelope") || !strings.Contains(u, "esriSpatialRelIntersects") {
			t.Errorf("outage query missing spatial envelope filter: %s", u)
		}
		if !strings.Contains(u, "geometryPrecision=5") {
			t.Errorf("outage query missing geometryPrecision: %s", u)
		}
	}
}

// A polygon-layer failure must fail the WHOLE fetch. Degrading to point-only
// geometry would flip an outage's geometry across ticks, and geometry is in the
// event content hash — every blip would mint a spurious revision pair.
func TestGetOutagesFailsWhenPolygonLayerFails(t *testing.T) {
	doer := outageDoer()
	doer.routes["/outages/MapServer/8/query"] = `{"error":{"code":429,"message":"Too Many Requests"}}`
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	if _, err := c.GetOutages(context.Background(), testBounds()); err == nil {
		t.Fatal("expected an error when the polygon layer fails, got nil")
	} else if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should carry the ArcGIS code: %v", err)
	}
}

// ArcGIS reports quota/throttle/token failures as HTTP 200 + an error envelope.
// An outage layer that silently reads empty is an all-clear we did not earn.
func TestOutagesArcGISErrorEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":499,"message":"Token Required"}}`,
		// Error wins even when a (stale/garbage) features array rides along.
		`{"error":{"code":403,"message":"forbidden"},"features":[]}`,
	} {
		doer := outageDoer()
		doer.routes["/outages/MapServer/4/query"] = body
		c := NewClientWithHTTPDoer("https://pge.test/43", doer)
		if _, err := c.GetOutages(context.Background(), testBounds()); err == nil {
			t.Fatalf("error envelope must surface as an error: %s", body)
		}
	}
}

func TestGetOutagesHTTPError(t *testing.T) {
	doer := outageDoer()
	doer.status = map[string]int{"/outages/MapServer/4/query": 503}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)
	if _, err := c.GetOutages(context.Background(), testBounds()); err == nil {
		t.Fatal("a non-2xx must surface as an error, not an empty list")
	}
}

// A clean fetch with no rows is a genuine "nothing out" — distinct from the
// failures above, and only meaningful because those error.
func TestGetOutagesCleanEmpty(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{
		"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[]}`,
		"/outages/MapServer/8/query": `{"type":"FeatureCollection","features":[]}`,
	}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)
	outages, err := c.GetOutages(context.Background(), testBounds())
	if err != nil {
		t.Fatalf("a clean empty fetch must not error: %v", err)
	}
	if len(outages) != 0 {
		t.Errorf("got %d outages, want 0", len(outages))
	}
}

const pspsSample = `{
  "type": "FeatureCollection",
  "features": [
    {"type":"Feature","properties":{
      "EventID":"20725","EventName":"PSPS_05172026","TimePeriod":"TP02_05172026","Stage":"Warning",
      "TotCustAff":"74786","TotMBLAff":"5623",
      "DeEngStart":"2026-05-17T13:00:00Z","DeEngEnd":"2026-05-19T15:00:00Z",
      "AllClear":"2026-05-19T18:00:00Z","ETOR":"2026-05-20T16:00:00Z","LstUpdated":"2026-05-16T07:43:13Z"},
     "geometry":{"type":"MultiPolygon","coordinates":[[[[-120.68,37.95],[-120.6,37.95],[-120.6,38.0],[-120.68,37.95]]]]}},
    {"type":"Feature","properties":{
      "EventID":"20725","EventName":"PSPS_05172026","TimePeriod":"TP02_05172026","Stage":"Warning",
      "TotCustAff":"74786","TotMBLAff":"5623",
      "DeEngStart":"2026-05-17T13:00:00Z","DeEngEnd":"2026-05-19T15:00:00Z",
      "AllClear":"2026-05-19T18:00:00Z","ETOR":"2026-05-20T16:00:00Z","LstUpdated":"2026-05-16T07:43:13Z"},
     "geometry":{"type":"Polygon","coordinates":[[[-120.4,38.3],[-120.3,38.3],[-120.3,38.4],[-120.4,38.3]]]}}
  ]
}`

func TestGetPSPSAreas(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{"/psps_public/MapServer/1/query": pspsSample}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	areas, err := c.GetPSPSAreas(context.Background(), testBounds())
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) != 2 {
		t.Fatalf("got %d areas, want 2", len(areas))
	}
	a := areas[0]
	// PG&E publishes these counts as STRINGS on this layer.
	if a.CustomersAffected != 74786 || a.MedicalBaselineAffected != 5623 {
		t.Errorf("stringified counts not parsed: %+v", a)
	}
	if a.Stage != "Warning" || a.EventID != "20725" || a.TimePeriod != "TP02_05172026" {
		t.Errorf("area = %+v", a)
	}
	want := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	if !a.DeEnergizationStart.Equal(want) {
		t.Errorf("DeEnergizationStart = %v, want %v", a.DeEnergizationStart, want)
	}
	if a.DeEnergizationEnd.IsZero() || a.EstimatedRestoration.IsZero() || a.LastUpdated.IsZero() {
		t.Errorf("RFC 3339 stamps not parsed: %+v", a)
	}

	// County-scale PSPS polygons are simplified server-side; without this the
	// same response measured 36x larger.
	urls := doer.urlsContaining("/psps_public/")
	if len(urls) != 1 || !strings.Contains(urls[0], "maxAllowableOffset") {
		t.Errorf("PSPS query should simplify geometry: %v", urls)
	}
}

// Rows sharing an event window merge into one geometry; a MultiPolygon's
// members splice in flat rather than nesting.
func TestCombineAreaGeometry(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{"/psps_public/MapServer/1/query": pspsSample}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)
	areas, err := c.GetPSPSAreas(context.Background(), testBounds())
	if err != nil {
		t.Fatal(err)
	}

	typ, coords, err := CombineAreaGeometry(areas)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "MultiPolygon" {
		t.Fatalf("type = %q, want MultiPolygon", typ)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(coords, &parts); err != nil {
		t.Fatal(err)
	}
	// 1 member spliced from the MultiPolygon row + 1 from the Polygon row.
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (flat, not nested)", len(parts))
	}
	// A flat member is a polygon: an array of rings of [lng,lat] pairs.
	var rings [][][]float64
	if err := json.Unmarshal(parts[0], &rings); err != nil {
		t.Errorf("member is not a polygon coordinate array (nesting bug?): %v", err)
	}
}

func TestCombineAreaGeometrySingleRow(t *testing.T) {
	typ, coords, err := CombineAreaGeometry([]PSPSArea{{
		GeometryType:   "Polygon",
		GeometryCoords: json.RawMessage(`[[[-120.4,38.3],[-120.3,38.3],[-120.3,38.4],[-120.4,38.3]]]`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if typ != "Polygon" {
		t.Errorf("a single row should stay a Polygon, got %q", typ)
	}
	if len(coords) == 0 {
		t.Error("coordinates dropped")
	}
}

func TestCombineAreaGeometryNoUsableGeometry(t *testing.T) {
	if _, _, err := CombineAreaGeometry([]PSPSArea{{GeometryType: "Point", GeometryCoords: json.RawMessage(`[-120.4,38.3]`)}}); err == nil {
		t.Fatal("a non-polygon geometry should not silently become a coverage area")
	}
}

func TestGetOutagesLastUpdate(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{
		"/lastupdate_time/MapServer/1/query": `{"features":[{"attributes":{"OBJECTID":1,"LAST_UPDATE":"2026-08-13 01:14:49"}}]}`,
	}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	ts, err := c.GetOutagesLastUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The stamp has NO zone marker and is UTC; parsing it in the local zone
	// would silently shift the freshness gate by the offset.
	want := time.Date(2026, 8, 13, 1, 14, 49, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("stamp = %v, want %v", ts, want)
	}
}

func TestGetOutagesLastUpdateFailures(t *testing.T) {
	cases := map[string]string{
		"no rows":     `{"features":[]}`,
		"unparseable": `{"features":[{"attributes":{"LAST_UPDATE":"not a time"}}]}`,
		"error enve":  `{"error":{"code":500,"message":"boom"}}`,
		"blank stamp": `{"features":[{"attributes":{"LAST_UPDATE":""}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			doer := &routeDoer{routes: map[string]string{"/lastupdate_time/MapServer/1/query": body}}
			c := NewClientWithHTTPDoer("https://pge.test/43", doer)
			// An unreadable stamp must error rather than return the zero time,
			// which the caller would read as "infinitely stale" and use to
			// declare a healthy feed frozen.
			if _, err := c.GetOutagesLastUpdate(context.Background()); err == nil {
				t.Fatalf("expected an error for %q", body)
			}
		})
	}
}

func TestPlannedCauseVariants(t *testing.T) {
	cases := []struct {
		cause string
		want  bool
	}{
		{"PLNND SHUTDOWN", true},
		{"plnnd shutdown", true},
		{"PLANNED OUTAGE", true},
		{"SCHED MAINT", true},
		{"TREE CONTACT", false},
		{"FIRE", false},
		{"", false},
		{"EMERG REPAIRS", false},
	}
	for _, tc := range cases {
		if got := (Outage{Cause: tc.cause}).Planned(); got != tc.want {
			t.Errorf("Planned(%q) = %v, want %v", tc.cause, got, tc.want)
		}
	}
}

func keysOf(m map[string]Outage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCombinedGeometryIsOrderIndependent: ArcGIS makes no ordering promise
// across queries, and the combined geometry bytes are part of the stored
// event's CONTENT HASH. If the same polygons came back swapped on the next
// poll, an outage that never changed would mint a revision — and keep
// flip-flopping one every time the upstream order did.
func TestCombinedGeometryIsOrderIndependent(t *testing.T) {
	const rowA = `{"type":"Feature","properties":{"OUTAGE_ID":"330042","OUTAGE_CAUSE":"FIRE",
      "CREW_CURRENT_STATUS":"","EST_CUSTOMERS":5,"OUTAGE_START":1786556836000,
      "LAST_UPDATE":1786570817000,"CURRENT_ETOR":0},
      "geometry":{"type":"Polygon","coordinates":[[[-120.075,38.4388],[-120.0766,38.4388],[-120.0766,38.4391],[-120.075,38.4388]]]}}`
	const rowB = `{"type":"Feature","properties":{"OUTAGE_ID":"330042","OUTAGE_CAUSE":"FIRE",
      "CREW_CURRENT_STATUS":"","EST_CUSTOMERS":5,"OUTAGE_START":1786556836000,
      "LAST_UPDATE":1786570817000,"CURRENT_ETOR":0},
      "geometry":{"type":"Polygon","coordinates":[[[-120.085,38.4488],[-120.0866,38.4488],[-120.0866,38.4491],[-120.085,38.4488]]]}}`

	fetch := func(polygons string) string {
		doer := &routeDoer{routes: map[string]string{
			"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[]}`,
			"/outages/MapServer/8/query": polygons,
		}}
		c := NewClientWithHTTPDoer("https://pge.test/43", doer)
		out, err := c.GetOutages(context.Background(), testBounds())
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d outages, want 1", len(out))
		}
		return string(out[0].GeometryCoords)
	}

	forward := fetch(`{"type":"FeatureCollection","features":[` + rowA + `,` + rowB + `]}`)
	reversed := fetch(`{"type":"FeatureCollection","features":[` + rowB + `,` + rowA + `]}`)
	if forward != reversed {
		t.Errorf("upstream row order changed the geometry bytes:\n forward  = %s\n reversed = %s", forward, reversed)
	}
}

// The same guarantee for the PSPS grouping path, which shares the merge.
func TestCombineAreaGeometryIsOrderIndependent(t *testing.T) {
	a := PSPSArea{GeometryType: "Polygon", GeometryCoords: json.RawMessage(`[[[-120.4,38.3],[-120.3,38.3],[-120.3,38.4],[-120.4,38.3]]]`)}
	b := PSPSArea{GeometryType: "Polygon", GeometryCoords: json.RawMessage(`[[[-120.6,38.1],[-120.5,38.1],[-120.5,38.2],[-120.6,38.1]]]`)}

	_, fwd, err := CombineAreaGeometry([]PSPSArea{a, b})
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := CombineAreaGeometry([]PSPSArea{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if string(fwd) != string(rev) {
		t.Errorf("row order changed the merged geometry:\n %s\n %s", fwd, rev)
	}
}

// TestGetOutagesSchemaBreakIsNotAnAllClear is the highest-stakes guard in this
// package. These endpoints are undocumented, so a field rename is a matter of
// when: encoding/json would ignore the unknown key, leave every OUTAGE_ID
// blank, and this function would return a clean, EMPTY, no-error result. The
// caller's disappearance policy is `resolve`, so that empty result publishes
// "power restored" for every stored outage in the region. An unreadable feed
// must not look like an empty one.
func TestGetOutagesSchemaBreakIsNotAnAllClear(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{
		"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[
		  {"type":"Feature","properties":{"OUTAGEID":"330042","EST_CUSTOMERS":1400},
		   "geometry":{"type":"Point","coordinates":[-120.3,38.2]}},
		  {"type":"Feature","properties":{"OUTAGEID":"330043","EST_CUSTOMERS":900},
		   "geometry":{"type":"Point","coordinates":[-120.4,38.3]}}]}`,
		"/outages/MapServer/8/query": `{"type":"FeatureCollection","features":[]}`,
	}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	out, err := c.GetOutages(context.Background(), testBounds())
	if err == nil {
		t.Fatalf("rows with no usable id must error, not return %d outages and nil", len(out))
	}
	if !strings.Contains(err.Error(), "OUTAGE_ID") {
		t.Errorf("the error should name the field that vanished: %v", err)
	}
	// It must stay distinguishable from a genuinely empty feed, which
	// TestGetOutagesCleanEmpty pins as a non-error.
}

// TestGetOutagesTruncatedResponseIsAnError: past the layer's maxRecordCount
// ArcGIS returns the first N rows and sets exceededTransferLimit — a partial
// answer that looks exactly like a complete one. The caller resolves anything
// missing from what it gets, so accepting a truncated page would RESOLVE the
// entire tail during the widespread outage that caused the truncation.
func TestGetOutagesTruncatedResponseIsAnError(t *testing.T) {
	doer := outageDoer()
	doer.routes["/outages/MapServer/4/query"] = `{"type":"FeatureCollection","exceededTransferLimit":true,"features":[
	  {"type":"Feature","properties":{"OUTAGE_ID":"330042","EST_CUSTOMERS":1},
	   "geometry":{"type":"Point","coordinates":[-120.3,38.2]}}]}`
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	if _, err := c.GetOutages(context.Background(), testBounds()); err == nil {
		t.Fatal("a truncated response must not be treated as the complete current set")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should say the response was truncated: %v", err)
	}
}

func TestGetPSPSAreasTruncatedResponseIsAnError(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{
		"/psps_public/MapServer/1/query": `{"type":"FeatureCollection","exceededTransferLimit":true,"features":[]}`,
	}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)
	if _, err := c.GetPSPSAreas(context.Background(), testBounds()); err == nil {
		t.Fatal("a truncated PSPS response must not read as a complete coverage set")
	}
}

// TestGetOutagesIDWhitespaceIsNormalized: the join key, the dedup key and the
// emitted id must all be the SAME string. Keying the map on the raw value while
// emitting a trimmed one would both miss the polygon join (the outage silently
// loses its affected area) and emit two rows sharing one id (which the store
// would then have overwrite each other every poll). Padding can appear on
// either layer, so all three combinations are covered.
func TestGetOutagesIDWhitespaceIsNormalized(t *testing.T) {
	cases := map[string]struct{ point, polygon string }{
		"polygon padded": {"330042", " 330042"},
		"point padded":   {" 330042", "330042"},
		"both padded":    {" 330042 ", " 330042"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doer := &routeDoer{routes: map[string]string{
				"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[
				  {"type":"Feature","properties":{"OUTAGE_ID":"` + tc.point + `","EST_CUSTOMERS":10},
				   "geometry":{"type":"Point","coordinates":[-120.3,38.2]}}]}`,
				"/outages/MapServer/8/query": `{"type":"FeatureCollection","features":[
				  {"type":"Feature","properties":{"OUTAGE_ID":"` + tc.polygon + `","EST_CUSTOMERS":10},
				   "geometry":{"type":"Polygon","coordinates":[[[-120.3,38.2],[-120.2,38.2],[-120.2,38.3],[-120.3,38.2]]]}}]}`,
			}}
			c := NewClientWithHTTPDoer("https://pge.test/43", doer)

			out, err := c.GetOutages(context.Background(), testBounds())
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d outages, want 1 (a padded id must join, not duplicate)", len(out))
			}
			if out[0].ID != "330042" {
				t.Errorf("id = %q, want the trimmed %q", out[0].ID, "330042")
			}
			if !out[0].HasPolygon {
				t.Error("the padded polygon row should have joined onto the point")
			}
		})
	}
}

// TestGetOutagesPolygonLayerSchemaBreakIsAnError is the quiet half of the
// schema-break guard, and the reason it is applied PER LAYER rather than to the
// whole result. If OUTAGE_ID breaks on the POLYGON layer only, the point rows
// still carry ids, so a "did we end up with zero outages" check stays silent —
// while every outage silently reverts polygon -> point geometry. Geometry is in
// the event content hash, so that mints a spurious revision for every stored
// outage, and another when the layer recovers.
func TestGetOutagesPolygonLayerSchemaBreakIsAnError(t *testing.T) {
	doer := outageDoer()
	doer.routes["/outages/MapServer/8/query"] = `{"type":"FeatureCollection","features":[
	  {"type":"Feature","properties":{"OUTAGEID":"330042"},
	   "geometry":{"type":"Polygon","coordinates":[[[-120.3,38.2],[-120.2,38.2],[-120.2,38.3],[-120.3,38.2]]]}}]}`
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	out, err := c.GetOutages(context.Background(), testBounds())
	if err == nil {
		t.Fatalf("a polygon-layer schema break must error, not silently return %d point-only outages", len(out))
	}
	if !strings.Contains(err.Error(), "polygons") {
		t.Errorf("the error should name the broken layer: %v", err)
	}
}

// A polygon layer that is legitimately EMPTY is not a schema break — the guard
// keys on "rows returned but none usable", not on emptiness.
func TestGetOutagesEmptyPolygonLayerIsNotASchemaBreak(t *testing.T) {
	doer := outageDoer()
	doer.routes["/outages/MapServer/8/query"] = `{"type":"FeatureCollection","features":[]}`
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	out, err := c.GetOutages(context.Background(), testBounds())
	if err != nil {
		t.Fatalf("an empty polygon layer is normal, not an error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d outages, want the 2 identified point rows", len(out))
	}
	for _, o := range out {
		if o.HasPolygon {
			t.Errorf("%s should have fallen back to its point geometry", o.ID)
		}
	}
}

// TestGetOutagesDeduplicatesPointRows: the polygon layer publishes multi-row
// outages, so duplicate point rows are plausible too. Emitting both would hand
// the store two events under ONE id, which it would have overwrite each other
// every tick — flip-flopping the served record forever, since ArcGIS promises
// no row ordering.
func TestGetOutagesDeduplicatesPointRows(t *testing.T) {
	doer := &routeDoer{routes: map[string]string{
		"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[
		  {"type":"Feature","properties":{"OUTAGE_ID":"330042","EST_CUSTOMERS":10,"CREW_CURRENT_STATUS":"Awaiting Crew"},
		   "geometry":{"type":"Point","coordinates":[-120.3,38.2]}},
		  {"type":"Feature","properties":{"OUTAGE_ID":"330042","EST_CUSTOMERS":11,"CREW_CURRENT_STATUS":"Crew On Site"},
		   "geometry":{"type":"Point","coordinates":[-120.31,38.21]}}]}`,
		"/outages/MapServer/8/query": `{"type":"FeatureCollection","features":[]}`,
	}}
	c := NewClientWithHTTPDoer("https://pge.test/43", doer)

	out, err := c.GetOutages(context.Background(), testBounds())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d outages for one OUTAGE_ID, want 1 — two events cannot share an id", len(out))
	}
}

// TestTruncationFlagDetectedWhenNested: ArcGIS hangs the truncation flag off a
// FeatureCollection as a foreign member, and PG&E's current server emits it BOTH
// top-level and nested under `properties`. Other ArcGIS versions emit only the
// nested form — and a guard that silently stops firing is worse than none,
// because what it protects against (a partial set read as complete, resolving
// the truncated tail) is invisible from the response itself.
func TestTruncationFlagDetectedWhenNested(t *testing.T) {
	shapes := map[string]string{
		"top-level only": `{"type":"FeatureCollection","exceededTransferLimit":true,"features":[]}`,
		"nested only":    `{"type":"FeatureCollection","properties":{"exceededTransferLimit":true},"features":[]}`,
		"both (PG&E today)": `{"type":"FeatureCollection","exceededTransferLimit":true,
		  "properties":{"exceededTransferLimit":true},"features":[]}`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			doer := outageDoer()
			doer.routes["/outages/MapServer/4/query"] = body
			c := NewClientWithHTTPDoer("https://pge.test/43", doer)
			if _, err := c.GetOutages(context.Background(), testBounds()); err == nil {
				t.Fatal("truncation must be detected in this response shape")
			}

			pd := &routeDoer{routes: map[string]string{"/psps_public/MapServer/1/query": body}}
			pc := NewClientWithHTTPDoer("https://pge.test/43", pd)
			if _, err := pc.GetPSPSAreas(context.Background(), testBounds()); err == nil {
				t.Fatal("PSPS truncation must be detected in this response shape too")
			}
		})
	}

	// An untruncated response is still fine.
	doer := outageDoer()
	if _, err := NewClientWithHTTPDoer("https://pge.test/43", doer).GetOutages(context.Background(), testBounds()); err != nil {
		t.Fatalf("a normal response must not trip the guard: %v", err)
	}
}

// TestPolygonOnlyOutageAttributesAreOrderIndependent: a polygon-only outage
// takes its non-geometry attributes from the group too, and those (cause, crew
// status, customer count, ETOR) are in the event content hash — so picking
// "whichever row ArcGIS returned first" would mint a spurious revision pair
// every time a multi-row outage came back reordered.
func TestPolygonOnlyOutageAttributesAreOrderIndependent(t *testing.T) {
	row := func(cause, crew string, cust int, lng, lat float64) string {
		return fmt.Sprintf(`{"type":"Feature","properties":{"OUTAGE_ID":"330900","OUTAGE_CAUSE":%q,
		  "CREW_CURRENT_STATUS":%q,"EST_CUSTOMERS":%d,"OUTAGE_START":1786500000000,
		  "LAST_UPDATE":1786570000000,"CURRENT_ETOR":0},
		  "geometry":{"type":"Polygon","coordinates":[[[%v,%v],[%v,%v],[%v,%v],[%v,%v]]]}}`,
			cause, crew, cust, lng, lat, lng+0.01, lat, lng+0.01, lat+0.01, lng, lat)
	}
	a := row("FIRE", "No Access", 19, -120.5, 38.1)
	b := row("TREE CONTACT", "Crew On Site", 21, -120.6, 38.2)

	fetch := func(polys string) Outage {
		doer := &routeDoer{routes: map[string]string{
			"/outages/MapServer/4/query": `{"type":"FeatureCollection","features":[]}`,
			"/outages/MapServer/8/query": polys,
		}}
		out, err := NewClientWithHTTPDoer("https://pge.test/43", doer).GetOutages(context.Background(), testBounds())
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d outages, want 1", len(out))
		}
		return out[0]
	}

	fwd := fetch(`{"type":"FeatureCollection","features":[` + a + `,` + b + `]}`)
	rev := fetch(`{"type":"FeatureCollection","features":[` + b + `,` + a + `]}`)
	if fwd.Cause != rev.Cause || fwd.CrewStatus != rev.CrewStatus || fwd.CustomersAffected != rev.CustomersAffected {
		t.Errorf("attributes changed with feed order:\n forward  = %+v\n reversed = %+v", fwd, rev)
	}
	if string(fwd.GeometryCoords) != string(rev.GeometryCoords) {
		t.Error("geometry changed with feed order")
	}
}
