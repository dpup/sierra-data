package hazards

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEvacStatusInactiveMatchesNormalize feeds the same status table to both
// EvacStatusInactive and normalizeEvacLevel: for every NON-BLANK status the
// two must agree ("" from normalizeEvacLevel <=> explicitly inactive). This is
// the sync guard for the duplicated keyword list — if a keyword is added to
// one and not the other, this table fails.
func TestEvacStatusInactiveMatchesNormalize(t *testing.T) {
	statuses := []string{
		// Explicitly inactive phrasings.
		"Evacuation Order Lifted",
		"LIFTED",
		"Back to Normal",
		"All Clear",
		"all-clear",
		"Repopulation in progress",
		"Repopulate",
		"No Evacuation",
		"no evac",
		// Active phrasings, recognized.
		"Evacuation Order",
		"Mandatory Evacuation",
		"Evacuation Warning",
		"Shelter in Place",
		"Advisory",
		"Voluntary Evacuation",
		// Active-by-default (unrecognized, conservative WARNING).
		"Prepare to leave",
		"GO NOW",
	}
	for _, status := range statuses {
		inactive := EvacStatusInactive(status)
		dropped := normalizeEvacLevel(status) == ""
		assert.Equal(t, dropped, inactive,
			"status %q: EvacStatusInactive=%v but normalizeEvacLevel drop=%v — keyword lists out of sync",
			status, inactive, dropped)
	}
}

// Blank is the deliberate divergence: normalizeEvacLevel conflates blank with
// inactive (returns ""), but EvacStatusInactive must report blank as NOT
// explicitly inactive so ingest keeps the zone active with a conservative
// default instead of fabricating an all-clear.
func TestEvacStatusInactiveBlankIsNotInactive(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t\n"} {
		assert.Equal(t, "", normalizeEvacLevel(blank), "normalizeEvacLevel(%q)", blank)
		assert.False(t, EvacStatusInactive(blank),
			"EvacStatusInactive(%q): blank STATUS is missing data, never an explicit all-clear", blank)
	}
}

// SeverityFromNWSSeverity is the NWS-direct mapping: it must preserve
// "Extreme" as EXTREME (the api.AlertSeverity path collapses it to SEVERE).
func TestSeverityFromNWSSeverity(t *testing.T) {
	assert.Equal(t, SevExtreme, SeverityFromNWSSeverity("Extreme"))
	assert.Equal(t, SevSevere, SeverityFromNWSSeverity("Severe"))
	assert.Equal(t, SevModerate, SeverityFromNWSSeverity("Moderate"))
	assert.Equal(t, SevMinor, SeverityFromNWSSeverity("Minor"))
	assert.Equal(t, SevInfo, SeverityFromNWSSeverity("Unknown"))
	assert.Equal(t, SevInfo, SeverityFromNWSSeverity(""))
}
