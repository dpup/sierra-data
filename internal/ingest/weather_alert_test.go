package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
)

type fakeWeatherAPI struct {
	resp *api.ListWeatherAlertsResponse
	err  error
}

func (f *fakeWeatherAPI) ListWeatherAlerts(ctx context.Context, req *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error) {
	return f.resp, f.err
}

func TestWeatherAlertPoll(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	resp := &api.ListWeatherAlertsResponse{Alerts: []*api.WeatherAlert{
		{
			Id:          "nws-1",
			Event:       "Red Flag Warning",
			Headline:    "Red Flag Warning until 8 PM PDT",
			Description: "* WHAT...Gusty winds and low humidity.",
			SenderName:  "NWS Sacramento CA",
			Severity:    api.AlertSeverity_CRITICAL,
			Zones:       []string{"CAZ064"},
			StartTime:   timestamppb.New(now.Add(-2 * time.Hour)),
			EndTime:     timestamppb.New(now.Add(8 * time.Hour)),
		},
		{
			Id:         "nws-2",
			Event:      "Winter Storm Watch",
			SenderName: "NWS Sacramento CA",
			Severity:   api.AlertSeverity_WARNING,
			Zones:      []string{"CAZ258"},
			StartTime:  timestamppb.New(now.Add(6 * time.Hour)), // not yet effective
		},
		{
			Id:         "nws-3",
			Event:      "Special Weather Statement",
			SenderName: "NWS Sacramento CA",
			Severity:   api.AlertSeverity_INFO,
			// No zones: can't be scoped, attaches to every area.
		},
	}}

	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{resp: resp})
	n.now = func() time.Time { return now }
	assert.Equal(t, []string{"nws"}, n.SourceIDs())

	res, err := n.Poll(testCtx())
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	require.Len(t, res.Events, 3)

	active := res.Events[0]
	assert.Equal(t, "wx:nws-1", active.Id)
	assert.Equal(t, gridv1.Layer_WEATHER_ALERT, active.Layer)
	assert.Equal(t, "Red Flag Warning until 8 PM PDT", active.Headline)
	assert.Equal(t, "Red Flag Warning", active.Category)
	assert.Equal(t, gridv1.Severity_SEVERE, active.Severity)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, active.Status)
	assert.Equal(t, "* WHAT...Gusty winds and low humidity.", active.Description) // verbatim
	assert.Equal(t, []string{"area:calaveras"}, active.PlaceIds)
	assert.Equal(t, now.Add(-2*time.Hour), active.Effective.AsTime())
	assert.Equal(t, now.Add(8*time.Hour), active.Expires.AsTime())
	require.NotNil(t, active.Provenance)
	assert.Equal(t, "nws", active.Provenance.SourceId)
	assert.Equal(t, "NWS Sacramento CA", active.Provenance.SourceName)
	d := active.GetWeatherAlert()
	require.NotNil(t, d)
	assert.Equal(t, "Red Flag Warning", d.Event)
	assert.Equal(t, "CRITICAL", d.NwsSeverity)
	assert.Equal(t, "NWS Sacramento CA", d.SenderName)
	assert.Equal(t, []string{"CAZ064"}, d.Zones)

	// Future effective time => SCHEDULED; missing headline falls back to event.
	scheduled := res.Events[1]
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, scheduled.Status)
	assert.Equal(t, "Winter Storm Watch", scheduled.Headline)
	assert.Equal(t, gridv1.Severity_MODERATE, scheduled.Severity)
	assert.Equal(t, []string{"area:tuolumne"}, scheduled.PlaceIds)

	// Zoneless alert attaches to every configured area.
	zoneless := res.Events[2]
	assert.Equal(t, gridv1.EventStatus_ACTIVE, zoneless.Status)
	assert.Equal(t, []string{"area:calaveras", "area:tuolumne"}, zoneless.PlaceIds)
	assert.Equal(t, gridv1.Severity_MINOR, zoneless.Severity)
}

func TestWeatherAlertPollError(t *testing.T) {
	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{err: assert.AnError})
	_, err := n.Poll(testCtx())
	assert.Error(t, err)
}
