package hazards

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// The 2026-11-13 storm, both feeds captured at 17:38 PST.
const (
	stormCWWP2 = "cc_d10_20261113_storm.json"
	stormKML   = "chain_controls_20261113_173800.kml"
)

// everywhere takes in all of California, so nothing is scoped away.
var everywhere = config.HazardArea{ID: "all", Bounds: config.GeoBounds{
	MinLatitude: 32, MaxLatitude: 42.5, MinLongitude: -125, MaxLongitude: -114,
}}

func loadKML(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "caltrans", name))
	require.NoError(t, err)
	return string(b)
}

// stormRegistry is the storm's District 10 signs as the layer hands them to
// groupChainStretches: the requirements, and every other chain sign and gate.
func stormRegistry(t *testing.T) (active, quiet []caltrans.ChainControlData) {
	t.Helper()
	p := &caltrans.FeedParser{HTTPClient: kmlDoer{200, emptyCCKML}}
	p.UseCWWP2ChainControls(fixtureSource{loadCheckpoints(t, stormCWWP2)}, []int{10})
	controls, quiet, err := p.ParseChainControlRegistry(testCtx())
	require.NoError(t, err)
	for _, c := range controls {
		if c.Unrecognized {
			quiet = append(quiet, c) // the gates reading "ROAD CLOSED"
			continue
		}
		active = append(active, c)
	}
	return active, quiet
}

type stretchSummary struct {
	Highway, Level, From, To string
	Signs                    int
	Inferred                 bool
}

func summarize(stretches []chainStretch) []stretchSummary {
	out := make([]stretchSummary, 0, len(stretches))
	for _, st := range stretches {
		out = append(out, stretchSummary{
			Highway: st.Highway, Level: st.Level, Signs: len(st.Signs), Inferred: st.Inferred,
			From: st.Signs[0].LocationName + " " + st.Signs[0].Direction,
			To:   st.Signs[len(st.Signs)-1].LocationName + " " + st.Signs[len(st.Signs)-1].Direction,
		})
	}
	return out
}

// District 10 in the storm: 29 signs requiring chains, on five highways.
// They are six stretches (Hwy 88 changes level), every one established by
// CWWP2's own signs.
func TestGroupChainStretches_Storm(t *testing.T) {
	active, quiet := stormRegistry(t)
	require.Len(t, active, 29)

	assert.Equal(t, []stretchSummary{
		{Highway: "Highway 4", Level: "R2", Signs: 7, From: "POISON SPRINGS Eastbound", To: "MOUNT REBA ROAD Westbound"},
		{Highway: "Highway 88", Level: "R1", Signs: 1, From: "PICKETT'S JUNCTION Westbound", To: "PICKETT'S JUNCTION Westbound"},
		{Highway: "Highway 88", Level: "R2", Signs: 11, From: "LUMBER YARD Eastbound", To: "RED LAKE CREEK Westbound"},
		{Highway: "Highway 89", Level: "R2", Signs: 2, From: "PICKETTS JUNCTION Northbound", To: "ALP/ELD CNTY LINE LUTHER PASS Southbound"},
		{Highway: "Highway 108", Level: "R2", Signs: 6, From: "CASCADE CREEK Eastbound", To: "KENNEDY MEADOWS Westbound"},
		{Highway: "Highway 207", Level: "R2", Signs: 2, From: "JCT HWY 4 Northbound", To: "MT. REBA SUMMIT Southbound"},
	}, summarize(groupChainStretches(active, quiet)))
}

