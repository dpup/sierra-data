package ingest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/calfire"
	"github.com/dpup/sierra-data/internal/clients/firis"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
)

// WildfireNormalizer joins CAL FIRE incidents (id namespace "calfire:") with
// perimeters from the CAL FIRE / FIRIS combo feed ("firis:") over the union bbox
// of the configured hazard areas. An incident adopts its perimeter polygon only
// on an unambiguous normalized-name match; perimeters with no matching incident
// are emitted standalone.
//
// The combo feed carries MANY rows per fire (successive CAL FIRE Intel / FIRIS IR
// flights), so Poll first dedups them to one perimeter per fire (dedupePerimeters,
// see docs/firis-perimeter-source-design.md §4) before the name-join runs.
type WildfireNormalizer struct {
	cfg     *config.Config
	calfire *calfire.Client
	firis   *firis.Client

	// The perimeter feature query is expensive and can be rate-limited (per-org
	// request units — see the note in the firis client). gatedPerimeters only
	// refetches when the layer's dataLastEditDate advanced (or the last-good set
	// aged past maxPerimCacheAge), otherwise reusing this in-process last-good set.
	// Touched only by Poll's single perimeter goroutine, serially across ticks (no
	// lock needed), and keyed on the static configured bounds.
	lastPerimEdit  time.Time
	cachedPerims   []firis.Perimeter
	lastPerimFetch time.Time
	havePerimCache bool
	now            func() time.Time // injectable clock (maxPerimCacheAge valve; tests)
}

// maxPerimCacheAge bounds how long the last-good perimeter set is served without a
// re-fetch while dataLastEditDate is unchanged — a safety valve so a stalled or
// CDN-pinned stamp can't silently freeze the fire map indefinitely. Still far
// fewer expensive queries than the poll interval.
const maxPerimCacheAge = 6 * time.Hour

// NewWildfireNormalizer wires the normalizer to its two clients (tests inject
// ones built with NewClientWithHTTPDoer).
func NewWildfireNormalizer(cfg *config.Config, cf *calfire.Client, fc *firis.Client) *WildfireNormalizer {
	return &WildfireNormalizer{cfg: cfg, calfire: cf, firis: fc, now: time.Now}
}

// SourceIDs implements Normalizer. One poller, two source rows.
func (n *WildfireNormalizer) SourceIDs() []string { return []string{"calfire", "firis"} }

// priorByID is a nil-tolerant Prior.ByID (the scheduler always passes a real
// Prior; unit tests may pass nil).
func priorByID(p Prior, id string) *gridv1.Event {
	if p == nil {
		return nil
	}
	return p.ByID(id)
}

// priorForSource is a nil-tolerant Prior.ForSource.
func priorForSource(p Prior, sourceID string) []*gridv1.Event {
	if p == nil {
		return nil
	}
	return p.ForSource(sourceID)
}

