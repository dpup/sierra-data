package firis

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

// A combo-feed response: a named CAL FIRE Intel row + a FIRIS mission row with
// null incident_name/incident_number (the shape the normalizer's dedup handles).
const sample = `{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "properties": {
        "incident_name": "DOVE",
        "mission": null,
        "incident_number": "be60b30d-5560-45ee-87bc-6854709a4876",
        "area_acres": 224.9,
        "poly_DateCurrent": 1785172006484,
        "source": "CAL FIRE INTEL FLIGHT DATA",
        "displayStatus": "Active"
      },
      "geometry": { "type": "Polygon", "coordinates": [[[-120.4,37.96],[-120.3,37.96],[-120.3,38.0],[-120.4,37.96]]] }
    },
    {
      "type": "Feature",
      "properties": {
        "incident_name": null,
        "mission": "CA-TCU-DOVE-N57B",
        "incident_number": null,
        "area_acres": 166.3,
        "poly_DateCurrent": 1785100000000,
        "source": "FIRIS",
        "displayStatus": "Active"
      },
      "geometry": { "type": "MultiPolygon", "coordinates": [[[[-120.41,37.97],[-120.31,37.97],[-120.31,38.01],[-120.41,37.97]]],[[[-120.5,37.9],[-120.45,37.9],[-120.45,37.95],[-120.5,37.9]]]] }
    }
  ]
}`

func TestGetPerimeters(t *testing.T) {
	doer := &fakeDoer{resp: sample}
	c := NewClientWithHTTPDoer("https://firis.test/query", doer)

	perims, err := c.GetPerimeters(context.Background(),
		Bounds{MinLatitude: 37.8, MaxLatitude: 38.55, MinLongitude: -120.9, MaxLongitude: -120.0})
	if err != nil {
		t.Fatal(err)
	}
	if len(perims) != 2 {
		t.Fatalf("got %d, want 2", len(perims))
	}

	dove := perims[0]
	if dove.IncidentName != "DOVE" || dove.Acres != 224.9 || dove.Source != "CAL FIRE INTEL FLIGHT DATA" {
		t.Errorf("props = %+v", dove)
	}
	if dove.IncidentNumber != "be60b30d-5560-45ee-87bc-6854709a4876" || dove.Status != "Active" {
		t.Errorf("ids/status = %+v", dove)
	}
	if !dove.DateCurrent.Equal(time.UnixMilli(1785172006484)) {
		t.Errorf("DateCurrent = %v", dove.DateCurrent)
	}
	if dove.GeometryType != "Polygon" || !strings.Contains(string(dove.GeometryCoords), "-120.4") {
		t.Errorf("geometry = %s / %s", dove.GeometryType, dove.GeometryCoords)
	}

	// The FIRIS mission row: null incident_name/incident_number decode to empty
	// strings; mission survives for the normalizer's name parse. Geometry is a
	// MultiPolygon (multi-lobe fire) carried through verbatim.
	mission := perims[1]
	if mission.IncidentName != "" || mission.IncidentNumber != "" || mission.Mission != "CA-TCU-DOVE-N57B" {
		t.Errorf("mission row = %+v", mission)
	}
	if mission.GeometryType != "MultiPolygon" || !strings.Contains(string(mission.GeometryCoords), "-120.5") {
		t.Errorf("multipolygon geometry = %s / %s", mission.GeometryType, mission.GeometryCoords)
	}

	// Query filters Active server-side and is a bbox geojson request.
	for _, want := range []string{"f=geojson", "esriGeometryEnvelope", "maxAllowableOffset=0.001", "displayStatus"} {
		if !strings.Contains(doer.lastURL, want) {
			t.Errorf("query missing %q: %s", want, doer.lastURL)
		}
	}
}

func TestLastEdit(t *testing.T) {
	doer := &fakeDoer{resp: `{"editingInfo":{"dataLastEditDate":1785172006484}}`}
	c := NewClientWithHTTPDoer("https://firis.test/query", doer)

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

// An ArcGIS throttle/error envelope arrives as HTTP 200 + {"error":...}; it must
// surface as an error, not decode to zero perimeters.
func TestGetPerimeters_ArcGISErrorEnvelope(t *testing.T) {
	doer := &fakeDoer{resp: `{"error":{"code":429,"message":"Rate limit exceeded"}}`}
	c := NewClientWithHTTPDoer("https://firis.test/query", doer)
	if _, err := c.GetPerimeters(context.Background(), Bounds{}); err == nil {
		t.Fatal("expected an error for an ArcGIS error envelope")
	}
}

// LastEdit must also fail loud on the 200+error envelope (the gating falls back to
// a direct fetch on a LastEdit error, so a swallowed envelope would silently
// disable gating rather than surface).
func TestLastEdit_ArcGISErrorEnvelope(t *testing.T) {
	doer := &fakeDoer{resp: `{"error":{"code":429,"message":"Rate limit exceeded"}}`}
	c := NewClientWithHTTPDoer("https://firis.test/query", doer)
	if _, err := c.LastEdit(context.Background()); err == nil {
		t.Fatal("expected an error for an ArcGIS error envelope")
	}
}
