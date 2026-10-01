package main

import (
	"testing"

	"github.com/dpup/sierra-data/internal/config"
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
