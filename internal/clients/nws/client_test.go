package nws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDoer struct {
	resp       string
	status     int
	lastURL    string
	lastUA     string
	lastAccept string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastURL = req.URL.String()
	f.lastUA = req.Header.Get("User-Agent")
	f.lastAccept = req.Header.Get("Accept")
	status := f.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.resp)),
		Header:     make(http.Header),
	}, nil
}

const sampleGeoJSON = `{
  "features": [
    {
      "properties": {
        "id": "urn:oid:2.49.0.1.840.0.abc",
        "event": "Red Flag Warning",
        "severity": "Severe",
        "certainty": "Likely",
        "urgency": "Expected",
        "headline": "Red Flag Warning in effect",
        "description": "Gusty winds and low humidity.",
        "instruction": "Avoid outdoor burning.",
        "senderName": "NWS Sacramento CA",
        "areaDesc": "Calaveras",
        "effective": "2026-06-26T10:00:00-07:00",
        "expires": "2026-06-27T20:00:00-07:00",
        "onset": "2026-06-27T05:00:00-07:00",
        "ends": "2026-06-28T21:00:00-07:00",
        "geocode": { "UGC": ["CAZ064", "CAZ065"] },
        "affectedZones": ["https://api.weather.gov/zones/fire/CAZ064", "https://api.weather.gov/zones/fire/caz065", "https://evil.test/x", "https://api.weather.gov/zones/fire/../../x"],
        "parameters": {
          "NWSheadline": ["RED FLAG WARNING IN EFFECT UNTIL 8 PM PDT FOR GUSTY WINDS AND LOW HUMIDITY"],
          "AWIPSidentifier": ["RFWSTO"]
        }
      }
    },
    {
      "properties": {
        "id": "urn:oid:2.49.0.1.840.0.def",
        "event": "Heat Advisory",
        "severity": "Moderate",
        "headline": "Heat Advisory",
        "description": "Hot.",
        "senderName": "NWS Sacramento CA",
        "geocode": { "UGC": ["CAZ258"] }
      }
    }
  ]
}`

func TestGetActiveZoneAlerts(t *testing.T) {
	doer := &fakeDoer{resp: sampleGeoJSON}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)

	alerts, err := c.GetActiveZoneAlerts(context.Background(), []string{"CAZ064", "CAZ065", "CAZ258", "CAZ259"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(alerts))
	}

	rf := alerts[0]
	if rf.Event != "Red Flag Warning" {
		t.Errorf("event = %q", rf.Event)
	}
	if rf.Severity != "Severe" {
		t.Errorf("severity = %q", rf.Severity)
	}
	if len(rf.Zones) != 2 || rf.Zones[0] != "CAZ064" {
		t.Errorf("zones = %v", rf.Zones)
	}
	// Typed zone paths only; anything else in affectedZones is dropped.
	if got := strings.Join(rf.ZoneRefs, ","); got != "fire/CAZ064,fire/CAZ065" {
		t.Errorf("zone refs = %q", got)
	}
	if rf.Effective.IsZero() {
		t.Error("expected effective time parsed")
	}
	// onset/ends are the HAZARD window and both must survive parsing —
	// `expires` here is a full day before `ends`, which is the norm.
	if rf.Onset.IsZero() || rf.Ends.IsZero() {
		t.Errorf("onset/ends not parsed: %v / %v", rf.Onset, rf.Ends)
	}
	if !rf.Begins().Equal(rf.Onset) || !rf.EndsAt().Equal(rf.Ends) {
		t.Errorf("hazard window = %v..%v, want onset..ends", rf.Begins(), rf.EndsAt())
	}
	// The second alert publishes neither, and falls back to the product window.
	if !alerts[1].Begins().Equal(alerts[1].Effective) {
		t.Errorf("Begins() should fall back to effective, got %v", alerts[1].Begins())
	}
	// parameters.NWSheadline is where the alert's REASON lives; the card line
	// is composed from it (see headline.go), so it has to survive parsing.
	if rf.NWSHeadline != "RED FLAG WARNING IN EFFECT UNTIL 8 PM PDT FOR GUSTY WINDS AND LOW HUMIDITY" {
		t.Errorf("NWSHeadline = %q", rf.NWSHeadline)
	}
	if got, want := rf.ShortHeadline(), "Red Flag Warning — gusty winds and low humidity"; got != want {
		t.Errorf("ShortHeadline() = %q, want %q", got, want)
	}
	// The second alert publishes no parameters block at all — the common case.
	if alerts[1].NWSHeadline != "" {
		t.Errorf("NWSHeadline = %q, want empty", alerts[1].NWSHeadline)
	}
	if got, want := alerts[1].ShortHeadline(), "Heat Advisory"; got != want {
		t.Errorf("ShortHeadline() = %q, want %q", got, want)
	}

	// Request headers and zone param
	if doer.lastUA != "test-agent" {
		t.Errorf("User-Agent = %q", doer.lastUA)
	}
	if !strings.Contains(doer.lastAccept, "geo+json") {
		t.Errorf("Accept = %q", doer.lastAccept)
	}
	if !strings.Contains(doer.lastURL, "zone=CAZ064%2CCAZ065%2CCAZ258%2CCAZ259") {
		t.Errorf("URL missing zone param: %q", doer.lastURL)
	}
}

