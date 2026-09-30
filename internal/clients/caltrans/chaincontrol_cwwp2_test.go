package caltrans

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

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedDoer answers every request with one status and body.
type fixedDoer struct {
	status int
	body   []byte
}

func (f fixedDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(string(f.body)))}, nil
}

func cwwp2Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "testdata", "cwwp2", name))
	require.NoError(t, err)
	return b
}

// cwwp2Client serves a CWWP2 fixture with the clock pinned to its capture.
func cwwp2Client(d cwwp2.HTTPDoer) *cwwp2.Client {
	c := cwwp2.NewClientWithHTTPDoer(d)
	c.Now = func() time.Time { return time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC) }
	return c
}

// kmlOnlyEntries is how many cc.kml entries carry no R-level (road closures,
// MAX/MIN/TS truck levels) — the supplement CWWP2 mode keeps.
func kmlOnlyEntries(t *testing.T) int {
	t.Helper()
	all, err := setupTestParser(t).ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	n := 0
	for _, c := range all {
		if c.Level == "" {
			n++
		}
	}
	require.Positive(t, n, "fixture should hold road-closed / truck entries")
	return n
}

// Quiet day: 149 checkpoints all R-0 contribute nothing, and the KML
// supplement still carries its closures.
func TestChainControlsCWWP2_QuietDay(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_20260930.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)
	assert.Len(t, controls, kmlOnlyEntries(t))
	for _, c := range controls {
		assert.Empty(t, c.Level, "cc.kml R-levels must not leak through: %+v", c)
		assert.Equal(t, SourceQuickMap, c.Source)
	}
}

func TestChainControlsCWWP2_Storm(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.NoError(t, err)

	var fromCWWP2 []ChainControlData
	for _, c := range controls {
		if c.Source == SourceCWWP2 {
			fromCWWP2 = append(fromCWWP2, c)
		}
	}
	require.Len(t, fromCWWP2, 7)
	assert.Len(t, controls, 7+kmlOnlyEntries(t))

	var arnold *ChainControlData
	for i := range fromCWWP2 {
		if fromCWWP2[i].LocationName == "ARNOLD" && fromCWWP2[i].Direction == "Eastbound" {
			arnold = &fromCWWP2[i]
		}
	}
	require.NotNil(t, arnold)
	assert.Equal(t, "Highway 4", arnold.Highway)
	assert.Equal(t, "R2", arnold.Level)
	assert.Equal(t, "10", arnold.District)
	assert.True(t, strings.HasPrefix(arnold.MessageID, "10-CAL-4-"), arnold.MessageID)
	assert.Contains(t, arnold.Description, "traction devices")
	assert.Equal(t, "2026-09-29T18:05:00-07:00", arnold.EffectiveTime)
	require.NotNil(t, arnold.Coordinates)
	assert.False(t, arnold.Unrecognized)
}

// A CWWP2 failure is a hard error: serving cc.kml's closures alone would read
// as "no chain controls" over the whole district.
func TestChainControlsCWWP2_SourceDownIsHardError(t *testing.T) {
	p := setupTestParser(t)
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{status: 500}), []int{10})
	controls, err := p.ParseChainControlsDetailed(context.Background())
	require.Error(t, err)
	var partial *PartialError
	assert.False(t, errors.As(err, &partial))
	assert.Nil(t, controls)

	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, []byte(`{"data": []}`)}), []int{10})
	_, err = p.ParseChainControlsDetailed(context.Background())
	assert.ErrorIs(t, err, cwwp2.ErrEmptyFeed)
}

// cc.kml down while CWWP2 is healthy: the levels are still good, so they come
// back with a PartialError rather than nothing.
func TestChainControlsCWWP2_KMLDownIsPartial(t *testing.T) {
	p := &FeedParser{HTTPClient: fixedDoer{status: 503}}
	p.UseCWWP2ChainControls(cwwp2Client(fixedDoer{200, cwwp2Fixture(t, "cc_d10_synthetic_storm.json")}), []int{10})

	controls, err := p.ParseChainControlsDetailed(context.Background())
	var partial *PartialError
	require.ErrorAs(t, err, &partial)
	assert.Len(t, controls, 7)
}

func TestChainControlFromCWWP2_Unrecognized(t *testing.T) {
	all, err := cwwp2.ParseChainControls(cwwp2Fixture(t, "cc_d07_bad_status_20260930.json"))
	require.NoError(t, err)
	var got []ChainControlData
	for _, cp := range all {
		if cp.Level != cwwp2.LevelNone {
			got = append(got, chainControlFromCWWP2(cp))
		}
	}
	require.Len(t, got, 2)
	for _, c := range got {
		assert.True(t, c.Unrecognized)
		assert.Empty(t, c.Level, "an unreadable status must not become a level")
		assert.Equal(t, "Highway 2", c.Highway)
		assert.Contains(t, c.Description, "unrecognized")
		assert.NotEmpty(t, c.RawStatus)
	}
}

func TestHighwayAndDirectionLabels(t *testing.T) {
	for in, want := range map[string]string{"SR-4": "Highway 4", "SR-108": "Highway 108", "US-50": "US 50", "I-80": "I-80", "": ""} {
		assert.Equal(t, want, highwayLabel(in), in)
	}
	for in, want := range map[string]string{"East": "Eastbound", "west": "Westbound", "North": "Northbound", "South": "Southbound", "": ""} {
		assert.Equal(t, want, directionLabel(in), in)
	}
}
