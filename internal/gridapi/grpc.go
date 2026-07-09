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

	"github.com/dpup/prefab/plugins/etag"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/census"
	"github.com/dpup/sierra-data/internal/store"
)

// RegisterGatewayRoutes mounts the two endpoints that stay hand-built (not proto)
// directly on the gateway mux, so they live under /api/v1 beside the proto RPCs
// with no routing collision:
//   - the place summary rollup — hand-built because active_evacuations must
//     serialize as an explicit JSON null (the life-safety invariant);
//   - the .geojson map layers — RFC 7946 geometry, which proto3 models poorly.
//
// Both keep their snake_case bodies (the plan summary shape; GeoJSON's RFC
// contract). Call from a prefab.WithGRPCGateway callback after registering the
// GridService handler.
func (g *GridServer) RegisterGatewayRoutes(mux *runtime.ServeMux) error {
	if err := mux.HandlePath("GET", "/api/v1/places/{place}/summary",
		func(w http.ResponseWriter, r *http.Request, pp map[string]string) {
			g.svc.serveSummary(w, r, pp["place"])
		}); err != nil {
		return err
	}
	return mux.HandlePath("GET", "/api/v1/places/{place}/map/{layer}",
		func(w http.ResponseWriter, r *http.Request, pp map[string]string) {
			g.svc.serveMapLayer(w, r, pp["place"], strings.TrimSuffix(pp["layer"], ".geojson"))
		})
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
func internalErr(ctx context.Context, err error) error {
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
// Endpoints are ported one at a time; see docs/grpc-gateway-migration-plan.md.
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
}

// NewGridServer wires the gRPC service over the existing gridapi Service.
func NewGridServer(svc *Service) *GridServer {
	return &GridServer{svc: svc, placesVersion: randomNonce()}
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

	// Filters are validated above (400/404 first); guard on a cheap global data
	// version + the filter set so a match skips the query entirely.
	dv, err := g.svc.Store.DataVersion(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	tag := weakListTag(g.placesVersion+"."+strconv.FormatInt(dv, 10),
		req.GetPlace(), strings.Join(req.GetLayer(), ","), strings.Join(req.GetStatus(), ","),
		req.GetSeverityMin(), req.GetSince(),
		strconv.Itoa(int(req.GetPageSize())), req.GetPageToken(), strconv.FormatBool(req.GetEnhancementIo()))
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the query
	}

	events, next, err := g.svc.Store.QueryEvents(ctx, eq)
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
	}
	stripEventsIO(events, req.GetEnhancementIo())
	return &gridv1.EventList{Events: events, NextPageToken: next}, nil
}

// GetEvent returns the current revision of one event. A cheap revision read
// answers a conditional GET (304) before the proto blob is loaded.
func (g *GridServer) GetEvent(ctx context.Context, req *gridv1.GetEventRequest) (*gridv1.Event, error) {
	rev, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	// The body depends on the revision and whether the large enhancement I/O
	// fields are included, so both key the validator.
	if err := etag.Guard(ctx, etag.Weak(fmt.Sprintf("%s:%d:%t", req.GetId(), rev, req.GetEnhancementIo()))); err != nil {
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
	return ev, nil
}

// GetEventHistory returns one event's revisions, newest first. Unknown ids 404
// (an empty history is indistinguishable from a revision-less event, which
// cannot exist).
func (g *GridServer) GetEventHistory(ctx context.Context, req *gridv1.GetEventHistoryRequest) (*gridv1.EventRevisionList, error) {
	// A cheap revision read doubles as the existence check (replacing a full
	// blob load) and the ETag validator: history grows only when the current
	// revision bumps; the page window and IO flag shape the body.
	rev, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	tag := etag.Weak(fmt.Sprintf("%s:%d:%d:%s:%t", req.GetId(), rev, req.GetPageSize(), req.GetPageToken(), req.GetEnhancementIo()))
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the history query + blob rehydrate
	}
	revs, next, err := g.svc.Store.EventHistory(ctx, req.GetId(), int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
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

	dv, err := g.svc.Store.DataVersion(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	tag := weakListTag(g.placesVersion+"."+strconv.FormatInt(dv, 10),
		req.GetPlace(), strings.Join(req.GetLayer(), ","), req.GetFrom(), req.GetTo(),
		strconv.Itoa(int(req.GetPageSize())), req.GetPageToken(), strconv.FormatBool(req.GetEnhancementIo()))
	if err := etag.Guard(ctx, tag); err != nil {
		return nil, err // 304 — skip the query
	}

	revs, next, err := g.svc.Store.QueryHistory(ctx, hq)
	if err != nil {
		return nil, tokenOrInternal(ctx, err)
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
	// The directory is static within a process; a match skips building the
	// (geometry-heavy) list.
	if err := etag.Guard(ctx, weakListTag(g.placesVersion, "kind="+req.GetKind(), "q="+req.GetQ())); err != nil {
		return nil, err
	}
	places, err := g.svc.Store.ListPlaces(ctx, kind, req.GetQ())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	return &gridv1.PlaceList{Places: places}, nil
}

// GetPlace returns one place by slug or namespaced id ("area:ebbetts-pass").
func (g *GridServer) GetPlace(ctx context.Context, req *gridv1.GetPlaceRequest) (*gridv1.Place, error) {
	place, err := g.svc.Store.GetPlace(ctx, req.GetPlace())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown place: %q", req.GetPlace())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	// Places are static within a process; the load above is a single indexed row,
	// so guard after it — a match still saves re-sending the geometry blob.
	if err := etag.Guard(ctx, etag.Weak(g.placesVersion+":"+place.GetId())); err != nil {
		return nil, err
	}
	return place, nil
}

// ResolvePlace maps a lat/lng or address to the containing places (most-specific
// first). lat/lng are validated exactly as the /v1 handler did; address geocodes
// via Census then runs the same point-in-polygon lookup.
func (g *GridServer) ResolvePlace(ctx context.Context, req *gridv1.ResolvePlaceRequest) (*gridv1.ResolvePlaceResponse, error) {
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
	resp, err := g.svc.Weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	data := resp.GetWeatherData()
	if p := req.GetPlace(); p != "" {
		box, ok, err := g.svc.placeBbox(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		data = filterWeather(data, g.svc.Cfg.Weather.Locations, box, ok)
	}

	out := &gridv1.Conditions{LastUpdated: resp.GetLastUpdated()}
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
		})
	}
	if fw := resp.GetFireWeather(); fw != nil {
		out.FireWeather = &gridv1.FireWeatherConditions{
			State:    fireWeatherSlug(fw.GetState()),
			Headline: fw.GetHeadline(),
			Zones:    fw.GetZones(),
		}
	}
	return out, nil
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
