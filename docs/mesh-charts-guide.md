# Charting mesh repeaters

A handoff for whoever builds the battery / temperature / signal charts against
The Grid (`data.sierragridteam.org`). Everything here is verified against the
running service, not written from memory.

The Grid now keeps **every** operator-monitor reading instead of only the latest
one, so a node's telemetry can finally be drawn as a line.

---

## Read this first: the archive starts empty

There is **no backfill**. The table is created on deploy and fills from the next
accepted report onward, so the first chart you draw will be a couple of points
and a lot of nothing. A day of history takes a day.

Build against the empty state first — it is the state the page will be in while
you are working on it, and it is a state real nodes reach anyway.

---

## The request

One node per call. `node` is the full 64-character public key.

```
GET /api/v1/mesh/telemetry?node=<pubkey>&from=<rfc3339>&to=<rfc3339>
```

| param  | meaning | default |
|--------|---------|---------|
| `node` | The node's public key, hex. **Required** — a cross-node dump is a different product. | — |
| `from` | Window start, RFC 3339. | `to` − 24h |
| `to`   | Window end. Windows wider than 400 days are clamped. | now |

No key required. CORS is open, GET only.

---

## The payload

One sample shown; a day at 15-minute cadence is 96 of them.

```jsonc
{
  "node": "de0715314cfa9b5e8f2c1d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b",
  "from": "2026-09-15T00:00:00Z",
  "to": "2026-09-16T00:00:00Z",
  "coverage": {                      // what the ARCHIVE holds, ignoring your window
    "from": "2026-08-20T11:00:00Z",
    "to": "2026-09-15T21:45:00Z",
    "samples": 2461
  },
  "cadenceSeconds": "900",            // STRING. null with < 2 samples
  "reboots": ["2026-09-15T04:12:00Z"],  // counters restart here
  "truncated": false,
  "samples": [
    {
      "reading": {
        "reporterId": "alanpi",
        "reportedAt": "2026-09-15T21:45:00Z",   // the sample's own time
        "lastSuccessAt": "2026-09-15T21:45:00Z",
        "lastAttemptAt": "2026-09-15T21:45:00Z",
        "batteryVolts": 4.06,
        "batteryPercent": 93,
        "batteryPercentSource": "estimated",    // or "measured" — say which
        "temperatureC": 23.7,
        "humidity": null,                       // no sensor / not read
        "pressure": null,
        "noiseFloorDbm": -115,
        "lastSnrDb": 11.75,                     // node-measured, not gateway
        "lastRssiDbm": -88,
        "txQueueLen": 0,
        "uptimeSeconds": "2670364",             // int64s are STRINGS
        "airtimeMs": "50707",
        "rxAirtimeMs": "216450",
        "packetsSent": "153621",
        "packetsReceived": "666796",
        "sentFlood": "153176",  "sentDirect": "445",
        "recvFlood": "656323",  "recvDirect": "9876",
        "directDups": "174",    "floodDups": "41354",
        "fullEvents": "0",      "recvErrors": "245449"
      },
      "receivedAt": "2026-09-15T21:45:22Z"      // when the Grid first held it
    }
  ]
}
```

### Three things that will bite

**Every int64 is a JSON string.** Protobuf's JSON mapping: `"153621"`, and
`cadenceSeconds: "900"` too. `Number()` them before any arithmetic, or your
counters sort lexicographically.

**The reading is nested.** `samples[i].reading.batteryPercent`, not
`samples[i].batteryPercent`. It is the same message the node's event carries,
reused deliberately so the archive and the live record cannot drift apart.

**Two clocks per sample.** Plot against `reading.reportedAt` — the monitor's own
stamp, and the reading's time. `receivedAt` is when the Grid first held it:
useful for debugging a lagging monitor, wrong for an x-axis.

---

## Five things the chart must not claim

Each is a specific false statement a naive rendering makes.

1. **`null` is not zero.** A gauge the monitor could not read comes back `null`.
   A battery at 0% and a battery nobody could read must never look alike.
   → Skip the point. Never `|| 0`, never `Number(null)`.

