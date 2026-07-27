package hazards

import (
	"context"
	"testing"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/config"
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
	fw        *api.FireWeather
	forecasts map[string]*nws.Forecast
}

func (f fakeWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return &api.ListWeatherResponse{FireWeather: f.fw}, nil
}
func (f fakeWeather) LocationForecasts(context.Context) map[string]*nws.Forecast { return f.forecasts }

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

// TestFireWeather_ForecastPoints: with no issued product, the layer is just the
// per-location forecast Points (INFO), and locations outside the area are dropped.
func TestFireWeather_ForecastPoints(t *testing.T) {
	cfg := &config.Config{}
	cfg.Weather.Locations = []config.WeatherLocation{
		{ID: "arnold", Name: "Arnold", Coordinates: config.Coordinates{Latitude: 38.25, Longitude: -120.35}},
		{ID: "faraway", Name: "Faraway", Coordinates: config.Coordinates{Latitude: 40.0, Longitude: -122.0}},
	}
	forecasts := map[string]*nws.Forecast{
		"arnold":  {Source: "NWS (STO 90,41)", HorizonHours: 48, PeakGustKmh: 45, MinHumidityPct: 12, HasMinHumidity: true},
		"faraway": {PeakGustKmh: 99},
	}
	s := &Service{cfg: cfg, weather: fakeWeather{forecasts: forecasts}}
	area := config.HazardArea{Bounds: config.GeoBounds{MinLatitude: 38, MaxLatitude: 38.5, MinLongitude: -120.5, MaxLongitude: -120.0}}

	feats, err := s.fireWeather(context.Background(), area)
	if err != nil {
		t.Fatal(err)
	}
	if len(feats) != 1 {
		t.Fatalf("got %d features, want 1 (arnold point; no banner, faraway out of bounds)", len(feats))
	}
	f := feats[0]
	if f.Geometry == nil || f.Geometry.Type != "Point" {
		t.Fatalf("geometry = %+v, want Point", f.Geometry)
	}
	if f.Properties.Severity != "INFO" {
		t.Errorf("severity = %q, want INFO (forecast never escalates)", f.Properties.Severity)
	}
	fc := f.Properties.FireWeather
	if fc == nil || fc.Forecast == nil {
		t.Fatalf("no forecast summary on the point")
	}
	if fc.Forecast.PeakWindGustKmh != 45 || fc.Forecast.MinHumidityPercent != 12 {
		t.Errorf("summary = %+v, want gust 45 / RH 12", fc.Forecast)
	}
}

// TestFireWeather_BannerPlusForecast: an issued product AND forecasts yield the
// colored banner PLUS an INFO forecast point — the forecast never colors the layer.
func TestFireWeather_BannerPlusForecast(t *testing.T) {
	cfg := &config.Config{}
	cfg.Weather.Locations = []config.WeatherLocation{
		{ID: "arnold", Name: "Arnold", Coordinates: config.Coordinates{Latitude: 38.25, Longitude: -120.35}},
	}
	s := &Service{cfg: cfg, weather: fakeWeather{
		fw:        &api.FireWeather{State: api.FireWeatherState_RED_FLAG, Zones: []string{"CAZ139"}},
		forecasts: map[string]*nws.Forecast{"arnold": {PeakGustKmh: 45}},
	}}
	area := config.HazardArea{Zones: []string{"CAZ139"}, Bounds: config.GeoBounds{MinLatitude: 38, MaxLatitude: 38.5, MinLongitude: -120.5, MaxLongitude: -120.0}}

	feats, _ := s.fireWeather(context.Background(), area)
	if len(feats) != 2 {
		t.Fatalf("got %d features, want 2 (banner + point)", len(feats))
	}
	var banner, point *Feature
	for i := range feats {
		if feats[i].Geometry == nil {
			banner = &feats[i]
		} else {
			point = &feats[i]
		}
	}
	if banner == nil || point == nil {
		t.Fatalf("expected one null-geometry banner + one point")
	}
	if banner.Properties.Severity == "INFO" {
		t.Errorf("banner severity = INFO; should reflect the issued Red Flag")
	}
	if point.Properties.Severity != "INFO" {
		t.Errorf("forecast point severity = %q, want INFO", point.Properties.Severity)
	}
}
