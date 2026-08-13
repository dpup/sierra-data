package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error)
}

func (s *Server) toolList() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) rpcResponse {
	var p struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcResponse{Error: &rpcError{errBadParams, "invalid params: " + err.Error()}}
	}
	var t *tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			t = &s.tools[i]
			break
		}
	}
	if t == nil {
		// An unrecognized tool is a protocol error, not a tool-execution error
		// (MCP splits the two channels) — return a JSON-RPC error, not isError.
		return rpcResponse{Error: &rpcError{errBadParams, "unknown tool: " + p.Name}}
	}
	res, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		return toolResult(map[string]interface{}{"error": err.Error(), "disclaimer": disclaimer}, true)
	}
	if res == nil {
		res = map[string]interface{}{}
	}
	res["disclaimer"] = disclaimer
	return toolResult(res, false)
}

// toolResult packages a structured object as an MCP tool result — a text content
// block (indented JSON, for models that read text) plus structuredContent.
func toolResult(obj map[string]interface{}, isErr bool) rpcResponse {
	return rpcResponse{Result: map[string]interface{}{
		"content":           []map[string]interface{}{{"type": "text", "text": prettyJSON(obj)}},
		"structuredContent": obj,
		"isError":           isErr,
	}}
}

/* --------------------------------------------------------------- registry */