2. **A gap is not a flat line.** Drawing straight through a silence asserts
   readings nobody took. `cadenceSeconds` is the observed median gap, so you know
   what abnormal looks like without hard-coding 15 minutes.
   → Break the path when Δt > ~2 × `cadenceSeconds`.

3. **Outside `coverage` is not a quiet node.** An empty window can mean the node
   said nothing, or that the window predates anything we hold.
   → Render "no data retained before `coverage.from`", not an empty plot.

4. **A reboot is not a collapse.** Every counter is lifetime-since-boot, so a
   restart sends packet totals and uptime back to zero. A line drawn across one
   plunges and climbs — a graph of arithmetic, not of the mesh.
   → Split counter series at each `reboots[]` time, and mark it.

5. **`truncated` is us, not the node.** It means the window held more samples
   than one response carries. A chart that just ends looks like a node that
   stopped reporting.
   → Say the series was cut, and narrow the window.

A correct rendering of a solar repeater over 24h: overnight discharge, recovery
after sunrise, a dashed marker at the reboot, and the line **stopping** at the
edge of a two-hour monitor outage and starting again on the other side. Mark the
reboot even on battery percentage (a gauge, which survives it) — the packet
counters on the same page do not, and a reader comparing them needs the same
landmark.

---

## What is worth plotting

Nine repeaters report today. Battery is the one an operator checks before
driving up a mountain in February; the rest are diagnostics.

| field | unit | notes |
|---|---|---|
| `batteryPercent` | % | The headline series. Pair with `batteryPercentSource` — an estimate derived from voltage is not a reading, and a UI showing 97% should say which it is. |
| `batteryVolts` | V | The honest one when percent is estimated. A flat 4.1 V through a cold night is the interesting shape. |
| `temperatureC` | °C | Enclosure temperature, not weather. Tracks the sun on the box. |
| `lastSnrDb` / `lastRssiDbm` | dB / dBm | **Node-measured.** Distinct from the gateway-reported pair on the node's event — same names, different listeners, never average them together. |
| `noiseFloorDbm` | dBm | A rising floor is local interference. Good companion to SNR. |
| `airtimeMs` / `rxAirtimeMs` | ms | Counters — plot as a rate (Δ per interval), reset at every reboot. |
| `packetsSent` / `packetsReceived` | count | Same. The flood/direct split underneath is where a misbehaving neighbour shows up. |
| `recvErrors` / `fullEvents` | count | Queue overflows and undecodable receptions. Rate, again. |

---

## Finding the nodes that have any of this

Eight of the Grid's 548 mesh nodes carry a monitor sample. The rest are known
only from community MQTT bridges and have no telemetry at all — a gap in
monitoring, not a broken node, and the page should say so rather than drawing an
empty chart.

```js
// every mesh node, then keep the monitored ones
GET /api/v1/events?layer=mesh&page_size=200

// a node with a chart worth drawing:
event.mesh.telemetry.admin !== null

// a node someone watches, reachable or not:
event.mesh.reachability === "REACHABLE" || "UNREACHABLE"
// "MESH_REACHABILITY_UNSPECIFIED" means nobody is watching it —
// never render that as fine

// the key to pass as ?node=
event.mesh.publicKey
```

Related: `GET /api/v1/mesh/links` is the relay topology (a weighted edge list,
windowed), if the page wants a map beside the charts.

---

## Left undone

- **The Grid's own site has no chart.** The endpoint has no consumer yet;
  `web/` renders the mesh roster and map but nothing plots. Whichever site gets
  there first sets the pattern.
- **Road-incident headlines churn.** Unrelated to charts, live now: one closure
  has produced 18 different public headlines from a single unchanged upstream
  description, because AI-written fields sit inside the content hash. Worth
  knowing if the site shows road incidents.
- **No rollup tier.** Raw samples only, ~864 rows/day, kept a year. A month view
  is 2,880 points per node — fine to draw, but decimate client-side if it feels
  heavy.

---

See `CHANGELOG.md` (2026-09-15, evening) for the API entry, and
`internal/store/CLAUDE.md` for why the archive is shaped the way it is.
