package ingest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dpup/sierra-data/internal/clients/nws"
	"github.com/dpup/sierra-data/internal/config"
)

// TestNWSEnhancerLive runs the real prompt against the real model on a real
// product and prints the result. It is skipped unless NWS_ENHANCE_LIVE=1 and an
// OpenAI key is present, so `make test` never spends money or needs the network.
//
//	NWS_ENHANCE_LIVE=1 go test ./internal/ingest/ -run Live -v
//
// It exists because prompt policy cannot be verified by unit test: the rules
// added for length, zone rosters and timestamp repetition are claims about what
// the model does, and the only way to check a claim about a model is to ask it.
// The fixture below is a verbatim NWS Fire Weather Watch (product
// urn:oid:…84f77c49…001.1, fetched from api.weather.gov) — the exact alert whose
// 865-character summary prompted the rewrite.
func TestNWSEnhancerLive(t *testing.T) {
	if os.Getenv("NWS_ENHANCE_LIVE") != "1" {
		t.Skip("set NWS_ENHANCE_LIVE=1 to run the live enhancer check")
	}
	key := os.Getenv("PF__OPENAI__API_KEY")
	if key == "" {
		t.Skip("PF__OPENAI__API_KEY not set")
	}
	model := os.Getenv("PF__OPENAI__MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}

	alert := nws.Alert{
		Event: "Fire Weather Watch",
		NWSHeadline: "FIRE WEATHER WATCH IN EFFECT FROM LATE WEDNESDAY NIGHT THROUGH THURSDAY " +
			"EVENING FOR THUNDERSTORMS AND STRONG OUTFLOW WINDS FOR FIRE ZONES " +
			"130,132,133,135,136,138 AND 139",
		Headline: "Fire Weather Watch issued August 11 at 9:57AM PDT until August 13 at " +
			"9:00PM PDT by NWS Sacramento CA",
		Description: `The National Weather Service in Sacramento has issued a Fire
Weather Watch for thunderstorms and strong outflow winds, which
is in effect from late Wednesday night through Thursday evening.

* Affected Area...Fire Zone 130 Sierra (Tehama-Plumas) Above
3000 ft, Fire Zone 132 Sierra (Yuba-Placer) 3000-5000 ft, Fire
Zone 133 Sierra (Sierra-Placer) Above 5000 ft, Fire Zone 135
Sierra 3000-5000 ft, Fire Zone 136 Sierra (El Dorado-Amador)
Above 5000 ft, Fire Zone 138 Sierra (Cal-Tuo) 3000-5000 ft and
Fire Zone 139 Sierra (Cal-Tuo) Above 5000 ft.

* Thunderstorms...Isolated to scattered coverage of a mix of wet
and dry thunderstorms expected. Lightning strikes may also occur
outside of main precipitation cores.

* Outflow Winds...Gusty and erratic outflow winds could occur near
any thunderstorm development.

* Impacts...Lightning can create new fire starts and may combine
with strong outflow winds to cause a fire to rapidly grow in
size and intensity.`,
		SenderName: "NWS Sacramento CA",
	}

	enh := NewNWSEnhancer(config.OpenAIClient{APIKey: key, Model: model, Timeout: 60 * time.Second})
	if enh == nil {
		t.Fatal("enhancer is nil with a key set")
	}

	out, err := enh.Enhance(context.Background(), alert.ShortHeadline(), alert.Description,
		[]string{"Ebbetts Pass Corridor"})
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}

	t.Logf("headline (%d chars): %s", len(alert.ShortHeadline()), alert.ShortHeadline())
	t.Logf("summary  (%d chars): %s", len(out.Summary), out.Summary)

	// The policy the rewrite added, checked against what the model actually did.
	if n := len(out.Summary); n > 400 {
		t.Errorf("summary is %d chars; policy says at most 320 (400 allowed as slack)", n)
	}
	for _, banned := range []string{"Fire Zone", "3000 ft", "5000 ft", "PDT", "Sacramento",
		// Policies 3, 6 and 10: no pointer to the instructions, no day-relative
		// timing, no exclamation.
		"instruction", "consult", "today", "tonight", "tomorrow", "!"} {
		if strings.Contains(out.Summary, banned) {
			t.Errorf("summary contains %q, which the policy excludes: %s", banned, out.Summary)
		}
	}
	// Policy 7: the headline is right above it, so opening with the product's
	// name spends the first four words saying nothing.
	if strings.HasPrefix(out.Summary, "Fire Weather Watch") {
		t.Errorf("summary opens by restating the headline: %s", out.Summary)
	}
	// Policy 5: the supplied place is where the reader is. Describing the area
	// generically when a name was given wastes the one piece of local grounding
	// the model is allowed.
	if !strings.Contains(out.Summary, "Ebbetts Pass") {
		t.Errorf("summary does not name the supplied place: %s", out.Summary)
	}
}

