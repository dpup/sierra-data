package hazards

import (
	"strings"

	api "github.com/dpup/sierra-data/api/v1"
)

// The unified severity scale (docs/hazard-aggregation-design.md §4.2). It
// expresses response urgency to the public, not physical magnitude — an
// editorial prioritization shared across all sources so a client can sort
// "most urgent first" and color a map without source-specific logic.
const (
	SevInfo     = "INFO"
	SevMinor    = "MINOR"
	SevModerate = "MODERATE"
	SevSevere   = "SEVERE"
	SevExtreme  = "EXTREME"
)

// severityRank maps a unified severity to its 0..4 rank (for sort/color).
func severityRank(s string) int {
	switch s {
	case SevExtreme:
		return 4
	case SevSevere:
		return 3
	case SevModerate:
		return 2
	case SevMinor:
		return 1
	default:
		return 0
	}
}

// fromAlertSeverity maps the shared api.AlertSeverity enum onto the unified
// scale (road incidents reuse it). Every enum value maps, incl. UNSPECIFIED.
func fromAlertSeverity(s api.AlertSeverity) string {
	switch s {
	case api.AlertSeverity_CRITICAL:
		return SevSevere
	case api.AlertSeverity_WARNING:
		return SevModerate
	case api.AlertSeverity_INFO:
		return SevMinor
	default: // ALERT_SEVERITY_UNSPECIFIED
		return SevInfo
	}
}

// fromNWSSeverity maps an NWS severity string onto the unified scale.
func fromNWSSeverity(s string) string {
	switch s {
	case "Extreme":
		return SevExtreme
	case "Severe":
		return SevSevere
	case "Moderate":
		return SevModerate
	case "Minor":
		return SevMinor
	default:
		return SevInfo
	}
}

// fromFireWeatherState maps a fire-weather state string ("normal"|"elevated"|
// "red-flag" or their UPPER enum names) onto the unified scale.
func fromFireWeatherState(state string) string {
	switch strings.ToLower(state) {
	case "red-flag", "red_flag":
		return SevSevere
	case "elevated":
		return SevModerate
	default:
		return SevInfo
	}
}

// normalizeEvacLevel maps Cal OES free-text STATUS to a coded level. Returns ""
// only for explicitly-inactive statuses (lifted/normal/all-clear) so the caller
// drops them.
//
// Life-safety bias: an unrecognized, non-inactive status must NOT be silently
// dropped — that would under-report active evacuations, the exact all-clear
// failure the fail-loud design forbids. So the default is a conservative active
// WARNING, and the evacuations builder logs the unrecognized phrasing so it can
// be classified explicitly. The inactive checks run first, so "Evacuation Order
// Lifted" resolves to "" (not ORDER).
func normalizeEvacLevel(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch {
	case s == "",
		strings.Contains(s, "lifted"),
		strings.Contains(s, "normal"),
		strings.Contains(s, "all clear"),
		strings.Contains(s, "all-clear"),
		strings.Contains(s, "repopulat"),
		strings.Contains(s, "no evac"):
		return ""
	case strings.Contains(s, "order"), strings.Contains(s, "mandatory"):
		return "ORDER"
	case strings.Contains(s, "shelter"):
		return "SHELTER_IN_PLACE"
	case strings.Contains(s, "warning"):
		return "WARNING"
	case strings.Contains(s, "advisory"), strings.Contains(s, "voluntary"):
		return "ADVISORY"
	default:
		return "WARNING"
	}
}

// evacStatusRecognized reports whether a (non-inactive) Cal OES STATUS matched a
// known keyword. The evacuations builder uses it to log unrecognized phrasings
// that fell through to the conservative WARNING default.
func evacStatusRecognized(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	for _, kw := range []string{"order", "mandatory", "shelter", "warning", "advisory", "voluntary"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// fromEvacLevel maps a coded evacuation level onto the unified scale.
func fromEvacLevel(level string) string {
	switch level {
	case "ORDER":
		return SevExtreme
	case "WARNING", "SHELTER_IN_PLACE":
		return SevSevere
	case "ADVISORY":
		return SevModerate
	default:
		return SevInfo
	}
}

// fromWildfire maps a fire onto the unified scale from its size AND containment,
// deliberately biased to OVER-estimate active threat rather than under-estimate
// it. It returns the worse (higher) of two heuristics, so a fire is never rated
// below either:
//
//   - containment (kept as a floor; CAL FIRE doesn't expose growth rate):
//     uncontained <50% (incl. unknown 0) → SEVERE, partly contained → MODERATE,
//     fully contained → MINOR.
//   - size, by NWCG fire size class (A ≤¼ac · B <10 · C <100 · D <300 · E <1,000
//     · F <5,000 · G ≥5,000): a large, still-active fire escalates — ≥1,000 ac
//     (class F/G) reads EXTREME while <50% contained, else SEVERE; ≥100 ac
//     (class D/E) reads SEVERE. Smaller or fully contained fires add no size
//     escalation (the containment floor governs).
//
// The old function looked at containment only, so it capped every fire at
// MODERATE once ≥50% contained and never returned EXTREME — a 5,000-ac fire at
// 55% read MODERATE. Now: a 5-ac new fire stays SEVERE (uncontained), Priest
// (9.5 ac, 60%) stays MODERATE, a 1,200-ac fire at 35% is EXTREME, a 5,000-ac
// fire at 55% is SEVERE.
func fromWildfire(acres float64, percentContained int32) string {
	cont := containmentSeverity(percentContained)
	size := sizeSeverity(acres, percentContained)
	if severityRank(size) > severityRank(cont) {
		return size
	}
	return cont
}

// containmentSeverity is the original containment-only heuristic, kept as a floor
// under fromWildfire (a fire is never rated below it).
func containmentSeverity(percentContained int32) string {
	switch {
	case percentContained >= 100:
		return SevMinor
	case percentContained < 50:
		return SevSevere
	default:
		return SevModerate
	}
}

// sizeSeverity escalates large, still-active fires by NWCG fire size class.
// Returns INFO (no escalation) for small (<100 ac) or fully contained fires — the
// containment floor governs those.
func sizeSeverity(acres float64, percentContained int32) string {
	if percentContained >= 100 {
		return SevInfo // contained — perimeter controlled, no size escalation
	}
	switch {
	case acres >= 1000: // NWCG class F/G
		if percentContained < 50 {
			return SevExtreme
		}
		return SevSevere
	case acres >= 100: // class D/E
		return SevSevere
	default: // class A–C — no size escalation
		return SevInfo
	}
}

// fromMagnitude maps an earthquake magnitude onto the unified scale.
func fromMagnitude(m float64) string {
	switch {
	case m >= 5:
		return SevSevere
	case m >= 4:
		return SevModerate
	case m >= 2.5:
		return SevMinor
	default:
		return SevInfo
	}
}

// fromChainLevelStr maps a Caltrans chain-control level string ("R1"|"R2"|"R3")
// onto the unified scale.
func fromChainLevelStr(level string) string {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "R3":
		return SevSevere
	case "R2":
		return SevModerate
	case "R1":
		return SevMinor
	default:
		return SevInfo
	}
}
