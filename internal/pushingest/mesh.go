package pushingest

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MeshStream is the stream name for operator-monitored MeshCore repeaters:
// POST /api/v1/ingest/mesh.repeater.
const MeshStream = "mesh.repeater"

// meshSchemaVersion is the payload contract this handler understands. The
// version lives in the BODY rather than the URL so a reporter's document is
// self-describing wherever it ends up (a file, a queue, a log) — the stream name
// says what kind of thing it is, the schema version says which revision of it.
const meshSchemaVersion = 1

// Bounds on an individual node id. A MeshCore public key is 32 bytes (64 hex);
// monitors commonly identify a node by a prefix of it (Alan's reports use 8
// bytes / 16 hex). Anything shorter than 4 bytes is too collision-prone to
// resolve to a node with any confidence, so it is rejected outright rather than
// silently matched against the wrong repeater.
const (
	minNodeIDHex = 8
	maxNodeIDHex = 64
)

// MeshNodeReport is one monitor's view of one node, normalized and
// clock-clamped. It is the unit the mesh normalizer merges with MQTT presence.
type MeshNodeReport struct {
	ReporterID string
	Priority   int
	PlaceIDs   []string

	// NodeID is the reported identifier, lowercased hex — a FULL public key or,
	// far more often, a prefix of one. Resolving a prefix to the full key is the
	// normalizer's job (it owns the node catalog); this package never guesses.
	NodeID string
	Name   string

	// ReceivedAt is our clock: when the report was accepted. It is the ceiling
	// for every reporter-supplied timestamp below, so a monitor with a skewed
	// clock can never claim to have seen something in the future.
	ReceivedAt  time.Time
	ReportedAt  time.Time // report generation time (generated_at)
	LastAttempt time.Time // monitor's last attempt on this node
	LastSuccess time.Time // monitor's last successful read; zero if never

	// HasSample is true when LastSuccess is set and the report carried real
	// metrics. A node the monitor has never reached carries NO sample at all
	// rather than a row of zeroes — "never read" and "read as zero" are
	// different facts and a counter of 0 would assert the second.
	HasSample bool
	Telemetry AdminTelemetry
}

// AdminTelemetry is one sample read off the node's admin interface. Gauges are
// pointers so a metric the device did not report (a repeater with no humidity
// sensor) stays absent instead of becoming a plausible-looking zero. Counters
// are plain: they are only ever populated alongside HasSample, where zero is a
// real reading.
type AdminTelemetry struct {
	BatteryVolts         *float64
	BatteryPercent       *float64
	BatteryPercentSource string
	TemperatureC         *float64
	Humidity             *float64
	Pressure             *float64
	NoiseFloorDBm        *int32
	LastSNRdB            *float64
	LastRSSIdBm          *int32
	TxQueueLen           *int32

	UptimeSeconds   int64
	AirtimeMs       int64
	RxAirtimeMs     int64
	PacketsSent     int64
	PacketsReceived int64
	SentFlood       int64
	SentDirect      int64
	RecvFlood       int64
	RecvDirect      int64
	DirectDups      int64
	FloodDups       int64
	FullEvents      int64
	RecvErrors      int64
}

// MeshSnapshot is what the normalizer reads each tick.
type MeshSnapshot struct {
	// Reports holds the buffered reports from reporters that are currently OK.
	// A STALE or DEAD reporter's reports are deliberately EXCLUDED: replaying a
	// silent monitor's last set would refresh last_seen_at and fabricate liveness
	// for nodes nobody has checked on in hours. Absence here is honest; what must
	// not follow from it is a disappearance verdict, which is what SuppressSweep
	// below prevents.
	Reports []MeshNodeReport
	// Reporters is every configured mesh reporter's health, for the source rows.
	Reporters []ReporterHealth
	// SuppressSweep is true while any reporter is STALE — silent, but not long
	// enough to give up on. See reporterDeadMultiple.
	SuppressSweep bool
	// Live counts reporters currently OK. Zero means push contributes nothing
	// this tick, which (with no MQTT broker connected) is what makes the poll
	// fail loud rather than report an empty mesh.
	Live int
}

// meshPayload is the wire contract, matching the document an operator monitor
// already generates. Every numeric is a pointer because the source emits JSON
// null for "I could not read this" — and null must not decode to zero.
type meshPayload struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	Repeaters     []meshRepeaterIn `json:"repeaters"`
}

