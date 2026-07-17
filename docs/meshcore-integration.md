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
  decoder, std-lib crypto) + `client.go` (`Registry` — dials N brokers via paho,
  parses the JSON envelope, filters `packet_type==4`, buffers node state keyed by
  pubkey, `Snapshot`/`Health`).
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

## Next steps (in order)

1. **Get a subscriber credential** (blocking): meshmapper onboarding, or a
   LetsMesh/bostonme.sh equivalent. Meanwhile, the decoder can be validated
   against a **self-hosted** broker + a publishing observer
   (`Cisien/meshcoretomqtt`) — no third-party gating.
2. **Live-capture verification** (the plan's original step 1): subscribe, capture
   a real `packet_type==4` message, and confirm:
   - envelope field names/casing match `packetEnvelope`;
   - **whether `raw` is the advert payload (pubkey-first) or the full LoRa frame** —
     this is the one framing ambiguity the decoder assumes (payload-first). If it's
     the full frame, add header-stripping in `client.go` `ingestPacket` before
     `DecodeAdvert`.
   - the decoded pubkey/role/lat-lng/name are sane.
3. **Flip `requireValidSignature: true`** once #2 confirms the signature framing
   (currently off — accepts structurally-valid adverts without verifying).
4. **Configure `prefab.yaml`**: set `grid.meshcore.enabled: true`, add the
   broker(s) with a region topic filter (e.g. `meshcore/{IATA}/+/packets` scoped
   to the Bay Area / service-area IATA codes), secrets via `PF__` env.
5. **End-to-end verify** with real traffic: nodes appear in
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
