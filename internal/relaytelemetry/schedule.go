package relaytelemetry

// The shared-repeater schedule. Airtime is shared by everyone on the mesh,
// so it is frugal, and mesh links come and go, so it expects failure.
//
// Each repeater has a cycle every RepeaterInterval (30m), at an offset from
// its pubkey so they don't all fire together. A cycle:
//
//   - Tries gateways in order: the primary (the one that last reached it),
//     the one that shared it, then the rest. Only gateways whose node has the
//     repeater as a contact (it heard its advert) are used; if none has, the
//     gateway that last reached it is tried with the contact added for the
//     check. At most maxMeshTries gateways transmit.
//   - On a gateway, a check that gets no answer is retried after 30s, 60s
//     and 120s (RepeaterRetries), then the next gateway is tried. Once a
//     link's route went unanswered, its retries flood at once (FloodFirst).
//     Retries are only for a repeater read in its last cycle: they catch a
//     lost packet on a working link. One that missed its last cycle gets a
//     single try per gateway, so a dead repeater costs at most
//     maxMeshTries floods per cycle.
//   - A busy, paused or offline gateway, or one whose node hasn't heard the
//     repeater, isn't a failure: the next gateway is tried at once. While
//     cycles end with no gateway able to try, the state says so
//     (WaitingSince, Waiting), so hosts see why it isn't being read.
//   - A refused password ends the cycle and stops polling until the share
//     is updated.
//
// A repeater not read for HibernateAfter cycles, and that nothing else
// reports alive (no advert, no other monitor's read, on the Grid, within
// those cycles), hibernates: one cycle a day, checked against the Grid each
// interval so it wakes as soon as something else hears it.
//
// The state is saved in the relay (SetRepeaterPollState), with the last
// reading, so a restart resumes the schedule rather than polling everything
// at once, and the report keeps each repeater's readings meanwhile.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-meshrelay/meshops"
	meshrelay "github.com/sierragridteam/sierra-meshrelay/relay"
)

// maxMeshTries bounds gateways that transmit for one repeater in a cycle.
const maxMeshTries = 3

// DefaultRepeaterRetries are the waits before each retry on one gateway.
var DefaultRepeaterRetries = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

// repeaterJob is one repeater's schedule.
type repeaterJob struct {
	pubkey string
	state  meshrelay.RepeaterPollState
	cycle  *repeaterCycle // nil between cycles
	// While hibernating: when to next ask the Grid whether it's alive.
	nextAliveCheck time.Time
}

// repeaterCycle is a cycle in progress.
type repeaterCycle struct {
	start      time.Time
	gateways   []string
	gi         int  // gateway being tried
	retry      int  // retries used on it
	retries    bool // retries allowed this cycle
	fallback   bool // trying the last gateway with the contact added
	triedFall  bool
	nextAt     time.Time
	tried      int // gateways that transmitted
	sent       bool
	unanswered map[string]bool // gateways whose route went unanswered
	lastErr    error
	via        string
	skipped    []string // "gateway: why" for gateways that couldn't try
}

// RepeaterTick runs whatever repeater work is due at the poller's clock:
// starting cycles, retries, and wake checks. Run calls it every few seconds.
func (p *Poller) RepeaterTick(ctx context.Context) {
	if p.reps == nil {
		return
	}
	creds, err := p.reps.RepeaterCredentials()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing shared repeaters failed", "error", err)
		return
	}
	recs, err := p.gws.Gateways()
	if err != nil {
		logging.Warnw(ctx, "Relay telemetry: listing gateways failed; repeaters wait", "error", err)
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
		p.tickRepeater(ctx, c, gateways)
	}
	// A repeater no longer shared leaves the schedule and the report.
	p.mu.Lock()
	for pk := range p.repeaters {
		if !current[pk] {
			delete(p.repeaters, pk)
		}
	}
	p.mu.Unlock()
	for pk := range p.jobs {
		if !current[pk] {
			delete(p.jobs, pk)
		}
	}
}

