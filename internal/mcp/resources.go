package mcp

import "encoding/json"

// A single MCP resource: a compact reference the agent can read to understand
// the data model and — critically — the fail-loud honesty contract. Full docs
// live at https://data.sierragridteam.org/docs.

const docsResourceURI = "grid://reference"

const docsText = `# The Grid — MCP reference

Public, read-only, keyless hazard/road/weather data for the central Sierra
(Calaveras & Tuolumne counties). Reference only — verify with official sources.

## Tools
- grid_situation(location) — area mode + rollup for a place/address/lat,lng. Start here.
- grid_events(location?, layer?, severity_min?, status?, since?, limit?) — list active events.
- grid_event(id) — full detail on one event.
- grid_conditions(location?) — roads + weather conditions.
- grid_resolve(address | lat,lng) — location → containing places.
- grid_places(kind?, q?) — the place directory (discover valid slugs).
- grid_sources() — upstream feed health.
- grid_history(location?, layer?, from?, to?) — revision timeline.

## Severity scale (rank 0–4)
INFO < MINOR < MODERATE < SEVERE < EXTREME. Editorial response-urgency, not
magnitude — an Evacuation Order (EXTREME) outranks an M5 earthquake (SEVERE).

## Query patterns (how to answer common questions)
There is no per-road or per-incident-type filter. Answer those by scoping the
list broadly and processing the rows yourself:
- "What happened on <a specific road>?" — call grid_events with
  layer=road_incident scoped to the containing county/area/corridor (e.g.
  location "Calaveras County"), then filter the rows on the road name (it's in
  each row's headline / areaLabel). For the verbatim CHP report on a match,
  call grid_event(id).
- "How many <X> / how many collisions?" — grid_situation gives per-domain active
  counts for one place. For a count by sub-type or across an area, list with
  grid_events and tally locally; raise limit (max 200) and follow
  nextPageToken for the full set.
- Scope with the broadest place that covers the question (county/area), not a
  street: an address resolves to its containing places, so it scopes to the
  county/area, not to that one street.

## The honesty contract (must respect when relaying)
- Every source reports OK | STALE | UNAVAILABLE. UNAVAILABLE means the status is
  UNKNOWN, not clear. Never present absence of data as an all-clear.
- Evacuation: activeEvacuations is an explicit null (source errored → UNKNOWN,
  render "check Genasys"), 0 (Cal OES healthy, no active zones — a caveated
  confirmed-empty, not a guarantee), or N (active). null and 0 are different;
  never collapse them.
- Evacuation and life-safety text is reference-only; link the authoritative
  source and never paraphrase directive orders.`

func resourceList() []map[string]interface{} {
	return []map[string]interface{}{{
		"uri":         docsResourceURI,
		"name":        "The Grid API reference",
		"description": "Data model, severity scale, and the fail-loud honesty contract.",
		"mimeType":    "text/markdown",
	}}
}

func readResource(params json.RawMessage) rpcResponse {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcResponse{Error: &rpcError{errBadParams, "invalid params: " + err.Error()}}
	}
	if p.URI != docsResourceURI {
		return rpcResponse{Error: &rpcError{errBadParams, "resource not found: " + p.URI}}
	}
	return rpcResponse{Result: map[string]interface{}{
		"contents": []map[string]interface{}{{
			"uri":      docsResourceURI,
			"mimeType": "text/markdown",
			"text":     docsText,
		}},
	}}
}
