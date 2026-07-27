package gridapi

import (
	"testing"
	"time"

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
