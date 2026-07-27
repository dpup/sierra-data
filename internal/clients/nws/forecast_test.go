package nws

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseISODuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT1H":    time.Hour,
		"PT5H":    5 * time.Hour,
		"PT30M":   30 * time.Minute,
		"P1D":     24 * time.Hour,
		"P1DT6H":  30 * time.Hour,
		"P7DT18H": 7*24*time.Hour + 18*time.Hour,
	}
	for in, want := range cases {
		got, err := parseISODuration(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", in, got, want)
		}
	}
	if _, err := parseISODuration("5H"); err == nil {
		t.Error("expected error for duration without leading P")
	}
}

func TestConvertUnit(t *testing.T) {
	if v := convertUnit("wmoUnit:m_s-1", 10); v < 35.9 || v > 36.1 {
		t.Errorf("m/s->km/h = %v, want ~36", v)
	}
	if v := convertUnit("wmoUnit:degF", 32); v < -0.01 || v > 0.01 {
		t.Errorf("F->C = %v, want 0", v)
	}
	if v := convertUnit("wmoUnit:km_h-1", 20); v != 20 {
		t.Errorf("km/h passthrough = %v, want 20", v)
	}
}

// gridFixture: 6h horizon from 12:00Z. windSpeed 10 then 20 (3h each); gust 30
// all 6h; RH 40 then 15; temp/dir constant. km/h units already.
const gridFixture = `{
  "properties": {
    "updateTime": "2026-07-27T11:30:00+00:00",
    "temperature":      {"uom":"wmoUnit:degC","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":20}]},
    "relativeHumidity": {"uom":"wmoUnit:percent","values":[
        {"validTime":"2026-07-27T12:00:00+00:00/PT2H","value":40},
        {"validTime":"2026-07-27T14:00:00+00:00/PT4H","value":15}]},
    "windSpeed":     {"uom":"wmoUnit:km_h-1","values":[
        {"validTime":"2026-07-27T12:00:00+00:00/PT3H","value":10},
        {"validTime":"2026-07-27T15:00:00+00:00/PT3H","value":20}]},
    "windDirection": {"uom":"wmoUnit:degree_(angle)","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":180}]},
    "windGust":      {"uom":"wmoUnit:km_h-1","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":30}]}
  }
}`

func TestGetGridForecast(t *testing.T) {
	doer := &fakeDoer{resp: gridFixture}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)
	c.now = func() time.Time { return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC) }

	f, err := c.GetGridForecast(context.Background(), "https://nws.test/gridpoints/STO/90,41", 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Points) != 6 {
		t.Fatalf("got %d points, want 6", len(f.Points))
	}
	if f.Source != "NWS (STO 90,41)" {
		t.Errorf("source = %q", f.Source)
	}
	if f.HorizonHours != 6 {
		t.Errorf("horizon = %d, want 6", f.HorizonHours)
	}
	// First hour (12:00): wind 10, gust 30, RH 40, temp 20, dir 180.
	p0 := f.Points[0]
	if !p0.Time.Equal(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("p0.time = %v", p0.Time)
	}
	if p0.WindKmh != 10 || p0.WindGustKmh != 30 || p0.HumidityPct != 40 || p0.TempC != 20 || p0.WindDirDeg != 180 {
		t.Errorf("p0 = %+v", p0)
	}
	// Wind steps to 20 at 15:00 (index 3); RH drops to 15 at 14:00 (index 2).
	if f.Points[3].WindKmh != 20 {
		t.Errorf("points[3].wind = %v, want 20", f.Points[3].WindKmh)
	}
	if f.Points[2].HumidityPct != 15 {
		t.Errorf("points[2].rh = %v, want 15", f.Points[2].HumidityPct)
	}
	// Summary: peak gust 30 (at 12:00), min RH 15.
	if f.PeakGustKmh != 30 || !f.PeakGustAt.Equal(p0.Time) {
		t.Errorf("peak gust = %v at %v", f.PeakGustKmh, f.PeakGustAt)
	}
	if !f.HasMinHumidity || f.MinHumidityPct != 15 {
		t.Errorf("min RH = %v (has=%v), want 15", f.MinHumidityPct, f.HasMinHumidity)
	}
	if !f.IssuedAt.Equal(time.Date(2026, 7, 27, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("issuedAt = %v", f.IssuedAt)
	}
}

func TestResolveForecastURL(t *testing.T) {
	doer := &fakeDoer{resp: `{"properties":{"forecastGridData":"https://api.weather.gov/gridpoints/STO/90,41"}}`}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)
	url, err := c.ResolveForecastURL(context.Background(), 38.2, -120.0)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://api.weather.gov/gridpoints/STO/90,41" {
		t.Errorf("url = %q", url)
	}
	if !strings.Contains(doer.lastURL, "/points/38.2000,-120.0000") {
		t.Errorf("points URL = %q", doer.lastURL)
	}
}
