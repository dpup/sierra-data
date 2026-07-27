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
