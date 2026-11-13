package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// shippedRoads reads roads.monitoredRoads from the shipped prefab.yaml, so
// these tests exercise the keywords and seasonal closures production runs with.
func shippedRoads(t *testing.T) []config.MonitoredRoad {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "prefab.yaml"))
	require.NoError(t, err)
	var doc struct {
		Roads struct {
			MonitoredRoads []struct {
				Name             string   `yaml:"name"`
				Section          string   `yaml:"section"`
				ID               string   `yaml:"id"`
				LocationKeywords []string `yaml:"locationKeywords"`
				SeasonalClosures []struct {
					Name                 string   `yaml:"name"`
					Side                 string   `yaml:"side"`
					Start                []string `yaml:"start"`
					End                  []string `yaml:"end"`
					StartOffsetDirection string   `yaml:"startOffsetDirection"`
					MaxStartOffsetMiles  float64  `yaml:"maxStartOffsetMiles"`
				} `yaml:"seasonalClosures"`
			} `yaml:"monitoredRoads"`
		} `yaml:"roads"`
	}
	require.NoError(t, yaml.Unmarshal(b, &doc))
	var out []config.MonitoredRoad
	for _, r := range doc.Roads.MonitoredRoads {
		road := config.MonitoredRoad{Name: r.Name, Section: r.Section, ID: r.ID, LocationKeywords: r.LocationKeywords}
		for _, sc := range r.SeasonalClosures {
			road.SeasonalClosures = append(road.SeasonalClosures, config.SeasonalClosure{Name: sc.Name, Side: sc.Side,
				Start: sc.Start, End: sc.End, StartOffsetDirection: sc.StartOffsetDirection, MaxStartOffsetMiles: sc.MaxStartOffsetMiles})
		}
		out = append(out, road)
	}
	require.Len(t, out, 4, "three Hwy 4 sections and Hwy 49")
	return out
}

// realConditions parses a roads.dot.ca.gov page captured on 2026-11-13.
func realConditions(t *testing.T, file, highway string) []caltrans.RoadCondition {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "caltrans", file))
	require.NoError(t, err)
	conditions, err := caltrans.ParseRoadConditionsHTML(string(b), highway)
	require.NoError(t, err)
	require.NotEmpty(t, conditions)
	return conditions
}

type applied struct {
	status api.RoadStatus
	chain  api.ChainControlStatus
	expl   string
	alerts []*api.RoadAlert
	causes []*api.RoadStatusCause
	beyond []*api.RoadBeyond
}

func applyTo(t *testing.T, roads []config.MonitoredRoad, id string, conditions []caltrans.RoadCondition) applied {
	t.Helper()
	s := &RoadsService{config: &config.Config{Roads: config.RoadsConfig{MonitoredRoads: roads}}}
	for _, r := range roads {
		if r.ID != id {
			continue
		}
		a := applied{status: api.RoadStatus_OPEN, chain: api.ChainControlStatus_NONE}
		a.beyond = s.applyRoadConditions(logging.EnsureLogger(context.Background()), r, conditions, &a.status, &a.chain, &a.expl, &a.alerts, &a.causes)
		return a
	}
	t.Fatalf("no monitored road %q", id)
	return applied{}
}

const (
	hwy4Lower  = "hwy4-angels-murphys"
	hwy4Middle = "hwy4-murphys-arnold"
	hwy4Upper  = "hwy4-arnold-bearvalley"
)

const ebbettsClosureText = "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow- Motorists are advised to use an alternate route"

// The shipped config names exactly one seasonal closure, on the last Hwy 4
// section, and keeps the places past Bear Valley as keywords of that section:
// any other text naming them is on it.
func TestShippedSeasonalClosures(t *testing.T) {
	for _, r := range shippedRoads(t) {
		if r.ID != hwy4Upper {
			assert.Empty(t, r.SeasonalClosures, r.ID)
			continue
		}
		assert.Subset(t, r.LocationKeywords, []string{"Lake Alpine", "Mt Reba", "Ebbetts"})
		require.Len(t, r.SeasonalClosures, 1)
		sc := r.SeasonalClosures[0]
		assert.Equal(t, "Ebbetts Pass winter closure", sc.Name)
		assert.Equal(t, "end", sc.Side)
		assert.Equal(t, "east", sc.StartOffsetDirection)
		assert.LessOrEqual(t, sc.MaxStartOffsetMiles, 2.0)
	}
}

