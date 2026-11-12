package gridapi

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// statsTTL bounds how often GetStats recomputes a place's totals. Totals reads
// every event row (a few milliseconds on PostgreSQL at production size, but
// it grows with history), so a burst of identical requests, an overview page
// and a dashboard polling together, must not each pay for it. A few seconds
// keeps the headline numbers current to the eye: the cache is a burst
// absorber, not a freshness policy (the legacy five minutes existed because
// the same query took over 30 s on EFS).
const statsTTL = 5 * time.Second

// statsComputeTimeout bounds one computation. It runs detached from any one
// caller (see GetStats), so it needs a bound of its own: a wedged database
// connection must not pin the flight, and every caller queued behind it,
// forever.
const statsComputeTimeout = 15 * time.Second

type statsCache struct {
	mu      sync.Mutex
	entries map[string]*gridv1.Stats // by resolved place id ("" = all)
	flight  singleflight.Group       // one computation per place at a time

	// totals reads the store. Nil means Store.Totals; tests replace it to
	// count and hold computations.
	totals func(ctx context.Context, placeID string) (store.Totals, error)
}

// fresh returns the cached entry for placeID when it is younger than statsTTL.
func (c *statsCache) fresh(placeID string, now time.Time) *gridv1.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit := c.entries[placeID]; hit != nil && now.Sub(hit.GetComputedAt().AsTime()) < statsTTL {
		return hit
	}
	return nil
}

// GetStats returns ingest totals, overall or for one place.
//
// Concurrent requests for the same place share ONE computation (single
// flight), and the result is cached for statsTTL. The computation runs on a
// context detached from the first caller's, so that caller disconnecting does
// not fail everyone waiting on the same flight; each caller still returns as
// soon as its own context ends. computedAt is the time the computation
// started, the same for every response that shares it.
func (g *GridServer) GetStats(ctx context.Context, req *gridv1.GetStatsRequest) (*gridv1.Stats, error) {
	placeID, err := g.resolvePlaceID(ctx, req.GetPlace())
	if err != nil {
		return nil, err
	}
	if hit := g.stats.fresh(placeID, g.svc.Now()); hit != nil {
		return proto.Clone(hit).(*gridv1.Stats), nil
	}

	ch := g.stats.flight.DoChan(placeID, func() (any, error) {
		// A flight that started just after another finished must not
		// recompute: the cache is checked again inside it.
		if hit := g.stats.fresh(placeID, g.svc.Now()); hit != nil {
			return hit, nil
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statsComputeTimeout)
		defer cancel()
		return g.computeStats(cctx, placeID)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, internalErr(ctx, res.Err)
		}
		return proto.Clone(res.Val.(*gridv1.Stats)).(*gridv1.Stats), nil
	case <-ctx.Done():
		return nil, internalErr(ctx, ctx.Err())
	}
}

// computeStats reads the totals and stores the answer in the cache.
func (g *GridServer) computeStats(ctx context.Context, placeID string) (*gridv1.Stats, error) {
	now := g.svc.Now()
	totals := g.stats.totals
	if totals == nil {
		totals = g.svc.Store.Totals
	}
	t, err := totals(ctx, placeID)
	if err != nil {
		return nil, err
	}
	out := &gridv1.Stats{
		Place:      placeID,
		Events:     int32(t.Events),
		Revisions:  int32(t.Revisions),
		Sources:    int32(t.Sources),
		ComputedAt: timestamppb.New(now),
	}
	if !t.Since.IsZero() {
		out.Since = timestamppb.New(t.Since)
	}
	for _, l := range t.Layers {
		out.Layers = append(out.Layers, &gridv1.LayerStats{Layer: l.Layer, Events: int32(l.Events), Revisions: int32(l.Revisions)})
	}

	g.stats.mu.Lock()
	if g.stats.entries == nil {
		g.stats.entries = map[string]*gridv1.Stats{}
	}
	g.stats.entries[placeID] = out
	g.stats.mu.Unlock()
	return out, nil
}
