# Changelog

All notable **API-facing** changes to The Grid (the S.I.E.R.R.A data service at
`data.sierragridteam.org`). This is the document to read before updating a
consuming site (e.g. ersn.net, sierragridteam.org).

There are no formal releases — the service deploys from `main`. Each entry below
is timestamped; add a new dated section at the top when the API surface changes.
The API is JSON over HTTP. As of 2026-07-09 there is a single surface,
`/api/v1/...`, served by gRPC + gRPC-Gateway (field names **camelCase**
throughout; errors are gRPC-standard `{code, codeName, message, details}`). The
`.geojson` map layers stay hand-built (RFC 7946 geometry) but are also camelCase.
(History: `/api/v1` originally hosted a different hand-built REST surface, replaced
by a snake_case `/v1` surface on 2026-07-05, which was in turn folded back onto the
proto-defined `/api/v1` gateway on 2026-07-09 — see those entries.)

## 2026-08-13

### New `power` layer: PG&E outages and Public Safety Power Shutoffs

**Additive — nothing existing changes shape.** A new event layer covering both
PG&E electric outages and PSPS de-energizations, sourced directly from PG&E's
public ArcGIS services.

- `GET /api/v1/events?layer=power` — the event feed. `category` separates
  `unplanned` | `planned` | `psps`.
- `GET /api/v1/places/{place}/map/power.geojson` — a new map layer (an
  **eleventh** layer slug; the others are unchanged). Features carry a new
  `properties.power` block.
- `GET /api/v1/places/{place}/summary` — **`domains[]` now contains a `power`
  entry**, always present. If you index `domains` by position rather than by
  `domain`, re-read it by name.
- `GET /api/v1/sources` — **two new rows, `pge` and `psps`.** If you assert on
  the number of sources, that count changed.
- `Event.detail` gains a `power` variant (`PowerDetail`): `outageId`, `cause`,
  `customersAffected`, `crewStatus`, `estimatedRestoration`, and for PSPS
  `eventId`, `eventName`, `timePeriod`, `stage`, `medicalBaselineAffected`,
  `deEnergizationStart`, `deEnergizationEnd`.

Things worth knowing before you render it:

- **Most outages are tiny.** Statewide, the median PG&E outage affects ONE
  customer. Severity reflects that (`<10` customers → `INFO`, `≥10` → `MINOR`,
  `≥100` → `MODERATE`, `≥1000` → `SEVERE`; a *planned* outage is demoted one
  rank because PG&E notified those customers). An outage PG&E reports with **no
  customer count** floors at `MINOR`, not `INFO` — unknown is not evidence of
  small, and its headline says "customer count not reported". Filter with
  `severity_min=MINOR` rather than expecting the feed to be pre-curated — we
  ingest every row so that a disappearing row reliably means "restored".
- **INFO-severity power events are excluded from the place-summary rollup**
  (`totalActive`, `severityCounts`, `topEvents`, `mode`) — the same carve-out
  mesh presence gets, so a quiet county isn't reported as "21 active" on the
  strength of single-premise service calls. They still appear in full in the
  `power` domain, in `/api/v1/events`, and on the map layer. A `MINOR` outage
  (10+ customers) counts normally.
- **`estimatedRestoration` is an estimate, not an expiry.** PG&E routinely
  overruns it, so it is NOT mapped onto `expires` (which stays null) and must
  not be used to hide an event. Same for PSPS `deEnergizationEnd`.
- **A PSPS is `SCHEDULED` until de-energization begins**, then `ACTIVE`. A
  `Warning` stage is `SEVERE`, a `Watch` is `MODERATE`.
- **`medicalBaselineAffected`** is customers whose medical equipment depends on
  grid power. No other feed on this service reports it.
- **Freshness is enforced, not assumed.** PG&E publishes its own ETL stamp; when
  it stops advancing, the `pge` source reports degraded and the layer's
  `sourceStatus` goes `STALE` rather than serving frozen outages as current.

### Evacuations: the directive text was empty, and the "as of" was missing

**Affects every evacuation event and the `evacuation` map layer.** Cal OES moved
the public directive text out of `PUBLIC_INFO` and into `NOTES`, and stopped
populating `ZONE_NAME`, `EVENT_TYPE` and `STATEWIDE_LAST_UPDATED` — measured
empty on **all 37 active rows statewide** on 2026-08-13. Nothing failed, because
a null column parses fine. The visible result was that **`description` was empty
on every evacuation we served** — on the one layer whose whole job is carrying a
county's instruction verbatim — while text like *"...issuing an immediate
EVACUATION ORDER... Leave Now."* sat unread in `NOTES`.

- `description` now falls back to `NOTES` when `PUBLIC_INFO` is empty. Still
  verbatim, still never paraphrased. The documented field wins when present, so
  this survives Cal OES moving it back.
- `observedAt` now falls back to the row's ArcGIS edit timestamp. It was
  **null on every evacuation** before this. It is the only freshness signal the
  layer populates, and it is what makes an ORPHANED zone visible — one a county
  lifted that the aggregation never retracted. If you show an "as of", it will
  now have a value, and that value can be days old.
- `headline` is unchanged and still falls back to the zone id when the zone has
  no name. `NOTES` is deliberately **not** used as a label: its content varies by
  county — a street in Tuolumne, the full sheriff's instruction in Monterey, the
  event type ("Flooding") in Tulare — so it is reliable as text and unreliable as
  a title.

An orphaned zone stays `ACTIVE`. Cal OES still lists it, and retiring a
life-safety event on our own inference would publish an all-clear no authority
issued. The age is surfaced instead, and logged for an operator to chase.

### Evacuations: non-Genasys counties link to their own viewer

**Affects `properties.source.url` on the `evacuation` map layer and
`provenance.sourceUrl` on evacuation events.** A zone in a county that is not on
Genasys used to get the generic `protect.genasys.com` viewer — which does not
contain that county's zones at all. Tuolumne (`US-CA-Toulumne117`) now links to
Tuolumne County's own evacuation-zone viewer.

Note those county viewers show **currently live zones only**, so a zone we still
report can be absent there — that absence is evidence the order was lifted. The
layer-level `metadata.sourceUrl` still points at Cal OES in every state.

### `weather_alert` and `earthquake` map layers now carry `properties.status`

**Additive.** Both omitted `status` while `/api/v1/events` carried it. That was
left over from byte-compatibility with the pre-2026-07 builder, which no longer
exists — and it had become harmful: the 2026-08-11 change made an advance watch
report `SCHEDULED` until its onset *precisely so consumers could distinguish it
from a warning in force*, and told them to read `status`. A map client reading
`weather_alert.geojson` had no such field, so every alert looked equally live.
`status` is the UPPERCASE lifecycle enum, matching `Event.status` and the other
layers.

### Fire weather: `red-flag` no longer starts hours before the Red Flag does

**Affects `conditions.fireWeather.state` and the `fire_weather` map layer.** NWS
lists a product on `/alerts/active` from the moment it is **issued**, which is
routinely hours before the weather. We took any listed Red Flag Warning as
current, so the service reported `state: "red-flag"` while the governing
product's own start time was still six hours away — asserting conditions NWS
says have not begun. Observed live at 2026-08-13T05:49Z against a warning
effective 12:00Z.

This is the same mistake the 2026-08-11 change fixed for the alert itself (which
now reads `SCHEDULED` until onset) and never propagated to the classification
derived from it.

A Red Flag Warning that has not begun now reports **`elevated`**, not
`red-flag` — and not `normal`, which would hide a real issued product. The
warning still supplies `sourceEvent` and the headline, and `elevated` escalates
a place's `mode` to `WATCH` exactly as `red-flag` does, so **nothing is
under-alerted by this**; only the claim about what is happening *now* changes. A
product that publishes no start time is still treated as in force.

### Map layers: `metadata.attribution` really is populated on every layer now

**Follow-up to the 2026-08-11 entry, which was only true of the event layers.**
`road_segment`, `chain_control`, `fire_weather` and `mesh_link` never pass
through the registry lookup that change added, so all four were still serving
`metadata.attribution: ""` in production. They now credit their upstreams from
the same source registry. Two per-feature blocks that were also blank —
`fire_weather` (`nws`) and `road_segment` (`google`) — now carry attribution too.

### Wildfire `headline`: upstream whitespace no longer doubles the separator

CAL FIRE ships trailing whitespace on some incident names, which reached the
headline verbatim: `Dennis Fire  — 16 ac, 40% contained` rendered with a double
space while its siblings rendered correctly. The name, county, location and URL
are trimmed on ingest.

### `conditions?place={town}` returned no weather for 3 of 7 towns

**Affects `GET /api/v1/conditions?place=`.** A TOWN place is a point, so its
bbox is the point itself — and stored place geometry is trimmed to 5 decimals
(the repo-wide GeoJSON convention) while the configured weather coordinates it
is compared against are not. A town therefore matched its own weather only when
its configured coordinates happened to have no more than 5 decimals:
`columbia` (38.034900 vs stored 38.0349) worked, `murphys` (38.139117 vs stored
38.13912) did not.

Live, `?place=murphys` — a resident asking for their own town's weather —
returned an empty list, as did `arnold` and `bearvalley`. All seven towns now
return their location. A zero-width or zero-height box is widened by ~11 m,
which is a rounding allowance and **not** a "near this town" radius: the nearest
pair of configured locations is kilometres apart, so no town picks up a
neighbour. Polygon places are untouched.

### BREAKING: road-incident event ids change, and Caltrans closures stop merging

**Affects every `road_incident` event id.** `Closure ID` is a route-level
PROJECT id, not a closure id, and using it alone merged unrelated closures into
one event. Measured on a live feed of 593 closure rows:

```
Closure ID alone            271 distinct — 77 of them covered >1 closure
Log Number alone             58 distinct — a small per-project counter
Closure ID + Log Number     419 distinct — still merged unrelated closures
+ location text             425 distinct — every group is 1 or 2 rows
```

`C99CB` alone covered **16 unrelated Route 99 ramp closures**; `C128AA` covered
18 spanning ~130 km. Because the id was also the dedup key, 15 of those 16 were
**silently dropped every poll**, and which one survived depended on feed order —
so one stored event's history walked 30 km across the map (33 distinct centroids
under `chp:C99CB`).

New ids, namespaced by feed:

| feed | before | after |
|---|---|---|
| CHP incident | `chp:260813SA0270` | `chp:260813SA0270` (unchanged) |
| Caltrans closure | `chp:C99CB` | `caltrans:C99CB-14-d65d75` |

The Caltrans form is `{ClosureID}-{LogNumber}-{hash of the location text}`. The
location text is what separates closures sharing the first two (the live case:
`C89HA`/`14`, four rows 167 km apart, split by "Lake Almanor Resort Rd" vs
"Eagle Point Campground"). Two captures 9.7 h apart shared 424 of 425 keys with
zero coordinate drift.

**The `chp:` prefix on a Caltrans record is also gone.** Those ids contradicted
the event's own `provenance.sourceId: "caltrans"` — 21 of 27 active incidents in
production. Prefix and source now agree on every row.

**You will see more road incidents.** In the service area the count went 27 → 46
on the same feed: the extra 19 are closures the colliding id was discarding.

**Migration: none needed, and none is possible.** The old ids are absent from the
first successful poll, and `caltrans` is a `resolve` source, so the disappearance
sweep retires them with a recorded closing revision — their history stays
queryable at `/api/v1/history`, it just ends. A rename was considered and
rejected: the mapping is 1:N (one stored row conflated 16 closures, with their
revisions interleaved), so there is no correct way to split it, and attaching the
mashup to whichever closure "won" would give one real ramp closure a fabricated
multi-week history. **If you cache or link road-incident ids, re-fetch them.**