// Poll implements Normalizer. One source failing degrades to a PerSource
// entry (the survivor's events still return); both failing is a hard error.
func (n *WildfireNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return nil, errEmptyScope("hazard areas")
	}

	// The two sources are independent; fetch concurrently so their timeouts
	// don't stack (same rationale as the shipped builder).
	var (
		incidents  []calfire.Incident
		perims     []firis.Perimeter
		ierr, perr error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); incidents, ierr = n.calfire.GetActiveIncidents(ctx) }()
	go func() {
		defer wg.Done()
		perims, perr = n.gatedPerimeters(ctx, firis.Bounds{
			MinLatitude:  minLat,
			MaxLatitude:  maxLat,
			MinLongitude: minLng,
			MaxLongitude: maxLng,
		})
	}()
	wg.Wait()

	if ierr != nil && perr != nil {
		return nil, fmt.Errorf("both wildfire sources failed: calfire=%v firis=%v", ierr, perr)
	}
	perSource := make(map[string]error)
	if ierr != nil {
		perSource["calfire"] = ierr
	}
	if perr != nil {
		perSource["firis"] = perr
	}

	// Collapse the combo feed's many-rows-per-fire to one perimeter per fire, then
	// index by normalized name so an incident can adopt its polygon. After dedup a
	// name maps to a single candidate UNLESS two genuinely-distinct same-named
	// fires exist (separate spatial clusters) — that residual ambiguity is handled
	// exactly as before: an incident must never adopt an arbitrary one, so both go
	// standalone.
	deduped := dedupePerimeters(ctx, perims)
	byName := make(map[string]perimCandidate, len(deduped))
	ambiguous := make(map[string]bool)
	for _, c := range deduped {
		if _, seen := byName[c.norm]; seen {
			ambiguous[c.norm] = true
		}
		byName[c.norm] = c
	}
	used := make(map[string]bool)

	// A wholesale-empty perimeter set from a SUCCESSFUL fetch is treated as
	// non-authoritative for the adopt path (like an outage): a working feed that
	// has any active fire in our bbox returns at least one perimeter, so zero is far
	// more likely a transient glitch than every fire's perimeter vanishing at once.
	// Carry prior geometry forward instead of downgrading every adopted fire to a
	// point (a false "perimeter gone" revision across the whole map). A NON-empty
	// feed that simply omits one fire is authoritative — that fire genuinely
	// downgrades. Standalones need no equivalent guard: their `expire` grace already
	// absorbs a transient empty. (perr != nil is the hard-outage case.)
	perimsUnusable := perr != nil || len(deduped) == 0

	var events []*gridv1.Event
	for _, in := range incidents {
		if in.Lat == 0 && in.Lng == 0 {
			continue
		}
		// CAL FIRE's list is statewide; scope to the configured areas' union.
		if in.Lat < minLat || in.Lat > maxLat || in.Lng < minLng || in.Lng > maxLng {
			continue
		}

		ev := NewEvent(
			"calfire:"+nonEmpty(in.UniqueID, hazards.NormFireName(in.Name)),
			gridv1.Layer_WILDFIRE,
			SeverityFromLabel(hazards.SeverityFromWildfire(in.Acres, in.PercentContained)),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("%s — %.0f ac, %d%% contained", in.Name, in.Acres, in.PercentContained), // shipped headline format
		)
		ev.Category = "wildfire"
		ev.AreaLabel = nonEmpty(in.Location, in.County)
		ev.CanonicalUrl = safeURL(in.URL)
		ev.Effective = tsProto(in.Started)
		ev.ObservedAt = tsProto(in.Updated)
		ev.Provenance = NewProvenance("calfire", "CAL FIRE", "CAL FIRE / FIRIS", safeURL(in.URL))

		detail := &gridv1.WildfireDetail{
			Acres:       in.Acres,
			Containment: in.PercentContained,
			County:      in.County,
		}
		norm := hazards.NormFireName(in.Name)
		if cand, matched := byName[norm]; matched && !ambiguous[norm] {
			// Geometry was already parsed + validated during dedup.
			ev.Geometry = cand.geom
			detail.HasPerimeter = true
			used[norm] = true
		} else if perimsUnusable {
			// The perimeter set is unusable (source down, or a non-authoritative
			// wholesale-empty response). Downgrading an incident that held a perimeter
			// last tick to a point + has_perimeter=false would write a false "perimeter
			// gone" revision and throw away real spatial extent. Carry the PRIOR
			// geometry and has_perimeter forward instead; the scalar fields (acres,
			// containment, headline) still update from CAL FIRE — those are genuine
			// revisions.
			if pe := priorByID(prior, ev.Id); pe.GetWildfire().GetHasPerimeter() && pe.GetGeometry() != nil {
				ev.Geometry = pe.GetGeometry()
				detail.HasPerimeter = true
			} else {
				ev.Geometry = GeometryFromPoint(in.Lat, in.Lng)
			}
		} else {
			ev.Geometry = GeometryFromPoint(in.Lat, in.Lng)
		}
		ev.Detail = &gridv1.Event_Wildfire{Wildfire: detail}
		events = append(events, ev)
	}

	standalone := standalonePerimeterEvents(deduped, used, prior)
	if ierr != nil {
		// CAL FIRE is down, so adoption is uncomputable and the used-map above
		// is empty: perimeters that are normally folded into calfire:* incidents
		// would be minted as NEW standalone firis:* ACTIVE events, duplicating
		// the still-active calfire events (whose sweep the PerSource error
		// suppresses) for up to the expire grace. While adoption is
		// uncomputable, emit standalone events ONLY for computed ids the store
		// already tracks as firis standalones — never mint new standalone ids.
		// Tradeoff: a genuinely new fire that first appears while CAL FIRE is
		// down stays invisible until CAL FIRE recovers; delayed visibility is
		// preferred over duplicate ACTIVE fires on the map.
		priorIDs := make(map[string]bool)
		for _, pe := range priorForSource(prior, "firis") {
			priorIDs[pe.GetId()] = true
		}
		kept := standalone[:0]
		for _, ev := range standalone {
			if priorIDs[ev.GetId()] {
				kept = append(kept, ev)
			}
		}
		standalone = kept
	}
	events = append(events, standalone...)

	if len(perSource) == 0 {
		perSource = nil
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}

