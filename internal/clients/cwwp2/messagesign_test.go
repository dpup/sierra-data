package cwwp2

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmsFixtureNow is just after the 2026-10-01 CMS captures' record stamps
// (D10: 12:44:04 PDT).
var cmsFixtureNow = time.Date(2026, 10, 1, 19, 50, 0, 0, time.UTC)

func cmsClient(d HTTPDoer) *Client {
	c := NewClientWithHTTPDoer(d)
	c.Now = func() time.Time { return cmsFixtureNow }
	return c
}

func findSign(t *testing.T, all []MessageSign, id string) MessageSign {
	t.Helper()
	for _, s := range all {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("sign %s not in fixture", id)
	return MessageSign{}
}

// District 10 on 2026-10-01: a statewide safety campaign on 82 signs, a
// Pacheco Pass wind warning, and Valley traffic and work-zone notices.
// Nothing for the mountain counties.
func TestParseMessageSigns_LiveD10(t *testing.T) {
	signs, err := ParseMessageSigns(fixture(t, "cms_d10_20261001.json"))
	require.NoError(t, err)
	require.Len(t, signs, 107)

	displays := map[Display]int{}
	inService := 0
	for _, s := range signs {
		displays[s.Display]++
		if s.InService {
			inService++
		}
		assert.True(t, s.Location.HasPosition, s.ID)
		assert.Equal(t, time.Date(2026, 10, 1, 19, 44, 4, 0, time.UTC), s.RecordedAt.UTC(), s.ID)
	}
	assert.Equal(t, map[Display]int{DisplayOnePage: 86, DisplayBlank: 14, DisplayTwoPage: 5, DisplayUnknown: 2}, displays)
	assert.Equal(t, 106, inService)

	// The signs the issue names, in or next to our area.
	soulsbyville := findSign(t, signs, "V42")
	assert.Equal(t, "42 - EB 108 SOULSBYVILLE", soulsbyville.Location.Name)
	assert.Equal(t, "Tuolumne", soulsbyville.Location.County)
	assert.Equal(t, "SR-108", soulsbyville.Location.Route)
	assert.Equal(t, "East", soulsbyville.Location.Direction)
	assert.InDelta(t, 37.992415, soulsbyville.Location.Latitude, 1e-6)
	assert.InDelta(t, -120.274819, soulsbyville.Location.Longitude, 1e-6)
	assert.True(t, soulsbyville.InService)
	assert.Equal(t, DisplayOnePage, soulsbyville.Display)
	assert.Equal(t, "1 Page (Normal)", soulsbyville.RawDisplay)
	// The JSON says DON&apos;T.
	assert.Equal(t, [3]string{"BE THE DRIVER", "WHO SAVES LIVES", "DON'T SPEED"}, soulsbyville.Phase1)
	assert.Equal(t, [][3]string{soulsbyville.Phase1}, soulsbyville.Pages())
	assert.Equal(t, "BE THE DRIVER WHO SAVES LIVES DON'T SPEED", soulsbyville.Text())
	// Pacific local: 08:00:01 PDT is 15:00:01 UTC.
	assert.Equal(t, time.Date(2026, 9, 30, 15, 0, 1, 0, time.UTC), soulsbyville.MessageSince.UTC())

	murphys := findSign(t, signs, "V50")
	assert.Equal(t, "50 - EB 4 W/O MURPHYS", murphys.Location.Name)
	assert.Equal(t, "Calaveras", murphys.Location.County)

	// Concurrent routes: the route field names one of them.
	moccasin := findSign(t, signs, "V45")
	assert.Equal(t, "45 - EB 49 (MOCCASIN)", moccasin.Location.Name)
	assert.Equal(t, "SR-120", moccasin.Location.Route)
	assert.Equal(t, "SR-108", findSign(t, signs, "V43").Location.Route, "43 - EB 120 W/O YOSEMITE")

	pacheco := findSign(t, signs, "V35")
	assert.Equal(t, DisplayTwoPage, pacheco.Display)
	assert.Len(t, pacheco.Pages(), 2)
	assert.Equal(t, "GUSTY WIND WARNING / OVER PACHECO PASS", pacheco.Text())

	dark := findSign(t, signs, "V81") // 81 - EB 88 AT DEW DROP
	assert.Equal(t, DisplayBlank, dark.Display)
	assert.Empty(t, dark.Pages())
	assert.Empty(t, dark.Text())
	assert.Equal(t, time.Date(2026, 5, 29, 2, 45, 2, 0, time.UTC), dark.MessageSince.UTC())

	// Out of service, every field the placeholder: unknown, never blank and
	// never the text "Not Reported".
	planada := findSign(t, signs, "V97")
	assert.False(t, planada.InService)
	assert.Equal(t, DisplayUnknown, planada.Display)
	assert.Equal(t, "Not Reported", planada.RawDisplay)
	assert.Equal(t, "Not Reported", planada.Phase1[0], "raw lines are kept for diagnosis")
	assert.Empty(t, planada.Text())
	assert.True(t, planada.MessageSince.IsZero())

	// In service but never reported: display unknown, stamp the Unix epoch.
	elPortal := findSign(t, signs, "V900")
	assert.True(t, elPortal.InService)
	assert.Equal(t, DisplayUnknown, elPortal.Display)
	assert.True(t, elPortal.MessageSince.IsZero(), "1970-01-01 is a placeholder, not a time")
}

// D7's file was frozen on 2026-10-01, and it never reports message times.
func TestParseMessageSigns_D07(t *testing.T) {
	signs, err := ParseMessageSigns(fixture(t, "cms_d07_frozen_20261001.json"))
	require.NoError(t, err)
	require.Len(t, signs, 5)

	hollywood := findSign(t, signs, "759126")
	assert.Equal(t, DisplayOnePage, hollywood.Display)
	assert.Equal(t, "S405 AT SEPULVDA 3 LANES ONLY 10-03 & 10-04", hollywood.Text(), "a bare & survives decoding")
	assert.True(t, hollywood.MessageSince.IsZero(), "message time Not Reported")
	assert.Equal(t, time.Date(2026, 9, 29, 12, 32, 32, 0, time.UTC), hollywood.RecordedAt.UTC())

	assert.Equal(t, "SAVE LIVES SLOW DOWN IN WORK ZONE / EAST 91 EXIT CLOSED", findSign(t, signs, "726730").Text())
	assert.Equal(t, DisplayBlank, findSign(t, signs, "726684").Display)
	assert.Equal(t, DisplayUnknown, findSign(t, signs, "777475").Display)
}

func TestParseMessageSigns_DistrictQuirks(t *testing.T) {
	// D3 capitalizes inService and pads lines to position them on the face.
	d3, err := ParseMessageSigns(fixture(t, "cms_d03_quirks_20261001.json"))
	require.NoError(t, err)
	require.Len(t, d3, 3)
	walnut := findSign(t, d3, "1179")
	assert.True(t, walnut.InService, `"True"`)
	assert.Equal(t, [3]string{"ELK GROVE 10 MIN", ".US 50 22 MIN", ""}, walnut.Phase1)
	assert.Equal(t, "SR 20 TRAFFIC CONTROL EXPECT DELAYS", findSign(t, d3, "1156").Text())
	assert.False(t, findSign(t, d3, "4109").InService, `"False"`)

	// D2 leaves inService blank on two rows, and one row has no location.
	d2, err := ParseMessageSigns(fixture(t, "cms_d02_quirks_20261001.json"))
	require.NoError(t, err)
	require.Len(t, d2, 2)
	ghost := findSign(t, d2, "0")
	assert.False(t, ghost.InService)
	assert.False(t, ghost.Location.HasPosition)
	assert.Empty(t, ghost.Location.Name)
	assert.True(t, findSign(t, d2, "46").Location.HasPosition)
}

// A display mode is believed only when the lines agree with it.
func TestParseDisplay(t *testing.T) {
	none := [3]string{}
	text := [3]string{"CHAINS", "REQUIRED", ""}
	cases := []struct {
		name   string
		raw    string
		p1, p2 [3]string
		want   Display
	}{
		{"blank", "Blank", none, none, DisplayBlank},
		{"case-insensitive", " blank ", none, none, DisplayBlank},
		{"one page", "1 Page (Normal)", text, none, DisplayOnePage},
		{"two pages", "2 Pages (Extended)", text, text, DisplayTwoPage},
		{"not reported", "Not Reported", none, none, DisplayUnknown},
		{"unrecognized mode", "3 Pages", text, text, DisplayUnknown},
		{"empty mode", "", text, none, DisplayUnknown},
		{"blank with text", "Blank", text, none, DisplayUnknown},
		{"one page without text", "1 Page (Normal)", none, none, DisplayUnknown},
		{"one page with a second page", "1 Page (Normal)", text, text, DisplayUnknown},
		{"two pages, second empty", "2 Pages (Extended)", text, none, DisplayUnknown},
		{"placeholder line", "1 Page (Normal)", [3]string{"CHAINS", "Not Reported", ""}, none, DisplayUnknown},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, parseDisplay(tc.raw, tc.p1, tc.p2), tc.name)
	}
	assert.Equal(t, "TWO_PAGE", DisplayTwoPage.String())
	assert.Equal(t, "UNKNOWN", DisplayUnknown.String())
}

