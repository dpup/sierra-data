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

// gridGapFixture: wind covers only hour 12; RH covers 12+13; gust covers all 3h.
const gridGapFixture = `{
  "properties": {
    "updateTime": "2026-07-27T11:30:00+00:00",
    "temperature":      {"uom":"wmoUnit:degC","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT3H","value":20}]},
    "relativeHumidity": {"uom":"wmoUnit:percent","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT2H","value":40}]},
    "windSpeed":        {"uom":"wmoUnit:km_h-1","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT1H","value":10}]},
    "windDirection":    {"uom":"wmoUnit:degree_(angle)","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT3H","value":180}]},
    "windGust":         {"uom":"wmoUnit:km_h-1","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT3H","value":35}]}
  }
}`

// A missing wind or RH hour must be DROPPED, never emitted as a false 0; the
// summary is still computed from the presence maps (peak gust counts point-less
// hours).
func TestGetGridForecast_GapsDroppedNotZeroed(t *testing.T) {
	doer := &fakeDoer{resp: gridGapFixture}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)
	c.now = func() time.Time { return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC) }

	f, err := c.GetGridForecast(context.Background(), "https://nws.test/gridpoints/STO/90,41", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Hour 12 has both wind+RH → 1 point. Hour 13 (RH only) and 14 (neither) are
	// dropped, NOT emitted with wind=0.
	if len(f.Points) != 1 {
		t.Fatalf("got %d points, want 1 (gap hours dropped, not zero-filled)", len(f.Points))
	}
	if f.Points[0].WindKmh != 10 || f.Points[0].HumidityPct != 40 {
		t.Errorf("p0 = %+v", f.Points[0])
	}
	// Peak gust is 35 from the gust presence map, even though hours 13/14 emit no
	// point — proving the summary isn't limited to emitted points.
	if f.PeakGustKmh != 35 {
		t.Errorf("peak gust = %v, want 35 (from presence map incl. point-less hours)", f.PeakGustKmh)
	}
	if f.MinHumidityPct != 40 {
		t.Errorf("min RH = %v, want 40", f.MinHumidityPct)
	}
}

// precipFixture: 6h from 12:00Z. PoP 20 then 0 (a real zero); QPF 6 mm over
// the whole 6 h (1 mm/h); snowfall 3 mm over the last 3 h; snow level in m for
// the first 4 h only; sky 50; a chance of light rain showers for 2 h.
const precipFixture = `{
  "properties": {
    "elevation": {"unitCode": "wmoUnit:m", "value": 715.9},
    "temperature":      {"uom":"wmoUnit:degC","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":2}]},
    "relativeHumidity": {"uom":"wmoUnit:percent","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":90}]},
    "windSpeed":        {"uom":"wmoUnit:km_h-1","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":10}]},
    "probabilityOfPrecipitation": {"uom":"wmoUnit:percent","values":[
        {"validTime":"2026-07-27T12:00:00+00:00/PT3H","value":20},
        {"validTime":"2026-07-27T15:00:00+00:00/PT3H","value":0}]},
    "quantitativePrecipitation": {"uom":"wmoUnit:mm","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":6}]},
    "snowfallAmount":   {"uom":"wmoUnit:mm","values":[{"validTime":"2026-07-27T15:00:00+00:00/PT3H","value":3}]},
    "snowLevel":        {"uom":"wmoUnit:m","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT4H","value":1500}]},
    "skyCover":         {"uom":"wmoUnit:percent","values":[{"validTime":"2026-07-27T12:00:00+00:00/PT6H","value":50}]},
    "weather": {"values":[
        {"validTime":"2026-07-27T12:00:00+00:00/PT2H","value":[{"coverage":"chance","weather":"rain_showers","intensity":"light","visibility":{"value":null},"attributes":[]}]},
        {"validTime":"2026-07-27T14:00:00+00:00/PT4H","value":[{"coverage":null,"weather":null,"intensity":null,"visibility":{"value":null},"attributes":[]}]}]}
  }
}`