func (s *Server) registerTools() []tool {
	return []tool{
		{
			Name: "grid_situation",
			Description: "The headline call: what's happening at a place, address, or lat,lng in the " +
				"central Sierra (Calaveras/Tuolumne). Returns the area mode (QUIET/WATCH/ACTIVE), " +
				"per-domain status and counts (fire/evacuation/weather/roads/seismic/power), top headlines, " +
				"evacuation status (activeEvacuations is an explicit null when the source is UNAVAILABLE " +
				"= unknown, 0 when confirmed none, N when active — never treat null/absence as safe), and " +
				"per-source freshness. Reference only; verify with official sources.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"a place slug/id (e.g. \"ebbetts-pass\"), a street address, or \"lat,lng\""}},"required":["location"]}`),
			Handler:     s.handleSituation,
		},
		{
			Name: "grid_events",
			Description: "List active events (wildfire, evacuation, weather alert, earthquake, road " +
				"incident, power outage / Public Safety Power Shutoff, and mesh-node presence) — " +
				"optionally scoped to a location and filtered by " +
				"layer/severity/status/time. " +
				"Compact rows; call grid_event for full detail (incl. the verbatim report) on one. " +
				"Geometry is omitted (a location centroid is included). " +
				"There is no per-road or per-incident-type filter: to answer \"what happened on <a road>?\", " +
				"a sub-type question (collisions vs. hazards vs. closures), or a count/tally, scope broadly " +
				"— pass a county/area name or a corridor slug (e.g. location \"Calaveras County\", layer " +
				"\"road_incident\") — and filter or count the returned rows yourself; each row's headline and " +
				"areaLabel carry the road name and incident type. " +
				"MeshCore mesh-node presence is layer \"mesh\" (legacy alias \"network\"): one INFO row per " +
				"node, the node's name in headline/areaLabel and its pubkey + radio telemetry in detail. To " +
				"find or check a specific node (e.g. one named \"SIERRA…\"), list layer=mesh — unscoped is " +
				"fine, the mesh spans places — and match the name in the rows; a node's observedAt is when the " +
				"Grid last heard it (the trustworthy freshness signal — detail.mesh.telemetry.lastAdvertAt " +
				"is the node's self-reported advert time and is diagnostic only, clocks skew). Absence of a " +
				"node here is not proof it's down. Raise limit (max 200) and follow " +
				"nextPageToken to get the full set. (grid_situation already gives per-domain active counts " +
				"for a single place.)",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id, address, or lat,lng to scope to"},"layer":{"type":"string","description":"wildfire|evacuation|weather_alert|earthquake|road_incident|power|mesh (power = PG&E outages AND Public Safety Power Shutoffs, separated by each row's category: unplanned|planned|psps. Most outages are tiny — the statewide median affects ONE customer — so pair layer=power with severity_min unless you want every service call. mesh = MeshCore node presence; \"network\" is a legacy alias)"},"severity_min":{"type":"string","description":"INFO|MINOR|MODERATE|SEVERE|EXTREME"},"status":{"type":"string","description":"default ACTIVE,SCHEDULED; pass RESOLVED/EXPIRED to see closed"},"since":{"type":"string","description":"RFC3339; only events observed at/after"},"limit":{"type":"integer"},"page_token":{"type":"string"}}}`),
			Handler:     s.handleEvents,
		},
		{
			Name:        "grid_event",
			Description: "Full detail on one event by id (from grid_events): headline, AI summary, verbatim description, severity/status, typed detail (acres/containment, evac level, magnitude…), provenance and the canonical source link.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"event id, e.g. calfire:2026-priest-fire"}},"required":["id"]}`),
			Handler:     s.handleEvent,
		},
		{
			Name:        "grid_conditions",
			Description: "Current weather and fire-weather state per location — temperature, wind, visibility, and the fire-weather classification — optionally scoped to a location. Conditions, not events. (Road status/incidents are events: grid_events with layer=road_incident.)",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id, address, or lat,lng to scope to"}}}`),
			Handler:     s.handleConditions,
		},
		{
			Name:        "grid_resolve",
			Description: "Resolve an address or lat,lng to the places that contain it (most-specific first: site, evac zone, town, corridor, county, area). Use the returned slug with the other tools.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"},"lat":{"type":"number"},"lng":{"type":"number"}}}`),
			Handler:     s.handleResolve,
		},
		{
			Name:        "grid_places",
			Description: "The place directory for discovery — areas, counties, towns, evacuation zones, corridors — so you can find valid place slugs. Filter by kind or a name query.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"AREA|COUNTY|TOWN|EVAC_ZONE|CORRIDOR|SITE"},"q":{"type":"string","description":"name substring"}}}`),
			Handler:     s.handlePlaces,
		},
		{
			Name:        "grid_sources",
			Description: "Feed health for every upstream source (OK/STALE/UNAVAILABLE, last success, last error) — use it to disclose data gaps. A source that is UNAVAILABLE means its hazard status is unknown, not clear.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler:     s.handleSources,
		},
		{
			Name:        "grid_history",
			Description: "After-action / timeline: the chronological revisions of events over a time range (how a fire grew, when a warning escalated, when it was resolved). Optionally scope by location and layer.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"},"layer":{"type":"string"},"from":{"type":"string","description":"RFC3339 start"},"to":{"type":"string","description":"RFC3339 end"}}}`),
			Handler:     s.handleHistory,
		},
	}
}

/* --------------------------------------------------------------- handlers */

func (s *Server) handleSituation(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	loc := argStr(args, "location")
	if loc == "" {
		return nil, fmt.Errorf("location is required (a place slug, address, or lat,lng)")
	}
	slug, resolved, err := s.resolvePlace(ctx, loc)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		return unresolved(loc), nil
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places/"+url.PathEscape(slug)+"/summary")
	if err != nil {
		return nil, fmt.Errorf("summary for %q unavailable: %w", slug, err)
	}
	body["resolved_place"] = slug
	if len(resolved) > 1 {
		body["also_contains"] = compactPlaceRefs(resolved)
	}
	return body, nil
}

func (s *Server) handleEvents(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	for _, k := range []string{"layer", "severity_min", "status", "since", "page_token"} {
		if v := argStr(args, k); v != "" {
			q.Set(k, v)
		}
	}
	if n := argInt(args, "limit"); n > 0 {
		q.Set("page_size", strconv.Itoa(n))
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/events?"+q.Encode())
	if err != nil {
		return nil, err
	}
	events := leanEvents(asArray(body["events"]))
	out := map[string]interface{}{"events": events, "count": len(events)}
	if t, ok := body["nextPageToken"]; ok && t != "" {
		out["nextPageToken"] = t
	}
	return out, nil
}

func (s *Server) handleEvent(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	id := argStr(args, "id")
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	body, code, err := s.callAPI(ctx, "/api/v1/events/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	if code == 404 {
		return nil, fmt.Errorf("no event with id %q", id)
	}
	if code != 200 {
		return nil, fmt.Errorf("event lookup failed (HTTP %d)", code)
	}
	return leanEvent(body, true), nil
}

func (s *Server) handleConditions(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	path := "/api/v1/conditions"
	if e := q.Encode(); e != "" {
		path += "?" + e
	}
	return s.callAPIJSON(ctx, path)
}

func (s *Server) handleResolve(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if addr := argStr(args, "address"); addr != "" {
		q.Set("address", addr)
	} else if lat, latOK := argFloat(args, "lat"); latOK {
		lng, lngOK := argFloat(args, "lng")
		if !lngOK {
			return nil, fmt.Errorf("lat requires lng")
		}
		q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
		q.Set("lng", strconv.FormatFloat(lng, 'f', -1, 64))
	} else {
		return nil, fmt.Errorf("provide address, or lat and lng")
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places:resolve?"+q.Encode())
	if err != nil {
		return nil, err // surface geocode/lookup failure as a tool error (isError)
	}
	return map[string]interface{}{"places": leanPlaces(asArray(body["places"]))}, nil
}

func (s *Server) handlePlaces(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if k := argStr(args, "kind"); k != "" {
		q.Set("kind", k)
	}
	if qq := argStr(args, "q"); qq != "" {
		q.Set("q", qq)
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places?"+q.Encode())
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"places": leanPlaces(asArray(body["places"]))}, nil
}

func (s *Server) handleSources(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	// grid_sources exists to disclose feed health, so a failed /api/v1/sources call
	// must surface as an error, not a body that looks like a health rollup.
	return s.callAPIJSON(ctx, "/api/v1/sources")
}

func (s *Server) handleHistory(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	for _, k := range []string{"layer", "from", "to"} {
		if v := argStr(args, k); v != "" {
			q.Set(k, v)
		}
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/history?"+q.Encode())
	if err != nil {
		return nil, err
	}
	revs := asArray(body["revisions"])
	for _, r := range revs {
		if m, ok := r.(map[string]interface{}); ok {
			if ev, ok := m["event"].(map[string]interface{}); ok {
				leanEvent(ev, false)
			}
		}
	}
	out := map[string]interface{}{"revisions": revs, "count": len(revs)}
	if t, ok := body["nextPageToken"]; ok && t != "" {
		out["nextPageToken"] = t
	}
	return out, nil
}

/* ---------------------------------------------------------- resolve helper */

// resolvePlace turns a free-form location (place slug/id, "lat,lng", or address)
// into a place slug plus the full containing-places list (most-specific first).
func (s *Server) resolvePlace(ctx context.Context, location string) (string, []interface{}, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", nil, nil
	}
	// 1. "lat,lng"
	if lat, lng, ok := parseLatLng(location); ok {
		body, _, err := s.callAPI(ctx, "/api/v1/places:resolve?"+url.Values{"lat": {lat}, "lng": {lng}}.Encode())
		if err != nil {
			return "", nil, err
		}
		places := asArray(body["places"])
		return firstSlug(places), places, nil
	}
	// 2. a place slug/id (single token, no spaces) — try a direct lookup. This is
	// one candidate strategy, so a transient failure falls through to the next.
	if !strings.ContainsAny(location, " ,") {
		if body, code, err := s.callAPI(ctx, "/api/v1/places/"+url.PathEscape(location)); err == nil && code == 200 {
			if slug := argStr(body, "slug"); slug != "" {
				return slug, []interface{}{leanPlace(body)}, nil
			}
		}
	}
	// 3. name search in the directory — resolves town/county/area names like
	// "Arnold" or "Calaveras County" that the street-address geocoder can't.
	for _, term := range nameTerms(location) {
		body, _, err := s.callAPI(ctx, "/api/v1/places?"+url.Values{"q": {term}}.Encode())
		if err != nil {
			return "", nil, err
		}
		if places := asArray(body["places"]); len(places) > 0 {
			return firstSlug(places), leanPlaces(places), nil
		}
	}
	// 4. street-address geocode (Census)
	body, _, err := s.callAPI(ctx, "/api/v1/places:resolve?"+url.Values{"address": {location}}.Encode())
	if err != nil {
		return "", nil, err
	}
	places := asArray(body["places"])
	return firstSlug(places), places, nil
}

// unresolved is the standard result when a location argument can't be mapped to
// a covered place — one shape across all location-scoped tools.
func unresolved(loc string) map[string]interface{} {
	return map[string]interface{}{
		"resolved": false,
		"message":  fmt.Sprintf("could not resolve %q to a covered place. This service only covers the central Sierra (Calaveras & Tuolumne). Try grid_places to see coverage.", loc),
	}
}

func firstSlug(places []interface{}) string {
	if len(places) == 0 {
		return ""
	}
	if m, ok := places[0].(map[string]interface{}); ok {
		if s := argStr(m, "slug"); s != "" {
			return s
		}
		return argStr(m, "id")
	}
	return ""
}

// compactPlaceRefs reduces resolved places to {slug,kind,name} refs.
func compactPlaceRefs(places []interface{}) []interface{} {
	out := make([]interface{}, 0, len(places))
	for _, p := range places {
		if m, ok := p.(map[string]interface{}); ok {
			out = append(out, map[string]interface{}{"slug": m["slug"], "kind": m["kind"], "name": m["name"]})
		}
	}
	return out
}

/* ---------------------------------------------------------------- arg utils */

func argStr(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func argInt(args map[string]interface{}, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func argFloat(args map[string]interface{}, key string) (float64, bool) {
	if args == nil {
		return 0, false
	}
	switch v := args[key].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// nameTerms yields directory-search terms for a location string: the whole
// string, and (if comma-separated, e.g. "Arnold, CA") the part before the first
// comma. Deduped, non-empty.
func nameTerms(location string) []string {
	terms := []string{location}
	if i := strings.Index(location, ","); i > 0 {
		if head := strings.TrimSpace(location[:i]); head != "" && head != location {
			terms = append(terms, head)
		}
	}
	return terms
}

// parseLatLng recognizes a "lat,lng" pair with both in valid ranges.
func parseLatLng(s string) (lat, lng string, ok bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return "", "", false
	}
	a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || a < -90 || a > 90 || b < -180 || b > 180 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}