## 2026-08-11

### Map layers: `metadata.attribution` is now populated on every layer

**Affects `GET /api/v1/places/{place}/map/{layer}.geojson`.** The field was
hardcoded and returned `""` for every layer except `evacuation`, so eight of the
nine served an empty attribution — while every feature inside them carried a
populated `properties.source.attribution`. It now comes from the source
registry, which has always had a line for each feed, and a layer aggregating two
feeds credits both (`road_incident` → `California Highway Patrol ·
quickmap.dot.ca.gov`).

If you render the envelope's attribution — which is the documented place to read
it — you were displaying nothing for eight layers. No shape change; the field
was always present, just empty. A registry row with no attribution still yields
`""` rather than a fabricated line.

### Wildfire `headline`: acreage carries thousands separators

`Gann Fire — 10339 ac, 90% contained` is now `Gann Fire — 10,339 ac, 90%
contained`. The format is otherwise unchanged (`{name} — {acres} ac, {pct}%
contained`), and only the acreage number is affected — a big fire is the one
that most needs reading at a glance and was rendering least readably. **If you
parse acreage out of `headline`, stop** — `wildfire.acres` is a typed float and
always has been.

### Weather alerts: `headline` is now the reason, not the issuance boilerplate

**Affects `layer=weather_alert` events and `conditions.fireWeather.headline`.**
No field is added, removed or retyped. The `headline` string changes shape:

```
before   Fire Weather Watch issued August 11 at 9:57AM PDT until August 13
         at 9:00PM PDT by NWS Sacramento CA                        (100 chars)
after    Fire Weather Watch — thunderstorms and strong outflow winds  (61)
```

The old value was CAP `properties.headline` passed through verbatim, and every
token in it is already a structured field on the same record — the product name
is `category`, the two stamps are `effective` and `expires`, the office is
`provenance.sourceName`. It said nothing new, in four places at once (the event,
`conditions.fireWeather.headline`, and both the `weather` and `fire` domain
lists on a place summary).

The new value is composed deterministically from the product name plus the
reason clause NWS publishes in `parameters.NWSheadline`, which we were not
parsing. It is **not** AI-generated, deliberately: `headline` participates in the
content hash, so a model-written one would differ on every poll and mint a
revision each time. Where a product publishes no `NWSheadline` — most do not —
the headline is the bare product name (`Winter Storm Watch`).

**If you display `headline`, you get a shorter and more useful line and need do
nothing. If you parsed it for the issuance time or the office, stop** — read
`effective` / `expires` / `provenance.sourceName`, which have always carried
those and are typed. `description` is unchanged and still verbatim.

### Weather alerts: `effective` / `expires` are the hazard's window, not the product's — **behaviour change**

CAP publishes four timestamps in two pairs, and we were reading the wrong pair.
`effective`/`expires` describe the **product**: when the office issued it, and
by when the office must re-issue it. `onset`/`ends` describe the **hazard**. They
routinely disagree, because a watch is issued the moment it is written:

```
                    effective  2026-08-11T09:57  ← issued Tuesday morning
                    expires    2026-08-12T16:00  ← office's re-issue deadline
                    onset      2026-08-13T05:00  ← the storms
                    ends       2026-08-13T21:00
```

We shipped `effective`/`expires` verbatim and never parsed `onset`/`ends` at
all. Two visible consequences, both now fixed:

- **`expires` was often before the hazard.** The Fire Weather Watch above
  advertised `expires: 2026-08-12T23:00Z` for storms running through the 13th —
  the record said the alert was over a day and a half before the weather it
  warns about. **If you hide or grey out alerts past `expires`, you were hiding
  live warnings early.**
- **`status` was never `SCHEDULED` for an advance watch.** The status test
  compared `effective` to now, and `effective` is issuance, so a watch for
  Thursday's storms went straight to `ACTIVE` on Tuesday. It is now `SCHEDULED`
  until `onset`, which is what the field was specified to mean.

`effective` and `expires` now carry `onset` and `ends` when the product
publishes them, falling back to the old values when it does not (most products
publish neither, and are unaffected). No field is added, removed or retyped.

**Event ids do not move.** `NWSAlertID`'s synthesized fallback still keys on the
product's own `effective`, deliberately — an id is an identity, not a schedule.

Same correction applies to `conditions.fireWeather.effective`/`expires`, which
took the same pair from the governing product.

### Weather alerts: `summary` is a regional summary, not a transcription

