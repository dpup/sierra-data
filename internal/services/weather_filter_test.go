package services

import (
	"testing"

	api "github.com/dpup/sierra-data/api/v1"
)

func TestFilterAlertsByZones(t *testing.T) {
	nwsIn := &api.WeatherAlert{Event: "Red Flag", Source: api.AlertSource_NWS, Zones: []string{"CAZ064"}}
	nwsOut := &api.WeatherAlert{Event: "Wind", Source: api.AlertSource_NWS, Zones: []string{"CAZ999"}}
	owm := &api.WeatherAlert{Event: "Heat", Source: api.AlertSource_OPENWEATHERMAP}
	all := []*api.WeatherAlert{nwsIn, nwsOut, owm}

	// No zones requested -> unchanged.
	if got := filterAlertsByZones(all, nil); len(got) != 3 {
		t.Errorf("no filter: got %d, want 3", len(got))
	}

	// Zone filter: keeps the matching NWS alert and the OWM alert (not
	// zone-scoped), drops the non-matching NWS alert.
	got := filterAlertsByZones(all, []string{"CAZ064"})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	events := map[string]bool{}
	for _, a := range got {
		events[a.Event] = true
	}
	if !events["Red Flag"] {
		t.Error("matching NWS alert should be kept")
	}
	if !events["Heat"] {
		t.Error("OpenWeatherMap alert should always pass (not zone-scoped)")
	}
	if events["Wind"] {
		t.Error("non-matching NWS alert should be dropped")
	}

	// Comma-separated zones are supported.
	if got := filterAlertsByZones(all, []string{"CAZ999,CAZ064"}); len(got) != 3 {
		t.Errorf("comma zones: got %d, want 3 (both NWS match + OWM)", len(got))
	}
}
