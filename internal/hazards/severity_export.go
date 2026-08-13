package hazards

import (
	"strings"

	api "github.com/dpup/sierra-data/api/v1"
)

// Exported wrappers over this package's pure severity/normalization helpers,
// for internal/ingest (docs/v2-implementation-plan.md Tier C). The ingest
// normalizers must reproduce the shipped envelope semantics exactly, so they
// delegate here rather than duplicating the mappings.

// SeverityFromMagnitude maps an earthquake magnitude onto the unified scale.
func SeverityFromMagnitude(m float64) string { return fromMagnitude(m) }

// SeverityFromWildfire maps a fire's size + containment onto the unified scale.
func SeverityFromWildfire(acres float64, percentContained int32) string {
	return fromWildfire(acres, percentContained)
}

// SeverityFromEvacLevel maps a coded evacuation level onto the unified scale.
func SeverityFromEvacLevel(level string) string { return fromEvacLevel(level) }

// SeverityFromAlertSeverity maps the shared api.AlertSeverity enum onto the
// unified scale.
func SeverityFromAlertSeverity(s api.AlertSeverity) string { return fromAlertSeverity(s) }

// SeverityFromNWSSeverity maps the raw NWS severity vocabulary
// (Extreme|Severe|Moderate|Minor|Unknown) onto the unified scale. Unlike the
// api.AlertSeverity path — which collapses NWS "Extreme" into CRITICAL and so
// into SEVERE — this direct mapping keeps "Extreme" as EXTREME, preserving the
// top of the NWS scale.
func SeverityFromNWSSeverity(s string) string { return fromNWSSeverity(s) }

// SeverityFromRoadImpact maps the AI-assessed traffic impact
// (none|light|moderate|severe) onto the unified scale, bypassing the
// api.AlertSeverity round-trip that collapses light and moderate together and
// caps the scale at SEVERE. Returns "" for an unknown or absent impact so the
// caller falls back to the enum path rather than under-rating the incident.
func SeverityFromRoadImpact(impact string) string { return fromRoadImpact(impact) }

// NormalizeEvacLevel maps Cal OES free-text STATUS to a coded level ("" only
// for explicitly-inactive statuses; unrecognized active statuses default to a
// conservative WARNING — see normalizeEvacLevel).
func NormalizeEvacLevel(status string) string { return normalizeEvacLevel(status) }

// EvacStatusRecognized reports whether a (non-inactive) Cal OES STATUS matched
// a known keyword; callers log unrecognized phrasings that fell through to the
// conservative WARNING default.
func EvacStatusRecognized(status string) bool { return evacStatusRecognized(status) }

// evacInactiveKeywords are the phrasings that mark a Cal OES zone explicitly
// inactive. MUST stay in sync with normalizeEvacLevel's inactive branch —
// TestEvacStatusInactiveMatchesNormalize cross-checks both against the same
// table.
var evacInactiveKeywords = []string{"lifted", "normal", "all clear", "all-clear", "repopulat", "no evac"}

// EvacStatusInactive reports whether a Cal OES STATUS explicitly marks a zone
// inactive (lifted / back to normal / all clear / repopulation / no
// evacuation). It deliberately does NOT treat a blank/whitespace STATUS as
// inactive: normalizeEvacLevel maps blank to "" alongside the explicit
// keywords, but a present row in the active-events-only aggregation layer
// whose STATUS is merely blank is missing data, not an all-clear — the
// life-safety consumer (ingest) must keep such a zone active with a
// conservative default rather than drop it (an error never becomes a 0).
func EvacStatusInactive(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return false
	}
	for _, kw := range evacInactiveKeywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// SeverityFromPowerOutage maps an electric outage onto the unified scale from
// the customers affected, demoting a planned (pre-notified) shutdown one rank.
func SeverityFromPowerOutage(customersAffected int32, planned bool) string {
	return fromPowerOutage(customersAffected, planned)
}

// SeverityFromPSPSStage maps a PSPS coverage stage (Watch|Warning) onto the
// unified scale. An unrecognized stage classifies conservatively as SEVERE.
func SeverityFromPSPSStage(stage string) string { return fromPSPSStage(stage) }

// PSPSStageRecognized reports whether a PSPS stage matched a known value;
// callers log the ones that fell through to the conservative SEVERE default so
// the phrasing can be classified explicitly.
func PSPSStageRecognized(stage string) bool { return pspsStageRecognized(stage) }

// NormFireName normalizes an incident/perimeter name for joining CAL FIRE
// incidents and FIRIS perimeters (e.g. "Salt Springs Fire" and "Salt Springs" →
// "saltsprings").
func NormFireName(s string) string { return normFireName(s) }
