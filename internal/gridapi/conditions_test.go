package gridapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roadsBody decodes a ListRoadsResponse / GetRoadResponse protojson body
// loosely (map-based so we can also assert key absence).
type roadsBody struct {
	Roads []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"roads"`
	LastUpdated string `json:"last_updated"`
}

func TestRoadsPassthrough(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/roads")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	var out roadsBody
	decode(t, rec, &out)
	require.Len(t, out.Roads, 2)
	assert.Equal(t, "hwy-4", out.Roads[0].ID)
	assert.Equal(t, "OPEN", out.Roads[0].Status, "shipped response shape preserved (enum names)")
	assert.NotEmpty(t, out.LastUpdated)
}

func TestRoadsPlaceFilter(t *testing.T) {
	s := newTestService(t)

	// high-country's box contains only hwy-4's destination endpoint.
	rec := get(t, s, "/v1/roads?place=high-country")
	require.Equal(t, http.StatusOK, rec.Code)
	var out roadsBody
	decode(t, rec, &out)
	require.Len(t, out.Roads, 1)
	assert.Equal(t, "hwy-4", out.Roads[0].ID)

	t.Run("wider place keeps both", func(t *testing.T) {
		rec := get(t, s, "/v1/roads?place=calaveras")
		require.Equal(t, http.StatusOK, rec.Code)
		var out roadsBody
		decode(t, rec, &out)
		assert.Len(t, out.Roads, 2)
	})
	t.Run("point place matches nothing", func(t *testing.T) {
		// A town's geometry is a point: its bbox is degenerate, so no road
		// endpoint falls inside — empty list, not an error.
		rec := get(t, s, "/v1/roads?place=arnold")
		require.Equal(t, http.StatusOK, rec.Code)
		var out roadsBody
		decode(t, rec, &out)
		assert.Empty(t, out.Roads)
	})
	t.Run("unknown place 404", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/roads?place=atlantis"), http.StatusNotFound, 5)
	})
}

func TestRoadByID(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/roads/hwy-108")
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Road struct {
			ID                string `json:"id"`
			StatusExplanation string `json:"status_explanation"`
		} `json:"road"`
		LastUpdated string `json:"last_updated"`
	}
	decode(t, rec, &out)
	assert.Equal(t, "hwy-108", out.Road.ID)
	assert.Equal(t, "One-way traffic control", out.Road.StatusExplanation)
	assert.NotEmpty(t, out.LastUpdated)

	requireStatus(t, get(t, s, "/v1/roads/hwy-999"), http.StatusNotFound, 5)
}

func TestRoadsUpstreamError(t *testing.T) {
	s := newTestService(t)
	s.Roads = &fakeRoads{err: errors.New("google routes 500")}
	rec := get(t, s, "/v1/roads")
	sb := requireStatus(t, rec, http.StatusInternalServerError, 13)
	assert.Equal(t, "internal error", sb.Message, "upstream detail must not leak")
}

// rawWeather decodes weather bodies into maps so alert stripping can be
// asserted as key absence, not just emptiness.
type rawWeather struct {
	WeatherData []map[string]json.RawMessage `json:"weather_data"`
	FireWeather map[string]json.RawMessage   `json:"fire_weather"`
	LastUpdated string                       `json:"last_updated"`
}

func TestWeatherAlertsStrippedFireWeatherKept(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/weather")
	require.Equal(t, http.StatusOK, rec.Code)
	var out rawWeather
	decode(t, rec, &out)
	require.Len(t, out.WeatherData, 2)
	for _, wd := range out.WeatherData {
		_, hasAlerts := wd["alerts"]
		assert.False(t, hasAlerts, "alerts are events now — never on /v1/weather")
		assert.Contains(t, wd, "location_id", "snake_case field names on the wire")
	}
	require.NotNil(t, out.FireWeather, "fire_weather survives the strip")
	assert.JSONEq(t, `"RED_FLAG"`, string(out.FireWeather["state"]))

	// Stripping must happen on a clone: the fake's response (standing in for
	// the weather service's cached state) keeps its alerts.
	fw := s.Weather.(*fakeWeather)
	require.NotEmpty(t, fw.resp.WeatherData[0].Alerts, "upstream response must not be mutated")
}

func TestWeatherByLocationAndPlace(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/weather/murphys")
	require.Equal(t, http.StatusOK, rec.Code)
	var single struct {
		WeatherData map[string]json.RawMessage `json:"weather_data"`
		FireWeather map[string]json.RawMessage `json:"fire_weather"`
	}
	decode(t, rec, &single)
	assert.JSONEq(t, `"murphys"`, string(single.WeatherData["location_id"]))
	_, hasAlerts := single.WeatherData["alerts"]
	assert.False(t, hasAlerts)
	assert.NotNil(t, single.FireWeather)

	t.Run("unknown location 404", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/weather/lodi"), http.StatusNotFound, 5)
	})
	t.Run("place filter", func(t *testing.T) {
		// Arnold is inside high-country's box; Murphys is south of it.
		rec := get(t, s, "/v1/weather?place=high-country")
		require.Equal(t, http.StatusOK, rec.Code)
		var out rawWeather
		decode(t, rec, &out)
		require.Len(t, out.WeatherData, 1)
		assert.JSONEq(t, `"arnold"`, string(out.WeatherData[0]["location_id"]))
	})
	t.Run("place filter scopes the id lookup too", func(t *testing.T) {
		rec := get(t, s, "/v1/weather/murphys?place=high-country")
		requireStatus(t, rec, http.StatusNotFound, 5)
	})
	t.Run("unknown place 404", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/weather?place=atlantis"), http.StatusNotFound, 5)
	})
}
