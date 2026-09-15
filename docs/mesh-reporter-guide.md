# Reporting mesh telemetry to The Grid

A guide for operators running a monitor that reads MeshCore repeater admin
interfaces and reports what it finds.

The Grid is the S.I.E.R.R.A data service at `data.sierragridteam.org`. Almost all
of it is public and read-only. This is the one endpoint that accepts data, and it
needs a token — you should have been given one out of band. If you have not, ask
whoever maintains the service; tokens are issued per reporter.

**What your reports add.** The Grid otherwise learns about mesh nodes from
community MQTT bridges, which only ever see what a node broadcasts. Your monitor
logs into the node, so it can report things nothing broadcasts — battery,
temperature, airtime, packet counters — and, uniquely, it can report that it
*tried* to reach a node and could not. A mesh has no goodbye packet, so silence
is ambiguous and everything downstream has to treat it that way. A failed login
attempt is not ambiguous. That is the most valuable thing you send.

---

## The request

```
POST https://data.sierragridteam.org/api/v1/ingest/mesh.repeater
```

| | |
|---|---|
| **Method** | `POST` (anything else gets `405`) |
| **Scheme** | HTTPS only. The token is a bearer credential — plain HTTP hands it to anyone on the path. |
| **`Authorization`** | `Bearer <your token>` — **required** |
| **`Content-Type`** | `application/json` — send it. The server does not currently reject a missing one, but don't build on that. |
| **Body** | One JSON document, described below |
| **Max body** | 1 MB |
| **Max repeaters per report** | 1000 |

A minimal working call:

```bash
curl -X POST https://data.sierragridteam.org/api/v1/ingest/mesh.repeater \
  -H "Authorization: Bearer $GRID_TOKEN" \
  -H 'Content-Type: application/json' \
  --data @report.json
```

Your token identifies you. Nothing in the body says who you are, and anything
that claimed to would be ignored.

---