func (p *Poller) tickRepeater(ctx context.Context, c meshrelay.RepeaterCredential, gateways []string) {
	now := p.cfg.Now()
	j := p.jobs[c.Pubkey]
	if j == nil {
		j = &repeaterJob{pubkey: c.Pubkey, state: c.Poll}
		if j.state.NextCycle.IsZero() {
			// New (or never scheduled): first cycle at its offset.
			j.state.NextCycle = now.Add(p.offset(c.Pubkey))
		}
		p.jobs[c.Pubkey] = j
		p.restore(c)
	}
	// A refused password waits for the share to be updated.
	if r := j.state.RefusedAt; r != nil {
		if !c.UpdatedAt.After(*r) {
			return
		}
		j.state.RefusedAt, j.state.FailedCycles, j.state.NextCycle = nil, 0, now
	}
	if j.cycle == nil && j.state.HibernatingSince != nil && !now.Before(j.nextAliveCheck) && now.Before(j.state.NextCycle) {
		j.nextAliveCheck = now.Add(p.cfg.RepeaterInterval)
		if alive, err := p.alive(ctx, c.Pubkey, p.hibernateWindow()); err == nil && alive {
			logging.Infow(ctx, "Relay telemetry: hibernating repeater reported alive elsewhere; polling it again",
				"repeater", c.Pubkey, "name", c.Name)
			j.state.HibernatingSince, j.state.NextCycle = nil, now
		}
	}
	if j.cycle == nil {
		if now.Before(j.state.NextCycle) {
			return
		}
		j.cycle = &repeaterCycle{start: now, nextAt: now, gateways: candidates(c, gateways),
			retries: j.state.FailedCycles == 0 && j.state.HibernatingSince == nil, unanswered: map[string]bool{}}
	}
	for j.cycle != nil && !now.Before(j.cycle.nextAt) && ctx.Err() == nil {
		p.step(ctx, j, c, gateways)
		now = p.cfg.Now()
	}
}

// step makes the cycle's next attempt, or ends it.
func (p *Poller) step(ctx context.Context, j *repeaterJob, c meshrelay.RepeaterCredential, gateways []string) {
	cy := j.cycle
	now := p.cfg.Now()
	if cy.gi >= len(cy.gateways) || cy.tried >= maxMeshTries {
		if !cy.sent && !cy.triedFall && c.LastGateway != "" && slices.Contains(gateways, c.LastGateway) && cy.tried < maxMeshTries {
			// No node has it as a contact (an admin shared it through a node
			// that hadn't heard it, or the node dropped it): try the gateway
			// that reached it last, adding the contact.
			cy.gateways, cy.gi, cy.retry, cy.fallback, cy.triedFall = []string{c.LastGateway}, 0, 0, true, true
			return
		}
		p.endCycle(ctx, j, c, nil)
		return
	}
	gw := cy.gateways[cy.gi]
	res, err := p.reps.CheckRepeater(ctx, meshrelay.CheckRequest{Gateway: gw, Client: ClientName,
		Priority: meshrelay.PriorityBackground, Pubkey: c.Pubkey, Password: c.Password,
		OnlyIfKnown: !cy.fallback, FloodFirst: cy.unanswered[gw]})
	switch {
	case err == nil:
		cy.via, cy.lastErr = gw, nil
		p.endCycle(ctx, j, c, &res)
		return
	case errors.Is(err, meshrelay.ErrNotKnown) || skipErr(err):
		// No airtime spent, and nothing learned about the repeater.
		if cy.lastErr == nil {
			cy.lastErr = err
		}
		why := meshrelay.RepeaterErrorText(err)
		if errors.Is(err, meshrelay.ErrNotKnown) {
			why = "its node hasn't heard the repeater"
		}
		cy.skipped = append(cy.skipped, gw+": "+why)
		cy.gi, cy.retry, cy.nextAt = cy.gi+1, 0, now
		return
	}
	cy.sent, cy.lastErr, cy.via = true, err, gw
	if errors.Is(err, meshops.ErrLoginFailed) {
		// A refused password won't work through another gateway either.
		p.endCycle(ctx, j, c, nil)
		return
	}
	if errors.Is(err, meshops.ErrNoMeshReply) {
		cy.unanswered[gw] = true
	}
	if cy.retries && cy.retry < len(p.cfg.RepeaterRetries) {
		cy.nextAt = now.Add(p.cfg.RepeaterRetries[cy.retry])
		cy.retry++
		return
	}
	cy.tried++
	cy.gi, cy.retry, cy.nextAt = cy.gi+1, 0, now
}

