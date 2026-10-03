package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/alerts"
)

// lcsFixtureNow is just after the D10 capture's record stamp, so the
// fixture's phases are the live ones: 3 ACTIVE windows (C26EA, C49GA,
// P88AA) among 57 SCHEDULED, 21 COMPLETED and 3 CANCELLED.
var lcsFixtureNow = time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)

func d10LaneClosures(t *testing.T) []cwwp2.LaneClosure {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "cwwp2", "lcs_d10_20260930.json"))
	require.NoError(t, err)
	rows, err := cwwp2.ParseLaneClosures(b)
	require.NoError(t, err)
	for i := range rows {
		rows[i].District = 10
	}
	return rows
}

type fakeLaneClosureFetcher struct {
	mu    sync.Mutex
	rows  map[int][]cwwp2.LaneClosure
	errs  map[int]error
	calls []int
}

func (f *fakeLaneClosureFetcher) LaneClosures(_ context.Context, district int) ([]cwwp2.LaneClosure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, district)
	if err := f.errs[district]; err != nil {
		return nil, err
	}
	return f.rows[district], nil
}

// roadFeedDoer serves an empty chp-only.kml and fails everything else
// (cc.kml, roads.dot.ca.gov — both tolerated by refreshRoadData). It records
// whether lcs2way.kml was requested.
type roadFeedDoer struct {
	mu       sync.Mutex
	laneHits int
	laneBody string
}

func (d *roadFeedDoer) Do(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	switch {
	case strings.Contains(u, "chp-only"):
		return kmlResponse(`<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document></Document></kml>`), nil
	case strings.Contains(u, "lcs2way"):
		d.mu.Lock()
		d.laneHits++
		d.mu.Unlock()
		if d.laneBody == "" {
			return nil, errors.New("lcs2way.kml should not be read")
		}
		return kmlResponse(d.laneBody), nil
	}
	return nil, fmt.Errorf("simulated outage: %s", u)
}

// statusEnhancer stands in for OpenAI: a full closure reads "closed", any
// other closure "restricted".
type statusEnhancer struct {
	mu     sync.Mutex
	titles []string
}

func (e *statusEnhancer) EnhanceAlert(_ context.Context, raw alerts.RawAlert) (alerts.EnhancedAlert, error) {
	e.mu.Lock()
	e.titles = append(e.titles, raw.Title)
	e.mu.Unlock()
	status := "restricted"
	if strings.Contains(raw.Description, "Full closure") {
		status = "closed"
	}
	return alerts.EnhancedAlert{StructuredDescription: alerts.StructuredDescription{
		Details:            raw.Description,
		Impact:             "severe",
		RoadStatus:         status,
		RestrictionDetails: raw.Title,
	}}, nil
}

func (e *statusEnhancer) HealthCheck(context.Context) error { return nil }

// sr26Road runs along SR-26 out of Mokelumne Hill, starting on the C26EA
// full closure's begin point.
func sr26Road() config.MonitoredRoad {
	return config.MonitoredRoad{
		ID: "sr26-mokelumne-hill", Name: "Hwy 26", Section: "Mokelumne Hill to Glencoe",
		Origin:      config.Coordinates{Latitude: 38.297818, Longitude: -120.704668},
		Destination: config.Coordinates{Latitude: 38.33, Longitude: -120.60},
	}
}

func newLaneClosureRoadsService(districts []int, fetcher LaneClosureFetcher, doer *roadFeedDoer, enh alerts.AlertEnhancer) (*RoadsService, *cache.Cache) {
	parser := caltrans.NewFeedParser()
	parser.HTTPClient = doer
	c := cache.NewCache()
	cfg := &config.Config{Roads: config.RoadsConfig{
		RefreshInterval: 15 * time.Minute,
		MonitoredRoads:  []config.MonitoredRoad{sr26Road()},
		CaltransFeeds: config.CaltransConfig{
			CWWP2: config.CWWP2Config{LaneClosureDistricts: districts},
		},
	}}
	s := NewRoadsService(nil, parser, c, cfg, enh)
	if fetcher != nil {
		s.UseCWWP2LaneClosures(fetcher)
	}
	s.now = func() time.Time { return lcsFixtureNow }
	return s, c
}

func closureNames(incs []caltrans.CaltransIncident) []string {
	var out []string
	for _, inc := range incs {
		out = append(out, inc.Name)
	}
	sort.Strings(out)
	return out
}

// With laneClosureDistricts set, segment status reads the CWWP2 windows that
// are set up right now, and never lcs2way.kml or a SCHEDULED window.
func TestSegmentLaneClosures_CWWP2ActiveOnly(t *testing.T) {
	fetcher := &fakeLaneClosureFetcher{rows: map[int][]cwwp2.LaneClosure{10: d10LaneClosures(t)}}
	doer := &roadFeedDoer{}
	s, _ := newLaneClosureRoadsService([]int{10}, fetcher, doer, nil)

	got, err := s.segmentLaneClosures(testCtx())
	require.NoError(t, err)
	assert.Equal(t, []string{"Route 26 Full Closure", "Route 49 Lane Closure", "Route 88 One-way Traffic Operation"},
		closureNames(got), "the 3 ACTIVE windows; the 57 SCHEDULED ones are not closures yet")
	assert.Equal(t, []int{10}, fetcher.calls)
	assert.Zero(t, doer.laneHits, "lcs2way.kml must not be read")
}

