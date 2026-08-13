# External API Clients

Each subpackage wraps one upstream data source. They are thin: fetch, parse, and
return API/proto types or small structs. Caching, classification, and AI
enhancement live in `internal/services`, not here.

| Package    | Source                | Auth                          | Notes |
|------------|-----------------------|-------------------------------|-------|
| `google`   | Google Routes API     | `PF__GOOGLE_ROUTES__API_KEY`  | Travel time + polyline. Rate-limited; callers cache aggressively (10k/mo budget). |
| `caltrans` | quickmap.dot.ca.gov KML | none                        | Lane closures, CHP incidents, chain control. |
| `weather`  | OpenWeatherMap        | `PF__OPENWEATHER__API_KEY`    | Current conditions only. `GetWeatherAlerts` (One Call 3.0, 1,000/day cap) is CLI-diagnostic only — the server sources alerts from `nws`. |
| `nws`      | api.weather.gov       | none (User-Agent required)    | Authoritative zone alerts + fire-weather products. |
| `firis`    | ArcGIS (CAL FIRE org) | none (public)                 | CAL FIRE/FIRIS combo fire perimeters. Dedup + `LastEdit` gating live in `internal/ingest` (wildfire). Replaced `wfigs` (retained unused). |
| `pge`      | ArcGIS (PG&E)         | none (public, undocumented)   | Electric outages (points + affected-area polygons) and PSPS coverage, plus PG&E's own ETL stamp. See below. |

All clients accept an `HTTPDoer` interface and expose a `NewClientWithHTTPDoer`
constructor so tests can inject canned responses instead of hitting the network.

## Caltrans KML — the format changed in 2026 (important)

The quickmap feeds (`chp-only.kml`, `lcs2way.kml`, `cc.kml`) **switched from a
legacy layout to a Google-Maps "infowindow" (`iw-*`) layout** around 2026:

- `<name>` is now **blank** (` `). The incident label moved into the
  description's `<div class="iw-header-left">` ("CHP Incident 260625SA1034") or
  `<h2 class="iw-title">` ("Route 4 One-way Traffic Operation").
- Details live in `<p class="iw-text">` blocks, the type in `<h2 class="iw-title">`,
  and the timestamp in `<span class="iw-timestamp">Last updated: <strong>…`.
- Lane closures carry `Closure ID: …, Log Number: …`.

`processPlacemark` backfills a meaningful `Name` from the description when the
KML `<name>` is blank (`deriveNameFromDescription`), so downstream road-alert
titles and the incidents feed keep working. The structured field parsing (log
number, location, reported time) lives in `internal/services/incidents.go`.

The test fixtures under `tests/testdata/caltrans/` are mostly the **legacy**
format; parsing keeps a legacy fallback so those tests stay valid. When the feed
format shifts again, capture a fresh sample with
`curl https://quickmap.dot.ca.gov/data/chp-only.kml` and add a fixture.

Caltrans/CHP timestamps are **Pacific time** with no zone marker. Parse them with
`time.ParseInLocation(..., America/Los_Angeles)`, not `time.Parse` (which would
mislabel them UTC). `cmd/server` blank-imports `time/tzdata` so the zone resolves
even in a minimal container.

## NWS (`nws`)

- No API key, but api.weather.gov **requires a descriptive `User-Agent`**
  (configured as `weather.nws.userAgent`). Requests without it get 403s.
- `GetActiveZoneAlerts(zones)` queries `/alerts/active?zone=CAZ064,...`. An empty
  zone list returns nothing (never a statewide fetch).
- `ClassifyFireWeather` derives Normal → Elevated → Red Flag purely from active
  products (Fire Weather Watch → elevated, Red Flag Warning → red-flag). It never
  invents a Red Flag that NWS hasn't issued — see issue #5.
- **Four timestamps, two pairs. Use `Begins()`/`EndsAt()`, not the raw fields.**
  `effective`/`expires` are the PRODUCT's window (issued at / re-issue by);
  `onset`/`ends` are the HAZARD's. A watch is issued the moment it is written, so
  they routinely disagree by a day or more — reading `expires` as a hazard end
  made records claim an alert was over before its weather arrived, and reading
  `effective` as a hazard start meant an advance watch was never `SCHEDULED`.
  `NWSAlertID`'s fallback still keys on raw `Effective`: an id is an identity,
  not a schedule, and changing it would rewrite every synthesized id.