// What the storm's stretches depend on, one fact at a time.
func TestGroupChainStretches_StormEdges(t *testing.T) {
	active, quiet := stormRegistry(t)
	find := func(list []caltrans.ChainControlData, id string) caltrans.ChainControlData {
		for _, c := range list {
			if c.MessageID == id {
				return c
			}
		}
		t.Fatalf("checkpoint %s not in the fixture", id)
		return caltrans.ChainControlData{}
	}

	// The exit sign: Poison Springs westbound reads R-0 thirty metres from
	// the eastbound sign that starts the Hwy 4 stretch, and Cascade Creek
	// westbound R-0 sits at the very milepost of the eastbound R-2.
	assert.Equal(t, "R0", find(quiet, "10-CAL-4-56.27-W-11W").Level)
	assert.Equal(t, "R0", find(quiet, "10-TUO-108-40.381-W-17W").Level)

	// The gates: R-0 a hundredth of a mile from chain signs reading R-2
	// (Eagle Meadows Road on Hwy 108, Red Lake Creek on Hwy 88), in the
	// middle of a stretch. They are known as gates, so they take no part in
	// the walk (TestGroupChainStretches_Rules shows what one would do).
	for _, id := range []string{"10-TUO-108-44.45-E-2", "10-ALP-88-8.11-W-2", "10-AMA-88-69.4-E-3", "10-AMA-88-71.35-W-4", "10-ALP-88-1.9-E-1"} {
		gate := find(quiet, id)
		assert.True(t, gate.Gate, id)
		assert.Equal(t, "R0", gate.Level, id)
	}
	// Mileposts, not postmiles, order a route across a county line: Hwy 4's
	// postmile falls from CAL 63.99 to ALP 0.65 where the milepost goes on
	// from 157.02 to 157.37.
	tamarack, bearValley := find(active, "10-CAL-4-63.99-E-14E"), find(active, "10-ALP-4-0.65-W-14W")
	assert.Equal(t, "63.99", tamarack.Postmile)
	assert.Equal(t, "0.65", bearValley.Postmile)
	assert.Less(t, tamarack.Milepost, bearValley.Milepost)
}

func sign(id, highway, direction, level string, milepost float64) caltrans.ChainControlData {
	return caltrans.ChainControlData{
		MessageID: id, District: "10", Source: caltrans.SourceCWWP2, Highway: highway, Direction: direction,
		Level: level, LocationName: id, Milepost: milepost, HasMilepost: true,
		// On a line running east, a mile every 0.02 degrees: far enough apart
		// that nothing here joins by proximity unless a test means it to.
		Coordinates: &api.Coordinates{Latitude: 38, Longitude: -121 + milepost*0.02},
	}
}

func ids(st chainStretch) []string {
	out := make([]string, 0, len(st.Signs))
	for _, c := range st.Signs {
		out = append(out, c.MessageID)
	}
	return out
}

