package hazards

import (
	"context"
	"testing"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
)

func TestZonesMatch(t *testing.T) {
	cases := []struct {
		name       string
		area, alrt []string
		want       bool
	}{
		{"unscoped area keeps everything", nil, []string{"CAZ258"}, true},
		{"zoneless alert (OWM) kept", []string{"CAZ064"}, nil, true},
		{"intersecting zones match", []string{"CAZ064", "CAZ065"}, []string{"CAZ065"}, true},
		{"disjoint zones drop", []string{"CAZ064", "CAZ065"}, []string{"CAZ258"}, false},
	}
	for _, c := range cases {
		if got := zonesMatch(c.area, c.alrt); got != c.want {
			t.Errorf("%s: zonesMatch(%v,%v)=%v want %v", c.name, c.area, c.alrt, got, c.want)
		}
	}
}

// fakeWeather implements WeatherAPI for builder tests.
type fakeWeather struct {
	alerts []*api.WeatherAlert
	fw     *api.FireWeather
}

func (f fakeWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return &api.ListWeatherResponse{FireWeather: f.fw}, nil
}
func (f fakeWeather) ListWeatherAlerts(context.Context, *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	return &api.ListWeatherAlertsResponse{Alerts: f.alerts}, nil
}

// TestWeatherAlerts_AreaScoping: an area only surfaces alerts for its own NWS
// zones, but zoneless (OpenWeatherMap) alerts are kept since they can't be scoped.
func TestWeatherAlerts_AreaScoping(t *testing.T) {
	s := &Service{weather: fakeWeather{alerts: []*api.WeatherAlert{
		{Id: "calaveras", Source: api.AlertSource_NWS, Severity: api.AlertSeverity_WARNING, Event: "Winter Storm Warning", Zones: []string{"CAZ064"}},
		{Id: "tuolumne", Source: api.AlertSource_NWS, Severity: api.AlertSeverity_CRITICAL, Event: "Red Flag Warning", Zones: []string{"CAZ258"}},
		{Id: "owm", Source: api.AlertSource_OPENWEATHERMAP, Severity: api.AlertSeverity_INFO, Event: "Heat Advisory"},
	}}}
	area := config.HazardArea{Zones: []string{"CAZ064", "CAZ065"}}

	feats, err := s.weatherAlerts(context.Background(), area)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range feats {
		got[f.Properties.ID] = true
	}
	if !got["wx:calaveras"] {
		t.Error("in-zone NWS alert should be present")
	}
	if got["wx:tuolumne"] {
		t.Error("out-of-zone NWS alert must be dropped (the multi-area bug)")
	}
	if !got["wx:owm"] {
		t.Error("zoneless OWM alert should be kept (can't be scoped)")
	}
}

// TestFireWeather_AreaScoping: fire weather only surfaces for areas whose zones
// the product covers.
func TestFireWeather_AreaScoping(t *testing.T) {
	fw := &api.FireWeather{State: api.FireWeatherState_RED_FLAG, Zones: []string{"CAZ258", "CAZ259"}}
	s := &Service{weather: fakeWeather{fw: fw}}

	// Tuolumne area: covered → one feature.
	in, _ := s.fireWeather(context.Background(), config.HazardArea{Zones: []string{"CAZ258"}})
	if len(in) != 1 {
		t.Fatalf("covered area got %d fire-weather features, want 1", len(in))
	}
	// Calaveras-only area: not covered → none.
	out, _ := s.fireWeather(context.Background(), config.HazardArea{Zones: []string{"CAZ064"}})
	if len(out) != 0 {
		t.Errorf("uncovered area got %d fire-weather features, want 0", len(out))
	}
}
