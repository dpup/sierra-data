package services

import (
	"context"
	"os"
	"path/filepath"
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
// these tests exercise the keyword lists production runs with.
func shippedRoads(t *testing.T) []config.MonitoredRoad {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "prefab.yaml"))
	require.NoError(t, err)
	var doc struct {
		Roads struct {
			MonitoredRoads []struct {
				Name                string   `yaml:"name"`
				Section             string   `yaml:"section"`
				ID                  string   `yaml:"id"`
				LocationKeywords    []string `yaml:"locationKeywords"`
				BeforeStartKeywords []string `yaml:"beforeStartKeywords"`
				PastEndKeywords     []string `yaml:"pastEndKeywords"`
			} `yaml:"monitoredRoads"`
		} `yaml:"roads"`
	}
	require.NoError(t, yaml.Unmarshal(b, &doc))
	var out []config.MonitoredRoad
	for _, r := range doc.Roads.MonitoredRoads {
		out = append(out, config.MonitoredRoad{Name: r.Name, Section: r.Section, ID: r.ID,
			LocationKeywords: r.LocationKeywords, BeforeStartKeywords: r.BeforeStartKeywords, PastEndKeywords: r.PastEndKeywords})
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

// The Ebbetts Pass closure as Caltrans published it on 2026-11-13. It starts
// half a mile east of the Mt Reba turnoff, which is where the Arnold to Bear
// Valley section ends, so every monitored Hwy 4 section is open; the closure
// is reported past the end of the last one.
func TestApplyRoadConditions_EbbettsPassClosureIsPastBearValley(t *testing.T) {
	roads := shippedRoads(t)
	conditions := realConditions(t, "road_conditions_sr4_20261113.html", "4")
	const text = "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow- Motorists are advised to use an alternate route"
	require.Equal(t, text, conditions[0].Description)
	require.Equal(t, caltrans.CONDITION_CLOSURE, conditions[0].Type)

	last := applyTo(t, roads, "hwy4-arnold-bearvalley", conditions)
	assert.Equal(t, api.RoadStatus_OPEN, last.status, "Arnold to Bear Valley is open")
	assert.Empty(t, last.causes)
	assert.Empty(t, last.expl)
	require.Len(t, last.beyond, 1)
	b := last.beyond[0]
	assert.Equal(t, api.RoadStatus_CLOSED, b.GetStatus())
	assert.Equal(t, "END", b.GetSide())
	assert.Equal(t, "road_condition", b.GetSource())
	assert.Equal(t, text, b.GetDescription())
	assert.Equal(t, "snow", b.GetReason())
	assert.Equal(t, "0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/", b.GetFrom())
	assert.Equal(t, "the Jct of SR 89 /Ebbetts Pass/", b.GetTo())
	require.Len(t, last.alerts, 1)
	assert.Equal(t, api.AlertClassification_NEARBY, last.alerts[0].GetClassification())

	// The sections below are open too, and do not repeat the closure: it is
	// past the end of the chain, not of each section. The Delta construction
	// notice on the same page touches none of them.
	for _, id := range []string{"hwy4-angels-murphys", "hwy4-murphys-arnold"} {
		a := applyTo(t, roads, id, conditions)
		assert.Equal(t, api.RoadStatus_OPEN, a.status, id)
		assert.Empty(t, a.beyond, id)
		assert.Empty(t, a.alerts, id)
	}
}

// What must still close or restrict a section: anything naming a place on it,
// or running through it from one side to the other.
func TestApplyRoadConditions_OnSegmentStillApplies(t *testing.T) {
	roads := shippedRoads(t)
	closure := func(text string) []caltrans.RoadCondition {
		return []caltrans.RoadCondition{{Highway: "4", Type: caltrans.CONDITION_CLOSURE, Description: text}}
	}
	for _, tt := range []struct {
		name, text string
		want       map[string]api.RoadStatus
		beyond     map[string]string // road id -> side
	}{
		{
			name: "a place on the section",
			text: "Is closed 1 mi east of Camp Connell (Calaveras Co) - Due to snow",
			want: map[string]api.RoadStatus{"hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name: "from on the section to past its end",
			text: "Is closed from Tamarack to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow",
			want: map[string]api.RoadStatus{"hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name: "the whole section, end to end",
			text: "Is closed from Arnold to Bear Valley (Calaveras, Alpine Co) - Due to snow",
			want: map[string]api.RoadStatus{"hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name: "from the section's start to past its end",
			text: "Is closed from Arnold to the Mt Reba turnoff (Calaveras Co) - Due to snow",
			want: map[string]api.RoadStatus{"hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name: "across two sections",
			text: "Is closed from Avery to Dorrington (Calaveras Co) - Due to a fire",
			want: map[string]api.RoadStatus{"hwy4-murphys-arnold": api.RoadStatus_CLOSED, "hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name: "at a town two sections share: both, the side is not known",
			text: "Is closed at Arnold (Calaveras Co) - Due to an accident",
			want: map[string]api.RoadStatus{"hwy4-murphys-arnold": api.RoadStatus_CLOSED, "hwy4-arnold-bearvalley": api.RoadStatus_CLOSED},
		},
		{
			name:   "the early-season gate at Lake Alpine",
			text:   "Is closed from 0.6 mi east of Lake Alpine to 7.2 mi west of the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - For the winter - Motorists are advised to use an alternate route",
			want:   map[string]api.RoadStatus{},
			beyond: map[string]string{"hwy4-arnold-bearvalley": "END"},
		},
		{
			name:   "starting exactly at the last section's end",
			text:   "Is closed from Bear Valley to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow",
			want:   map[string]api.RoadStatus{},
			beyond: map[string]string{"hwy4-arnold-bearvalley": "END"},
		},
		{
			name:   "before the first section",
			text:   "Is closed from the Jct of SR 99 to Copperopolis (San Joaquin, Calaveras Co) - Due to flooding",
			want:   map[string]api.RoadStatus{},
			beyond: map[string]string{"hwy4-angels-murphys": "START"},
		},
		{
			name: "nowhere near",
			text: "Is closed at the Contra Costa/San Joaquin Co Line /at Old River Bridge/ - Due to construction",
			want: map[string]api.RoadStatus{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, id := range []string{"hwy4-angels-murphys", "hwy4-murphys-arnold", "hwy4-arnold-bearvalley"} {
				a := applyTo(t, roads, id, closure(tt.text))
				want, closed := tt.want[id]
				if !closed {
					want = api.RoadStatus_OPEN
				}
				assert.Equal(t, want, a.status, id)
				assert.Equal(t, closed, len(a.causes) == 1, "%s causes", id)
				if side, ok := tt.beyond[id]; ok {
					if assert.Len(t, a.beyond, 1, id) {
						assert.Equal(t, side, a.beyond[0].GetSide(), id)
					}
				} else {
					assert.Empty(t, a.beyond, id)
				}
			}
		})
	}
}

// A chain requirement past the end is not a requirement on the section, and
// is not reported beside it either: the chain_control layer has the
// checkpoints.
func TestApplyRoadConditions_ChainsPastTheEnd(t *testing.T) {
	roads := shippedRoads(t)
	chains := []caltrans.RoadCondition{{Highway: "4", Type: caltrans.CONDITION_CHAIN,
		Description: "Chains are required on all vehicles except 4-wheel-drive vehicles with snow tires on all 4 wheels from Lake Alpine to Ebbetts Pass (Alpine Co)"}}
	a := applyTo(t, roads, "hwy4-arnold-bearvalley", chains)
	assert.Equal(t, api.ChainControlStatus_NONE, a.chain)
	assert.Empty(t, a.beyond)

	onRoute := []caltrans.RoadCondition{{Highway: "4", Type: caltrans.CONDITION_CHAIN,
		Description: "Chains are required on all vehicles except 4-wheel-drive vehicles with snow tires on all 4 wheels from Arnold to the Mt Reba turnoff (Calaveras Co)"}}
	assert.Equal(t, api.ChainControlStatus_REQUIRED, applyTo(t, roads, "hwy4-arnold-bearvalley", onRoute).chain)
	assert.Equal(t, api.ChainControlStatus_NONE, applyTo(t, roads, "hwy4-murphys-arnold", onRoute).chain,
		"it starts where Murphys to Arnold ends")
}

func TestPlacesFor_Chain(t *testing.T) {
	roads := shippedRoads(t)
	byID := map[string]config.MonitoredRoad{}
	for _, r := range roads {
		byID[r.ID] = r
	}
	mid := placesFor(byID["hwy4-murphys-arnold"], roads)
	assert.Equal(t, "Murphys", mid.Origin)
	assert.Equal(t, "Arnold", mid.Destination)
	assert.False(t, mid.firstInChain)
	assert.False(t, mid.lastInChain)
	assert.Subset(t, mid.PastEnd, []string{"Bear Valley", "Camp Connell", "Tamarack", "Ebbetts"})
	assert.Subset(t, mid.BeforeStart, []string{"Angels Camp", "Vallecito", "Copperopolis"})
	assert.NotContains(t, mid.PastEnd, "Arnold")
	assert.NotContains(t, mid.BeforeStart, "Murphys")

	last := placesFor(byID["hwy4-arnold-bearvalley"], roads)
	assert.True(t, last.lastInChain)
	assert.False(t, last.firstInChain)
	assert.ElementsMatch(t, []string{"Mt Reba", "Lake Alpine", "Ebbetts"}, last.PastEnd)

	// Hwy 49 starts at Angels Camp too, but it is another highway.
	other := placesFor(byID["hwy49-angels-sonora"], roads)
	assert.True(t, other.firstInChain)
	assert.True(t, other.lastInChain)
	assert.Empty(t, other.PastEnd)
	assert.Empty(t, other.BeforeStart)
}