func TestGroupChainStretches_Rules(t *testing.T) {
	const hwy = "Highway 4"

	t.Run("road no sign puts under the requirement is a gap", func(t *testing.T) {
		// Eastbound: required from 10, lifted at 20. Westbound: required from
		// 40 down to 30. Nothing covers 20..30.
		got := groupChainStretches(
			[]caltrans.ChainControlData{sign("E10", hwy, "Eastbound", "R2", 10), sign("W40", hwy, "Westbound", "R2", 40)},
			[]caltrans.ChainControlData{sign("E20", hwy, "Eastbound", "R0", 20), sign("W30", hwy, "Westbound", "R0", 30)},
		)
		require.Len(t, got, 2)
		assert.Equal(t, []string{"E10"}, ids(got[0]))
		assert.Equal(t, []string{"W40"}, ids(got[1]))
		assert.Equal(t, "cc:hwy4:r2", chainStretchID(got, 0))
		assert.Equal(t, "cc:hwy4:r2:2", chainStretchID(got, 1), "numbered along the road")
	})

	t.Run("the other direction's R-0 at the edge is the exit sign", func(t *testing.T) {
		got := groupChainStretches(
			[]caltrans.ChainControlData{sign("E10", hwy, "Eastbound", "R2", 10), sign("E20", hwy, "Eastbound", "R2", 20), sign("W30", hwy, "Westbound", "R2", 30)},
			[]caltrans.ChainControlData{sign("W10", hwy, "Westbound", "R0", 10.03), sign("E30", hwy, "Eastbound", "R0", 30.4)},
		)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"E10", "E20", "W30"}, ids(got[0]))
		assert.False(t, got[0].Inferred)
	})

	t.Run("a gate's R-0 is not a sign", func(t *testing.T) {
		gate := sign("GATE", hwy, "Eastbound", "R0", 15)
		gate.Gate = true
		active := []caltrans.ChainControlData{sign("E10", hwy, "Eastbound", "R2", 10), sign("W30", hwy, "Westbound", "R2", 30)}
		assert.Len(t, groupChainStretches(active, []caltrans.ChainControlData{gate, sign("W5", hwy, "Westbound", "R0", 5)}), 1)
		gate.Gate = false
		assert.Len(t, groupChainStretches(active, []caltrans.ChainControlData{gate, sign("W5", hwy, "Westbound", "R0", 5), sign("W20", hwy, "Westbound", "R0", 20)}), 2,
			"the same R-0 on a chain sign does end the eastbound requirement")
	})

	t.Run("levels are separate stretches, end to end", func(t *testing.T) {
		got := groupChainStretches([]caltrans.ChainControlData{
			sign("E10", hwy, "Eastbound", "R2", 10), sign("W20", hwy, "Westbound", "R2", 20), sign("W30", hwy, "Westbound", "R1", 30),
		}, nil)
		require.Len(t, got, 2)
		assert.Equal(t, "R1", got[0].Level)
		assert.Equal(t, []string{"W30"}, ids(got[0]))
		assert.Equal(t, []string{"E10", "W20"}, ids(got[1]))
	})

	t.Run("highways are never joined", func(t *testing.T) {
		got := groupChainStretches([]caltrans.ChainControlData{
			sign("A", "Highway 4", "Eastbound", "R2", 10), sign("B", "Highway 207", "Northbound", "R2", 10),
		}, nil)
		assert.Len(t, got, 2)
	})

	t.Run("the two signs of one checkpoint stand apart", func(t *testing.T) {
		// Westbound below, eastbound above, half a mile between them that
		// neither sign's span takes in.
		got := groupChainStretches(
			[]caltrans.ChainControlData{sign("W10", hwy, "Westbound", "R2", 10), sign("E10", hwy, "Eastbound", "R2", 10.56)},
			[]caltrans.ChainControlData{sign("W0", hwy, "Westbound", "R0", 5), sign("E20", hwy, "Eastbound", "R0", 20)},
		)
		assert.Len(t, got, 1)
	})

	t.Run("without mileposts signs join by distance, and say so", func(t *testing.T) {
		kml := func(id string, lng float64) caltrans.ChainControlData {
			return caltrans.ChainControlData{MessageID: id, District: "3", Source: caltrans.SourceQuickMap, Highway: "US 50",
				Direction: "Eastbound", Level: "R2", LocationName: id, Coordinates: &api.Coordinates{Latitude: 38.8, Longitude: lng}}
		}
		// 0.1 degrees of longitude here is 8.7 km; 0.3 is 26 km.
		got := groupChainStretches([]caltrans.ChainControlData{kml("far", -119.7), kml("b", -120.1), kml("a", -120.2), kml("c", -120.0)}, nil)
		require.Len(t, got, 2)
		assert.Equal(t, []string{"a", "b", "c"}, ids(got[0]), "west to east")
		assert.True(t, got[0].Inferred)
		assert.Equal(t, []string{"far"}, ids(got[1]))
		assert.False(t, got[1].Inferred, "a stretch of one sign infers nothing")
		assert.Equal(t, "cc:us50:r2", chainStretchID(got, 0))
		assert.Equal(t, "cc:us50:r2:2", chainStretchID(got, 1))
	})

	t.Run("a sign that cannot be joined is a stretch of its own", func(t *testing.T) {
		noHighway := sign("X", "", "Eastbound", "R2", 10)
		noDirection := sign("Y", hwy, "", "R3", 11)
		nowhere := caltrans.ChainControlData{MessageID: "Z", District: "10", Highway: hwy, Level: "R2", LocationName: "Z"}
		in := []caltrans.ChainControlData{noHighway, sign("E10", hwy, "Eastbound", "R2", 10), noDirection, nowhere}
		got := groupChainStretches(in, nil)
		require.Len(t, got, 4)
		total := 0
		for i, st := range got {
			total += len(st.Signs)
			assert.NotEmpty(t, chainStretchID(got, i))
		}
		assert.Equal(t, len(in), total, "nothing dropped")
		// No highway: no route to name, so the sign keeps its own id.
		for i, st := range got {
			if st.Highway == "" {
				assert.Equal(t, "cc:10-X", chainStretchID(got, i))
			}
		}
	})

	t.Run("the id outlives a sign", func(t *testing.T) {
		four := []caltrans.ChainControlData{sign("E10", hwy, "Eastbound", "R2", 10), sign("E20", hwy, "Eastbound", "R2", 20), sign("W20", hwy, "Westbound", "R2", 20), sign("W30", hwy, "Westbound", "R2", 30)}
		grown, shrunk := groupChainStretches(four, nil), groupChainStretches(four[1:], nil)
		require.Len(t, grown, 1)
		require.Len(t, shrunk, 1)
		assert.Equal(t, chainStretchID(grown, 0), chainStretchID(shrunk, 0), "the low end moved up a checkpoint")
		raised := append([]caltrans.ChainControlData(nil), four...)
		for i := range raised {
			raised[i].Level = "R3"
		}
		assert.Equal(t, "cc:hwy4:r3", chainStretchID(groupChainStretches(raised, nil), 0), "a new level is a new requirement")
	})

	t.Run("a sign reported twice is one sign", func(t *testing.T) {
		a := sign("9013", "US 50", "Westbound", "R2", 10)
		b := a
		b.MessageID = "9014"
		got := groupChainStretches([]caltrans.ChainControlData{a, b}, nil)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"9013"}, ids(got[0]))
	})
}

