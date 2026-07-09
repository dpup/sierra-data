# MCP endpoint design

**Status: implemented** at `/mcp` (`internal/mcp`), all phases. This doc is the
design of record; the notes below describe what shipped.

How The Grid would expose its hazard/roads/weather data to LLM agents via the
Model Context Protocol (MCP). The service is read-only and unauthenticated, so
the protocol maps almost 1:1 onto the existing `/api/v1` surface
(`docs/v2-api-spec.md`). The design work is **not** the plumbing — it's shaping
responses for LLMs (compact, geometry-free) and baking the fail-loud honesty
contract into every tool result so a model cannot turn "unknown" into
"all-clear."

## Fit

- **Read-only, no auth** → no OAuth, no write tools ever. Add basic per-IP rate
  limiting since it's public.
- **One store, one query layer** → MCP tools reuse the same store reads as the
  `internal/gridapi` `/api/v1` surface; the only new code is an LLM-shaped
  projection + tool schemas.
- **Pure-Go** → a pure-Go MCP SDK (`modelcontextprotocol/go-sdk` or
  `mark3labs/mcp-go`) keeps `CGO_ENABLED=0`.

## Transport & mounting

- **Remote MCP over Streamable HTTP**, mounted at `/mcp` exactly like `/api/v1`:
  `prefab.WithHTTPHandler("/mcp", mcpHandler)`. One URL —
  `https://data.sierragridteam.org/mcp` — no install for the agent.
- Use the current **Streamable HTTP** transport, not the deprecated SSE
  transport. Pin the SDK; the MCP spec is still moving.

## Common agent query patterns (design targets)

1. "Is it safe / what's happening near {address | town | lat,lng}?" — dominant.
2. "Any evacuations / active fires near {place}?" — layer-filtered listing.
3. "Details on {this fire}?" — one event, full.
4. "Road & weather on Hwy 4 / in Arnold?" — conditions.
5. "What area/town is {point} in?" — resolve a location to place handles.
6. "What places do you cover?" — discovery so the agent uses valid names.
7. "Is this data current?" — source freshness (honesty disclosure).

## Tool surface (6 core)

Each tool accepts a flexible `location` — a place slug/id **or** an address
**or** `lat,lng` — so agents don't need a resolve round-trip.

| Tool | Input | Returns (compact, no raw geometry) |
|------|-------|------------------------------------|
| `grid_situation` | `location` | `mode` (QUIET/WATCH/ACTIVE), per-domain status + counts, top headlines, **evacuation: `null`=unknown / `0` / `N`** with framing + Genasys link, source freshness. The flagship call. |
| `grid_events` | `location?`, `layer?`, `severity_min?`, `status?`, `since?`, `limit?`, `page_token?` | rows `{id, layer, kind, severity, headline, area_label, observed_at, source, canonical_url}`; geometry → centroid + `map_url`. |
| `grid_event` | `id` | full event: AI `summary`, verbatim `description`, severity/status, effective/expires, typed detail (acres/containment, evac level, magnitude…), provenance + `canonical_url`. |
| `grid_conditions` | `location?` | weather per location (temp/wind/visibility) + `fire_weather` state. (Road status/incidents are events: `grid_events` `layer=road_incident`.) |
| `grid_resolve` | `address` or `lat,lng` | containing places, most-specific-first `{slug, id, kind, name}`. |
| `grid_places` | `kind?`, `q?` | directory for discovery `{slug, id, kind, name, parent}`. |

**Optional / later:** `grid_sources` (feed health), `grid_history`
(after-action), MCP **resources** (the `/api/v1` reference as a readable resource),
and a "hazard briefing for {place}" **prompt** template.

## Response-shaping rules (the real work)

- **Strip geometry.** Polygons are huge and useless to an LLM — replace with a
  centroid + a `map_url`. This alone is the difference between a usable tool and
  a token bomb.
- **Compact envelope only** in list results; full detail only via `grid_event`.
  Default `limit`, keyset pagination.
- **Bake in the honesty contract** — non-negotiable given life-safety:
  - Every aggregate carries per-layer `source_status` (`OK|STALE|UNAVAILABLE`)
    and a top-level `caveats` line.
  - Evacuation returns explicit **`null` (unknown — source errored) vs `0`
    (confirmed none) vs `N`**, each with the required framing string + the
    Genasys link, so a model cannot render absence as safety.
  - A standing `disclaimer` field on every result: *"Reference only — verify
    with official sources; absence of data is not an all-clear."*
- **Timestamps** RFC 3339 + a relative hint; **units named** (`acres`,
  `depth_km`).
- Use MCP **structured output** plus a 1–2 line text summary per result.
- **Tool descriptions teach the contract** — MCP tools are selected by their
  descriptions, so each states the data, freshness, and reference-only/fail-loud
  posture up front.

## Effort & risk

- **~2–3 days.** Transport is boilerplate; handlers reuse existing store reads.
  New code = one LLM projection (an alternate to `gridapi.ProjectEvents` that
  drops geometry and adds caveats) + 6 schemas + descriptions.
- **Top risk is life-safety framing, not tech** — an LLM relaying evac/fire
  data. Mitigated entirely by the honesty rules above; over-invest there, not in
  tool count.
- Minor: public rate limiting; SDK/transport churn (pin, use Streamable HTTP).

## Phasing

1. **Phase 1** — `grid_situation`, `grid_events`, `grid_event`, `grid_resolve` +
   the honesty projection. ~80% of the value (the "what's happening near me"
   loop).
2. **Phase 2** — `grid_conditions`, `grid_places`, `grid_sources`.
3. **Phase 3** — `grid_history`, docs-as-resource, a briefing prompt.