func TestParseMessageSigns_EmptyAndMalformed(t *testing.T) {
	_, err := ParseMessageSigns([]byte(`{"data": []}`))
	assert.ErrorIs(t, err, ErrEmptyFeed)

	_, err = ParseMessageSigns([]byte(`{"data": [{"cms": {"index": "a"}} {"cms": {"index": "b"}}]}`))
	assert.Error(t, err)
}

func TestClient_MessageSigns(t *testing.T) {
	ctx := context.Background()
	d := &fakeDoer{status: 200, body: fixture(t, "cms_d10_20261001.json")}
	signs, err := cmsClient(d).MessageSigns(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, signs, 107)
	assert.Equal(t, []string{"https://cwwp2.dot.ca.gov/data/d10/cms/cmsStatusD10.json"}, d.urls)

	_, err = cmsClient(&fakeDoer{status: 500}).MessageSigns(ctx, 10)
	assert.ErrorContains(t, err, "HTTP 500")

	_, err = cmsClient(&fakeDoer{status: 200, body: []byte(`{"data": []}`)}).MessageSigns(ctx, 10)
	assert.ErrorIs(t, err, ErrEmptyFeed)

	// D7 as captured: still served, last regenerated two days earlier.
	_, err = cmsClient(&fakeDoer{status: 200, body: fixture(t, "cms_d07_frozen_20261001.json")}).MessageSigns(ctx, 7)
	assert.ErrorIs(t, err, ErrStaleFeed)

	unstamped := []byte(`{"data": [{"cms": {"index": "V1", "recordTimestamp": {"recordDate": "10/01/2026", "recordTime": "12:44pm"}, "message": {"display": "Blank"}}}]}`)
	_, err = cmsClient(&fakeDoer{status: 200, body: unstamped}).MessageSigns(ctx, 10)
	assert.ErrorIs(t, err, ErrNoRecordTime)
}