- **`Alert.ShortHeadline()` is the display line**, not `Headline`. CAP's
  `properties.headline` is issuance boilerplate ("… issued August 11 at 9:57AM
  PDT until … by NWS Sacramento CA") whose every token is already a structured
  field. `ShortHeadline` composes `<Event> — <reason>` from the product name and
  the reason clause in `parameters.NWSheadline`. Deterministic on purpose — see
  `internal/ingest/CLAUDE.md` for why this must never become an AI field.
- Zone codes for the service area (verify with `api.weather.gov/points/{lat},{lng}`,
  don't guess): CAZ137 (1000–3000 ft), CAZ138 (3000–5000 ft), CAZ139 (above
  5000 ft) — NWS Sacramento (STO), covering Calaveras & Tuolumne.

## PG&E (`pge`) — undocumented endpoints, so the failure mode is silence

Folder 43 of PG&E's ArcGIS server is the backend behind their public outage map.
There is no API contract, no version, no published terms, and no `robots.txt`.
Treat a schema change as a matter of when, not if — the same posture as the
Caltrans KML feeds, which did exactly that in 2026.

Three feeds, four requests:

- **`outages/MapServer/4`** (points) and **`/8`** (polygons), joined on
  `OUTAGE_ID`. **Both must succeed or `GetOutages` fails.** Degrading to
  point-only geometry looks harmless but geometry is in the event content hash,
  so a polygon-layer blip would flip an outage's geometry there and back and
  mint a spurious revision pair in the history of an outage that never changed.
  An outage can have several polygon rows (a multi-part area); they combine into
  one MultiPolygon so it stays ONE event.
- **`psps_public/MapServer/1`** — PSPS coverage. **Empty is the normal state**;
  the layer only fills during an event. A window is published as MANY rows
  sharing every attribute (12 rows for one real footprint), so the caller groups
  them — `internal/ingest` does, on `(EventID, TimePeriod)`.
  **`psps_staging` on the same host holds PG&E's TEST events** (names like
  `PSPS_05312024_SKN9_TEST52`, future-dated windows). Never consume it; it is
  useful only for reading the schema when no real event is running.
- **`lastupdate_time/MapServer/1`** — PG&E's ETL stamp for the outage service.
  This is the important one. These endpoints do not fail with a 500; they fail
  by **freezing** — still answering 200, still serving the last set, so a
  restored outage stays listed forever and a new one never appears. The stamp is
  the only way to see it. (Not hypothetical: the Cal OES statewide mirror of this
  same data was measured 26 h stale while reporting every row as `Active`, which
  is why we read PG&E directly.) `internal/ingest` turns an old stamp into a
  source failure — see that package's guide.

Field-type traps, all confirmed against live responses:

- The outage layers publish **epoch-millisecond integers** (`OUTAGE_START`,
  `LAST_UPDATE`, `CURRENT_ETOR`) with `_TEXT` string twins; PSPS publishes
  **RFC 3339 strings** and **stringified counts** (`TotCustAff: "74786"`).
- The ETL stamp is a bare `2006-01-02 15:04:05` with **no zone marker**. It is
  UTC — parse it with `time.ParseInLocation(..., time.UTC)`, never `time.Parse`
  in local time, or the freshness gate shifts by the offset.
- `COUNTY` is **null on most outage rows**, so scoping is spatial (envelope
  intersect), never by county string.
- `OUTAGE_CAUSE` is null on roughly half of all rows.

Query hygiene: ask for `geometryPrecision=5` (the repo-wide 5-decimal GeoJSON
convention) and, for PSPS only, `maxAllowableOffset` — a county-scale coverage
polygon set measured **8.0 MB raw vs 222 KB simplified** with the same feature
count. Outage polygons are a few hundred metres across and are NOT simplified.

`./bin/test-pge` (`make test-pge`) probes all of this live, including whether
the freshness gate would call the feed frozen.

## Cal OES evacuations (`caloes`) — the columns move without warning

This layer changed shape under us and nothing failed. Measured across all 37
active rows statewide on 2026-08-13:

| column | populated | note |
|---|---|---|
| `ZONE_ID`, `COUNTY`, `STATUS` | 37/37 | the identity + level we key on |
| `NOTES` | **37/37** | where the public directive text lives NOW |
| `EditDate` | **37/37** | ArcGIS editor tracking — the only freshness signal |
| `PUBLIC_INFO` | **0/37** | the documented directive field |
| `ZONE_NAME`, `EVENT_TYPE`, `CITY`, `CRITICAL_INFO`, `STATEWIDE_LAST_UPDATED` | **0/37** | all empty |

Consequences to keep in mind:

- **Read both text columns, prefer the documented one.** `PUBLIC_INFO` first,
  then `NOTES`. Until 2026-08-13 the client asked only for `PUBLIC_INFO`, so
  every evacuation we served carried an EMPTY `description` — the one field this
  layer exists to deliver — while Monterey's *"...issuing an immediate
  EVACUATION ORDER... Leave Now."* sat unread in `NOTES`.
- **`NOTES` is free text, not a label.** Its meaning varies by county: a street
  ("Southgate Dr", Tuolumne), the full sheriff's instruction (Monterey,
  Humboldt), or the event type ("Flooding", Tulare). Carry it; do not parse it,
  and do not put it in a headline.
- **`EditDate` (case-sensitive, distinct from the always-null `EDIT_DATE`) is
  how you spot an ORPHANED row** — a zone a county lifted that the aggregation
  never retracted. The script rewrites live rows continually, so a row frozen for
  days is a stale one. Observed: every county within 1.8 days except a Tuolumne
  row at 6.3. `internal/ingest` uses it for `observed_at` and logs the outliers —
  it deliberately does NOT expire them (see that package's guide).
- **`ZoneURL` needs the COUNTY, not just the zone id.** Non-Genasys counties have
  no parseable id scheme (Tuolumne's is `US-CA-Toulumne117`, including upstream's
  misspelling), and sending them to `protect.genasys.com` links a resident to a
  viewer their zone will never appear in. County viewers show **live zones only**,
  so never construct a per-zone deep link into one.
