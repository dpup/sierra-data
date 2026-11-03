package meshcore

import (
	"slices"
	"strings"
	"time"
)

// Relay attribution: which known node forwarded a packet, from the hop hashes
// in its path.
//
// A hop is the first 1, 2 or 3 bytes of a repeater's public key, at the size
// the packet's SENDER chose (one size per packet; path_len encodes it). Two
// rules resolve a hop to a node, and a hop that neither pins to exactly one
// node is credited to nobody:
//
//  1. By prefix. A 2- or 3-byte hash carried by exactly one known RELAY-CAPABLE
//     node is that node. Never for a 1-byte hash: with ~1,000 repeaters each
//     byte is shared by about five, and a byte "unique" among the nodes we know
//     is as likely to be a relayer we have never catalogued.
//  2. By neighbourhood. A hop is next to its neighbours in the path, and the
//     ends of the path have anchors: the observer that reported the packet
//     heard the LAST hop directly, and an advert's sender transmitted to the
//     FIRST. A hop whose prefix matches exactly one known neighbour of an
//     adjacent, already-resolved hop (or anchor) is that neighbour. Resolution
//     propagates inward from both ends until nothing changes. This is what
//     resolves 1-byte hops, and it narrows ~5 same-byte candidates to the
//     handful of nodes that have actually been heard next to the anchor.
//
// The neighbour graph is learned ONLY from rule 1, never from rule 2, so an
// inference cannot reinforce itself. That includes the boot seed: SeedPaths
// replays the store's raw receptions through rule 1, rather than loading the
// link rollup, whose edges were resolved by the older unique-prefix rule that
// does match 1-byte hops (the "3c" -> San Benito misattribution below).
//
// Measured on a 2-hour, 3,000-packet capture: rule 2 agreed with CoreScope's
// resolver on 97% of the hops both resolved, and in every disagreement
// CoreScope's choice had never been seen next to that anchor. It also showed
// why rule 1 had to exclude 1-byte hops: "3c" (SIERRA Lake Alpine's byte) was
// pinned to San Benito Repeater, near Hollister, in 1,378 paths, and Lake
// Alpine was even a candidate in only 25.

// KnownNode is a node the caller knows from outside the registry (the store's
// mesh events), offered to DrainRelays for resolution.
type KnownNode struct {
	PubKey string
	Role   string
}

// SeedPath is one stored reception's relay path, for SeedPaths.
type SeedPath struct {
	Hops     []string // lowercase hex, sender -> observer order
	Observer string   // the radio that heard the last hop; "" if unknown
	Sender   string   // the advert's sender
	At       time.Time
}

const (
	// maxRelayPaths caps the distinct paths buffered between drains (a tick
	// sees a few hundred); past it new paths are dropped and known ones still
	// update.
	maxRelayPaths = 50000
	// AdjacencyRetention is how long a neighbour link stays usable without
	// being seen again.
	AdjacencyRetention = 30 * 24 * time.Hour
	// fullKeyHex is a full public key's hex length; only full keys can be
	// credited (an observer id may be a prefix or a name).
	fullKeyHex = 64
)

type relayPath struct {
	hops     []string // lowercase hex, sender -> observer order
	observer string   // heard the last hop directly
	sender   string   // an advert's sender; "" for other packet types
}

type relayEntry struct {
	path relayPath
	at   time.Time
}

// canRelay reports whether a node of this role can appear as a path hop.
// Companions and sensors originate packets but never forward them; an unknown
// role is kept, which can only add ambiguity, never remove it.
func canRelay(role string) bool {
	switch role {
	case RoleCompanion, RoleSensor:
		return false
	}
	return true
}

// recordRelaysLocked buffers one flood path as one observer heard it. A path
// with no hops is kept only for an advert, where it still says the observer
// hears the sender directly. Caller holds r.mu.
func (r *Registry) recordRelaysLocked(hops []string, observer, sender string, t time.Time) {
	if len(hops) == 0 && sender == "" {
		return
	}
	observer = anchorKey(observer)
	sender = strings.ToLower(sender)
	key := observer + "|" + sender + "|" + strings.Join(hops, ",")
	if r.relayBuf == nil {
		r.relayBuf = make(map[string]relayEntry)
	}
	e, ok := r.relayBuf[key]
	if !ok {
		if len(r.relayBuf) >= maxRelayPaths {
			return
		}
		e.path = relayPath{hops: slices.Clone(hops), observer: observer, sender: sender}
	}
	if t.After(e.at) {
		e.at = t
	}
	r.relayBuf[key] = e
}

