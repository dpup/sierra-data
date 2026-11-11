package gridapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/dpup/prefab/plugins/etag"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/sierragridteam/sierra-data/internal/config"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/census"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// RegisterGatewayRoutes mounts the .geojson map layers — the one endpoint that
// stays hand-built (RFC 7946 geometry, which proto3 models poorly) — directly on
// the gateway mux, so it lives under /api/v1 beside the proto RPCs with no
// routing collision. Its property/metadata keys are camelCase (json struct tags)
// to match the rest of the surface. Call from a prefab.WithGRPCGateway callback
// after registering the GridService handler. (The place summary is now the
// GetPlaceSummary RPC.)
//
// wrap, when non-nil, wraps each route; cmd/server passes the handoff
// admission gate, since these routes reach the store without going through
// the gRPC interceptor.
func (g *GridServer) RegisterGatewayRoutes(mux *runtime.ServeMux, wrap func(runtime.HandlerFunc) runtime.HandlerFunc) error {
	if wrap == nil {
		wrap = func(h runtime.HandlerFunc) runtime.HandlerFunc { return h }
	}
	if err := mux.HandlePath("GET", "/api/v1/places/{place}/map/{layer}",
		wrap(func(w http.ResponseWriter, r *http.Request, pp map[string]string) {
			g.svc.serveMapLayer(w, r, pp["place"], strings.TrimSuffix(pp["layer"], ".geojson"))
		})); err != nil {
		return err
	}
	// The MeshCore relay topology — a global, windowed weighted edge list derived
	// from the observation rollup (not place-scoped: a mesh spans places).
	// (The telemetry archive next door is a proto RPC — GetMeshTelemetry. A
	// series of numbers is exactly what proto3 models well, so it gets the
	// generated contract, the reflection entry and the OpenAPI row like every
	// other read. Hand-building is for geometry.)
	return mux.HandlePath("GET", "/api/v1/mesh/links",
		wrap(func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			g.svc.serveMeshLinks(w, r)
		}))
}

// notFoundErr / invalidErr / internalErr map store/parse failures to grpc status
// codes (the gateway renders these as google.rpc.Status with the mapped HTTP
// status). resolvePlaceID looks up a place filter, returning NotFound on miss.
func notFoundErr(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}
func invalidErr(err error) error { return status.Error(codes.InvalidArgument, err.Error()) }

// internalErr logs the real error server-side and returns a generic
// codes.Internal status — the raw error never reaches the (public, unauthed)
// wire. Mirrors errors.go's internal() convention for the hand-built handlers.
//
// A store error on a request whose context already ended is not internal:
// the query was interrupted (a deploy's drain cancels in-flight requests),
// so it is the retryable interruptedErr instead.
func internalErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		logging.Infow(logging.EnsureLogger(ctx), "gridapi: request interrupted", "error", err, "cause", ctx.Err())
		return interruptedErr(ctx)
	}
	logError(ctx, "gridapi: internal error", err)
	return status.Error(codes.Internal, "internal error")
}
func tokenOrInternal(ctx context.Context, err error) error {
	if isBadToken(err) {
		return status.Error(codes.InvalidArgument, "invalid page_token")
	}
	return internalErr(ctx, err)
}

// resolvePlaceID resolves an optional place filter to its id ("" stays "").
func (g *GridServer) resolvePlaceID(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	place, err := g.svc.Store.GetPlace(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", notFoundErr("unknown place: %q", key)
	}
	if err != nil {
		return "", internalErr(ctx, err)
	}
	return place.GetId(), nil
}