type meshRepeaterIn struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	LastAttempt *int64 `json:"last_attempt"`
	LastSuccess *int64 `json:"last_success"`

	BatteryVoltage       *float64 `json:"battery_voltage"`
	BatteryPercent       *float64 `json:"battery_percent"`
	BatteryPercentSource string   `json:"battery_percent_source"`
	TemperatureC         *float64 `json:"temperature_c"`
	Humidity             *float64 `json:"humidity"`
	Pressure             *float64 `json:"pressure"`
	UptimeS              *int64   `json:"uptime_s"`
	AirtimeMs            *int64   `json:"airtime_ms"`
	RxAirtimeMs          *int64   `json:"rx_airtime_ms"`
	NoiseFloorDBm        *int32   `json:"noise_floor_dbm"`
	LastRSSIdBm          *int32   `json:"last_rssi_dbm"`
	LastSNRdB            *float64 `json:"last_snr_db"`
	TxQueueLen           *int32   `json:"tx_queue_len"`
	NbSent               *int64   `json:"nb_sent"`
	NbRecv               *int64   `json:"nb_recv"`
	SentFlood            *int64   `json:"sent_flood"`
	SentDirect           *int64   `json:"sent_direct"`
	RecvFlood            *int64   `json:"recv_flood"`
	RecvDirect           *int64   `json:"recv_direct"`
	DirectDups           *int64   `json:"direct_dups"`
	FloodDups            *int64   `json:"flood_dups"`
	FullEvts             *int64   `json:"full_evts"`
	RecvErrors           *int64   `json:"recv_errors"`

	// Online is the monitor's own boolean. We deliberately do NOT use it for the
	// hashed reachability field: it flips per poll on a marginal repeater, and
	// every flip would mint a revision. Reachability is derived from
	// last_success's age instead, which has hysteresis built in. Kept in the
	// contract because rejecting a field the reporter already sends would be
	// gratuitous, and it is useful in logs.
	Online *bool `json:"online"`
}

// ingestMesh parses, validates and buffers one mesh.repeater report. It returns
// the number of accepted and rejected nodes plus per-node warnings; a malformed
// ENVELOPE is an error (the whole request is rejected), while a malformed NODE
// is a warning (the rest of the report still lands). A monitor that adds a
// repeater with a typo'd id should not lose the other eight.
func (r *Registry) ingestMesh(rep *reporter, body []byte, maxItems int) (accepted int, warnings []string, err error) {
	payload, err := decodeJSON[meshPayload](body)
	if err != nil {
		return 0, nil, err
	}
	if payload.SchemaVersion != meshSchemaVersion {
		return 0, nil, fmt.Errorf("unsupported schema_version %d (want %d)",
			payload.SchemaVersion, meshSchemaVersion)
	}
	if len(payload.Repeaters) > maxItems {
		return 0, nil, fmt.Errorf("report carries %d repeaters, limit is %d",
			len(payload.Repeaters), maxItems)
	}

	now := r.now()
	generatedAt := clampPast(parseRFC3339(payload.GeneratedAt), now)

	set := make(map[string]MeshNodeReport, len(payload.Repeaters))
	for i, in := range payload.Repeaters {
		id := strings.ToLower(strings.TrimSpace(in.ID))
		if !validNodeID(id) {
			warnings = append(warnings, fmt.Sprintf(
				"repeaters[%d]: id %q is not %d-%d hex characters; skipped",
				i, in.ID, minNodeIDHex, maxNodeIDHex))
			continue
		}
		if _, dup := set[id]; dup {
			warnings = append(warnings, fmt.Sprintf("repeaters[%d]: duplicate id %q; keeping the first", i, id))
			continue
		}

		nr := MeshNodeReport{
			ReporterID:  rep.cfg.ID,
			Priority:    rep.cfg.Priority,
			PlaceIDs:    rep.cfg.PlaceIDs,
			NodeID:      id,
			Name:        strings.TrimSpace(in.Name),
			ReceivedAt:  now,
			ReportedAt:  generatedAt,
			LastAttempt: clampPast(unixOrZero(in.LastAttempt), now),
			LastSuccess: clampPast(unixOrZero(in.LastSuccess), now),
		}
		// A sample exists only if the monitor actually got a reading. Without
		// this gate a never-reached node (every metric null) would be published
		// with zeroed counters, asserting that it has sent and received nothing.
		if !nr.LastSuccess.IsZero() && hasAnyMetric(in) {
			nr.HasSample = true
			nr.Telemetry = AdminTelemetry{
				BatteryVolts:         in.BatteryVoltage,
				BatteryPercent:       in.BatteryPercent,
				BatteryPercentSource: strings.TrimSpace(in.BatteryPercentSource),
				TemperatureC:         in.TemperatureC,
				Humidity:             in.Humidity,
				Pressure:             in.Pressure,
				NoiseFloorDBm:        in.NoiseFloorDBm,
				LastSNRdB:            in.LastSNRdB,
				LastRSSIdBm:          in.LastRSSIdBm,
				TxQueueLen:           in.TxQueueLen,
				UptimeSeconds:        deref(in.UptimeS),
				AirtimeMs:            deref(in.AirtimeMs),
				RxAirtimeMs:          deref(in.RxAirtimeMs),
				PacketsSent:          deref(in.NbSent),
				PacketsReceived:      deref(in.NbRecv),
				SentFlood:            deref(in.SentFlood),
				SentDirect:           deref(in.SentDirect),
				RecvFlood:            deref(in.RecvFlood),
				RecvDirect:           deref(in.RecvDirect),
				DirectDups:           deref(in.DirectDups),
				FloodDups:            deref(in.FloodDups),
				FullEvents:           deref(in.FullEvts),
				RecvErrors:           deref(in.RecvErrors),
			}
		}
		set[id] = nr
	}

	r.mu.Lock()
	// REPLACE, never merge: a report is this reporter's complete current set, the
	// same contract PollResult.Events has with the disappearance sweep. Merging
	// would make a node immortal the moment a monitor stopped listing it.
	r.mesh[rep.cfg.ID] = set
	rep.lastAcceptedAt = now
	rep.lastError = ""
	rep.reports++
	r.mu.Unlock()

	return len(set), warnings, nil
}

