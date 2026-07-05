package gridapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/census"
	"github.com/dpup/info.ersn.net/server/internal/store"
)

// servePlaces handles GET /v1/places with the kind (enum name or lowercase,
// case-insensitive) and q (case-insensitive name substring) filters.
func (s *Service) servePlaces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED
	if v := q.Get("kind"); v != "" {
		k, err := parseKind(v)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		kind = k
	}
	places, err := s.Store.ListPlaces(r.Context(), kind, q.Get("q"))
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	writeMessage(w, r, &gridv1.PlaceList{Places: places}, maxAgeEntities)
}

// servePlace handles GET /v1/places/{place}; {place} is a slug or a
// namespaced id ("area:calaveras") — the store disambiguates on ':'.
func (s *Service) servePlace(w http.ResponseWriter, r *http.Request, key string) {
	place, err := s.Store.GetPlace(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, fmt.Sprintf("unknown place: %q", key))
		return
	}
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	writeMessage(w, r, place, maxAgeEntities)
}

// resolveOut is the hand-built GET /v1/places/resolve response: protojson of
// PlaceList cannot carry the resolved coordinates / matched address, so the
// places are embedded as raw protojson objects beside a query echo. All
// snake_case; matched_address appears only on the address path.
//
//	{"query":{"lat":38.2,"lng":-120.3,"matched_address":"..."},"places":[{...Place protojson...}]}
//
// Places are ordered most-specific-first: SITE, EVAC_ZONE, TOWN, CORRIDOR,
// COUNTY, AREA (plan §2.4; the store's PlacesContaining owns the ordering).
// This endpoint is JSON-only — Accept: application/proto is ignored because
// the wrapper is not a proto message.
type resolveOut struct {
	Query  resolveQuery      `json:"query"`
	Places []json.RawMessage `json:"places"`
}

type resolveQuery struct {
	Lat            float64 `json:"lat"`
	Lng            float64 `json:"lng"`
	MatchedAddress string  `json:"matched_address,omitempty"`
}

// serveResolve handles GET /v1/places/resolve with either lat+lng (pure
// point-in-polygon, no network) or address (Census geocode, then PIP).
func (s *Service) serveResolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	latS, lngS, addr := q.Get("lat"), q.Get("lng"), q.Get("address")

	var lat, lng float64
	var matched string
	switch {
	case (latS != "" || lngS != "") && addr != "":
		badRequest(w, "provide lat+lng or address, not both")
		return
	case latS != "" || lngS != "":
		if latS == "" || lngS == "" {
			badRequest(w, "lat and lng are both required")
			return
		}
		var err error
		if lat, err = strconv.ParseFloat(latS, 64); err != nil {
			badRequest(w, fmt.Sprintf("invalid lat: %q", latS))
			return
		}
		if lng, err = strconv.ParseFloat(lngS, 64); err != nil {
			badRequest(w, fmt.Sprintf("invalid lng: %q", lngS))
			return
		}
		if lat < -90 || lat > 90 {
			badRequest(w, fmt.Sprintf("lat out of range [-90, 90]: %v", lat))
			return
		}
		if lng < -180 || lng > 180 {
			badRequest(w, fmt.Sprintf("lng out of range [-180, 180]: %v", lng))
			return
		}
	case addr != "":
		var err error
		lat, lng, matched, err = s.Census.Geocode(ctx, addr)
		if errors.Is(err, census.ErrNoMatch) {
			notFound(w, fmt.Sprintf("no address match for %q — try adding city/state/zip, or resolve by lat/lng", addr))
			return
		}
		if err != nil {
			// The geocoder is a best-effort external dependency: log the real
			// error, tell the client to retry or fall back to coordinates.
			logError(ctx, "gridapi: census geocode failed", err)
			unavailable(w, "address geocoder unavailable; retry later or resolve by lat/lng")
			return
		}
	default:
		badRequest(w, "lat+lng or address query parameters required")
		return
	}

	places, err := s.Store.PlacesContaining(ctx, lat, lng)
	if err != nil {
		internal(ctx, w, err)
		return
	}

	out := resolveOut{
		Query:  resolveQuery{Lat: lat, Lng: lng, MatchedAddress: matched},
		Places: make([]json.RawMessage, 0, len(places)), // non-nil: renders [] not null
	}
	for _, p := range places {
		raw, err := jsonOpts.Marshal(p)
		if err != nil {
			internal(ctx, w, err)
			return
		}
		out.Places = append(out.Places, raw)
	}
	body, err := json.Marshal(out)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	writeJSON(w, r, body, contentTypeJSON, maxAgeEntities)
}

// parseKind maps a place-kind param (enum name, case-insensitive, so
// lowercase "town"/"evac_zone" work) onto the enum; unknown or UNSPECIFIED
// values are rejected — omit the param to mean "all kinds".
func parseKind(v string) (gridv1.PlaceKind, error) {
	n, ok := gridv1.PlaceKind_value[strings.ToUpper(strings.TrimSpace(v))]
	if !ok || n == int32(gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED) {
		return 0, fmt.Errorf("unknown kind: %q", v)
	}
	return gridv1.PlaceKind(n), nil
}
