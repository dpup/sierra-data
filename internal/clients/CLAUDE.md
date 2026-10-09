# External API Clients

Each subpackage wraps one upstream data source. They are thin: fetch, parse, and
return API/proto types or small structs. Caching, classification, and AI
enhancement live in `internal/services`, not here.

| Package    | Source                | Auth                          | Notes |
|------------|-----------------------|-------------------------------|-------|
| `google`   | Google Routes API     | `PF__GOOGLE_ROUTES__API_KEY`  | Travel time + polyline. Rate-limited; callers cache aggressively (10k/mo budget). |
| `caltrans` | quickmap.dot.ca.gov KML | none                        | CHP incidents, chain control (merged with `cwwp2` when configured), lane closures (`lcs2way.kml`; read only when `cwwp2.laneClosureDistricts` is empty; otherwise the `road_incident` layer and per-road status both read `cwwp2`). |
| `cwwp2`    | cwwp2.dot.ca.gov JSON (Caltrans data portal) | none (public, undocumented) | Per-checkpoint chain-control status; lane-closure windows incl. scheduled (the `road_incident` closures); message-sign text; CCTV camera list. See below. |
| `weather`  | OpenWeatherMap        | `PF__OPENWEATHER__API_KEY`    | Current conditions only. `GetWeatherAlerts` (One Call 3.0, 1,000/day cap) is CLI-diagnostic only — the server sources alerts from `nws`. |
| `nws`      | api.weather.gov       | none (User-Agent required)    | Authoritative zone alerts + fire-weather products. |
| `firis`    | ArcGIS (CAL FIRE org) | none (public)                 | CAL FIRE/FIRIS combo fire perimeters. Dedup + `LastEdit` gating live in `internal/ingest` (wildfire). Replaced `wfigs` (retained unused). |
| `pge`      | ArcGIS (PG&E)         | none (public, undocumented)   | Electric outages (points + affected-area polygons) and PSPS coverage, plus PG&E's own ETL stamp. See below. |
| `burnline` | county burn phone line | Twilio + OpenAI              | Places a recorded call, transcribes it, extracts today's burn day. Driven by `cmd/burn-line` from CI, not the server — see below. |
| `twilio`   | Twilio REST API       | account SID + auth token      | Minimal hand-rolled client: place a recorded call, hang up, fetch the recording. |
| `calfireburn` | burnpermit.fire.ca.gov HTML | none (browser headers required) | CAL FIRE per-county burn suspension. HTML scrape behind Akamai bot management — see below. |

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

## Caltrans CWWP2 (`cwwp2`) — Caltrans's structured data portal

QuickMap (quickmap.dot.ca.gov) is a static app: `/config/layers.json` maps each
layer to a KML under `/data/`. Some of those KMLs visibly draw on the CWWP2
portal (the camera KML links CWWP2 snapshot URLs), which publishes structured
JSON/XML/CSV per district at
`https://cwwp2.dot.ca.gov/data/d{N}/{feed}/{feed}StatusD{NN}.json`. **The file
name zero-pads the district, the directory does not** (`/d3/cc/ccStatusD03.json`).
District 10 covers our whole footprint (Calaveras, Tuolumne, Alpine, Amador,
Mariposa).

