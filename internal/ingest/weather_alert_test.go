package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/services"
)

type fakeWeatherAPI struct {
	alerts    []nws.Alert
	fetchedAt time.Time
	err       error
}

func (f *fakeWeatherAPI) RawNWSAlerts(ctx context.Context) ([]nws.Alert, time.Time, error) {
	return f.alerts, f.fetchedAt, f.err
}

func testNWSAlerts(now time.Time) []nws.Alert {
	return []nws.Alert{
		{
			ID:          "urn:oid:2.49.0.1.840.0.abc",
			Event:       "Red Flag Warning",
			Severity:    "Severe",
			Certainty:   "Likely",
			Urgency:     "Expected",
			Headline:    "Red Flag Warning until 8 PM PDT",
			Description: "* WHAT...Gusty winds and low humidity.",
			Instruction: "Avoid outdoor burning.",
			SenderName:  "NWS Sacramento CA",
			AreaDesc:    "West Slope Northern Sierra Nevada",
			Effective:   now.Add(-2 * time.Hour),
			Expires:     now.Add(8 * time.Hour),
			Zones:       []string{"CAZ064"},
		},
		{
			ID:         "urn:oid:2.49.0.1.840.0.def",
			Event:      "Winter Storm Watch",
			Severity:   "Moderate",
			SenderName: "NWS Sacramento CA",
			Zones:      []string{"CAZ258"},
			Effective:  now.Add(6 * time.Hour), // not yet effective
		},
		{
			ID:         "urn:oid:2.49.0.1.840.0.ghi",
			Event:      "Extreme Wind Warning",
			Severity:   "Extreme",
			SenderName: "NWS Sacramento CA",
			// No zones: can't be scoped, attaches to every area.
		},
	}
}

func TestWeatherAlertPoll(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{alerts: testNWSAlerts(now), fetchedAt: now})
	n.now = func() time.Time { return now }
	assert.Equal(t, []string{"nws"}, n.SourceIDs())

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	assert.Nil(t, res.PerSource)
	require.Len(t, res.Events, 3)

	active := res.Events[0]
	assert.Equal(t, "wx:urn:oid:2.49.0.1.840.0.abc", active.Id, "id must be wx:+NWSAlertID, byte-identical to shipped ids")
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

	// Envelope carries the event name (category) and sender (provenance); the
	// detail carries only the kind-specific NWS fields.
	assert.Equal(t, "Red Flag Warning", active.Category)
	assert.Equal(t, "NWS Sacramento CA", active.GetProvenance().GetSourceName())
	d := active.GetWeatherAlert()
	require.NotNil(t, d)
	assert.Equal(t, "Severe", d.NwsSeverity, "detail carries the RAW NWS vocabulary, not the api enum name")
	assert.Equal(t, "Likely", d.Certainty)
	assert.Equal(t, "Expected", d.Urgency)
	assert.Equal(t, "Avoid outdoor burning.", d.Instruction)
	assert.Equal(t, "West Slope Northern Sierra Nevada", d.AreaDesc)
	assert.Equal(t, []string{"CAZ064"}, d.Zones)

	// Future effective time => SCHEDULED; missing headline falls back to event.
	scheduled := res.Events[1]
	assert.Equal(t, gridv1.EventStatus_SCHEDULED, scheduled.Status)
	assert.Equal(t, "Winter Storm Watch", scheduled.Headline)
	assert.Equal(t, gridv1.Severity_MODERATE, scheduled.Severity)
	assert.Equal(t, []string{"area:tuolumne"}, scheduled.PlaceIds)
	assert.Equal(t, "Moderate", scheduled.GetWeatherAlert().NwsSeverity)

	// Zoneless alert attaches to every configured area; NWS "Extreme" maps to
	// EXTREME (the shipped api path collapsed it to SEVERE — deliberate
	// accuracy improvement).
	extreme := res.Events[2]
	assert.Equal(t, gridv1.EventStatus_ACTIVE, extreme.Status)
	assert.Equal(t, []string{"area:calaveras", "area:tuolumne"}, extreme.PlaceIds)
	assert.Equal(t, gridv1.Severity_EXTREME, extreme.Severity)
	assert.Equal(t, "Extreme", extreme.GetWeatherAlert().NwsSeverity)
}

// An alert with no upstream ID gets the synthesized id — through the SAME
// exported derivation the weather service ships, so the namespaced ids stay
// byte-identical.
func TestWeatherAlertPoll_SynthesizedID(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	a := nws.Alert{Event: "Flood Watch", Severity: "Minor", Effective: now.Add(-time.Hour)}
	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{alerts: []nws.Alert{a}})
	n.now = func() time.Time { return now }

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Equal(t, "wx:"+services.NWSAlertID(a), res.Events[0].Id)
}

// RawNWSAlerts serving a last-good stale list alongside the fetch error:
// events still emit (availability) AND nws carries the error (the sweep is
// skipped — a stale list must never expire alerts it merely failed to
// refresh).
func TestWeatherAlertPoll_StaleServeReportsDegraded(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{
		alerts:    testNWSAlerts(now),
		fetchedAt: now.Add(-30 * time.Minute),
		err:       assert.AnError,
	})
	n.now = func() time.Time { return now }

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err, "stale-with-data must degrade, not hard-fail")
	require.Len(t, res.Events, 3, "last-good alerts keep flowing")
	require.NotNil(t, res.PerSource)
	assert.Error(t, res.PerSource["nws"], "the source must report degraded while serving stale")
}

func TestWeatherAlertPollError(t *testing.T) {
	// Fetch failure with no usable cache: hard error, nothing sweeps.
	n := NewWeatherAlertNormalizer(testConfig(), &fakeWeatherAPI{err: assert.AnError})
	_, err := n.Poll(testCtx(), nil)
	assert.Error(t, err)
}

// Empty NWS-zone config is a hard error, not a success-empty poll that would let
// the sweep expire stored active alerts with no fetch (fail-loud mechanism 4).
func TestWeatherAlertPoll_EmptyScopeHardError(t *testing.T) {
	cfg := testConfig()
	cfg.Weather.NWS.Zones = nil
	n := NewWeatherAlertNormalizer(cfg, &fakeWeatherAPI{})
	_, err := n.Poll(testCtx(), nil)
	require.Error(t, err, "no configured NWS zones must be a hard error")
}