// gatedPerimeters returns the current perimeters, running the expensive feature
// query ONLY when the layer's dataLastEditDate advanced since our last successful
// fetch, or the last-good set has aged past maxPerimCacheAge (see the rate-limit
// note in the firis client). While the stamp is unchanged and the cache is fresh
// it serves the in-process last-good set — a genuine success, since the data has
// not changed, so the disappearance sweep can act on it. The bounds are static
// config, so a cached set stays valid for that scope.
//
// On a fetch error we do NOT advance the stamp (the next tick retries) and return
// the error so the caller flags the source failed and the sweep is skipped
// (fail-loud). If the cheap metadata check itself fails, we log it and fall back
// to a direct GetPerimeters — exactly the pre-gating behavior, never worse.
func (n *WildfireNormalizer) gatedPerimeters(ctx context.Context, b firis.Bounds) ([]firis.Perimeter, error) {
	edit, err := n.firis.LastEdit(ctx)
	if err != nil {
		// Metadata check failed (rare — it's CDN-cached). Log it so a persistently
		// failing check — which silently reverts to a fetch every tick and disables
		// the gating — is visible; then fall back to a direct fetch so a metadata
		// hiccup never blocks perimeters. Best-effort: if that also errors it fails
		// loud exactly like pre-gating.
		logging.Warnw(ctx, "FIRIS metadata check failed; falling back to a direct perimeter fetch", "error", err)
		return n.firis.GetPerimeters(ctx, b)
	}
	// Reuse the in-process last-good set only while the stamp is unchanged AND the
	// cache is still fresh. The maxPerimCacheAge valve forces a refetch if
	// dataLastEditDate ever stalls upstream while the perimeter data changed.
	if n.havePerimCache && edit.Equal(n.lastPerimEdit) && n.now().Sub(n.lastPerimFetch) <= maxPerimCacheAge {
		return n.cachedPerims, nil // unchanged + fresh — skip the expensive origin query
	}
	perims, perr := n.firis.GetPerimeters(ctx, b)
	if perr != nil {
		return nil, perr // stamp not advanced → retried next tick; sweep skipped
	}
	if len(perims) == 0 {
		// Never cache/replay an empty set as last-good (the hazards fail-loud rule:
		// "empty results are never cached"). An empty combo-feed response is often a
		// transient ArcGIS glitch (backend load-shedding, a momentarily-empty spatial
		// index), not a genuine all-clear. Return it for THIS tick — the adopt path
		// treats a wholesale-empty result as non-authoritative and carries prior
		// geometry forward — but leave the last-good cache + stamp untouched so a good
		// set is never overwritten by empty and the next tick re-fetches.
		return perims, nil
	}
	n.cachedPerims, n.lastPerimEdit, n.lastPerimFetch, n.havePerimCache = perims, edit, n.now(), true
	return perims, nil
}

// perimCandidate is one deduped perimeter: the chosen raw row, its parsed +
// validated geometry (so adoption/standalone never re-parse), the display name,
// and the normalized name the name-join keys on.
type perimCandidate struct {
	perim firis.Perimeter
	geom  *gridv1.Geometry
	name  string // display name (incident_name or mission-derived)
	norm  string
}

// perimClusterThresholdSq is the squared lat/lng distance below which two
// same-named perimeters are treated as the SAME fire (successive IR flights share
// a location). ~0.15° ≈ 15 km — far larger than a fire moves between flights, far
// smaller than the gap between two genuinely-distinct same-named fires.
const perimClusterThresholdSq = 0.15 * 0.15

