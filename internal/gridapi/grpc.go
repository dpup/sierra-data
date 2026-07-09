package gridapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

// GridServer implements the gRPC GridService — the read-only /api/v1 entity and
// query surface exposed over gRPC-Gateway. Endpoints are ported one at a time
// from the hand-built /v1 handlers; see docs/grpc-gateway-migration-plan.md.
type GridServer struct {
	gridv1.UnimplementedGridServiceServer
	store *store.Store
}

// NewGridServer wires the service to the grid event store.
func NewGridServer(st *store.Store) *GridServer {
	return &GridServer{store: st}
}

// ListSources returns the source registry with per-source health (OK / STALE /
// UNAVAILABLE) — the honesty mechanism clients key layer trust off.
func (g *GridServer) ListSources(ctx context.Context, _ *gridv1.ListSourcesRequest) (*gridv1.SourceList, error) {
	sources, err := g.store.ListSources(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "list sources: "+err.Error())
	}
	return &gridv1.SourceList{Sources: sources}, nil
}