// The Ebbetts Pass closure as Caltrans published it on 2026-11-13. It is the
// named seasonal closure: Arnold to Bear Valley stays open and reports it
// past its end. The other sections never matched it.
func TestApplyRoadConditions_EbbettsPassWinterClosure(t *testing.T) {
	roads := shippedRoads(t)
	conditions := realConditions(t, "road_conditions_sr4_20261113.html", "4")
	require.Equal(t, ebbettsClosureText, conditions[0].Description)
	require.Equal(t, caltrans.CONDITION_CLOSURE, conditions[0].Type)

	last := applyTo(t, roads, hwy4Upper, conditions)
	assert.Equal(t, api.RoadStatus_OPEN, last.status, "Arnold to Bear Valley is open")
	assert.Equal(t, api.ChainControlStatus_NONE, last.chain)
	assert.Empty(t, last.causes)
	assert.Empty(t, last.expl)
	require.Len(t, last.beyond, 1)
	b := last.beyond[0]
	assert.Equal(t, "Ebbetts Pass winter closure", b.GetName())
	assert.Equal(t, api.RoadStatus_CLOSED, b.GetStatus())
	assert.Equal(t, "END", b.GetSide())
	assert.Equal(t, "road_condition", b.GetSource())
	assert.Equal(t, ebbettsClosureText, b.GetDescription())
	assert.Equal(t, "snow", b.GetReason())
	assert.Equal(t, "0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/", b.GetFrom())
	assert.Equal(t, "the Jct of SR 89 /Ebbetts Pass/", b.GetTo())
	require.Len(t, last.alerts, 1, "the closure is still an alert, beside the road")
	assert.Equal(t, api.AlertClassification_NEARBY, last.alerts[0].GetClassification())
	assert.Equal(t, api.AlertType_CLOSURE, last.alerts[0].GetType())

	// The Delta construction notice on the same page touches no section.
	for _, id := range []string{hwy4Lower, hwy4Middle} {
		a := applyTo(t, roads, id, conditions)
		assert.Equal(t, api.RoadStatus_OPEN, a.status, id)
		assert.Empty(t, a.beyond, id)
		assert.Empty(t, a.alerts, id)
	}

	// Sonora Pass, the same day: nothing monitored is on SR 108, and no
	// seasonal closure is configured for it.
	sr108 := realConditions(t, "road_conditions_sr108_20261113.html", "108")
	for _, r := range roads {
		a := applyTo(t, roads, r.ID, sr108)
		assert.Empty(t, a.beyond, r.ID)
	}
}

type want struct {
	status  api.RoadStatus
	chain   api.ChainControlStatus
	onRoute bool // the text matched the section: one ON_ROUTE alert
}

var (
	clear    = want{api.RoadStatus_OPEN, api.ChainControlStatus_NONE, false}
	closed   = want{api.RoadStatus_CLOSED, api.ChainControlStatus_NONE, true}
	chained  = want{api.RoadStatus_OPEN, api.ChainControlStatus_REQUIRED, true}
	restrict = want{api.RoadStatus_RESTRICTED, api.ChainControlStatus_NONE, true}
	// info: an on-route notice that changes neither status nor chain control.
	info = want{api.RoadStatus_OPEN, api.ChainControlStatus_NONE, true}
)

const ebbettsSpan = "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/"