// MeshReports returns the current snapshot for the mesh normalizer.
func (r *Registry) MeshReports() MeshSnapshot {
	if r == nil {
		return MeshSnapshot{}
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	var snap MeshSnapshot
	for _, rep := range r.order {
		if !rep.streams[MeshStream] {
			continue
		}
		h := r.healthLocked(rep, now)
		snap.Reporters = append(snap.Reporters, h)
		switch h.State {
		case ReporterOK:
			snap.Live++
			for _, nr := range r.mesh[rep.cfg.ID] {
				snap.Reports = append(snap.Reports, nr)
			}
		case ReporterStale:
			// Silent, but plausibly coming back: contribute nothing, and stop the
			// sweep from reading that silence as nodes having left.
			snap.SuppressSweep = true
		case ReporterDead, ReporterUnknown:
			// DEAD: silent long enough that we must admit we no longer know.
			// UNKNOWN: never configured up, so it has no nodes to protect.
		}
	}
	return snap
}

// validNodeID accepts an even-length lowercase hex string within the bounds a
// pubkey prefix can meaningfully carry.
func validNodeID(id string) bool {
	if len(id) < minNodeIDHex || len(id) > maxNodeIDHex || len(id)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func hasAnyMetric(in meshRepeaterIn) bool {
	return in.BatteryVoltage != nil || in.BatteryPercent != nil || in.TemperatureC != nil ||
		in.Humidity != nil || in.Pressure != nil || in.UptimeS != nil || in.AirtimeMs != nil ||
		in.RxAirtimeMs != nil || in.NoiseFloorDBm != nil || in.LastRSSIdBm != nil ||
		in.LastSNRdB != nil || in.TxQueueLen != nil || in.NbSent != nil || in.NbRecv != nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func unixOrZero(v *int64) time.Time {
	if v == nil || *v <= 0 {
		return time.Time{}
	}
	return time.Unix(*v, 0).UTC()
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// clampPast caps a reporter-supplied stamp at our own clock.
//
// A monitor's clock is not ours, and mesh deployments have a documented history
// of skew (the MQTT path already refuses to trust node-stamped advert times for
// exactly this reason). A future stamp that survived here would propagate into
// last_seen_at and keep a node alive past any grace, so the ceiling is applied
// once, at the boundary, rather than defended against everywhere downstream.
func clampPast(t, now time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	if t.After(now) {
		return now
	}
	return t
}