func TestGetActiveZoneAlerts_NoZones(t *testing.T) {
	doer := &fakeDoer{resp: sampleGeoJSON}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)
	alerts, err := c.GetActiveZoneAlerts(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alerts != nil {
		t.Errorf("expected nil alerts for empty zones, got %v", alerts)
	}
	if doer.lastURL != "" {
		t.Errorf("expected no request to be made, got %q", doer.lastURL)
	}
}

func TestCleanZones(t *testing.T) {
	got := cleanZones([]string{" caz064 ", "CAZ064", "CAZ065,CAZ258", ""})
	want := []string{"CAZ064", "CAZ065", "CAZ258"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGetAlert(t *testing.T) {
	doer := &fakeDoer{resp: `{"properties": {
		"id": "urn:oid:2.49.0.1.840.0.old.001.1",
		"event": "Winter Storm Watch",
		"messageType": "Alert",
		"references": [],
		"replacedBy": "https://api.weather.gov/alerts/urn:oid:2.49.0.1.840.0.new.001.1",
		"replacedAt": "2026-11-11T12:03:00-08:00"
	}}`}
	c := NewClientWithHTTPDoer("ua-test", "https://nws.test", doer)
	a, err := c.GetAlert(context.Background(), "urn:oid:2.49.0.1.840.0.old.001.1")
	if err != nil {
		t.Fatal(err)
	}
	if doer.lastURL != "https://nws.test/alerts/urn:oid:2.49.0.1.840.0.old.001.1" {
		t.Errorf("url = %s", doer.lastURL)
	}
	if doer.lastUA != "ua-test" {
		t.Errorf("User-Agent = %q", doer.lastUA)
	}
	if a.MessageType != "Alert" || len(a.References) != 0 {
		t.Errorf("messageType/references = %q %v", a.MessageType, a.References)
	}
	if a.ReplacedBy != "urn:oid:2.49.0.1.840.0.new.001.1" {
		t.Errorf("replacedBy = %q, want the replacing alert's id", a.ReplacedBy)
	}

	for _, bad := range []string{"", "  ", "../zones/x", "a?b", "a#b"} {
		if _, err := c.GetAlert(context.Background(), bad); err == nil {
			t.Errorf("GetAlert(%q) should be refused", bad)
		}
	}
	doer.status = 404
	if _, err := c.GetAlert(context.Background(), "urn:oid:gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 err = %v, want ErrNotFound", err)
	}
}
