package services

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/clients/nws"
)

// forecastDoer routes /points → the points body and gridpoints → the grid body,
// or fails everything when err is set.
type forecastDoer struct {
	points, grid string
	err          error
}

func (d forecastDoer) Do(req *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	body := d.grid
	if strings.Contains(req.URL.Path, "/points/") {
		body = d.points
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// gridBody builds a 6h grid fixture anchored at the current top-of-hour so the
// real-clock forecast horizon overlaps it: wind 10, gust 30, RH 15, temp 20.
func gridBody() string {
	base := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	issued := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	v := func(unit string, val int) string {
		return fmt.Sprintf(`{"uom":%q,"values":[{"validTime":"%s/PT6H","value":%d}]}`, unit, base, val)
	}
	return fmt.Sprintf(`{"properties":{"updateTime":%q,
		"temperature":%s,"relativeHumidity":%s,"windSpeed":%s,"windDirection":%s,"windGust":%s}}`,
		issued, v("wmoUnit:degC", 20), v("wmoUnit:percent", 15), v("wmoUnit:km_h-1", 10),
		v("wmoUnit:degree_(angle)", 180), v("wmoUnit:km_h-1", 30))
}

func forecastConfig(enabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.Weather.Forecast = config.ForecastConfig{Enabled: enabled, RefreshInterval: time.Hour, HorizonHours: 6}
	cfg.Weather.Locations = []config.WeatherLocation{
		{ID: "arnold", Name: "Arnold", Coordinates: config.Coordinates{Latitude: 38.25, Longitude: -120.35}},
	}
	return cfg
}

const pointsBody = `{"properties":{"forecastGridData":"https://nws.test/gridpoints/STO/90,41"}}`

func newForecastSvc(c *cache.Cache, doer nws.HTTPDoer, enabled bool) *WeatherService {
	return NewWeatherService(nil, nws.NewClientWithHTTPDoer("test", "https://nws.test", doer), c, forecastConfig(enabled))
}

func TestForecasts_WarmThenRead(t *testing.T) {
	svc := newForecastSvc(cache.NewCache(), forecastDoer{points: pointsBody, grid: gridBody()}, true)

	// The request path never fetches: before warming, LocationForecasts is empty.
	assert.Empty(t, svc.LocationForecasts(testCtx()), "request path must not fetch on a cold cache")

	// The background refresher warms the cache; the read then returns it.
	svc.RefreshForecasts(testCtx())
	fcs := svc.LocationForecasts(testCtx())
	require.Len(t, fcs, 1)
	f := fcs["arnold"]
	require.NotNil(t, f)
	assert.Len(t, f.Points, 6)
	assert.Equal(t, float64(30), f.PeakGustKmh)
	assert.Equal(t, float64(15), f.MinHumidityPct)
	assert.Contains(t, f.Source, "STO 90,41")
}

func TestForecasts_Disabled(t *testing.T) {
	svc := newForecastSvc(cache.NewCache(), forecastDoer{points: pointsBody, grid: gridBody()}, false)
	svc.RefreshForecasts(testCtx()) // no-op
	assert.Nil(t, svc.LocationForecasts(testCtx()))
}

func TestForecasts_RefreshFailSoft(t *testing.T) {
	// Upstream errors, no prior cache → the location is omitted, never a panic/error.
	svc := newForecastSvc(cache.NewCache(), forecastDoer{err: errors.New("boom")}, true)
	svc.RefreshForecasts(testCtx())
	assert.Empty(t, svc.LocationForecasts(testCtx()))
}

func TestForecasts_ServeStaleThenOmit(t *testing.T) {
	c := cache.NewCache()
	svc := newForecastSvc(c, forecastDoer{points: pointsBody, grid: gridBody()}, true)
	svc.RefreshForecasts(testCtx())
	require.NotNil(t, svc.LocationForecasts(testCtx())["arnold"])

	// Stale (past the 1h TTL) but within the 2× very-stale bound → still served.
	c.Backdate("nws:forecast:arnold", 90*time.Minute)
	require.NotNil(t, svc.LocationForecasts(testCtx())["arnold"], "stale within 2x TTL is served")

	// Past the very-stale bound → omitted (never fabricate a forecast).
	c.Backdate("nws:forecast:arnold", 60*time.Minute) // ~150m total > 120m
	assert.Empty(t, svc.LocationForecasts(testCtx()), "past very-stale is omitted")
}

// reResolveDoer 404s the first grid fetch (a re-tiled gridpoint) and serves the
// grid on the second, so the re-resolve path can be exercised.
type reResolveDoer struct {
	points, grid           string
	pointsCalls, gridCalls int
}

func (d *reResolveDoer) Do(req *http.Request) (*http.Response, error) {
	body, status := d.grid, 200
	if strings.Contains(req.URL.Path, "/points/") {
		d.pointsCalls++
		body = d.points
	} else {
		d.gridCalls++
		if d.gridCalls == 1 {
			status = 404
			body = "not found"
		}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestForecasts_ReResolveOnGrid404(t *testing.T) {
	d := &reResolveDoer{points: pointsBody, grid: gridBody()}
	svc := newForecastSvc(cache.NewCache(), d, true)

	svc.RefreshForecasts(testCtx())

	// A 404 on the cached grid URL must invalidate it and re-resolve via /points,
	// so the forecast still lands (self-heals a moved gridpoint).
	require.NotNil(t, svc.LocationForecasts(testCtx())["arnold"], "forecast recovered after a grid 404")
	assert.Equal(t, 2, d.pointsCalls, "gridpoint URL was re-resolved after the 404")
	assert.Equal(t, 2, d.gridCalls)
}