// recordRouteLocked buffers a direct-routed packet's remaining source route
// (see FrameHeader.RouteHops). Caller holds r.mu.
func (r *Registry) recordRouteLocked(hops []string, t time.Time) {
	key := strings.Join(hops, ",")
	if r.routeBuf == nil {
		r.routeBuf = make(map[string]relayEntry)
	}
	e, ok := r.routeBuf[key]
	if !ok {
		if len(r.routeBuf) >= maxRelayPaths {
			return
		}
		e.path = relayPath{hops: slices.Clone(hops)}
	}
	if t.After(e.at) {
		e.at = t
	}
	r.routeBuf[key] = e
}

// SeedPaths rebuilds the neighbour graph from stored receptions, so a restart
// does not stop crediting 1-byte hops until the graph re-learns. Each path is
// resolved by rule 1 against the registry's catalog plus `known`, exactly as
// live traffic is, and only what that pins down is learned. Call at boot,
// after Seed and before Connect.
func (r *Registry) SeedPaths(paths []SeedPath, known []KnownNode) {
	now := r.now()
	idx := relayIndex(r.relayRoles(known))
	r.adjMu.Lock()
	defer r.adjMu.Unlock()
	for _, sp := range paths {
		if now.Sub(sp.At) > AdjacencyRetention {
			continue
		}
		p := relayPath{hops: sp.Hops, observer: anchorKey(sp.Observer), sender: strings.ToLower(sp.Sender)}
		r.learnLocked(p, resolveByPrefix(p.hops, idx), sp.At)
	}
}

// relayRoles is the resolution catalog: every node the registry holds, plus
// `known` for keys it does not.
func (r *Registry) relayRoles(known []KnownNode) map[string]string {
	r.mu.Lock()
	roles := make(map[string]string, len(r.nodes)+len(known))
	for pk, e := range r.nodes {
		roles[pk] = e.Role
	}
	r.mu.Unlock()
	for _, k := range known {
		pk := strings.ToLower(k.PubKey)
		if _, ok := roles[pk]; pk != "" && !ok {
			roles[pk] = k.Role
		}
	}
	return roles
}

// anchorKey normalizes an observer id for the graph.
func anchorKey(observer string) string {
	return strings.ToLower(strings.TrimSpace(observer))
}

func (r *Registry) addEdgeLocked(a, b string, t time.Time) {
	if a == "" || b == "" || a == b {
		return
	}
	if r.adj == nil {
		r.adj = make(map[string]map[string]time.Time)
	}
	for _, pair := range [2][2]string{{a, b}, {b, a}} {
		m := r.adj[pair[0]]
		if m == nil {
			m = make(map[string]time.Time)
			r.adj[pair[0]] = m
		}
		if t.After(m[pair[1]]) {
			m[pair[1]] = t
		}
	}
}

