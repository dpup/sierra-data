package caloes

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDoer struct {
	resp    string
	lastURL string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastURL = req.URL.String()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(f.resp)), Header: make(http.Header)}, nil
}

const sample = `{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "properties": {
        "ZONE_ID": "CAL-E063",
        "ZONE_NAME": "Hathaway Pines",
        "COUNTY": "Calaveras",
        "STATUS": "Evacuation Order",
        "EVENT_TYPE": "Fire",
        "PUBLIC_INFO": "Leave now.",
        "STATEWIDE_LAST_UPDATED": 1782400000000
      },
      "geometry": { "type": "Polygon", "coordinates": [[[-120.4,38.2],[-120.3,38.2],[-120.3,38.3],[-120.4,38.2]]] }
    }
  ]
}`

func TestGetActiveEvacuations(t *testing.T) {
	doer := &fakeDoer{resp: sample}
	c := NewClientWithHTTPDoer("https://caloes.test/query", doer)

	zones, err := c.GetActiveEvacuations(context.Background(), Bounds{
		MinLatitude: 37.8, MaxLatitude: 38.55, MinLongitude: -120.9, MaxLongitude: -120.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 {
		t.Fatalf("got %d, want 1", len(zones))
	}
	z := zones[0]
	if z.ZoneID != "CAL-E063" || z.Status != "Evacuation Order" || z.County != "Calaveras" {
		t.Errorf("zone = %+v", z)
	}
	if z.LastUpdated.IsZero() {
		t.Error("last updated should parse from ms epoch")
	}
	if z.GeometryType != "Polygon" || len(z.GeometryCoords) == 0 {
		t.Errorf("geometry = %s / %s", z.GeometryType, z.GeometryCoords)
	}
	// Filtering is spatial (envelope intersect), so any in-area zone is caught
	// regardless of its COUNTY tag.
	if !strings.Contains(doer.lastURL, "esriGeometryEnvelope") || !strings.Contains(doer.lastURL, "esriSpatialRelIntersects") {
		t.Errorf("query missing spatial envelope filter: %s", doer.lastURL)
	}
}

// TestArcGISErrorEnvelope: ArcGIS returns HTTP 200 with an error envelope on
// quota/throttle failures. For this life-safety feed that MUST surface as an
// error (→ UNAVAILABLE/unknown), never as an empty all-clear.
func TestArcGISErrorEnvelope(t *testing.T) {
	doer := &fakeDoer{resp: `{"error":{"code":499,"message":"Token Required"}}`}
	c := NewClientWithHTTPDoer("https://caloes.test/query", doer)
	_, err := c.GetActiveEvacuations(context.Background(), Bounds{})
	if err == nil {
		t.Fatal("expected an error for an ArcGIS 200-with-error-envelope response, got nil")
	}
	if !strings.Contains(err.Error(), "499") {
		t.Errorf("error should carry the ArcGIS code: %v", err)
	}
}

func TestArcGISErrorEnvelopeAttacker(t *testing.T) {
	// The error-envelope check also covers the case where the upstream returns an
	// error AND a (stale/garbage) features array — error must win.
	doer := &fakeDoer{resp: `{"error":{"code":403,"message":"forbidden"},"features":[]}`}
	c := NewClientWithHTTPDoer("https://caloes.test/query", doer)
	if _, err := c.GetActiveEvacuations(context.Background(), Bounds{}); err == nil {
		t.Fatal("error envelope must surface even when a features array is present")
	}
}

const tuolumneViewer = "https://experience.arcgis.com/experience/701c7fd899574b6ea6c1596cbbd1dcc6/page/Page?org=Tuolumne"

func TestZoneURL(t *testing.T) {
	cases := []struct {
		name, zoneID, county, want string
	}{
		// Genasys/Zonehaven-scheme ids deep-link into the zone (confirmed live:
		// US-CA-XTU-PVL-E032 resolves on protect.genasys.com).
		{"calaveras genasys", "US-CA-XCA-CCU-153", "CALAVERAS", "https://protect.genasys.com/zones/US-CA-XCA-CCU-153"},
		{"tulare genasys", "US-CA-XTU-PVL-E032", "TULARE", "https://protect.genasys.com/zones/US-CA-XTU-PVL-E032"},
		// Tuolumne runs its own vendor, so it gets its OWN county viewer — NOT
		// the generic Genasys one, which does not contain Tuolumne's zones at
		// all and so was a dead end dressed up as an authoritative link.
		// (Upstream misspells the county in the id; match on COUNTY, not the id.)
		{"tuolumne uses its own viewer", "US-CA-Toulumne101", "TUOLUMNE", tuolumneViewer},
		{"tuolumne case/space insensitive", "US-CA-Toulumne117", " tuolumne ", tuolumneViewer},
		// An unmapped non-Genasys county still falls back to the generic viewer;
		// every such county is outside this service's footprint.
		{"unmapped county", "SOMEWHERE-1", "MODOC", SourceURL},
		// Legacy / other aggregation id shapes and blanks fall back too.
		{"legacy short id", "CAL-E-046", "", SourceURL},
		{"empty", "", "", SourceURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ZoneURL(tc.zoneID, tc.county); got != tc.want {
				t.Errorf("ZoneURL(%q, %q) = %q, want %q", tc.zoneID, tc.county, got, tc.want)
			}
		})
	}
}

// TestGetActiveEvacuationsReadsCurrentSchema pins the columns Cal OES actually
// populates TODAY. This is the shape measured across all 37 live rows statewide
// on 2026-08-13: the documented text/time fields empty, the real content in
// NOTES and EditDate. Before this, the client asked for neither, so every
// evacuation we served carried an empty description and no observed_at — and
// nothing failed, because null parses fine.
func TestGetActiveEvacuationsReadsCurrentSchema(t *testing.T) {
	const currentShape = `{
	  "type": "FeatureCollection",
	  "features": [{
	    "type": "Feature",
	    "properties": {
	      "ZONE_ID": "US-CA-Toulumne117",
	      "ZONE_NAME": null,
	      "COUNTY": "TUOLUMNE",
	      "STATUS": "Evacuation Warning",
	      "EVENT_TYPE": null,
	      "PUBLIC_INFO": null,
	      "NOTES": "Southgate Dr",
	      "STATEWIDE_LAST_UPDATED": null,
	      "EditDate": 1786052055575
	    },
	    "geometry": { "type": "Polygon", "coordinates": [[[-120.4,38.0],[-120.3,38.0],[-120.3,38.1],[-120.4,38.0]]] }
	  }]
	}`
	doer := &fakeDoer{resp: currentShape}
	c := NewClientWithHTTPDoer("https://caloes.test/query", doer)

	zones, err := c.GetActiveEvacuations(context.Background(), Bounds{})
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 {
		t.Fatalf("got %d zones, want 1", len(zones))
	}
	z := zones[0]
	if z.Notes != "Southgate Dr" {
		t.Errorf("NOTES not carried: %q", z.Notes)
	}
	if z.EditedAt.IsZero() {
		t.Error("EditDate not parsed — the only freshness signal this layer populates")
	}
	if !z.LastUpdated.IsZero() {
		t.Errorf("a null STATEWIDE_LAST_UPDATED must stay zero, got %v", z.LastUpdated)
	}
	// Both columns must be requested, or they come back absent regardless of
	// what the server holds.
	for _, field := range []string{"NOTES", "EditDate"} {
		if !strings.Contains(doer.lastURL, field) {
			t.Errorf("query does not request %s: %s", field, doer.lastURL)
		}
	}
}

// The legacy fields still win when Cal OES populates them, so this survives
// them moving the text back.
func TestGetActiveEvacuationsPrefersDocumentedFields(t *testing.T) {
	const legacyShape = `{
	  "type": "FeatureCollection",
	  "features": [{
	    "type": "Feature",
	    "properties": {
	      "ZONE_ID": "US-CA-XCA-CCU-153", "ZONE_NAME": "Hathaway Pines", "COUNTY": "Calaveras",
	      "STATUS": "Evacuation Order", "PUBLIC_INFO": "Leave now.", "NOTES": "internal note",
	      "STATEWIDE_LAST_UPDATED": 1782400000000, "EditDate": 1786052055575
	    },
	    "geometry": { "type": "Polygon", "coordinates": [[[-120.4,38.2],[-120.3,38.2],[-120.3,38.3],[-120.4,38.2]]] }
	  }]
	}`
	c := NewClientWithHTTPDoer("https://caloes.test/query", &fakeDoer{resp: legacyShape})
	zones, err := c.GetActiveEvacuations(context.Background(), Bounds{})
	if err != nil {
		t.Fatal(err)
	}
	z := zones[0]
	if z.PublicInfo != "Leave now." || z.Notes != "internal note" {
		t.Errorf("both columns should be carried: %+v", z)
	}
	if z.LastUpdated.IsZero() {
		t.Error("STATEWIDE_LAST_UPDATED should parse when populated")
	}
}