// GridServer implements the gRPC GridService — the read-only /api/v1 entity and
// query surface exposed over gRPC-Gateway. It wraps the existing *Service, which
// already holds the store, geocoder, config, hazards builder, and clock plus the
// shared helpers, so the RPCs reuse that logic rather than duplicating it.
// Endpoints are ported one at a time; see docs/design/grpc-gateway-migration-plan.md.
type GridServer struct {
	gridv1.UnimplementedGridServiceServer
	svc *Service
	// placesVersion is a per-process nonce mixed into the place and event-list
	// ETag validators. The place directory is seeded once at boot and never
	// mutates mid-process, so a fresh nonce per start IS its version. Folding it
	// into the event-list validators too invalidates them on redeploy, covering
	// the rare case where a changed place polygon reattaches events without a new
	// revision (which DataVersion alone would miss).
	placesVersion string
	// pageBudget is the encoded-size cap on one list page (PageByteBudget;
	// tests shrink it to keep fixtures small).
	pageBudget int
	// stats caches GetStats per place for statsTTL.
	stats statsCache
}

// NewGridServer wires the gRPC service over the existing gridapi Service.
func NewGridServer(svc *Service) *GridServer {
	return &GridServer{svc: svc, placesVersion: randomNonce(), pageBudget: PageByteBudget}
}

// randomNonce returns a short random hex string. On the (practically
// impossible) crypto/rand failure it returns a fixed value — the validators
// then stay stable-but-weak rather than panicking at startup.
func randomNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "boot"
	}
	return hex.EncodeToString(b[:])
}

// weakListTag builds a weak ETag from a version prefix plus the request's filter
// values, hashed so the header stays clean and collision-resistant regardless of
// what the values contain.
func weakListTag(version string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return etag.Weak(version + "-" + hex.EncodeToString(sum[:8]))
}

// parsePageSize validates a list RPC's page_size. The proto contract is
// "0 = default (store clamps to 1..200)": 0 takes the store default (50) and
// anything above the ceiling is clamped by the store, not refused, so a client
// asking for "as many as you will give me" gets a full page. A negative size has
// no meaning, and used to be read as 0, so it is refused rather than guessed.
func parsePageSize(n int32) (int, error) {
	if n < 0 {
		return 0, fmt.Errorf("page_size must be >= 0 (0 = the default of %d; above %d is clamped to %d), got %d",
			store.DefaultPageSize, store.MaxPageSize, store.MaxPageSize, n)
	}
	return int(n), nil
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
	if eq.PageSize, err = parsePageSize(req.GetPageSize()); err != nil {
		return nil, invalidErr(err)
	}
	eq.PageToken = req.GetPageToken()
	// NONE (this RPC only) serves each event's geometry-free copy: bbox and
	// centroid, no GeoJSON. It is what makes a deep list page cheap, because
	// the shapes are what a cold read spends its time on (store.migrationV12).
	var geomFmt geometryFormat
	if strings.EqualFold(strings.TrimSpace(req.GetGeometryFormat()), "NONE") {
		eq.Lite = true
	} else if geomFmt, err = parseGeometryFormat(req.GetGeometryFormat()); err != nil {
		return nil, invalidErr(fmt.Errorf("%w; /api/v1/events also takes NONE", err))
	}

	// Filters are validated above (400/404 first); guard on a cheap global data
	// version + the filter set so a match skips the query entirely.
	dv, err := g.svc.Store.DataVersion(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	// DataVersion counts revisions; RefreshVersion counts the in-place blob
	// rewrites (enhancement, mesh telemetry, burn observations, place ids)
	// that change a body without one. Per layer, so an unrelated layer's
	// refresh leaves this list's 304 alone.
	rv, err := g.svc.Store.RefreshVersion(ctx, eq.Layers)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	version := g.placesVersion + "." + strconv.FormatInt(dv, 10) + ".r" + strconv.FormatInt(rv, 10)
	if layersIncludeMesh(eq.Layers) {
		// Mesh activity is overlaid at read time and moves without a revision.
		mv, err := g.svc.Store.MeshActivityVersion(ctx)
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		version += ".m" + strconv.FormatInt(mv, 10)
	}
	tag := weakListTag(version,
		req.GetPlace(), strings.Join(req.GetLayer(), ","), strings.Join(req.GetStatus(), ","),
		req.GetSeverityMin(), req.GetSince(),
		strconv.Itoa(int(req.GetPageSize())), req.GetPageToken(), strconv.FormatBool(req.GetEnhancementIo()),
		geomFmt.String(), strconv.FormatBool(eq.Lite))
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the query
	}

	page, err := g.svc.Store.QueryEventsPage(ctx, eq)
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
	}
	events := page.Items
	stripEventsIO(events, req.GetEnhancementIo())
	g.svc.overlayMeshActivity(ctx, events)
	applyGeometryFormat(ctx, geomFmt, eventGeometries(events)...)
	// Cut at the byte budget only now, with every rewrite applied, so the
	// budget measures what is actually sent.
	n := fitToBudget(events, g.pageBudget)
	return &gridv1.EventList{Events: events[:n], NextPageToken: page.Next(n)}, nil
}