// DrainRelays returns, per node public key, the latest reception of a flood
// packet that node relayed since the last drain, and clears the buffer. Called
// once per scheduler tick (single reader). See the top of this file for how a
// hop resolves.
//
// The catalog is every node the registry holds PLUS `known` (the store's mesh
// events). The extra keys rescue a repeater the registry pruned while it still
// relays, and add collision evidence. Only relay-capable roles are candidates.
//
// Resolved nodes the registry holds are stamped for relay presence and
// retention. A key known only from `known` gets activity, not presence.
func (r *Registry) DrainRelays(known []KnownNode) map[string]time.Time {
	now := r.now()
	r.mu.Lock()
	buf := r.relayBuf
	r.relayBuf = nil
	routes := r.routeBuf
	r.routeBuf = nil
	r.mu.Unlock()
	roles := r.relayRoles(known)

	r.adjMu.Lock()
	r.pruneAdjacencyLocked(now)
	if len(buf) == 0 && len(routes) == 0 {
		r.adjMu.Unlock()
		return nil
	}
	idx := relayIndex(roles)

	// Rule 1 for every path first, learning from what it pins down, so this
	// tick's unambiguous traffic informs this tick's ambiguous traffic.
	entries := make([]relayEntry, 0, len(buf))
	resolved := make([][]string, 0, len(buf))
	for _, e := range buf {
		res := resolveByPrefix(e.path.hops, idx)
		r.learnLocked(e.path, res, e.at)
		entries = append(entries, e)
		resolved = append(resolved, res)
	}
	out := make(map[string]time.Time)
	// Direct routes: rule 1 only, learning nothing (see FrameHeader.RouteHops).
	for _, e := range routes {
		for _, pk := range resolveByPrefix(e.path.hops, idx) {
			if pk != "" && e.at.After(out[pk]) {
				out[pk] = e.at
			}
		}
	}
	for i, e := range entries {
		res := resolved[i]
		r.resolveByNeighboursLocked(e.path, res, roles)
		for _, pk := range res {
			if pk != "" && e.at.After(out[pk]) {
				out[pk] = e.at
			}
		}
	}
	r.adjMu.Unlock()

	r.mu.Lock()
	for pk, t := range out {
		if e := r.nodes[pk]; e != nil && t.After(e.LastRelayedAt) {
			e.LastRelayedAt = t
		}
	}
	r.mu.Unlock()
	return out
}

// relayIndex maps 1/2/3-byte prefixes to the relay-capable full keys carrying
// them.
func relayIndex(roles map[string]string) map[string][]string {
	keys := make(map[string]struct{}, len(roles))
	for pk, role := range roles {
		if len(pk) == fullKeyHex && canRelay(role) {
			keys[pk] = struct{}{}
		}
	}
	return prefixIndex(keys)
}

// resolveByPrefix is rule 1: a 2+ byte hop carried by exactly one candidate.
func resolveByPrefix(hops []string, idx map[string][]string) []string {
	res := make([]string, len(hops))
	if len(hops) == 0 || len(hops[0]) < 4 {
		return res // 1-byte path: no hop resolves by prefix alone
	}
	for i, h := range hops {
		if c := idx[h]; len(c) == 1 {
			res[i] = c[0]
		}
	}
	return res
}

// learnLocked records the links rule 1 proves: consecutive positions in
// sender, hops..., observer where both ends are known. A gap breaks the chain;
// nothing is learned across an unresolved hop. Caller holds adjMu.
func (r *Registry) learnLocked(p relayPath, res []string, t time.Time) {
	chain := make([]string, 0, len(res)+2)
	chain = append(chain, p.sender)
	chain = append(chain, res...)
	chain = append(chain, p.observer)
	for i := 0; i+1 < len(chain); i++ {
		r.addEdgeLocked(chain[i], chain[i+1], t)
	}
}

// resolveByNeighboursLocked is rule 2, filling res in place. Caller holds adjMu.
func (r *Registry) resolveByNeighboursLocked(p relayPath, res []string, roles map[string]string) {
	n := len(p.hops)
	for changed := true; changed; {
		changed = false
		for i := 0; i < n; i++ {
			if res[i] != "" {
				continue
			}
			left, right := p.sender, p.observer
			if i > 0 {
				left = res[i-1]
			}
			if i < n-1 {
				right = res[i+1]
			}
			var pick string
			count := 0
			for _, anchor := range [2]string{left, right} {
				if anchor == "" {
					continue
				}
				for nb := range r.adj[anchor] {
					if nb == pick || len(nb) != fullKeyHex || !strings.HasPrefix(nb, p.hops[i]) {
						continue
					}
					// Only a catalogued relay-capable node can be credited. The
					// graph also holds observers (often companions we never
					// heard advertise), and an unverified pick here would go on
					// to anchor the next hop inward.
					if role, ok := roles[nb]; !ok || !canRelay(role) {
						continue
					}
					pick = nb
					count++
				}
			}
			if count == 1 {
				res[i] = pick
				changed = true
			}
		}
	}
}

func (r *Registry) pruneAdjacencyLocked(now time.Time) {
	for a, m := range r.adj {
		for b, t := range m {
			if now.Sub(t) > AdjacencyRetention {
				delete(m, b)
			}
		}
		if len(m) == 0 {
			delete(r.adj, a)
		}
	}
}
