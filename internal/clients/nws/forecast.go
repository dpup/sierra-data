package nws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound signals a NWS 404 — e.g. a cached gridpoint URL that stopped
// resolving because NWS re-tiled the grid. Callers use it to invalidate + re-resolve.
var ErrNotFound = errors.New("nws: not found")

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// ForecastPoint is one hourly step of a location's fire-weather forecast. Speeds
// are km/h, temperature °C, humidity/direction as-is — the NWS gridpoint's native
// units, normalized on ingest (see convertUnit).
type ForecastPoint struct {
	Time        time.Time
	TempC       float64
	HumidityPct float64
	WindKmh     float64
	WindDirDeg  float64
	WindGustKmh float64

	// Precipitation and sky, nil when NWS gave no value for the hour (never 0:
	// "0% chance" and "no forecast" are different statements).
	PopPct     *float64 // probability of precipitation
	QpfMm      *float64 // liquid precipitation, mm in this hour (see accumulate)
	SnowMm     *float64 // snowfall, mm in this hour
	SnowLevelM *float64 // snow level, metres above sea level
	SkyPct     *float64 // sky cover
	// Weather is the grid's weather for the hour in words, e.g. "Chance of
	// light rain showers"; empty when NWS forecasts none.
	Weather string
}

// DailyPeriod is one of NWS's named 12-hour forecast periods ("Tonight",
// "Wednesday"), from the point's /forecast product in SI units.
type DailyPeriod struct {
	Name             string
	Start, End       time.Time
	IsDaytime        bool
	TempC            float64
	PopPct           *float64
	WindSpeed        string // NWS text, e.g. "5 to 10 km/h"
	WindDirection    string // compass point, e.g. "SW"
	ShortForecast    string
	DetailedForecast string
}

// Forecast is a location's short-range fire-weather forecast (NWS gridpoint): the
// hourly series plus an at-a-glance summary. Exported fields so it round-trips
// through the JSON cache. The summary is computed from the raw grid presence maps
// (not the emitted Points), so a missing value never masquerades as 0.
type Forecast struct {
	Source         string
	IssuedAt       time.Time
	HorizonHours   int
	Points         []ForecastPoint
	PeakGustKmh    float64
	PeakGustAt     time.Time
	MinHumidityPct float64
	HasMinHumidity bool
	// GridElevationM is the elevation NWS forecasts the grid cell at, which
	// can differ from the town's by hundreds of metres. Nil when not given.
	GridElevationM *float64
	// Daily is the 7-day forecast in 12-hour periods. Empty when that product
	// could not be fetched.
	Daily []DailyPeriod
}

// ResolveForecastURL maps a coordinate to its NWS forecast-grid URL via
// /points/{lat},{lng}. The mapping is static per location, so callers cache it.
func (c *Client) ResolveForecastURL(ctx context.Context, lat, lng float64) (string, error) {
	url := fmt.Sprintf("%s/points/%.4f,%.4f", c.baseURL, lat, lng)
	var out struct {
		Properties struct {
			ForecastGridData string `json:"forecastGridData"`
		} `json:"properties"`
	}
	if err := c.getJSON(ctx, url, &out); err != nil {
		return "", err
	}
	if out.Properties.ForecastGridData == "" {
		return "", fmt.Errorf("NWS points: no forecastGridData for %.4f,%.4f", lat, lng)
	}
	return out.Properties.ForecastGridData, nil
}

