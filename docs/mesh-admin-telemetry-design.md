# Operator-Reported Mesh Telemetry — Technical Design

Status: **shipped 2026-09-15.** Companion to `docs/mesh-topology-design.md`,
which this extends rather than replaces. The operator-facing setup guide — what
to hand someone standing up a monitor — is
`docs/push-ingest-reporter-guide.md`; this document is the reasoning behind it.

## 1. Summary

A second input for the `MESH` layer: operator-run monitors that log into MeshCore
repeaters over the mesh, read their admin interface, and POST the result to an
authenticated webhook. Nine SIERRA backbone repeaters were previously invisible
to the Grid — they are quiet by design (power-saver mode) and no community MQTT
bridge covers them.

The webhook does not write the store. It fills an in-memory buffer that the
existing mesh poller drains on its tick, exactly as the MQTT subscriber's buffer
is drained. Everything downstream — identity, lifecycle, grace, sweep, history,
projection — is unchanged.

## 2. What a monitor can say that a bridge cannot

| | MQTT adverts | Operator monitor |
|---|---|---|
| Evidence | positive only | positive **and negative** |
| Identity | signed, self-declared, with location | name + pubkey PREFIX, no coordinates |
| Metrics | what a listener observed (SNR/RSSI/hops) | what the node knows (battery, airtime, counters) |
| Coverage | whatever the bridges hear | whatever the operator owns |

The negative evidence is the most valuable part and the least obvious. A mesh has
no goodbye packet, so silence is ambiguous and the whole lifecycle design bends
around that ambiguity. A monitor that tried a node and failed has removed the
ambiguity for that node.

## 3. Three questions the design answers

### 3.1 Dedupe: a monitor knows a node by a PREFIX

Reports identify a node by 8 bytes of its Ed25519 public key; our event ids carry
all 32. So dedupe is prefix resolution, not string equality.

`Poll` resolves a reported prefix against a catalog of full keys (this tick's
adverts + the store's existing mesh events + the registry's retained nodes) under
a **unique-match-only** rule — the same rule `meshcore.resolvePath` applies to
relay hops, for the same reason: attaching a repeater's battery reading to the
wrong repeater is worse than leaving it unattached.

- Resolved → the id is `meshcore:<full key>`, which *is* the MQTT event's id. One
  event, two contributing inputs. There is no merge of two stored events, because
  two were never created.
- Unresolved → `meshcore:<prefix>`. A full key is 64 hex and a prefix is shorter,
  so the two id forms cannot collide.
- Promotion → when an advert finally supplies the full key,
  `supersededPrefixIDs` retires the prefix event through `Superseded`. Positive
  evidence naming the successor; the same shape as a standalone FIRIS perimeter
  being adopted by a CAL FIRE incident.

**Consumer consequence:** a mesh event id is not permanent for a node that has
never been heard over MQTT. This is in `CHANGELOG.md` and the public reference.

### 3.2 Where telemetry lives — and why the event table needed a change

Telemetry belongs in the event's hash-excluded `MeshTelemetry` block; a separate
measurements table would be over-engineering for data nobody needs to trend
forever. But putting it there naively means it is **never written at all**, and
the reason is worth stating because it is not obvious from either side alone:

1. `store.ContentHash` zeroes `MeshDetail.Telemetry`, so a telemetry-only change
   is hash-equal. (Required — counters move every report; hashed, a 5-minute
   cadence would mint ~288 revisions per node per day.)
2. `Scheduler.shouldUpsert` skips the write path for hash-equal events. (Also
   required — this is what stopped a 400-node mesh tick opening 400 pointless
   transactions, and the read-latency spikes that came with them.)
3. Even when it doesn't skip, `UpsertEvent`'s `oldHash == hash` branch returns
   early **without rewriting the blob**.

Each of those is correct in isolation; together they mean a battery reading would
only reach the store on the ticks where the node's *identity* changed, which for a
fixed repeater is never. The value served would sit frozen at whatever was current
the last time someone renamed it.

The fix reuses the machinery that already exists for the other hash-excluded
field, the AI enhancement:

- `PollResult.ForceWrite` — ids the poller says carry changed hash-excluded
  content. `shouldUpsert` honours it (its fourth case, alongside `fullReconcile`,
  a carried `Enhancement`, and a failed `NeedsUpdate`).
- `refreshEventPlaces`' new `telChanged` branch rewrites the blob **without
  bumping the revision**, exactly as `enhChanged` does.
- `shouldPersistTelemetry` **coalesces** on `grid.ingest.telemetryPersistInterval`
  (10m). Each forced write is a transaction, and on EFS every commit invalidates
  every reader's page cache — the same reasoning behind `touchSeenCoalesce`, and
  the same bounded-staleness trade.

Net effect: `/api/v1/events/{id}` serves fresh telemetry; `/history` shows only
meaningful changes.

### 3.3 Reachability — the one thing that IS hashed

`MeshDetail.reachability` is part of the content hash, deliberately breaking the
rule above, because a repeater going down is real history. "How often is Lilac
Park down?" is a question the hash-excluded block can never answer.

