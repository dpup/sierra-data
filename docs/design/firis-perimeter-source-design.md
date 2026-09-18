# FIRIS / CAL FIRE Perimeter Source — Technical Design

Status: **Implemented** · Owner: The Grid (S.I.E.R.R.A) · Last updated: 2026-07-27

## 1. Summary

The `wildfire` layer adopts a WFIGS perimeter onto a CAL FIRE incident by name.
WFIGS (the NIFC interagency upload) **lags real fires by hours** — the Dove Fire
had a mapped perimeter on Watch Duty and in CAL FIRE's own feed while WFIGS still
returned zero (verified 2026-07-27). WFIGS also lives on NIFC's chronically
429-saturated ArcGIS org.

This swaps the perimeter source to **CA Perimeters — CAL FIRE / NIFC / FIRIS**
(`CA_Perimeters_NIFC_FIRIS_public_view`, the layer CAL FIRE's own public incident
map uses). It **combines CAL FIRE Intel remote-sensing + FIRIS IR flights + WFIGS**
into one layer, updated every ~5 min, on the **CAL FIRE-Forestry** org (`services1
.arcgis.com/jUJYIo9tSA7EHvfZ`) — a *different* quota than NIFC's, with metadata
`Cache-Control: public, max-age=3600`. It is a **superset of WFIGS**, faster, and
on a healthier quota. Verified: it carries the Dove perimeter (CAL FIRE Intel,
225 ac, current) that WFIGS lacks.

**The one genuinely new problem is dedup** (§4): the combo feed has *multiple rows
per fire* (successive IR flights + a FIRIS mission), so we must collapse them to
one perimeter per fire before the existing name-match adoption runs — without
merging two genuinely-distinct same-named fires.

## 2. The source

- **Endpoint:** `https://services1.arcgis.com/jUJYIo9tSA7EHvfZ/arcgis/rest/services/CA_Perimeters_NIFC_FIRIS_public_view/FeatureServer/0` — layer "FIRIS WFIGS ComboLayer". Keyless, public (CalOES-funded FIRIS; CAL FIRE-hosted "public view").
- **Fields we use:** `incident_name`, `mission`, `incident_number`, `area_acres`,
  `poly_DateCurrent` (perimeter currency, epoch ms), `source`
  (`CAL FIRE INTEL FLIGHT DATA` | `FIRIS` | WFIGS), `type` (e.g. `Heat Perimeter`),
  `displayStatus` (`Active` | `Inactive`).
