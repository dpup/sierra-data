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

func TestZoneURL(t *testing.T) {
	cases := []struct {
		name, zoneID, want string
	}{
		// Genasys/Zonehaven-scheme ids deep-link into the zone (confirmed live:
		// US-CA-XTU-PVL-E032 resolves on protect.genasys.com).
		{"calaveras genasys", "US-CA-XCA-CCU-153", "https://protect.genasys.com/zones/US-CA-XCA-CCU-153"},
		{"tulare genasys", "US-CA-XTU-PVL-E032", "https://protect.genasys.com/zones/US-CA-XTU-PVL-E032"},
		// Non-Genasys counties (Tuolumne's own vendor) fall back to the viewer.
		{"tuolumne non-genasys", "US-CA-Toulumne101", SourceURL},
		// Legacy / other aggregation id shapes and blanks fall back too.
		{"legacy short id", "CAL-E-046", SourceURL},
		{"empty", "", SourceURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ZoneURL(tc.zoneID); got != tc.want {
				t.Errorf("ZoneURL(%q) = %q, want %q", tc.zoneID, got, tc.want)
			}
		})
	}
}
