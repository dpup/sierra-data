package services

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type zoneDoer struct {
	zones map[string]string // path suffix -> geometry JSON
	calls int
}

func (d *zoneDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	for suffix, geom := range d.zones {
		if strings.HasSuffix(req.URL.Path, suffix) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"geometry":` + geom + `}`)), Header: http.Header{}}, nil
		}
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

func TestAlertZoneGeometry(t *testing.T) {
	d := &zoneDoer{zones: map[string]string{
		"/zones/fire/CAZ138":     `{"type":"Polygon","coordinates":[[[-120.5,38.0],[-120.0,38.0],[-120.0,38.5],[-120.5,38.5],[-120.5,38.0]]]}`,
		"/zones/forecast/CAZ139": `{"type":"Polygon","coordinates":[[[-120.0,38.0],[-119.5,38.0],[-119.5,38.5],[-120.0,38.0]]]}`,
	}}
	s := &WeatherService{nwsClient: nws.NewClientWithHTTPDoer("t", "https://nws.test", d), cache: cache.NewCache()}
	ctx := context.Background()

	a, err := s.AlertZoneGeometry(ctx, []string{"forecast/CAZ139", "fire/CAZ138"})
	require.NoError(t, err)
	assert.Contains(t, string(a), `"MultiPolygon"`)
	b, err := s.AlertZoneGeometry(ctx, []string{"fire/CAZ138", "forecast/CAZ139"})
	require.NoError(t, err)
	assert.Equal(t, a, b, "order of refs does not matter")
	assert.Equal(t, 2, d.calls, "each zone fetched once")

	_, err = s.AlertZoneGeometry(ctx, []string{"fire/CAZ138", "fire/CAZ999"})
	assert.Error(t, err, "all or nothing")
	g, err := s.AlertZoneGeometry(ctx, nil)
	assert.NoError(t, err)
	assert.Nil(t, g)
}
