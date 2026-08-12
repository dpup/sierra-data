package nws

import (
	"strings"
	"time"
)

// Fire-weather states, ordered Normal -> Elevated -> Red Flag. Red Flag is the
// only alarm signal and is only ever reported when an actual NWS Red Flag
// Warning is in effect (issue #5: "never reports a Red Flag it can't confirm").
const (
	FireWeatherNormal   = "normal"
	FireWeatherElevated = "elevated"
	FireWeatherRedFlag  = "red-flag"
)

// NWS fire-weather product event names.
const (
	eventRedFlagWarning   = "Red Flag Warning"
	eventFireWeatherWatch = "Fire Weather Watch"
)

// FireWeather is a derived fire-weather classification for a set of zones.
type FireWeather struct {
	State       string    // normal | elevated | red-flag
	SourceEvent string    // The NWS product driving the state (empty when normal)
	Headline    string    // Headline of the governing alert
	SenderName  string    // Issuing office
	Effective   time.Time // Zero when normal
	Expires     time.Time // Zero when normal
	Zones       []string  // Zones the governing alert applies to
}

// ClassifyFireWeather derives the fire-weather state from active alerts. If
// zones is non-empty, only alerts intersecting those zones are considered;
// otherwise all alerts are considered. A Red Flag Warning always wins over a
// Fire Weather Watch.
func ClassifyFireWeather(alerts []Alert, zones []string) FireWeather {
	zoneSet := make(map[string]bool)
	for _, z := range cleanZones(zones) {
		zoneSet[z] = true
	}

	var redFlag, watch *Alert
	for i := range alerts {
		a := &alerts[i]
		if len(zoneSet) > 0 && !alertIntersectsZones(a, zoneSet) {
			continue
		}
		switch a.Event {
		case eventRedFlagWarning:
			if redFlag == nil {
				redFlag = a
			}
		case eventFireWeatherWatch:
			if watch == nil {
				watch = a
			}
		}
	}

	if redFlag != nil {
		return fireWeatherFromAlert(FireWeatherRedFlag, redFlag)
	}
	if watch != nil {
		return fireWeatherFromAlert(FireWeatherElevated, watch)
	}
	return FireWeather{State: FireWeatherNormal}
}

func alertIntersectsZones(a *Alert, zoneSet map[string]bool) bool {
	for _, z := range a.Zones {
		if zoneSet[strings.ToUpper(strings.TrimSpace(z))] {
			return true
		}
	}
	return false
}

func fireWeatherFromAlert(state string, a *Alert) FireWeather {
	return FireWeather{
		State:       state,
		SourceEvent: a.Event,
		// The banner takes the same compact line as the event card; it used to
		// carry the CAP boilerplate, which put the issuing office and two
		// timestamps into conditions.fireWeather.headline.
		Headline:   a.ShortHeadline(),
		SenderName: a.SenderName,
		Effective:  a.Begins(),
		Expires:    a.EndsAt(),
		Zones:      a.Zones,
	}
}
