# Fire-Weather Forecast — Technical Design

Status: **Implemented** · Owner: The Grid (S.I.E.R.R.A) · Last updated: 2026-07-27

## 1. Summary

The Grid surfaces **current** weather per location (OpenWeatherMap) and the
region's **issued** fire-weather state (NWS Red Flag / Fire Weather Watch →
`fire_weather`). It has no *forecast*. For fire monitoring the leading signals are
**wind (speed + gusts)** and **relative humidity** over the next day or two — the
lead-up before a warning is issued.

This adds a short-range **fire-weather forecast** from the **NWS gridpoint** API
(`api.weather.gov`, the same keyless service we already use for alerts). It is
**geolocated per configured weather location** (Arnold ≠ Sonora — different
elevation bands, different NWS gridpoints), exposed two ways from **one** fetch:

- `GET /api/v1/conditions` gains `forecast[]` — the per-location **hourly** series
  (48h) of wind/gust/RH/temp plus an at-a-glance summary.
- The existing **`fire_weather` map layer** becomes a mixed FeatureCollection: the
  region **banner** (issued state, unchanged) **plus one Point per location**
  carrying that location's forecast summary — geolocated, no new layer.

**Guardrail:** the forecast is *informational*. It never derives a fire-weather
warning NWS hasn't issued and never changes the `fire_weather` layer's severity
(the CLAUDE.md rule). The issued state stays the only thing that colors the map.

## 2. Goals / Non-goals

**Goals**
- Per-location short-range wind/gust/RH forecast, authoritative + keyless (NWS).
- Reuse the existing `nws.Client`, the weather-service cache pattern, and the
  `fire_weather` layer — additive, no new endpoint, no new map layer, no new key.
- Fail-soft: a forecast fetch failure never blocks `conditions`; freshness is
  marked honestly; a stale/absent forecast is never fabricated.
- Geolocated on the map at the locations we actually monitor.

**Non-goals**
- A continuous/interpolated wind field. NWS gives a point forecast per ~2.5 km
  grid cell; we forecast **at configured locations**, not a gridded surface
  (HRRR/RTMA is out of scope). Denser coverage = more `weather.locations`.
- A derived/forecast Red Flag. Only NWS-issued products set fire-weather state.
- Replacing current conditions (OpenWeatherMap stays the current-obs source).

## 3. Source: NWS gridpoint forecast

Verified live for our region (Ebbetts Pass → NWS `STO 90,41`):
`/points/{lat},{lng}` → `forecastGridData` carries hourly, ~7-day series for
`temperature`, `relativeHumidity`, `dewpoint`, `windSpeed`, `windDirection`,
**`windGust`** — the full fire-weather set, in clean numeric units
(`km_h-1`, `percent`, `degC`, `degree`). We take **48h**.

Why `forecastGridData` and not `forecast/hourly`: only the grid carries
`windGust` and clean numeric `relativeHumidity`; the hourly product gives wind as
a string (`"6 mph"`) and omits gust.

**Parsing nuance (the one real complexity).** The grid run-length-encodes each
variable over ISO-8601 spans with *different* span counts per variable
(`{"validTime":"2026-07-27T12:00:00+00:00/PT5H","value":X}` = value X for the
next 5h; windSpeed had 47 spans, RH 150). Expand each variable's spans into an
hourly `map[time]value`, then merge into aligned `ForecastPeriod`s over the
horizon. A small ISO-8601-duration expander, unit-tested against a real sample.

## 4. Data model

### 4.1 Proto (`api/grid/v1/grid.proto`) — the `conditions` surface

```proto
message ForecastPeriod {
  google.protobuf.Timestamp time = 1;      // hourly period start, UTC
  int32 temperature_celsius = 2;
  int32 humidity_percent = 3;              // relative humidity
  int32 wind_speed_kmh = 4;
  int32 wind_direction_degrees = 5;
  int32 wind_gust_kmh = 6;
}
message WeatherForecast {
  string location_id = 1;                  // joins to WeatherConditions.location_id
  string source = 2;                       // "NWS (STO gridpoint)"
  google.protobuf.Timestamp issued_at = 3; // grid update time
  int32 horizon_hours = 4;
  repeated ForecastPeriod periods = 5;
  int32 peak_wind_gust_kmh = 6;            // summary: max over the horizon
  google.protobuf.Timestamp peak_wind_gust_at = 7;
  int32 min_humidity_percent = 8;          // summary: min over the horizon
}
message Conditions {                        // one additive field
  repeated WeatherConditions weather = 1;
  FireWeatherConditions fire_weather = 2;
  google.protobuf.Timestamp last_updated = 3;
  repeated WeatherForecast forecast = 4;    // NEW — per location
}
```

(Absent numeric fields serialize as 0, matching the existing `WeatherConditions`
scalars; a period is only emitted when the core wind/RH values are present.)

### 4.2 GeoJSON (`internal/hazards/properties.go`) — the map layer

`FireWeatherProps` gains an optional forecast summary; the `fire_weather` builder
emits it on **per-location Point features** (not the banner):

