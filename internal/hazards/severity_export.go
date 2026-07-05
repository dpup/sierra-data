package hazards

import (
	api "github.com/dpup/info.ersn.net/server/api/v1"
)

// Exported wrappers over this package's pure severity/normalization helpers,
// for internal/ingest (docs/v2-implementation-plan.md Tier C). The ingest
// normalizers must reproduce the shipped envelope semantics exactly, so they
// delegate here rather than duplicating the mappings.

// SeverityFromMagnitude maps an earthquake magnitude onto the unified scale.
func SeverityFromMagnitude(m float64) string { return fromMagnitude(m) }

// SeverityFromWildfire maps a fire's containment onto the unified scale.
func SeverityFromWildfire(percentContained int32) string { return fromWildfire(percentContained) }

// SeverityFromEvacLevel maps a coded evacuation level onto the unified scale.
func SeverityFromEvacLevel(level string) string { return fromEvacLevel(level) }

// SeverityFromAlertSeverity maps the shared api.AlertSeverity enum onto the
// unified scale.
func SeverityFromAlertSeverity(s api.AlertSeverity) string { return fromAlertSeverity(s) }

// NormalizeEvacLevel maps Cal OES free-text STATUS to a coded level ("" only
// for explicitly-inactive statuses; unrecognized active statuses default to a
// conservative WARNING — see normalizeEvacLevel).
func NormalizeEvacLevel(status string) string { return normalizeEvacLevel(status) }

// EvacStatusRecognized reports whether a (non-inactive) Cal OES STATUS matched
// a known keyword; callers log unrecognized phrasings that fell through to the
// conservative WARNING default.
func EvacStatusRecognized(status string) bool { return evacStatusRecognized(status) }

// NormFireName normalizes an incident/perimeter name for joining CAL FIRE and
// WFIGS (e.g. "Salt Springs Fire" and "Salt Springs" → "saltsprings").
func NormFireName(s string) string { return normFireName(s) }
