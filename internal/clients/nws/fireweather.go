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
	return classifyFireWeatherAt(alerts, zones, time.Now())
}

// classifyFireWeatherAt is ClassifyFireWeather with an injected clock, so the
// not-yet-begun branch is testable.
//
// THE ONSET SPLIT. NWS lists a product on /alerts/active from the moment it is
// ISSUED, which is routinely hours before the weather. A Red Flag Warning
// issued at 06:00 for noon was therefore reported as `red-flag` at 06:00 — the
// service asserting Red Flag conditions that NWS says do not start for six
// hours. Observed live: state "red-flag" with the governing product's own
// effective 6.0 h in the future.
//
// That is the same mistake the 2026-08-11 change fixed for the ALERT (which now
// reads SCHEDULED until onset) and never propagated to the classification
// derived from it.
//
// A not-yet-begun Red Flag drops to `elevated` rather than `normal`: something
// real is issued and coming, and `normal` would hide it. `elevated` is the rung
// a Watch already occupies — "prepare, it isn't here yet" — and it escalates a
// place's mode to WATCH exactly as `red-flag` does, so nothing is under-alerted
// by this. What changes is only the claim about what is happening NOW.
func classifyFireWeatherAt(alerts []Alert, zones []string, now time.Time) FireWeather {
	zoneSet := make(map[string]bool)
	for _, z := range cleanZones(zones) {
		zoneSet[z] = true
	}

	var redFlag, pendingRedFlag, watch *Alert
	for i := range alerts {
		a := &alerts[i]
		if len(zoneSet) > 0 && !alertIntersectsZones(a, zoneSet) {
			continue
		}
		switch a.Event {
		case eventRedFlagWarning:
			// Begins() is the HAZARD's start (CAP onset, falling back to
			// effective) — not the product's issuance.
			if begins := a.Begins(); !begins.IsZero() && begins.After(now) {
				if pendingRedFlag == nil {
					pendingRedFlag = a
				}
				continue
			}
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
	// An imminent Red Flag outranks a Watch: both read `elevated`, but the
	// Warning is the more certain product, so it governs the headline.
	if pendingRedFlag != nil {
		return fireWeatherFromAlert(FireWeatherElevated, pendingRedFlag)
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