- **Chain controls (`cc`) — in use, MERGED with cc.kml, neither ranked.**
  CWWP2 lists every checkpoint with an explicit status, so a quiet day is
  149 × `R-0` rather than an empty file — an empty cc.kml can't be told from a
  broken one. The client fails an empty file (`ErrEmptyFeed`), a frozen one
  (`ErrStaleFeed`, newest `recordTimestamp` older than `staleAfter`, default
  1h) and one whose stamps no longer parse (`ErrNoRecordTime`).

  **Do not make either source authoritative over the other.** Both describe
  the same checkpoint registry — every point in the 2025-12-24 cc.kml capture
  sits 0 m from a same-named CWWP2 checkpoint, pass-closure gates included —
  but cc.kml carries Highway-Information-style `District / Message ID`
  identifiers (the CHIN layer uses the same scheme; CWWP2 uses checkpoint
  indexes), so its STATUS may come from a different system, and the two have
  never been seen side by side in a storm. `caltrans.mergeKML`:

  - cc.kml road closures (`#full-closure` → `ChainControlData.Closed`) and
    truck-only `MAX`/`MIN`/`TS` levels are always kept (`isKMLSupplement`,
    positive match only).
  - Any other cc.kml entry at an ACTIVE CWWP2 checkpoint (≤200 m, same
    direction) is a duplicate and dropped. Matching is **positional**, so it
    survives cc.kml moving to the blank-`<name>` iw-* layout.
  - Every other cc.kml entry is KEPT: outside the configured districts CWWP2 is
    silent, and inside them a disagreement shows the control (flagged
    `Uncorroborated` when it sits on an R-0 checkpoint; the `chain_control`
    layer logs these — the evidence #12 needs).
  - An `Unrecognized` CWWP2 checkpoint with a cc.kml entry at the same spot is
    dropped (cc.kml says what's there), so a closure gate carrying a non-R
    status all winter doesn't degrade the layer for five months.
  - CWWP2 down → cc.kml alone, as `PartialError` (what we served before
    CWWP2), unless cc.kml is empty, which stays a hard error. cc.kml down →
    CWWP2 alone, as `PartialError`.

  CWWP2 can list **two checkpoints at one spot** (`RED LAKE CREEK` and
  `RED LAKE CREEK - CARSON PASS`, 1 m apart, both westbound — a chain sign and
  its closure gate). An out-of-service checkpoint is skipped only when it
  reports R-0. `Closed` entries are never a chain requirement: the roads
  service skips them (the Ebbetts gate ~3 km past Bear Valley would otherwise
  mark Arnold–Bear Valley "chains required" all winter).
- **Lane closures (`lcs`) — the `road_incident` layer's closures** (via
  `ingest.LaneClosureNormalizer`, `laneClosureDistricts: [3, 10]`) **and
  per-road segment status** (ACTIVE windows only, via
  `caltrans.IncidentFromCWWP2LaneClosure`; see `internal/services/CLAUDE.md`). One row per
  closure WINDOW, including scheduled ones (10x the closures lcs2way.kml shows
  for the incident box), with epoch times and the radio codes 10-97 (set up),
  10-98 (picked up), 10-22 (cancelled). `LaneClosure.PhaseAt` derives
  SCHEDULED/ACTIVE/COMPLETED/CANCELLED. Codes win over the clock, so a set-up
  closure past its window is still ACTIVE (overruns happen) — but only for
  `laneClosureOverrunGrace` past its planned end (default
  `DefaultOverrunGrace`, 12h). Crews sometimes never radio the 10-98 and the
  row lingers for days; after the grace `PhaseAt` presumes it COMPLETED.
  Indefinite windows have no planned end and are exempt. Two gates keep a
  broken file from reading as "every closure ended":
  - **An empty file is `ErrEmptyFeed`** (D3 lists ~1,100 windows, D10 ~630, D9
    81; an empty file also has no record stamp to check freshness against).
  - **`LaneClosure.Unrecognized`** names a row that can't be phased: a code flag
    that is not exactly `true`/`false` (a renamed key decodes as blank), no
    start, no end on a non-indefinite window, no index, or no position at
    either end. All 2,593 rows across D3/D6/D9/D10 on 2026-10-01 pass. The
    poller degrades the source on any such row in scope.
  `recordTimestamp` is the FILE's generation stamp (every row carries the same
  one), not a per-row edit time. The portal does not gzip.