```go
type ForecastSummary struct {
	Source             string `json:"source,omitempty"`
	IssuedAt           string `json:"issuedAt,omitempty"`
	HorizonHours       int    `json:"horizonHours,omitempty"`
	PeakWindGustKmh    int32  `json:"peakWindGustKmh,omitempty"`
	PeakWindGustAt     string `json:"peakWindGustAt,omitempty"`
	MinHumidityPercent int32  `json:"minHumidityPercent,omitempty"`
}
// FireWeatherProps.Forecast *ForecastSummary `json:"forecast,omitempty"`
```

## 5. Client (`internal/clients/nws/forecast.go`)

Same pattern as `GetActiveZoneAlerts` (User-Agent, `HTTPDoer`, decode):

- `ResolveForecastURL(ctx, lat, lng) (string, error)` — `/points/{lat},{lng}` →
  `properties.forecastGridData`. The mapping is **static per location**; the
  service caches it ~forever (re-resolve only on a 404).
- `GetGridForecast(ctx, gridURL, horizon) (*Forecast, error)` — fetch the grid,
  expand the RLE spans, merge into hourly `ForecastPoint`s within `horizon`,
  carry `updateTime` as `issued_at`.

```go
type ForecastPoint struct {
	Time                                          time.Time
	TempC, HumidityPct, WindKmh, WindDirDeg, WindGustKmh float64
}
type Forecast struct { Source string; IssuedAt time.Time; Points []ForecastPoint }
```

## 6. Service + caching (`internal/services/weather*.go`)

**Read/refresh split so the request path never fetches NWS:**

- `RefreshForecasts(ctx)` — the **background** warmer (`periodic_refresh.go`):
  fetches each location, caches `nws:forecast:{locID}` (TTL
  `weather.forecast.refreshInterval`, default **1h**). Skips still-fresh entries,
  so it runs on the forecast's hourly cadence even though the refresher ticks on
  the roads interval. On a **404** (NWS re-tiled the grid → the cached
  `nws:gridurl:{locID}` is dead) it `Delete`s the URL and re-resolves once. Never
  caches an empty result.
- `LocationForecasts(ctx)` — the **request-path READ** (`GetConditions` + the
  `fire_weather` builder): reads the warmed cache only (fresh, or last-good within
  the 2× very-stale bound), **no fetch**. Additive + fail-soft: a location with no
  warm entry is omitted; a forecast outage never blocks/slows `conditions`.
- `nws:gridurl:{locID}` — the static `/points`→grid mapping, cached ~forever
  (`gridURLTTL` 30d), invalidated on a 404.
- Rate: ~7 locations × 1/h = **~168 keyless calls/day**, all off the request path.

## 7. Projection

- **`gridapi` `GetConditions`** attaches `forecast[]` by `location_id`, respecting
  the existing `?place=` bbox filter (forecast follows the same location set).
- **`hazards` `fireWeather` builder** returns a **mixed FeatureCollection**: the
  existing null-geometry **banner** (issued state) **+ one Point per location** in
  the area, each `severity: INFO`, `kind: fire_weather_forecast`, with the
  `fireWeather.forecast` summary. `/map` renders the points as neutral dots
  (INFO), forecast numbers in the popup — never on the Red-Flag ramp.

## 8. Config (`prefab.yaml` + struct) — no new secret

```yaml
weather:
  forecast:
    enabled: true
    refreshInterval: "1h"
    horizonHours: 48
```

Reuses `weather.nws.userAgent` and each `WeatherLocation.Coordinates`. No changes
to `weather.locations`.

## 9. Honesty / guardrails

- **Informational only.** Forecast never sets/escalates `fire_weather` severity;
  the issued NWS product is the sole color. Forecast points are `INFO`.
- **Freshness marked.** `source` + `issuedAt` on every forecast; a stale served
  set carries its real age; a failed fetch omits the block rather than faking one.
- **Granularity stated.** Point forecasts at configured locations, not a field.

## 10. Testing

- `nws`: the ISO-8601 span expander; `GetGridForecast` parses a real STO grid
  fixture into aligned hourly points with gust + RH; `/points` resolution.
- `services`: forecast cache-first, serve-stale on error, omit on cold miss
  (never blocks conditions).
- `gridapi`: `conditions.forecast` present + joined by location; `?place=` filter.
- `hazards`: `fire_weather` emits banner + per-location Points; severity stays the
  issued state (a windy forecast does not change color).

## 11. Sequencing

1. **Proto**: `ForecastPeriod` / `WeatherForecast` / `Conditions.forecast`;
   `make proto`. (This doc's step 1.)
2. **Client**: `nws.ResolveForecastURL` + `GetGridForecast` + the span expander.
3. **Service**: per-location forecast cache + fail-soft.
4. **Projection**: `gridapi` conditions; `hazards` `fire_weather` mixed features +
   `ForecastSummary`.
5. **Config + prefab**; **CHANGELOG** (additive `conditions.forecast` +
   `fireWeather.forecast` on the layer) + `docs`; tests throughout.

## 12. Fast-follow: NASA FIRMS (early detection)

Once the forecast lands, the next source is **NASA FIRMS** active-fire satellite
detections (VIIRS/MODIS, near-real-time; free `MAP_KEY`, Area API by bbox → CSV) —
inherently geolocated points that catch ignitions before CAL FIRE/WFIGS list them.
Present as *"satellite thermal detection,"* not *"confirmed fire"* (false
positives: industrial heat, sun glint). Scoped separately.
