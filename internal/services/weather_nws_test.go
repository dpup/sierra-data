package services

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/prefab/logging"

	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/config"
)

// failingDoer simulates an NWS outage: every request errors.
type failingDoer struct{}

func (failingDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated NWS outage")
}

// testCtx returns a context with a logger attached — prefab's logging helpers
// panic on a bare context.Background().
func testCtx() context.Context {
	return logging.EnsureLogger(context.Background())
}

// newNWSTestService builds a WeatherService whose NWS alert list is seeded
// directly into the cache, so fetchNWSAlerts serves it without touching the
// network.
func newNWSTestService(t *testing.T, alerts []nws.Alert) *WeatherService {
	t.Helper()
	c := cache.NewCache()
	if err := c.Set("nws:alerts", alerts, time.Minute, "nws_alerts"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	return NewWeatherService(nil, nws.NewClient("test"), c, &config.Config{
		Weather: config.WeatherConfig{
			NWS: config.NWSConfig{
				UserAgent: "test",
				Zones:     []string{"CAZ064", "CAZ065"},
			},
			RefreshInterval: time.Minute,
		},
	})
}

func TestNWSAlertsForZone(t *testing.T) {
	svc := newNWSTestService(t, []nws.Alert{
		{ID: "a1", Event: "Winter Storm Warning", Zones: []string{"CAZ065"}},
		{ID: "a2", Event: "Wind Advisory", Zones: []string{"CAZ064", "CAZ065"}},
		{ID: "a3", Event: "Flood Watch", Zones: []string{"CAZ258"}},
	})
	ctx := testCtx()

	got := svc.nwsAlertsForZone(ctx, "CAZ065")
	if len(got) != 2 {
		t.Fatalf("CAZ065: got %d alerts, want 2", len(got))
	}
	for _, a := range got {
		if a.Source != api.AlertSource_NWS {
			t.Errorf("alert %s: source = %v, want NWS", a.Id, a.Source)
		}
	}

	// Zone matching is case/whitespace tolerant.
	if got := svc.nwsAlertsForZone(ctx, " caz064 "); len(got) != 1 {
		t.Errorf("caz064 (lax): got %d alerts, want 1", len(got))
	}

	// An unzoned location gets no alerts rather than everything.
	if got := svc.nwsAlertsForZone(ctx, ""); got != nil {
		t.Errorf("empty zone: got %d alerts, want none", len(got))
	}

	// A zone with no active alerts gets an empty list.
	if got := svc.nwsAlertsForZone(ctx, "CAZ999"); len(got) != 0 {
		t.Errorf("CAZ999: got %d alerts, want 0", len(got))
	}
}

// newOutageTestService builds a WeatherService whose NWS client always fails,
// for exercising the stale-fallback paths.
func newOutageTestService(t *testing.T) (*WeatherService, *cache.Cache) {
	t.Helper()
	c := cache.NewCache()
	svc := NewWeatherService(nil, nws.NewClientWithHTTPDoer("test", "http://nws.invalid", failingDoer{}), c, &config.Config{
		Weather: config.WeatherConfig{
			NWS: config.NWSConfig{
				UserAgent: "test",
				Zones:     []string{"CAZ064"},
			},
			RefreshInterval: time.Minute,
		},
	})
	return svc, c
}

// A stale (but not very stale) alerts cache must be served when the NWS
// refresh fails; past the very-stale bound the endpoint fails loud instead.
func TestListWeatherAlertsServesStaleOnRefreshFailure(t *testing.T) {
	svc, c := newOutageTestService(t)
	cached := []*api.WeatherAlert{{Id: "a1", Event: "Winter Storm Warning", Source: api.AlertSource_NWS, Zones: []string{"CAZ064"}}}
	if err := c.Set("weather:alerts", cached, time.Minute, "weather_alerts"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	c.Backdate("weather:alerts", 90*time.Second) // stale, within the 2x bound

	resp, err := svc.ListWeatherAlerts(testCtx(), &api.ListWeatherAlertsRequest{})
	if err != nil {
		t.Fatalf("expected stale fallback, got error: %v", err)
	}
	if len(resp.Alerts) != 1 || resp.Alerts[0].Id != "a1" {
		t.Fatalf("expected the stale cached alert, got %v", resp.Alerts)
	}
	if resp.LastUpdated == nil || time.Since(resp.LastUpdated.AsTime()) < 80*time.Second {
		t.Error("LastUpdated should reflect the stale entry's original CreatedAt")
	}

	// Past the very-stale bound the fallback no longer applies: fail loud
	// rather than serving old alerts as current.
	c.Backdate("weather:alerts", 2*time.Minute)
	if _, err := svc.ListWeatherAlerts(testCtx(), &api.ListWeatherAlertsRequest{}); err == nil {
		t.Fatal("expected error once cache is very stale and NWS is down")
	}
}

// getNWSAlerts (fire weather / per-location path) falls back to the last-good
// NWS list during an outage, bounded by the very-stale threshold.
func TestGetNWSAlertsBoundedStaleFallback(t *testing.T) {
	svc, c := newOutageTestService(t)
	if err := c.Set("nws:alerts", []nws.Alert{{ID: "a1", Event: "Red Flag Warning", Zones: []string{"CAZ064"}}}, time.Minute, "nws_alerts"); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	c.Backdate("nws:alerts", 90*time.Second) // stale, within the 2x bound

	if got := svc.getNWSAlerts(testCtx()); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("expected stale NWS list during outage, got %v", got)
	}

	c.Backdate("nws:alerts", 2*time.Minute) // now very stale
	if got := svc.getNWSAlerts(testCtx()); got != nil {
		t.Fatalf("expected no alerts once cache is very stale, got %v", got)
	}
}

func TestRefreshWeatherAlertsIsNWSOnly(t *testing.T) {
	svc := newNWSTestService(t, []nws.Alert{
		{ID: "a1", Event: "Red Flag Warning", Zones: []string{"CAZ064"}},
	})

	alerts, err := svc.refreshWeatherAlerts(testCtx())
	if err != nil {
		t.Fatalf("refreshWeatherAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	if alerts[0].Source != api.AlertSource_NWS {
		t.Errorf("source = %v, want NWS", alerts[0].Source)
	}
}

func TestNWSAlertsToProto_NoDuplicateText(t *testing.T) {
	in := []nws.Alert{{
		ID:          "urn:oid:abc",
		Event:       "Red Flag Warning",
		Severity:    "Severe",
		Headline:    "Red Flag Warning in effect until 8 PM",
		Description: "Gusty winds and low humidity will create critical fire conditions.",
		SenderName:  "NWS Sacramento CA",
		Effective:   time.Unix(1782400000, 0),
		Expires:     time.Unix(1782490000, 0),
		Zones:       []string{"CAZ064"},
	}}

	out := nwsAlertsToProto(in)
	if len(out) != 1 {
		t.Fatalf("got %d alerts, want 1", len(out))
	}
	a := out[0]

	if a.Source != api.AlertSource_NWS {
		t.Errorf("source = %v, want NWS", a.Source)
	}
	if a.Severity != api.AlertSeverity_CRITICAL {
		t.Errorf("severity = %v, want CRITICAL", a.Severity)
	}
	// Headline and Description are the two distinct authoritative fields.
	if a.Headline == "" || a.Description == "" {
		t.Error("headline/description should be populated")
	}
	if a.Headline == a.Description {
		t.Error("headline must not duplicate description")
	}
	// Summary/details are AI-enhancement slots and must be empty for NWS (no
	// 4x duplication of the same text).
	if a.Summary != "" {
		t.Errorf("summary should be empty for NWS, got %q", a.Summary)
	}
	if a.Details != "" {
		t.Errorf("details should be empty for NWS, got %q", a.Details)
	}
	if a.GetStartTime() == nil || a.GetEndTime() == nil {
		t.Error("start/end time should be set")
	}
}
