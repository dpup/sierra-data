// Package pushingest is the authenticated push-ingest side of the Grid: the one
// WRITE surface on an otherwise public, read-only, keyless API.
//
// It exists for data no upstream feed publishes. A MeshCore repeater's battery,
// airtime and packet counters live only on the device itself, reachable by
// logging into its admin interface over the mesh — and, more valuable still, an
// operator's monitor can report the one thing a broadcast feed structurally
// cannot: that it TRIED to reach a node and failed. The MQTT advert firehose
// only ever carries positive evidence.
//
// # The shape of the thing
//
// A reporter POSTs a report; the handler authenticates it, validates it, and
// writes it into an in-memory buffer. It does NOT touch the store. The mesh
// poller drains that buffer on its next scheduler tick, exactly as it already
// drains the MQTT subscriber's buffer, so:
//
//   - single-writer discipline holds (the scheduler goroutine is still the only
//     thing that writes events),
//   - pushed nodes get the same identity, lifecycle, grace and sweep machinery
//     as MQTT-heard nodes, rather than a parallel implementation of them,
//   - and a node heard BOTH ways is one event, not two, because both inputs
//     resolve to the same event id.
//
// That is why a push source needs no new lifecycle concepts: "wrap a push source
// as a poller" is a pattern this codebase already committed to for MeshCore MQTT
// (see internal/ingest/CLAUDE.md). This is the second instance of it, not a new
// idea.
//
// # Extensibility
//
// A stream names a payload contract ("mesh.repeater"); the body's own
// schema_version versions it. Streams are registered in a map, and a reporter is
// authorized per stream, so a second monitor pushing a different shape is one
// StreamHandler plus a config block — no changes to routing, auth, rate
// limiting, health, or the normalizer contract.
package pushingest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dpup/sierra-data/internal/config"
)

// reporterDeadMultiple bounds how long a silent reporter may suppress the
// disappearance sweep for its layer, as a multiple of its StaleAfter.
//
// Suppression is the honest response to a reporter blip: its nodes are missing
// from the snapshot for OUR reason, so their absence proves nothing (the
// fail-loud invariant in internal/ingest/CLAUDE.md). But suppression cannot be
// unbounded — a monitor that dies permanently would otherwise freeze the whole
// mesh layer's lifecycle forever, and every node it ever reported would stay
// ACTIVE for good.
//
// So a reporter passes through three states: OK, then STALE (suppress — this is
// probably a blip), then DEAD (stop suppressing — we have to admit we no longer
// know). Nodes then EXPIRE on the source's normal grace, which is the "we lost
// track of this" terminus, NOT the fabricated all-clear that RESOLVED would be.
// That distinction is what makes the bound acceptable here; it would not be
// acceptable on a life-safety layer, and mesh presence is ambient INFO.
const reporterDeadMultiple = 4

// ReporterState is a reporter's liveness as the ingest scheduler sees it.
type ReporterState int

const (
	// ReporterUnknown: configured but has never successfully reported. Not an
	// error — a monitor that has not been set up yet is not a failing feed.
	ReporterUnknown ReporterState = iota
	ReporterOK                    // reported within StaleAfter
	ReporterStale                 // silent past StaleAfter; suppress its sweep
	ReporterDead                  // silent past StaleAfter*reporterDeadMultiple
)

func (s ReporterState) String() string {
	switch s {
	case ReporterOK:
		return "OK"
	case ReporterStale:
		return "STALE"
	case ReporterDead:
		return "DEAD"
	default:
		return "UNKNOWN"
	}
}

// ReporterHealth is one reporter's status for the source registry and the
// normalizer's sweep decision.
type ReporterHealth struct {
	ID             string
	Name           string
	State          ReporterState
	LastAcceptedAt time.Time
	LastAttemptAt  time.Time
	LastError      string
	Reports        int64 // accepted reports since process start
}

// reporter is one authorized client plus its runtime counters.
type reporter struct {
	cfg     config.Reporter
	streams map[string]bool

	// Guarded by Registry.mu.
	lastAcceptedAt time.Time
	lastAttemptAt  time.Time
	lastError      string
	reports        int64
}

// Registry authenticates reporters and holds the buffered reports between the
// HTTP request that delivered them and the scheduler tick that consumes them.
type Registry struct {
	cfg config.IngestConfig

	// byTokenHash keys reporters by the SHA-256 of their bearer token. Lookup is
	// by hash, never by a reporter id supplied in the request: identity comes
	// from the credential, so a body claiming to be someone else changes nothing.
	byTokenHash map[string]*reporter
	order       []*reporter // config order, for stable iteration

	mu sync.Mutex
	// mesh holds the latest full set per reporter: reporterID -> nodeID -> report.
	// A report REPLACES its reporter's map wholesale, because a report is that
	// reporter's complete current set — the same contract PollResult.Events has
	// with the disappearance sweep. A node dropped from a report is therefore no
	// longer monitored, which is information, not a gap.
	mesh map[string]map[string]MeshNodeReport

	now func() time.Time
}

