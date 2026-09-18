# MeshCore — open questions

The MeshCore mesh-node presence source is **live** (`grid.meshcore.enabled: true`,
subscribing to `wss://mqtt.gomesh.dev:443/mqtt`). Two decisions were deferred
"until it's live" and are still unmade. Everything else from the integration
effort is settled and archived in
[`docs/design/meshcore-integration.md`](design/meshcore-integration.md).

_Last updated: 2026-09-15._

## 1. Privacy: companion nodes in a public, full-history API

**Nothing has been implemented.** There is no role filter in
`internal/ingest/network.go` or `config.MeshcoreConfig`, so a located
**companion** (personal, human-carried) node is ingested exactly like a backbone
repeater: one `MESH` event, published on `/api/v1/events?layer=mesh` and the
`mesh_node.geojson` layer, with a revision history that is kept indefinitely.

The only thing standing between a person and a public location trail is
`quantizeCoord` (`internal/ingest/network.go:667`, ~11 m) — and that exists to
stop location jitter minting spurious revisions, **not** as a privacy measure.
Note also that a node only enters the store at all because it *advertised* its
location unencrypted over LoRa; the question is whether re-publishing that into a
queryable archive with history is a different act than relaying it.

Options, roughly in increasing order of cost:

- **Role-filter on ingest** — only `repeater` / `room_server` / `sensor` become
  events; companions are counted but not published. Simplest, and infrastructure
  presence is what the `comms` domain is actually for.
- **Suppress geometry for companions** — keep the node, drop the `Point`. Keeps
  "N nodes heard" honest without the trail.
- **Coarsen companion location** — a much larger quantization (~1 km) for that
  role only. Still leaves a history.
- **Do nothing, document it** — the adverts are already public broadcast.

Whichever is chosen, decide it for the **revision history** too: filtering ingest
from here on does not retract what the store already holds.

## 2. Region topic scoping and geofence width

Both are still deliberately wide, with the original "prove the source is live"
rationale intact in `prefab.yaml`:

- **Subscribe topic** is `meshcore/+/+/packets` — the *global* mesh, no IATA
  scoping. Memory is bounded by `graceCeil`, but a topic filter is cheaper than
  buffering and discarding.
- **`grid.meshcore.bounds`** spans 36–39°N / -123 to -119.5°W (Bay Area +
  Monterey + the Calaveras/Tuolumne Sierra), a long way outside the hazard area.

The tightening condition was "once traffic is confirmed". The open question is
what to tighten *to*: our Sierra repeaters run power-saver and are quiet, so a
service-area-only feed may be too sparse to distinguish "mesh is quiet" from
"our subscriber died" — which is exactly what the wide net was bought to prevent.
Confirm the Sierra nodes are actually being heard before narrowing either one,
and consider keeping the wide bounds as a liveness canary even if the topic
filter narrows.
