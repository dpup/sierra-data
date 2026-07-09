package gridapi

import (
	"github.com/dpup/sierra-data/internal/config"
)

// allScannerFeeds is every configured area's feeds deduped by feed_id,
// preserving area order then feed order (deterministic for ETags).
func (s *Service) allScannerFeeds() []config.ScannerFeed {
	seen := make(map[string]bool)
	var out []config.ScannerFeed
	for _, a := range s.Cfg.Hazards.Areas {
		for _, f := range a.ScannerFeeds {
			if seen[f.FeedID] {
				continue
			}
			seen[f.FeedID] = true
			out = append(out, f)
		}
	}
	return out
}

// areaByID finds a configured hazard area by id (== the seeded area place's
// slug).
func (s *Service) areaByID(id string) (config.HazardArea, bool) {
	for _, a := range s.Cfg.Hazards.Areas {
		if a.ID == id {
			return a, true
		}
	}
	return config.HazardArea{}, false
}
