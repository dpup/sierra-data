# SIERRA Mesh Relay — Wire Protocol v1

*Canonical spec. Both the relay (data server) and the bridge (meshbridge) implement this. Keep this file identical in both repos; change it in one place and copy it.*

## Overview

Three parties:

- **Node**: a stock MeshCore companion radio (upstream firmware ≥ 1.18). Speaks the MeshCore companion protocol over USB serial or TCP port 5000.
- **Bridge**: software at a host's house. Holds the one connection to the node and dials *out* to the relay.
- **Relay**: runs at `data.sierragridteam.org`. Accepts bridges, and lets authenticated clients talk to a node through its bridge.

```
 node <--serial/TCP--> bridge ==WSS (bridge dials out)==> relay <==WSS== client
```

Nothing ever connects in to a host's house. Nothing on the relay gives unauthenticated access to a node.

## Endpoints

| Path | Used by | Subprotocol |
|---|---|---|
| `wss://data.sierragridteam.org/mesh/relay/<gateway>/dial` | Bridge | `sierra-mesh-bridge.v1` |
| `wss://data.sierragridteam.org/mesh/relay/<gateway>` | Client | `sierra-mesh-client.v1` |
| `https://data.sierragridteam.org/mesh/relay/api/...` | Admin tools | n/a (see relay spec) |

`<gateway>` is a lowercase name, `[a-z0-9-]{3,40}`, e.g. `avery-ridge`.

## Authentication

- Both bridge and client send `Authorization: Bearer <token>` on the WebSocket upgrade request.
- Bridge tokens are per gateway. The relay rejects a bridge whose token doesn't belong to `<gateway>`.
- Client tokens are per person or per agent. They carry a role (see relay spec).
- Tokens are opaque: 32 random bytes, base64url, prefixed `smb_` (bridge) or `smc_` (client) so they're recognizable if leaked.
- The relay rejects the upgrade with HTTP 401 for a bad token, 404 for an unknown gateway, and 426 for an unsupported subprotocol.

## Frames

Each WebSocket message is one of two kinds:

- **Binary message** = exactly one MeshCore companion protocol frame *payload*, without the serial/TCP header.
  - On the node link, companion frames are wrapped as `[0x3c][len_lo][len_hi][payload]` toward the node and `[0x3e][len_lo][len_hi][payload]` from the node. The bridge strips the header when reading from the node and adds it when writing to the node.
  - Direction is implied by who sent the message, so no header is needed on the WebSocket.
  - Max payload: 512 bytes. Anything larger is a protocol error. (Current firmware max frame is 176; this leaves headroom.)
  - Verified against MeshCore v1.18.0: `ArduinoSerialInterface.cpp` (`checkRecvFrame` / `writeFrame`) and `SerialWifiInterface.cpp` both use `'<'` (0x3c) toward the node and `'>'` (0x3e) from it, each followed by a 2-byte little-endian length. `MAX_FRAME_SIZE` is 176 (`src/helpers/BaseSerialInterface.h`); the firmware drops oversized outgoing frames and truncates oversized incoming ones.
- **Text message** = one JSON control object with a `type` field. Unknown `type` values are ignored (forward compatibility).

The relay forwards binary messages between the bridge and the client holding the lease, unchanged. It doesn't parse them, except where the relay spec says otherwise.

**Rate limit.** The relay throttles each connection to about 50 messages/s (burst 100). Throttling is backpressure: the relay reads more slowly. It does not drop frames or disconnect, so a node that sends its whole contact list at once is slowed down, not cut off.

## Bridge ↔ relay control messages

### `hello` (bridge → relay, first message, required)

```json
{
  "type": "hello",
  "protocol": 1,
  "gateway": "avery-ridge",
  "agent": { "name": "meshbridge", "version": "0.3.1", "os": "linux", "arch": "arm64" },
  "link": { "kind": "serial", "detail": "/dev/ttyACM0" },
  "node": { "state": "connected", "pubkey": "a1b2…64 hex", "name": "SIERRA-GW-avery-ridge", "fw": "1.18.0", "model": "Heltec V4" }
}
```

