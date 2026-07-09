package gridapi

import (
	"context"
	"errors"
	"math"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/census"
	"github.com/dpup/sierra-data/internal/store"
)

// notFoundErr / invalidErr / internalErr map store/parse failures to grpc status
// codes (the gateway renders these as google.rpc.Status with the mapped HTTP
// status). resolvePlaceID looks up a place filter, returning NotFound on miss.
func notFoundErr(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}
func invalidErr(err error) error  { return status.Error(codes.InvalidArgument, err.Error()) }
func internalErr(err error) error { return status.Error(codes.Internal, err.Error()) }
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
	place, err := g.svc.Store.GetPlace(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", notFoundErr("unknown place: %q", key)
	}
	if err != nil {
		return "", internalErr(err)
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
}

// NewGridServer wires the gRPC service over the existing gridapi Service.
func NewGridServer(svc *Service) *GridServer {
	return &GridServer{svc: svc}
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

	events, next, err := g.svc.Store.QueryEvents(ctx, eq)
	if err != nil {
		return nil, tokenOrInternal(err)
	}
	stripEventsIO(events, req.GetEnhancementIo())
	return &gridv1.EventList{Events: events, NextPageToken: next}, nil
}

// GetEvent returns the current revision of one event.
func (g *GridServer) GetEvent(ctx context.Context, req *gridv1.GetEventRequest) (*gridv1.Event, error) {
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
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
	if _, err := g.svc.Store.GetEvent(ctx, req.GetId()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown event: %q", req.GetId())
		}
		return nil, internalErr(err)
	}
	revs, next, err := g.svc.Store.EventHistory(ctx, req.GetId(), int(req.GetPageSize()), req.GetPageToken())
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

	revs, next, err := g.svc.Store.QueryHistory(ctx, hq)
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
	places, err := g.svc.Store.ListPlaces(ctx, kind, req.GetQ())
	if err != nil {
		return nil, internalErr(err)
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
		return nil, internalErr(err)
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
		return nil, internalErr(err)
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
			return nil, internalErr(err)
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
		return nil, internalErr(err)
	}
	data := resp.GetWeatherData()
	if p := req.GetPlace(); p != "" {
		box, ok, err := g.svc.placeBbox(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(err)
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
		return nil, internalErr(err)
	}
	return &gridv1.SourceList{Sources: sources}, nil
}