// dedupePerimeters collapses the combo feed's many-rows-per-fire into one
// perimeter per fire (docs/firis-perimeter-source-design.md §4): derive a name for
// every row (incident_name, else parsed from the FIRIS mission id) and drop rows
// with neither a name nor usable geometry; group by normalized name; within a
// name spatially cluster (so two distinct same-named fires stay separate); keep
// the freshest row per cluster (latest poly_DateCurrent, then Active + source
// priority). Result: ≤1 candidate per (name, cluster).
func dedupePerimeters(ctx context.Context, perims []firis.Perimeter) []perimCandidate {
	var rows []perimCandidate
	for _, p := range perims {
		// Defense in depth: the query already filters displayStatus='Active', but
		// never ingest a perimeter the feed marks Inactive even if that server-side
		// filter ever breaks (a syntax quirk would otherwise flood the map with
		// hundreds of stale/contained statewide perimeters). A blank status is
		// treated leniently (kept) — only an explicit non-Active is dropped.
		if p.Status != "" && !isActiveStatus(p.Status) {
			continue
		}
		name := firisName(p)
		if name == "" || p.GeometryType == "" {
			continue // unattributable (no name / unparseable mission) or undrawable
		}
		geom, err := geometryFromTyped(p.GeometryType, p.GeometryCoords)
		if err != nil {
			logging.Warnw(ctx, "Skipping FIRIS perimeter with unusable geometry", "fire", name, "error", err)
			continue
		}
		rows = append(rows, perimCandidate{perim: p, geom: geom, name: name, norm: hazards.NormFireName(name)})
	}

	// Group by normalized name, preserving first-seen order for determinism.
	byName := make(map[string][]perimCandidate)
	var order []string
	for _, r := range rows {
		if _, ok := byName[r.norm]; !ok {
			order = append(order, r.norm)
		}
		byName[r.norm] = append(byName[r.norm], r)
	}

	var out []perimCandidate
	for _, norm := range order {
		for _, cluster := range clusterByCentroid(byName[norm]) {
			best := cluster[0]
			for _, r := range cluster[1:] {
				if perimFresher(r.perim, best.perim) {
					best = r
				}
			}
			out = append(out, best)
		}
	}
	return out
}

// clusterByCentroid greedily partitions same-named candidates into per-fire
// clusters (see sameFire). The group is first sorted into a deterministic order —
// incident_number-bearing rows first, then by that id, then by centroid — so the
// clustering can NEVER depend on the upstream feature order (which ArcGIS does not
// guarantee stable across polls); an unstable cluster count would flap the
// standalone id suffixes and mint phantom duplicate events. Each candidate joins
// the first cluster whose representative is the same fire, else opens a new one;
// sorting id-bearing rows first makes the representative carry the incident_number
// whenever any cluster member has one, so the differing-id split (below) is
// reliable.
func clusterByCentroid(group []perimCandidate) [][]perimCandidate {
	sort.SliceStable(group, func(i, j int) bool {
		ai, aj := group[i].perim.IncidentNumber, group[j].perim.IncidentNumber
		if (ai == "") != (aj == "") {
			return ai != "" // non-empty incident_number sorts first
		}
		if ai != aj {
			return ai < aj
		}
		ci, cj := group[i].geom.Centroid, group[j].geom.Centroid
		if ci.Lat != cj.Lat {
			return ci.Lat < cj.Lat
		}
		return ci.Lng < cj.Lng
	})
	var clusters [][]perimCandidate
	for _, c := range group {
		placed := false
		for i := range clusters {
			if sameFire(clusters[i][0], c) {
				clusters[i] = append(clusters[i], c)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []perimCandidate{c})
		}
	}
	return clusters
}

// sameFire reports whether two same-named perimeters belong to one fire. The
// combo feed's incident_number is a stable per-fire uuid on CAL FIRE Intel rows
// (null on FIRIS mission rows), so it is authoritative when present: a shared
// non-empty id is definitely one fire (merge even if successive flights' centroids
// drifted apart), and two DIFFERENT non-empty ids are definitely distinct fires
// (never merge, even co-located — this is what stops two same-named fires ~10 km
// apart from collapsing into one and dropping a real perimeter). Only when at
// least one id is absent do we fall back to centroid proximity.
func sameFire(a, b perimCandidate) bool {
	ai, bi := a.perim.IncidentNumber, b.perim.IncidentNumber
	if ai != "" && bi != "" {
		return ai == bi
	}
	return centroidDistSq(a.geom, b.geom) < perimClusterThresholdSq
}