// GetGridForecast fetches the forecast-grid data and expands its run-length-
// encoded per-variable spans into aligned hourly ForecastPoints within horizon
// (from the current top-of-hour). Each grid variable is a sparse series of
// {validTime:"<RFC3339>/<ISO-8601 duration>", value} spans, with different span
// counts per variable, so we expand each to a per-hour map and merge.
func (c *Client) GetGridForecast(ctx context.Context, gridURL string, horizon time.Duration) (*Forecast, error) {
	var out struct {
		Properties struct {
			UpdateTime       string       `json:"updateTime"`
			Temperature      gridVariable `json:"temperature"`
			RelativeHumidity gridVariable `json:"relativeHumidity"`
			WindSpeed        gridVariable `json:"windSpeed"`
			WindDirection    gridVariable `json:"windDirection"`
			WindGust         gridVariable `json:"windGust"`
			Elevation        struct {
				Value *float64 `json:"value"`
			} `json:"elevation"`
			ProbabilityOfPrecipitation gridVariable    `json:"probabilityOfPrecipitation"`
			QuantitativePrecipitation  gridVariable    `json:"quantitativePrecipitation"`
			SnowfallAmount             gridVariable    `json:"snowfallAmount"`
			SnowLevel                  gridVariable    `json:"snowLevel"`
			SkyCover                   gridVariable    `json:"skyCover"`
			Weather                    weatherVariable `json:"weather"`
		} `json:"properties"`
	}
	if err := c.getJSON(ctx, gridURL, &out); err != nil {
		return nil, err
	}
	p := out.Properties
	start := c.nowUTC().Truncate(time.Hour)
	end := start.Add(horizon)

	temp := p.Temperature.hourly(start, end)
	rh := p.RelativeHumidity.hourly(start, end)
	wind := p.WindSpeed.hourly(start, end)
	dir := p.WindDirection.hourly(start, end)
	gust := p.WindGust.hourly(start, end)
	pop := p.ProbabilityOfPrecipitation.hourly(start, end)
	qpf := p.QuantitativePrecipitation.accumulate(start, end)
	snow := p.SnowfallAmount.accumulate(start, end)
	snowLevel := p.SnowLevel.hourly(start, end)
	sky := p.SkyCover.hourly(start, end)
	wx := p.Weather.hourly(start, end)

	f := &Forecast{
		Source:       "NWS (" + gridID(gridURL) + ")",
		IssuedAt:     parseTime(p.UpdateTime),
		HorizonHours: int(horizon / time.Hour),
	}
	if v := p.Elevation.Value; v != nil {
		e := *v
		f.GridElevationM = &e
	}
	minRH := math.MaxFloat64
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		// Summary is computed from the presence maps, independent of which points
		// are emitted, so a gust- or RH-only hour still counts.
		if g, ok := gust[t]; ok && g > f.PeakGustKmh {
			f.PeakGustKmh, f.PeakGustAt = g, t
		}
		if h, ok := rh[t]; ok && h < minRH {
			minRH, f.HasMinHumidity = h, true
		}
		// Emit an hourly point ONLY when both core fire variables are present, so an
		// absent wind or RH is never serialized as a false 0 (a false 0% RH reads as
		// catastrophic dryness; a false 0 km/h hides a windy hour).
		w, hasW := wind[t]
		h, hasH := rh[t]
		if !hasW || !hasH {
			continue
		}
		f.Points = append(f.Points, ForecastPoint{
			Time: t, TempC: temp[t], HumidityPct: h, WindKmh: w, WindDirDeg: dir[t], WindGustKmh: gust[t],
			PopPct: at(pop, t), QpfMm: at(qpf, t), SnowMm: at(snow, t), SnowLevelM: at(snowLevel, t), SkyPct: at(sky, t),
			Weather: wx[t],
		})
	}
	if f.HasMinHumidity {
		f.MinHumidityPct = minRH
	}
	return f, nil
}

