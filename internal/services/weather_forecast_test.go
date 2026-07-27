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

func TestLocationForecasts_Success(t *testing.T) {
	doer := forecastDoer{points: `{"properties":{"forecastGridData":"https://nws.test/gridpoints/STO/90,41"}}`, grid: gridBody()}
	svc := NewWeatherService(nil, nws.NewClientWithHTTPDoer("test", "https://nws.test", doer), cache.NewCache(), forecastConfig(true))

	fcs := svc.LocationForecasts(testCtx())
	require.Len(t, fcs, 1)
	f := fcs["arnold"]
	require.NotNil(t, f)
	assert.Len(t, f.Points, 6)
	assert.Equal(t, float64(30), f.PeakGustKmh)
	assert.True(t, f.HasMinHumidity)
	assert.Equal(t, float64(15), f.MinHumidityPct)
	assert.Contains(t, f.Source, "STO 90,41")

	// Second call is served from the fresh cache — no additional upstream fetch is
	// needed (the gridurl + forecast are both cached).
	assert.NotNil(t, svc.LocationForecasts(testCtx())["arnold"])
}

func TestLocationForecasts_Disabled(t *testing.T) {
	doer := forecastDoer{points: `{}`, grid: gridBody()}
	svc := NewWeatherService(nil, nws.NewClientWithHTTPDoer("test", "https://nws.test", doer), cache.NewCache(), forecastConfig(false))
	assert.Nil(t, svc.LocationForecasts(testCtx()))
}

func TestLocationForecasts_FailSoft(t *testing.T) {
	// Upstream errors with no prior cache → the location is omitted, never an error.
	doer := forecastDoer{err: errors.New("boom")}
	svc := NewWeatherService(nil, nws.NewClientWithHTTPDoer("test", "https://nws.test", doer), cache.NewCache(), forecastConfig(true))
	fcs := svc.LocationForecasts(testCtx())
	assert.Empty(t, fcs, "a failed forecast fetch omits the location rather than erroring")
}