## The payload

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-15T00:23:32+00:00",
  "repeaters": [
    {
      "id": "de0715314cfa9b5e",
      "name": "SIERRA Arnold Summit",
      "last_attempt": 1789431672,
      "last_success": 1789431672,

      "battery_voltage": 4.14,
      "battery_percent": 97.0,
      "battery_percent_source": "estimated",
      "temperature_c": 37.0,
      "humidity": null,
      "pressure": null,

      "uptime_s": 2615083,
      "airtime_ms": 49646,
      "rx_airtime_ms": 211238,
      "noise_floor_dbm": -116,
      "last_rssi_dbm": -88,
      "last_snr_db": 12.5,
      "tx_queue_len": 0,

      "nb_sent": 150305,
      "nb_recv": 649331,
      "sent_flood": 149984,
      "sent_direct": 321,
      "recv_flood": 642043,
      "recv_direct": 6702,
      "direct_dups": 53,
      "flood_dups": 30331,
      "full_evts": 0,
      "recv_errors": 240197,

      "online": true
    },
    {
      "id": "6781a18b2b47cb4e",
      "name": "SIERRA Lilac Park",
      "last_attempt": 1789421128,
      "last_success": null,
      "battery_voltage": null,
      "battery_percent": null,
      "temperature_c": null,
      "uptime_s": null,
      "nb_sent": null,
      "online": false
    }
  ]
}
```

The second entry is the important shape to get right: a node your monitor tried
and could not reach. Everything it could not read is `null`, including
`last_success`. Send it anyway — see [the four rules](#four-rules-that-matter).

### Envelope

| Field | Type | Notes |
|---|---|---|
| `schema_version` | integer | Must be `1`. Anything else is rejected with `400`. |
| `generated_at` | RFC 3339 string | When you built this report. |
| `repeaters` | array | Your complete current set. May be empty, but see rule 1. |

### Per repeater

**Identity and liveness**

| Field | Type | Notes |
|---|---|---|
| `id` | hex string | The node's public key, **or a prefix of it**. 8–64 hex characters, even length, case-insensitive. Prefer the full 64-character key if you have it — see [node ids](#node-ids). |
| `name` | string | Whatever the node calls itself. Optional. |
| `last_attempt` | unix seconds \| `null` | When you last *tried* this node, successfully or not. |
| `last_success` | unix seconds \| `null` | When you last *succeeded*. `null` means never. |

**Gauges** — current readings. Send `null` for anything the node did not report.

| Field | Type | Unit |
|---|---|---|
| `battery_voltage` | number | volts |
| `battery_percent` | number | 0–100 |
| `battery_percent_source` | string | `"measured"` or `"estimated"` — say which, so a UI can too |
| `temperature_c` | number | °C |
| `humidity` | number | % |
| `pressure` | number | hPa |
| `noise_floor_dbm` | integer | dBm |
| `last_rssi_dbm` | integer | dBm, as the node measured it |
| `last_snr_db` | number | dB, as the node measured it |
| `tx_queue_len` | integer | packets queued |

**Counters** — lifetime since the node booted.

| Field | Type |
|---|---|
| `uptime_s` | integer (seconds) |
| `airtime_ms`, `rx_airtime_ms` | integer (ms) |
| `nb_sent`, `nb_recv` | integer |
| `sent_flood`, `sent_direct` | integer |
| `recv_flood`, `recv_direct` | integer |
| `direct_dups`, `flood_dups` | integer |
| `full_evts` | integer (queue overflows) |
| `recv_errors` | integer |

**`online`** — your own boolean. Accepted, but **not** what determines whether
the Grid calls a node reachable; `last_success` is. See
[reachability](#how-reachability-is-decided).

Fields we don't recognize are ignored, so you can add your own without
coordinating a release.

---

## Four rules that matter

### 1. Every report is your complete current set

Send every node you monitor, every time. A node missing from a report is read as
*no longer monitored*, and after a grace period its record is retired.

This is not a quirk — it is the only way absence can mean anything. If reports
were merged instead of replaced, a node would become immortal the moment your
monitor stopped listing it, and nobody could ever tell the difference between
"still there" and "we stopped looking."

If your monitor can only report a subset — say it batches by site — talk to us
before sending partial reports. Don't send them and hope.

### 2. Unread values are `null`, never `0`

A `null` is published as "unknown". A `0` is published as a measurement.

A repeater reporting `0%` battery and a repeater whose battery could not be read
must not look the same to someone deciding whether to drive up a mountain in
February. If your client library defaults missing numbers to zero, override it.

### 3. A node you have never reached carries no readings

Set `last_success: null` and leave the metrics `null`. The Grid will publish the
node with **no telemetry block at all**, rather than a row of zeroed counters
claiming it has sent and received nothing.

Keep reporting it. "I am watching this node and cannot reach it" is information;
dropping it from the report throws that information away.

### 4. Timestamps are unix seconds, and clocks are not trusted

`last_attempt` and `last_success` are integer unix seconds (UTC).
`generated_at` is RFC 3339.

Any timestamp in the future is clamped to server time on arrival. Mesh
deployments have a long history of clock skew, and a future timestamp that
survived would keep a node looking alive past any grace period. Keep your
monitor's clock synced (NTP) and this never comes up.

---

## Node ids

Your monitor probably knows a node by a prefix of its public key — 16 hex
characters is common. That works: the Grid matches the prefix against the nodes
it knows about, and a match merges your report into that node's existing record
so it stays one node, not two.

Two things follow:

- **Send the full 64-character public key if you can get it.** A prefix only
  resolves when exactly one known node carries it. If two do, it resolves to
  neither — attaching your battery reading to the wrong repeater is worse than
  leaving it unattached.
- **A node we only know by prefix has a provisional id.** Once an MQTT bridge
  hears it and supplies the full key, its id changes and the provisional record
  is retired. If you build anything that stores Grid event ids, don't assume
  they're permanent for a node that has never been heard on the mesh backbone.

Ids are case-insensitive; we lowercase them.

---

## How reachability is decided

The Grid publishes each monitored node as `REACHABLE` or `UNREACHABLE`. It
derives that from **how old `last_success` is**, not from your `online` flag.

- Last success within 45 minutes → `REACHABLE`
- Older than that, or never → `UNREACHABLE`
- No monitor watching the node at all → neither (`MESH_REACHABILITY_UNSPECIFIED`
  on the events API, omitted on the map layer)

A per-poll boolean flips constantly on a marginal repeater, and this field is
part of the node's permanent record — every flip would be a history entry, and
the real signal ("Lilac Park has been down since Tuesday") would be buried in
noise. An age threshold has hysteresis built in: one missed poll changes nothing,
a sustained outage registers exactly once.

**What this asks of you:** keep attempting every node on every cycle and report
`last_attempt` and `last_success` accurately. That is the whole input.

---

## How often to report

**Every 5 minutes is a good default.** The constraints:

| | |
|---|---|
| **Minimum gap** | 30 seconds between accepted reports (may differ for your token — ask). Faster gets `429`. |
| **Go-quiet threshold** | 30 minutes. Past this your reporter is flagged degraded on the public sources board. |
| **Reachability window** | 45 minutes, so a cadence slower than that can't distinguish a down node from a slow monitor. |

The rate limit is measured from your last **accepted** report, so a rejected
request never locks you out of retrying.

**If you stop reporting**, the Grid does not start retiring your nodes
immediately. It treats your silence as *our* missing information, not evidence
that the nodes left — so their records are held rather than expired. That
protection lasts about two hours at the default settings, because a monitor that
never comes back must not freeze the record permanently. Planned downtime longer
than that is worth a heads-up.

---

## Responses

Success is **`202 Accepted`**, not `200`:

```json
{
  "reporterId": "your-id",
  "stream": "mesh.repeater",
  "accepted": 9,
  "receivedAt": "2026-09-15T00:23:35Z"
}
```

`202` means validated and queued, not yet stored. The data lands on the next
ingest cycle, within about a minute.

If some entries were skipped, the response carries `warnings`:

```json
{
  "reporterId": "your-id", "stream": "mesh.repeater", "accepted": 8,
  "warnings": ["repeaters[3]: id \"nothex!!\" is not 8-64 hex characters; skipped"],
  "receivedAt": "2026-09-15T00:23:35Z"
}
```

**A bad entry does not sink the report** — one repeater with a malformed id
should not cost you the other eight. But warnings mean something is wrong in your
output, so log them.

### Errors

Errors are `{"code": <number>, "message": "..."}`.

| Status | Meaning | What to do |
|---|---|---|
| `400` | Malformed JSON, wrong `schema_version`, or too many repeaters | Fix the output. Retrying unchanged won't help. |
| `401` | Missing or unrecognized token | Check the `Authorization` header format. If it looks right, the token may have been rotated — ask. |
| `403` | Token is valid but not authorized for this stream | Ask; it's a config fix on our end. |
| `404` | Unknown stream in the URL | Check the path spelling: `mesh.repeater`, singular. |
| `413` | Body over 1 MB | Reduce the report. |
| `429` | Reporting too fast | Honour the `Retry-After` header (seconds). |
| `5xx` | Our problem | Retry with backoff. |

**Retries are safe.** Entries are keyed by node, so resending a report overwrites
rather than duplicates. If a request times out and you aren't sure it landed,
send it again.

Recommended: retry `429` and `5xx` with exponential backoff; do not retry `400`,
`401`, `403`, `404`, or `413` without changing something.

---

## Checking it worked

Both are public — no token needed.

**Your reporter's health:**

```bash
curl -s https://data.sierragridteam.org/api/v1/sources | jq '.sources[] | select(.id=="your-id")'
```

```json
{
  "id": "your-id",
  "name": "Your monitor",
  "lastSuccessAt": "2026-09-15T00:23:35Z",
  "status": "OK"
}
```

`OK` means we heard from you recently. `STALE` or `UNAVAILABLE` means we haven't.

**Your nodes:**

```bash
curl -s 'https://data.sierragridteam.org/api/v1/events?layer=mesh' \
  | jq '.events[] | {name: .mesh.name, reachability: .mesh.reachability,
                     battery: .mesh.telemetry.admin.batteryPercent}'