- `link.kind`: `serial` or `tcp`. `detail` is informational (port name or `host:port`).
- `node.state`: `connected`, `disconnected`, or `mismatch` (the node's pubkey doesn't match the bridge's configured `expected_pubkey`).
- When `node.state` isn't `connected`, `pubkey`, `name`, `fw`, and `model` may be omitted.
- The relay closes the socket with 4003 if `hello` doesn't arrive within 10 seconds.

### `welcome` (relay → bridge)

```json
{ "type": "welcome", "session": "s_7f3c…", "ping_interval_s": 25, "server_time": "2026-11-03T22:51:00Z" }
```

The bridge must not send binary messages before `welcome`.

### `node_status` (bridge → relay, on any change)

```json
{ "type": "node_status", "state": "disconnected", "error": "serial port closed" }
```

Same `state` values and node fields as `hello.node`. Send it when the node link drops, reconnects, or the pubkey check fails.

### `stats` (bridge → relay, every 60 s)

```json
{ "type": "stats", "uptime_s": 86400, "node_link_reconnects": 2, "frames_to_node": 1204, "frames_from_node": 3388, "relay_reconnects": 1 }
```

### `attach` / `detach` (relay → bridge)

```json
{ "type": "attach", "lease": "l_91ad…", "client": "elmer" }
{ "type": "detach", "lease": "l_91ad…" }
```

Informational: tells the bridge a client started or stopped using the node. The bridge logs these. If no client is attached, the bridge drops frames that arrive from the node, except for its own identity check.

### `relink` (relay → bridge)

```json
{ "type": "relink" }
```

The bridge closes and reopens its connection to the node, then re-runs the identity check and sends `node_status`. Used to clear a stuck node session.

### `update_available` (relay → bridge, optional)

```json
{ "type": "update_available", "version": "0.4.0", "url": "https://…", "sha256": "…", "sig": "base64 ed25519" }
```

v1 bridges may log this and do nothing else.

### `error` (either direction)

```json
{ "type": "error", "code": "frame_too_large", "message": "…" }
```

Codes the relay sends: `hello_required`, `unsupported_protocol`, `frame_too_large`, `busy`, `gateway_offline`, `pubkey_mismatch`, `pubkey_unverified`, `node_<state>`, `renew_denied`, `unknown_lease`. Treat `message` as free text and don't rely on its content. The relay logs only the `code` and the length of `message` for an `error` that a bridge sends.

## Client ↔ relay control messages

### Getting a lease

A client gets a lease by connecting. The relay grants one only when the bridge is online, the node is `connected`, and the node's pubkey matches the gateway's pinned key. Otherwise:

- **Gateway busy:** the relay sends `error` with `code: "busy"` and closes with 4029.
- **Gateway unavailable:** the relay sends `gateway_status`, then `error` with a `code` (`gateway_offline`, `pubkey_mismatch`, `pubkey_unverified`, or `node_<state>`), then closes with 1000.

An `admin` can append `?preempt=1` to the client URL. That takes the lease from its current holder, who gets `lease_ending` with reason `preempted`. A non-admin who asks for preemption gets HTTP 403.

### `lease` (relay → client, after a successful upgrade)

```json
{ "type": "lease", "lease": "l_91ad…", "gateway": "avery-ridge", "expires_at": "2026-11-03T23:21:00Z", "node": { "state": "connected", "pubkey": "…", "name": "…" } }
```

The client may send binary frames after `lease`. The client is responsible for the normal companion handshake (`APP_START`, etc.).

### `lease_ending` (relay → client)

```json
{ "type": "lease_ending", "lease": "l_91ad…", "in_s": 60, "reason": "expiry" }
```

`reason`: `expiry`, `preempted` (an admin took over), or `gateway_offline`. Relay v1 also sends these additive values. A client must treat any reason it doesn't recognize as "the lease is ending":

- `idle`: no frames in either direction for 10 min.
- `revoked`: the client's token was revoked, or the gateway was deleted.
- `pubkey_mismatch`: the node started reporting a pubkey that doesn't match the pin.
- `relay_shutdown`: the relay is restarting.

With `in_s` > 0 it's a warning: the relay sends one 60 s before expiry, and a `renew` cancels it. With `in_s: 0` the lease has already ended, and the relay closes the socket with 1000.

`renew` extends the lease by another 30 min. Renewals stop at 8 h from the start of the lease. After that the relay answers `renew` with `error` `renew_denied`. A `renew` for a different lease id gets `error` `unknown_lease`.

### `renew` (client → relay)

```json
{ "type": "renew", "lease": "l_91ad…" }
```

Extends the lease if policy allows. The relay answers with a new `lease` message.

### `gateway_status` (relay → client, on change)

```json
{ "type": "gateway_status", "bridge": "offline", "node": { "state": "disconnected" } }
```

## Identity check (bridge)

When the bridge opens the node link, it sends `APP_START` with app name `sierra-meshbridge`, reads `SELF_INFO`, and takes the node's public key from it.

- If `expected_pubkey` is configured and doesn't match, the node is `mismatch`. The bridge reports it and forwards nothing.
- If `expected_pubkey` isn't configured, the bridge reports the key it found. The relay pins it on first sight and flags any later change.

This is the only time the bridge parses companion frames.

## Keepalive and close

- Both sides send WebSocket pings every `ping_interval_s` (default 25 s). This keeps proxies with an idle timeout of about 100 s from dropping the socket.
- Either side closes after 3 missed pongs.

Close codes:

| Code | Meaning | Bridge reaction |
|---|---|---|
| 1000 | Normal | Reconnect with backoff |
| 1001 | Going away: the relay is restarting | Reconnect with backoff |
| 1009 | Message too big (text over 8 KiB) | Reconnect with backoff, and fix the sender |
| 4001 | Bad or revoked token | Back off 15 min, log loudly |
| 4002 | Gateway name doesn't match token | Back off 15 min, log loudly |
| 4003 | Protocol error or missing `hello` | Reconnect with backoff |
| 4004 | Unsupported protocol version | Back off 1 h, log "update required" |
| 4009 | Replaced: a newer session for this gateway connected | Reconnect with backoff (2 bridges with the same token will flap; that's visible in relay logs) |
| 4029 | (Client) Gateway busy, lease held by someone else | n/a |

Bridge reconnect backoff: start at 1 s, double up to 60 s, add ±20% jitter. Reset after 5 minutes connected.

## Versioning

- The subprotocol name carries the major version. Breaking changes mean `v2` and a new subprotocol name.
- Additive changes (new optional fields, new message types) keep `v1`. Receivers ignore unknown fields and types.
- `hello.protocol` carries the same major version as a sanity check.

## Design constraint

Keep the protocol small enough for a future embedded bridge (ESP32-class): JSON control messages are tiny and infrequent, binary frames are passed through unchanged, and nothing requires the bridge to keep companion-protocol state beyond the identity check.