// endCycle records a finished cycle and schedules the next. res is the
// reading when it succeeded.
func (p *Poller) endCycle(ctx context.Context, j *repeaterJob, c meshrelay.RepeaterCredential, res *meshrelay.RepeaterCheck) {
	cy := j.cycle
	j.cycle = nil
	now := p.cfg.Now()
	st := &j.state
	next := cy.start.Add(p.cfg.RepeaterInterval)
	if !next.After(now) {
		next = now.Add(p.cfg.RepeaterInterval)
	}
	outcome := outcomeSkipped // no gateway could try: says nothing about the repeater
	switch {
	case res != nil:
		outcome = outcomeOK
		st.Primary, st.FailedCycles, st.HibernatingSince = cy.via, 0, nil
		if b, err := json.Marshal(sampleFromCheck(*res)); err == nil {
			st.Reading = b
		}
	case errors.Is(cy.lastErr, meshops.ErrLoginFailed):
		outcome = outcomeFailed
		st.RefusedAt = ptr(now)
	case cy.sent:
		outcome = outcomeFailed
		st.FailedCycles++
		if st.HibernatingSince != nil || st.FailedCycles >= p.cfg.HibernateAfter {
			alive, err := p.alive(ctx, c.Pubkey, p.hibernateWindow())
			switch {
			case err != nil:
				logging.Warnw(ctx, "Relay telemetry: couldn't ask the Grid whether a repeater is alive; not hibernating it",
					"repeater", c.Pubkey, "error", err)
				st.HibernatingSince = nil
			case alive:
				st.HibernatingSince = nil
			case st.HibernatingSince == nil:
				st.HibernatingSince = ptr(now)
				logging.Infow(ctx, "Relay telemetry: repeater unreachable and not heard elsewhere; polling it daily",
					"repeater", c.Pubkey, "name", c.Name, "missedCycles", st.FailedCycles)
			}
			if st.HibernatingSince != nil {
				next = now.Add(p.cfg.HibernateInterval)
				j.nextAliveCheck = now.Add(p.cfg.RepeaterInterval)
			}
		}
	}
	// Waiting: cycles end with no gateway able to try. A cycle that went over
	// the mesh (or read it) ends it.
	if outcome == outcomeSkipped {
		if st.WaitingSince == nil {
			st.WaitingSince = ptr(cy.start)
		}
		st.Waiting = waitingText(cy.skipped)
	} else {
		st.WaitingSince, st.Waiting = nil, ""
	}
	st.NextCycle = next
	p.reps.SetRepeaterPollState(c.Pubkey, *st)
	if outcome != outcomeSkipped || errors.Is(cy.lastErr, meshrelay.ErrNotKnown) {
		p.reps.RecordRepeaterPoll(c.Pubkey, cy.via, cy.lastErr)
	}

	p.mu.Lock()
	ns := p.repeaters[c.Pubkey]
	if ns == nil {
		ns = &nodeState{pubkey: c.Pubkey}
		p.repeaters[c.Pubkey] = ns
	}
	ns.name = c.Name
	switch outcome {
	case outcomeOK:
		ns.lastAttempt, ns.lastSuccess, ns.sample = cy.start, cy.start, sampleFromCheck(*res)
		if res.Name != "" {
			ns.name = res.Name
		}
		if len(res.Advert) > 0 {
			ns.advert = res.Advert
		}
	case outcomeFailed:
		ns.lastAttempt = cy.start
	}
	changed := ns.outcome != outcome
	ns.outcome = outcome
	p.mu.Unlock()

	kv := []any{"repeater", c.Pubkey, "name", c.Name, "outcome", outcome, "via", cy.via,
		"reason", meshrelay.RepeaterErrorText(cy.lastErr), "missedCycles", st.FailedCycles,
		"hibernating", st.HibernatingSince != nil, "duration", now.Sub(cy.start)}
	if changed {
		logging.Infow(ctx, "Relay telemetry repeater poll", kv...)
	} else {
		logging.Debugw(ctx, "Relay telemetry repeater poll", kv...)
	}
}

