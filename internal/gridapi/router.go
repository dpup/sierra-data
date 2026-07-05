package gridapi

import (
	"net/http"
	"net/url"
	"strings"
)

// ServeHTTP routes GET/HEAD requests under /v1/ by path segments (the hazards
// pattern — no ServeMux, so ".geojson" suffixes and colon-bearing event ids
// route cleanly). Anything unmatched is a 404 google.rpc.Status body.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// HEAD must be supported wherever GET is (RFC 9110 §9.1) — monitors and
	// link checkers probe with it. net/http discards the body automatically,
	// so HEAD simply runs the GET handlers.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		methodNotAllowed(w, r.Method)
		return
	}

	// Split the ESCAPED path and unescape each segment afterwards: event ids
	// can legitimately contain "/" (evac: ids embed upstream zone names
	// verbatim), which reaches us as %2F and must not act as a separator —
	// splitting the pre-decoded r.URL.Path would make such ids unroutable.
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), HandlerPrefix), "/")
	// Empty segments (trailing slash, "//", bare "/v1/") match nothing.
	for i, p := range parts {
		if p == "" {
			notFound(w, "not found: "+r.URL.Path)
			return
		}
		dec, err := url.PathUnescape(p)
		if err != nil {
			notFound(w, "not found: "+r.URL.Path)
			return
		}
		parts[i] = dec
	}

	switch parts[0] {
	case "events":
		switch len(parts) {
		case 1: // GET /v1/events
			s.serveEvents(w, r)
			return
		case 2: // GET /v1/events/{id}
			s.serveEvent(w, r, parts[1])
			return
		case 3: // GET /v1/events/{id}/history
			if parts[2] == "history" {
				s.serveEventHistory(w, r, parts[1])
				return
			}
		}
	case "history":
		if len(parts) == 1 { // GET /v1/history
			s.serveHistory(w, r)
			return
		}
	case "places":
		switch len(parts) {
		case 1: // GET /v1/places
			s.servePlaces(w, r)
			return
		case 2:
			if parts[1] == "resolve" { // GET /v1/places/resolve
				s.serveResolve(w, r)
				return
			}
			// GET /v1/places/{place}
			s.servePlace(w, r, parts[1])
			return
		case 3: // GET /v1/places/{place}/summary
			if parts[2] == "summary" {
				s.serveSummary(w, r, parts[1])
				return
			}
		case 4: // GET /v1/places/{place}/map/{layer}.geojson
			if parts[2] == "map" && strings.HasSuffix(parts[3], ".geojson") {
				s.serveMapLayer(w, r, parts[1], strings.TrimSuffix(parts[3], ".geojson"))
				return
			}
		}
	case "roads":
		switch len(parts) {
		case 1: // GET /v1/roads
			s.serveRoads(w, r, "")
			return
		case 2: // GET /v1/roads/{id}
			s.serveRoads(w, r, parts[1])
			return
		}
	case "weather":
		switch len(parts) {
		case 1: // GET /v1/weather
			s.serveWeather(w, r, "")
			return
		case 2: // GET /v1/weather/{location}
			s.serveWeather(w, r, parts[1])
			return
		}
	case "scanners":
		if len(parts) == 1 { // GET /v1/scanners
			s.serveScanners(w, r)
			return
		}
	case "sources":
		if len(parts) == 1 { // GET /v1/sources
			s.serveSources(w, r)
			return
		}
	}
	notFound(w, "not found: "+r.URL.Path)
}