- **Message signs (`cms`) — in use: the `message_sign` map layer (#13).** It
  is always `INFO` and `/summary` doesn't read it; see
  `internal/hazards/CLAUDE.md`. Every changeable
  message sign with what it is showing: up to two pages ("phases") of three
  lines. D10 has 107 signs, 11 of them in the five mountain counties
  (Hwy 4 west of Murphys, Hwy 108 at Soulsbyville, Hwy 120 at Moccasin and
  Buck Meadows, Hwy 88 at Pine Grove and Dew Drop, among others).
  `MessageSign.Display` is believed only when the lines agree with it (blank
  = no text, one page = text in phase 1 alone, two pages = text in both);
  anything else, and the portal's `"Not Reported"` placeholder, is
  `DisplayUnknown`. That is never "blank" and never the text "Not Reported".
  Read the message through `Pages()`/`Text()`, which honor `Display`;
  `Phase1`/`Phase2` keep the raw lines for diagnosis. An empty file is
  `ErrEmptyFeed` and a frozen one `ErrStaleFeed`, like `cc`.

  **Most of what the signs say is boilerplate, and it rotates.** On 2026-10-01,
  82 of D10's 107 signs showed one statewide safety campaign ("BE THE DRIVER /
  WHO SAVES LIVES / DON'T SPEED"). The day before, the issue saw a different
  one on every sign ("SAVE LIVES / SLOW DOWN / IN WORK ZONE"), and most
  in-area signs changed message at 08:00 PDT on 9/30. So a fixed boilerplate
  list will go stale; a message on dozens of signs at once is the better
  tell. Classifying is the caller's job — this package only parses.
  Real operational text does appear ("SR 20 / TRAFFIC CONTROL / EXPECT
  DELAYS", D3), so the signal exists.

  Quirks, all from the 2026-10-01 survey of all twelve districts:
  - Lines are XML-escaped (`DON&apos;T`), sometimes padded to position text on
    the face (`" .US 50 22 MIN"`), and not always upper case (D8 travel times).
  - `inService` is `true`/`false` in some districts, `True`/`False` in D3, D8,
    D11 and D12, and blank on two D2 rows (read as not in service).
  - The index is unique in D10 (`V42`, matching the sign number in the name)
    but **not an identity elsewhere**: D12 numbers all 67 signs `1`, D3 and
    D11 repeat some, D3 has `N/A`. It never carries the district. Key on it
    only per district, and only where it is unique.
  - `route` names one route where routes share the road: D10's `45 - EB 49
    (MOCCASIN)` is filed under SR-120 and `43 - EB 120 W/O YOSEMITE` under
    SR-108. Match a sign to a corridor by position, not route.
  - `messageTimestamp` (when the message last changed) is `"Not Reported"`
    throughout D7, even on signs showing text, and `1970-01-01 00:00:00` on a
    D10 sign that has never reported. Both parse as zero.
  - **D7's file was frozen for over two days** while still answering 200, and
    still showing an `INCIDENT ... RT LANES BLKD` message. Every other
    district's record stamp was within two minutes of the wall clock; D10's
    advanced every two minutes.
  - D2 has a row (index `0`) with an entirely blank location.
- **Cameras (`cctv`) — in use, District 10 only** (`GET /api/v1/cameras`, via
  `services.CameraService`). One row per camera: snapshot JPG URL, refresh
  minutes, HLS `.m3u8` URL (D9 has none: image-only), position, route.
  `Camera.ID` is `d{district}-{index}` because the index is per-district. D10
  names carry a camera-number prefix (`179 - EB 108 …`), which is stripped.
  Findings from 2026-10-01:
  - **No freshness check is possible.** `recordTimestamp` is when the camera's
    RECORD was edited (2022–2026), not when the file was generated. The file's
    Last-Modified moves only when Caltrans edits the registry, weeks to months
    apart.
  - **`inService: false` is the only dead-camera signal, and it is
    conservative.** Every out-of-service camera checked in D10 and D9 serves a
    "Down for Construction" placeholder, so the directory drops them. Two D3
    ones (I-80 Chiles Rd) were live anyway. **Image age cannot catch a dead
    camera**: the placeholder is regenerated every cycle with a current
    burned-in timestamp and a fresh Last-Modified.
  - Coverage near us: Hwy 108 Soulsbyville (inside the Ebbetts Pass area),
    Hwy 88 Pine Grove (13.2 km out), Hwy 120 Ferretti Rd (13.9 km) and Buck
    Meadows (23.9 km). There are none on Hwy 4 or Hwy 49, and none on Carson,
    Ebbetts or Sonora Pass. **D3** has no Hwy 88 camera; its Sierra cameras
    are all US-50, the nearest at Wrights Lake (26.5 km). **D9**'s only Sonora
    Pass camera is image-only, at the US-395/SR-108 junction (43 km). Both are
    outside the 25 km radius, so only D10 is configured.
  - URLs go straight into consumers' `<img>`/players, so anything that is not
    an absolute `https` URL is dropped at parse.
- **Also available, unused:** `rwis` (road-weather stations — D10's are all Valley sites, useless to us, and its
  JSON doesn't parse).

**It is hand-templated JSON; parse strictly and never read garbage as R-0.**
Observed 2026-09-30: `rwis` drops commas between repeated sensor entries (XML
variant is fine); D11 `cc` is not valid UTF-8; D4/D5/D12 `cc` answer 500; **D7
carried a longitude in a checkpoint's `status`**. An unparseable status is
`cwwp2.LevelUnknown` → `ChainControlData.Unrecognized`: the roads service
ignores it, and the `chain_control` layer drops it and degrades to `STALE` when
it's in-area — unless cc.kml reports something at that checkpoint (see above). The documentation page answers 403 — there is no contract.
Date/time strings are Pacific local; `*Epoch` fields are real Unix epochs.

`./bin/test-caltrans -feed=cwwp2 [-district=N]` probes it live (levels,
unrecognized statuses, freshness, sign messages grouped by text, closure
phases, servable and out-of-service cameras by county). Fixtures and the list
of still-unverified winter behavior:
`tests/testdata/cwwp2/README.md`.

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

  **Layer 8 also returns ZERO rows sometimes, with a 200 and no error envelope** —
  which is the same flip arriving through the one door the rule above does not
  cover, because nothing failed and nothing was malformed. Confirmed live on
  2026-09-15: all three in-window outages carried polygons at 21:08:00, none at
  21:12:32, all three again at 21:12:48 — a single request landing wrong, not a
  time window (a following 20-request sweep saw no blank at all). It looks like
  one bad backend behind their load balancer, but that is a hypothesis; the
  behaviour is the fact. In production it was frequent enough to cost a revision
  pair every few polls: one 7-customer planned outage had **25 revisions in an
  afternoon**, every one of them its area redrawn as its own centre point and
  back, with the Hwy 4 corridor place attaching and detaching each time.

  `GetOutages` reports it as `ErrPolygonLayerBlank` **and still returns the
  outages** — the point rows are good, only the footprints are missing. The
  poller treats it like the freshness gate: degrade the `pge` source, skip its
  sweep, keep the events, and carry each outage's last-known footprint rather
  than redraw it as a point.

  **It comes in BURSTS, so `./bin/test-pge` will usually look fine.** Measured:
  the stored revisions flipped geometry on roughly every other 5-minute poll from
  19:53 to 21:13, then went quiet — while 33 hand-sampled requests across that
  quiet tail caught exactly one blank. Do not read a clean run of the CLI as the
  upstream being fixed; the evidence that matters is an outage's revision history
  alternating Point and Polygon with nothing else changing.
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

## Burn status: two clients, two very different trust levels

`burnline` and `calfireburn` back one poller (`internal/ingest/burn_status.go`).
Both are unusual, in opposite directions, and neither should be treated like the
ArcGIS feeds above.

### `calfireburn` — an HTML scrape behind a bot wall

There is no API. As of 2026-09 no per-county burn-status layer exists on CAL
FIRE's ArcGIS org (368 services checked), on data.ca.gov, or in AGOL search. The
closest-named layer, `BP_Restrictions_Log_View`, is a **per-applicant** log
carrying names, emails and phone numbers — do not use it.

The host rejects any client that does not present as a browser NAVIGATION.
Measured against the live host:

| request shape | result |
|---|---|
| bare curl | 403 |
| descriptive bot UA (`SierraGrid/1.0`) | 403 |
| Chrome UA + `Accept` + `Accept-Language` | 403 |
| Chrome UA + `Accept` + `Sec-Fetch-*` | **200** |
| bot UA + `Accept` + `Sec-Fetch-*` | 403 |

So `browserHeaders` is load-bearing and an honest self-identifying User-Agent is
not an option that works. Worth knowing: the site's own `robots.txt` is
`User-agent: *` with **zero Disallow rules** plus an advertised sitemap, so its
declared crawl policy permits this while its edge configuration does not. We
poll twice a day.

**Expect it to break** — Sitecore markup that can change without notice (the
Caltrans KML feeds did exactly that in 2026), behind a fingerprinting WAF a third
party updates. That is why it is the **secondary** source: the burn line carries
the answer that changes often, and a 403 leaves the burn-day facet untouched.

Two parsing rules that must not be relaxed:

- **An empty table is an ERROR, never an empty map.** CAL FIRE lists every
  county, so zero rows means the markup moved. Returning an empty map would tell
  ingest that no county has a suspension — in fire season the most dangerous
  wrong answer this package could give.
- **Effective times are PACIFIC with no zone marker** ("Effective, June 15, 2026
  at 8:00 AM"). Parse with `time.ParseInLocation`, never `time.Parse`, or every
  suspension shifts 7-8 hours.

### `burnline` — an LLM reading of a phone recording

The authority is the county's recorded burn-information line (Calaveras:
(209) 754-6600). There is no API and no website behind it — the number IS the
source. `burnline.Reader` places a recorded call via `twilio`, transcribes it
with Whisper, and extracts today's status with a structured-output chat call.

**The server never runs this.** `cmd/burn-line` does, on a daily schedule from
`.github/workflows/burn-line.yml`, and PUSHES the results to
`POST /api/v1/ingest/burn.line` as ONE batched report.
It reads `grid.burn.lines` from `prefab.yaml` and dials every enabled line, so
the workflow enumerates no phone numbers — one source of truth, and no CI edit
that can drift from config in either direction.
Placing a phone call costs money and must happen exactly once a day; a
long-running server would re-dial on every restart unless carefully guarded,
whereas a scheduled workflow has those semantics for free.

Three behaviours are deliberate:

- **The caller hangs up.** The line is a looping recorded message that never ends
  the call, so an un-hung-up call bills until Twilio's own timeout. `HangUpAfter`
  (90s) covers one full pass of the message.
- **An unusable extraction is an ERROR, not a fallback.** The TypeScript pipeline
  this replaces (`dpup/burnday`) defaulted to `"red"` on any failure, reasoning
  that no-burn is the safe direction. It is — but it is still an assertion we
  never read. Here a failure pushes NOTHING, the previous reading ages past the
  server's freshness gate, and the facet reads UNKNOWN, which a consumer renders
  as "call the line". Equally safe, and honest.
- **`orange` is specifically an ELEVATION-RESTRICTED burn day** ("permissive burn
  days at 3500 feet elevation or more"), not a vague middle. It is the
  classification people get wrong, so the extraction prompt calls it out
  explicitly and `TestRead_ElevationRestrictedIsOrange` pins it.

The result carries a `Confidence` and the cleaned `Transcript` because it is a
model's reading of phone audio, and every event publishes the phone number
alongside it so a reader can always reach the actual authority.

### `twilio`

Hand-rolled rather than pulling in the Twilio SDK: the surface used here is three
form-encoded POSTs and a media download, and the SDK would add a large dependency
tree to a service whose other upstreams are plain HTTP. Operational failures
(unverified caller id, insufficient balance) come back as a 4xx with a
human-readable body, which the client carries into the error — without it a CI
log says only "status 400".