// restore puts a repeater the poller hasn't read since it started back in
// the report, from what the relay kept: the last attempt and success, and
// the saved reading. The report is the complete set, so without this a
// restart would blank its readings on the Grid until its next cycle.
func (p *Poller) restore(c meshrelay.RepeaterCredential) {
	if c.LastAttemptAt == nil {
		return // never tried: not reported yet
	}
	ns := &nodeState{pubkey: c.Pubkey, name: c.Name, lastAttempt: *c.LastAttemptAt, outcome: outcomeFailed}
	if c.LastOKAt != nil {
		ns.lastSuccess = *c.LastOKAt
		if c.LastError == "" {
			ns.outcome = outcomeOK
		}
	}
	if len(c.Poll.Reading) > 0 && !ns.lastSuccess.IsZero() {
		var s Sample
		if err := json.Unmarshal(c.Poll.Reading, &s); err == nil {
			ns.sample = &s
		}
	}
	p.mu.Lock()
	if p.repeaters[c.Pubkey] == nil {
		p.repeaters[c.Pubkey] = ns
	}
	p.mu.Unlock()
}

// waitingText says why no gateway could try, for the first few.
func waitingText(skipped []string) string {
	if len(skipped) == 0 {
		return "no gateway is online"
	}
	slices.Sort(skipped)
	skipped = slices.Compact(skipped)
	more := ""
	if len(skipped) > 3 {
		more = fmt.Sprintf("; %d more", len(skipped)-3)
		skipped = skipped[:3]
	}
	return strings.Join(skipped, "; ") + more
}

// candidates orders the gateways to try for a repeater: its primary (or the
// one that last reached it), the one that shared it, then the rest.
func candidates(c meshrelay.RepeaterCredential, gateways []string) []string {
	var out []string
	for _, g := range []string{c.Poll.Primary, c.LastGateway, c.AddedByGateway} {
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

// offset spreads repeaters' cycles across the interval by pubkey.
func (p *Poller) offset(pubkey string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pubkey))
	return time.Duration(h.Sum64() % uint64(p.cfg.RepeaterInterval))
}

func (p *Poller) hibernateWindow() time.Duration {
	return time.Duration(p.cfg.HibernateAfter) * p.cfg.RepeaterInterval
}

// gridAlive asks the Grid whether anything other than this reporter has
// shown the repeater alive within window: an advert, or another monitor's
// successful read. A node the Grid doesn't know isn't alive.
func (p *Poller) gridAlive(ctx context.Context, pubkey string, window time.Duration) (bool, error) {
	u := p.cfg.GridURL + "/api/v1/events/" + url.PathEscape("meshcore:"+pubkey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("the Grid answered %d", resp.StatusCode)
	}
	var ev struct {
		Mesh struct {
			Telemetry struct {
				LastAdvertAt *time.Time `json:"lastAdvertAt"`
				Admin        *struct {
					ReporterID    string     `json:"reporterId"`
					LastSuccessAt *time.Time `json:"lastSuccessAt"`
				} `json:"admin"`
			} `json:"telemetry"`
		} `json:"mesh"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ev); err != nil {
		return false, err
	}
	since := p.cfg.Now().Add(-window)
	t := ev.Mesh.Telemetry
	if t.LastAdvertAt != nil && t.LastAdvertAt.After(since) {
		return true, nil
	}
	if a := t.Admin; a != nil && a.ReporterID != p.cfg.ReporterID && a.LastSuccessAt != nil && a.LastSuccessAt.After(since) {
		return true, nil
	}
	return false, nil
}
