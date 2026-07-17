# MeshCore mesh-node presence — integration status & next steps

_Last updated: 2026-07-17. Status: **implemented, disabled, blocked on broker access.**_

MeshCore LoRa mesh-node presence is ingested as `NETWORK`-layer events (one event
per node, keyed by Ed25519 public key) from community MQTT bridges. All code is
merged and tested; the source is **disabled by default** and stays that way until
we have a broker **subscriber credential** (see Blocked, below).

## What's built

- **Proto** (`api/grid/v1/grid.proto`): `NetworkDetail` (tag 26) + `NetworkTelemetry`.
  `Layer_NETWORK` (=13) already existed.
- **Store** (`internal/store/events.go`): `ContentHash` zeroes `network.telemetry`
  so the advert firehose refreshes liveness (`last_seen_at`) without minting a
  revision. Only identity/role/name/location/status change writes history.
- **Client** (`internal/clients/meshcore/`): `advert.go` (pure Ed25519 advert
  payload decoder, std-lib crypto) + `frame.go` (`DecodeFrame` — strips the
  MeshCore over-the-air transport framing that the bridge publishes in `raw`:
  header + optional transport codes + path, where `path_len` packs
  `((hash_size-1)<<6)|hash_count`) + `client.go` (`Registry` — dials N brokers via
  paho, parses the JSON envelope, filters `packet_type==4`, `DecodeFrame`s the
  `raw`, buffers node state keyed by pubkey, `Snapshot`/`Health`).
- **Normalizer** (`internal/ingest/network.go`): wraps the push subscriber behind
  the pull `Normalizer`; geofences to configured areas; **hard-errors Poll when 0
  brokers are connected** so our outage never falsely expires live nodes.
- **Wiring**: `cmd/server/main.go` (optional poller, gated on
  `enabled && len(brokers)>0`), `internal/config` + `prefab.yaml`
  (`grid.meshcore` + `grid.sources.meshcore`, `disappearance: expire`,
  `expireAfter: 120h`).
- **Projection**: `mesh_node.geojson` map layer + `/api/v1/events?layer=network`
  + a `comms` place-summary domain (shown only when enabled; excluded from
  top-level `totalActive`/`mode` as ambient INFO state).
- **Docs**: `CHANGELOG.md`, `CLAUDE.md`, `internal/{store,ingest}/CLAUDE.md`,
  and the public reference (`web/src/partials/docs-body.html`).
- **Tests**: advert decode (real signed keypairs), registry ingest/merge/prune,
  normalizer geofence + fail-loud, store telemetry-no-op, geojson projection.

## Blocked: broker subscriber access

The community brokers (`mqtt.meshmapper.net`, LetsMesh US/EU, `mqttmc01.bostonme.sh`,
all WSS+TLS :443) split auth (michaelhart/meshcore-mqtt-broker model):

- **Publishing** is self-sovereign: username `v1_{PUBKEY}` + a self-signed Ed25519
  JWT password, no allowlist.
- **Subscribing** (what we do) needs a **separate operator-issued
  `username:password`** account. A device key alone does **not** grant read access.

meshmapper is explicitly "not a data broker"; access is via a Region Onboarding
form → credentials over Discord. **Access request is pending** (as of 2026-07-17).

Our client already supports this: the operator drops the issued user/pass into
`grid.meshcore.brokers[].username/password`. The device-key JWT path is **not
needed** for a read-only subscriber.

## Framing verified + decoder fixed (2026-07-17)

Live-captured against `wss://mqtt.gomesh.dev:443/mqtt` (subscriber
`<user>:<pass>`, a "full" test user). Findings:

- Envelope field names/casing match `packetEnvelope`; `packet_type`/`SNR`/`RSSI`
  arrive as strings (handled). Multi-gateway dedup confirmed (same `raw`+`hash`
  from multiple repeaters).
- **`raw` is the FULL over-the-air frame, not the pubkey-first payload.** The
  original decoder read from byte 0 and produced garbage (all `sig=false`, absurd
  lat/lng, junk names). Fixed by `meshcore.DecodeFrame`, which strips
  `header(1) + transport_codes(4 for transport routes) + path_len(1) + path` before
  `DecodeAdvert`. The key subtlety: `path_len = ((hash_size-1)<<6) | hash_count`,
  so the path is `hash_size*hash_count` bytes — a naive `raw[2+raw[1]:]` strip is
  wrong for `hash_size>1` meshes (seen live). Golden frames are pinned in
  `frame_test.go`; after the fix, real adverts decode with valid signatures, sane
  roles/locations (e.g. "ESP4 Gilroy Repeater" @ 37.02,-121.59), and clean names.
- **`requireValidSignature` is now `true`** by default (framing confirmed, real
  adverts verify) so spoofed/corrupt adverts are dropped.

## Remaining steps to go live (in order)

1. **Get a subscriber credential for a broker covering our service area.** The
   gomesh.dev capture is a Bay Area/Monterey mesh (IATA `OAK`/`MRY`) — great for
   decoder validation, not our Calaveras/Tuolumne region. Still need meshmapper
   onboarding (or a LetsMesh/bostonme.sh equivalent) for a region-relevant feed.
2. **Configure `prefab.yaml`**: set `grid.meshcore.enabled: true`, add the
   broker(s) with a region topic filter (e.g. `meshcore/{IATA}/+/packets` scoped
   to the service-area IATA codes), secrets via `PF__` env. The node geofence is
   `grid.meshcore.bounds` — deliberately **wider than the hazard area** (Bay Area
   + Monterey + Sierra) because our Sierra repeaters run power-saver and are quiet;
   the wider net proves the source is live. Tighten once traffic is confirmed;
   omit `bounds` to fall back to the `hazards.areas` union.
3. **End-to-end verify** with real traffic: nodes appear in
   `/api/v1/events?layer=network`, `/sources` shows `meshcore` OK, the
   `mesh_node.geojson` layer draws points, the `comms` summary domain appears, and
   a telemetry-only re-advert does **not** grow `/events/{id}/history`.

## Open design questions to revisit when live

- **Privacy**: we currently persist located companion (personal) nodes into a
  public, full-history API. Consider role-filtering (infrastructure only) or
  suppressing exact geometry for `companion`-type nodes. Location is already
  quantized to ~11 m.
- **`activeWindow` vs `expireAfter`**: current 30m / 120h. Tune once we see real
  advert cadence (repeaters ~12h; companions irregular).
- **Region topic scoping**: decide the IATA filter for the subscribe topic to
  avoid buffering the global mesh (memory is bounded by `RetainFor`, but a topic
  filter is cheaper).