- **Freshness / quota:** ~5 min updates; `editingInfo.dataLastEditDate` present and
  metadata CDN-cached 1h (better than WFIGS's 5m) → the `dataLastEditDate` gating
  we already built applies. Different org than NIFC → not the 429-saturated pool.
- **Coverage:** California only — exactly our service area. **Licensing:** public
  emergency-awareness data; confirm terms of use before prod, but it is the feed
  CAL FIRE publishes for public consumption.

## 3. Why replace WFIGS (not run both)

The combo layer *includes* WFIGS, so running both double-counts. Replacing is
simplest and strictly more coverage. WFIGS-as-fallback (for a CAL FIRE-org outage)
is deferred — the existing fail-loud carry-forward already covers a source outage,
and a second perimeter feed would need its own cross-feed dedup. The `wfigs`
client + gating stay in the tree (unused) so re-adding a fallback is cheap.

## 4. Dedup — the careful part

**Goal:** turn the combo feed's many-rows-per-fire into **one perimeter per fire**,
keyed by normalized name (what adoption matches on), *without* merging two distinct
same-named fires.

**Why neither id field alone works** (measured on the live feed): `incident_number`
is a stable per-fire uuid *only on CAL FIRE Intel rows* — it is null on FIRIS
mission rows (149/254 null). `incident_name` is null on 110/254 (FIRIS mission
rows). So the only thing that links a fire's CAL FIRE Intel rows to its FIRIS rows
is the **name** — `incident_name` when present, else parsed from `mission`.

### 4.1 Derive a name for every row

- `incident_name` if non-empty; else parse from `mission`
  (`CA-<UNIT>-<NAME>-<FLIGHTID>`): drop `CA` + the unit token, drop a trailing
  flight-id token (`^N\w` — `N57B`/`N50X`/`N42Z`), the remainder is the name
  (`CA-TCU-DOVE-N57B` → `DOVE`, `CA-FKU-PARAMOUNT` → `PARAMOUNT`). Normalize with
  the existing `NormFireName`. A row with neither a name nor a parseable mission is
  **dropped** (can't attribute or give a stable id — rare).

### 4.2 Group by name, then split into per-fire clusters

Group rows by normalized name. Within a name group, cluster into fires with
`sameFire`:

- **`incident_number` is authoritative when present.** It is a stable per-fire uuid
  on CAL FIRE Intel rows (null on FIRIS mission rows). A **shared** non-empty id is
  one fire (merge even if successive flights' centroids drifted apart); two
  **different** non-empty ids are distinct fires (never merge, even co-located —
  this is what stops two same-named fires ~10 km apart from collapsing and dropping
  a real perimeter).
- **Centroid proximity is the fallback** when at least one id is absent
  (`centroidDistSq < ~(0.15°)²`, ~15 km): successive flights of one fire share a
  location → one cluster.

The group is **sorted deterministically** (id-bearing rows first, then by id, then
centroid) before the greedy single-pass clustering, so the cluster count can never
depend on the upstream feature order (ArcGIS does not guarantee it stable) — an
order-dependent count would flap the standalone `-2`/`-3` suffixes and mint phantom
duplicate events. The query also sets `orderByFields=OBJECTID` as defense in depth.

### 4.3 Pick one perimeter per cluster

Keep the row with the **latest `poly_DateCurrent`** (the most current footprint).
Tiebreak on equal timestamps: `displayStatus == Active` first, then a source
priority (`CAL FIRE INTEL` ≈ `FIRIS` current > WFIGS to-date), then larger
`area_acres`, then `OBJECTID` for determinism.

**Filter:** query `displayStatus = 'Active'` server-side — drops stale/contained
perimeters (e.g. the months-old Inactive "TWIST") so they never become stale
standalones. A fire whose only perimeter is Inactive is intentionally skipped
(it's contained/old). Adjustable if that proves too aggressive. The dedup **also**
drops an explicit non-Active row client-side (defense in depth: if the server
`where` clause ever breaks, we still never ingest hundreds of Inactive statewide
perimeters); a blank status is kept.

### 4.4 Worked example — the Dove Fire

Rows in-bbox: `DOVE` CAL FIRE Intel 225 ac @ 19:54Z, `DOVE` CAL FIRE Intel 223 ac
@ 02:23Z (same `incident_number`), FIRIS `mission=CA-TCU-DOVE-N57B` 166 ac @ prev
day (name null). → all normalize to `dove`, all co-located → **one cluster** →
latest wins = **CAL FIRE Intel 225 ac @ 19:54Z**. One `dove` perimeter → the
existing adoption attaches it to the `calfire:` Dove incident. ✓

### 4.5 Feeds the existing adoption unchanged

After dedup there is **≤1 perimeter per (name, cluster)**. A name with one cluster
→ one perimeter → the incident adopts it cleanly. A name with *two* clusters (two
real fires) → two same-name perimeters → the existing `ambiguous`/standalone
handling (centroid-ordered `-2`/`-3` suffixes) applies exactly as today. So dedup
only *collapses co-located duplicates*; genuine same-name-distinct-fire ambiguity
is untouched.

## 5. Client + normalizer changes

- **`internal/clients/firis`** — new client (adapted from `wfigs`): fetch the combo
  `FeatureServer/0` (`where=displayStatus='Active'`, our bbox, the fields in §2,
  `returnGeometry=true`, `maxAllowableOffset` simplification) → `[]Perimeter`
  (`Name, Mission, IncidentNumber, Acres, DateCurrent, Source, Status,
  GeometryType, GeometryCoords`). Reuse the **`LastEdit()` gating** (its metadata is
  1h-cacheable) + the 429/backoff hardening from the wfigs work.
- **`internal/ingest/wildfire.go`** — `gatedPerimeters` fetches from `firis`; a new
  **`dedupePerimeters`** step (§4) runs before the `byName` index; provenance/source
  become `firis`. The adoption + standalone code is unchanged.
- **Source rename** `wfigs` → `firis` across `grid.sources`, the source registry
  (`gridSourceInfo`), provenance constants, `SourceIDs()`, the `firis:` standalone
  id namespace, and `gridapi.layerSourceIDs[wildfire]` (`calfire`+`firis`).
- Containment/cause on a *standalone* firis perimeter are absent (the combo feed
  has neither); an *adopted* incident keeps CAL FIRE's containment/cause (as today).

## 6. Fail-loud / honesty

Unchanged posture: a failed fetch never resolves/expires (carry-forward via the
`expire` policy); the source's health is `firis` in `/api/v1/sources`; the gating
serves last-good on a stamp-unchanged tick. `expire` grace stays (perimeter uploads
still lag ignition, just less).

**Empty-response handling.** An empty combo-feed response (HTTP 200, zero features)
is a common transient ArcGIS glitch, not a genuine all-clear. It is treated as
non-authoritative: it is **never cached** as last-good (so it can't be replayed for
`maxPerimCacheAge`), and a **wholesale-empty** deduped set carries adopted
perimeters forward (like a hard outage) instead of downgrading every fire to a
point + writing a false "perimeter gone" revision. A **non-empty** feed that simply
omits one fire *is* authoritative — that fire genuinely downgrades. Standalones
need no equivalent guard: their `expire` grace already absorbs a transient empty.

## 6a. Retiring the old `wfigs:` events (deploy migration)

The disappearance sweep only runs for a live poller's `SourceIDs()`, so ACTIVE
standalone `wfigs:` events already in the store would otherwise persist forever
after the rename (stale duplicates beside the fresh `firis:` events).
`retireOrphanedSources` (in `cmd/server/main.go`) transitions any active
`source_id='wfigs'` events to `EXPIRED` once at boot (a recorded revision) and
removes the defunct `wfigs` source registry row (`Store.DeleteSource`) so
`/api/v1/sources` doesn't list it forever (`SeedSources` is upsert-only). Both are
idempotent — empty/no-op after they drain. The `retiredSourceIDs` entry can be
removed a deploy cycle later.

## 7. Config

`prefab.yaml`: rename `grid.sources.wfigs` → `grid.sources.firis` (same poll
interval / disappearance policy), point the client at the combo endpoint. No new
secret (keyless).

## 8. Testing

- `firis` client: parse the combo response (fields, geometry); `LastEdit` gating.
- **Dedup (the core):** co-located same-name rows collapse to the latest
  `poly_DateCurrent`; a FIRIS mission row (name null) groups via mission-parse and
  loses to a newer CAL FIRE Intel row; two distinct same-name fires (far apart)
  stay **separate**; `displayStatus` filter drops Inactive; a nameless+missionless
  row is dropped.
- Wildfire normalizer: an incident adopts the deduped perimeter; the Dove-shaped
  fixture yields one adopted perimeter, not an ambiguous non-adoption.

## 9. Rollout

1. `firis` client + `LastEdit` gating (fixture from a real combo response).
2. `dedupePerimeters` + tests (§8) — land this with the client, it's the risk.
3. Swap the wildfire normalizer's perimeter source + the `wfigs`→`firis` rename.
4. Config/prefab; main.go wiring (client, source registry, poller); CHANGELOG.