// GetEvent returns the current revision of one event. A cheap revision read
// answers a conditional GET (304) before the proto blob is loaded.
func (g *GridServer) GetEvent(ctx context.Context, req *gridv1.GetEventRequest) (*gridv1.Event, error) {
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}
	stamp, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	// The body depends on the revision, the in-place rewrites of that
	// revision's blob (a fresh enhancement, mesh telemetry, a burn observation
	// or re-attached place_ids land without a revision bump — see
	// store.refreshEventPlaces), and whether the large enhancement I/O fields
	// are included, and the geometry format. placesVersion is the per-boot
	// nonce the in-process counters need.
	tag := fmt.Sprintf("%s:%s:%d.%d:%t:%s", g.placesVersion, req.GetId(), stamp.Revision, stamp.Refreshes, req.GetEnhancementIo(), geomFmt)
	// A mesh node's activity clocks are overlaid from outside the blob and move
	// without a revision, so they are part of the validator. One primary-key
	// read, reused for the overlay below.
	var activity map[string]store.MeshActivity
	if native, ok := strings.CutPrefix(req.GetId(), meshActivityPrefix); ok {
		if activity, err = g.svc.Store.MeshActivityFor(ctx, []string{native}); err != nil {
			logError(ctx, "gridapi: reading mesh activity", err)
			activity = nil
		}
		tag += ":" + meshActivityTag(activity[strings.ToLower(native)])
	}
	if err := etag.Guard(ctx, etag.Weak(tag)); err != nil {
		return nil, err // 304 Not Modified — the blob load below is skipped
	}
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	stripEventsIO([]*gridv1.Event{ev}, req.GetEnhancementIo())
	applyMeshActivity([]*gridv1.Event{ev}, activity)
	applyGeometryFormat(ctx, geomFmt, ev.GetGeometry())
	return ev, nil
}

// GetEventHistory returns one event's revisions, newest first. Unknown ids 404
// (an empty history is indistinguishable from a revision-less event, which
// cannot exist).
func (g *GridServer) GetEventHistory(ctx context.Context, req *gridv1.GetEventHistoryRequest) (*gridv1.EventRevisionList, error) {
	pageSize, err := parsePageSize(req.GetPageSize())
	if err != nil {
		return nil, invalidErr(err)
	}
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}
	// A cheap revision read doubles as the existence check (replacing a full
	// blob load) and the ETag validator: history grows only when the current
	// revision bumps; the page window and IO flag shape the body.
	stamp, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	// Revision snapshots are immutable, so in-place rewrites of the current
	// blob (stamp.Refreshes) never change this body: the revision alone keys it.
	tag := etag.Weak(fmt.Sprintf("%s:%s:%d:%d:%s:%t:%s", g.placesVersion, req.GetId(), stamp.Revision, req.GetPageSize(), req.GetPageToken(), req.GetEnhancementIo(), geomFmt))
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the history query + blob rehydrate
	}
	page, err := g.svc.Store.EventHistoryPage(ctx, req.GetId(), pageSize, req.GetPageToken())
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
	}
	return g.revisionPage(ctx, page, req.GetEnhancementIo(), geomFmt), nil
}