// TestNWSEnhancerLiveRelativeTiming runs the verbatim Red Flag Warning
// (urn:oid:…f4cb664e…001.1, fetched from api.weather.gov 2026-10-14) whose
// summary prompted policies 3, 6, 9 and 10: it said "from midnight tonight",
// wrote "RH", and closed with "The alert includes instructions—consult the
// original". Its only timing is day-relative, so policy 6 says leave it out.
func TestNWSEnhancerLiveRelativeTiming(t *testing.T) {
	if os.Getenv("NWS_ENHANCE_LIVE") != "1" {
		t.Skip("set NWS_ENHANCE_LIVE=1 to run the live enhancer check")
	}
	key := os.Getenv("PF__OPENAI__API_KEY")
	if key == "" {
		t.Skip("PF__OPENAI__API_KEY not set")
	}
	model := os.Getenv("PF__OPENAI__MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}

	alert := nws.Alert{
		Event:       "Red Flag Warning",
		NWSHeadline: "RED FLAG WARNING IN EFFECT FROM MIDNIGHT TONIGHT TO 10 AM PDT FRIDAY",
		Description: `...RED FLAG WARNING IN EFFECT FROM MIDNIGHT TONIGHT TO 10 AM PDT
FRIDAY DUE TO WIND AND LOW RELATIVE HUMIDITY FOR PORTIONS OF CENTRAL
AND NORTHERN CALIFORNIA...

The National Weather Service in Sacramento CA has issued a Red Flag
Warning.

* AFFECTED AREA...Portions of the Sierra Nevada and adjacent
Foothills.

* TIMING...From midnight tonight to 10 AM PDT Friday.

* WINDS...Northeast to east winds 15 to 25 mph with gusts 25 to 45
mph.

* RELATIVE HUMIDITY...Daytime humidity 10 to 25% with single digits
possible and very poor overnight recoveries.

* IMPACTS...Any fires that develop will spread rapidly. Outdoor
burning is not recommended.`,
		SenderName: "NWS Sacramento CA",
	}

	enh := NewNWSEnhancer(config.OpenAIClient{APIKey: key, Model: model, Timeout: 60 * time.Second})
	out, err := enh.Enhance(context.Background(), alert.ShortHeadline(), alert.Description,
		[]string{"Ebbetts Pass Corridor"})
	if err != nil {
		t.Fatalf("enhance: %v", err)
	}
	t.Logf("summary  (%d chars): %s", len(out.Summary), out.Summary)

	lower := strings.ToLower(out.Summary)
	for _, banned := range []string{"tonight", "today", "tomorrow", "midnight", "instruction", "consult", "original", " rh", "!"} {
		if strings.Contains(lower, banned) {
			t.Errorf("summary contains %q, which the policy excludes: %s", banned, out.Summary)
		}
	}
	// Policy 9: the alert's own qualifier on the single-digit humidity.
	if strings.Contains(lower, "single digit") && !strings.Contains(lower, "possible") {
		t.Errorf("summary dropped the alert's \"possible\" qualifier: %s", out.Summary)
	}
}
