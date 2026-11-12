package gridapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// A retired alert's pointer to its replacement is part of the API response,
// on the event and in its history, and survives the store round trip.
func TestGetEventCarriesReplacedByEventID(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)
	wx := func(id string) *gridv1.Event {
		return &gridv1.Event{
			Id: id, Layer: gridv1.Layer_WEATHER_ALERT, Category: "Winter Storm Watch",
			Severity: gridv1.Severity_MODERATE, Status: gridv1.EventStatus_ACTIVE, Headline: "Watch",
			Provenance: &gridv1.Provenance{SourceId: "nws", SourceName: "NWS"},
			Detail:     &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{NwsSeverity: "Severe"}},
		}
	}
	upsert(t, s.Store, wx("wx:old"))
	upsert(t, s.Store, wx("wx:new"))

	require.NoError(t, s.Store.TransitionEventsAmending(rpcBG(), []string{"wx:old"}, gridv1.EventStatus_RESOLVED, time.Now(),
		func(ev *gridv1.Event) { ev.GetWeatherAlert().ReplacedByEventId = "wx:new" }))

	old, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "wx:old"})
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, old.GetStatus())
	assert.Equal(t, uint32(2), old.GetRevision(), "one revision for the retirement and the pointer together")
	assert.Equal(t, "wx:new", old.GetWeatherAlert().GetReplacedByEventId())
	assert.Equal(t, "Severe", old.GetWeatherAlert().GetNwsSeverity())

	fresh, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "wx:new"})
	require.NoError(t, err)
	assert.Empty(t, fresh.GetWeatherAlert().GetReplacedByEventId())

	hist, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "wx:old"})
	require.NoError(t, err)
	require.Len(t, hist.GetRevisions(), 2)
	for _, r := range hist.GetRevisions() {
		want := ""
		if r.GetRevision() == 2 {
			want = "wx:new"
		}
		assert.Equal(t, want, r.GetEvent().GetWeatherAlert().GetReplacedByEventId(), "revision %d", r.GetRevision())
	}
}
