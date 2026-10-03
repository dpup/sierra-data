package cwwp2

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures are live captures from 2026-09-30 (see tests/testdata/cwwp2/README.md);
// cc_d10_synthetic_storm.json is the only hand-edited one.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "testdata", "cwwp2", name))
	require.NoError(t, err)
	return b
}

// fixtureNow is just after the captures' newest record stamps.
var fixtureNow = time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)

type fakeDoer struct {
	status int
	body   []byte
	urls   []string
	err    error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.urls = append(f.urls, req.URL.String())
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(string(f.body)))}, nil
}

func pinnedClient(d HTTPDoer) *Client {
	c := NewClientWithHTTPDoer(d)
	c.Now = func() time.Time { return fixtureNow }
	return c
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"R-0": LevelNone, "R-1": LevelR1, "R-2": LevelR2, "R-3": LevelR3,
		"r2": LevelR2, " R-1 ": LevelR1,
		"": LevelUnknown, "R-4": LevelUnknown, "-118.1307759": LevelUnknown, "Closed": LevelUnknown,
	}
	for in, want := range cases {
		assert.Equal(t, want, ParseLevel(in), "ParseLevel(%q)", in)
	}
	assert.False(t, LevelNone.Active())
	assert.False(t, LevelUnknown.Active())
	assert.True(t, LevelR3.Active())
	assert.Equal(t, "R2", LevelR2.String())
}

// A quiet September day: every D10 checkpoint reports explicitly, all R-0.
func TestParseChainControls_LiveQuietDay(t *testing.T) {
	controls, err := ParseChainControls(fixture(t, "cc_d10_20260930.json"))
	require.NoError(t, err)
	require.Len(t, controls, 149)

	byRoute := map[string]int{}
	for _, c := range controls {
		assert.Equal(t, LevelNone, c.Level, c.ID)
		assert.True(t, c.InService, c.ID)
		assert.True(t, c.Location.HasPosition, c.ID)
		byRoute[c.Location.Route]++
	}
	// The corridors we serve are all covered checkpoint by checkpoint.
	for _, r := range []string{"SR-4", "SR-108", "SR-120", "SR-26", "SR-88", "SR-49"} {
		assert.Positive(t, byRoute[r], r)
	}

	bv := controls[0]
	assert.Equal(t, "10-ALP-4-0.65-W-14W", bv.ID)
	assert.Equal(t, "BEAR VALLEY", bv.Location.Name)
	assert.Equal(t, "Alpine", bv.Location.County)
	assert.Equal(t, "SR-4", bv.Location.Route)
	assert.Equal(t, "West", bv.Location.Direction)
	assert.Equal(t, "0.65", bv.Location.Postmile)
	assert.InDelta(t, 38.4605, bv.Location.Latitude, 1e-6)
	assert.InDelta(t, -120.0448, bv.Location.Longitude, 1e-6)
	assert.InDelta(t, 7116, bv.Location.ElevationFt, 0)
	assert.Equal(t, "R-0", bv.RawStatus)
	assert.Equal(t, "No chain controls are in effect at this time.", bv.Description)
	// Pacific local strings: 13:01:03 PDT is 20:01:03 UTC.
	assert.Equal(t, time.Date(2026, 5, 5, 20, 1, 3, 0, time.UTC), bv.StatusSince.UTC())
	assert.Equal(t, time.Date(2026, 9, 30, 3, 57, 3, 0, time.UTC), bv.RecordedAt.UTC())
}

func TestParseChainControls_SyntheticStorm(t *testing.T) {
	controls, err := ParseChainControls(fixture(t, "cc_d10_synthetic_storm.json"))
	require.NoError(t, err)
	levels := map[Level]int{}
	for _, c := range controls {
		levels[c.Level]++
	}
	assert.Equal(t, 6, levels[LevelR2])
	assert.Equal(t, 1, levels[LevelR1])
	assert.Equal(t, 142, levels[LevelNone])
}

// D7 carried a longitude in the status field. It must parse as Unknown, never
// as R-0, and keep the raw value for diagnosis.
func TestParseChainControls_GarbageStatusIsUnknown(t *testing.T) {
	controls, err := ParseChainControls(fixture(t, "cc_d07_bad_status_20260930.json"))
	require.NoError(t, err)
	var unknown []ChainControl
	for _, c := range controls {
		if c.Level == LevelUnknown {
			unknown = append(unknown, c)
		}
	}
	require.Len(t, unknown, 2)
	assert.Equal(t, "-118.1307759", unknown[0].RawStatus)
	assert.Equal(t, "SR-2", unknown[0].Location.Route)
}

