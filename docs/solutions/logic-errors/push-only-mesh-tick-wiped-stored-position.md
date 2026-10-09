---
title: A tick that learned nothing about a node's position wiped the position it had
date: 2026-10-09
category: logic-errors
module: internal/ingest/network
problem_type: logic_error
component: background_job
severity: medium
tags: [ingest, meshcore, push-ingest, geometry, place-attachment, content-hash, revisions, carry-forward, presence-window]
applies_when:
  - a normalizer merges two inputs where only one of them carries a field (here coordinates) and the other is routinely the only input present
  - an event's derived state (place attachment, map membership) hangs off a field that one input cannot supply
  - a hashed field flips between a value and nil on alternate ticks, minting a revision pair each cycle with nothing upstream having changed
  - a map or place-scoped list shows far fewer items than the global event list for the same layer
---

# A tick that learned nothing about a node's position wiped the position it had

Primary files: `internal/ingest/network.go` (`buildEvent`), `internal/ingest/network_push_test.go` (`TestPushOnlyTickKeepsStoredPosition`), `internal/ingest/CLAUDE.md`.

## Symptom

The Ebbetts Pass mesh map showed one SIERRA repeater. `GET /api/v1/events?layer=mesh`
listed 27 nodes, nine of them SIERRA, all `ACTIVE`, both mesh sources `OK` in
`/api/v1/sources`. The same query scoped to `place=ebbetts-pass` (or
`calaveras-county`) returned the one node the map drew. The first suspicion was
that the MQTT bridge had changed its envelope or framing.

## Diagnosis

It had not. Non-SIERRA adverts were decoding, verifying (`requireValidSignature`
is on) and landing minutes before the check, and the one SIERRA node on the map
carried fresh gateways and a `lastAdvertAt` from that night. The eight missing
nodes all looked alike in event detail: `geometry: null`, `placeIds: []`,
`telemetry.gateways: []`, `observedAt` a whole-second stamp (the monitor's
clock, not our receive clock). They were being rebuilt from the push reporter
alone.

The history of one of them (`meshcore:a2d649…`, Lilac Park) is the tell:

| rev | ingested | geometry | gateways |
| --- | --- | --- | --- |
| 39 | 2026-10-08 11:09:50Z | point | `E3635C…` (an advert) |
| 40 | 2026-10-09 01:09:51Z | **nil** | none (report only) |

Revision 40 landed 14h00m after the advert that produced 39 — exactly
`grid.meshcore.graceFloor`. The registry's `Snapshot` drops a node once its
last advert is older than its presence window; for a repeater that advertises
about once a day that is most of the day. The monitor still reached the node,
so `Poll` kept it in `merged` with `m.mqtt == nil`, and `buildEvent` only set
geometry inside `if m.mqtt != nil`. Geometry is hashed, so the nil minted a
revision; `UpsertEvent` recomputed `event_places` from no geometry, so the
node left every place; the next advert put it back and minted another.
Revisions 36→37→38→39→40 on Lilac Park are that cycle, five times.

The map showed the one SIERRA node a distant gateway had happened to hear
within the last 14 hours. Nothing was wrong with ingest, and nothing was wrong
with the data: the store held every node, just not where it was.

## Fix

In the `m.mqtt == nil` branch, carry the prior event's geometry forward:

```go
} else {
    ev.Geometry = priorByID(prior, ev.GetId()).GetGeometry()
}
```

This is the same shape as two rules already in the file. `stablePosition`
returns the STORED geometry, byte for byte, when a fresh fix is within the
noise threshold, because equal bytes are what make the content hash equal.
`keepPriorAttribution` carries the broker attribution across a restart because
"this tick learned no broker" is not "there is no broker". A report carries no
coordinates, and the Grid does not invent them, but it does not forget them
either: "nothing new" is not "nowhere".

A node a monitor knows that has never advertised a location has nothing to
carry and stays place-less, as before, until an advert supplies one
(`TestPushOnlyNodeNeverLocatedStaysPlaceless`).

The regression test runs two ticks through the real `Poll`: an advert plus a
report, then the report alone with the first tick's event as the prior. It
asserts `store.ContentHash` is unchanged, not just that geometry is non-nil,
because "no revision minted" is the property, and the geometry is only how it
is achieved.

## Recovery

The fix carries the prior's geometry, and a node already stored WITHOUT
geometry has nothing to carry. Each wiped node recovers on its next advert that
reaches a subscribed broker; from then on the position persists through every
report-only tick. For once-a-day repeaters that is up to a day; for ones the
brokers hear rarely it is however long that takes.

## Guidance

- When a merge has two inputs and only one can supply a field, decide what a
  tick with the OTHER input alone means for that field. If the answer is
  "nothing changed", the code must say so by carrying the stored value, not by
  leaving the field unset. Unset is a claim.
- A hashed field that one input cannot supply will flap between a value and
  nil whenever the inputs alternate. Look for a revision history that
  alternates on one field with every other field identical; that pattern is
  this bug, whatever the field.
- Derived state hanging off that field (here `event_places`, and with it every
  place-scoped read) disappears and reappears with it, silently. The global
  list stayed correct the whole time; only the scoped reads were wrong, which
  is why it looked like an upstream outage rather than an ingest bug.
- Pin the carry-forward on the content hash. A test on the field alone would
  pass a fix that re-encoded the coordinates to equal values with different
  bytes, and that fix would still mint revisions.
