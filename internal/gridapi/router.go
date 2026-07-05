package gridapi

import (
	"net/http"
	"strings"
)

// ServeHTTP routes GET requests under /v1/ by path segments (the hazards
// pattern — no ServeMux, so ".geojson" suffixes and colon-bearing event ids
// route cleanly). Anything unmatched is a 404 google.rpc.Status body.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		methodNotAllowed(w, r.Method)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, HandlerPrefix), "/")
	// Empty segments (trailing slash, "//", bare "/v1/") match nothing.
	for _, p := range parts {
		if p == "" {
			notFound(w, "not found: "+r.URL.Path)
			return
		}
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

// serveSummary handles GET /v1/places/{place}/summary.
//
// TODO(T12b): real implementation lands in summary.go (owned by the T12b
// agent, which replaces this stub); routed here so the URL space is complete
// and the package compiles standalone.
func (s *Service) serveSummary(w http.ResponseWriter, r *http.Request, place string) {
	notImplemented(w, "summary not implemented yet")
}

// serveMapLayer handles GET /v1/places/{place}/map/{layer}.geojson.
//
// TODO(T12b): real implementation lands in maplayers.go (store projection for
// event layers, hazards delegation for condition layers); this stub keeps the
// route wired until then.
func (s *Service) serveMapLayer(w http.ResponseWriter, r *http.Request, place, layer string) {
	notImplemented(w, "map layers not implemented yet")
}