func TestParseChainControls_EmptyAndMalformed(t *testing.T) {
	_, err := ParseChainControls([]byte(`{"data": []}`))
	assert.ErrorIs(t, err, ErrEmptyFeed)

	// The rwis feed's real defect: a missing comma between repeated entries.
	_, err = ParseChainControls([]byte(`{"data": [{"cc": {"index": "a"}} {"cc": {"index": "b"}}]}`))
	assert.Error(t, err)
}

func TestClient_ChainControls(t *testing.T) {
	d := &fakeDoer{status: 200, body: fixture(t, "cc_d10_20260930.json")}
	controls, err := pinnedClient(d).ChainControls(context.Background(), 10)
	require.NoError(t, err)
	assert.Len(t, controls, 149)
	assert.Equal(t, []string{"https://cwwp2.dot.ca.gov/data/d10/cc/ccStatusD10.json"}, d.urls)
}

func TestClient_FeedURLPadsFileNameOnly(t *testing.T) {
	c := NewClient()
	assert.Equal(t, "https://cwwp2.dot.ca.gov/data/d3/cc/ccStatusD03.json", c.FeedURL(3, "cc", "cc"))
	assert.Equal(t, "https://cwwp2.dot.ca.gov/data/d10/lcs/lcsStatusD10.json", c.FeedURL(10, "lcs", "lcs"))
}

func TestClient_ChainControlsFailLoud(t *testing.T) {
	ctx := context.Background()

	_, err := pinnedClient(&fakeDoer{status: 500}).ChainControls(ctx, 4)
	assert.ErrorContains(t, err, "HTTP 500")

	_, err = pinnedClient(&fakeDoer{err: errors.New("dial tcp: timeout")}).ChainControls(ctx, 10)
	assert.Error(t, err)

	_, err = pinnedClient(&fakeDoer{status: 200, body: []byte(`{"data": []}`)}).ChainControls(ctx, 10)
	assert.ErrorIs(t, err, ErrEmptyFeed)

	// A file still served but no longer regenerated reads as all-R-0 forever.
	c := pinnedClient(&fakeDoer{status: 200, body: fixture(t, "cc_d10_20260930.json")})
	c.Now = func() time.Time { return fixtureNow.Add(3 * time.Hour) }
	_, err = c.ChainControls(ctx, 10)
	assert.ErrorIs(t, err, ErrStaleFeed)

	// Stamps that no longer parse must not switch the freshness check off.
	unstamped := []byte(`{"data": [{"cc": {"index": "x", "recordTimestamp": {"recordDate": "09/29/2026", "recordTime": "8:57pm"}, "statusData": {"status": "R-0"}}}]}`)
	_, err = pinnedClient(&fakeDoer{status: 200, body: unstamped}).ChainControls(ctx, 10)
	assert.ErrorIs(t, err, ErrNoRecordTime)

	c.StaleAfter = 0 // disabled
	_, err = c.ChainControls(ctx, 10)
	assert.NoError(t, err)
}

func findClosure(t *testing.T, all []LaneClosure, id string) LaneClosure {
	t.Helper()
	for _, lc := range all {
		if lc.ID == id {
			return lc
		}
	}
	t.Fatalf("closure %s not in fixture", id)
	return LaneClosure{}
}

