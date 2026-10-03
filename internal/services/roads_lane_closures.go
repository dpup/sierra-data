package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// LaneClosureFetcher is the slice of *cwwp2.Client segment status uses.
type LaneClosureFetcher interface {
	LaneClosures(ctx context.Context, district int) ([]cwwp2.LaneClosure, error)
}

// cwwp2ActiveClosuresKey caches the last good set of ACTIVE CWWP2 windows,
// the stale fallback when the portal fails.
const cwwp2ActiveClosuresKey = "cwwp2_lane_closures:active"

// UseCWWP2LaneClosures wires the CWWP2 client segment status reads lane
// closures from when roads.caltransFeeds.cwwp2.laneClosureDistricts is set.
// cmd/server passes the same client the lane-closure poller uses.
func (s *RoadsService) UseCWWP2LaneClosures(src LaneClosureFetcher) {
	s.laneClosureSource = src
}

func (s *RoadsService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// segmentLaneClosures returns the lane closures that feed per-road status.
//
// With laneClosureDistricts empty it is lcs2way.kml, failures swallowed, as
// it always was. With them set it is the CWWP2 windows that are set up right
// now (PhaseActive), the same rows the road_incident layer shows as ACTIVE,
// so a segment's status and the closure events agree. SCHEDULED windows are
// not closures yet and never count.
//
// Fail-loud, unlike the KML path: a district failing fails the whole set (a
// missing district would read as "no closures" across its footprint, and so
// OPEN). The last good set is served while servable-stale (< 2x the roads
// refresh interval, the caching model every read path follows); after that
// it is an error, which fails refreshRoadData and leaves the last good
// roads:all in place rather than publishing a guessed OPEN.
//
// Request budget: one fetch per district per refreshRoadData, the cadence
// lcs2way.kml was fetched at (the 15m periodic refresh).
func (s *RoadsService) segmentLaneClosures(ctx context.Context) ([]caltrans.CaltransIncident, error) {
	districts := s.config.Roads.CaltransFeeds.CWWP2.LaneClosureDistricts
	if len(districts) == 0 {
		kml, _ := s.caltransClient.ParseLaneClosures(ctx)
		return kml, nil
	}

	now := s.clock()
	active, err := s.fetchActiveCWWP2Closures(ctx, districts, now)
	if err != nil {
		var cached []cwwp2.LaneClosure
		_, found, cerr := s.cache.GetWithMetadata(cwwp2ActiveClosuresKey, &cached)
		if !found || cerr != nil || s.cache.IsVeryStale(cwwp2ActiveClosuresKey) {
			return nil, fmt.Errorf("cwwp2 lane closures (no servable cache): %w", err)
		}
		logging.Warnw(ctx, "CWWP2 lane closures failed; serving last good set for segment status",
			"error", err, "closures", len(cached))
		active = cached
	} else {
		ttl := s.config.Roads.RefreshInterval
		if ttl <= 0 {
			ttl = 15 * time.Minute
		}
		if err := s.cache.Set(cwwp2ActiveClosuresKey, active, ttl, "cwwp2_lane_closures"); err != nil {
			logging.Errorw(ctx, "Failed to cache CWWP2 lane closures", "error", err)
		}
	}

	out := make([]caltrans.CaltransIncident, 0, len(active))
	for _, lc := range active {
		inc := caltrans.IncidentFromCWWP2LaneClosure(lc, now)
		if inc.Coordinates == nil {
			continue // unplaceable; the client marks these Unrecognized anyway
		}
		out = append(out, inc)
	}
	return out, nil
}

// fetchActiveCWWP2Closures reads every district and keeps the ACTIVE windows.
func (s *RoadsService) fetchActiveCWWP2Closures(ctx context.Context, districts []int, now time.Time) ([]cwwp2.LaneClosure, error) {
	if s.laneClosureSource == nil {
		return nil, errors.New("laneClosureDistricts set but no CWWP2 client wired")
	}
	var (
		active       []cwwp2.LaneClosure
		unrecognized int
	)
	for _, d := range districts {
		rows, err := s.laneClosureSource.LaneClosures(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, lc := range rows {
			if lc.Unrecognized != "" {
				// Its phase is unknown. The lane-closure poller degrades the
				// caltrans source for these; here it is logged and skipped.
				unrecognized++
				continue
			}
			// Same overrun grace as the lane-closure poller, so a segment never
			// stays CLOSED on a window the closure events have already resolved.
			if lc.PhaseAt(now, s.config.Roads.CaltransFeeds.CWWP2.LaneClosureOverrunGrace) == cwwp2.PhaseActive {
				active = append(active, lc)
			}
		}
	}
	if unrecognized > 0 {
		logging.Warnw(ctx, "CWWP2 lane closures: unreadable rows skipped for segment status", "count", unrecognized)
	}
	return active, nil
}