```

Give it a minute after posting — `202` means queued, not stored.

---

## Looking after the token

- **Treat it like a password.** It is the only thing proving a report is yours.
- **Keep it out of your repository**, out of shell history, and out of logs. An
  environment variable from a `0600` file, or a systemd credential, is plenty.
- **We cannot recover it.** Only a hash of it is stored on our side — that's what
  lets the service configuration live in a public repository. If you lose it, we
  rotate; we can't look it up.
- **If it leaks, say so.** Rotation takes a minute and costs nothing. A leaked
  token lets someone publish false readings about your equipment.

Rotating issues a new token and stops the old one at the next restart, so collect
the new one before the changeover.

---

## A worked example

A monitor that reads your repeaters and posts once every 5 minutes:

```python
#!/usr/bin/env python3
"""Post a MeshCore repeater report to The Grid."""
import json, os, sys, time, urllib.error, urllib.request

ENDPOINT = "https://data.sierragridteam.org/api/v1/ingest/mesh.repeater"
TOKEN = os.environ["GRID_TOKEN"]          # never hard-code this


def read_repeater(node):
    """Return one repeater entry. Every unread value MUST be None, not 0."""
    entry = {
        "id": node.pubkey,                 # full key preferred, prefix accepted
        "name": node.name,
        "last_attempt": int(time.time()),
        "last_success": None,
        "battery_voltage": None, "battery_percent": None,
        "battery_percent_source": None, "temperature_c": None,
        "uptime_s": None, "airtime_ms": None, "nb_sent": None, "nb_recv": None,
        "online": False,
    }
    try:
        s = node.admin_status()            # your transport
    except Exception:
        return entry                       # unreachable: nulls, and that is the point
    entry.update({
        "last_success": int(time.time()),
        "battery_voltage": s.battery_v,
        "battery_percent": s.battery_pct,
        "battery_percent_source": "estimated",
        "temperature_c": s.temp_c,
        "uptime_s": s.uptime_s,
        "airtime_ms": s.airtime_ms,
        "nb_sent": s.sent, "nb_recv": s.recv,
        "online": True,
    })
    return entry


def post(report):
    req = urllib.request.Request(
        ENDPOINT,
        data=json.dumps(report).encode(),
        headers={"Authorization": f"Bearer {TOKEN}",
                 "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            body = json.load(r)
            for w in body.get("warnings", []):
                print(f"warning: {w}", file=sys.stderr)
            print(f"accepted {body['accepted']} repeaters")
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")
        print(f"rejected: HTTP {e.code} {detail}", file=sys.stderr)
        if e.code == 429:
            print(f"retry after {e.headers.get('Retry-After')}s", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    post({
        "schema_version": 1,
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S+00:00", time.gmtime()),
        # EVERY node you monitor, every time — reachable or not.
        "repeaters": [read_repeater(n) for n in my_repeaters()],
    })
```

Run it from cron or a systemd timer:

```
*/5 * * * * GRID_TOKEN=$(cat /etc/grid/token) /usr/local/bin/grid-report.py
```

---

## Questions

The public API reference is at <https://data.sierragridteam.org/docs>, and the
endpoint is documented there under **Push ingest**. For anything about your
token, your reporting cadence, or reporting something other than MeshCore
repeaters, ask the service maintainer — new kinds of report are a small
configuration change on our side.