// getJSON performs a NWS GET (User-Agent required) and decodes into dst.
func (c *Client) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create NWS request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/geo+json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute NWS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("NWS API error 404 (%s): %w", url, ErrNotFound)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("NWS API error %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (c *Client) nowUTC() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

// --- grid variable parsing (RLE spans → hourly) ---

type gridVariable struct {
	UOM    string          `json:"uom"`
	Values []gridTimeValue `json:"values"`
}

type gridTimeValue struct {
	ValidTime string   `json:"validTime"` // "2026-07-27T12:00:00+00:00/PT5H"
	Value     *float64 `json:"value"`
}

// hourly expands each span into a per-hour value within [start, end), converting
// to normalized units. A nil value (gap) contributes no hours.
func (v gridVariable) hourly(start, end time.Time) map[time.Time]float64 {
	out := make(map[time.Time]float64)
	for _, tv := range v.Values {
		if tv.Value == nil {
			continue
		}
		spanStart, dur, err := parseISOInterval(tv.ValidTime)
		if err != nil {
			continue
		}
		val := convertUnit(v.UOM, *tv.Value)
		spanEnd := spanStart.Add(dur)
		for t := spanStart.UTC().Truncate(time.Hour); t.Before(spanEnd); t = t.Add(time.Hour) {
			if !t.Before(start) && t.Before(end) {
				out[t] = val
			}
		}
	}
	return out
}

// accumulate expands an AMOUNT variable (precipitation, snowfall), whose span
// value is a total over the span, into an equal share per hour. hourly would
// repeat a 6-hour total in each of the six hours.
func (v gridVariable) accumulate(start, end time.Time) map[time.Time]float64 {
	out := make(map[time.Time]float64)
	for _, tv := range v.Values {
		if tv.Value == nil {
			continue
		}
		spanStart, dur, err := parseISOInterval(tv.ValidTime)
		if err != nil || dur < time.Hour {
			continue
		}
		share := convertUnit(v.UOM, *tv.Value) / float64(dur/time.Hour)
		spanEnd := spanStart.Add(dur)
		for t := spanStart.UTC().Truncate(time.Hour); t.Before(spanEnd); t = t.Add(time.Hour) {
			if !t.Before(start) && t.Before(end) {
				out[t] = share
			}
		}
	}
	return out
}

func at(m map[time.Time]float64, t time.Time) *float64 {
	if v, ok := m[t]; ok {
		return &v
	}
	return nil
}

// weatherVariable is the grid's `weather` series: each span lists zero or
// more conditions (coverage, intensity, weather type) in NWS vocabulary.
type weatherVariable struct {
	Values []struct {
		ValidTime string `json:"validTime"`
		Value     []struct {
			Coverage  *string `json:"coverage"`
			Weather   *string `json:"weather"`
			Intensity *string `json:"intensity"`
		} `json:"value"`
	} `json:"values"`
}

func (v weatherVariable) hourly(start, end time.Time) map[time.Time]string {
	out := make(map[time.Time]string)
	for _, tv := range v.Values {
		spanStart, dur, err := parseISOInterval(tv.ValidTime)
		if err != nil {
			continue
		}
		var parts []string
		for _, c := range tv.Value {
			if c.Weather == nil || *c.Weather == "" {
				continue
			}
			parts = append(parts, weatherPhrase(deref(c.Coverage), deref(c.Intensity), *c.Weather))
		}
		if len(parts) == 0 {
			continue
		}
		text := strings.Join(parts, "; ")
		spanEnd := spanStart.Add(dur)
		for t := spanStart.UTC().Truncate(time.Hour); t.Before(spanEnd); t = t.Add(time.Hour) {
			if !t.Before(start) && t.Before(end) {
				out[t] = text
			}
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// weatherPhrase renders one NWS grid condition in words, deterministically:
// ("chance", "light", "rain_showers") -> "Chance of light rain showers".
func weatherPhrase(coverage, intensity, weather string) string {
	words := func(s string) string { return strings.ReplaceAll(s, "_", " ") }
	w := words(weather)
	if intensity != "" && intensity != "none" {
		w = words(intensity) + " " + w
	}
	var out string
	switch coverage {
	case "slight_chance", "chance":
		out = words(coverage) + " of " + w
	case "", "definite":
		out = w
	default: // likely, areas, patchy, isolated, scattered, numerous, ...
		out = words(coverage) + " " + w
	}
	return strings.ToUpper(out[:1]) + out[1:]
}

// GetDailyForecast fetches the point's named 12-hour periods (7 days) from
// the grid's /forecast product in SI units.
func (c *Client) GetDailyForecast(ctx context.Context, gridURL string) ([]DailyPeriod, error) {
	var out struct {
		Properties struct {
			Periods []struct {
				Name                       string  `json:"name"`
				StartTime                  string  `json:"startTime"`
				EndTime                    string  `json:"endTime"`
				IsDaytime                  bool    `json:"isDaytime"`
				Temperature                float64 `json:"temperature"`
				TemperatureUnit            string  `json:"temperatureUnit"`
				ProbabilityOfPrecipitation struct {
					Value *float64 `json:"value"`
				} `json:"probabilityOfPrecipitation"`
				WindSpeed        string `json:"windSpeed"`
				WindDirection    string `json:"windDirection"`
				ShortForecast    string `json:"shortForecast"`
				DetailedForecast string `json:"detailedForecast"`
			} `json:"periods"`
		} `json:"properties"`
	}
	if err := c.getJSON(ctx, strings.TrimSuffix(gridURL, "/")+"/forecast?units=si", &out); err != nil {
		return nil, err
	}
	var periods []DailyPeriod
	for _, p := range out.Properties.Periods {
		temp := p.Temperature
		if p.TemperatureUnit == "F" {
			temp = (temp - 32) * 5 / 9
		}
		d := DailyPeriod{
			Name: p.Name, Start: parseTime(p.StartTime), End: parseTime(p.EndTime), IsDaytime: p.IsDaytime,
			TempC: temp, WindSpeed: p.WindSpeed, WindDirection: p.WindDirection,
			ShortForecast: p.ShortForecast, DetailedForecast: p.DetailedForecast,
		}
		if v := p.ProbabilityOfPrecipitation.Value; v != nil {
			pv := *v
			d.PopPct = &pv
		}
		periods = append(periods, d)
	}
	if len(periods) == 0 {
		return nil, fmt.Errorf("NWS forecast: no periods")
	}
	return periods, nil
}

// parseISOInterval splits "<RFC3339>/<ISO-8601 duration>" into a start time and
// duration.
func parseISOInterval(s string) (time.Time, time.Duration, error) {
	slash := strings.LastIndexByte(s, '/')
	if slash < 0 {
		return time.Time{}, 0, fmt.Errorf("bad interval %q", s)
	}
	t, err := time.Parse(time.RFC3339, s[:slash])
	if err != nil {
		return time.Time{}, 0, err
	}
	d, err := parseISODuration(s[slash+1:])
	if err != nil {
		return time.Time{}, 0, err
	}
	return t, d, nil
}

// parseISODuration parses the P[nD]T[nH][nM][nS] subset NWS emits (days/hours,
// occasionally minutes). Weeks/months/years don't appear in gridpoint spans.
func parseISODuration(s string) (time.Duration, error) {
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	s = s[1:]
	var d time.Duration
	datePart, timePart := s, ""
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
	}
	if datePart != "" {
		if !strings.HasSuffix(datePart, "D") {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		n, err := strconv.Atoi(strings.TrimSuffix(datePart, "D"))
		if err != nil {
			return 0, err
		}
		d += time.Duration(n) * 24 * time.Hour
	}
	for _, u := range []struct {
		suf byte
		mul time.Duration
	}{{'H', time.Hour}, {'M', time.Minute}, {'S', time.Second}} {
		if i := strings.IndexByte(timePart, u.suf); i >= 0 {
			n, err := strconv.Atoi(timePart[:i])
			if err != nil {
				return 0, err
			}
			d += time.Duration(n) * u.mul
			timePart = timePart[i+1:]
		}
	}
	return d, nil
}

// convertUnit normalizes a WMO unit to the client's units (km/h speeds, °C temp).
// The grid usually emits km_h-1 / degC already; the conversions are defensive.
func convertUnit(uom string, v float64) float64 {
	switch {
	case strings.Contains(uom, "m_s-1"):
		return v * 3.6 // m/s → km/h
	case strings.Contains(uom, "degF"):
		return (v - 32) * 5 / 9 // °F → °C
	case strings.HasSuffix(uom, ":ft"):
		return v * 0.3048 // ft → m (snow level is normally wmoUnit:m)
	case strings.HasSuffix(uom, ":in"):
		return v * 25.4 // in → mm (amounts are normally wmoUnit:mm)
	default:
		return v // km_h-1, degC, percent, degree
	}
}

// gridID renders ".../gridpoints/STO/90,41" as "STO 90,41" for the source label.
func gridID(gridURL string) string {
	i := strings.Index(gridURL, "/gridpoints/")
	if i < 0 {
		return "gridpoint"
	}
	return strings.Replace(strings.TrimPrefix(gridURL[i:], "/gridpoints/"), "/", " ", 1)
}
