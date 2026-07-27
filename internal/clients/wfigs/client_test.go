package wfigs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
        "poly_IncidentName": "Salt Springs",
        "attr_IncidentSize": 1377.0,
        "attr_PercentContained": 20.0,
        "attr_FireCause": "Under Investigation"
      },
      "geometry": { "type": "Polygon", "coordinates": [[[-120.3,38.3],[-120.2,38.3],[-120.2,38.4],[-120.3,38.3]]] }
    }
  ]
}`

func TestGetPerimeters(t *testing.T) {
	doer := &fakeDoer{resp: sample}
	c := NewClientWithHTTPDoer("https://wfigs.test/query", doer)

	perims, err := c.GetPerimeters(context.Background(),
		Bounds{MinLatitude: 37.8, MaxLatitude: 38.55, MinLongitude: -120.9, MaxLongitude: -120.0})
	if err != nil {
		t.Fatal(err)
	}
	if len(perims) != 1 {
		t.Fatalf("got %d, want 1", len(perims))
	}
	p := perims[0]
	if p.Name != "Salt Springs" || p.Acres != 1377 || p.PercentContained != 20 {
		t.Errorf("props = %+v", p)
	}
	if p.GeometryType != "Polygon" || len(p.GeometryCoords) == 0 {
		t.Errorf("geometry = %s / %s", p.GeometryType, p.GeometryCoords)
	}
	// Geometry is carried verbatim (no re-encode).
	if !strings.Contains(string(p.GeometryCoords), "-120.3") {
		t.Errorf("coords not preserved: %s", p.GeometryCoords)
	}
	// Query is a bbox geojson request.
	for _, want := range []string{"f=geojson", "esriGeometryEnvelope", "maxAllowableOffset=0.001"} {
		if !strings.Contains(doer.lastURL, want) {
			t.Errorf("query missing %q: %s", want, doer.lastURL)
		}
	}
}

func TestLastEdit(t *testing.T) {
	doer := &fakeDoer{resp: `{"editingInfo":{"dataLastEditDate":1785172006484}}`}
	c := NewClientWithHTTPDoer("https://wfigs.test/query", doer)

	got, err := c.LastEdit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := time.UnixMilli(1785172006484); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Hits the cheap metadata endpoint (?f=json), NOT the feature /query.
	if strings.Contains(doer.lastURL, "/query") || !strings.Contains(doer.lastURL, "f=json") {
		t.Errorf("LastEdit URL = %s (want metadata, /query stripped)", doer.lastURL)
	}
}
