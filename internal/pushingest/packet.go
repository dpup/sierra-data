package pushingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dpup/sierra-data/internal/clients/meshcore"
)

// MeshPacketStream is the stream name for forwarded over-the-air MeshCore
// packets: POST /api/v1/ingest/mesh.packet.
//
// It exists because the radios that hear the SIERRA backbone at zero hops are
// companions in Arnold and Dorrington, and some of what they hear reaches no
// MQTT observer at all. (The first measurement, "every few days per repeater",
// was taken during our own subscriber outage of 2026-10-03..09; through a
// delivering subscription the brokers carry most of the backbone. See "A deaf
// broker" in internal/ingest/CLAUDE.md.) The map at map.meshcore.io hears those
// adverts through a small uploader bot that reads a companion's received-packet
// log and POSTs each ADVERT frame. This stream accepts exactly that: the raw frame, in the envelope the MQTT bridges already publish, so the
// MeshCore registry ingests it through the same code and the same trust rule.
// The node signed the advert; the forwarder is only the courier, and the
// Ed25519 check (grid.meshcore.requireValidSignature) still applies.
const MeshPacketStream = "mesh.packet"

// packetSchemaVersion is the payload contract this handler understands.
const packetSchemaVersion = 1

// maxPacketWarnings bounds the per-packet warnings in one response. A
// forwarder relaying garbage should hear about it, not receive a thousand lines
// of it.
const maxPacketWarnings = 25

// packetSourcePrefix marks a registry source that is a push reporter rather
// than an MQTT broker URL, so provenance can name the reporter.
const packetSourcePrefix = "reporter:"

// PacketSource is the registry source id a reporter's packets are recorded
// under — the slot a broker URL occupies for an MQTT reception.
func PacketSource(reporterID string) string { return packetSourcePrefix + reporterID }

// ReporterFromPacketSource inverts PacketSource.
func ReporterFromPacketSource(source string) (string, bool) {
	if !strings.HasPrefix(source, packetSourcePrefix) {
		return "", false
	}
	return strings.TrimPrefix(source, packetSourcePrefix), true
}

// PacketSink is where forwarded packets go: the MeshCore registry (satisfied by
// *meshcore.Registry). The handler never decodes a frame itself — the registry
// owns advert decoding, signature policy, the spam floor and presence, and a
// second decoder here would be a second place for those rules to drift.
type PacketSink interface {
	IngestEnvelope(payload []byte, source, defaultGateway string) (meshcore.PacketOutcome, error)
}

// SetPacketSink wires the MeshCore registry in. Until it is set the stream
// answers 400 with a clear message; a reporter authorized for it on a server
// with MeshCore ingest off has nowhere for its packets to go, and silently
// accepting them would be the worst answer.
func (r *Registry) SetPacketSink(sink PacketSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.packets = sink
}

// packetPayload is the wire contract. Each packet is the bridge-style per-packet
// envelope (`packet_type`, hex `raw`, `SNR`, `RSSI`, `origin_id`, ...) passed to
// the registry verbatim, so a forwarder built from a bridge, or from the map's
// uploader, needs no re-shaping. `raw` may also be a `meshcore://<hex>` link.
type packetPayload struct {
	SchemaVersion int    `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	// Observer is the receiving radio's public key (hex, full or a prefix), used
	// as the gateway for every packet that carries no `origin_id` of its own.
	// Optional; the reporter id stands in when absent.
	Observer string            `json:"observer"`
	Packets  []json.RawMessage `json:"packets"`
}

// ingestPackets validates one mesh.packet report and hands each packet to the
// registry. A malformed ENVELOPE is an error (the whole request is rejected); a
// packet the registry could not use is a warning (the rest still land), and a
// packet of a type we do not ingest is silently fine — a forwarder relaying its
// companion's whole RX log is doing nothing wrong.
//
// `accepted` counts ADVERTs applied to the registry, not packets received, so a
// forwarder can see from the response whether what it sends is useful.
func (r *Registry) ingestPackets(_ context.Context, rep *reporter, body []byte, maxItems int) (accepted int, warnings []string, err error) {
	r.mu.Lock()
	sink := r.packets
	r.mu.Unlock()
	if sink == nil {
		return 0, nil, errors.New("mesh.packet is not enabled on this server: MeshCore ingest is off")
	}

	payload, err := decodeJSON[packetPayload](body)
	if err != nil {
		return 0, nil, err
	}
	if payload.SchemaVersion != packetSchemaVersion {
		return 0, nil, fmt.Errorf("unsupported schema_version %d (want %d)",
			payload.SchemaVersion, packetSchemaVersion)
	}
	if len(payload.Packets) > maxItems {
		return 0, nil, fmt.Errorf("report carries %d packets, limit is %d", len(payload.Packets), maxItems)
	}
	observer := strings.ToLower(strings.TrimSpace(payload.Observer))
	if observer != "" && !validNodeID(observer) {
		return 0, nil, fmt.Errorf("observer %q is not %d-%d hex characters", payload.Observer, minNodeIDHex, maxNodeIDHex)
	}

	source := PacketSource(rep.cfg.ID)
	gateway := observer
	if gateway == "" {
		gateway = rep.cfg.ID
	}

	dropped := 0
	for i, pk := range payload.Packets {
		outcome, perr := sink.IngestEnvelope(pk, source, gateway)
		switch outcome {
		case meshcore.PacketAccepted:
			accepted++
		case meshcore.PacketIgnored:
			// Not an advert. Fine.
		default:
			dropped++
			if len(warnings) < maxPacketWarnings {
				warnings = append(warnings, fmt.Sprintf("packets[%d]: %s: %v", i, outcome, perr))
			}
		}
	}
	if dropped > maxPacketWarnings {
		warnings = append(warnings, fmt.Sprintf("... and %d more packets dropped", dropped-maxPacketWarnings))
	}

	now := r.now()
	r.mu.Lock()
	rep.lastAcceptedAt = now
	rep.lastAcceptedByStream[MeshPacketStream] = now
	rep.lastError = ""
	rep.reports++
	r.mu.Unlock()

	return accepted, warnings, nil
}