The AI `summary` on weather alerts was averaging three to four times its useful
length — one live Fire Weather Watch ran to 865 characters, of which ~350 was a
transcription of seven NWS fire zones with their elevation bands (mostly outside
this service's area) plus a restatement of the issuance and expiry times. The
prompt asked it to unwrap the alert's `* WHAT/WHERE/WHEN` blocks into prose with
no length ceiling, so it did.

It is now capped at two sentences / 320 characters and told to lead with the
hazard and what it means locally: no zone identifiers or elevation bands, no
issuing office, no timestamps (all of which the record already carries as typed
fields), and no restating the headline it sits under. Where a place name is
attached to the event it is named, rather than the region described generically.

```
after   Thunderstorms with gusty, erratic outflow winds expected in Ebbetts Pass
        Corridor late Wednesday night through Thursday evening; isolated to
        scattered wet and dry storms with lightning possible even away from main
        rain cores. Lightning may start new fires and could combine with strong
        outflow winds to make fires grow rapidly.
```

Unchanged: `summary` is still absent when enhancement is disabled or over
budget, `enhancement` still badges it with the model and the captured I/O, and
directive text is still never paraphrased — `weatherAlert.instruction` and
`description` remain verbatim.

### Event data: three fields change value (no shape change)

No field is added, removed or retyped, and no URL or status code moves. Three
fields now carry **different values** than they did, and each was wrong before.

**`headline` on `evacuation` events now names the zone.** The format is
unchanged — `Evacuation {Level} — {what}` — but `{what}` was the zone name
falling back to the **county**, and Cal OES frequently sends no zone name. A
county-wide activation therefore produced a dozen simultaneous records all
reading `Evacuation Order — CALAVERAS`, distinguishable only by `id`:

```
before   Evacuation Order — CALAVERAS      (x12, identical)
after    Evacuation Order — CAL-E-109-C
         Evacuation Order — CAL-E-139-D    …
```

A zone that *does* have a name now carries both: `Evacuation Warning — Zone A
(CAL-E-046)`. **If you match on this string, stop** — `evacuation.zoneId` is the
stable identifier and always has been. The zone id is also what an evacuee is
told over the radio, which is why it beats the county as the fallback.

**`areaLabel` on `mesh` events is now empty.** It was being set to the node's
own name, which is not a location: `areaLabel` is the human description of
*where* something is (`Hwy 4 at Avery`, `10km NE of Murphys`), and a mesh node
has coordinates and nothing else. The name was never lost — it is
`mesh.name`, and it is the `headline` — so this removes a duplicate that was
also a false claim. The Grid does not reverse geocode; an empty string is the
honest answer and `geometry` carries the position.

**`mesh.telemetry.snr` / `.rssi` stop being clobbered by absent readings.**
Community MQTT bridges are inconsistent about which keys they send. An advert
that omitted `SNR` was decoding to `0` and overwriting the last real
measurement, so a node heard at 4.5 dB began reporting `0` the moment one
bridge left the field out. Absent and zero are now distinguished, and the last
known reading survives an advert that does not carry one. An explicit `RSSI` of
`0` is also treated as unknown — received signal strength is negative dBm, so
`0` is not a value any radio produces.

`hopCount` is deliberately **not** included in that change: `0` is a real
reading there (a zero-hop beacon heard directly), and it comes from the decoded
over-the-air frame rather than the bridge envelope.

**Consumer impact.** If you display `mesh.telemetry`, values will look
*steadier* and fewer will read `0`; a genuine `0` SNR is still reported. If you
display `areaLabel` for mesh nodes, you now get an empty string — render it as
absent, not as a blank line, and use `mesh.name` for the node's identity.

### Site: `/assets/*` must revalidate (no API change)

Static assets under `/assets/` are served `Cache-Control: no-cache` instead of
`max-age=300`, and every site response now carries an `ETag`, so revalidation
costs a conditional request and a `304` rather than the file again. The page
HTML and the JavaScript it loads are one unit — the script binds to the markup
by element id — and a five-minute window in which a browser could hold new HTML
beside old script produced a hard failure on load after each deploy.
`_astro/*` keeps its immutable caching (those filenames are content-hashed) and
`lib/*` still caches for a day.

## 2026-08-10

### `/api/v1/history` is much faster (no API change)

No request or response shape changed — this is purely a latency fix, recorded
here because consumers were feeling it. The cross-event revision archive was
measured at **6–40s** (`page_size=50` took 18.7s, 39.0s and once timed out at
40s); a browser client with any sane request deadline saw it as a hang.

Cause: `event_revisions` had no index on `observed_at`, so the query behind
`/api/v1/history` scanned the whole revision table and sorted it in a temp
B-tree on every call. Store migration **v4** adds
`idx_revisions_observed(observed_at DESC, event_id ASC, revision DESC)`,
matching the query's `ORDER BY` exactly, so the range filter and the sort are
both satisfied by an index walk. The migration applies automatically on boot
(append-only ladder); no operator action.

**If you previously worked around this** by avoiding `/api/v1/history`, capping
`page_size`, or setting a long client timeout, you can undo that. The per-event
timeline (`/api/v1/events/{id}/history`) was never affected — it keys on the
primary key.

Two things worth knowing that did **not** change, because they are sometimes
misreported as bugs:

- The `from` / `to` bounds **do** filter (verified: `from=2027-01-01` returns 0
  revisions; `to=2026-08-01` returns only July). The window is half-open,
  `[from, to)`, over `observedAt`.
- `observedAt` is **upstream-stamped**, and mesh nodes timestamp their own
  adverts from unsynchronized on-board clocks. A `MESH` revision can therefore
  carry a future time and sort above genuinely newer records. That is the
  upstream value reported verbatim. To ask *when this service learned
  something*, read `ingestedAt`, which we stamp and which is monotonic.

### Site: the documentation site has been rebuilt (no API change)

`data.sierragridteam.org` has moved from the dark dev-console layout to a
"broadsheet" design — paper for reading, black for data. The API is untouched;
this affects only the human-facing site. Three changes worth noting for anyone
who links to or embeds it:

- **The Map screen no longer draws a basemap when nothing can honestly be
  drawn.** If every selected layer is `UNAVAILABLE`, errored, or empty, the map
  element is removed from the page and replaced with a banner naming which case
  it is. A rendered basemap with zero features reads as an all-clear, which this
  API's contract forbids. A *partial* failure counts too: one failed layer among
  eight empty ones is an unknown, not a confirmed-clear region.
- **Events is now a list + full record**, replacing the table + raw-JSON
  inspector. The record shown is rendered by the same code as the `/event`
  permalink page, so the two can no longer disagree. `/event?id=…` is unchanged
  and remains the permalink.
- **Docs endpoint examples are runnable.** Each carries a RUN button that
  fetches that exact URL live and prints the real response, so an example that
  has gone stale now fails visibly instead of quietly misleading.

The remaining screens have now been rebuilt on the same system, completing the
redesign. Nothing below changes a request or a response; they are listed because
they change what a linked-to page looks like or how a control behaves:

- **Roads** leads with the two-idiom split it always should have: road
  *incidents* are events, road *conditions* (congestion, chain control) are
  projections that never appear in `/events`. Incidents are now record rows that
  open the full event; the segment and chain-control tables are unchanged in
  content.
- **Map layer controls are chips, not checkboxes**, and the page prints one
  `GET …/map/{layer}.geojson` per selected layer. Layers are still
  multi-select — several can be drawn at once, as before.
- **History is a feed of records** carrying `rev {n}`, replacing the six-column
  table, and each row now shows its ingest age alongside the id. Filters, the
  date range and paging are unchanged.
- **Places** gains client-side `kind` filter chips and a single directory table
  (place · kind · geometry · parent) in place of the per-kind groups. The
  geometry column distinguishes a full GeoJSON body from `bbox only` — worth
  knowing before you rely on a place for point-in-polygon work. The endpoint
  reference that used to sit at the top of this page now lives only in Docs,
  which has the runnable version.
- **Sources** shows each feed's `attribution` string on the board, and states
  the staleness rule (`STALE` once last success is older than
  `staleAfterSeconds`, default three poll intervals). **Read those live values
  rather than hard-coding intervals** — they are per-source tunables.
- **AI-enhanced text is badged neutrally** rather than in the alert accent. It
  was rendering in signal red, which made a machine paraphrase look like a
  warning. The verbatim upstream `description` remains reachable on every
  record, unchanged.
- **Events was rebuilt around its filter and data regions.** Place moved out of
  the filters into a scope header (it is scope, not a facet); the filters are a
  plain row of chips per facet that requery on click (no apply step); the
  request echo is a sticky black band carrying the literal page size and the
  server's full sort rule including the tiebreak. Row titles lead with the
  record's distinguishing field — `ZONE CAL-E043 — Evacuation Warning` — because
  a headline is not distinguishing for evacuation records, and each row now
  shows both its ingest and observed times. **`status` is now two choices** —
  `open only` (the API default, ACTIVE+SCHEDULED) or `include closed` (all four)
  — rather than four independent checkboxes. Any other subset is still valid on
  the endpoint and can be sent by hand; the echoed URL prints whatever is used.
- **The event detail pane is one flat sequence of open sections** — envelope,
  detail, geometry, provenance, enhancement, revision timeline — each under a
  small caps heading and a heavy rule, with a single raw-protojson disclosure at
  the foot instead of one per card.
- **Relative times are spelled** (`2 min ago`, not `2m ago`) everywhere. In the
  uppercase mono meta lines the compact form read as "two months".
- **Absent values in event data no longer render as a dash.** Absence has three
  distinct meanings and now three renderings: `not provided by source`,
  `n/a for {layer}`, and a red `MISSING` with the source URL for a value that
  should exist and does not. A dash reads as "nothing to report", which is
  precisely the inference the fail-loud contract exists to prevent.

- **Maps are light, and inert until you use them.** The basemap moved from CARTO
  dark to OpenFreeMap's Positron, so hazard geometry now paints in the paper
  severity ramp rather than the dark-adapted one — if you were colour-matching
  screenshots of the map, the hexes changed. Maps also no longer capture the
  scroll wheel: gestures are disabled until the map is clicked or focused, and
  released on Escape. A map whose tile host is unreachable still draws its
  geometry over a blank backdrop rather than rendering nothing.
- **The paper family is neutral, not warm** (2026-08-11). Paper moved
  `#f4f1ea → #fbfbfa` and the four surface tokens beneath it followed
  (paper-sunken, both rules, the faint text tone). Ink, the accent and both
  severity ramps are unchanged, so **if you colour-match hazard geometry or
  chips to this site, nothing you key on moved** — only the page under them.
- **Long ids truncate and copy.** Event ids run to seventy characters
  (`meshcore:3811ef…`); they are clipped with an ellipsis and click-to-copy
  hands over the full value. Copying now marks itself with a tick beside the
  value rather than replacing the text with the word "copied" — the id stays
  legible and the line does not reflow.
- **Every screen that filters now filters the same way.** Map, History, Places
  and Roads used a label/grid form with native `<select>`s while Events used
  chips; they are now one component — a facet per query parameter, labelled with
  the parameter's own name, above the rule that divides the query from its
  results. Behaviour is unchanged on each page except that place is chosen from
  a styled menu rather than an OS dropdown, and a `?place=` naming a corridor or
  a town (rather than an AREA) is now preserved by every picker instead of being
  silently reset by two of them.

**Not changed, and worth stating because it is often asked for:** `/api/v1/events`
returns no total record count. `EventList` is `{events, nextPageToken}` — keyset
pagination over an opaque cursor. Clients cannot render "12 of 47" or a page
count without a server change, and the site does not fabricate one. Where a real
count IS available it is used: the Events header reports
`summary.totalActive` for a scoped place, which is live and exact.

## 2026-08-06

### CORS is now open (`Access-Control-Allow-Origin: *`)

Every `/api/v1/...` response (and the `.geojson` map layers) now returns
`Access-Control-Allow-Origin: *`, so **any** web origin can fetch The Grid
directly from the browser. Previously only an explicit allowlist
(`ersn.net`, `sierragridteam.org`, and localhost dev ports) got a CORS header;
every other origin's cross-origin fetch was blocked by the browser.

**Impact for consumers**: purely additive — nothing that worked before stops
working. If you were on the old allowlist, no change is needed. If you were
building a browser client from any other origin (or a local dev server on a
non-standard port) and hitting CORS errors, those are gone.

No field names, types, status codes, or URLs change. This is a response-header
change only.

Notes:
- The API stays public, read-only, keyless, and **credential-less** — `*` CORS
  is served with `Access-Control-Allow-Credentials: false` (the two are required
  to go together). Don't send cookies/credentials on cross-origin requests; they
  won't be honored.
- Only `GET` is granted cross-origin (`Access-Control-Allow-Methods: GET`). The
  `/mcp` JSON-RPC endpoint is POST and remains not callable cross-origin from a
  browser.

## 2026-08-04

### Wildfire detection widened, and fires now attach to a place by perimeter and by proximity

Fire is now the **widest** thing The Grid watches. Previously the `wildfire`
layer was scoped to the same box as everything else — the union of
`hazards.areas[].bounds` — which was in fact *narrower* than the CHP/Caltrans
incident box, so a fire on the edge of the coverage region could be invisible
until it had already crossed in. No field names, types or status codes change;
what changes is **which fires appear, and on which places**.

Three changes, all wildfire-only (`layer=wildfire` events, the `wildfire`
geojson layer, and the `fire` summary domain):

- **Wider ingest scope.** The CAL FIRE incident filter and the FIRIS perimeter
  query now run over the hazard-area union grown by
  `grid.wildfire.marginDegrees` (default 0.5° ≈ 55 km a side). Expect more
  `wildfire` events in `GET /api/v1/events?layer=wildfire`, including fires
  outside the coverage footprint.
- **In scope by perimeter, not just by point.** CAL FIRE publishes one origin
  coordinate per incident; a large fire's perimeter can reach tens of km from
  it. An incident whose *perimeter* overlaps the scope is now kept along with
  its acreage, containment and incident URL. Previously it was dropped and only
  the orphaned perimeter surfaced, as a bare `firis:` standalone with unknown
  containment.
- **Approaching fires attach to a place.** A wildfire within
  `grid.wildfire.placeBufferMeters` (default 20 km) of an **area** or **town**
  place now attaches to it, so it appears in that place's
  `map/wildfire.geojson`, in `?place=`-filtered event queries, and in the place
  summary — before the perimeter crosses the boundary. Counties and corridors
  are unchanged (strict geometry).

**Consumer note:** a proximity-attached fire is a normal active event for that
place. It counts toward `summary.totalActive` and `severityCounts`, is eligible
for `topEvents`, and can lift `summary.mode` to WATCH/ACTIVE. A place can
therefore report an active fire whose perimeter is not inside its boundary —
that is intended, but if you render "fires in <place>" you may want to say
"fires in or near <place>".

**Event ids can change for fires that were previously perimeter-only.** A fire
whose CAL FIRE origin point sat outside the old box surfaced as a bare
standalone `firis:<name>` (perimeter acreage, no containment, no incident link).
Now that the incident itself is in scope, the same fire is the richer
`calfire:<uuid>` event with the perimeter adopted. Verified live on the **Gann
Fire** (3,760 ac, Valley Springs): it was `firis:gann` attached only to Calaveras
County and absent from the Ebbetts Pass map entirely; it is now
`calfire:e51208a8-…` with the CAL FIRE incident URL and location, attached to
`area:ebbetts-pass`.

The old `firis:<name>` event is **RESOLVED on the first tick** after its
perimeter is adopted, so the fire is never drawn twice. Previously the `firis`
source's `disappearance: expire` policy would have held the orphan ACTIVE for its
full 24h grace. This applies in steady state too, not just at deploy: any fire
CAL FIRE adds to its curated list after we already had its perimeter now
transitions cleanly. A consumer holding the old id sees `status: RESOLVED` (a
recorded revision — the history is kept, nothing is deleted) and should follow
the `calfire:` event.

## 2026-07-27

### BREAKING — wildfire perimeter source WFIGS → CAL FIRE / FIRIS combo feed

The `wildfire` layer's perimeter source moved from the NIFC WFIGS interagency
upload to the **CAL FIRE / FIRIS combo feed** (`CA_Perimeters_NIFC_FIRIS_public_
view` — the layer CAL FIRE's own public map uses). It combines CAL FIRE Intel +
FIRIS IR-flight + WFIGS perimeters and updates every ~5 min, so mapped perimeters
now appear **hours sooner** (the Dove Fire had a perimeter here while WFIGS still
returned none). Fire geometry/adoption semantics are unchanged; the feed carries
many rows per fire, deduped to one (latest IR flight) before the name-join. See
`docs/firis-perimeter-source-design.md`.

**Breaking (source id + event-id namespace renamed).** Migration: repoint any code
keyed on the source id `wfigs`, or on stored `wfigs:` event ids, to `firis`. On
deploy, the old standalone `wfigs:` events are transitioned to `EXPIRED` once (a
recorded revision) and superseded by fresh `firis:` events — a consumer that
cached a `wfigs:` id will get a 404 for it after the swap.

Consumer-visible changes (no field *renames*, but value changes):

- **`GET /api/v1/sources`**: the `wfigs` source row is replaced by **`firis`**
  (name `CAL FIRE / FIRIS`, attribution `CAL FIRE / FIRIS / NIFC`).
- **Event provenance / geojson `source`**: adopted CAL FIRE incidents now carry
  attribution `CAL FIRE / FIRIS` (was `CAL FIRE / WFIGS`); standalone perimeters
  emit `{id: firis, name: "CAL FIRE / FIRIS", attribution: "CAL FIRE / FIRIS /
  NIFC"}` (was the `wfigs`/`NIFC WFIGS` block).
- **Standalone perimeter event ids** use the **`firis:`** namespace (was
  `wfigs:`) — e.g. `firis:{normname}`. Adopted fires keep their `calfire:` ids.
- Standalone perimeters carry **no `containment`/`cause`** (the combo feed has
  neither); `wildfire.containment` is `0` and `cause` is omitted on them. Adopted
  CAL FIRE incidents keep their containment/cause. The wildfire layer's
  `metadata.sourceStatus` now aggregates `calfire` + `firis`.
- **Severity of standalone perimeters may read higher.** With containment unknown
  (treated as 0%), `severity`/`severityRank` sit at the uncontained floor
  (`SEVERE`+), where a WFIGS standalone that reported partial containment could
  previously read lower — a deliberately conservative default for an
  un-cross-referenced active perimeter. Adopted incidents are unaffected (they use
  CAL FIRE's containment). Consumers that style/sort by `severityRank` should
  expect this.
- **Standalone `headline` format** drops the containment clause: `"{name} — {N}
  ac"` (was `"{name} — {N} ac, {P}% contained"`).

### Added — per-location fire-weather forecast (`conditions` + `fire_weather` layer)

Adds a short-range NWS fire-weather forecast — keyless, additive, informational
(never an un-issued Red Flag). See `docs/fire-weather-forecast-design.md`.

- **`GET /api/v1/conditions` gains `forecast[]`** — a per-location NWS gridpoint
  forecast (48h hourly), joined to `weather[]` by `locationId`. Each
  `WeatherForecast`: `source`, `issuedAt`, `horizonHours`, `periods[]` (`time`,
  `temperatureCelsius`, `humidityPercent`, `windSpeedKmh`, `windDirectionDegrees`,
  `windGustKmh`), plus an at-a-glance summary (`peakWindGustKmh`/`peakWindGustAt`,
  `minHumidityPercent`). Honors `?place=` (same location set as `weather[]`).
- **The `fire_weather` map layer gains per-location forecast Points.** Alongside
  the region banner (issued state), each in-area configured location now emits a
  `Point` feature with `properties.fireWeather.forecast`
  (`peakWindGustKmh`/`peakWindGustAt`/`minHumidityPercent`/`source`/`issuedAt`/
  `horizonHours`). These are **`severity: INFO`** — a windy/dry *forecast* never
  colors the layer like an issued Red Flag; only the banner carries the issued
  state. Fail-soft: a forecast outage omits the block/points, never blocks
  conditions.
  - **Consumer note (behavioral):** `fire_weather` is now a **multi-feature**
    collection. Discriminate features by `kind` — `Fire weather` (the null-geometry
    banner) vs `Fire-weather forecast` (the per-location `Point`). `properties.
    fireWeather.state`/`category` are **banner-only** (now `omitempty`); code that
    read `features[0]` as the issued state, or assumed `state` on every feature,
    must select the banner by `kind`.

## 2026-07-25

### BREAKING — mesh-node presence layer renamed `NETWORK` → `MESH`

The mesh-node presence layer was enum `NETWORK` while the rest of the surface
called it `mesh_node` (map layer, `/api/v1/mesh` endpoints). That split made the
layer undiscoverable — `?layer=mesh_node` returned `400 unknown layer` while only
`?layer=network` worked, so clients (and MCP agents) concluded no per-node presence
data existed. The layer is now uniformly **mesh**.

**Migration for consumers (ersn.net, sierragridteam.org):**

- **`event.layer` / `topEvents[].layer` now serialize as `"MESH"`** (was
  `"NETWORK"`). Update any equality check on the old string.
- **The event detail block renamed `detail.network` → `detail.mesh`** (same
  fields: `publicKey`, `nodeType`, `name`, `telemetry{…}`). The `.geojson` feature
  property renamed `properties.network` → `properties.mesh` to match.
- **Query values:** `?layer=mesh` is now canonical; `?layer=mesh_node` also works;
  **`?layer=network` is kept as a legacy alias** so existing URLs don't break.
  Applies to `/api/v1/events` and `/api/v1/history`.
- **Unchanged:** the enum *number* (13) — so persisted data needs no migration —
  the map-layer slug/URL (`mesh_node.geojson`), and `properties.layer` on that
  layer (`"MESH_NODE"`, the uppercased slug).
- Per-node fields are otherwise unchanged: `headline`/`areaLabel` carry the node
  name, `observedAt` is when the Grid last heard the node, and
  `detail.mesh.telemetry.lastAdvertAt` is the node's self-reported advert time
  (diagnostic only — node clocks skew).
- The MCP server (`internal/mcp`) advertises the `mesh` layer and how to find a
  node by name in its `grid_events` tool schema and the `grid://reference`
  resource.

## 2026-07-24

### Added — place-scoped mesh topology layer `mesh_link.geojson`

Makes the relay topology a drop-in map layer, consistent with every other
`.geojson` layer (the global `GET /api/v1/mesh/links` stays for whole-mesh views).

- **`GET /api/v1/places/{place}/map/mesh_link.geojson?window=<Go duration>`** —
  the topology scoped to a place, as one self-contained subgraph
  `FeatureCollection`: `Point` features for the nodes located **inside** the place
  **plus the 1-hop neighbours they link to** (so a link out to the wider mesh
  isn't amputated at the boundary), and `LineString` features for the edges among
  them. Node `properties.network` gains **`inRegion`** (`true` = inside the place,
  `false` = a pulled-in neighbour; omitted on the plain `mesh_node` layer). Edge
  `properties.meshLink` carries `a`, `b`, `observations`, `daysActive`,
  `firstSeen`, `lastSeen`, `bestSnr`. Same camelCase envelope + `metadata`
  (`sourceStatus`, etc.) as the other layers; `window` defaults to `72h`.

## 2026-07-23

### Added — mesh relay topology endpoint `GET /api/v1/mesh/links`

The MeshCore relay topology is now served as a **derived, durable** graph instead
of one frozen path sample per node. A path is a property of a *reception*, not of
a node, so per-advert relay paths are rolled up over time and served as a weighted
edge list.

- **New `GET /api/v1/mesh/links?window=<Go duration>`** — the whole mesh's relay
  links (global, not place-scoped). Response `{window, generatedAt, links[]}`;
  each link is `{a, b, observations, daysActive, firstSeen, lastSeen, bestSnr}`
  (`a`/`b` are node public keys, canonical `a < b`). `daysActive` (distinct days
  the link was observed) and `lastSeen` let a client weight and recency-fade an
  intermittent mesh honestly. `window` defaults to `72h`, clamps to ~400d. Join
  `a`/`b` to node coordinates from `GET /api/v1/events?layer=network` (or the
  `mesh_node.geojson` layer) to draw the links.

### Removed — `telemetry.path` / `telemetry.pathNodes` (`NETWORK` layer) — **breaking**

- **`telemetry.path` and `telemetry.pathNodes` are gone** from network events
  (`GET /api/v1/events?layer=network`) and from the `mesh_node.geojson` `network`
  block. They were briefly shipped earlier on 2026-07-23; a single frozen path
  per node was the wrong model. **Migration:** use `GET /api/v1/mesh/links` for
  relay topology. `telemetry.snr`/`rssi`/`hopCount`/`gateways`/`lastAdvertAt`
  remain as last-heard values.

### Changed — trustworthy observed time (`NETWORK` layer)

- **Observed time is our receive time, not the node's.** Mesh node clocks are
  frequently badly skewed (we saw adverts stamped months in the future). A
  network event's `observedAt` — which `/events` orders and `since`-filters on —
  is now **when we received the advert**, never the node-reported timestamp. The
  node's own stamp survives only as `telemetry.lastAdvertAt` (diagnostic). If you
  sorted or filtered network events by `observedAt`, results are now correct.

## 2026-07-16

### Added — MeshCore mesh-node presence (`NETWORK` layer, `meshcore` source)

New data source: MeshCore LoRa mesh-node presence, ingested from community MQTT
bridges. One event per node (keyed by its Ed25519 public key). All additive —
no existing response shape changes.

- **`GET /api/v1/events?layer=network`** — mesh nodes as events. Each carries a
  `network` detail block: `publicKey`, `nodeType`
  (`companion|repeater|room_server|sensor`), `name`, and a nested `telemetry`
  (`snr`, `rssi`, `hopCount`, `path`, `gateways`, `lastAdvertAt`). `severity` is
  always `INFO`; `status` is `ACTIVE` while heard, `EXPIRED` after ~5 days of
  silence (a mesh has no departure signal). Scope: any node advertising a
  location inside a configured area; locationless nodes are dropped.
- **`GET /api/v1/places/{place}/map/mesh_node.geojson`** — a new map layer.
  Point features `[lng, lat]`, the shared camelCase `properties` envelope plus a
  `network` block. Node locations are quantized to ~4 decimals (~11 m).
- **`GET /api/v1/sources`** — a new `meshcore` source row (health = broker
  connectivity: `OK` while ≥1 broker connected).
- **`GET /api/v1/places/{place}/summary`** — a new `comms` domain, present
  **only when the source is enabled**. Mesh presence is ambient `INFO` state, so
  it does **not** count toward top-level `summary.totalActive`, `severityCounts`,
  `topEvents`, or `mode` (same exclusion as baseline conditions, 2026-07-09).

The `telemetry` sub-block is deliberately excluded from the event content hash,
so the advert firehose refreshes liveness without minting revisions — only a
node's identity, role, name, location, or status change writes history.

Disabled by default (no brokers configured). NOTE for the public docs site
(`../web`): add the `network`/`mesh_node` layer + `network` detail block to the
`/api/v1` reference — that source lives outside this repo.

## 2026-07-09

### Fixed — `summary` domains no longer count baseline conditions as active

`GET /api/v1/places/{place}/summary` domains previously set `activeCount` (and
`headlines`) to the raw count of every merged item, including always-present
baseline condition features. On a genuinely quiet area this produced a
self-contradicting rollup — e.g. `mode: QUIET`, `summary.totalActive: 0`,
`topEvents: []`, yet `domains[roads].activeCount: 4` (four OPEN road segments)
and `domains[fire].activeCount: 1` (a "normal" fire-weather banner), all at
`INFO` severity. Now `activeCount`/`headlines` count **active** items only:
every event, but a condition feature (`road_segment`, `chain_control`,
`fire_weather`) only when it is **above `INFO`**. Baseline monitoring (an open
road, normal fire weather) is excluded, so a quiet domain reports
`activeCount: 0` with no headlines. `status`, `highestSeverity`, and the map
layers are unchanged — the full feature set still drives the `.geojson` layers.
A consumer that rendered `activeCount` will now show fewer (accurate) items.

### Changed — enum values are UPPERCASE on the `.geojson` and `summary` surfaces (BREAKING for map clients)

The `.geojson` map layers and the place `summary` previously lowercased a few enum
values that the proto RPCs already emitted as UPPERCASE. They are now consistent:
**every enum value in a response body is the UPPERCASE proto enum name, on every
endpoint.** The lowercase slug form now appears **only** in URLs and the geojson
`metadata` block (resource addressing).

What changes for a consumer:

- **`.geojson` `properties.layer`:** `"wildfire"` → `"WILDFIRE"` (now matches
  `event.layer`). Applies to all eight map layers.
- **`.geojson` `properties.status`:** the event-lifecycle value is now UPPERCASE
  (`"active"`→`"ACTIVE"`, `"scheduled"`→`"SCHEDULED"`) on the `wildfire` and
  `road_incident` layers; the `road_segment` road status too
  (`"open"`/`"closed"`/`"restricted"` → `"OPEN"`/`"CLOSED"`/`"RESTRICTED"`).
  (Evacuation `status` was already the UPPERCASE level `ORDER`/`WARNING`/….)
- **`summary` `topEvents[].layer`:** `"wildfire"` → `"WILDFIRE"`.
- **Unchanged — already consistent across both surfaces:** `severity`/`severityRank`;
  `fireWeather.state` and the `fire_weather` `category` (lowercase-hyphenated
  `normal`/`elevated`/`red-flag`); the per-kind `category` field (free-form source
  slug); the `{layer}` URL path segment and `?layer=` query value (still accepted
  case-insensitively); and `metadata.layer`/`metadata.area` (lowercase slugs).

**Migration:** a map client that string-compared `properties.layer` or
`properties.status` against a lowercase literal must compare case-insensitively
(or against the UPPERCASE name). One rule now holds everywhere: **enum values are
UPPERCASE; slugs — URLs and the geojson `metadata` block — are lowercase.**

### Added — the OpenAPI spec is now published

`GET /api/openapi.json` serves the generated OpenAPI (Swagger 2.0) description of
the `/api/v1` surface (protoc-gen-openapiv2, embedded). The `/docs` page links it.
Previously the spec was only a repo artifact.

## 2026-07-09

### Fixed — road incidents now attach to road corridors

`GET /api/v1/events?place={corridor}&layer=road_incident` (the "road alerts on a
corridor" query) returned **nothing**: corridors are `LineString` places (the
straight origin→destination chord), and event→place attachment used a
point-in-*polygon* test, which never matches a zero-width line — so no incident
ever attached to a corridor. Now a point event attaches to a corridor when it is
within ~1.5 km of the corridor line (`corridorBufferMeters`). `GET
/api/v1/places:resolve?lat=&lng=` gains the same behavior: a point on a monitored
road now resolves to its `corridor:*` place. (The chord is a straight
approximation of the real road, so the buffer is deliberately generous; incident
coordinates are approximate too.)

Also clarified: the `?place=` filter (and `/api/v1/places/{place}`) accept **either
the slug** (`hwy4-murphys-arnold`) **or the namespaced id**
(`corridor:hwy4-murphys-arnold`) — the bare slug is the intended form.

### Changed — the data API is now proto-defined gRPC + gRPC-Gateway at `/api/v1` (BREAKING)

The hand-built snake_case `/v1` surface (added 2026-07-05) has been **removed** and
its endpoints re-defined as a Protocol Buffers `GridService` served over
gRPC-Gateway at `/api/v1`. This is the "real prefab project" shape: proto
messages + service definitions, auto-generated OpenAPI, gRPC reflection. Move any
consumer from `/v1/*` to `/api/v1/*` and update field casing.

**What changes for a consumer:**

- **Path prefix:** `/v1/...` → `/api/v1/...`. Query parameters are unchanged (the
  gateway accepts both snake_case and camelCase param names).
- **Field casing:** proto RPC responses are now **camelCase** (was snake_case):
  `next_page_token`→`nextPageToken`, `parent_id`→`parentId`, `observed_at`→`observedAt`,
  `area_label`→`areaLabel`, `matched_address`→`matchedAddress`, `homepage_url`→`homepageUrl`,
  `poll_interval_seconds`→`pollIntervalSeconds`, `last_success_at`→`lastSuccessAt`, etc.
- **Errors (proto RPCs):** now gRPC-standard `{code, codeName, message, details}`
  (was `google.rpc.Status`-style `{code, message, details}`) with the mapped HTTP
  status. The one hand-built endpoint (`.geojson`) still emits the
  `google.rpc.Status`-style body (`{code, message, details}`, no `codeName`).
- **camelCase everywhere (incl. `summary` and `.geojson`).** The place `summary`
  is now the `GetPlaceSummary` proto RPC, so it is camelCase like the rest
  (`place_id`→`placeId`, `active_evacuations`→`activeEvacuations`,
  `severity_counts`→`severityCounts`, `top_events`→`topEvents`, …); the
  `active_evacuations` explicit-null-vs-0-vs-N invariant is preserved (an
  `Int32Value` renders as JSON `null` when unknown). The `.geojson` map layers
  stay hand-built but their `properties`/`metadata` are now **camelCase** too
  (`area_label`→`areaLabel`, `severity_rank`→`severityRank`,
  `source_status`→`sourceStatus`, `updated_at`→`updatedAt`,
  `last_source_update`→`lastSourceUpdate`, `source_url`→`sourceUrl`,
  `generated_at`→`generatedAt`, `has_perimeter`→`hasPerimeter`, `zone_id`→`zoneId`,
  `depth_km`→`depthKm`, `log_number`→`logNumber`, …). Map clients reading the old
  snake_case property keys must update.
- **Also changed by the gateway move:** `HEAD` on `/api/v1/*` now returns `501`
  (the old `/v1` router answered `HEAD`); `page_size` out of range (`0`, `>200`,
  non-numeric) is now silently clamped to `1..200` rather than rejected with
  `400`; the `Accept: application/proto` binary rendering (never widely used) is
  gone — responses are JSON only.
- **Conditional GET (`ETag`/`If-None-Match` → `304`):** most read endpoints now
  carry a weak `ETag`; a matching `If-None-Match` returns `304` and skips the
  work. Coverage: `events/{id}` + `.../history` (keyed on the event revision);
  `events` + `history` lists (a global data-version that bumps on any event
  change, plus the filter set); `places` + `places/{place}` (the directory is
  static within a deploy); and the hand-built `.geojson` (body-hash). Not yet
  instrumented: `conditions`, `sources`, `places:resolve`, `scanners`.
- **`summary` conditional-GET (`ETag`) regressed:** moving to the
  `GetPlaceSummary` proto RPC dropped its body-hash `ETag` (a summary `ETag`
  would have to key off event **and** live-condition freshness, so it was
  deferred). It still carries `Cache-Control: public, max-age=30` — a blanket
  30-second freshness lifetime that in fact applies to **every** GridService RPC,
  including the other not-yet-`ETag`ged reads (`conditions`, `sources`,
  `places:resolve`, `scanners`): proxies/CDN may serve them up to 30&nbsp;s stale,
  and past that window a client gets a full re-download rather than a cheap `304`
  until an `ETag` is added.
- **`sources[].lastSuccessAt` (summary) shape change:** for a never-succeeded
  source it is now an explicit `null` (was an omitted key) — a consequence of the
  proto move (protojson `EmitUnpopulated`).
  (Wired via prefab 0.6.0's `etag` plugin.)

**Endpoint map (`/v1` → `/api/v1`):**

| Old `/v1` | New `/api/v1` | Notes |
|---|---|---|
| `GET /v1/events` | `GET /api/v1/events` | `ListEvents`; `{events, nextPageToken}`. |
| `GET /v1/events/{id}` | `GET /api/v1/events/{id}` | `GetEvent`. |
| `GET /v1/events/{id}/history` | `GET /api/v1/events/{id}/history` | `GetEventHistory`; `{revisions, nextPageToken}`. |
| `GET /v1/history` | `GET /api/v1/history` | `ListHistory`. |
| `GET /v1/places` | `GET /api/v1/places` | `ListPlaces`. |
| `GET /v1/places/{place}` | `GET /api/v1/places/{place}` | `GetPlace`. |
| `GET /v1/places/resolve` | `GET /api/v1/places:resolve` | **Path changed** to an AIP colon custom-verb (avoids the `{place}` route collision). `query.matched_address`→`query.matchedAddress`. |
| `GET /v1/sources` | `GET /api/v1/sources` | `ListSources`; a source's health field is `status`. |
| `GET /v1/scanners` | `GET /api/v1/scanners` | `ListScanners`. |
| `GET /v1/roads`, `/v1/roads/{id}` | *(removed)* | Roads are events: `GET /api/v1/events?place={corridor}&layer=road_incident`. |
| `GET /v1/weather` | `GET /api/v1/conditions` | `GetConditions` — current weather + fire-weather only. **Per-location alerts dropped** (alerts are events: `GET /api/v1/events?layer=weather_alert`). |
| `GET /v1/places/{place}/summary` | `GET /api/v1/places/{place}/summary` | `GetPlaceSummary` RPC, camelCase; evac fail-loud (`activeEvacuations` null vs 0 vs N) intact. |
| `GET /v1/places/{place}/map/{layer}.geojson` | `GET /api/v1/places/{place}/map/{layer}.geojson` | Same envelope, hand-built, now camelCase `properties`/`metadata`. |

The MCP endpoint (`/mcp`) is unchanged for its clients — it now calls `/api/v1`
in-process instead of `/v1`. Conditional-GET / ETag support is not yet wired
(deferred behind a future prefab `WithETag`).

## 2026-07-08

### Removed — legacy `/api/v1` surface (BREAKING)

The original `/api/v1` REST surface and its OpenAPI specs have been **removed**;
those paths now return `404`. Everything it served is available on `/v1` (same
binary, same store, same data). Port old URLs with this map:

> **Superseded (2026-07-09):** the `/v1/...` targets in the "Use instead" column
> below were themselves folded back onto `/api/v1/...` on 2026-07-09 — replace
> `/v1/` with `/api/v1/` and see that entry for the current field casing and
> shape. `/v1` now `404`s.

| Removed | Use instead | Notes |
|---|---|---|
| `GET /api/v1/situation/{area}` | `GET /v1/places/{area}/summary` | `layers[]` → `domains[]`; adds `mode`; scanners moved to a sidecar. Evac fail-loud semantics identical. |
| `GET /api/v1/hazards/{area}/{layer}.geojson` | `GET /v1/places/{area}/map/{layer}.geojson` | Envelope byte-unchanged — URL move only. |
| `GET /api/v1/incidents/{area}` | `GET /v1/events?place={area}&layer=road_incident` | Typed `Incident` → Event envelope + `road_incident` detail. |
| `GET /api/v1/weather/alerts` | `GET /v1/events?layer=weather_alert` | Same envelope move. |
| `GET /api/v1/roads*` | `GET /v1/roads*` | Conditions shape kept; adds `?place=` filter. |
| `GET /api/v1/weather*` | `GET /v1/weather*` | Same shape, minus alerts (alerts are events). |
| `GET /api/v1/scanners/{area}` | `GET /v1/scanners?place={area}` | Path → query filter. |
| `GET /api/v1/metrics` (`GetProcessingMetrics`) | *(removed)* | Internal/admin ops surface. |
| `GET /api/docs/*.swagger.json` | *(removed)* | The gRPC-gateway/OpenAPI surface is gone; `/v1` is documented at `/docs`. |

The underlying Roads/Weather/hazards services are unchanged — they're still
consumed in-process by `/v1` and the ingest pollers; only the HTTP/gRPC-gateway
exposure was removed.

### Changed — `ebbetts-pass` coverage is now a polygon, not a square

The `ebbetts-pass` area footprint changed from a coarse square bounding box to a
hand-drawn coverage polygon (`data/places/areas.geojson`): a wedge along the
Hwy 4 corridor (Angels Camp → Bear Valley), bulging northwest to the Tiger Creek
forest above Arnold and extending southeast up Hwy 108 into the Stanislaus
National Forest (Pinecrest / Dodge Ridge), with the corners the square
over-reached cut off — San Andreas & Jackson (NW) and Farmington (SW). Because
area membership is point-in-polygon, events in those trimmed corners no longer
attach to `ebbetts-pass`, so `/v1/places/ebbetts-pass/*`,
`/v1/events?place=ebbetts-pass`, and `/api/v1/hazards/ebbetts-pass/*` return a
tighter, more relevant set; `/v1/places/ebbetts-pass` geometry is now the polygon
rather than a rectangle. The coarse fetch `bounds` were retightened to the
polygon's bounding box (they now reach slightly further east to cover the Hwy 108
/ Stanislaus extension, and no longer reach the far NW/SW). Memberships
re-converge within one poll cycle after deploy. The `/map` page now draws the
area outline so the footprint is visible.

### Changed — road-incident `original_text` no longer carries the "Last updated" stamp

The verbatim Caltrans/CHP incident text (`original_text` on `/api/v1/incidents`
and the road-incident event `description` on `/v1`) previously included the
feed's `Last updated: <date> <time>` line. Caltrans re-stamps that line every
poll, so it churned the stored event and minted a spurious grid revision each
poll (with `observed_at` advancing alongside it, since `observed_at` tracks the
content-observed time). The stamp is already captured structurally (it drives
the incident's last-updated / `observed_at`), so it's now stripped from the
verbatim text. Effect for consumers: `original_text` is slightly shorter (no
trailing "Last updated…"), and road-incident revision history stops showing
no-op revisions — a new revision now reflects a genuine content change.

## 2026-07-07

### Added — MCP endpoint for LLM agents at `/mcp`

A read-only Model Context Protocol server (Streamable HTTP, JSON-RPC 2.0) exposes
the `/v1` data to LLM agents (`docs/mcp-design.md`). Eight tools —
`grid_situation`, `grid_events`, `grid_event`, `grid_conditions`, `grid_resolve`,
`grid_places`, `grid_sources`, `grid_history` — plus a reference resource and a
`hazard_briefing` prompt. It's a thin in-process adapter over `/v1`: geometry is
stripped for token efficiency (a compact `location` centroid/bbox replaces it),
and the fail-loud honesty contract is preserved (per-source status, evacuation
`null`-vs-`0`, a reference-only disclaimer on every result). Tools accept a place
slug, an address, or `lat,lng`. Read-only and unauthenticated, like the rest of
the API. `GET /mcp` returns 405 (no server→client stream); clients POST JSON-RPC.

### Changed — wildfire severity now factors in fire size, not just containment

Wildfire `severity` was derived from containment percentage alone, which capped
every fire at MODERATE once it reached ≥50% containment and never returned
EXTREME — a 5,000-acre fire at 55% contained read MODERATE. Severity now also
weighs fire **size** (NWCG fire size classes) and is biased to over-estimate
active threat: it's the higher of the containment heuristic and a size
escalation, so a fire is never rated *below* its old value. Large, still-active
fires escalate — ≥1,000 ac and <50% contained → EXTREME, ≥100 ac → SEVERE;
small/contained fires are unchanged (e.g. a 9.5-ac fire at 60% stays MODERATE).

Affects `severity`/`severity_rank` on wildfire events (`/v1/events?layer=wildfire`,
the `.geojson` wildfire layer) and, transitively, place `summary` rollups and the
area `mode` (a large partly-contained fire can now push a place to ACTIVE). No
field or shape changes; only the computed value.

### Fixed — weather alerts were scoped to the wrong NWS zones (out-of-area alerts)

The configured NWS forecast zones were incorrect, so `weather_alert` events (and
the `fire_weather` classification) covered the wrong geography. `CAZ065` — used
for Calaveras mountain towns — is actually **"San Gorgonio Pass Near Banning"**
(a Southern California zone, NWS San Diego), which leaked an Extreme Heat Warning
into the central-Sierra service area; `CAZ258/259` (labeled Tuolumne) don't
exist. Corrected to the NWS Sacramento (STO) elevation-banded zones that actually
cover Calaveras & Tuolumne, each verified against `api.weather.gov/points`:
`CAZ137` (1000–3000 ft), `CAZ138` (3000–5000 ft), `CAZ139` (above 5000 ft).

Effect on consumers: out-of-area (SoCal) weather alerts stop appearing on
`/v1/events?layer=weather_alert`, `/api/v1/weather/alerts`, the map
`weather_alert` layer, and per-location `alerts`; real Motherlode/Sierra alerts
are now scoped correctly. Config-only change (`prefab.yaml`); no field or shape
changes.

## 2026-07-06

### Changed — evacuation `source_url` now deep-links into the specific Genasys zone

Evacuation events previously carried the generic Genasys viewer homepage
(`https://protect.genasys.com/`) as their `source_url`. When the Cal OES
`ZONE_ID` is a Genasys/Zonehaven-scheme id (`US-CA-X{county}-{agency}-{zone}`,
e.g. `US-CA-XCA-CCU-153`), the per-event `source_url` now deep-links straight to
that zone: `https://protect.genasys.com/zones/{ZONE_ID}`. This affects both
`provenance.source_url` on `/v1/events` (`layer=evacuation`) and the per-feature
`properties.source.url` in `/v1/places/{place}/map/evacuation.geojson` (and the
legacy `/api/v1/hazards/{area}/evacuation.geojson`).

- Counties **not** hosted on Genasys (e.g. Tuolumne, whose ids look like
  `US-CA-Toulumne101`) keep the generic viewer URL — we don't map their bespoke
  per-county viewers.
- The **layer-level `metadata.source_url` is unchanged** (still the generic
  viewer) — it must stay valid in the fail-loud UNAVAILABLE/empty states.

No field names or shapes change; only the `source_url` value is now more
specific. Consuming sites can link users directly to the affected zone.

### Changed — rebrand to **The Grid** + primary domain moves to `data.sierragridteam.org`

The service is now **The Grid**, the S.I.E.R.R.A data service. Its primary home is
**`https://data.sierragridteam.org`**; `info.ersn.net` remains a supported CNAME
alias through the transition, and ersn.net is a consuming site ("powered by
S.I.E.R.R.A"). **No endpoint paths, field names, or response shapes change** —
consumers on either hostname keep working. Point new integrations at
`data.sierragridteam.org`.

Cosmetic metadata updated to match: the OpenAPI docs served at
`/api/docs/{roads,weather}.swagger.json` now carry the title "The Grid — Roads/Weather
API" and contact `data.sierragridteam.org`, and the NWS `User-Agent` identifies
`data.sierragridteam.org`. The Go module path and GitHub repo were **renamed**
`github.com/dpup/info.ersn.net/server` → **`github.com/dpup/sierra-data`**
(internal identity only — no effect on the HTTP API or its consumers).

### Added — road geometry: `Road.polyline`, and `road_segment` follows the highway

`GET /api/v1/roads` (and `/v1/roads`) now include a `polyline` on each road: an
ordered array of `{latitude, longitude}` tracing the actual route, decoded from
the Google Routes polyline we already request (Compute Routes **Pro** SKU — no
billing change). It falls back to a straight `[origin, destination]` pair when
the routing call is unavailable (e.g. no API key). Additive, non-breaking.

Consequently the **`road_segment` map layer** — `/api/v1/hazards/{area}/road_segment.geojson`
and `/v1/places/{place}/map/road_segment.geojson` — now draws each segment along
that polyline instead of a straight origin→destination line, so corridors follow
the highway. Same `LineString` geometry type, just more vertices; a straight
2-point line is still emitted as the fallback. (The corridor *place* geometry in
`/v1/places` is seeded statically and remains the 2-point line for now.)


### Changed — coverage area renamed `calaveras` → `ebbetts-pass` (**breaking** for `/api/v1` hazard URLs)

The one configured coverage area was mis-named "Calaveras County" (slug `calaveras`)
but actually spans **Calaveras and Tuolumne** along the Hwy 4 + Hwy 49 corridor, and
its slug shadowed the real `county:calaveras-county` place. It is now
**"Ebbetts Pass Corridor"**, slug/id `ebbetts-pass` (`area:ebbetts-pass`). Naming
convention going forward: coverage areas are named for their corridor/region
identity, never for an administrative unit they could be confused with.

The area slug is the `{area}` path segment on the legacy hazard endpoints and the
`{place}` segment on the new ones, so the URLs move:
- `/api/v1/hazards/calaveras/{layer}.geojson` → `/api/v1/hazards/ebbetts-pass/{layer}.geojson`
- `/api/v1/situation/calaveras` → `/api/v1/situation/ebbetts-pass`
- `/api/v1/scanners/calaveras` → `/api/v1/scanners/ebbetts-pass`
- `/v1/places/calaveras/*`, `?place=calaveras` → `ebbetts-pass`

**Consumers hitting `/api/v1/hazards/calaveras/*` (e.g. ersn.net maps) must update
the slug.** Done pre-deploy, so no stored-data migration (the dev DB is re-seeded).
Also fixed a docs typo that showed the county id as `county:calaveras` (correct:
`county:calaveras-county`).


### Changed — SQLite journal mode is configurable; default TRUNCATE (EFS-safe)

The grid store's journal mode is now `grid.journalMode` (`PF__GRID__JOURNALMODE`),
defaulting to **TRUNCATE** — which works on both local disk and a network
filesystem (NFS/EFS). WAL (the previous hardcoded mode) is faster for concurrent
reads but its memory-mapped `-shm` sidecar does **not** work over NFS/EFS, so it
is now opt-in and only for a real local disk. Rollback modes (TRUNCATE/DELETE/
PERSIST) run with `synchronous=FULL` (crash-safe history); WAL keeps NORMAL. Ops:
mount the persistent volume at the `/data` directory (not the file); a single
writer + single running task is required. Deploying on EFS needs no config change
(TRUNCATE is the default); WAL on local disk is `PF__GRID__JOURNALMODE=WAL`.


### Added — AI enhancement transparency: model I/O on `Event.enhancement`

`Event.enhancement` now carries `request` (the incident-specific prompt sent to
the model), `response` (the raw structured JSON returned), and a populated
`enhanced_at` (when the model ran — was previously unset on road incidents). So a
client can show what was sent and what came back, not just that enhancement
happened. Applies to both AI-enhanced layers (road incidents, weather alerts).
The data is **persisted** with the event (stored per revision, survives restart,
returned by `/v1/events/{id}` and `/history`) but **excluded from the content
hash**, so capturing it never churns revisions. On `/api/v1/incidents`, the same
I/O is exposed additively as `ai_request` / `ai_response` / `ai_enhanced_at`.

**Opt-in to keep lists lean.** The heavy `request` / `response` strings are
**omitted by default** and returned only when asked via `?enhancement_io=true`
(also `1`/`yes`). The lightweight provenance (`model`, `enhanced_at`, `fields` on
`/v1`; `ai_enhanced_at` on `/api/v1`) is always present. Applies to `/v1/events`,
`/v1/events/{id}`, `/v1/events/{id}/history`, `/v1/history`, and
`/api/v1/incidents`. The data site's event-detail page requests it; list views
and other consumers stay small by default.

### Added — `/api/v1/incidents[].original_text` (verbatim pre-AI text)

Each incident now carries `original_text`: the raw upstream feed text (CHP CAD
log / radio codes) as received, **before** any AI enhancement. `description` may
be an AI narrative; `original_text` is always the verbatim original, so a client
can show both — the "translate, never assert" transparency contract. Additive
field; existing consumers are unaffected.

### Fixed — road incident text roles on `/v1` events

For an AI-enhanced incident the `/v1` event now reads: `headline` = the short AI
condensed line, `summary` = the AI narrative, `description` = the **verbatim
original** (from `original_text`). `Event.enhancement.fields` accurately lists the
AI-generated Event fields (`headline, summary, severity`) — previously it listed a
`summary`/`description` that no longer matched the mapping. The
`.geojson` map envelope is unchanged (it shows the readable narrative as
`description`; byte-compatible).

## 2026-07-05 18:00 UTC

### Added — Grid Info Service v2: a `/v1` surface, a persistent event store, and a data site

The service now normalizes every hazard source into a canonical **event** model
persisted in SQLite with full revision history, and serves it through a new
first-principles API at `/v1` alongside a public data site. `/api/v1` is
untouched in shape (see the migration note below for behavior changes on the
hazard layers). All `/v1` JSON is **snake_case** (proto field names on the wire);
timestamps are RFC 3339; errors are `google.rpc.Status`; ETags/`If-None-Match`
everywhere. Full reference: the site's `/docs.html` (when deployed) and
`docs/v2-api-spec.md`; build/design notes in `docs/v2-implementation-plan.md`.

**New `/v1` endpoints:**

- `GET /v1/places/{place}/summary` — one-fetch place rollup: `mode`
  (QUIET/WATCH/ACTIVE), a cross-layer `summary`, per-`domains[]` status, top
  events, and a source-health sidecar. Carries the evacuation fail-loud invariant
  (`active_evacuations: int|null` + `evacuation_status`). Replaces
  `/api/v1/situation/{area}` (`layers[]` → `domains[]`, adds `mode`, scanners
  moved out).
- `GET /v1/places/{place}/map/{layer}.geojson` — RFC 7946 FeatureCollection per
  layer, envelope byte-identical to `/api/v1/hazards/{area}/{layer}.geojson`
  (map cutover is a source-URL swap).
- `GET /v1/events` — cross-layer query with filters
  `place,layer,status,severity_min,since,page_token,page_size` (default status
  `ACTIVE,SCHEDULED`), keyset pagination. Subsumes `/api/v1/incidents/{area}`
  (`layer=road_incident`) and weather-alert listing (`layer=weather_alert`).
- `GET /v1/events/{id}` — current revision of one event.
- `GET /v1/events/{id}/history` — that event's revision timeline.
- `GET /v1/history` — cross-event revision archive (`place,from,to,layer`).
- `GET /v1/places` / `GET /v1/places/{place}` — place directory (`kind`,`q`
  filters); places addressable by slug (`ebbetts-pass`) or id (`county:calaveras-county`).
- `GET /v1/places/resolve?lat=&lng=` or `?address=` — point/address → containing
  places, most-specific first (address path geocodes via the keyless Census
  geocoder).
- `GET /v1/roads` / `GET /v1/roads/{id}` — road conditions passthrough with an
  optional `?place=` bbox filter (alerts field retained).
- `GET /v1/weather` / `GET /v1/weather/{location}` — weather conditions +
  `fire_weather`, **minus per-location alerts** (alerts are events now — see
  below).
- `GET /v1/scanners?place=` — Broadcastify feed config (link-out only).
- `GET /v1/sources` — the source registry with per-source health.

**Event detail blocks are kind-specific only.** Each `detail` oneof block carries
only fields not already in the envelope — the incident/alert type is `category`,
the human location is `area_label`, the short line is `headline`, the sending
office is `provenance.source_name`, and the source page is `canonical_url`. The
`road_incident` `metadata` map additionally strips internal keys (`style_url`, a
KML rendering artifact; `source`; and `duration`, which is promoted to the typed
field). The `.geojson` envelope is unchanged (byte-compatible).

**Persistence (SQLite + revision history).** Events, their full revision
snapshots, the place directory, and source health live in a SQLite database
(`grid.dbPath`, default `./data/grid.db`; production points at a persistent volume (EFS) via
`PF__GRID__DBPATH=/data/grid.db`, mounted at `/data` in the container). The store
is the system of record: a restart **rehydrates** all events and revisions — no
warm-up re-fetch needed. Every state transition (including the all-clear when an
event leaves its feed) is written as a revision, so history is complete and
replayable.

**Sources registry + per-source health.** `/v1/sources` (and every
`source_status` on the map/summary endpoints) is driven by a registry of the
upstreams — `usgs, calfire, wfigs, caloes, nws, chp, caltrans` — each carrying
`status` (`OK|STALE|UNAVAILABLE`), last success/attempt times, poll interval, and
last error. A poll degrades a source `OK → STALE` (within 3× its poll interval of
the last success) `→ UNAVAILABLE` as failures age.

**Data site at `/`.** The homepage handler is replaced by an embedded static
site (`data.sierragridteam.org`) served at `/`: a source-health board, an event
explorer + detail/revision views, a place directory + zone resolver, a map layer
previewer, a history browser, and the hand-authored `/v1` reference at
`/docs.html`. Self-contained (MapLibre GL vendored, no CDN). The Docker
healthcheck on `GET /` still works.

### Changed — `/api/v1` hazard event layers are now store-backed (same envelope; behavior notes)

The five **event-backed** `/api/v1/hazards/{area}/{layer}.geojson` layers
(`wildfire`, `evacuation`, `weather_alert`, `earthquake`, `road_incident`) are
re-backed by the grid event store through a projection that is byte-compatible
with the previous live builders. The **conditions** layers (`road_segment`,
`chain_control`, `fire_weather`) are unchanged live projections. The envelope,
field names, and severity scale are identical; these are the deliberate behavior
changes (each also applies to the new `/v1/.../map/{layer}.geojson`):

- **`wfigs` standalone perimeter ids are stabilized.** A NIFC/WFIGS perimeter not
  joined to a CAL FIRE incident now has id `wfigs:{normalized-name}` (with a
  `-2`,`-3` centroid-ordered disambiguator for same-name perimeters), instead of
  the previous slice-index id that changed across polls. Ids were never stable
  before; treat this as an id-stability fix.
- **NWS "Extreme" alerts now rank `EXTREME` (rank 4), not `SEVERE`.** The store
  maps NWS severity directly; the old path collapsed "Extreme" into the API's
  `CRITICAL`, which projected to `SEVERE`. Sort/color for the top of the scale
  shifts up one rank. (`weather_alert` layer only.)
- **`earthquake` `updated_at` is omitted when it equals the event time.** Matches
  the prior omit-when-zero behavior; consumers already treated a missing
  `updated_at` as "never revised".
- **Upstream outages now serve `STALE` with the last-good stored data instead of
  `UNAVAILABLE`-empty**, once a source has succeeded and still has active events
  stored. The store *is* the last-good cache, so a transient source failure no
  longer blanks a layer that has data to show; `UNAVAILABLE`-empty is reserved
  for a source that failed with nothing stored to serve. The evacuation
  life-safety invariant is preserved: `UNAVAILABLE` still means empty features and
  `active_evacuations: null` — an error never becomes a `0`.
- **Store-backed layers no longer regenerate AI enhancement every poll.**
  Enhancement (and the summary text) is content-hash-gated and persisted with the
  event, so an unchanged alert keeps its enhanced fields across polls instead of
  being re-generated per refresh cycle — fewer OpenAI calls, stable output.
- **`road_incident` `headline` role corrected.** For an AI-enhanced incident,
  `headline` is now the short, card-renderable `condensed_summary` (previously it
  held the long detail text). The `/api/v1/hazards/{area}/road_incident.geojson`
  envelope shows the short headline + the readable narrative as `description`;
  the full `/v1` event field roles (`summary` = AI narrative, `description` =
  verbatim original) are finalized in the 2026-07-06 entry above. Unenhanced
  incidents (no condensed summary) keep the readable text as the `headline`. On
  deploy, existing stored incidents self-heal to the new shape on their next
  enhanced poll.

### Changed — weather alerts removed from `/v1/weather` (still on `/api/v1`)

On the new surface, `/v1/weather` and `/v1/weather/{location}` no longer carry
per-location `alerts[]` — weather alerts are events, queryable at
`/v1/events?layer=weather_alert` (and projected on the `weather_alert` map
layer). `fire_weather` stays on `/v1/weather`. The legacy `/api/v1/weather*` and
`/api/v1/weather/alerts` endpoints are **unchanged** — alerts remain there for
existing consumers.

### Deprecation plan — `/api/v1` (per `docs/v2-api-spec.md` §6)

`/api/v1` and `/v1` run on the same binary over the same store; there is no
compatibility shim to maintain. Frontends cut over per page: map layers first
(URL swap, zero shape risk), then the Now page onto `/summary`, then
incident/alert views onto `/events`. `/api/v1` will be **deleted after N quiet
weeks** in the access logs — target weeks, not months. New pollers (PSPS, FIRMS,
gauges, AQI) land on `/v1` only. Old → new mapping:

| Current | New |
|---|---|
| `/api/v1/situation/{area}` | `/v1/places/{area}/summary` |
| `/api/v1/hazards/{area}/{layer}.geojson` | `/v1/places/{area}/map/{layer}.geojson` |
| `/api/v1/incidents/{area}` | `/v1/events?place={area}&layer=road_incident` |
| WeatherService `/weather/alerts` | `/v1/events?layer=weather_alert` |
| `/api/v1/roads*` / `/api/v1/weather*` | `/v1/roads*` / `/v1/weather*` (weather minus alerts) |
| `/api/v1/scanners/{area}` | `/v1/scanners?place={area}` |

## 2026-07-04 21:00 UTC

### Added — region incidents are now AI-enhanced, with model-assessed severity

`GET /api/v1/incidents/{area}` items now go through the same AI pipeline as
road alerts (model: gpt-5-mini). Additive, non-breaking:

- `description` becomes a readable narrative (e.g. "A collision with injuries
  occurred; emergency services are on scene") instead of the raw type line.
- New optional fields on `Incident`: `condensedSummary` (short mobile text),
  `impact` (`IMPACT_NONE|LIGHT|MODERATE|SEVERE`), and `metadata` (structured
  extras like vehicles involved / injuries).
- `severity` is now derived from the model's impact assessment (severe →
  CRITICAL, moderate/light → WARNING, none → INFO — same mapping as road
  alerts) instead of a keyword heuristic, so severity values may shift for
  equivalent incidents compared to before.
- Enhancement is cached 24h by content hash and capped per refresh, so a large
  backlog enriches over a few refresh cycles — an incident may appear on one
  poll with structural fields and keyword-heuristic severity, then enhanced
  (with model severity) on the next. Consumers should treat the new fields as
  progressive enrichment.

## 2026-07-04 00:00 UTC

### Changed — weather alerts are now NWS-only (**breaking**); weather refresh 5m → 15m

The server was exceeding OpenWeather's One Call 3.0 free cap (1,000 calls/day)
fetching per-location alerts that, for US locations, are relabeled NWS data we
already fetch directly. OpenWeather is now used **only** for current conditions.

- **`GET /api/v1/weather/alerts`**: alerts with `source: "OPENWEATHERMAP"` are
  gone; every alert now has `source: "NWS"`. **Migration**: drop any
  source-based branching; read `headline` + `description` (authoritative NWS
  wording). The AI-enhancement fields `summary`/`details` — previously populated
  on OpenWeatherMap alerts — are empty on NWS alerts (unchanged for NWS-sourced
  alerts; the fields remain in the schema).
- **`GET /api/v1/weather`** (and `/weather/{location_id}`): per-location
  `alerts[]` are now the NWS alerts active in the location's configured forecast
  zone, instead of OpenWeather One Call alerts. Same `WeatherAlert` shape, but:
  ids are NWS URNs (no longer prefixed `{locationId}_…`), `zones` is populated,
  and the same alert appears under every location in the affected zone.
- The `hazards` `weather_alert` layer inherits this: feature `source` is always
  `NWS` now.
- **Freshness**: `weather.refreshInterval` raised from 5m to **15m** (current
  conditions may be up to ~15 minutes old; up to 30m when serving stale on an
  upstream failure). Alert latency is effectively unchanged — NWS alerts refresh
  on the same interval and NWS is the originating source.
- Reliability note: a NWS outage on the alerts path now propagates as an
  error/stale-cache response rather than being silently absorbed, consistent
  with the fail-loud `source_status` contract.
- Bug fix: the weather endpoints' serve-stale-on-upstream-failure fallback was
  dead code (the cache accessor it used never returns stale entries), so an
  upstream failure after the TTL returned an error even when ≤2×TTL-old data
  existed. The fallback now works: on refresh failure you may receive stale
  data with `lastUpdated` reflecting its true age, instead of a 5xx.

## 2026-06-29 00:00 UTC

### Changed — evacuation now distinguishes "no active zones" from "feed error"

Previously a healthy Cal OES feed reporting **no** active evacuation zones was
indistinguishable from a Cal OES **error**: both returned `source_status:
UNAVAILABLE` with empty features (and `situation.active_evacuations: null`). On a
normal quiet day that read as a permanent outage. The two are now split:

- **Confirmed-empty** (Cal OES healthy, no active zones):
  `…/evacuation.geojson` → `metadata.source_status: "OK"` with `features: []`;
  `…/situation` → `evacuation_status: "OK"`, `active_evacuations: 0`.
- **Error/unreachable** (transport error, non-2xx, or ArcGIS error-envelope):
  `source_status: "UNAVAILABLE"`, `evacuation_status: "UNAVAILABLE"`,
  `active_evacuations: null` (unchanged).

So `active_evacuations` can now be `0` (it previously never was). Consumer action:
render `0` as "no active evacuations reported by Cal OES" (still caveated — the
Genasys `source_url` is present in every state and it is never a guaranteed
all-clear), and keep rendering `null` as "unknown — check Genasys". The safety
invariant is preserved: **an error never becomes a `0`** (empty results are not
cached, so a later fetch error can't replay a stale `0`).

## 2026-06-27 01:15 UTC

### Changed — road-condition alerts are now localized to the matching segment

`GET /api/v1/roads` no longer duplicates a route-wide Caltrans road condition
(`roads.dot.ca.gov`, `alerts[].title = "SR N Road Condition"`) onto every
monitored segment of that highway. These advisories have no coordinates, so the
feed returns every condition on the route statewide; a segment now shows one only
when the condition text matches that segment's section/`locationKeywords`.

Effect for consumers: fewer, correctly-placed condition alerts. An out-of-area
advisory (e.g. an SR-4 closure at the Delta) no longer appears on the Calaveras
SR-4 segments, and the "No traffic restrictions are reported" filler no longer
shows. Geolocated CHP/lane-closure incidents (which have real coordinates) are
unaffected. No response-shape change.

## 2026-06-27 00:30 UTC

### Changed — hazard layer resilience + contract polish (code-review follow-up)

Hardening pass on the M1–M5 hazard endpoints. All additive/clarifying — no
consumer of the (still-unreleased) hazard API needs to change, but new fields and
states are now observable:

- **`source_status: STALE` is now emitted** (previously only OK/UNAVAILABLE). A
  layer reports `STALE` when it is serving the last good fetch after a transient
  upstream failure, or when one of a layer's multiple sources failed (e.g. CAL
  FIRE up but WFIGS down). `metadata.last_source_update` (and, in `/situation`,
  `layers[].last_source_update`) carries the RFC3339 time of that last good fetch.
- **`/situation` `active_evacuations` now also reports a count when evacuation
  data is `STALE`** (served from cache), with `evacuation_status: "STALE"`. It
  remains `null` only when truly `UNAVAILABLE`. Still never `0`.
- **`road_segment` numeric fields (`delay_minutes`, `duration_minutes`,
  `distance_km`) are now omitted when a segment has no live data yet**, instead of
  serializing `0`. A present `0` now unambiguously means a real zero (e.g. no
  delay). `congestion`/`status` were already omitted when absent.
- Evacuation zones are now selected by the area's geographic bounds (ArcGIS
  spatial query) rather than a county-name list, so an in-area zone tagged to a
  neighboring county is no longer dropped. No response-shape change.
- Internal: the new upstreams (USGS, CAL FIRE, WFIGS, Cal OES) and the live
  Caltrans chain-control fetch are now server-side cached (2–5 min TTL) with
  stale-on-error fallback; a burst of map clients no longer fans out to every
  source on every request.

## 2026-06-26 23:55 UTC

### Added — hazard layers M2–M5 (earthquake, wildfire, evacuation, situation rollup)

Completes the hazard aggregation roadmap. All additive — existing endpoints and
the M1 layers are unchanged.

New GeoJSON layers at `GET /api/v1/hazards/{area}/{layer}.geojson`:

- `earthquake` — USGS events (M≥2.5, last 7 days) within the area bounds, as
  Points with `properties.earthquake` (`magnitude, depth_km, felt`).
- `wildfire` — CAL FIRE active incidents joined to NIFC/WFIGS perimeters by fire
  name. Polygon where a perimeter exists, else a Point; `properties.wildfire`
  (`acres, containment, county, has_perimeter`).
- `evacuation` — Cal OES active evacuation zones (Order/Warning/Advisory/SIP) as
  Polygons; `properties.evacuation` (`zone_id, level, event_type, county`).
  **Fail-loud / life-safety:** this is an active-events-only source, so an empty
  result is `metadata.source_status = UNAVAILABLE` (never an implied all-clear),
  and `metadata.source_url` always links the authoritative Genasys viewer
  (`protect.genasys.com`). Attribution is "reference only".

New JSON (non-GeoJSON) endpoints:

- `GET /api/v1/situation/{area}` — one-call rollup for a dashboard: per-layer
  `source_status` + `feature_count`, a cross-layer `summary` (`highest_severity`,
  `severity_counts`, `top_headlines`), and a `scanners` sidecar.
  **`summary.active_evacuations` is `null` when evacuation data is unavailable**
  (`summary.evacuation_status` says which) — render `null` as "unknown", never as
  zero. A real `0` only appears when Cal OES answered with no active zones.
- `GET /api/v1/scanners/{area}` — Broadcastify scanner feeds for the area
  (`feed_id, channel_label, agency, broadcastify_url`). Link-out only; no embed.

## 2026-06-26 22:28 UTC

### Added — unified hazard GeoJSON feed (M1)

New map-ready endpoints aggregating hazard data into one standardized RFC 7946
GeoJSON interface (see `docs/hazard-aggregation-design.md`):

```
GET /api/v1/hazards/{area}/{layer}.geojson
```

- Areas are configured under `hazards.areas` in `prefab.yaml` (first: `ebbetts-pass`).
- M1 layers (re-project existing feeds): `road_incident`, `chain_control`,
  `road_segment` (LineString), `weather_alert` (null-geometry banner),
  `fire_weather` (null-geometry banner). Roadmap: `earthquake`, `wildfire`,
  `evacuation`, and a `/situation/{area}` aggregator.
- Every feature uses a common `properties` envelope (`id, layer, kind, severity,
  severity_rank, headline, source, …`) + a namespaced per-kind block, and a
  unified severity scale `INFO..EXTREME` (rank 0–4) for sort/color.
- Coordinates are RFC 7946 `[longitude, latitude]`. Collections carry a
  `metadata` member with `source_status` (OK/STALE/UNAVAILABLE) for fail-loud
  provenance. `Content-Type: application/geo+json`.
- Consumes directly in MapLibre GL / Leaflet (`addSource({type:'geojson'})`).

This is additive — existing endpoints are unchanged.

## 2026-06-26 16:47 UTC

A large API cleanup pass. Several responses changed shape — see **Breaking
changes** first.

### ⚠ Breaking changes (consumers must update)

| Area | Before | After | Migration |
|------|--------|-------|-----------|
| **Weather alert times** | `startTimestamp` / `endTimestamp` (unix seconds, as a quoted string) | `startTime` / `endTime` (RFC3339 string, e.g. `"2026-06-26T02:33:00Z"`) | Parse RFC3339 instead of `parseInt(...)*1000`. |
| **Enum values** | lowercase / mixed strings | UPPER_SNAKE enum names | See enum table below. |
| **Fire weather location** | `weatherData[].fireWeather` (one per location, all identical) | top-level `fireWeather` on the `/weather` and `/weather/{id}` responses | Read `response.fireWeather` once instead of per location. |
| **Empty timestamps** | `""` (empty string) | `null` / omitted | Treat missing time as null. Affects `fireWeather.effective/expires`, `chainControlInfo.effectiveTime`. |
| **Incidents URL** | _new this session, no migration_ | `GET /api/v1/incidents/{area}` (area is a path param) | n/a (endpoint is brand new). |
| **Metrics URL** | `GET /api/v1/roads/metrics` (returned all-zeros) | `GET /api/v1/metrics` (returns `501 Unimplemented` until real metrics exist) | Stop relying on it; it was never real data. |
| **Client errors** | `500` for unknown road/location/area | `404` (unknown id) / `400` (bad input) | Handle 4xx as "not found / bad request", not server error. |

**Enum value changes** (JSON string values):

| Field | Before | After |
|-------|--------|-------|
| `roads[].alerts[].impact` | `"none"`,`"light"`,`"moderate"`,`"severe"` | `"IMPACT_NONE"`,`"IMPACT_LIGHT"`,`"IMPACT_MODERATE"`,`"IMPACT_SEVERE"` |
| `roads[].alerts[].duration` | `"unknown"`,`"< 1 hour"`,`"several hours"`,`"ongoing"` | `"DURATION_UNKNOWN"`,`"DURATION_UNDER_ONE_HOUR"`,`"DURATION_SEVERAL_HOURS"`,`"DURATION_ONGOING"` |
| `incidents[].status` | `"active"` | `"ACTIVE"` |
| `fireWeather.state` | `"normal"`,`"elevated"`,`"red-flag"` | `"NORMAL"`,`"ELEVATED"`,`"RED_FLAG"` |
| weather `alerts[].source` | `"NWS"`, `"OpenWeatherMap"` | `"NWS"`, `"OPENWEATHERMAP"` |
| weather `alerts[].severity` | NWS text (`"Severe"`,`"Moderate"`,`"Minor"`) | `"CRITICAL"`,`"WARNING"`,`"INFO"` (shared scale) |

(Existing road `status`, `congestionLevel`, alert `type`/`severity`/`classification`
were already UPPER_SNAKE enums and are unchanged.)

### Added

- **Region-wide incidents feed**: `GET /api/v1/incidents/{area}` (e.g.
  `/api/v1/incidents/mother-lode`) — a flat list of CHP/Caltrans dispatch
  incidents in an area, independent of the monitored roads. Each incident has
  `id`, `type`, `severity`, `location`, `locationDescription`, `description`,
  `status`, `logNumber`, `started`, `lastUpdated`, `area`.
- **Authoritative NWS weather alerts**: `/weather/alerts` now returns NWS zone
  alerts (`source: "NWS"`, with `severity` and `zones`) alongside OpenWeatherMap
  alerts (`source: "OPENWEATHERMAP"`). New `?zones=CAZ064,CAZ065` filter narrows
  the NWS alerts (OpenWeatherMap alerts are not zone-scoped and always pass).
- **Fire-weather classification**: top-level `fireWeather` on `/weather` and
  `/weather/{id}` — `state` is `NORMAL` → `ELEVATED` (Fire Weather Watch) →
  `RED_FLAG` (Red Flag Warning), only ever `RED_FLAG` when NWS confirms it.
- **Road alert id**: `roads[].alerts[].id` (CHP log / closure number) — matches
  `incidents[].id` for the same event, so per-road alerts and the region feed can
  be correlated.
- **Chain control**: `roads[].chainControlInfo` (level R1/R2/R3, location,
  direction, `effectiveTime`) from the Caltrans chain-control feed.
- **Coverage**: Hwy 49 (Angels Camp ↔ Sonora) road; weather for Sonora, Columbia,
  Twain Harte, Dorrington.
- **HTTP caching**: read endpoints now send `Cache-Control: public, max-age=60`
  and `Last-Modified`.
- **CORS**: `https://www.ersn.net`, `https://ersn.net`, `https://sierragridteam.org`
  (and `www.`) are allowlisted; browser `fetch()` now receives
  `Access-Control-Allow-Origin`.

### Changed

- Incident `description` is humanized (`"1182-Trfc Collision-No Inj"` →
  `"Traffic Collision-No Injury"`).
- Incidents are de-duplicated and geometry-only placemarks dropped, so the feed
  is one clean entry per incident.
- All timestamps across the API are RFC3339 (`google.protobuf.Timestamp`).

### Fixed

- CORS: `Access-Control-Allow-Origin` is now emitted for allowlisted origins;
  `Access-Control-Allow-Methods` is correctly restricted to `GET`; the needless
  `Access-Control-Allow-Credentials` header was removed.
- Unknown road / location / area now return `404`, and bad input `400`, instead
  of `500`.
