package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// DefaultCameraRefresh is how often the camera directory refetches each
// district's camera list when roads.caltransFeeds.cwwp2.cameras.refreshInterval
// is unset. The list is near-static (D10's file was last edited a week before
// the 2026-10-01 capture, D9's seven months before), so this only bounds how
// long a camera Caltrans takes out of service keeps being listed.
const DefaultCameraRefresh = 6 * time.Hour

// cameraRetryDelay is how soon a failed district is retried, instead of
// waiting out the full refresh interval.
const cameraRetryDelay = 5 * time.Minute

// CameraFetcher is the slice of *cwwp2.Client the camera directory uses.
type CameraFetcher interface {
	Cameras(ctx context.Context, district int) ([]cwwp2.Camera, error)
}

// CameraService keeps the Caltrans CCTV camera directory: every configured
// district's in-service cameras, refreshed in the background (Run) so a request
// never waits on Caltrans.
//
// It deliberately does NOT use the TTL cache the other services share (see
// CLAUDE.md here). That cache stops serving at 2x the refresh interval, which
// suits feeds that change by the minute. A camera list from last week is still
// right: when a refresh fails, the district's last good list keeps being served,
// marked STALE with the time it was fetched, for as long as the failure lasts.
// The images it points at are live either way.
type CameraService struct {
	fetcher   CameraFetcher
	districts []int
	refresh   time.Duration
	retry     time.Duration
	now       func() time.Time

	mu     sync.RWMutex
	byDist map[int]cameraDistrict
	snap   cameraSnapshot
}

// cameraDistrict is one district's last good list and its latest attempt.
type cameraDistrict struct {
	cams      []cwwp2.Camera // last good fetch, already filtered
	fetchedAt time.Time      // last success; zero if none yet
	err       error          // the latest attempt's error; nil once it succeeds
}

// cameraSnapshot is what Cameras serves, rebuilt after every refresh.
type cameraSnapshot struct {
	cams       []cwwp2.Camera
	status     string
	lastUpdate time.Time
	version    string
}

// NewCameraService builds the directory over the given districts. refresh <= 0
// uses DefaultCameraRefresh. Nothing is fetched until Refresh or Run.
func NewCameraService(f CameraFetcher, districts []int, refresh time.Duration) *CameraService {
	if refresh <= 0 {
		refresh = DefaultCameraRefresh
	}
	s := &CameraService{
		fetcher:   f,
		districts: append([]int(nil), districts...),
		refresh:   refresh,
		retry:     min(cameraRetryDelay, refresh),
		now:       time.Now,
		byDist:    make(map[int]cameraDistrict, len(districts)),
	}
	s.snap = s.buildSnapshot()
	return s
}

// Cameras returns the served directory: in-service cameras that have a
// position and an image, in configured-district order, then the portal's row
// order. status is OK, STALE (some district is being served from an older
// fetch, or is missing) or UNAVAILABLE (no district has ever been fetched).
// lastSourceUpdate is zero when OK, else the oldest fetch being served. version
// changes whenever any of the rest does — an ETag input.
func (s *CameraService) Cameras() (cams []cwwp2.Camera, status string, lastSourceUpdate time.Time, version string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap.cams, s.snap.status, s.snap.lastUpdate, s.snap.version
}

// Refresh fetches every district once and reports whether all of them
// succeeded. A failed district keeps its last good list.
func (s *CameraService) Refresh(ctx context.Context) bool {
	type result struct {
		cams []cwwp2.Camera
		err  error
	}
	results := make(map[int]result, len(s.districts))
	for _, d := range s.districts {
		cams, err := s.fetcher.Cameras(ctx, d)
		if err != nil {
			logging.Warnw(ctx, "Camera list refresh failed; serving the last good list", "district", d, "error", err)
		}
		results[d] = result{cams: servableCameras(cams), err: err}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ok := true
	for d, r := range results {
		st := s.byDist[d]
		if r.err != nil {
			ok = false
			st.err = r.err
		} else {
			st = cameraDistrict{cams: r.cams, fetchedAt: s.now()}
		}
		s.byDist[d] = st
	}
	s.snap = s.buildSnapshot()
	return ok
}

// Run refreshes immediately and then on a schedule until ctx is cancelled:
// every refresh interval, or after cameraRetryDelay when a district failed.
func (s *CameraService) Run(ctx context.Context) {
	for {
		wait := s.refresh
		if !s.Refresh(ctx) {
			wait = s.retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// servableCameras drops what a consumer cannot use: cameras Caltrans flags out
// of service (every one checked served a "Down for Construction" placeholder —
// with a fresh timestamp, so image age cannot catch them), and rows with no
// position or no usable image URL.
func servableCameras(cams []cwwp2.Camera) []cwwp2.Camera {
	out := make([]cwwp2.Camera, 0, len(cams))
	for _, c := range cams {
		if c.InService && c.Location.HasPosition && c.ImageURL != "" {
			out = append(out, c)
		}
	}
	return out
}

// buildSnapshot derives the served snapshot from per-district state. Called
// with mu held (or before the service is shared).
func (s *CameraService) buildSnapshot() cameraSnapshot {
	var snap cameraSnapshot
	seen := make(map[string]bool)
	served, degraded := 0, false
	for _, d := range s.districts {
		st := s.byDist[d]
		if st.err != nil || st.fetchedAt.IsZero() {
			degraded = true
		}
		if st.fetchedAt.IsZero() {
			continue
		}
		served++
		if snap.lastUpdate.IsZero() || st.fetchedAt.Before(snap.lastUpdate) {
			snap.lastUpdate = st.fetchedAt
		}
		for _, c := range st.cams {
			if !seen[c.ID] {
				seen[c.ID] = true
				snap.cams = append(snap.cams, c)
			}
		}
	}
	switch {
	case served == 0:
		snap.status, snap.lastUpdate = "UNAVAILABLE", time.Time{}
	case degraded:
		snap.status = "STALE"
	default:
		snap.status, snap.lastUpdate = "OK", time.Time{}
	}

	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		Cams       []cwwp2.Camera
		Status     string
		LastUpdate time.Time
	}{snap.cams, snap.status, snap.lastUpdate})
	snap.version = hex.EncodeToString(h.Sum(nil)[:8])
	return snap
}
