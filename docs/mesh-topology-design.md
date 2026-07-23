# Mesh Topology & Relay History — Technical Design

Status: **Proposed** · Owner: The Grid (S.I.E.R.R.A) · Last updated: 2026-07-23

## 1. Summary

The MeshCore presence source (`NETWORK` layer, `internal/ingest/network.go`)
projects each mesh node into a `grid.v1` event. We recently added relay-path
capture — each advert carries the repeater chain it traversed — with the goal of
drawing the mesh as a **topology map**: nodes plus the links between them.

That surfaced a modeling problem. A relay path is not a property of a *node* — it
is a property of a single *reception*, and a node has thousands of receptions over
time. We were hanging one `path` on the node event as a scalar field, which (a)
kept only one sample, and (b) — because `network.telemetry` is deliberately
excluded from the content hash to avoid a revision per packet — that one sample
**froze at the node's last revision** and never updated. Post-deploy the map drew
~1 edge.

The fix is to store each kind of mesh data according to its true shape:

1. **Node identity / presence** stays an event (it has a real, if lazy,
   lifecycle: appear → rename/relocate/role-change → disappear). Slimmed to
   identity only.
2. **Receptions** (path, SNR, RSSI, gateway, hop count, our receive time) become
   an **append-only observation stream** — a firehose of immutable measurements,
   never revisioned.
3. **Topology** (weighted repeater↔repeater links) is **derived** by aggregating
   observations, exposed as a new `mesh_link` GeoJSON layer with recency and
   reliability annotations so an intermittent mesh reads honestly.

The freeze bug disappears by construction: there is no longer a "current value"
living inside a revisioned event.

## 2. Goals / Non-goals

**Goals**
- Persist the relay-reception stream so topology survives restarts (a deploy must
  never blank the map — the failure that motivated this).
- Draw the whole mesh as nodes + links, **recency-faded** rather than
  hard-cutoff, so a backbone repeater that adverts only every 12h still shows.
- Make **historical links first-class** — the network is shaky, and "this link
  was up 6 of the last 30 days" is exactly the signal operators want.
- Keep interesting history cheaply and indefinitely; prune the raw firehose
  aggressively without losing rare links or miscounting busy ones.
- **Cadence-aware presence**: expire dead chatty nodes and drive-through
  transients promptly, while protecting slow backbone repeaters — across a
  cadence spread of ~3 orders of magnitude.
- No new endpoints, no change to the event store's revision/ETag/hash contract.

**Non-goals**
- A general time-series/metrics store. This is a purpose-built two-tier rollup
  for one source; it is not Prometheus.
- Per-reception forensics beyond a short window. Individual receptions are
  disposable; the aggregate is what we keep.
- Authoritative link quality / routing. We report what we *observed*, weighted by
  how often and how recently — not a routing table.
- Re-resolving hops that never resolve. A repeater we only ever hear as a relay
  (never directly) stays an unnamed hop; that is honest.

## 3. The three kinds of data (and why one event was wrong)

| Data | Shape | Home |
|------|-------|------|
| Node identity / presence | Slow lifecycle, meaningful revisions (rename, relocate, role, appear/disappear) | **Event store** (`events`/`event_revisions`), unchanged mechanism |
| Receptions (path, SNR, RSSI, gateway, ts) | Immutable measurements, firehose, no lifecycle | **`mesh_observations`** — append-only (Tier 0) |
| Topology (weighted links) | Derived aggregate over receptions | **`mesh_link_rollup`** (Tier 1) + the live `mesh_link` layer |

The tell that receptions never belonged in the event: we had to *hash-exclude*
`network.telemetry` so it wouldn't mint a revision per packet. That exclusion is
the event store rejecting data that isn't event-shaped — and the freeze was the
exclusion biting back.

## 4. Data flow

```
MQTT advert  ──►  Registry (in-mem)
 (heterogeneous    ├─ node state (latest per pubkey) + per-node advert cadence (EWMA)
  cadence)         └─ observation buffer (every reception, per-(node,gateway) spam floor)
                          │
   scheduler tick ────────┤  Poll():
                          │   • Snapshot()          → presence Events (identity only),
                          │                            each node within k × its own cadence
                          │   • DrainObservations() → []MeshObservation (delta since last tick)
                          ▼
                   store (single writer, one tx / tick)
                   ├─ events / event_revisions        (presence — unchanged)
                   └─ mesh_observations (Tier 0, raw)  ── batch insert
                          │
   maintenance tick ──────┤  CompactMeshObservations():   (hourly, same writer goroutine)
                          │   explode path_nodes → edges, group by (a,b,day),
                          │   upsert rollup, advance watermark
                          ▼
                   mesh_link_rollup (Tier 1)
                   + prune Tier 0 past retention (and ≤ watermark)
                   + prune Tier 1 past retention

reads:  mesh_node.geojson   ← events                    (unchanged)
        mesh_link.geojson   ← rollup (history) ∪ Tier 0 tail (freshness), windowed
```