func TestSegmentLaneClosures_SkipsUnrecognizedRows(t *testing.T) {
	rows := d10LaneClosures(t)
	for i := range rows {
		if rows[i].ClosureID == "C26EA" {
			rows[i].Unrecognized = "unreadable 10-97 flag"
		}
	}
	fetcher := &fakeLaneClosureFetcher{rows: map[int][]cwwp2.LaneClosure{10: rows}}
	s, _ := newLaneClosureRoadsService([]int{10}, fetcher, &roadFeedDoer{}, nil)

	got, err := s.segmentLaneClosures(testCtx())
	require.NoError(t, err)
	assert.NotContains(t, closureNames(got), "Route 26 Full Closure", "its phase is unknown: not guessed")
}

// A CWWP2 failure must never read as "no closures" (→ OPEN). The last good
// set is served while it is servable-stale (< 2x the refresh interval); past
// that, and with nothing cached, it is an error.
func TestSegmentLaneClosures_FailLoud(t *testing.T) {
	t.Run("no cache: error", func(t *testing.T) {
		fetcher := &fakeLaneClosureFetcher{errs: map[int]error{10: cwwp2.ErrStaleFeed}}
		s, _ := newLaneClosureRoadsService([]int{10}, fetcher, &roadFeedDoer{}, nil)
		_, err := s.segmentLaneClosures(testCtx())
		require.Error(t, err)
		assert.ErrorIs(t, err, cwwp2.ErrStaleFeed)
	})

	t.Run("one district down fails the set", func(t *testing.T) {
		fetcher := &fakeLaneClosureFetcher{
			rows: map[int][]cwwp2.LaneClosure{10: d10LaneClosures(t)},
			errs: map[int]error{3: cwwp2.ErrEmptyFeed},
		}
		s, _ := newLaneClosureRoadsService([]int{3, 10}, fetcher, &roadFeedDoer{}, nil)
		_, err := s.segmentLaneClosures(testCtx())
		assert.ErrorIs(t, err, cwwp2.ErrEmptyFeed, "a missing district would read as no closures in its footprint")
	})

	t.Run("stale fallback, then very stale", func(t *testing.T) {
		fetcher := &fakeLaneClosureFetcher{rows: map[int][]cwwp2.LaneClosure{10: d10LaneClosures(t)}}
		s, c := newLaneClosureRoadsService([]int{10}, fetcher, &roadFeedDoer{}, nil)
		_, err := s.segmentLaneClosures(testCtx())
		require.NoError(t, err)

		fetcher.errs = map[int]error{10: errors.New("portal down")}
		c.Backdate(cwwp2ActiveClosuresKey, 20*time.Minute) // stale, not very stale
		got, err := s.segmentLaneClosures(testCtx())
		require.NoError(t, err)
		assert.Len(t, got, 3, "the last good set")

		c.Backdate(cwwp2ActiveClosuresKey, 15*time.Minute) // 35m > 2x15m
		_, err = s.segmentLaneClosures(testCtx())
		assert.Error(t, err)
	})

	t.Run("no fetcher wired", func(t *testing.T) {
		s, _ := newLaneClosureRoadsService([]int{10}, nil, &roadFeedDoer{}, nil)
		_, err := s.segmentLaneClosures(testCtx())
		assert.Error(t, err)
	})
}

// Empty laneClosureDistricts keeps the lcs2way.kml path, errors swallowed as
// before.
func TestSegmentLaneClosures_KMLFallbackWhenUnconfigured(t *testing.T) {
	fetcher := &fakeLaneClosureFetcher{}
	doer := &roadFeedDoer{laneBody: laneFeedKML}
	s, _ := newLaneClosureRoadsService(nil, fetcher, doer, nil)

	got, err := s.segmentLaneClosures(testCtx())
	require.NoError(t, err)
	assert.Equal(t, []string{"Route 4 One-way Traffic Operation"}, closureNames(got))
	assert.Equal(t, 1, doer.laneHits)
	assert.Empty(t, fetcher.calls, "CWWP2 is not read for segment status when unconfigured")

	doer.laneBody = "" // now fails
	got, err = s.segmentLaneClosures(testCtx())
	require.NoError(t, err, "the KML path has always tolerated a failed fetch")
	assert.Empty(t, got)
}

// End to end: a set-up CWWP2 full closure on the segment closes it, through
// the same AI status path lcs2way.kml closures took.
func TestRefreshRoadData_CWWP2ClosureDrivesSegmentStatus(t *testing.T) {
	fetcher := &fakeLaneClosureFetcher{rows: map[int][]cwwp2.LaneClosure{10: d10LaneClosures(t)}}
	doer := &roadFeedDoer{}
	enh := &statusEnhancer{}
	s, _ := newLaneClosureRoadsService([]int{10}, fetcher, doer, enh)

	roads, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)
	require.Len(t, roads, 1)
	assert.Equal(t, api.RoadStatus_CLOSED, roads[0].Status)
	assert.Equal(t, "Route 26 Full Closure", roads[0].StatusExplanation)
	assert.Zero(t, doer.laneHits)
	var ids []string
	for _, a := range roads[0].Alerts {
		ids = append(ids, a.Id)
	}
	assert.Contains(t, ids, "C26EA", "alert id from the Closure ID, as with lcs2way.kml")
}

func TestRefreshRoadData_CWWP2FailureIsAnError(t *testing.T) {
	fetcher := &fakeLaneClosureFetcher{errs: map[int]error{10: errors.New("portal down")}}
	s, _ := newLaneClosureRoadsService([]int{10}, fetcher, &roadFeedDoer{}, &statusEnhancer{})

	_, err := s.refreshRoadData(testCtx())
	assert.Error(t, err, "an unknown closure state must not publish the segment as OPEN")
}