func TestParseLaneClosures_Live(t *testing.T) {
	all, err := ParseLaneClosures(fixture(t, "lcs_d10_20260930.json"))
	require.NoError(t, err)
	require.Len(t, all, 84)

	lc := findClosure(t, all, "C4QB-0004-2026-10-02-07:01:00")
	assert.Equal(t, "C4QB", lc.ClosureID)
	assert.Equal(t, "4", lc.LogNumber)
	assert.Equal(t, "East / West", lc.FlowDirection)
	assert.Equal(t, "Avery", lc.Begin.Name)
	assert.Equal(t, "Calaveras", lc.Begin.County)
	assert.Equal(t, "SR-4", lc.Begin.Route)
	assert.Equal(t, "37.500", lc.Begin.Postmile)
	assert.InDelta(t, 38.205083, lc.Begin.Latitude, 1e-6)
	assert.InDelta(t, 3405, lc.Begin.ElevationFt, 0)
	assert.Equal(t, "Lane", lc.ClosureType)
	assert.Equal(t, "Drainage Work", lc.WorkType)
	assert.Equal(t, "RShoulder", lc.LanesClosed)
	assert.Equal(t, 2, lc.TotalLanes)
	assert.Equal(t, "Conventional Hwy", lc.Facility)
	assert.False(t, lc.EndIndefinite)
	// Epochs are authoritative: 07:01 PDT = 14:01 UTC.
	assert.Equal(t, time.Date(2026, 10, 2, 14, 1, 0, 0, time.UTC), lc.Start.UTC())
	assert.Equal(t, time.Date(2026, 10, 2, 21, 59, 0, 0, time.UTC), lc.EndTime.UTC())
	assert.False(t, lc.SetUp || lc.PickedUp || lc.Cancelled)
	assert.Equal(t, PhaseScheduled, lc.PhaseAt(fixtureNow))
}

// Every lifecycle the live capture holds, at capture time.
func TestLaneClosurePhases_Live(t *testing.T) {
	all, err := ParseLaneClosures(fixture(t, "lcs_d10_20260930.json"))
	require.NoError(t, err)

	counts := map[Phase]int{}
	for _, lc := range all {
		counts[lc.PhaseAt(fixtureNow)]++
	}
	assert.Equal(t, map[Phase]int{PhaseScheduled: 57, PhaseCompleted: 21, PhaseCancelled: 3, PhaseActive: 3}, counts)

	cases := map[string]Phase{
		"C4QB-0004-2026-10-02-07:01:00":  PhaseScheduled, // future window
		"C26EA-0001-2026-08-24-07:01:00": PhaseActive,    // 10-97, long-term full closure on SR-26
		"P88AA-0004-2026-09-29-08:01:00": PhaseActive,    // 10-97 with the window past: an overrun, still in place
		"C4JA-0002-2026-09-29-08:01:00":  PhaseCompleted, // window over
		"C88UA-0001-2026-08-31-08:01:00": PhaseCompleted, // 10-98
		"C4JA-0003-2026-09-30-08:01:00":  PhaseCancelled, // 10-22 before it started
	}
	for id, want := range cases {
		assert.Equal(t, want, findClosure(t, all, id).PhaseAt(fixtureNow), id)
	}

	active := findClosure(t, all, "C26EA-0001-2026-08-24-07:01:00")
	assert.Equal(t, "Full", active.ClosureType)
	assert.Equal(t, time.Date(2026, 8, 24, 14, 58, 1, 0, time.UTC), active.SetUpAt.UTC())
}

func TestLaneClosurePhaseAt(t *testing.T) {
	start := time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	base := LaneClosure{Start: start, EndTime: end}
	before, during, after := start.Add(-time.Hour), start.Add(time.Hour), end.Add(time.Hour)

	assert.Equal(t, PhaseScheduled, base.PhaseAt(before))
	assert.Equal(t, PhaseScheduled, base.PhaseAt(during), "window open but no 10-97 yet")
	assert.Equal(t, PhaseCompleted, base.PhaseAt(after))

	set := base
	set.SetUp = true
	assert.Equal(t, PhaseActive, set.PhaseAt(during))
	assert.Equal(t, PhaseActive, set.PhaseAt(after), "overrun: codes win over the clock")

	set.PickedUp = true
	assert.Equal(t, PhaseCompleted, set.PhaseAt(during))

	cancelled := base
	cancelled.Cancelled = true
	assert.Equal(t, PhaseCancelled, cancelled.PhaseAt(before))

	open := LaneClosure{Start: start, EndIndefinite: true}
	assert.Equal(t, PhaseScheduled, open.PhaseAt(after.Add(1000*time.Hour)))
}

func TestClient_LaneClosures(t *testing.T) {
	d := &fakeDoer{status: 200, body: fixture(t, "lcs_d10_20260930.json")}
	all, err := pinnedClient(d).LaneClosures(context.Background(), 10)
	require.NoError(t, err)
	assert.Len(t, all, 84)
	assert.Equal(t, []string{"https://cwwp2.dot.ca.gov/data/d10/lcs/lcsStatusD10.json"}, d.urls)
	for _, lc := range all {
		require.Equal(t, 10, lc.District, lc.ID)
		require.Empty(t, lc.Unrecognized, lc.ID)
	}
}