Receptions arrive on MQTT callbacks (many at once) but are **batched one tx per
tick** via the drain pattern the node snapshot already uses — never a write per
packet. All writes remain on the single scheduler-writer goroutine; the
compaction/prune maintenance tick runs on that same goroutine, so single-writer
discipline (`internal/store/CLAUDE.md`) is untouched.

## 5. Schema (new migration, appended to the ladder)

```sql
-- Tier 0: raw receptions. High resolution, short life. One row per advert we heard.
CREATE TABLE mesh_observations (
  id         INTEGER PRIMARY KEY,       -- rowid, monotonic
  pubkey     TEXT NOT NULL,             -- advertising node
  heard_at   INTEGER NOT NULL,          -- OUR receive time (unix sec) — the trustworthy clock
  broker     TEXT NOT NULL DEFAULT '',  -- MQTT server heard on
  gateway    TEXT NOT NULL DEFAULT '',  -- origin_id / gateway that reported it
  snr        REAL,
  rssi       INTEGER,
  hop_count  INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL DEFAULT '',  -- comma-joined hop prefix-hashes (hex), as received
  path_nodes TEXT NOT NULL DEFAULT ''   -- resolved pubkeys ('' where a hop was unresolved)
);
CREATE INDEX idx_mesh_obs_heard  ON mesh_observations(heard_at);          -- live-window read + prune
CREATE INDEX idx_mesh_obs_pubkey ON mesh_observations(pubkey, heard_at);

-- Tier 1: link rollups. Compact, long-lived. One row per (undirected link, day).
CREATE TABLE mesh_link_rollup (
  a_pubkey     TEXT NOT NULL,           -- canonical a < b
  b_pubkey     TEXT NOT NULL,
  bucket       INTEGER NOT NULL,        -- heard_at truncated to UTC day
  observations INTEGER NOT NULL DEFAULT 0,
  best_snr     REAL,                    -- peak SNR on the link that day
  first_seen   INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL,
  PRIMARY KEY (a_pubkey, b_pubkey, bucket)
);
CREATE INDEX idx_mesh_link_bucket ON mesh_link_rollup(bucket);

-- Tiny KV for the compaction watermark (last heard_at folded into the rollup).
CREATE TABLE mesh_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
```

Migration rules per `internal/store/CLAUDE.md`: append a new `migrations[]` entry
(never edit a shipped one), update `schema.sql`'s trailing comment to note the new
tables. These tables are **not** proto-blob-canonical — they are pure derived
telemetry, not part of the system-of-record event model, so plain columns are the
right call (and a lost DB re-accumulates them from the live feed, unlike event
history).

Edges are exploded in **Go** at compaction, not via SQL string-splitting: read
observations `> watermark`, walk each `[pubkey, ...path_nodes]` chain into
canonical `a<b` pairs (breaking on empty/unresolved hops), accumulate
`map[edge]agg`, upsert with `ON CONFLICT(a,b,bucket)` doing
`observations += n, best_snr = max(...), last_seen = max(...)`. Idempotent via the
watermark + upsert.

## 6. Retention & compaction — what counts as "interesting"

| Tier | Grain | Kept | Powers | Prune rule |
|------|-------|------|--------|------------|
| 0 `mesh_observations` | every reception | **48h** | live-map freshness, re-resolution, forensics | `heard_at < now−48h` **and** `≤ watermark` |
| 1 `mesh_link_rollup` | per link, per day | **2 years** (sparse → tiny) | history, "as of date", reliability | `bucket < now−2y` |
| — presence | event revisions | forever | node lifecycle | unchanged |

Principles that matter more than the numbers:

- **Chattiness is neutralized at the rollup.** One row per link per day whether it
  adverted 5× or 5,000×; the difference is the `observations` count. A flood node
  cannot bloat history, and a rare node is not underweighted out of existence.
- **Never threshold-drop rare links.** A link seen *once* is often the most
  interesting datum on the map — a long-haul shot that proves coverage.
  Compaction keeps every edge, weighted by count; it never discards low-count
  links as noise. (The opposite of typical downsampling, deliberately.)
- **Keep the extremes, not just the mean.** `best_snr` (peak) and `first_seen`
  survive compaction — "this link *can* do −4 dB" and "it first appeared on the
  3rd" are facts you cannot reconstruct from an average later.
