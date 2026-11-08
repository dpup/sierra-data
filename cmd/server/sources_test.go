package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The caltrans row credits the feeds actually behind it, which config decides.
// It is also the chain_control layer's metadata.attribution.
func TestCaltransAttribution(t *testing.T) {
	cfg := &config.Config{}
	if got := caltransAttribution(cfg); got != "quickmap.dot.ca.gov" {
		t.Errorf("KML only: got %q", got)
	}
	cfg.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts = []int{3, 10}
	if got := caltransAttribution(cfg); got != "cwwp2.dot.ca.gov · quickmap.dot.ca.gov" {
		t.Errorf("CWWP2 lane closures: got %q", got)
	}
	cfg.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts = nil
	cfg.Roads.CaltransFeeds.CWWP2.ChainControlDistricts = []int{10}
	if got := caltransAttribution(cfg); got != "cwwp2.dot.ca.gov · quickmap.dot.ca.gov" {
		t.Errorf("CWWP2 chain controls (merged with cc.kml): got %q", got)
	}

	cfg.Grid.Sources = map[string]config.SourceTuning{"caltrans": {}}
	seeds := gridSourceSeeds(cfg)
	if len(seeds) != 1 || seeds[0].Attribution != "cwwp2.dot.ca.gov · quickmap.dot.ca.gov" {
		t.Errorf("seeded caltrans row: %+v", seeds)
	}
}

// Reporter rows are marked REPORTER and state the cadence of the poller that
// re-checks them: the mesh tick for mesh streams, the burn-status poller's for
// burn.line. Stating the mesh tick for burn-line told clients its row was
// re-checked every minute when it is re-checked hourly.
func TestGridSourceSeeds_Reporters(t *testing.T) {
	cfg := &config.Config{}
	cfg.Grid.Sources = map[string]config.SourceTuning{
		"meshcore": {PollInterval: time.Minute},
		"burnline": {PollInterval: time.Hour},
		"calfire":  {PollInterval: 5 * time.Minute},
	}
	cfg.Grid.Ingest.Reporters = []config.Reporter{
		{ID: "alanpi", Streams: []string{pushingest.MeshStream}, StaleAfter: time.Hour},
		{ID: "burn-line", Streams: []string{pushingest.BurnStream}, StaleAfter: 30 * time.Hour},
		{ID: "both", Streams: []string{pushingest.BurnStream, pushingest.MeshPacketStream}},
	}

	byID := map[string]store.SourceSeed{}
	for _, seed := range gridSourceSeeds(cfg) {
		byID[seed.ID] = seed
	}
	assert.Equal(t, gridv1.SourceKind_SOURCE_KIND_UNSPECIFIED, byID["calfire"].Kind, "feeds seed as the store's default, FEED")
	for id, want := range map[string]time.Duration{"alanpi": time.Minute, "burn-line": time.Hour, "both": time.Minute} {
		assert.Equal(t, gridv1.SourceKind_REPORTER, byID[id].Kind, id)
		assert.Equal(t, want, byID[id].PollInterval, id)
	}
	assert.Equal(t, 30*time.Hour, byID["burn-line"].StaleAfter)
}
