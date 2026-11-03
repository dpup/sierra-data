---
title: The mesh looked days stale because observedAt was read as "last heard"
date: 2026-11-03
category: logic-errors
module: internal/ingest/network, internal/gridapi, web/public/assets/pages/mesh.js
problem_type: logic_error
component: api_semantics
symptoms:
  - "The /mesh roster shows SIERRA repeaters 'heard 3 days ago' while CoreScope shows them active minutes ago"
  - "GET /api/v1/sources says meshcore OK and the mesh_link layer shows links to the same nodes seen minutes ago"
  - "A mesh node's observedAt only moves when its reachability, name or position changes"
root_cause: logic_error
resolution_type: code_fix
severity: medium
tags: [meshcore, observed_at, content-hash, freshness, relay, flood, direct, corescope]
applies_when:
  - a field excluded from the content hash is presented as a liveness signal
  - a push feed's "last heard" is needed at a cadence the revisioned blob cannot afford
  - comparing our freshness against a third-party analyzer
---

# The mesh looked days stale because observedAt was read as "last heard"

Primary files: `internal/clients/meshcore/frame.go` (`FrameHeader.RelayHops`),
`internal/clients/meshcore/client.go` (`DrainRelays`, `NodeState.ReceivedAt`),
`internal/ingest/network.go` (`meshActivity`), `internal/store/mesh_activity.go`,
`internal/gridapi/mesh_activity.go`.

## Symptom

On 2026-11-03 the `/mesh` roster said eight SIERRA repeaters were last heard on
2026-10-31. CoreScope, reading the same gomesh broker, said all of them were
active within the hour. `/api/v1/sources` said `meshcore OK`, and the
`mesh_link` layer had relay links for Columbia, Forest Meadows and Snowshoe
Lake seen within the last three minutes. So the data was arriving. The page
was reading the wrong clock.

## Why

The roster's "Heard" column, the API reference, and the MCP tool text all said
a mesh event's `observedAt` is when the Grid last heard the node. `observedAt`
is zeroed out of the content hash and persists only with a revision, so it
moves when a node's identity, position or reachability changes. Lilac Park's
revisions were all reachability flips from the operator monitor. Its 00:14 and
12:14 adverts changed nothing hashed and so changed nothing visible. The
telemetry block (`lastAdvertAt`, SNR) was frozen the same way.

A second, smaller gap: we read only ADVERT packets. A backbone repeater adverts
every day or two and relays all day. In a 2-hour sample, flood packets named
Camp Connell as a relay hop 435 times against 33 adverts.

## Fix

- `telemetry.last_heard_at` (our receive time of the node's last advert) and
  `telemetry.last_relayed_at` (our receive time of the last FLOOD packet, of any
  type, that carried its hash as a hop).
- Stored in `mesh_activity`, one row per node, MAX-merged, written in the
  observation insert's transaction. Never in the event blob: a per-tick blob
  rewrite of ~600 events is the EFS cost `TouchSeen` coalescing exists to avoid.
  `gridapi` overlays them on current events at read time and folds them into
  the ETag validators.
- The roster shows the later of the two and names an absence; it no longer
  falls back to `observedAt`.

## Guidance

- A hash-excluded field is not a liveness signal unless something writes it
  back. Before presenting a timestamp as "last seen", check whether it persists
  without a revision.
- Only a flood path records who relayed. A direct path is the route still to
  travel (observed shrinking `[2cfe ab07]` → `[ab07]` → `[]` across observers),
  so a hop there proves nothing. An analyzer that counts both will read fresher
  than you for repeaters that are being called but not answering.
- When a third-party analyzer disagrees, ask which packets it counts before
  assuming you are deaf. Here the adverts matched. The disagreement was in
  what each side called "heard".