// Everything that is not the named closure is matched the way it always was:
// a text naming a place of a section is ON that section. No row here may
// produce a beyond entry, and none may leave the upper section clear.
//
// The first nine rows are the false all-clears an adversarial review found
// in the general "past the end" rule this replaced.
func TestApplyRoadConditions_EverythingElseStaysOnTheSection(t *testing.T) {
	roads := shippedRoads(t)
	const r2 = "Chains are required on all vehicles except 4-wheel-drive vehicles with snow tires on all 4 wheels"
	for _, tt := range []struct {
		text                 string
		lower, middle, upper want
	}{
		{r2 + " from Cottage Springs to the Jct of SR 207 /Mt Reba Turnoff/", clear, clear, chained},
		{"Is closed from Cottage Springs to the Jct of SR 89 /Ebbetts Pass/ - Due to snow", clear, clear, closed},
		{"Is closed from 10 mi west of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/", clear, clear, closed},
		{"Is closed from 5 mi west of Bear Valley to Lake Alpine", clear, clear, closed},
		{"Is closed 4 mi west of Lake Alpine", clear, clear, closed},
		{"Is closed at the Jct of SR 207 /Mt Reba Turnoff/", clear, clear, closed},
		{"Is closed from the Alpine/Calaveras Co Line to Lake Alpine", clear, clear, closed},
		{"Is closed from 2 mi west of Arnold to Dorrington", clear, closed, closed},
		{"Chains are required from 5 mi west of Arnold to the Jct of SR 207 /Mt Reba Turnoff/", clear, chained, chained},

		// Near misses of the named closure.
		{"Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to Lake Alpine (Alpine Co) - Due to snow", clear, clear, closed},
		{"Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to 7.2 mi west of the Jct of SR 89 /Ebbetts Pass/ (Alpine Co)", clear, clear, closed},
		{"Is closed from 0.5 mi west of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co)", clear, clear, closed},
		{"Is closed from 2.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co)", clear, clear, closed},
		{"Is closed 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ (Alpine Co) - Due to snow", clear, clear, closed},
		{r2 + " from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co)", clear, clear, chained},
		{"Chains or snow tires are required from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co)", clear, clear, chained},
		{"1-way controlled traffic from the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ - Due to construction", clear, clear, restrict},
		// The named span, with more on the section said after it.
		{"Is closed from the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ - Due to snow - Also closed at Dorrington", clear, clear, closed},
		// The named span, with anything else said after it: places nobody
		// configured, another requirement, another sentence. On the section.
		{ebbettsSpan + " (Alpine Co) - Due to snow - Chains are required from Cottage Springs to Cabbage Patch", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Black Springs", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Cottage Springs", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Spicer Reservoir Rd", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at the Calaveras/Alpine Co Line", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Hells Kitchen", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Bear Vly", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Mt. Reba", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Also closed at Mount Reba", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - Chains or snow tires are required 5 mi west of the closure", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) - Due to snow - The road is closed west of the turnoff as well", clear, clear, closed},
		{ebbettsSpan + " (Alpine Co) 24 hrs a day and at Cottage Springs - Due to snow", clear, clear, closed},
		// One paragraph holding the closure, a line break and a chain
		// requirement is parsed as one closure: on the section, not lost.
		{ebbettsSpan + " (Alpine Co) - Due to snow- Motorists are advised to use an alternate route<br />Chains are required from Cottage Springs to Cabbage Patch", clear, clear, closed},
		// Not requirements, and a restriction that mentions one.
		{"Chains are no longer required from Arnold to Dorrington", clear, info, info},
		{"Is open from Arnold to Dorrington - no chains or traction devices required", clear, info, info},
		{"1-way controlled traffic from Arnold to Dorrington - chains or snow tires are required", clear, restrict, restrict},
		// The early-season gate at Lake Alpine is not the configured closure.
		{"Is closed from 0.6 mi east of Lake Alpine to 7.2 mi west of the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - For the winter - Motorists are advised to use an alternate route", clear, clear, closed},
		{"Is closed from Bear Valley to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow", clear, clear, closed},

		// As on main, across the sections.
		{"Is closed 1 mi east of Camp Connell (Calaveras Co) - Due to snow", clear, clear, closed},
		{"Is closed from Arnold to Bear Valley (Calaveras, Alpine Co) - Due to snow", clear, closed, closed},
		{"Is closed at Arnold (Calaveras Co) - Due to an accident", clear, closed, closed},
		{"Is closed from Avery to Dorrington (Calaveras Co) - Due to a fire", clear, closed, closed},
		{"Is closed from the Jct of SR 99 to Copperopolis (San Joaquin, Calaveras Co) - Due to flooding", closed, clear, clear},
		{r2 + " from Arnold to the Mt Reba turnoff (Calaveras Co)", clear, chained, chained},
		{"Chains or snow tires are required from 3.5 mi east of Arnold to the Jct of SR 207 /Mt Reba Turnoff/ (Calaveras, Alpine Co)", clear, chained, chained},
		{"Is closed at the Contra Costa/San Joaquin Co Line /at Old River Bridge/ - Due to construction", clear, clear, clear},
	} {
		t.Run(tt.text, func(t *testing.T) {
			parsed, err := caltrans.ParseRoadConditionsHTML("<h3>SR 4</h3><p>"+tt.text+"</p><hr>", "4")
			require.NoError(t, err)
			require.Len(t, parsed, 1)
			for id, w := range map[string]want{hwy4Lower: tt.lower, hwy4Middle: tt.middle, hwy4Upper: tt.upper} {
				a := applyTo(t, roads, id, parsed)
				assert.Equal(t, w.status, a.status, id)
				assert.Equal(t, w.chain, a.chain, id)
				assert.Empty(t, a.beyond, id)
				if !w.onRoute {
					assert.Empty(t, a.alerts, id)
				} else if assert.Len(t, a.alerts, 1, id) {
					assert.Equal(t, api.AlertClassification_ON_ROUTE, a.alerts[0].GetClassification(), id)
				}
			}
		})
	}
}

