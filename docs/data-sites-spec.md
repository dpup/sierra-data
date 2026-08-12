# data.sierragridteam.org — Site Spec (Draft v0.1)

> **Visual design (2026-08-06): the "broadsheet" redesign.** The site moved off
> the dark dev-console look to a paper/ink split — paper (`#fbfbfa` — warm
> `#f4f1ea` until 2026-08-11) for reading,
> ink (`#14161a`) for anything that *is* the API (the front-page deck, response
> panes, code, the sidebar, map frames). Archivo carries display type, IBM Plex
> Sans prose, IBM Plex Mono every value. The design system, its two severity
> ramps, and the fail-loud obligations the UI must honour are documented in
> **`web/CLAUDE.md`** — read that before changing anything under `web/`. The
> principles and information architecture below are unchanged by the redesign.

A light frontend over the Grid Info Service (`grid-info-api-spec.md`). Dual duty:
**documentation of the data APIs** and **nerdy investigation of the data itself** —
and the design premise is that these are the same thing done well once.

Audience: SIERRA operators, contributors, curious residents, and anyone who might
build on the feeds. Not the resident-facing site; sierragridteam.org stays calm and
question-oriented, data. is dense and endpoint-oriented.

---

## 1. Principles

1. **Unprivileged client.** The site consumes only the public API. No backdoor
   queries, no internal endpoints. A page that can't be built from public endpoints
   is a defect in the API. Enforced structurally: the site is static assets; every
   data fetch happens in the browser through the same CORS surface any third party
   would use.
2. **Docs are live.** There is no separate "reference" and "explorer" — every
   documented endpoint renders as a page whose examples are working links into
   live data, and every explorer view displays the request that produced it
   (`GET /v1/events?place=ebbetts-pass&layer=wildfire`) as copyable curl/URL. Reading
   the docs *is* using the API; using the explorer *teaches* the API.
3. **URL-addressable state.** All explorer state (filters, time ranges, selected
   event) lives in the query string. Any view is shareable and bookmarkable —
   "look at this weird revision sequence" is a link, which is how operators will
   actually use it during an incident post-mortem.
4. **Read-only, no accounts, no state.** Nothing to secure, nothing to back up.
5. **Cheap.** Static hosting + the already-cacheable API. The site adds zero
   marginal backend cost.

---

## 2. Information architecture

Six sections, one per API concept. Each section page opens with the endpoint
reference (paths, parameters, response shape) and is followed immediately by the
live explorer for that endpoint.

### `/` — Home
Service identity, one-paragraph orientation, and a live status strip: per-source
health dots from `/v1/sources`, the current mode for the primary service areas from
`/v1/places/{area}/summary`, and generated-at freshness. Links into each section.
This page doubles as the ops glance — if something's red, it's visible in one load.

### `/sources` — Feed health
The board: every source with poll interval, last success/attempt, staleness state
(OK/STALE/UNAVAILABLE), last error text, attribution, and upstream link. Sorted
unhealthy-first. This is the monitoring dashboard the backend otherwise wouldn't
have — a browser tab on this page is the ops tooling.

### `/events` — Event explorer
Filter bar mapping 1:1 to `/v1/events` parameters: place, layer(s), status,
severity_min, since. Results as a dense table (severity chip, headline, place
labels, source, observed_at, revision count), paginated via page_token.

`/events/{id}` — Event detail: full envelope + typed detail rendered as a
definition list, geometry on a small map, provenance block, and the **revision
timeline** — every revision with observed/ingested timestamps and a field-level
diff between consecutive revisions (client-side diff of the protojson). Raw
protojson toggle on everything. If the event was AI-enhanced, the enhancement
badge shows model + fields, with the verbatim original alongside.

### `/history` — Archive browser
Time-range + place + layer query over `/v1/history`. Renders as a chronological
feed of revisions — the after-action review view. A date-range permalink of an
incident's whole arc is the shareable artifact here.

### `/places` — Place directory
Browse by kind (areas, counties, towns, zones, corridors, sites); each place page
shows geometry on a map, parent/children, and live links: "active events here,"
"summary here." Plus the **resolve tester**: click the map or enter an address →
`/v1/places/resolve` → containing places. This page is secretly the QA tool for
the zone import.

### `/map` — Layer previews
Per-layer GeoJSON rendered on MapLibre with the `metadata` member (source_status,
generated_at, attribution) displayed prominently — the honest-freshness contract,
visualized. Layer picker + place picker; the map URL shown is the exact `.geojson`
URL a third-party map client would use.

### `/docs` — Reference
Generated endpoint reference: proto-backed routes from the OpenAPI the gateway
already emits; the GeoJSON projections and their envelope documented by hand
(port §4 of the hazard-aggregation design doc — it's already written). Every
example is a live link per principle 2. Includes the severity scale + color ramp
table, the place id scheme, and the evac fail-loud contract — the things a
consumer must not get wrong.

---

## 3. Presentation

Utilitarian and dense: monospace-leaning, tables over cards, severity color chips
using the canonical ramp from the API spec, timestamps always shown with relative
+ absolute. Dark-mode default is appropriate for the audience. No hero images, no
marketing. The aesthetic north star is "instrument panel," not "product page."

Every page footer: "This site is an unprivileged client of the public API —
[view the request behind this page]."

---

## 4. Implementation

- **Static site** (same hosting pattern as sierragridteam.org), vanilla or
  minimal-framework JS; MapLibre GL for geometry; no build-time data dependency —
  everything fetched client-side from `data.sierragridteam.org/v1/...`.
- Site and API share the domain, so CORS is trivially satisfied for the site
  itself while remaining open for third parties per the API spec.
- The revision diff is client-side JSON diffing — no API support needed.
- `/docs` generation: build step renders the gateway's OpenAPI JSON to HTML;
  hand-written pages for the GeoJSON contract.

## 5. Phasing

- **M0:** Home + `/sources`. One page of real ops value; exercises `/v1/sources`
  and `/summary` end to end.
- **M1:** `/events` explorer + event detail with revision timeline. The core
  investigation loop; first real test of pagination and filter ergonomics.
- **M2:** `/places` browser + resolve tester. Lands with the zone import; QA tool
  for it.
- **M3:** `/map` layer previews.
- **M4:** `/history` archive browser.
- **M5:** `/docs` generated reference.

Each milestone is a forcing function on its endpoints: M1 will surface whatever is
awkward about `/events` filtering before any external consumer hits it.

## 6. Non-goals

- Resident-facing presentation (main site's job), charts/dashboards for the
  public, alert subscriptions UI (phase 2 of the API, and likely main-site).
- Auth, saved views, user preferences — URL state is the only state.
- Uptime/analytics tooling beyond what `/sources` already shows.
