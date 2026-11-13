package caltrans

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHTML = `<!DOCTYPE html>
<html lang="en">
<head><title>Road Conditions</title></head>
<body>
<div id="main_content_1" style="width: 500px;">
    <div id="main_content_2">
        <div id="middle_column">
            <div>
            <hr>
<p><em>This highway information is the latest reported as of Tuesday, February 17th, 2026 at 10:25 PM.</em></p>
<h3>SR 4</h3>
<p>
<strong>[IN THE CENTRAL CALIFORNIA AREA]</strong><br />
Is closed 1 mi east of Camp Connell (Calaveras Co) - Due to snow
- Motorists are advised to use an alternate route
</p>
<p>
Is closed from 0.6 mi east of Lake Alpine to 7.2 mi west of the
Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - For the winter - Motorists are
advised to use an alternate route
</p>
<p>
Chains are required on all vehicles except 4-wheel-drive vehicles with snow
tires on all 4 wheels from Arnold to the Mt Reba turnoff (Calaveras Co)
</p>
<p>
Please research chain control locations as caltrans is currently working to
update chain control descriptions for consistency with internet mapping,
like google maps &amp; mapquest.
</p>
<p>
1-way controlled traffic at the Contra Costa/San Joaquin Co Line
/at Old River Bridge/ 24 hrs a day 7 days a week thru 1900 hrs on 7/26/26
- Due to construction
</p>
<hr />
            </div>
        </div>
    </div>
</div>
</body>
</html>`

func TestParseRoadConditionsHTML(t *testing.T) {
	conditions, err := ParseRoadConditionsHTML(testHTML, "4")

	require.NoError(t, err)
	require.Len(t, conditions, 4, "Should parse 4 conditions (skipping info notice)")

	// Verify first condition: closure at Camp Connell
	assert.Equal(t, CONDITION_CLOSURE, conditions[0].Type)
	assert.Contains(t, conditions[0].Description, "Camp Connell")
	assert.Contains(t, conditions[0].Description, "snow")
	assert.Equal(t, "snow", conditions[0].Reason)
	assert.Equal(t, "4", conditions[0].Highway)
	assert.Equal(t, "IN THE CENTRAL CALIFORNIA AREA", conditions[0].Area)

	// Verify second condition: winter closure at Ebbetts Pass
	assert.Equal(t, CONDITION_CLOSURE, conditions[1].Type)
	assert.Contains(t, conditions[1].Description, "Lake Alpine")
	assert.Contains(t, conditions[1].Description, "Ebbetts Pass")
	assert.Equal(t, "winter_closure", conditions[1].Reason)

	// Verify third condition: chain control
	assert.Equal(t, CONDITION_CHAIN, conditions[2].Type)
	assert.Contains(t, conditions[2].Description, "Arnold")
	assert.Contains(t, conditions[2].Description, "Mt Reba")

	// Verify fourth condition: restriction
	assert.Equal(t, CONDITION_RESTRICTION, conditions[3].Type)
	assert.Contains(t, conditions[3].Description, "1-way controlled traffic")
	assert.Equal(t, "construction", conditions[3].Reason)
}

func TestParseRoadConditionsHTML_NoConditions(t *testing.T) {
	html := `<html><body><h3>SR 99</h3><p>No conditions reported</p><hr /></body></html>`
	conditions, err := ParseRoadConditionsHTML(html, "4")

	require.NoError(t, err)
	assert.Nil(t, conditions, "Should return nil when highway not found")
}

func TestParseRoadConditionsHTML_LastUpdated(t *testing.T) {
	conditions, err := ParseRoadConditionsHTML(testHTML, "4")
	require.NoError(t, err)
	require.NotEmpty(t, conditions)

	assert.Equal(t, "2026-02-17T22:25:00Z", conditions[0].LastUpdated)
}

