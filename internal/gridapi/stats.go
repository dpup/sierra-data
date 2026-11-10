package gridapi

import (
	"context"
	"sync"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// statsTTL bounds how often GetStats recomputes a place's totals. The counts
// walk every event the place has ever held, and on EFS that is a burst of
// network reads; a headline number on an overview page does not need to be
// fresher than this.
const statsTTL = 5 * time.Minute

type statsCache struct {
	mu      sync.Mutex
	entries map[string]*gridv1.Stats // by resolved place id ("" = all)
}

// GetStats returns ingest totals, overall or for one place.
func (g *GridServer) GetStats(ctx context.Context, req *gridv1.GetStatsRequest) (*gridv1.Stats, error) {
	placeID, err := g.resolvePlaceID(ctx, req.GetPlace())
	if err != nil {
		return nil, err
	}
	now := g.svc.Now()

	g.stats.mu.Lock()
	if hit := g.stats.entries[placeID]; hit != nil && now.Sub(hit.GetComputedAt().AsTime()) < statsTTL {
		g.stats.mu.Unlock()
		return proto.Clone(hit).(*gridv1.Stats), nil
	}
	g.stats.mu.Unlock()

	t, err := g.svc.Store.Totals(ctx, placeID)
	if err != nil {
		return nil, internalErr(ctx, err)
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
	return proto.Clone(out).(*gridv1.Stats), nil
}