// perimFresher reports whether a is the better representative of a fire than b:
// the most-current footprint (latest poly_DateCurrent), then an Active perimeter
// over Inactive, then source priority (CAL FIRE Intel > FIRIS > WFIGS), then
// larger acreage — a total order so the pick is deterministic.
func perimFresher(a, b firis.Perimeter) bool {
	if !a.DateCurrent.Equal(b.DateCurrent) {
		return a.DateCurrent.After(b.DateCurrent)
	}
	if as, bs := isActiveStatus(a.Status), isActiveStatus(b.Status); as != bs {
		return as
	}
	if ap, bp := sourcePriority(a.Source), sourcePriority(b.Source); ap != bp {
		return ap > bp
	}
	return a.Acres > b.Acres
}

func isActiveStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "Active")
}

// sourcePriority ranks the combo feed's contributing sources when timestamps tie:
// CAL FIRE Intel flights and FIRIS lead the WFIGS to-date perimeters.
func sourcePriority(src string) int {
	s := strings.ToUpper(src)
	switch {
	case strings.Contains(s, "INTEL"):
		return 3
	case strings.Contains(s, "FIRIS"):
		return 2
	case strings.Contains(s, "WFIGS"):
		return 1
	default:
		return 0
	}
}

// firisName derives the incident name for a combo-feed row: incident_name when
// present, else parsed from the FIRIS mission id. Empty when neither yields a name
// (the row can't be attributed to a fire and is dropped).
func firisName(p firis.Perimeter) string {
	if n := strings.TrimSpace(p.IncidentName); n != "" {
		return n
	}
	return nameFromMission(p.Mission)
}

// nameFromMission extracts the fire name from a FIRIS mission id of the form
// CA-<UNIT>-<NAME…>[-<FLIGHTID>] (e.g. "CA-TCU-DOVE-N57B" → "DOVE",
// "CA-FKU-PARAMOUNT" → "PARAMOUNT"): drop the "CA" + unit tokens and a trailing
// flight-id token. Returns "" when the string doesn't match that shape.
func nameFromMission(mission string) string {
	toks := strings.Split(strings.TrimSpace(mission), "-")
	if len(toks) < 3 || !strings.EqualFold(toks[0], "CA") {
		return ""
	}
	body := toks[2:] // drop "CA" and the unit token
	if len(body) > 1 && isFlightID(body[len(body)-1]) {
		body = body[:len(body)-1]
	}
	// A mission with no embedded incident name (e.g. "CA-TCU-N57B" — unit + flight
	// id only) leaves a lone flight-id token. That names the aircraft, not the
	// fire, so treat the row as unattributable rather than minting a fire called
	// "N57B" (which would also merge two of that plane's unnamed missions).
	if len(body) == 1 && isFlightID(body[0]) {
		return ""
	}
	return strings.TrimSpace(strings.Join(body, " "))
}

// isFlightID reports whether tok looks like a FIRIS flight identifier (an "N"
// followed by alphanumerics including at least one digit — N57B, N50X, N42Z).
func isFlightID(tok string) bool {
	if len(tok) < 2 || (tok[0] != 'N' && tok[0] != 'n') {
		return false
	}
	hasDigit := false
	for _, r := range tok[1:] {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		default:
			return false
		}
	}
	return hasDigit
}

