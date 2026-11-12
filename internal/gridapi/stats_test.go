package gridapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetStats(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return clock }
	g := NewGridServer(s)

	all, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Positive(t, all.GetEvents())
	assert.GreaterOrEqual(t, all.GetRevisions(), all.GetEvents())
	assert.Positive(t, all.GetSources())
	require.NotNil(t, all.GetSince())
	var sum int32
	for _, l := range all.GetLayers() {
		sum += l.GetEvents()
	}
	assert.Equal(t, all.GetEvents(), sum, "layers add up to the total")

	// Cached: a new event inside the TTL does not move the count.
	upsert(t, s.Store, &gridv1.Event{
		Id: "caloes:extra", Layer: gridv1.Layer_EVACUATION, Severity: gridv1.Severity_MINOR,
		Status: gridv1.EventStatus_ACTIVE, Headline: "Warning",
		Provenance: &gridv1.Provenance{SourceId: "caloes", SourceName: "Cal OES"},
	})
	again, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Equal(t, all.GetEvents(), again.GetEvents())
	clock = clock.Add(statsTTL)
	fresh, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Equal(t, all.GetEvents()+1, fresh.GetEvents())

	_, err = g.GetStats(rpcBG(), &gridv1.GetStatsRequest{Place: "nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// Concurrent identical requests share one computation, and every response
// carries that computation's computedAt.
func TestGetStatsSingleFlight(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	var clock atomic.Pointer[time.Time]
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock.Store(&start)
	s.Now = func() time.Time { return *clock.Load() }
	g := NewGridServer(s)

	var calls atomic.Int32
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	g.stats.totals = func(ctx context.Context, placeID string) (store.Totals, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return s.Store.Totals(ctx, placeID)
	}

	const n = 32
	var wg sync.WaitGroup
	got := make([]*gridv1.Stats, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
		}()
	}
	<-entered // the one computation is running; the rest queue behind it
	time.Sleep(50 * time.Millisecond)
	// Time moves on while it runs: computedAt is when the computation began.
	later := start.Add(2 * time.Second)
	clock.Store(&later)
	close(release)
	wg.Wait()

	assert.EqualValues(t, 1, calls.Load(), "%d concurrent requests, one computation", n)
	for i := range n {
		require.NoError(t, errs[i])
		assert.True(t, got[i].GetComputedAt().AsTime().Equal(start), "computedAt is the computation's, got %v", got[i].GetComputedAt().AsTime())
		assert.Equal(t, got[0].GetEvents(), got[i].GetEvents())
	}

	// Inside the TTL: served from the cache, still one computation.
	again, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load())
	assert.True(t, again.GetComputedAt().AsTime().Equal(start))

	// Past it: a new computation with a new computedAt. Another place is its
	// own flight and its own cache entry.
	past := start.Add(statsTTL)
	clock.Store(&past)
	g.stats.totals = func(ctx context.Context, placeID string) (store.Totals, error) {
		calls.Add(1)
		return s.Store.Totals(ctx, placeID)
	}
	fresh, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
	assert.True(t, fresh.GetComputedAt().AsTime().Equal(past))
	place, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{Place: "arnold"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load(), "a different place is a different flight")
	assert.Contains(t, place.GetPlace(), "arnold")
}

// A caller that goes away neither fails the others nor leaves the flight
// running unbounded; a failed computation is not cached.
func TestGetStatsCancelAndError(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var calls atomic.Int32
	fail := atomic.Bool{}
	g.stats.totals = func(ctx context.Context, placeID string) (store.Totals, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return store.Totals{}, ctx.Err()
		}
		if fail.Load() {
			return store.Totals{}, errors.New("boom")
		}
		return s.Store.Totals(ctx, placeID)
	}

	// The first caller starts the flight and is cancelled; a second joined it.
	ctx1, cancel1 := context.WithCancel(rpcBG())
	first := make(chan error, 1)
	go func() { _, err := g.GetStats(ctx1, &gridv1.GetStatsRequest{}); first <- err }()
	<-entered
	second := make(chan error, 1)
	var secondStats atomic.Pointer[gridv1.Stats]
	go func() {
		st, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
		secondStats.Store(st)
		second <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel1()
	assert.Equal(t, codes.Unavailable, status.Code(<-first), "the cancelled caller returns at once")
	close(release)
	require.NoError(t, <-second, "the computation outlived the caller that started it")
	require.NotNil(t, secondStats.Load())
	assert.EqualValues(t, 1, calls.Load())

	// An error is returned to the callers of that flight and not cached.
	g.stats.mu.Lock()
	g.stats.entries = nil
	g.stats.mu.Unlock()
	fail.Store(true)
	release = make(chan struct{})
	close(release)
	_, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	assert.Equal(t, codes.Internal, status.Code(err))
	fail.Store(false)
	ok, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Positive(t, ok.GetEvents())
	assert.EqualValues(t, 3, calls.Load())
}