// NewRegistry builds a Registry from config. Reporters with an empty id or a
// malformed token hash are REJECTED at construction with an error rather than
// skipped: a typo'd hash would otherwise present as a reporter that silently
// never authenticates, which is the worst possible failure mode for a
// credential — indistinguishable from a wrong token on the client side.
func NewRegistry(cfg config.IngestConfig) (*Registry, error) {
	r := &Registry{
		cfg:         cfg,
		byTokenHash: make(map[string]*reporter, len(cfg.Reporters)),
		mesh:        make(map[string]map[string]MeshNodeReport),
		now:         time.Now,
	}
	seen := make(map[string]bool, len(cfg.Reporters))
	for _, rc := range cfg.Reporters {
		if rc.ID == "" {
			return nil, fmt.Errorf("pushingest: reporter with empty id")
		}
		if seen[rc.ID] {
			return nil, fmt.Errorf("pushingest: duplicate reporter id %q", rc.ID)
		}
		seen[rc.ID] = true

		h := strings.ToLower(strings.TrimSpace(rc.TokenSha256))
		if len(h) != 64 {
			return nil, fmt.Errorf("pushingest: reporter %q: tokenSha256 must be 64 hex characters, got %d", rc.ID, len(h))
		}
		if _, err := hex.DecodeString(h); err != nil {
			return nil, fmt.Errorf("pushingest: reporter %q: tokenSha256 is not hex: %w", rc.ID, err)
		}
		if _, dup := r.byTokenHash[h]; dup {
			return nil, fmt.Errorf("pushingest: reporter %q shares a token with another reporter", rc.ID)
		}
		if len(rc.Streams) == 0 {
			return nil, fmt.Errorf("pushingest: reporter %q authorizes no streams", rc.ID)
		}
		rep := &reporter{cfg: rc, streams: make(map[string]bool, len(rc.Streams))}
		for _, s := range rc.Streams {
			rep.streams[s] = true
		}
		r.byTokenHash[h] = rep
		r.order = append(r.order, rep)
	}
	return r, nil
}

// Enabled reports whether any reporter is configured. When false the server does
// not mount the endpoint at all — the default build has no write surface.
func (r *Registry) Enabled() bool { return r != nil && len(r.byTokenHash) > 0 }

// TokenHash is the value that goes in config as tokenSha256.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// authenticate resolves a bearer token to a reporter in constant time with
// respect to the token's VALUE.
//
// The map lookup is on the token's hash, so a wrong token leaks nothing about
// which tokens exist. The subtle.ConstantTimeCompare afterwards is belt and
// braces against a future change to a non-constant-time lookup; the hash
// comparison is the real defence, since an attacker controls the input to
// SHA-256 but cannot steer a preimage.
func (r *Registry) authenticate(token string) (*reporter, bool) {
	if token == "" {
		return nil, false
	}
	h := TokenHash(token)
	rep, ok := r.byTokenHash[h]
	if !ok {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(h), []byte(strings.ToLower(rep.cfg.TokenSha256))) != 1 {
		return nil, false
	}
	return rep, true
}

// ReporterIDs returns configured reporter ids authorized for a stream, in config
// order. The server seeds one source-registry row per id so /api/v1/sources
// reports a monitor's health next to every other feed.
func (r *Registry) ReporterIDs(stream string) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, rep := range r.order {
		if rep.streams[stream] {
			out = append(out, rep.cfg.ID)
		}
	}
	return out
}

// ReporterConfig returns a reporter's config by id.
func (r *Registry) ReporterConfig(id string) (config.Reporter, bool) {
	if r == nil {
		return config.Reporter{}, false
	}
	for _, rep := range r.order {
		if rep.cfg.ID == id {
			return rep.cfg, true
		}
	}
	return config.Reporter{}, false
}

// health computes a reporter's state. Caller holds r.mu.
func (r *Registry) healthLocked(rep *reporter, now time.Time) ReporterHealth {
	h := ReporterHealth{
		ID:             rep.cfg.ID,
		Name:           rep.cfg.Name,
		LastAcceptedAt: rep.lastAcceptedAt,
		LastAttemptAt:  rep.lastAttemptAt,
		LastError:      rep.lastError,
		Reports:        rep.reports,
	}
	switch {
	case rep.lastAcceptedAt.IsZero():
		h.State = ReporterUnknown
	case now.Sub(rep.lastAcceptedAt) <= rep.cfg.StaleAfterOrDefault():
		h.State = ReporterOK
	case now.Sub(rep.lastAcceptedAt) <= rep.cfg.StaleAfterOrDefault()*reporterDeadMultiple:
		h.State = ReporterStale
	default:
		h.State = ReporterDead
	}
	return h
}

// Health returns every reporter's status for a stream, in config order.
func (r *Registry) Health(stream string) []ReporterHealth {
	if r == nil {
		return nil
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ReporterHealth
	for _, rep := range r.order {
		if !rep.streams[stream] {
			continue
		}
		out = append(out, r.healthLocked(rep, now))
	}
	return out
}