// standalonePerimeterEvents emits deduped perimeters no CAL FIRE incident adopted
// (mapped fires the curated list omits). Ids are "firis:{normname}"; when several
// distinct perimeters share a normalized name (two same-named fires in separate
// spatial clusters) the -2/-3 collision suffixes are assigned in centroid
// (lat, lng) order, so ids are stable across polls regardless of upstream feature
// order. When a name is back down to exactly ONE candidate, standaloneContinuityID
// keeps the survivor on the id it already holds in the store instead of silently
// reassigning it the bare id.
//
// The combo feed has no containment/cause, so a standalone perimeter is emitted at
// unknown (0%) containment — conservative for fire (higher severity) and honest:
// a fire missing from CAL FIRE's curated list has no containment figure to quote.
func standalonePerimeterEvents(cands []perimCandidate, used map[string]bool, prior Prior) []*gridv1.Event {
	open := make([]perimCandidate, 0, len(cands))
	for _, c := range cands {
		if used[c.norm] {
			continue
		}
		open = append(open, c)
	}
	sort.SliceStable(open, func(i, j int) bool {
		if open[i].norm != open[j].norm {
			return open[i].norm < open[j].norm
		}
		ci, cj := open[i].geom.Centroid, open[j].geom.Centroid
		if ci.Lat != cj.Lat {
			return ci.Lat < cj.Lat
		}
		return ci.Lng < cj.Lng
	})

	counts := make(map[string]int)
	for _, c := range open {
		counts[c.norm]++
	}

	seq := make(map[string]int)
	out := make([]*gridv1.Event, 0, len(open))
	for _, c := range open {
		seq[c.norm]++
		var id string
		if counts[c.norm] == 1 {
			id = standaloneContinuityID(prior, c.norm, c.geom)
		} else {
			id = "firis:" + c.norm
			if seq[c.norm] > 1 {
				id = fmt.Sprintf("%s-%d", id, seq[c.norm])
			}
		}
		p := c.perim
		ev := NewEvent(
			id,
			gridv1.Layer_WILDFIRE,
			SeverityFromLabel(hazards.SeverityFromWildfire(p.Acres, 0)),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("%s — %.0f ac", c.name, p.Acres),
		)
		ev.Category = "wildfire"
		ev.Geometry = c.geom
		ev.Provenance = NewProvenance("firis", "CAL FIRE / FIRIS", "CAL FIRE / FIRIS / NIFC", "")
		ev.Detail = &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{
			Acres:        p.Acres,
			HasPerimeter: true,
		}}
		out = append(out, ev)
	}
	return out
}

// standaloneContinuityID picks the standalone id for a normalized name with
// exactly ONE current candidate. Without continuity, when one of two
// same-named perimeters disappears the survivor always inherits the bare id —
// if the survivor had been "firis:{name}-2", the bare id would silently splice
// two different fires' histories together.
//
// Rule: if the store's prior standalone set holds the bare id (and nothing
// else for this name), keep it; else if it holds exactly one suffixed id for
// this name, REUSE that suffixed id; else mint the bare id.
//
// Residual edge: prior holds SEVERAL ids for this name (e.g. both the bare id
// and "-2") while only one candidate remains — which fire survived is a
// spatial question, not a naming one, so pick the prior id whose stored
// centroid is nearest the candidate's centroid; the others expire under the
// firis disappearance policy.
func standaloneContinuityID(prior Prior, norm string, geom *gridv1.Geometry) string {
	bare := "firis:" + norm
	var priorIDs []*gridv1.Event
	for _, pe := range priorForSource(prior, "firis") {
		if isStandaloneIDForName(pe.GetId(), norm) {
			priorIDs = append(priorIDs, pe)
		}
	}
	switch len(priorIDs) {
	case 0:
		return bare
	case 1:
		return priorIDs[0].GetId()
	}
	best := priorIDs[0]
	bestDist := centroidDistSq(best.GetGeometry(), geom)
	for _, pe := range priorIDs[1:] {
		if d := centroidDistSq(pe.GetGeometry(), geom); d < bestDist {
			best, bestDist = pe, d
		}
	}
	return best.GetId()
}

// isStandaloneIDForName reports whether id is "firis:{norm}" or
// "firis:{norm}-{digits}". Normalized names contain only [a-z0-9], so the "-"
// separator is unambiguous.
func isStandaloneIDForName(id, norm string) bool {
	bare := "firis:" + norm
	if id == bare {
		return true
	}
	rest, ok := strings.CutPrefix(id, bare+"-")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// centroidDistSq is the squared lat/lng distance between two geometries'
// centroids — a comparison key only, not a physical distance. A missing
// centroid sorts last.
func centroidDistSq(a, b *gridv1.Geometry) float64 {
	ca, cb := a.GetCentroid(), b.GetCentroid()
	if ca == nil || cb == nil {
		return math.Inf(1)
	}
	dLat := ca.GetLat() - cb.GetLat()
	dLng := ca.GetLng() - cb.GetLng()
	return dLat*dLat + dLng*dLng
}
