---
title: The MQTT subscriber went deaf for six days and the mesh source said OK
date: 2026-10-09
category: logic-errors
module: internal/clients/meshcore, internal/ingest/network
problem_type: logic_error
component: background_job
symptoms:
  - "The mesh map shows 2 of ~15 SIERRA repeaters, while CoreScope (same broker) shows most of them advertising today"
  - "GET /api/v1/sources says meshcore OK while hundreds of mesh nodes flip to EXPIRED"
  - "Mesh revisions fall from ~100 a day to ~20, then jump to 200+ an hour after a restart"
  - "A local server on the committed config logs 'MeshCore broker connection lost' every 1-2 seconds"
root_cause: logic_error
resolution_type: code_fix
severity: high
tags: [meshcore, mqtt, paho, fail-loud, health, client-id, session-takeover, sweep, expire]
applies_when:
  - a source's health is derived from a client library's notion of "connected"
  - a push feed (subscription) is wrapped as a poller, so no fetch ever fails
  - one input of a multi-input poller can keep the poll "successful" while another is dead
  - a fixed client or session id is committed in config that every environment loads
---

# The MQTT subscriber went deaf for six days and the mesh source said OK

Primary files: `internal/clients/meshcore/client.go` (`Brokers`,
`deliveryProblem`, `clientID`), `internal/ingest/network.go` (`Poll`,
`brokerDelivery`), `internal/ingest/CLAUDE.md` ("A deaf broker").

## Symptom

The mesh map showed two SIERRA repeaters. CoreScope, an analyzer subscribed to
the same gomesh broker, showed fifteen, twelve of them heard in the last day, and
for one of them (Lilac Park) its observer list matched our gateway list exactly.
So the feed was fine, and we were not ingesting it.

The store's own history showed when it happened. Mesh revisions per day:

| days | activations | expirations |
| --- | --- | --- |
| 2026-09-15 .. 10-02 | 50-150 | 38-128 |
| 2026-10-04 .. 10-08 | 14-22 | 31-198 |
| 2026-10-09 after 09:00 UTC | 224 | 12 |

Relay-link discovery ran at 1-5 new links an hour before 09:00 on 10-09 and
~150 an hour after. `/api/v1/sources` read `meshcore OK` the whole time.

## Why nothing noticed

Three gaps lined up:

1. **paho's `IsConnected` is not "connected".** With `AutoReconnect` (or
   `ConnectRetry` while connecting) it returns true while the client is
   reconnecting. `Registry.Health` counted that as a connected broker, so a
   broker that had dropped us stayed "connected" indefinitely.
2. **Nothing read the message clock.** An open session can carry nothing. The
   registry kept `lastMsgAt`, and `Poll` discarded it.
3. **A live monitor masked the broker.** The hard error needed EVERY input
   dead. The alanpi monitor stayed live, so `Poll` succeeded with only its
   nodes, and the sweep expired every node the broker alone had heard.

## Likeliest cause, reproduced

The client id was `data.sierragridteam.org`, committed in `prefab.yaml` and
loaded by every environment. gomesh accepts a connection with **no
credentials** under that id. It delivers nothing to it, but it still takes the
session over. A pre-fix build run locally with no credentials logged `MeshCore
broker connected` / `connection lost` every 1-2 seconds, as the broker handed
the one session back and forth between it and production. The same build with
a unique id stayed connected. So any second process on the committed config (a
dev server, an agent sandbox following the verify skill, an overlapping
deploy) starves production for as long as it runs. We could not identify which
process was running from 10-03 to 10-09.

## Fix

- `Registry.Brokers()` replaces `Health()`. `Open` is `IsConnectionOpen`.
  Every MQTT message of any type stamps a per-broker atomic clock.
  `deliveryProblem` fails a broker that is closed, or open and silent past
  `grid.meshcore.silenceAfter` (15m). Silence is measured from the last
  message, else from the FIRST dial, never from the latest reconnect, so a
  session that keeps dropping and returning with nothing never looks fresh.
- `Poll` turns any non-delivering broker into `PerSource["meshcore"]`. That
  skips the sweep and the `TouchSeen` refresh and degrades the source row,
  while a live monitor's or forwarder's events still land. With nothing live at
  all it is still a hard error, now naming the broker.
- Client ids get a random per-process suffix (`data.sierragridteam.org-1a2b3c4d`),
  stable across that process's own reconnects.

Verified locally: an anonymous session on a unique id, with
`silenceAfter=1m`, turned `meshcore` STALE with `lastError` "...wss://mqtt.gomesh.dev:443/mqtt
connected but silent (no message since ...)".

## Guidance

- "Connected" from a client library is a claim about the library's state
  machine, not about data flowing. Health for a subscription is the age of the
  last message.
- When one input can keep a multi-input poll successful, every other input
  needs its own way to fail the source. "All inputs dead ⇒ error" alone lets
  the healthy input vouch for the dead one.
- Never commit a session-unique id (an MQTT client id, a consumer-group member
  id) in config that every environment loads. Derive it per process.
- When an external analyzer reads the same feed, compare against it first. It
  answers "is upstream quiet, or are we deaf?" in one request.
