package related

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
)

// Rules is the registry. Order does not matter: a pair matched by several
// rules is reported under its strongest. Thresholds come from the survey of
// July-November 2026 (notes/grid/2026-11-10-related-events-survey.md in the
// workspace): evacuation zones sat within ~8 km of their fire and were issued
// within a day of its start.
func Rules() []Rule {
	return []Rule{
		{
			// Cal OES notes often name the incident: "Ross Fire - Fire is
			// approximately 19.9 acres near Canyon Ct".
			Name:   "fire-evacuation-named",
			A:      Side{Layer: gridv1.Layer_WILDFIRE},
			B:      Side{Layer: gridv1.Layer_EVACUATION},
			Before: 24 * time.Hour, After: 7 * 24 * time.Hour, MaxKm: 50,
			Judge: func(p Pair) (Match, bool) {
				name := fireName(p.A)
				if name == "" || !namesFire(p.B.GetDescription()+" "+p.B.GetHeadline(), name) {
					return Match{}, false
				}
				return Match{Confidence: High, Reason: fmt.Sprintf("Cal OES notes name the %s Fire", name), Score: p.GapKm}, true
			},
		},
		{
			// Most zones carry no notes, so place and timing: issued from an
			// hour before to three days after the fire started, within 8 km,
			// and only for the nearest such fire.
			Name:   "fire-evacuation-near",
			A:      Side{Layer: gridv1.Layer_WILDFIRE},
			B:      Side{Layer: gridv1.Layer_EVACUATION},
			Before: time.Hour, After: 0, MaxKm: 8, ExclusiveB: true,
			Judge: func(p Pair) (Match, bool) {
				if p.Lag > 72*time.Hour {
					return Match{}, false
				}
				return Match{Confidence: Medium, Score: p.GapKm,
					Reason: fmt.Sprintf("%s from the fire, issued %s", km(p.GapKm), lagText(p.Lag, "the fire started"))}, true
			},
		},
		{
			// PG&E sometimes states the cause.
			Name:   "fire-outage-cause",
			A:      Side{Layer: gridv1.Layer_WILDFIRE},
			B:      Side{Layer: gridv1.Layer_POWER, Keep: isOutage},
			Before: time.Hour, After: 48 * time.Hour, MaxKm: 10,
			Judge: func(p Pair) (Match, bool) {
				if !strings.Contains(strings.ToUpper(p.B.GetPower().GetCause()), "FIRE") {
					return Match{}, false
				}
				return Match{Confidence: Medium, Score: p.GapKm,
					Reason: fmt.Sprintf("PG&E lists the cause as fire; %s from the fire", km(p.GapKm))}, true
			},
		},
		{
			// A shutoff near a burning fire. PG&E de-energizes for fire risk,
			// but does not say which fire, so this claims only coincidence.
			Name:   "fire-shutoff-near",
			A:      Side{Layer: gridv1.Layer_WILDFIRE},
			B:      Side{Layer: gridv1.Layer_POWER, Keep: isShutoff},
			Before: 24 * time.Hour, After: 0, MaxKm: 25,
			Judge: func(p Pair) (Match, bool) {
				return Match{Confidence: Low, Score: p.GapKm,
					Reason: fmt.Sprintf("PG&E shutoff %s from the fire while it burned", km(p.GapKm))}, true
			},
		},
		{
			// A road report that names the fire.
			Name:   "fire-road-named",
			A:      Side{Layer: gridv1.Layer_WILDFIRE},
			B:      Side{Layer: gridv1.Layer_ROAD_INCIDENT},
			Before: 6 * time.Hour, After: 3 * 24 * time.Hour, MaxKm: 30,
			Judge: func(p Pair) (Match, bool) {
				name := fireName(p.A)
				text := p.B.GetHeadline() + " " + p.B.GetSummary() + " " + p.B.GetDescription()
				if name == "" || !namesFire(text, name) {
					return Match{}, false
				}
				return Match{Confidence: High, Score: p.GapKm, Reason: fmt.Sprintf("The road report names the %s Fire", name)}, true
			},
		},
		{
			// Shutoffs under fire-weather and wind products: PG&E's PSPS
			// criteria are wind and dryness, so this is the usual pairing,
			// still stated as "during".
			Name:   "weather-shutoff",
			A:      Side{Layer: gridv1.Layer_WEATHER_ALERT, Keep: alertIn(fireWeatherAlerts, windAlerts)},
			B:      Side{Layer: gridv1.Layer_POWER, Keep: isShutoff},
			Before: 12 * time.Hour, After: 0, ShapeA: true,
			Judge: insideAlert(Low, "PG&E shutoff during the %s, inside its area"),
		},
		{
			// Unplanned outages inside a wind or winter storm product's area
			// while it is in effect. Context only: outages happen every day.
			Name:   "weather-outage",
			A:      Side{Layer: gridv1.Layer_WEATHER_ALERT, Keep: alertIn(windAlerts, winterAlerts, stormAlerts)},
			B:      Side{Layer: gridv1.Layer_POWER, Keep: isUnplannedOutage},
			Before: 0, After: 0, ShapeA: true,
			Judge: insideAlert(Context, "Outage during the %s, inside its area"),
		},
		{
			// Road closures and incidents inside a winter storm or wind
			// product's area while it is in effect.
			Name:   "weather-road",
			A:      Side{Layer: gridv1.Layer_WEATHER_ALERT, Keep: alertIn(winterAlerts, windAlerts, stormAlerts)},
			B:      Side{Layer: gridv1.Layer_ROAD_INCIDENT, Keep: notPlannedWork},
			Before: 0, After: 0, ShapeA: true,
			Judge: insideAlert(Context, "Road report during the %s, inside its area"),
		},
	}
}