- **Prune raw only after it is folded in.** Tier 0 delete is gated on the
  watermark, so a compaction hiccup delays pruning rather than dropping
  un-rolled-up receptions.
- **Raw retention ≥ live window, with margin, and it re-resolves.** The map reads
  Tier 0's recent tail for sub-day freshness; 48h covers a delayed compaction or a
  long deploy, and lets an ambiguous hop **re-resolve** as we learn new nodes (the
  raw `path` is retained). Re-resolution barely needs longer — an unresolved hop
  is a repeater, and repeaters advert within a cycle.

### Chatty-node handling (raw tier)

Even with uncontrolled nodes adverting hourly and frequent companions, this is a
regional mesh (hundreds of nodes); 48h of raw is low-hundreds-of-thousands of
rows — trivial for SQLite, and rollup counts come straight from it (no separate
tally to drift). The only real threat is a **pathological** node (misconfig or
spam adverting every second). Guard it with a **per-(node, gateway) persist
floor**: drop a second raw row from the same node *and* same gateway within ~30s.
That caps a spammer in both storage *and* rollup weight (weight derives from raw),
while preserving multi-gateway diversity — different gateways hearing one advert
each still land a row, because resilience ("5 gateways still hear this link") is
genuine signal we want to count.

## 7. The `mesh_link` layer — recency + reliability, not a cutoff

A hard "live window" strobes a slow mesh: a 12h repeater blinks out between
adverts. Instead the map is **recency-faded** and offers a **window selector**.

| View | Source | Shows |
|------|--------|-------|
| **Live (72h default)** | Tier 0 raw | current backbone, faded by recency |
| **7d / 30d** | rollup + today's raw tail | intermittent links — the shaky picture |
| **All-time** | rollup | every link ever observed |

The default window is 72h (~6 cycles of a 12h repeater, robust to a couple of
missed adverts); edges fade by `lastSeen` age so a link heard 2h ago is bright and
one heard 60h ago is dim-but-present. The window bounds what is *drawn*; the fade
tells you what is *fresh*. A quiet-but-recent link is context, never an all-clear
that it is gone — the same fail-loud posture as the rest of the Grid.

`mesh_link.geojson` is a hand-built layer (like the condition layers) — one RFC
7946 `FeatureCollection` of `LineString` edges, `[lng,lat]` coordinates, camelCase
`properties`:

```
a, b          endpoint pubkeys
observations  total receptions on the link in the window
daysActive    distinct days seen in the window  →  "up 6 of the last 30 days"
firstSeen     when the link first appeared        (RFC 3339)
lastSeen      most recent — drives the fade       (RFC 3339)
bestSnr       peak SNR ever on the link
```

`daysActive` earns its keep: a backbone link reads `28/30`, a lucky long-haul
shot reads `1/30`, and the difference is visible. The read path merges tiers —
rollup supplies multi-day history, Tier 0 supplies sub-day freshness for the tail
newer than the watermark: `lastSeen = max(rollup, raw)`, `firstSeen = min`,
`observations = sum`, `daysActive = distinct days`, computed in Go.

The `/mesh` page drops its client-side edge reconstruction and just renders the
two layers (`mesh_node` + `mesh_link`), with a window control that re-queries.

## 8. Presence event — slimmed to identity

`NetworkTelemetry` sheds `path` and `path_nodes` (reserve the field numbers;
deprecate rather than renumber). The event keeps **identity only** — pubkey, role,
name, location. `snr`/`rssi`/`gateways` also drop from the event so there is
exactly one home for signal and **zero frozen fields**; a node popup that wants
"last heard at SNR X" reads the latest observation. `mesh_node.geojson` keeps
projecting nodes from events (unchanged).

This means what mints a presence revision is exactly a node's stable identity,
role, name, location, or status — never signal. Same rule as today, minus the two
path fields that never belonged.

## 9. Cadence-aware presence (the "chatty vs fixed expiration" fix)

A single fixed `expireAfter` cannot serve a cadence spread of ~3 orders of
magnitude: long enough for a 12h repeater ⇒ a dead hourly node squats for days;
short enough to reap the hourly node ⇒ backbone repeaters expire between adverts.
The fix puts the adaptivity **in the Registry, where the cadence data already
lives** — not in the disappearance sweep (which would need a per-event TTL, a
non-hashed grace field, and proto churn):

- `nodeEntry` tracks a rolling **inter-advert interval** (EWMA of `heardAt`
  deltas) — a couple of cheap in-memory fields.
- `Snapshot` stops using one global 30-min `activeWindow` and returns each node
  **within `k × its own cadence`**, clamped to `[floor, ceil]`.
- The disappearance-sweep grace collapses to a **short uniform safety net** — it
  no longer does cadence reasoning, just backstops.

