package hazards

import (
	"context"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	api "github.com/sierragridteam/sierra-data/api/v1"
)

// RoadHistory is the read side of the road segment archive (satisfied by
// *store.Store).
type RoadHistory interface {
	RoadStatusSince(ctx context.Context, roadID string) (since time.Time, status int32, atStart, ok bool, err error)
	TypicalRoadMinutes(ctx context.Context, roadID string, openStatus int32, at, from, to time.Time, loc *time.Location, minSamples int) (int32, string, bool, error)
}

const (
	// typicalWindow and typicalMinSamples: eight weeks of 15-minute samples
	// is ~32 per hour of the week, so 8 is a few weeks of that hour.
	typicalWindow     = 8 * 7 * 24 * time.Hour
	typicalMinSamples = 8
	typicalCacheTTL   = time.Hour
)

type roadHistoryHolder struct {
	h  RoadHistory
	mu sync.Mutex
	// typical caches a road's typical minutes per hour of the week.
	typical map[string]typicalEntry
}

type typicalEntry struct {
	minutes int32
	basis   string
	ok      bool
	at      time.Time
}

// UseRoadHistory enables statusSince and the typical travel time on the
// road_segment layer. Called when the store opens; nil disables it.
func (s *Service) UseRoadHistory(h RoadHistory) {
	if h == nil {
		s.history.Store(nil)
		return
	}
	s.history.Store(&roadHistoryHolder{h: h, typical: map[string]typicalEntry{}})
}

var pacific = func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PST", -8*3600)
}()

// addRoadHistory fills the archive-backed fields. Best-effort: a read error
// leaves them absent (unknown), never a guess.
func (s *Service) addRoadHistory(ctx context.Context, p *RoadProps, rd *api.Road, now time.Time) {
	hh := s.history.Load()
	if hh == nil || rd == nil {
		return
	}
	since, status, atStart, ok, err := hh.h.RoadStatusSince(ctx, rd.GetId())
	if err != nil {
		logging.Warnw(ctx, "road history: status since", "road", rd.GetId(), "error", err)
	} else if ok && status == int32(rd.GetStatus()) {
		p.StatusSince = since.UTC().Format(time.RFC3339)
		p.StatusSinceAtLeast = atStart
	}
	key := rd.GetId() + "|" + now.In(pacific).Format("Mon15")
	hh.mu.Lock()
	e, hit := hh.typical[key]
	hh.mu.Unlock()
	if !hit || now.Sub(e.at) > typicalCacheTTL {
		m, basis, ok, err := hh.h.TypicalRoadMinutes(ctx, rd.GetId(), int32(api.RoadStatus_OPEN), now, now.Add(-typicalWindow), now, pacific, typicalMinSamples)
		if err != nil {
			logging.Warnw(ctx, "road history: typical minutes", "road", rd.GetId(), "error", err)
			return
		}
		e = typicalEntry{minutes: m, basis: basis, ok: ok, at: now}
		hh.mu.Lock()
		hh.typical[key] = e
		hh.mu.Unlock()
	}
	if e.ok {
		v := e.minutes
		p.TypicalMinutes = &v
		p.TypicalBasis = e.basis
	}
}
