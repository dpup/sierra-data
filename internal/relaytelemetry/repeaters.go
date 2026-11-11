package relaytelemetry

// Shared repeaters: hosts (and admins) give SIERRA a repeater's password
// (`meshbridge repeater add`), and the relay keeps it sealed in its registry.
// The poller logs in to each one over the mesh through a gateway whose node
// has heard it, reads its status and telemetry, and reports it with the
// gateway nodes. The schedule (cycles, retries, failover, hibernation) is in
// schedule.go.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/dpup/prefab/logging"

	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// Repeaters is what the poller needs from the relay's repeater registry:
// *meshrelay.Relay.
type Repeaters interface {
	RepeaterCredentials() ([]meshrelay.RepeaterCredential, error)
	RecordRepeaterPoll(pubkey, gateway string, err error)
	SetRepeaterPollState(pubkey string, st meshrelay.RepeaterPollState)
	CheckRepeater(context.Context, meshrelay.CheckRequest) (meshrelay.RepeaterCheck, error)
}

// skipErr is a refusal that says nothing about the repeater: the gateway is
// busy, paused, offline or the relay is stopping.
func skipErr(err error) bool {
	var ue *meshrelay.UnavailableError
	return errors.Is(err, meshrelay.ErrBusy) || errors.As(err, &ue) || errors.Is(err, meshrelay.ErrLeaseEnded)
}

func sampleFromCheck(r meshrelay.RepeaterCheck) *Sample {
	s := &Sample{}
	if st := r.Status; st != nil {
		if st.BatteryMilliVolts != nil {
			s.BatteryVoltage = ptr(float64(*st.BatteryMilliVolts) / 1000)
		}
		s.UptimeS, s.NbSent, s.NbRecv = st.UptimeS, st.PacketsSent, st.PacketsRecv
		s.SentFlood, s.SentDirect, s.RecvFlood, s.RecvDirect = st.SentFlood, st.SentDirect, st.RecvFlood, st.RecvDirect
		s.RecvErrors, s.LastSNRdB = st.RecvErrors, st.LastSNRdB
		s.TxQueueLen, s.NoiseFloorDBm, s.LastRSSIdBm = i32(st.TxQueueLen), i32(st.NoiseFloorDBm), i32(st.LastRSSIdBm)
		s.DirectDups, s.FloodDups = i64(st.DirectDups), i64(st.FloodDups)
		if st.AirtimeS != nil {
			s.AirtimeMs = ptr(*st.AirtimeS * 1000)
		}
		if st.RxAirtimeS != nil {
			s.RxAirtimeMs = ptr(*st.RxAirtimeS * 1000)
		}
	}
	if t := r.Telemetry; t != nil {
		s.TemperatureC = t.TemperatureC
		if s.BatteryVoltage == nil {
			s.BatteryVoltage = t.Voltage
		}
	}
	return s
}

func i32(v *int) *int32 {
	if v == nil {
		return nil
	}
	return ptr(int32(*v))
}

func i64(v *int) *int64 {
	if v == nil {
		return nil
	}
	return ptr(int64(*v))
}

// payloadAdvert is MeshCore's ADVERT payload type (header bits 2-5).
const payloadAdvert = 4

// pushAdverts forwards shared repeaters' signed adverts, as the gateway nodes
// stored them, to the Grid's mesh.packet stream, which verifies each
// signature. That establishes a repeater on an island whose adverts never
// reach a broker. Only an advert the Grid hasn't accepted yet is sent: the
// Grid stamps packets with its own receive time, so re-sending an old one
// would read as fresh presence. Presence between adverts comes from the
// repeater report's successful logins instead.
//
// Gateway nodes' own adverts go out the same way, so the Grid can establish
// a gateway node no broker hears. Each is exported fresh from a successful
// read, so it is true presence; it is still sent only when its content
// (role, location to about 100 m, name) changed or the last one is
// SelfAdvertResend old.
func (p *Poller) pushAdverts(ctx context.Context) {
	type pending struct {
		st  *nodeState
		raw []byte
	}
	var out []pending
	now := p.cfg.Now()
	p.mu.Lock()
	for _, st := range p.repeaters {
		if len(st.advert) > 0 && st.advert[0]>>2&0x0f == payloadAdvert && !bytes.Equal(st.advert, st.advertSent) {
			out = append(out, pending{st, st.advert})
		}
	}
	for _, st := range p.nodes {
		if st.advert == nil {
			continue
		}
		cur, sent := selfAdvertOf(st.advert, st.pubkey), selfAdvertOf(st.advertSent, st.pubkey)
		same := st.advertSent != nil && (cur == nil) == (sent == nil) && selfAdvertKey(cur) == selfAdvertKey(sent)
		if !same || now.Sub(st.advertSentAt) >= p.cfg.SelfAdvertResend {
			out = append(out, pending{st, st.advert})
		}
	}
	p.mu.Unlock()
	if len(out) == 0 {
		return
	}
	type packet struct {
		PacketType int    `json:"packet_type"`
		Raw        string `json:"raw"`
	}
	body := struct {
		SchemaVersion int      `json:"schema_version"`
		GeneratedAt   string   `json:"generated_at"`
		Packets       []packet `json:"packets"`
	}{SchemaVersion: 1, GeneratedAt: p.cfg.Now().UTC().Format(time.RFC3339)}
	for _, o := range out {
		body.Packets = append(body.Packets, packet{PacketType: payloadAdvert, Raw: hex.EncodeToString(o.raw)})
	}
	// No observer: the gateway nodes that stored these stay unnamed (the
	// relay's public status never ties a node to its gateway); the Grid
	// attributes them to this reporter.
	if !p.post(ctx, "mesh.packet", body) {
		return
	}
	p.mu.Lock()
	for _, o := range out {
		o.st.advertSent, o.st.advertSentAt = o.raw, now
	}
	p.mu.Unlock()
	logging.Infow(ctx, "Relay telemetry: forwarded node adverts", "count", len(out))
}