// A seasonal closure configured before a section's origin reports START.
func TestApplyRoadConditions_SeasonalClosureBeforeTheStart(t *testing.T) {
	roads := []config.MonitoredRoad{{
		ID: "hwy108-strawberry-sonora", Name: "Hwy 108", Section: "Kennedy Meadows to Sonora",
		LocationKeywords: []string{"Strawberry", "Sonora Pass"},
		SeasonalClosures: []config.SeasonalClosure{{
			Name: "Sonora Pass winter closure", Side: "start",
			Start: []string{"Kennedy Meadows"}, End: []string{"Jct of US 395", "Sonora Pass"},
			StartOffsetDirection: "east", MaxStartOffsetMiles: 1,
		}},
	}}
	closure := func(text string) []caltrans.RoadCondition {
		return []caltrans.RoadCondition{{Highway: "108", Type: caltrans.CONDITION_CLOSURE, Description: text, Reason: "snow"}}
	}
	a := applyTo(t, roads, "hwy108-strawberry-sonora", closure("Is closed from 0.5 mi east of Kennedy Meadows to the Jct of US 395 /Sonora Pass/ (Tuolumne,Mono Co) - Due to snow"))
	assert.Equal(t, api.RoadStatus_OPEN, a.status)
	require.Len(t, a.beyond, 1)
	assert.Equal(t, "START", a.beyond[0].GetSide())
	assert.Equal(t, "Sonora Pass winter closure", a.beyond[0].GetName())

	// Mirrored near misses stay on the section.
	for _, text := range []string{
		"Is closed from 0.5 mi west of Kennedy Meadows to the Jct of US 395 /Sonora Pass/ - Due to snow",
		"Is closed from 3 mi east of Kennedy Meadows to the Jct of US 395 /Sonora Pass/ - Due to snow",
		"Is closed from Strawberry to the Jct of US 395 /Sonora Pass/ - Due to snow",
		"Is closed from Kennedy Meadows to 5.6 mi west of the Jct of US 395 /Sonora Pass/ - Due to snow",
		"Is closed at Kennedy Meadows - Due to snow",
	} {
		a := applyTo(t, roads, "hwy108-strawberry-sonora", closure(text))
		assert.Equal(t, api.RoadStatus_CLOSED, a.status, text)
		assert.Empty(t, a.beyond, text)
	}

	// A closure with no name in the config is ignored: the text is on the section.
	roads[0].SeasonalClosures[0].Name = " "
	a = applyTo(t, roads, "hwy108-strawberry-sonora", closure("Is closed from Kennedy Meadows to the Jct of US 395 /Sonora Pass/"))
	assert.Equal(t, api.RoadStatus_CLOSED, a.status)
}

// The wording around the named span may vary in case, spacing and its
// closing clauses and is still the seasonal closure; the same closure
// published twice is one entry and one alert.
func TestApplyRoadConditions_SeasonalClosureWordingAndDuplicates(t *testing.T) {
	roads := shippedRoads(t)
	for _, text := range []string{
		ebbettsClosureText,
		ebbettsSpan + " (Alpine Co)- Due to snow",
		ebbettsSpan + " (Alpine Co) - DUE TO SNOW",
		ebbettsSpan + " (Alpine Co) - Due to snow - Motorists are advised to use an alternate route.",
		ebbettsSpan + " (Alpine Co) - For the winter",
		ebbettsSpan + " (Alpine Co) 24 hrs a day - Due to snow",
		strings.ToUpper(ebbettsClosureText),
	} {
		parsed, err := caltrans.ParseRoadConditionsHTML("<h3>SR 4</h3><p>"+text+"</p><hr>", "4")
		require.NoError(t, err)
		require.Len(t, parsed, 1)
		a := applyTo(t, roads, hwy4Upper, parsed)
		assert.Equal(t, api.RoadStatus_OPEN, a.status, text)
		if assert.Len(t, a.beyond, 1, text) {
			assert.Equal(t, "Ebbetts Pass winter closure", a.beyond[0].GetName())
			assert.Equal(t, "the jct of sr 89 /ebbetts pass/", strings.ToLower(a.beyond[0].GetTo()), text)
		}
	}

	twice, err := caltrans.ParseRoadConditionsHTML("<h3>SR 4</h3><p>"+ebbettsClosureText+"</p><p>"+ebbettsSpan+" (Alpine Co) - For the winter</p><hr>", "4")
	require.NoError(t, err)
	require.Len(t, twice, 2)
	a := applyTo(t, roads, hwy4Upper, twice)
	assert.Equal(t, api.RoadStatus_OPEN, a.status)
	require.Len(t, a.beyond, 1)
	assert.Equal(t, "snow", a.beyond[0].GetReason(), "the first one published")
	assert.Len(t, a.alerts, 1)

	// The closure and, in its own paragraph, a requirement on the section.
	both, err := caltrans.ParseRoadConditionsHTML("<h3>SR 4</h3><p>"+ebbettsClosureText+"</p><p>Chains or snow tires are required from Dorrington to the Jct of SR 207 /Mt Reba Turnoff/</p><hr>", "4")
	require.NoError(t, err)
	a = applyTo(t, roads, hwy4Upper, both)
	assert.Equal(t, api.RoadStatus_OPEN, a.status)
	assert.Equal(t, api.ChainControlStatus_REQUIRED, a.chain)
	assert.Len(t, a.beyond, 1)
	assert.Len(t, a.alerts, 2)
}