// Whatever the two feeds send, every sign that requires chains is in exactly
// one stretch: grouping changes how signs are presented, never whether.
func TestChainControlLayer_StormDropsNoSign(t *testing.T) {
	for name, cps := range map[string][]cwwp2.ChainControl{"both feeds": loadCheckpoints(t, stormCWWP2), "cc.kml alone": nil} {
		t.Run(name, func(t *testing.T) {
			s := chainService(kmlDoer{200, loadKML(t, stormKML)}, cps)
			if cps == nil {
				s = NewServiceWithAPIs(&config.Config{}, nil, nil, &caltrans.FeedParser{HTTPClient: kmlDoer{200, loadKML(t, stormKML)}}, nil)
			}
			controls, err := s.caltrans.ParseChainControlsDetailed(testCtx())
			require.NoError(t, err)
			want := map[string]bool{}
			for _, c := range controls {
				if isChainLevel(c.Level) && !c.Closed && !c.Unrecognized {
					want[chainControlID(c)] = true
				}
			}
			require.Len(t, want, 101, "29 in District 10, 72 in Districts 3 and 9")

			features, status, _, _, _, _ := s.BuildLayer(testCtx(), everywhere, LayerChainControl)
			assert.Equal(t, "OK", status)
			got := map[string]int{}
			for _, f := range features {
				for _, sg := range f.Properties.ChainControl.Signs {
					got[sg.ID]++
					assert.Equal(t, f.Properties.ChainControl.Level, sg.Level, "one level to a stretch")
				}
			}
			for id := range want {
				assert.Equal(t, 1, got[id], id)
			}
			assert.Len(t, got, len(want))
		})
	}
}