// revisionPage shapes a store page of revisions for the wire: I/O stripped,
// geometry in the requested form, then cut at the byte budget (after the
// rewrites, so the budget measures what is sent).
func (g *GridServer) revisionPage(ctx context.Context, page store.Page[*gridv1.EventRevision], keepIO bool, geomFmt geometryFormat) *gridv1.EventRevisionList {
	revs := page.Items
	stripRevisionsIO(revs, keepIO)
	applyGeometryFormat(ctx, geomFmt, revisionGeometries(revs)...)
	n := fitToBudget(revs, g.pageBudget)
	return &gridv1.EventRevisionList{Revisions: revs[:n], NextPageToken: page.Next(n)}
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
	if hq.PageSize, err = parsePageSize(req.GetPageSize()); err != nil {
		return nil, invalidErr(err)
	}
	hq.PageToken = req.GetPageToken()
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}

	dv, err := g.svc.Store.DataVersion(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	version := g.placesVersion + "." + strconv.FormatInt(dv, 10)
	if hq.PlaceID != "" {
		// Snapshots never change, but which events a place matches can: a
		// place re-attachment rewrites event_places without a revision.
		rv, err := g.svc.Store.RefreshVersion(ctx, hq.Layers)
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		version += ".r" + strconv.FormatInt(rv, 10)
	}
	tag := weakListTag(version,
		req.GetPlace(), strings.Join(req.GetLayer(), ","), req.GetFrom(), req.GetTo(),
		strconv.Itoa(int(req.GetPageSize())), req.GetPageToken(), strconv.FormatBool(req.GetEnhancementIo()),
		geomFmt.String())
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the query
	}

	page, err := g.svc.Store.QueryHistoryPage(ctx, hq)
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
	}
	return g.revisionPage(ctx, page, req.GetEnhancementIo(), geomFmt), nil
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
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}
	// The directory is static within a process; a match skips building the
	// (geometry-heavy) list.
	if err := etag.Guard(ctx, weakListTag(g.placesVersion, "kind="+req.GetKind(), "q="+req.GetQ(), "geometry="+geomFmt.String())); err != nil {
		return nil, err
	}
	places, err := g.svc.Store.ListPlaces(ctx, kind, req.GetQ())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	applyGeometryFormat(ctx, geomFmt, placeGeometries(places)...)
	return &gridv1.PlaceList{Places: places}, nil
}

// GetPlace returns one place by slug or namespaced id ("area:ebbetts-pass").
func (g *GridServer) GetPlace(ctx context.Context, req *gridv1.GetPlaceRequest) (*gridv1.Place, error) {
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}
	place, err := g.svc.Store.GetPlace(ctx, req.GetPlace())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown place: %q", req.GetPlace())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	// Places are static within a process; the load above is a single indexed row,
	// so guard after it — a match still saves re-sending the geometry blob.
	if err := etag.Guard(ctx, etag.Weak(g.placesVersion+":"+place.GetId()+":"+geomFmt.String())); err != nil {
		return nil, err
	}
	applyGeometryFormat(ctx, geomFmt, place.GetGeometry())
	return place, nil
}

// GetPlaceSummary returns the one-call place rollup (mode, severity summary,
// per-domain status, top events, the evacuation invariant, source health).
func (g *GridServer) GetPlaceSummary(ctx context.Context, req *gridv1.GetPlaceSummaryRequest) (*gridv1.PlaceSummary, error) {
	var hb hazardsBuilder
	if g.svc.Hazards != nil {
		hb = g.svc.Hazards // keep a nil *hazards.Service a nil interface
	}
	sum, err := g.svc.buildPlaceSummary(ctx, hb, req.GetPlace())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown place: %q", req.GetPlace())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	return sum, nil
}

