package calfire

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDoer struct{ resp string }

func (f *fakeDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(f.resp)), Header: make(http.Header)}, nil
}

const sample = `[
  {
    "UniqueId": "abc-123",
    "Name": "Salt Springs Fire",
    "County": "Calaveras",
    "Location": "near Hwy 4",
    "AcresBurned": 1377.0,
    "PercentContained": 20.0,
    "Latitude": 38.33,
    "Longitude": -120.27,
    "Started": "2026-06-26T14:02:00Z",
    "Updated": "2026-06-26T15:40:00",
    "Url": "https://www.fire.ca.gov/incidents/2026/6/26/salt-springs-fire/",
    "IsActive": true
  },
  { "UniqueId": "x", "Name": "Null Fire", "AcresBurned": null, "PercentContained": null, "Latitude": null, "Longitude": null }
]`

func TestGetActiveIncidents(t *testing.T) {
	c := NewClientWithHTTPDoer("https://calfire.test", &fakeDoer{resp: sample})
	inc, err := c.GetActiveIncidents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 2 {
		t.Fatalf("got %d, want 2", len(inc))
	}
	a := inc[0]
	if a.Name != "Salt Springs Fire" || a.County != "Calaveras" {
		t.Errorf("name/county = %q/%q", a.Name, a.County)
	}
	if a.Acres != 1377 || a.PercentContained != 20 {
		t.Errorf("acres/containment = %v/%v", a.Acres, a.PercentContained)
	}
	if a.Lat != 38.33 || a.Lng != -120.27 {
		t.Errorf("lat/lng = %v/%v", a.Lat, a.Lng)
	}
	if a.Started.IsZero() || a.Updated.IsZero() {
		t.Error("timestamps should parse (RFC3339 and no-zone variants)")
	}
	if a.URL == "" || !a.IsActive {
		t.Error("url/active should be set")
	}
	// Null-field row handled gracefully.
	if inc[1].Acres != 0 || inc[1].Lat != 0 {
		t.Errorf("null row: %+v", inc[1])
	}
}

// TestNormalizeTrimsFreeText: CAL FIRE ships trailing whitespace on some
// incident names, and Name goes straight into the event headline — one live
// fire rendered "Dennis Fire  — 16 ac, 40% contained" with a double space while
// its siblings rendered correctly, which reads as our formatting bug.
func TestNormalizeTrimsFreeText(t *testing.T) {
	in := incidentJSON{
		UniqueID: "abc",
		Name:     "Dennis Fire ",
		County:   " El Dorado",
		Location: "  Mormon Emigrant Trail  ",
		URL:      " https://www.fire.ca.gov/incidents/dennis ",
	}.normalize()

	if in.Name != "Dennis Fire" {
		t.Errorf("Name = %q, want %q", in.Name, "Dennis Fire")
	}
	if in.County != "El Dorado" {
		t.Errorf("County = %q", in.County)
	}
	if in.Location != "Mormon Emigrant Trail" {
		t.Errorf("Location = %q", in.Location)
	}
	if in.URL != "https://www.fire.ca.gov/incidents/dennis" {
		t.Errorf("URL = %q — an untrimmed URL fails the http(s) prefix check downstream", in.URL)
	}
}
