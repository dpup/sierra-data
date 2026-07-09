package gridapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

// notFoundErr / invalidErr / internalErr map store/parse failures to grpc status
// codes (the gateway renders these as google.rpc.Status with the mapped HTTP
// status). resolvePlaceID looks up a place filter, returning NotFound on miss.
func notFoundErr(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}
func invalidErr(err error) error   { return status.Error(codes.InvalidArgument, err.Error()) }
func internalErr(err error) error  { return status.Error(codes.Internal, err.Error()) }
func tokenOrInternal(err error) error {
	if isBadToken(err) {
		return status.Error(codes.InvalidArgument, "invalid page_token")
	}
	return internalErr(err)
}

// resolvePlaceID resolves an optional place filter to its id ("" stays "").
func (g *GridServer) resolvePlaceID(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	place, err := g.store.GetPlace(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", notFoundErr("unknown place: %q", key)
	}
	if err != nil {
		return "", internalErr(err)
	}
	return place.GetId(), nil
}

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

// ListEvents returns store-backed events with the place/layer/status/severity/
// since filters and keyset pagination (order: severity DESC, observed_at DESC,
// id ASC). page_size 0 defaults via the store's clamp.
func (g *GridServer) ListEvents(ctx context.Context, req *gridv1.ListEventsRequest) (*gridv1.EventList, error) {
	var eq store.EventQuery
	var err error
	if eq.PlaceID, err = g.resolvePlaceID(ctx, req.GetPlace()); err != nil {
		return nil, err
	}
	if eq.Layers, err = parseLayers(req.GetLayer()); err != nil {
		return nil, invalidErr(err)
	}
	if eq.Statuses, err = parseStatuses(req.GetStatus()); err != nil {
		return nil, invalidErr(err)
	}
	if v := req.GetSeverityMin(); v != "" {
		if eq.MinSeverity, err = parseSeverity(v); err != nil {
			return nil, invalidErr(err)
		}
	}
	if v := req.GetSince(); v != "" {
		if eq.Since, err = parseRFC3339("since", v); err != nil {
			return nil, invalidErr(err)
		}
	}
	eq.PageSize = int(req.GetPageSize())
	eq.PageToken = req.GetPageToken()

	events, next, err := g.store.QueryEvents(ctx, eq)
	if err != nil {
		return nil, tokenOrInternal(err)
	}
	stripEventsIO(events, req.GetEnhancementIo())
	return &gridv1.EventList{Events: events, NextPageToken: next}, nil
}

// GetEvent returns the current revision of one event.
func (g *GridServer) GetEvent(ctx context.Context, req *gridv1.GetEventRequest) (*gridv1.Event, error) {
	ev, err := g.store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(err)
	}
	stripEventsIO([]*gridv1.Event{ev}, req.GetEnhancementIo())
	return ev, nil
}

// GetEventHistory returns one event's revisions, newest first. Unknown ids 404
// (an empty history is indistinguishable from a revision-less event, which
// cannot exist).
func (g *GridServer) GetEventHistory(ctx context.Context, req *gridv1.GetEventHistoryRequest) (*gridv1.EventRevisionList, error) {
	if _, err := g.store.GetEvent(ctx, req.GetId()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown event: %q", req.GetId())
		}
		return nil, internalErr(err)
	}
	revs, next, err := g.store.EventHistory(ctx, req.GetId(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, tokenOrInternal(err)
	}
	stripRevisionsIO(revs, req.GetEnhancementIo())
	return &gridv1.EventRevisionList{Revisions: revs, NextPageToken: next}, nil
}

// ListHistory returns revisions across events filtered by place, layer, and a
// half-open [from, to) window over observed_at.
func (g *GridServer) ListHistory(ctx context.Context, req *gridv1.ListHistoryRequest) (*gridv1.EventRevisionList, error) {
	var hq store.HistoryQuery
	var err error
	if hq.PlaceID, err = g.resolvePlaceID(ctx, req.GetPlace()); err != nil {
		return nil, err
	}
	if hq.Layers, err = parseLayers(req.GetLayer()); err != nil {
		return nil, invalidErr(err)
	}
	if v := req.GetFrom(); v != "" {
		if hq.From, err = parseRFC3339("from", v); err != nil {
			return nil, invalidErr(err)
		}
	}
	if v := req.GetTo(); v != "" {
		if hq.To, err = parseRFC3339("to", v); err != nil {
			return nil, invalidErr(err)
		}
	}
	hq.PageSize = int(req.GetPageSize())
	hq.PageToken = req.GetPageToken()

	revs, next, err := g.store.QueryHistory(ctx, hq)
	if err != nil {
		return nil, tokenOrInternal(err)
	}
	stripRevisionsIO(revs, req.GetEnhancementIo())
	return &gridv1.EventRevisionList{Revisions: revs, NextPageToken: next}, nil
}

// ListPlaces returns the place directory filtered by kind (enum name or
// lowercase) and a case-insensitive name substring q.
func (g *GridServer) ListPlaces(ctx context.Context, req *gridv1.ListPlacesRequest) (*gridv1.PlaceList, error) {
	kind := gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED
	if v := req.GetKind(); v != "" {
		k, err := parseKind(v)
		if err != nil {
			return nil, invalidErr(err)
		}
		kind = k
	}
	places, err := g.store.ListPlaces(ctx, kind, req.GetQ())
	if err != nil {
		return nil, internalErr(err)
	}
	return &gridv1.PlaceList{Places: places}, nil
}

// GetPlace returns one place by slug or namespaced id ("area:ebbetts-pass").
func (g *GridServer) GetPlace(ctx context.Context, req *gridv1.GetPlaceRequest) (*gridv1.Place, error) {
	place, err := g.store.GetPlace(ctx, req.GetPlace())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown place: %q", req.GetPlace())
	}
	if err != nil {
		return nil, internalErr(err)
	}
	return place, nil
}

// ListSources returns the source registry with per-source health (OK / STALE /
// UNAVAILABLE) — the honesty mechanism clients key layer trust off.
func (g *GridServer) ListSources(ctx context.Context, _ *gridv1.ListSourcesRequest) (*gridv1.SourceList, error) {
	sources, err := g.store.ListSources(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	return &gridv1.SourceList{Sources: sources}, nil
}