// ResolvePlace maps a lat/lng or address to the containing places (most-specific
// first). lat/lng are validated exactly as the /v1 handler did; address geocodes
// via Census then runs the same point-in-polygon lookup.
func (g *GridServer) ResolvePlace(ctx context.Context, req *gridv1.ResolvePlaceRequest) (*gridv1.ResolvePlaceResponse, error) {
	geomFmt, err := parseGeometryFormat(req.GetGeometryFormat())
	if err != nil {
		return nil, invalidErr(err)
	}
	latS, lngS, addr := req.GetLat(), req.GetLng(), req.GetAddress()
	var lat, lng float64
	var matched string
	switch {
	case (latS != "" || lngS != "") && addr != "":
		return nil, status.Error(codes.InvalidArgument, "provide lat+lng or address, not both")
	case latS != "" || lngS != "":
		if latS == "" || lngS == "" {
			return nil, status.Error(codes.InvalidArgument, "lat and lng are both required")
		}
		var err error
		if lat, err = strconv.ParseFloat(latS, 64); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid lat: %q", latS)
		}
		if lng, err = strconv.ParseFloat(lngS, 64); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid lng: %q", lngS)
		}
		if math.IsNaN(lat) || lat < -90 || lat > 90 {
			return nil, status.Errorf(codes.InvalidArgument, "lat out of range [-90, 90]: %v", lat)
		}
		if math.IsNaN(lng) || lng < -180 || lng > 180 {
			return nil, status.Errorf(codes.InvalidArgument, "lng out of range [-180, 180]: %v", lng)
		}
	case addr != "":
		var err error
		lat, lng, matched, err = g.svc.Census.Geocode(ctx, addr)
		if errors.Is(err, census.ErrNoMatch) {
			return nil, notFoundErr("no address match for %q — try adding city/state/zip, or resolve by lat/lng", addr)
		}
		if err != nil {
			logError(ctx, "gridapi: census geocode failed", err)
			return nil, status.Error(codes.Unavailable, "address geocoder unavailable; retry later or resolve by lat/lng")
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "lat+lng or address query parameters required")
	}

	places, err := g.svc.Store.PlacesContaining(ctx, lat, lng)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	applyGeometryFormat(ctx, geomFmt, placeGeometries(places)...)
	return &gridv1.ResolvePlaceResponse{
		Query:  &gridv1.ResolveQuery{Lat: lat, Lng: lng, MatchedAddress: matched},
		Places: places,
	}, nil
}

// ListScanners returns Broadcastify scanner feeds. ?place naming an area serves
// that area's feeds; any other place (or none) serves every area's feeds deduped.
func (g *GridServer) ListScanners(ctx context.Context, req *gridv1.ListScannersRequest) (*gridv1.ScannerList, error) {
	feeds := g.svc.allScannerFeeds()
	if p := req.GetPlace(); p != "" {
		place, err := g.svc.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		if place.GetKind() == gridv1.PlaceKind_AREA {
			if area, ok := g.svc.areaByID(place.GetSlug()); ok {
				feeds = area.ScannerFeeds
			}
		}
	}
	out := &gridv1.ScannerList{Scanners: make([]*gridv1.Scanner, 0, len(feeds))}
	for _, f := range feeds {
		out.Scanners = append(out.Scanners, &gridv1.Scanner{
			FeedId:          f.FeedID,
			ChannelLabel:    f.ChannelLabel,
			Agency:          f.Agency,
			BroadcastifyUrl: "https://www.broadcastify.com/listen/feed/" + f.FeedID,
		})
	}
	return out, nil
}

// fireWeatherSlug maps the api FireWeatherState enum to the wire slug.
func fireWeatherSlug(s api.FireWeatherState) string {
	switch s {
	case api.FireWeatherState_NORMAL:
		return "normal"
	case api.FireWeatherState_ELEVATED:
		return "elevated"
	case api.FireWeatherState_RED_FLAG:
		return "red-flag"
	default:
		return ""
	}
}

