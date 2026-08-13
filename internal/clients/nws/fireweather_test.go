package nws

import (
	"testing"
	"time"
)

func TestClassifyFireWeather(t *testing.T) {
	redFlag := Alert{Event: eventRedFlagWarning, Headline: "RFW", Zones: []string{"CAZ064"}}
	watch := Alert{Event: eventFireWeatherWatch, Headline: "FWW", Zones: []string{"CAZ065"}}
	heat := Alert{Event: "Heat Advisory", Zones: []string{"CAZ064"}}

	tests := []struct {
		name   string
		alerts []Alert
		zones  []string
		want   string
		event  string
	}{
		{"no alerts", nil, []string{"CAZ064"}, FireWeatherNormal, ""},
		{"only heat advisory", []Alert{heat}, []string{"CAZ064"}, FireWeatherNormal, ""},
		{"red flag wins over watch", []Alert{watch, redFlag}, []string{"CAZ064", "CAZ065"}, FireWeatherRedFlag, eventRedFlagWarning},
		{"watch only", []Alert{watch}, []string{"CAZ065"}, FireWeatherElevated, eventFireWeatherWatch},
		{"red flag outside zone filtered out", []Alert{redFlag}, []string{"CAZ999"}, FireWeatherNormal, ""},
		{"no zone filter considers all", []Alert{redFlag}, nil, FireWeatherRedFlag, eventRedFlagWarning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := ClassifyFireWeather(tt.alerts, tt.zones)
			if fw.State != tt.want {
				t.Errorf("state = %q, want %q", fw.State, tt.want)
			}
			if fw.SourceEvent != tt.event {
				t.Errorf("source event = %q, want %q", fw.SourceEvent, tt.event)
			}
		})
	}
}

// TestClassifyFireWeather_NotYetBegunRedFlag: NWS lists a product from the
// moment it is ISSUED, routinely hours before the weather. Reporting `red-flag`
// then asserts conditions NWS says have not started — observed live with the
// governing product's own effective 6 hours out. It drops to `elevated`:
// something real is coming (so not `normal`, which would hide it) but it is not
// here yet.
func TestClassifyFireWeather_NotYetBegunRedFlag(t *testing.T) {
	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	pending := Alert{
		Event: eventRedFlagWarning, Zones: []string{"CAZ139"},
		Onset: now.Add(6 * time.Hour), Ends: now.Add(22 * time.Hour),
		Headline: "Red Flag Warning — thunderstorms and strong outflow winds",
	}

	got := classifyFireWeatherAt([]Alert{pending}, []string{"CAZ139"}, now)
	if got.State != FireWeatherElevated {
		t.Errorf("state = %q, want %q for a Red Flag that starts in 6h", got.State, FireWeatherElevated)
	}
	if got.SourceEvent != eventRedFlagWarning {
		t.Errorf("the governing product should still be named: %q", got.SourceEvent)
	}

	// Once it begins, it is a Red Flag.
	if got := classifyFireWeatherAt([]Alert{pending}, []string{"CAZ139"}, now.Add(7*time.Hour)); got.State != FireWeatherRedFlag {
		t.Errorf("state = %q, want %q once onset has passed", got.State, FireWeatherRedFlag)
	}
}

// An in-force Red Flag still wins over one that has not begun.
func TestClassifyFireWeather_InForceBeatsPending(t *testing.T) {
	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	inForce := now.Add(-1 * time.Hour)
	alerts := []Alert{
		{Event: eventRedFlagWarning, Zones: []string{"CAZ139"}, Onset: now.Add(6 * time.Hour)},
		{Event: eventRedFlagWarning, Zones: []string{"CAZ139"}, Onset: inForce},
	}
	got := classifyFireWeatherAt(alerts, []string{"CAZ139"}, now)
	if got.State != FireWeatherRedFlag {
		t.Errorf("state = %q, want %q", got.State, FireWeatherRedFlag)
	}
	// The IN-FORCE product must be the one governing, not merely the first seen.
	if !got.Effective.Equal(inForce) {
		t.Errorf("governing alert effective = %v, want the in-force one at %v", got.Effective, inForce)
	}
}

// A pending WARNING outranks a WATCH — both elevated, but the warning is the
// more certain product so it supplies the headline.
func TestClassifyFireWeather_PendingWarningOutranksWatch(t *testing.T) {
	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	alerts := []Alert{
		{Event: eventFireWeatherWatch, Zones: []string{"CAZ139"}, Onset: now.Add(2 * time.Hour)},
		{Event: eventRedFlagWarning, Zones: []string{"CAZ139"}, Onset: now.Add(6 * time.Hour)},
	}
	got := classifyFireWeatherAt(alerts, []string{"CAZ139"}, now)
	if got.State != FireWeatherElevated {
		t.Errorf("state = %q, want %q", got.State, FireWeatherElevated)
	}
	if got.SourceEvent != eventRedFlagWarning {
		t.Errorf("the warning is the more certain product and should govern, got %q", got.SourceEvent)
	}
}

// An alert with no onset at all keeps the old behaviour: present means in force.
func TestClassifyFireWeather_NoOnsetIsInForce(t *testing.T) {
	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	got := classifyFireWeatherAt([]Alert{{Event: eventRedFlagWarning, Zones: []string{"CAZ139"}}}, []string{"CAZ139"}, now)
	if got.State != FireWeatherRedFlag {
		t.Errorf("state = %q, want %q when the product publishes no start time", got.State, FireWeatherRedFlag)
	}
}
