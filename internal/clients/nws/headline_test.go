package nws

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Cases are real NWSheadline strings captured from api.weather.gov, plus the
// shapes that have to degrade safely.
func TestShortHeadline(t *testing.T) {
	tests := []struct {
		name  string
		alert Alert
		want  string
	}{
		{
			name: "reason between the timing clause and the zone roll-call",
			alert: Alert{
				Event: "Fire Weather Watch",
				NWSHeadline: "FIRE WEATHER WATCH IN EFFECT FROM LATE WEDNESDAY NIGHT THROUGH " +
					"THURSDAY EVENING FOR THUNDERSTORMS AND STRONG OUTFLOW WINDS FOR FIRE " +
					"ZONES 130,132,133,135,136,138 AND 139",
				Headline: "Fire Weather Watch issued August 11 at 9:57AM PDT until August 13 at 9:00PM PDT by NWS Sacramento CA",
			},
			want: "Fire Weather Watch — thunderstorms and strong outflow winds",
		},
		{
			name: "a reason that itself contains FOR",
			alert: Alert{
				Event: "Fire Weather Watch",
				NWSHeadline: "FIRE WEATHER WATCH IN EFFECT FROM FRIDAY MORNING THROUGH FRIDAY " +
					"EVENING FOR THE POTENTIAL FOR NEW FIRE STARTS DUE TO LIGHTNING",
			},
			want: "Fire Weather Watch — the potential for new fire starts due to lightning",
		},
		{
			name: "DUE TO reads as the reason with no timing clause to strip",
			alert: Alert{
				Event:       "Air Quality Alert",
				NWSHeadline: "AIR QUALITY ALERT DUE TO HARMFUL PARTICLE POLLUTION LEVELS FROM WINDBLOWN DUST",
			},
			want: "Air Quality Alert — due to harmful particle pollution levels from windblown dust",
		},
		{
			// Timing only. The product said when, not why, and when is already
			// effective/expires — so the headline is the product name alone.
			name: "no reason clause",
			alert: Alert{
				Event:       "Flood Watch",
				NWSHeadline: "FLOOD WATCH IN EFFECT FROM WEDNESDAY AFTERNOON THROUGH THURSDAY EVENING",
			},
			want: "Flood Watch",
		},
		{
			name: "units and numbers keep their case",
			alert: Alert{
				Event:       "Wind Advisory",
				NWSHeadline: "WIND ADVISORY IN EFFECT UNTIL 11 PM PDT FOR SOUTHWEST WINDS 25 TO 35 MPH",
			},
			want: "Wind Advisory — southwest winds 25 to 35 MPH",
		},
		{
			name: "REMAINS IN EFFECT is a timing clause too",
			alert: Alert{
				Event:       "Red Flag Warning",
				NWSHeadline: "RED FLAG WARNING REMAINS IN EFFECT UNTIL 8 PM PDT FOR GUSTY WINDS AND LOW HUMIDITY",
			},
			want: "Red Flag Warning — gusty winds and low humidity",
		},
		{
			name: "some products bracket the line in ellipses",
			alert: Alert{
				Event:       "Winter Weather Advisory",
				NWSHeadline: "...WINTER WEATHER ADVISORY IN EFFECT UNTIL 10 AM PST FOR HEAVY MOUNTAIN SNOW...",
			},
			want: "Winter Weather Advisory — heavy mountain snow",
		},
		{
			// Most products publish no NWSheadline at all. The product name is
			// still a correct, if terser, headline — and still beats the CAP
			// boilerplate it replaces.
			name: "no NWSheadline falls back to the product name",
			alert: Alert{
				Event:    "Winter Storm Watch",
				Headline: "Winter Storm Watch issued March 2 at 3:04AM PST until March 3 at 4:00PM PST by NWS Sacramento CA",
			},
			want: "Winter Storm Watch",
		},
		{
			name: "no product name at all keeps the boilerplate rather than nothing",
			alert: Alert{
				Headline: "Special Weather Statement issued August 11 by NWS Sacramento CA",
			},
			want: "Special Weather Statement issued August 11 by NWS Sacramento CA",
		},
		{
			name:  "empty alert yields empty headline",
			alert: Alert{},
			want:  "",
		},
		{
			// A run-on is a paragraph, not a headline. Better the bare product
			// name than a hundred-character card line.
			name: "an over-long reason is dropped",
			alert: Alert{
				Event: "Flood Warning",
				NWSHeadline: "FLOOD WARNING IN EFFECT UNTIL NOON FOR MINOR FLOODING OF LOW LYING AREAS " +
					"ROADWAYS UNDERPASSES AND POOR DRAINAGE LOCATIONS ACROSS THE ENTIRE VALLEY FLOOR",
			},
			want: "Flood Warning",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.alert.ShortHeadline())
		})
	}
}

// The headline must never contain what the record already carries elsewhere —
// that redundancy is the whole reason this function exists.
func TestShortHeadlineDropsBoilerplate(t *testing.T) {
	a := Alert{
		Event: "Fire Weather Watch",
		NWSHeadline: "FIRE WEATHER WATCH IN EFFECT FROM LATE WEDNESDAY NIGHT THROUGH THURSDAY " +
			"EVENING FOR THUNDERSTORMS AND STRONG OUTFLOW WINDS FOR FIRE ZONES 130,132",
		Headline:   "Fire Weather Watch issued August 11 at 9:57AM PDT until August 13 at 9:00PM PDT by NWS Sacramento CA",
		SenderName: "NWS Sacramento CA",
	}
	got := a.ShortHeadline()
	assert.NotContains(t, got, "issued", "issuance time is `effective`")
	assert.NotContains(t, got, "PDT", "timestamps are `effective`/`expires`")
	assert.NotContains(t, got, "Sacramento", "the office is `provenance.sourceName`")
	assert.NotContains(t, got, "ZONE", "zone codes are `zones`")
	assert.NotContains(t, got, "130", "zone codes are `zones`")
	assert.Less(t, len(got), len(a.Headline), "the point is that it is shorter")
}

// The four CAP times are two pairs, and conflating them is what made a live
// Fire Weather Watch advertise an end time a day and a half before the storms
// it warned about. Values are verbatim from product
// urn:oid:…84f77c49…001.1 (api.weather.gov, 2026-08-11).
func TestHazardWindowPrefersOnsetAndEnds(t *testing.T) {
	issued := time.Date(2026, 8, 11, 9, 57, 0, 0, time.UTC)
	reissueBy := time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC)
	stormsStart := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)
	stormsEnd := time.Date(2026, 8, 13, 21, 0, 0, 0, time.UTC)

	a := Alert{Effective: issued, Expires: reissueBy, Onset: stormsStart, Ends: stormsEnd}
	assert.Equal(t, stormsStart, a.Begins(), "the hazard starts at onset, not at issuance")
	assert.Equal(t, stormsEnd, a.EndsAt(), "expires is the re-issue deadline, not the hazard end")
	assert.True(t, a.EndsAt().After(a.Expires),
		"this is the whole bug: the product lapses before the hazard does")

	// Most products publish neither, and must be unaffected.
	bare := Alert{Effective: issued, Expires: reissueBy}
	assert.Equal(t, issued, bare.Begins())
	assert.Equal(t, reissueBy, bare.EndsAt())

	// One without the other still works.
	onsetOnly := Alert{Effective: issued, Expires: reissueBy, Onset: stormsStart}
	assert.Equal(t, stormsStart, onsetOnly.Begins())
	assert.Equal(t, reissueBy, onsetOnly.EndsAt())
}
