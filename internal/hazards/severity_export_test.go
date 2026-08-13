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

// TestSeverityFromPowerOutage pins the boundaries. The thresholds exist because
// the statewide MEDIAN PG&E outage affects ONE customer — half of every fetch is
// single-premise service calls — so the bottom of the scale has to absorb them
// or a real community-scale outage is buried under noise.
func TestSeverityFromPowerOutage(t *testing.T) {
	cases := []struct {
		customers int32
		planned   bool
		want      string
	}{
		{1, false, SevInfo},
		{9, false, SevInfo},
		{10, false, SevMinor},
		{99, false, SevMinor},
		{100, false, SevModerate},
		{999, false, SevModerate},
		{1000, false, SevSevere},
		{50000, false, SevSevere},
		// A pre-notified shutdown is one rank less urgent than the same
		// unplanned outage, floored at INFO — never demoted out of existence,
		// and a large planned de-energization still outranks a small unplanned one.
		{1000, true, SevModerate},
		{100, true, SevMinor},
		{10, true, SevInfo},
		{1, true, SevInfo},
	}
	for _, tc := range cases {
		got := SeverityFromPowerOutage(tc.customers, tc.planned)
		assert.Equal(t, tc.want, got, "customers=%d planned=%v", tc.customers, tc.planned)
	}
	// A demoted large outage must still outrank an undemoted small one.
	assert.Greater(t,
		severityRank(SeverityFromPowerOutage(1000, true)),
		severityRank(SeverityFromPowerOutage(50, false)))

	// An UNKNOWN count (PG&E reporting no EST_CUSTOMERS; JSON null unmarshals
	// to 0) must not land at the bottom of the scale. INFO is no longer merely
	// "low priority" — the place summary drops INFO-severity power events from
	// its rollup — so rating an unsized outage INFO would make a possibly
	// community-scale one vanish from the summary while its own headline says
	// the count is unknown. Unknown is not evidence of small.
	for _, planned := range []bool{false, true} {
		for _, n := range []int32{0, -1} {
			assert.Equal(t, SevMinor, SeverityFromPowerOutage(n, planned),
				"unknown count (n=%d, planned=%v) must floor at MINOR, not INFO", n, planned)
		}
	}
}

// TestSeverityFromPSPSStage: the PSPS layer is ACTIVE-ONLY — a shutoff that is
// over leaves the feed — so any row present is live, and an unrecognized stage
// must classify conservatively rather than dropping to INFO. Same life-safety
// bias as the evacuation WARNING default.
func TestSeverityFromPSPSStage(t *testing.T) {
	assert.Equal(t, SevSevere, SeverityFromPSPSStage("Warning"))
	assert.Equal(t, SevSevere, SeverityFromPSPSStage("  warning "))
	assert.Equal(t, SevModerate, SeverityFromPSPSStage("Watch"))
	assert.Equal(t, SevModerate, SeverityFromPSPSStage("WATCH"))

	assert.True(t, PSPSStageRecognized("Warning"))
	assert.True(t, PSPSStageRecognized("watch"))
	for _, unknown := range []string{"", "Imminent De-energization", "Stage 3"} {
		assert.False(t, PSPSStageRecognized(unknown), unknown)
		assert.Equal(t, SevSevere, SeverityFromPSPSStage(unknown),
			"an unrecognized stage on an active-only layer must not read as INFO: %q", unknown)
	}
}

func TestDemoteSeverity(t *testing.T) {
	assert.Equal(t, SevSevere, demoteSeverity(SevExtreme))
	assert.Equal(t, SevModerate, demoteSeverity(SevSevere))
	assert.Equal(t, SevMinor, demoteSeverity(SevModerate))
	assert.Equal(t, SevInfo, demoteSeverity(SevMinor))
	assert.Equal(t, SevInfo, demoteSeverity(SevInfo), "INFO is the floor")
}
