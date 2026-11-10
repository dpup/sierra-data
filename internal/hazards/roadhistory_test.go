package hazards

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	api "github.com/sierragridteam/sierra-data/api/v1"
)

type fakeRoadHistory struct {
	since        time.Time
	status       int32
	atStart, ok  bool
	typical      int32
	typicalCalls int
}

func (f *fakeRoadHistory) RoadStatusSince(ctx context.Context, roadID string) (time.Time, int32, bool, bool, error) {
	return f.since, f.status, f.atStart, f.ok, nil
}

func (f *fakeRoadHistory) TypicalRoadMinutes(ctx context.Context, roadID string, openStatus int32, at, from, to time.Time, loc *time.Location, minSamples int) (int32, string, bool, error) {
	f.typicalCalls++
	return f.typical, "HOUR_OF_WEEK", f.typical > 0, nil
}

func TestAddRoadHistory(t *testing.T) {
	s := &Service{}
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	rd := &api.Road{Id: "hwy4", Status: api.RoadStatus_CLOSED}

	p := &RoadProps{}
	s.addRoadHistory(context.Background(), p, rd, now)
	assert.Empty(t, p.StatusSince, "no history wired: nothing claimed")

	f := &fakeRoadHistory{since: now.Add(-3 * time.Hour), status: int32(api.RoadStatus_CLOSED), ok: true, typical: 22}
	s.UseRoadHistory(f)
	s.addRoadHistory(context.Background(), p, rd, now)
	assert.Equal(t, "2026-11-10T17:00:00Z", p.StatusSince)
	assert.False(t, p.StatusSinceAtLeast)
	assert.Equal(t, int32(22), *p.TypicalMinutes)
	assert.Equal(t, "HOUR_OF_WEEK", p.TypicalBasis)

	s.addRoadHistory(context.Background(), &RoadProps{}, rd, now.Add(10*time.Minute))
	assert.Equal(t, 1, f.typicalCalls, "typical is cached per road and hour of week")

	// The archive's latest status disagrees with the live one (a refresh the
	// sampler has not seen yet): no since is better than a wrong one.
	f.status = int32(api.RoadStatus_OPEN)
	q := &RoadProps{}
	s.addRoadHistory(context.Background(), q, rd, now)
	assert.Empty(t, q.StatusSince)
}