// --- predicates -------------------------------------------------------------

func isShutoff(ev *gridv1.Event) bool { return ev.GetCategory() == "psps" }

func isOutage(ev *gridv1.Event) bool {
	return ev.GetCategory() == "unplanned" || ev.GetCategory() == "planned"
}

func isUnplannedOutage(ev *gridv1.Event) bool { return ev.GetCategory() == "unplanned" }

// notPlannedWork drops scheduled Caltrans work windows: a paving job is not
// about the storm.
func notPlannedWork(ev *gridv1.Event) bool { return ev.GetRoadIncident().GetClosure() == nil }

var (
	fireWeatherAlerts = []string{"red flag warning", "fire weather watch"}
	windAlerts        = []string{"high wind", "wind advisory", "extreme wind"}
	winterAlerts      = []string{"winter storm", "winter weather", "blizzard", "ice storm", "heavy snow"}
	stormAlerts       = []string{"severe thunderstorm", "flood"}
)

// alertIn keeps weather alerts whose NWS event name (category) contains any
// of the phrases.
func alertIn(groups ...[]string) func(*gridv1.Event) bool {
	return func(ev *gridv1.Event) bool {
		c := strings.ToLower(ev.GetCategory())
		for _, g := range groups {
			for _, phrase := range g {
				if strings.Contains(c, phrase) {
					return true
				}
			}
		}
		return false
	}
}

// insideAlert matches a B whose centroid lies inside the alert's area (its
// NWS zones). An alert with no shape matches nothing: zone codes alone
// cannot place a point.
func insideAlert(conf Confidence, reason string) func(Pair) (Match, bool) {
	return func(p Pair) (Match, bool) {
		c := centroidOf(p.B)
		if p.AShape == nil || c == nil || !geojson.PointInGeometry(c.GetLat(), c.GetLng(), p.AShape) {
			return Match{}, false
		}
		return Match{Confidence: conf, Reason: fmt.Sprintf(reason, p.A.GetCategory())}, true
	}
}

// --- text helpers -------------------------------------------------------------

// fireName is a fire's name as people write it, without "Fire": "Gann Fire —
// 10,339 ac" and "GANN — 6495 ac" both give "Gann".
func fireName(ev *gridv1.Event) string {
	name, _, _ := strings.Cut(ev.GetHeadline(), " — ")
	name = strings.TrimSpace(name)
	if l := strings.ToLower(name); strings.HasSuffix(l, " fire") {
		name = strings.TrimSpace(name[:len(name)-len(" fire")])
	}
	if len(name) < 3 {
		return ""
	}
	return strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
}

// namesFire reports whether text names "<name> Fire" (word-bounded,
// case-insensitive). The word "Fire" is required: a zone note mentioning a
// street called Ross is not the Ross Fire.
func namesFire(text, name string) bool {
	re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(name) + `\s+fire\b`)
	return err == nil && re.MatchString(text)
}

func km(v float64) string {
	if v < 0.1 {
		return "at the same place"
	}
	return fmt.Sprintf("%.1f km", v)
}

func lagText(d time.Duration, ref string) string {
	switch {
	case d < -time.Minute:
		return fmt.Sprintf("%s before %s", roughDuration(-d), ref)
	case d < time.Minute:
		return "as " + ref
	}
	return fmt.Sprintf("%s after %s", roughDuration(d), ref)
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