// GetConditions returns current non-event state: per-location weather + the
// region's fire-weather classification, optionally scoped to a place's bbox.
// Weather alerts are dropped — they are events (/api/v1/events?layer=weather_alert).
func (g *GridServer) GetConditions(ctx context.Context, req *gridv1.GetConditionsRequest) (*gridv1.Conditions, error) {
	// Current weather (OpenWeatherMap) failing must not take the NWS
	// forecasts and fire weather down with it: serve those, and say plainly
	// that current conditions are unknown.
	weatherStatus := gridv1.SourceStatus_OK
	var staleAfter time.Duration
	if g.svc.Cfg != nil && g.svc.Cfg.Weather.RefreshInterval > 0 {
		staleAfter = g.svc.Cfg.Weather.RefreshInterval * 3 / 2
	}
	resp, err := g.svc.Weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		logError(ctx, "gridapi: current weather unavailable; serving forecasts only", err)
		weatherStatus = gridv1.SourceStatus_UNAVAILABLE
		resp = &api.ListWeatherResponse{FireWeather: g.svc.Weather.RegionFireWeather(ctx)}
	} else if lu := resp.GetLastUpdated(); lu != nil && staleAfter > 0 && g.svc.Now().Sub(lu.AsTime()) > staleAfter {
		weatherStatus = gridv1.SourceStatus_STALE
	}
	readAt := g.svc.Weather.ReadingTimes()
	data := resp.GetWeatherData()
	var locations []config.WeatherLocation
	if g.svc.Cfg != nil {
		locations = g.svc.Cfg.Weather.Locations
	}
	if p := req.GetPlace(); p != "" {
		box, ok, err := g.svc.placeBbox(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		data = filterWeather(data, locations, box, ok)
		locations = filterLocations(locations, box, ok)
	}

	out := &gridv1.Conditions{LastUpdated: resp.GetLastUpdated(), WeatherStatus: weatherStatus}
	for _, wd := range data {
		out.Weather = append(out.Weather, &gridv1.WeatherConditions{
			LocationId:           wd.GetLocationId(),
			LocationName:         wd.GetLocationName(),
			WeatherMain:          wd.GetWeatherMain(),
			WeatherDescription:   wd.GetWeatherDescription(),
			WeatherIcon:          wd.GetWeatherIcon(),
			TemperatureCelsius:   wd.GetTemperatureCelsius(),
			FeelsLikeCelsius:     wd.GetFeelsLikeCelsius(),
			HumidityPercent:      wd.GetHumidityPercent(),
			WindSpeedKmh:         wd.GetWindSpeedKmh(),
			WindDirectionDegrees: wd.GetWindDirectionDegrees(),
			VisibilityKm:         wd.GetVisibilityKm(),
			ObservedAt:           tsOrNil(readAt[wd.GetLocationId()]),
		})
		// A town whose fetch keeps failing rides forward in a fresh set:
		// the set's own time says OK, the town's says otherwise.
		if at, ok := readAt[wd.GetLocationId()]; ok && staleAfter > 0 && weatherStatus == gridv1.SourceStatus_OK &&
			g.svc.Now().Sub(at) > staleAfter {
			weatherStatus = gridv1.SourceStatus_STALE
		}
	}
	out.WeatherStatus = weatherStatus
	if fw := resp.GetFireWeather(); fw != nil {
		out.FireWeather = &gridv1.FireWeatherConditions{
			State:    fireWeatherSlug(fw.GetState()),
			Headline: fw.GetHeadline(),
			Zones:    fw.GetZones(),
		}
	}
	for _, l := range locations {
		wl := &gridv1.WeatherLocation{
			Id: l.ID, Name: l.Name, Zone: l.Zone,
			Latitude: l.Coordinates.Latitude, Longitude: l.Coordinates.Longitude,
		}
		if l.ElevationMeters > 0 {
			wl.ElevationMeters = wrapperspb.Int32(int32(math.Round(l.ElevationMeters)))
		}
		out.Locations = append(out.Locations, wl)
	}
	// Per-location forecast (fail-soft) for every location in scope. It used
	// to be joined to the current-weather set, so an OpenWeatherMap failure
	// hid NWS forecasts that had nothing to do with it.
	if fcs := g.svc.Weather.LocationForecasts(ctx); len(fcs) > 0 {
		for _, l := range locations {
			if f := fcs[l.ID]; f != nil {
				out.Forecast = append(out.Forecast, projectForecast(l.ID, f))
			}
		}
	}
	return out, nil
}

