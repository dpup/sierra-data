package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/nws"
)

func TestProjectForecast(t *testing.T) {
	at := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	f := &nws.Forecast{
		Source: "NWS (STO 90,41)", IssuedAt: time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC),
		HorizonHours: 48, PeakGustKmh: 44.6, PeakGustAt: at, MinHumidityPct: 12.4, HasMinHumidity: true,
		Points: []nws.ForecastPoint{{Time: at, TempC: 20.5, HumidityPct: 15.4, WindKmh: 10.6, WindDirDeg: 180, WindGustKmh: 30.4}},
	}
	wf := projectForecast("arnold", f)

	if wf.GetLocationId() != "arnold" || wf.GetSource() != "NWS (STO 90,41)" || wf.GetHorizonHours() != 48 {
		t.Errorf("envelope = %+v", wf)
	}
	if wf.GetPeakWindGustKmh() != 45 { // 44.6 rounds to 45
		t.Errorf("peak gust = %d, want 45", wf.GetPeakWindGustKmh())
	}
	if wf.GetMinHumidityPercent() != 12 {
		t.Errorf("min RH = %d, want 12", wf.GetMinHumidityPercent())
	}
	if !wf.GetPeakWindGustAt().AsTime().Equal(at) || !wf.GetIssuedAt().AsTime().Equal(f.IssuedAt) {
		t.Errorf("timestamps: peakAt=%v issuedAt=%v", wf.GetPeakWindGustAt().AsTime(), wf.GetIssuedAt().AsTime())
	}
	if len(wf.GetPeriods()) != 1 {
		t.Fatalf("got %d periods, want 1", len(wf.GetPeriods()))
	}
	p := wf.GetPeriods()[0]
	if p.GetWindSpeedKmh() != 11 || p.GetWindGustKmh() != 30 || p.GetHumidityPercent() != 15 || p.GetTemperatureCelsius() != 21 {
		t.Errorf("period rounding = %+v", p)
	}
}

func TestProjectForecast_AbsentMinHumidity(t *testing.T) {
	// HasMinHumidity=false → the proto field stays 0 (not the raw MinHumidityPct).
	wf := projectForecast("x", &nws.Forecast{HasMinHumidity: false, MinHumidityPct: 99})
	if wf.GetMinHumidityPercent() != 0 {
		t.Errorf("absent min RH = %d, want 0", wf.GetMinHumidityPercent())
	}
}

// GetConditions joins the per-location forecast to the weather set it returns
// (only locations that have a forecast get one) — the integration projectForecast
// alone doesn't cover.
func TestGetConditions_ForecastJoin(t *testing.T) {
	wresp := &api.ListWeatherResponse{WeatherData: []*api.WeatherData{
		{LocationId: "arnold", LocationName: "Arnold"},
		{LocationId: "sonora", LocationName: "Sonora"},
	}}
	forecasts := map[string]*nws.Forecast{
		"arnold": {Source: "NWS (STO 90,41)", HorizonHours: 48, PeakGustKmh: 40,
			Points: []nws.ForecastPoint{{Time: time.Now().UTC(), WindKmh: 10, HumidityPct: 20}}},
		// sonora deliberately absent → no forecast joined for it.
	}
	g := NewGridServer(&Service{Weather: &fakeWeather{resp: wresp, forecasts: forecasts}})

	resp, err := g.GetConditions(context.Background(), &gridv1.GetConditionsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetWeather(), 2)
	require.Len(t, resp.GetForecast(), 1, "forecast joined only for locations that have one")
	fc := resp.GetForecast()[0]
	assert.Equal(t, "arnold", fc.GetLocationId())
	assert.Equal(t, int32(40), fc.GetPeakWindGustKmh())
	assert.Len(t, fc.GetPeriods(), 1)
}

// TestPlaceBboxPadsDegenerate: a TOWN place is a POINT, so its bbox is the
// point itself — and stored place geometry is trimmed to 5 decimals while the
// configured coordinates it is compared against are not. A town therefore
// matched its own weather only when its configured coordinates happened to have
// no more than 5 decimals. Live, `?place=murphys` (38.139117 vs stored
// 38.13912) returned NO weather for Murphys, as did arnold and bearvalley,
// while columbia (38.034900 vs 38.0349) worked.
func TestPlaceBboxPadsDegenerate(t *testing.T) {
	// The real Murphys case, to 5 decimals as the store holds it.
	stored := bbox{minLat: 38.13912, minLng: -120.45611, maxLat: 38.13912, maxLng: -120.45611}
	if stored.contains(38.139117, -120.456111) {
		t.Fatal("precondition: the untrimmed point should NOT sit in the raw degenerate box")
	}
	padded := stored.padDegenerate()
	if !padded.contains(38.139117, -120.456111) {
		t.Error("a town must match its own weather location across the 5-decimal trim")
	}

	// The pad is a rounding allowance, not a radius: the nearest other
	// configured location (Arnold, ~14 km away) must stay out.
	if padded.contains(38.25501, -120.35023) {
		t.Error("the epsilon must not pull in a neighbouring town")
	}

	// A real polygon box is untouched.
	poly := bbox{minLat: 38.0, minLng: -120.7, maxLat: 38.5, maxLng: -120.0}
	if got := poly.padDegenerate(); got != poly {
		t.Errorf("a non-degenerate box must not be widened: %+v -> %+v", poly, got)
	}
}
