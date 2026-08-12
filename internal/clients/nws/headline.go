package nws

import (
	"regexp"
	"strings"
)

// ShortHeadline is the alert's card line: "<Event> — <reason>".
//
// WHY THIS EXISTS. CAP `properties.headline` — what we used to ship verbatim —
// is machine boilerplate with zero unique information:
//
//	Fire Weather Watch issued August 11 at 9:57AM PDT until August 13 at 9:00PM PDT by NWS Sacramento CA
//
// Every token in that string is already a structured field on the same event:
// the product name is `category`, the two stamps are `effective`/`expires`, and
// the office is `provenance.sourceName`. So a hundred characters of headline
// said nothing the record did not already say, four times over (the event card,
// `conditions.fireWeather.headline`, and both the weather and fire domain
// lists on a place summary).
//
// Meanwhile the one thing a reader wants — WHY the watch was issued — is
// published in `parameters.NWSheadline` and we were not even parsing it:
//
//	FIRE WEATHER WATCH IN EFFECT FROM LATE WEDNESDAY NIGHT THROUGH THURSDAY
//	EVENING FOR THUNDERSTORMS AND STRONG OUTFLOW WINDS FOR FIRE ZONES 130,132,…
//
// This lifts the reason clause out of that and pairs it with the product name:
//
//	Fire Weather Watch — thunderstorms and strong outflow winds
//
// DETERMINISTIC, DELIBERATELY. This is not a job for the model, for two
// reasons. `store.ContentHash` zeroes `Enhancement` and `Summary` but not
// `Headline`, so an AI-written headline would differ from the normalizer's on
// every 5-minute tick — an unbounded re-spend against a 5-call budget, and a
// spurious revision each time the wording drifted. And because the product
// name is copied verbatim from `Event`, a model can never turn a Watch into a
// Warning.
//
// Nothing is lost: the CAP headline is exactly reconstructable from the fields
// named above, the product text stays verbatim in `Description`, and any
// directive stays verbatim in `Instruction`.
func (a Alert) ShortHeadline() string {
	event := strings.TrimSpace(a.Event)
	reason := headlineReason(a.NWSHeadline, event)
	switch {
	case event != "" && reason != "":
		return event + " — " + reason
	case event != "":
		return event
	}
	// No product name at all: the boilerplate is still better than nothing.
	return strings.TrimSpace(a.Headline)
}

// Timing clauses that follow the product name. Everything up to the first
// " FOR " after one of these is when, not why — and when is `effective`.
var timingPrefixes = []string{
	"REMAINS IN EFFECT", "NOW IN EFFECT", "IS IN EFFECT", "IN EFFECT",
	"HAS BEEN EXTENDED", "HAS BEEN CANCELLED", "HAS BEEN CANCELED",
	"HAS EXPIRED", "IS CANCELLED", "IS CANCELED", "CANCELLED", "CANCELED",
	"EXTENDED",
}

// The trailing zone roll-call: "… FOR FIRE ZONES 130,132,133". The zones are
// `zones` on the record and mean nothing to a reader.
var zoneTail = regexp.MustCompile(`(?i)\s+(FOR|IN)\s+((THE)\s+)?((FIRE|FORECAST|COASTAL|MARINE)\s+)?ZONES?\b.*$`)

// A reason longer than this is a paragraph, not a headline; fall back to the
// bare product name rather than shipping a run-on.
const maxReasonLen = 96

// headlineReason extracts the "why" clause from an NWSheadline, or "".
func headlineReason(nwsHeadline, event string) string {
	rest := strings.TrimSpace(strings.ToUpper(nwsHeadline))
	if rest == "" {
		return ""
	}
	rest = strings.TrimPrefix(rest, "..." /* some products bracket with ellipses */)
	rest = strings.TrimSuffix(rest, "...")
	rest = strings.TrimSpace(rest)

	// Drop the product name; it becomes the headline's first half.
	if e := strings.ToUpper(strings.TrimSpace(event)); e != "" && strings.HasPrefix(rest, e) {
		rest = strings.TrimSpace(rest[len(e):])
	}

	for _, p := range timingPrefixes {
		if !strings.HasPrefix(rest, p) {
			continue
		}
		// "… IN EFFECT FROM X THROUGH Y FOR <reason>" — the reason, if there is
		// one, starts after the first " FOR ". No " FOR " means the product
		// said only when, so there is no reason to state.
		i := strings.Index(rest, " FOR ")
		if i == -1 {
			return ""
		}
		rest = strings.TrimSpace(rest[i+len(" FOR "):])
		break
	}
	// "<EVENT> FOR <reason>" with no timing clause at all.
	rest = strings.TrimPrefix(rest, "FOR ")
	rest = strings.TrimSpace(zoneTail.ReplaceAllString(rest, ""))
	rest = strings.Trim(rest, " .,;")

	if rest == "" || len(rest) > maxReasonLen {
		return ""
	}
	return unshout(rest)
}

// Tokens that are genuinely upper case and must survive: units, times and
// anything carrying a digit (I-5, 5000, PM2.5). Everything else in an
// ALL-CAPS product line is ordinary prose that was shouted by the wire format,
// not by intent.
var keepUpper = map[string]bool{
	"MPH": true, "KT": true, "KTS": true, "RH": true, "AQI": true, "UV": true,
	"AM": true, "PM": true, "PST": true, "PDT": true, "MST": true, "MDT": true,
	"UTC": true, "NWS": true, "CWA": true, "F": true, "C": true,
}

// unshout lower-cases an ALL-CAPS clause, keeping units and anything numeric.
//
// It does NOT sentence-case the result: the clause is a continuation after an
// em dash ("Fire Weather Watch — thunderstorms and strong outflow winds"), so
// a capital there would read as the start of a new sentence.
func unshout(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		bare := strings.Trim(f, ".,;:()")
		if keepUpper[bare] || strings.ContainsAny(f, "0123456789") {
			continue
		}
		fields[i] = strings.ToLower(f)
	}
	return strings.Join(fields, " ")
}