// The storm as the Ebbetts Pass area sees it. It was fifteen features (the
// fourteen signs inside the rectangle, one headline per highway repeated, and
// the Ebbetts gate); it is four stretches and the gate.
func TestChainControlLayer_StormEbbettsPass(t *testing.T) {
	s := chainService(kmlDoer{200, loadKML(t, stormKML)}, loadCheckpoints(t, stormCWWP2))
	features, status, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	assert.Equal(t, "OK", status)

	var got []string
	for _, f := range features {
		got = append(got, f.Properties.ID+" | "+f.Properties.Headline)
	}
	assert.Equal(t, []string{
		"cc:hwy4:r2 | Hwy 4 chains required, Poison Springs to Mount Reba Road (R2)",
		"cc:hwy88:r2 | Hwy 88 chains required, Lumber Yard to Red Lake Creek (R2)",
		"cc:hwy108:r2 | Hwy 108 chains required, Cascade Creek to Kennedy Meadows (R2)",
		"cc:hwy207:r2 | Hwy 207 chains required, Jct Hwy 4 to Mt. Reba Summit (R2)",
		"cc:10-2515 | Highway 4 road closed",
	}, got)

	hwy4 := features[0]
	p := hwy4.Properties
	assert.Equal(t, "r2", p.Category)
	assert.Equal(t, SevModerate, p.Severity)
	assert.Equal(t, "Poison Springs to Mount Reba Road", p.AreaLabel)
	assert.Equal(t, "2026-11-13T13:45:34-08:00", p.Effective, "the earliest sign: Mount Reba Road")
	assert.Contains(t, p.Description, "Chains or traction devices are required")
	assert.Equal(t, caltrans.SourceCWWP2, p.Source.Attribution)
	cc := p.ChainControl
	assert.Equal(t, "R2", cc.Level)
	assert.Equal(t, "Highway 4", cc.Highway)
	assert.Empty(t, cc.Direction, "signed both ways")
	assert.False(t, cc.Inferred)
	assert.Equal(t, &ChainPlace{Place: "Poison Springs", Coordinates: []float64{-120.18315, 38.38902}}, cc.From)
	assert.Equal(t, &ChainPlace{Place: "Mount Reba Road", Coordinates: []float64{-120.02152, 38.4816}}, cc.To)
	// A Point at the low end, where the requirement starts on the way up.
	assert.Equal(t, &Geometry{Type: "Point", Coordinates: []float64{-120.18315, 38.38902}}, hwy4.Geometry)
	require.Len(t, cc.Signs, 7)
	assert.Equal(t, ChainSign{
		ID: "cc:10-CAL-4-56.3-E-12E", Place: "POISON SPRINGS", Direction: "Eastbound", Level: "R2",
		Since: "2026-11-13T15:29:26-08:00", Coordinates: []float64{-120.18315, 38.38902},
		Source: caltrans.SourceCWWP2, County: "Calaveras", Postmile: "56.30",
	}, cc.Signs[0])
	assert.Equal(t, "cc:10-ALP-4-2.77-W-15W", cc.Signs[6].ID)

	// Hwy 108 runs out of the area's rectangle east of Eagle Meadows Road
	// (and only two of Hwy 88's eleven signs are inside it). The stretch is
	// still whole: the same feature wherever it is read.
	assert.Len(t, features[1].Properties.ChainControl.Signs, 11)
	hwy108 := features[2].Properties.ChainControl
	assert.Len(t, hwy108.Signs, 6)
	assert.False(t, ebbettsArea.Bounds.Contains(hwy108.To.Coordinates[1], hwy108.To.Coordinates[0]))

	// The gate is the feature it always was.
	gate := features[4]
	assert.Equal(t, "closed", gate.Properties.Category)
	assert.Equal(t, SevMinor, gate.Properties.Severity)
	assert.Equal(t, "Point", gate.Geometry.Type)
	assert.Nil(t, gate.Properties.ChainControl.Signs)
	assert.Nil(t, gate.Properties.ChainControl.From)
}

