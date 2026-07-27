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

	f := &Forecast{
		Source:       "NWS (" + gridID(gridURL) + ")",
		IssuedAt:     parseTime(p.UpdateTime),
		HorizonHours: int(horizon / time.Hour),
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
