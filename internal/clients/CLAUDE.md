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
- Zone codes for the service area (verify with `api.weather.gov/points/{lat},{lng}`,
  don't guess): CAZ137 (1000–3000 ft), CAZ138 (3000–5000 ft), CAZ139 (above
  5000 ft) — NWS Sacramento (STO), covering Calaveras & Tuolumne.