Presence becomes self-tuning: the 12h repeater at hour 6 is well inside `k×12h` →
returned → stays ACTIVE; a dead hourly node at hour 3 is past `k×1h` → dropped →
reaped promptly. **No proto, hash, or sweep-model change** — a self-contained
Registry refinement, testable in isolation.

Two edges handled explicitly:

- **One-shot transients** (wide-geofence drive-throughs): a node with a single
  advert has no interval yet → **moderate default window**, so a one-shot
  evaporates in a few hours instead of squatting for days. The
  "prune chatty/transient nodes" behavior falls out of the same rule.
- **Brand-new slow node, first gap:** heard once, dropped after the default
  window, EXPIRED; re-adverts at 12h → reappears (one resolve→reappear revision).
  It flaps exactly once, on its first gap, before we have learned it is slow;
  after the second advert its window widens to its true cadence and it stops
  flapping. Acceptable, and honest — we genuinely did not know it was there for
  those hours.

The fail-loud invariant is preserved: all-brokers-down still hard-errors `Poll`
(no snapshot, no sweep), so our outage never expires live nodes. The adaptive
window only narrows *which recently-heard nodes* count as present; it never runs
when the feed is down.

## 10. Config (`prefab.yaml` → `grid.meshcore`)

```yaml
grid:
  meshcore:
    observationRetention: 48h    # Tier 0 raw
    rollupBucket: 24h            # Tier 1 grain
    rollupRetention: 17520h      # 2 years
    compactionInterval: 1h
    liveWindow: 72h             # default mesh_link recency window
    spamFloor: 30s              # min gap between persisted raw rows per (node, gateway)
    presence:
      cadenceK: 3               # keep a node present within k × its measured interval
      graceFloor: 3h            # min presence window (one-shot / unknown cadence)
      graceCeil: 72h            # max presence window (slowest backbone)
    sweepGrace: 1h              # short uniform safety net on the disappearance sweep
```

## 11. Integration seams

- **`meshcore.Registry`**: observation ring buffer (appended in `ingestPacket`
  under `mu`, per-(node,gateway) spam floor, capped) + `DrainObservations()`;
  per-node inter-advert EWMA; `Snapshot` takes per-node cadence windows instead of
  a single `activeWindow`.
- **`ingest`**: `PollResult` gains an optional `MeshObservations []MeshObservation`
  (nil for every other poller); the scheduler flushes it in the same writer tx as
  the presence upserts, keeping "normalizers don't write" intact.
- **`store`**: the new migration; `InsertMeshObservations(batch)`;
  `CompactMeshObservations()` (explode → upsert rollup → advance watermark →
  prune); `MeshLinks(window)` read for the layer builder. A new **maintenance
  tick** on the scheduler goroutine drives compaction/prune.
- **`gridapi`/`hazards`**: register the hand-built `mesh_link` layer
  (`RegisterGatewayRoutes` / `eventLayers`), reading `MeshLinks`. Drop `path`/
  `path_nodes` from `projectNetwork`.
- **`web`**: `/mesh` renders `mesh_node` + `mesh_link` layers with a window
  selector; remove the client-side edge reconstruction.
- **Docs**: `site/docs.html` (`/api/v1` reference) + a `CHANGELOG.md` entry
  (breaking: `network.telemetry.path`/`pathNodes` removed from events; additive:
  `mesh_link` layer).

## 12. Rollout

1. Migration + `mesh_observations` write path (drain + batch insert). Observations
   start accumulating; nothing reads them yet.
2. Compaction maintenance tick + `mesh_link_rollup`. History begins building.
3. `mesh_link` layer + `/mesh` two-layer render with the window selector.
4. Slim the presence event (drop path/telemetry-signal fields); update docs +
   CHANGELOG.
5. Cadence-aware presence in the Registry (EWMA + per-node window); drop the
   sweep grace to the short safety net.

Steps 1–3 deliver the durable, restart-proof topology map (the motivating fix).
Step 5 is the expiration correctness win and is self-contained; it can land in the
same series or immediately after.

## 13. Open questions

- **Count receptions vs. unique adverts.** Current call: count receptions —
  multi-gateway reception is resilience signal, and `daysActive` already separates
  reliable links from busy-single-day ones. Revisit if weighting looks off against
  live data.
- **`cadenceK` / clamp tuning.** Start `k=3`, `[3h, 72h]`; tune once we can eyeball
  real per-node cadence from the rollup. The rollup is precisely the dataset that
  tells us whether these are right.
- **Directed vs. undirected links.** v1 is undirected (canonical `a<b`). MeshCore
  paths have a direction (origin → us); if asymmetry turns out interesting we can
  keep both orientations later without a schema change to Tier 0.