// filterLocations keeps the configured locations inside the place's box.
func filterLocations(locations []config.WeatherLocation, b bbox, ok bool) []config.WeatherLocation {
	if !ok {
		return nil
	}
	var out []config.WeatherLocation
	for _, l := range locations {
		if b.contains(l.Coordinates.Latitude, l.Coordinates.Longitude) {
			out = append(out, l)
		}
	}
	return out
}

func wrapI32(v *float64) *wrapperspb.Int32Value {
	if v == nil {
		return nil
	}
	return wrapperspb.Int32(int32(math.Round(*v)))
}

// wrapTenth rounds to 0.1 (mm amounts).
func wrapTenth(v *float64) *wrapperspb.DoubleValue {
	if v == nil {
		return nil
	}
	return wrapperspb.Double(math.Round(*v*10) / 10)
}

// projectForecast maps a client forecast onto the proto WeatherForecast. Rounded
// int32 scalars match WeatherConditions; absent min-humidity stays 0.
func projectForecast(locID string, f *nws.Forecast) *gridv1.WeatherForecast {
	wf := &gridv1.WeatherForecast{
		LocationId:      locID,
		Source:          f.Source,
		IssuedAt:        tsOrNil(f.IssuedAt),
		HorizonHours:    int32(f.HorizonHours),
		PeakWindGustKmh: int32(math.Round(f.PeakGustKmh)),
		PeakWindGustAt:  tsOrNil(f.PeakGustAt),
	}
	if f.HasMinHumidity {
		wf.MinHumidityPercent = int32(math.Round(f.MinHumidityPct))
	}
	for _, p := range f.Points {
		wf.Periods = append(wf.Periods, &gridv1.ForecastPeriod{
			Time:                 tsOrNil(p.Time),
			TemperatureCelsius:   int32(math.Round(p.TempC)),
			HumidityPercent:      int32(math.Round(p.HumidityPct)),
			WindSpeedKmh:         int32(math.Round(p.WindKmh)),
			WindDirectionDegrees: int32(math.Round(p.WindDirDeg)),
			WindGustKmh:          int32(math.Round(p.WindGustKmh)),

			PrecipitationProbabilityPercent: wrapI32(p.PopPct),
			PrecipitationMm:                 wrapTenth(p.QpfMm),
			SnowfallMm:                      wrapTenth(p.SnowMm),
			SnowLevelMeters:                 wrapI32(p.SnowLevelM),
			SkyCoverPercent:                 wrapI32(p.SkyPct),
			Weather:                         p.Weather,
		})
	}
	wf.GridElevationMeters = wrapI32(f.GridElevationM)
	for _, d := range f.Daily {
		wf.Daily = append(wf.Daily, &gridv1.DailyForecastPeriod{
			Name: d.Name, Start: tsOrNil(d.Start), End: tsOrNil(d.End), IsDaytime: d.IsDaytime,
			TemperatureCelsius:              int32(math.Round(d.TempC)),
			PrecipitationProbabilityPercent: wrapI32(d.PopPct),
			WindSpeed:                       d.WindSpeed,
			WindDirection:                   d.WindDirection,
			ShortForecast:                   d.ShortForecast,
			DetailedForecast:                d.DetailedForecast,
		})
	}
	return wf
}

func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// ListSources returns the source registry with per-source health (OK / STALE /
// UNAVAILABLE) — the honesty mechanism clients key layer trust off.
func (g *GridServer) ListSources(ctx context.Context, _ *gridv1.ListSourcesRequest) (*gridv1.SourceList, error) {
	sources, err := g.svc.Store.ListSources(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	return &gridv1.SourceList{Sources: sources}, nil
}