// The fail-loud half: everything that would otherwise reach the grid's
// disappearance sweep as "these closures ended".
func TestClient_LaneClosuresFailLoud(t *testing.T) {
	ctx := context.Background()

	// An empty file is broken, not quiet: it has no stamp to check freshness
	// against, and D10 has never published fewer than hundreds of windows.
	_, err := pinnedClient(&fakeDoer{status: 200, body: []byte(`{"data": []}`)}).LaneClosures(ctx, 10)
	assert.ErrorIs(t, err, ErrEmptyFeed)

	// A file the portal stopped regenerating.
	c := pinnedClient(&fakeDoer{status: 200, body: fixture(t, "lcs_d10_20260930.json")})
	c.Now = func() time.Time { return fixtureNow.Add(3 * time.Hour) }
	_, err = c.LaneClosures(ctx, 10)
	assert.ErrorIs(t, err, ErrStaleFeed)

	_, err = pinnedClient(&fakeDoer{status: 500}).LaneClosures(ctx, 10)
	assert.Error(t, err)
}

// A code block whose shape drifts must not decode as "not called".
func TestParseLaneClosures_UnrecognizedRows(t *testing.T) {
	body := fixture(t, "lcs_d10_20260930.json")
	all, err := ParseLaneClosures(body)
	require.NoError(t, err)
	for _, lc := range all {
		require.Empty(t, lc.Unrecognized, "live row %s", lc.ID)
	}

	// mutate rewrites the first occurrence and returns the row it landed in:
	// the only row the rewrite can have changed.
	mutate := func(old, new string) LaneClosure {
		t.Helper()
		require.Contains(t, string(body), old)
		rows, err := ParseLaneClosures([]byte(strings.Replace(string(body), old, new, 1)))
		require.NoError(t, err)
		require.Len(t, rows, len(all))
		for i := range rows {
			if rows[i].Unrecognized != "" || rows[i].SetUp != all[i].SetUp ||
				rows[i].PickedUp != all[i].PickedUp || rows[i].Cancelled != all[i].Cancelled {
				return rows[i]
			}
		}
		return LaneClosure{}
	}
	cases := map[string]LaneClosure{
		"unreadable 10-97 flag": mutate(`"isCode1097": "false"`, `"isCode1097": "no"`),
		"unreadable 10-98 flag": mutate(`"isCode1098": "false"`, `"code1098Is": "false"`),
		"unreadable 10-22 flag": mutate(`"isCode1022": "false"`, `"isCode1022": ""`),
	}
	for want, row := range cases {
		assert.Equal(t, want, row.Unrecognized)
	}
	assert.Empty(t, mutate(`"isCode1097": "false"`, `"isCode1097": "False"`).Unrecognized, "case is not drift")
}

func TestUnrecognizedReason(t *testing.T) {
	ok := [3]bool{true, true, true}
	at := Location{Latitude: 38.2, Longitude: -120.4, HasPosition: true}
	good := LaneClosure{ID: "C4QB-0004-2026-10-02-07:01:00", Start: fixtureNow, EndTime: fixtureNow.Add(time.Hour), Begin: at}
	assert.Empty(t, unrecognizedReason(good, ok))

	endOnly := good
	endOnly.Begin, endOnly.End = Location{}, at
	assert.Empty(t, unrecognizedReason(endOnly, ok), "one positioned endpoint is enough")

	open := good
	open.EndTime, open.EndIndefinite = time.Time{}, true
	assert.Empty(t, unrecognizedReason(open, ok), "an indefinite window has no end")

	cases := map[string]func(*LaneClosure){
		"no index":      func(l *LaneClosure) { l.ID = "" },
		"no start time": func(l *LaneClosure) { l.Start = time.Time{} },
		"no end time":   func(l *LaneClosure) { l.EndTime = time.Time{} },
		"no position":   func(l *LaneClosure) { l.Begin = Location{} },
	}
	for want, edit := range cases {
		lc := good
		edit(&lc)
		assert.Equal(t, want, unrecognizedReason(lc, ok))
	}
	assert.Equal(t, "unreadable 10-98 flag", unrecognizedReason(good, [3]bool{true, false, true}))
}