// Districts 3 and 9 reach us through cc.kml alone: active signs, no
// mileposts, no R-0. Their stretches are put together by distance and marked.
func TestChainControlLayer_StormKMLOnlyDistricts(t *testing.T) {
	s := chainService(kmlDoer{200, loadKML(t, stormKML)}, loadCheckpoints(t, stormCWWP2))
	features, _, _, _, _, _ := s.BuildLayer(testCtx(), everywhere, LayerChainControl)
	byID := map[string]Feature{}
	for _, f := range features {
		byID[f.Properties.ID] = f
	}

	// Hwy 89 over Luther Pass is one requirement in two districts: CWWP2's
	// two District 10 signs, and District 3's Luther Pass sign 280 m on.
	hwy89 := byID["cc:hwy89:r2"].Properties
	require.NotNil(t, hwy89.ChainControl)
	assert.True(t, hwy89.ChainControl.Inferred)
	assert.Equal(t, "Picketts Junction", hwy89.ChainControl.From.Place)
	assert.Equal(t, "cwwp2.dot.ca.gov · quickmap.dot.ca.gov", hwy89.Source.Attribution)
	var signs []string
	for _, sg := range hwy89.ChainControl.Signs {
		signs = append(signs, sg.ID)
	}
	require.GreaterOrEqual(t, len(signs), 4)
	assert.Equal(t, []string{"cc:10-ALP-89-21.6-N-02N", "cc:3-9110", "cc:10-ALP-89-23.9-S-002S", "cc:3-9109"}, signs[:4], "by distance from the south end")

	// US 395's three control areas are 14 km and more apart: three stretches.
	for _, id := range []string{"cc:us395:r1", "cc:us395:r1:2", "cc:us395:r1:3"} {
		assert.Len(t, byID[id].Properties.ChainControl.Signs, 3, id)
	}
	// District 9 pads its place names with the sign face's layout.
	assert.Equal(t, "Bodie Flats / 8 Mi. S of Bridgeport", byID["cc:us395:r1:2"].Properties.ChainControl.To.Place)
	// Hwy 49 changes level at Yuba Pass Summit, and is signed north/south
	// where it runs east: Bassetts is still its low end.
	assert.Equal(t, "Hwy 49 chains required, Bassetts to Yuba Pass Summit (R2)", byID["cc:hwy49:r2"].Properties.Headline)
	assert.Equal(t, "Hwy 49 chains or snow tires required, Yuba Pass Summit to Sierra/Plumas Co Line (R1)", byID["cc:hwy49:r1"].Properties.Headline)

	// Truck and hold levels stay one point each, as before: not stretch
	// members, no level, INFO.
	nyack := byID["cc:3-9031"]
	require.NotNil(t, nyack.Geometry, "I-80 Nyack, traffic held")
	assert.Equal(t, "Interstate 80 chain control", nyack.Properties.Headline)
	assert.Equal(t, SevInfo, nyack.Properties.Severity)
	assert.Nil(t, nyack.Properties.ChainControl.Signs)
	// cc.kml sends the Meyers hold twice (messages 9013 and 9014, alike in
	// everything else). One feature.
	_, first := byID["cc:3-9013"]
	_, second := byID["cc:3-9014"]
	assert.True(t, first != second, "exactly one of the pair")

	// The wire shape of a stretch: nothing here is new to an old reader but
	// chainControl.from, .to, .inferred and .signs.
	b, err := json.Marshal(byID["cc:hwy207:r2"])
	require.NoError(t, err)
	var wire struct {
		Geometry   struct{ Type string }
		Properties struct {
			ID, Kind, Category, Headline, AreaLabel string
			ChainControl                            map[string]any
		}
	}
	require.NoError(t, json.Unmarshal(b, &wire))
	assert.Equal(t, "Point", wire.Geometry.Type)
	assert.Equal(t, "Chain control", wire.Properties.Kind)
	assert.ElementsMatch(t, []string{"level", "highway", "from", "to", "signs"}, keys(wire.Properties.ChainControl))
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