func TestGetGridForecastPrecipitation(t *testing.T) {
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", &fakeDoer{resp: precipFixture})
	c.now = func() time.Time { return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC) }
	f, err := c.GetGridForecast(context.Background(), "https://nws.test/gridpoints/STO/90,41", 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Points) != 6 {
		t.Fatalf("got %d points", len(f.Points))
	}
	if f.GridElevationM == nil || *f.GridElevationM != 715.9 {
		t.Errorf("grid elevation = %v", f.GridElevationM)
	}
	p0, p3, p5 := f.Points[0], f.Points[3], f.Points[5]
	if p0.PopPct == nil || *p0.PopPct != 20 || p3.PopPct == nil || *p3.PopPct != 0 {
		t.Errorf("pop = %v / %v; a forecast 0 must stay 0, not absent", p0.PopPct, p3.PopPct)
	}
	if p0.QpfMm == nil || *p0.QpfMm != 1 {
		t.Errorf("qpf = %v, want the 6 mm total spread as 1 mm/h", p0.QpfMm)
	}
	if p0.SnowMm != nil || p3.SnowMm == nil || *p3.SnowMm != 1 {
		t.Errorf("snow = %v / %v", p0.SnowMm, p3.SnowMm)
	}
	if p0.SnowLevelM == nil || *p0.SnowLevelM != 1500 || p5.SnowLevelM != nil {
		t.Errorf("snow level = %v / %v; absent hours stay nil", p0.SnowLevelM, p5.SnowLevelM)
	}
	if p0.SkyPct == nil || *p0.SkyPct != 50 {
		t.Errorf("sky = %v", p0.SkyPct)
	}
	if p0.Weather != "Chance of light rain showers" || p3.Weather != "" {
		t.Errorf("weather = %q / %q", p0.Weather, p3.Weather)
	}
}

func TestWeatherPhrase(t *testing.T) {
	cases := map[[3]string]string{
		{"slight_chance", "", "snow_showers"}: "Slight chance of snow showers",
		{"likely", "moderate", "rain"}:        "Likely moderate rain",
		{"", "", "fog"}:                       "Fog",
		{"areas", "none", "smoke"}:            "Areas smoke",
	}
	for in, want := range cases {
		if got := weatherPhrase(in[0], in[1], in[2]); got != want {
			t.Errorf("%v = %q, want %q", in, got, want)
		}
	}
}

func TestGetDailyForecast(t *testing.T) {
	doer := &fakeDoer{resp: `{"properties":{"periods":[
		{"name":"Tonight","startTime":"2026-11-10T18:00:00-08:00","endTime":"2026-11-11T06:00:00-08:00","isDaytime":false,
		 "temperature":10,"temperatureUnit":"C","probabilityOfPrecipitation":{"value":null},
		 "windSpeed":"5 km/h","windDirection":"E","shortForecast":"Clear","detailedForecast":"Clear, with a low around 10."},
		{"name":"Veterans Day","startTime":"2026-11-11T06:00:00-08:00","endTime":"2026-11-11T18:00:00-08:00","isDaytime":true,
		 "temperature":75,"temperatureUnit":"F","probabilityOfPrecipitation":{"value":0},
		 "windSpeed":"5 to 10 km/h","windDirection":"SW","shortForecast":"Sunny","detailedForecast":"Sunny."}]}}`}
	c := NewClientWithHTTPDoer("test-agent", "https://nws.test", doer)
	d, err := c.GetDailyForecast(context.Background(), "https://api.weather.gov/gridpoints/STO/90,41")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(doer.lastURL, "/gridpoints/STO/90,41/forecast?units=si") {
		t.Errorf("url = %q", doer.lastURL)
	}
	if len(d) != 2 || d[0].Name != "Tonight" || d[0].IsDaytime || d[0].PopPct != nil {
		t.Errorf("tonight = %+v", d[0])
	}
	if d[1].TempC < 23.8 || d[1].TempC > 23.9 || d[1].PopPct == nil || *d[1].PopPct != 0 {
		t.Errorf("day = %+v", d[1])
	}
	if d[0].Start.IsZero() || !d[0].End.After(d[0].Start) {
		t.Errorf("window = %v..%v", d[0].Start, d[0].End)
	}
}
