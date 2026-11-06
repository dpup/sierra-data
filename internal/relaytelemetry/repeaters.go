package relaytelemetry

// Shared repeaters: hosts (and admins) give SIERRA a repeater's password
// (`meshbridge repeater add`), and the relay keeps it sealed in its registry.
// Every RepeaterInterval the poller logs in to each one over the mesh,
// through a gateway whose node has heard it, reads its status and
// telemetry, and reports it with the gateway nodes.
//
// Airtime is shared by everyone on the mesh, so the poller is frugal:
//   - It only uses gateways whose node already has the repeater as a contact
//     (it heard its advert): the best cheap sign of being in range, and the
//     host's contacts are never added to. Only if none has, it tries the
//     gateway that last reached it, adding the contact for the check. (A
//     stored direct route that goes unanswered is reset and the check
//     floods once, as the apps' "Reset path" does: see CheckRepeater.)
//   - The gateway that last reached it goes first, then the one that shared
//     it, then the rest.
//   - At most maxMeshTries gateways actually transmit per repeater per cycle.
//   - A refused password stops the cycle for that repeater.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-meshrelay/meshops"
	meshrelay "github.com/dpup/sierra-meshrelay/relay"
)

// Repeaters is what the poller needs from the relay's repeater registry:
// *meshrelay.Relay.
type Repeaters interface {
	RepeaterCredentials() ([]meshrelay.RepeaterCredential, error)
	RecordRepeaterPoll(pubkey, gateway string, err error)
	CheckRepeater(context.Context, meshrelay.CheckRequest) (meshrelay.RepeaterCheck, error)
}

// maxMeshTries bounds gateways that transmit for one repeater in a cycle.
const maxMeshTries = 3

// pollRepeaters reads every shared repeater once.
func (p *Poller) pollRepeaters(ctx context.Context) {
	creds, err := p.reps.RepeaterCredentials()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing shared repeaters failed; skipping them this cycle", "error", err)
		return
	}
	recs, err := p.gws.Gateways()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing gateways failed; skipping repeaters this cycle", "error", err)
		return
	}
	var gateways []string
	for _, r := range recs {
		gateways = append(gateways, r.Name)
	}
	current := map[string]bool{}
	for _, c := range creds {
		if ctx.Err() != nil {
			return
		}
		current[c.Pubkey] = true
		p.pollRepeater(ctx, c, gateways)
	}
	// A repeater no longer shared leaves the report (and its readings).
	p.mu.Lock()
	for pk := range p.repeaters {
		if !current[pk] {
			delete(p.repeaters, pk)
		}
	}
	p.mu.Unlock()
}

// candidates orders the gateways to try for a repeater.
func candidates(c meshrelay.RepeaterCredential, gateways []string) []string {
	var out []string
	for _, g := range []string{c.LastGateway, c.AddedByGateway} {
		if g != "" && slices.Contains(gateways, g) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	for _, g := range gateways {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func (p *Poller) pollRepeater(ctx context.Context, c meshrelay.RepeaterCredential, gateways []string) {
	start := p.cfg.Now()
	var (
		res         meshrelay.RepeaterCheck
		via         string
		lastErr     error
		transmitted int // gateways that actually tried over the mesh
		ok          bool
	)
	check := func(gw string, onlyIfKnown bool) (stop bool) {
		r, err := p.reps.CheckRepeater(ctx, meshrelay.CheckRequest{Gateway: gw, Client: ClientName,
			Priority: meshrelay.PriorityBackground, Pubkey: c.Pubkey, Password: c.Password, OnlyIfKnown: onlyIfKnown})
		switch {
		case err == nil:
			res, via, lastErr, ok = r, gw, nil, true
			return true
		case errors.Is(err, meshrelay.ErrNotKnown) || skipErr(err):
			// No airtime spent, and nothing learned about the repeater.
			if lastErr == nil {
				lastErr = err
			}
			return false
		}
		transmitted++
		lastErr, via = err, gw
		// A refused password won't work through another gateway either.
		return errors.Is(err, meshops.ErrLoginFailed) || transmitted >= maxMeshTries
	}
	for _, gw := range candidates(c, gateways) {
		if check(gw, true) || ctx.Err() != nil {
			break
		}
	}
	if !ok && transmitted == 0 && c.LastGateway != "" && slices.Contains(gateways, c.LastGateway) && ctx.Err() == nil {
		// No node has it as a contact (an admin shared it through a node that
		// hadn't heard it): try the gateway that reached it last, adding it.
		check(c.LastGateway, false)
	}

	outcome := outcomeSkipped // busy, paused, or no node has heard it: says nothing about the repeater
	switch {
	case ok:
		outcome = outcomeOK
	case transmitted > 0:
		outcome = outcomeFailed
	}
	if outcome != outcomeSkipped || errors.Is(lastErr, meshrelay.ErrNotKnown) {
		p.reps.RecordRepeaterPoll(c.Pubkey, via, lastErr)
	}

	p.mu.Lock()
	st := p.repeaters[c.Pubkey]
	if st == nil {
		st = &nodeState{pubkey: c.Pubkey}
		p.repeaters[c.Pubkey] = st
	}
	st.name = c.Name
	switch outcome {
	case outcomeOK:
		st.lastAttempt, st.lastSuccess, st.sample = start, start, sampleFromCheck(res)
		if res.Name != "" {
			st.name = res.Name
		}
		if len(res.Advert) > 0 {
			st.advert = res.Advert
		}
	case outcomeFailed:
		st.lastAttempt = start
	}
	changed := st.outcome != outcome
	st.outcome = outcome
	p.mu.Unlock()

	kv := []any{"repeater", c.Pubkey, "name", c.Name, "outcome", outcome, "via", via, "tried", transmitted,
		"reason", meshrelay.RepeaterErrorText(lastErr), "duration", p.cfg.Now().Sub(start)}
	if changed {
		logging.Infow(ctx, "Relay telemetry repeater poll", kv...)
	} else {
		logging.Debugw(ctx, "Relay telemetry repeater poll", kv...)
	}
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

// repeaterDue reports whether the shared repeaters should be polled this
// cycle (every RepeaterInterval).
func (p *Poller) repeaterDue(now time.Time) bool {
	return p.reps != nil && (p.lastRepeaterPoll.IsZero() || now.Sub(p.lastRepeaterPoll) >= p.cfg.RepeaterInterval)
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
func (p *Poller) pushAdverts(ctx context.Context) {
	type pending struct {
		st  *nodeState
		raw []byte
	}
	var out []pending
	p.mu.Lock()
	for _, st := range p.repeaters {
		if len(st.advert) > 0 && st.advert[0]>>2&0x0f == payloadAdvert && !bytes.Equal(st.advert, st.advertSent) {
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
		o.st.advertSent = o.raw
	}
	p.mu.Unlock()
	logging.Infow(ctx, "Relay telemetry: forwarded repeater adverts", "count", len(out))
}