To make a hashed field affordable it must not flap, so it is derived from the
**age** of the monitor's last success (`grid.ingest.unreachableAfter`, 45m) and
not from the reporter's own per-poll `online` boolean, which flips on any
marginal node. One missed poll changes nothing; a sustained outage transitions
exactly once.

`UNSPECIFIED` (omitted on the wire) means no monitor watches the node. It is not
`UNREACHABLE`, and a client must not render it as one.

## 4. Fail-loud with two inputs

The rule was "all brokers down ⇒ hard `Poll` error". It splits:

- **Every input dead** (no broker AND no fresh reporter) ⇒ hard error, as before.
- **Some input degraded** ⇒ emit what we have, plus `SweepSuppress["meshcore"]`.

A silent reporter contributes **nothing** to the snapshot — replaying its last set
would refresh `last_seen_at` and fabricate liveness for nodes nobody has checked
in hours — while **suppressing the sweep**, because its silence must not read as
departure. Those are the same honesty applied to two different questions.

Suppression is **bounded**: OK → STALE (suppress; probably a blip) → DEAD (stop
suppressing) at `4 × staleAfter`. A monitor that never returns must not freeze the
layer's lifecycle forever. This bound is only acceptable because `meshcore` is an
`expire` source, so nodes reach EXPIRED — "we lost track" — rather than the
fabricated all-clear RESOLVED would be, and because mesh presence is ambient
`INFO`. **Do not copy the bound onto a life-safety layer.**

## 5. Auth, in a public repository

A reporter presents `Authorization: Bearer <token>`. Config stores only
`sha256(token)`, and the asymmetry is the whole point: a SHA-256 of a 256-bit
random token cannot be replayed or reversed, so the committed file carries it
safely, adding a reporter is an ordinary PR, and the token exists only on the
operator's machine. `make ingest-token REPORTER=<id>` mints one and splices the
entry into `prefab.yaml` — editing the file as text, because that config is half
prose and a YAML round-trip would delete every comment in it. Re-running with an
existing id rotates the token in place rather than rebuilding the entry, so a
hand-tuned `staleAfter` or `placeIds` survives a credential change. The tool
asserts the token does not appear in what it writes.

A malformed or duplicate token hash is **fatal at startup**. Skipping the reporter
would present as a monitor that silently never authenticates — indistinguishable
from a wrong token at the client, and liable to go unnoticed for weeks.

Other guards, in the order the handler applies them: method, auth, stream
authorization, rate limit, body size, parse. Nothing about the body is examined
before the caller is known.

CORS is what keeps this unreachable from a cross-origin browser page:
`corsAllowMethods: [GET]` denies the POST preflight — the same property that
protects `/mcp`. **Never add POST to that list.**

## 6. Extensibility

A *stream* names a payload contract (`mesh.repeater`); the body's own
`schema_version` versions it. Streams live in a map (`Registry.dispatch`) and a
reporter is authorized per stream, so a second monitor pushing a different shape
is one `streamHandler` plus a config block — no change to routing, auth, limits,
health, or the normalizer contract.

Multiple reporters with overlapping coverage already work:

- Reports are keyed `(reporter, node)`, so overlap is not lost.
- Identity precedence is **by input class, then configured `priority`, then
  reporter id** — never recency. Identity is hashed, so a rule that could flip
  per tick would mint a revision each time.
- Liveness is the max across inputs; reachability is "any monitor reached it";
  telemetry is the freshest successful sample, labelled with its `reporterId`.
- Each reporter gets its own health-only source row.

## 7. Deliberate omissions

- **No geofence on pushed nodes.** The fence scopes an anonymous global broadcast
  feed; a reporter is an authenticated operator asserting facts about its own
  equipment, and the nodes that most need this path are exactly the ones that
  advertise no location to test.
- **Therefore no geometry, and no geometric place attachment**, until an advert
  supplies a position. `Reporter.PlaceIDs` is the operator's escape hatch
  (`UpsertEvent` unions caller-preset `place_ids` with geometric matches); empty
  by default, because asserting a location we do not have is worse than admitting
  we lack one.
- **No sample at all for a never-reached node.** Zeroed counters would assert it
  has sent and received nothing. "Never read" and "read as zero" are different
  facts; gauges use wrapper types for the same reason.
- **We do not poll a reporter's published file.** Push only, as specified.

## 8. Open questions

- **Should a reporter be able to supply coordinates?** The obvious next schema
  addition, and it would close §7's gap. Deferred until an operator has them.
- **Does a stale reporter belong in the mesh layer's `sourceStatus`?**
  `layerSourceIDs[mesh]` is still `meshcore` alone, so a silent monitor degrades
  its own source row but not the layer. Revisit if a partial-coverage layer
  reading `OK` proves misleading in practice.
- **Reachability as a severity signal.** Today a downed repeater is `INFO` like
  everything else on the layer, so it never escalates a place's mode. If comms
  become load-bearing for the Ebbetts Pass corridor, a backbone repeater being
  down may deserve more.