func TestClassifyCondition(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected RoadConditionType
	}{
		{"Closure - is closed", "Is closed 1 mi east of Camp Connell", CONDITION_CLOSURE},
		{"Closure - are closed", "Lanes are closed due to construction", CONDITION_CLOSURE},
		{"Closure - starts with closed", "Closed from mile 10 to mile 20", CONDITION_CLOSURE},
		{"Chain control", "Chains are required on all vehicles", CONDITION_CHAIN},
		{"Chain control - required", "Chains required from Arnold", CONDITION_CHAIN},
		{"Restriction - 1-way", "1-way controlled traffic at the bridge", CONDITION_RESTRICTION},
		{"Restriction - no traffic", "No traffic permitted after 10pm", CONDITION_RESTRICTION},
		{"Info", "Road work scheduled for next week", CONDITION_INFO},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyCondition(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractReason(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string
	}{
		{"Snow", "Closed due to snow", "snow"},
		{"Construction", "Closed due to construction", "construction"},
		{"Winter", "Closed for the winter", "winter_closure"},
		{"Rockslide", "Closed due to rock slide", "rockslide"},
		{"Fire", "Closed due to fire", "fire"},
		{"No reason", "Road is closed", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractReason(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractLastUpdatedFromPage(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		expected string
	}{
		{
			name:     "Standard format",
			html:     `<em>This highway information is the latest reported as of Tuesday, February 17th, 2026 at 10:25 PM.</em>`,
			expected: "2026-02-17T22:25:00Z",
		},
		{
			name:     "No timestamp",
			html:     `<em>Some other text</em>`,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractLastUpdated(tt.html)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMatchConditionToSegment(t *testing.T) {
	tests := []struct {
		name             string
		condition        RoadCondition
		sectionName      string
		locationKeywords []string
		expected         bool
	}{
		{
			name: "Matches section endpoint - Arnold",
			condition: RoadCondition{
				Description: "Chains are required from Arnold to the Mt Reba turnoff",
			},
			sectionName:      "Arnold to Bear Valley",
			locationKeywords: nil,
			expected:         true,
		},
		{
			name: "Matches location keyword - Camp Connell",
			condition: RoadCondition{
				Description: "Is closed 1 mi east of Camp Connell (Calaveras Co)",
			},
			sectionName:      "Arnold to Bear Valley",
			locationKeywords: []string{"Camp Connell", "Dorrington", "White Pines"},
			expected:         true,
		},
		{
			name: "Matches location keyword - Tamarack",
			condition: RoadCondition{
				Description: "Is closed from 0.6 mi east of Tamarack to 7.2 mi west of the Jct of SR 89",
			},
			sectionName:      "Arnold to Bear Valley",
			locationKeywords: []string{"Camp Connell", "Tamarack"},
			expected:         true,
		},
		{
			name: "Does not match different segment",
			condition: RoadCondition{
				Description: "Is closed 1 mi east of Camp Connell (Calaveras Co)",
			},
			sectionName:      "Angels Camp to Murphys",
			locationKeywords: []string{"Vallecito", "Douglas Flat"},
			expected:         false,
		},
		{
			name: "Matches Contra Costa segment for construction",
			condition: RoadCondition{
				Description: "1-way controlled traffic at the Contra Costa/San Joaquin Co Line",
			},
			sectionName:      "Arnold to Bear Valley",
			locationKeywords: []string{"Camp Connell"},
			expected:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MatchConditionToSegment(tt.condition, tt.sectionName, tt.locationKeywords)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractSectionLocations(t *testing.T) {
	tests := []struct {
		name     string
		section  string
		expected []string
	}{
		{"Standard format", "Arnold to Bear Valley", []string{"Arnold", "Bear Valley"}},
		{"With dash", "Arnold - Bear Valley", []string{"Arnold", "Bear Valley"}},
		{"Multi-word locations", "Angels Camp to Murphys", []string{"Angels Camp", "Murphys"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractSectionLocations(tt.section)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// The pages Caltrans published on 2026-11-13, the day the passes closed.
func realPage(t *testing.T, file, highway string) []RoadCondition {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "testdata", "caltrans", file))
	require.NoError(t, err)
	conditions, err := ParseRoadConditionsHTML(string(b), highway)
	require.NoError(t, err)
	return conditions
}

func TestRelateConditionToSegment_RealClosures(t *testing.T) {
	sr4 := realPage(t, "road_conditions_sr4_20261113.html", "4")
	require.Len(t, sr4, 2)
	ebbetts := sr4[0]
	assert.Equal(t, CONDITION_CLOSURE, ebbetts.Type)
	assert.Equal(t, "snow", ebbetts.Reason)
	assert.Equal(t, "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow- Motorists are advised to use an alternate route", ebbetts.Description)

	arnoldBearValley := SectionPlaces("Arnold to Bear Valley",
		[]string{"Camp Connell", "Dorrington", "White Pines", "Big Trees", "Ganns", "Tamarack"}, nil,
		[]string{"Mt Reba", "Lake Alpine", "Ebbetts"})
	assert.Equal(t, SegmentPastEnd, RelateConditionToSegment(ebbetts, arnoldBearValley),
		"the closure starts east of the Mt Reba turnoff, where the section ends")
	assert.Equal(t, SegmentUnrelated, RelateConditionToSegment(sr4[1], arnoldBearValley), "construction at the Delta")

	// With the places past the end listed as places ON the section, as they
	// were until 2026-11-13, the same text closed Arnold to Bear Valley.
	assert.True(t, MatchConditionToSegment(ebbetts, "Arnold to Bear Valley", []string{"Camp Connell", "Lake Alpine", "Mt Reba", "Ebbetts"}))

	// Sonora Pass: a section ending at Strawberry would not be closed by it.
	sr108 := realPage(t, "road_conditions_sr108_20261113.html", "108")
	require.NotEmpty(t, sr108)
	sonora := sr108[0]
	assert.Equal(t, CONDITION_CLOSURE, sonora.Type)
	toStrawberry := SectionPlaces("Sonora to Strawberry", []string{"Twain Harte", "Mi-Wuk", "Long Barn", "Pinecrest"}, nil,
		[]string{"Kennedy Meadows", "Sonora Pass", "US 395"})
	// "Sonora Pass" is past the end; the origin "Sonora" inside it is not a
	// mention of the town.
	assert.Equal(t, SegmentPastEnd, RelateConditionToSegment(sonora, toStrawberry))
}

func TestRelateConditionToSegment_Rule(t *testing.T) {
	places := SegmentPlaces{Origin: "Arnold", Destination: "Bear Valley",
		On: []string{"Dorrington"}, BeforeStart: []string{"Murphys"}, PastEnd: []string{"Lake Alpine", "Ebbetts"}}
	for text, want := range map[string]SegmentRelation{
		"Is closed at Dorrington":                        SegmentOn,
		"Is closed from Dorrington to Ebbetts Pass":      SegmentOn,
		"Is closed from Murphys to Lake Alpine":          SegmentOn,
		"Is closed from Arnold to Bear Valley":           SegmentOn,
		"Is closed from Arnold to Lake Alpine":           SegmentOn,
		"Is closed from Murphys to Bear Valley":          SegmentOn,
		"Is closed at Arnold":                            SegmentOn,
		"Is closed at Bear Valley":                       SegmentOn,
		"Is closed from Lake Alpine to Ebbetts Pass":     SegmentPastEnd,
		"Is closed from Bear Valley to Ebbetts Pass":     SegmentPastEnd,
		"Is closed from Angels Camp to Murphys":          SegmentBeforeStart,
		"Is closed from Murphys to Arnold":               SegmentBeforeStart,
		"Is closed at the Contra Costa/San Joaquin line": SegmentUnrelated,
	} {
		assert.Equal(t, want, RelateConditionToSegment(RoadCondition{Description: text}, places), text)
	}
}

func TestConditionSpan(t *testing.T) {
	for _, tt := range []struct {
		typ            RoadConditionType
		text, from, to string
	}{
		{CONDITION_CLOSURE, "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow- Motorists are advised to use an alternate route",
			"0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/", "the Jct of SR 89 /Ebbetts Pass/"},
		{CONDITION_CLOSURE, "Is closed from 26.4 mi east of Strawberry to 5.6 mi west of the Jct of US 395 /Sonora Pass/ (Tuolumne,Mono Co) - Due to snow- Motorists are advised to use an alternate route",
			"26.4 mi east of Strawberry", "5.6 mi west of the Jct of US 395 /Sonora Pass/"},
		{CONDITION_CLOSURE, "Is closed from the Jct of US 395 (Mono Co) to the Jct of SR 4 /Monitor Pass/ (Alpine Co) - Due to snow - Motorists are advised to use an alternate route",
			"the Jct of US 395", "the Jct of SR 4 /Monitor Pass/"},
		{CONDITION_CLOSURE, "Is closed 5.1 mi east of the Jct of US 395 to 7.5 mi west of west of the Jct of US 6 (Mono Co) - Due to snow - Motorists are advised to use an alternate route",
			"5.1 mi east of the Jct of US 395", "7.5 mi west of west of the Jct of US 6"},
		{CONDITION_CLOSURE, "Is closed 1 mi east of Camp Connell (Calaveras Co) - Due to snow - Motorists are advised to use an alternate route",
			"1 mi east of Camp Connell", ""},
		{CONDITION_CLOSURE, "Is closed from 0.6 mi east of Lake Alpine to 7.2 mi west of the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - For the winter - Motorists are advised to use an alternate route",
			"0.6 mi east of Lake Alpine", "7.2 mi west of the Jct of SR 89 /Ebbetts Pass/"},
		{CONDITION_RESTRICTION, "1-way controlled traffic at various locations from the Contra Costa/San Joaquin Co Line to 8.2 mi east of the Contra Costa/San Joaquin Co Line /at Trapper Rd/ from 2000 hrs each night to 0400 hrs each morning Thursday thru Sunday thru 11/15/26 - Due to construction",
			"the Contra Costa/San Joaquin Co Line", "8.2 mi east of the Contra Costa/San Joaquin Co Line /at Trapper Rd/"},
		{CONDITION_INFO, "For Yosemite Nat'l Park road information call 209-372-0200", "", ""},
	} {
		from, to := ConditionSpan(RoadCondition{Type: tt.typ, Description: tt.text})
		assert.Equal(t, tt.from, from, tt.text)
		assert.Equal(t, tt.to, to, tt.text)
	}
}
