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
	"github.com/dpup/sierra-data/internal/clients/wfigs"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
)

// WildfireNormalizer joins CAL FIRE incidents (id namespace "calfire:") with
// WFIGS perimeters ("wfigs:") over the union bbox of the configured hazard
// areas, porting the shipped wildfires builder's name-join semantics: an
// incident adopts its perimeter polygon only on an unambiguous normalized-name
// match; perimeters with no matching incident are emitted standalone.
type WildfireNormalizer struct {
	cfg     *config.Config
	calfire *calfire.Client
	wfigs   *wfigs.Client

	// The WFIGS perimeter query is expensive and rate-limited (NIFC's shared org
	// quota — see the note in the wfigs client). gatedPerimeters only refetches
	// when the layer's dataLastEditDate advanced (or the last-good set aged past
	// maxPerimCacheAge), otherwise reusing this in-process last-good set. Touched
	// only by Poll's single wfigs goroutine, serially across ticks (no lock
	// needed), and keyed on the static configured bounds.
	lastPerimEdit  time.Time
	cachedPerims   []wfigs.Perimeter
	lastPerimFetch time.Time
	havePerimCache bool
	now            func() time.Time // injectable clock (maxPerimCacheAge valve; tests)
}

// maxPerimCacheAge bounds how long the last-good perimeter set is served without a
// re-fetch while dataLastEditDate is unchanged — a safety valve so a stalled or
// CDN-pinned stamp can't silently freeze the fire map indefinitely. Still ~36x
// fewer expensive queries than the 10m poll, and WFIGS updates ~daily anyway.
const maxPerimCacheAge = 6 * time.Hour

// NewWildfireNormalizer wires the normalizer to its two clients (tests inject
// ones built with NewClientWithHTTPDoer).
func NewWildfireNormalizer(cfg *config.Config, cf *calfire.Client, wf *wfigs.Client) *WildfireNormalizer {
	return &WildfireNormalizer{cfg: cfg, calfire: cf, wfigs: wf, now: time.Now}
}

// SourceIDs implements Normalizer. One poller, two source rows.
func (n *WildfireNormalizer) SourceIDs() []string { return []string{"calfire", "wfigs"} }

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
		perims     []wfigs.Perimeter
		ierr, perr error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); incidents, ierr = n.calfire.GetActiveIncidents(ctx) }()
	go func() {
		defer wg.Done()
		perims, perr = n.gatedPerimeters(ctx, wfigs.Bounds{
			MinLatitude:  minLat,
			MaxLatitude:  maxLat,
			MinLongitude: minLng,
			MaxLongitude: maxLng,
		})
	}()
	wg.Wait()

	if ierr != nil && perr != nil {
		return nil, fmt.Errorf("both wildfire sources failed: calfire=%v wfigs=%v", ierr, perr)
	}
	perSource := make(map[string]error)
	if ierr != nil {
		perSource["calfire"] = ierr
	}
	if perr != nil {
		perSource["wfigs"] = perr
	}

	// Index perimeters by normalized name so an incident can adopt its polygon.
	// Two distinct perimeters normalizing to the same name are ambiguous: an
	// incident must never adopt an arbitrary one (wrong-geometry risk); both are
	// emitted standalone instead.
	byName := make(map[string]wfigs.Perimeter, len(perims))
	ambiguous := make(map[string]bool)
	for _, p := range perims {
		norm := hazards.NormFireName(p.Name)
		if _, seen := byName[norm]; seen {
			ambiguous[norm] = true
		}
		byName[norm] = p
	}
	used := make(map[string]bool)

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
		ev.Provenance = NewProvenance("calfire", "CAL FIRE", "CAL FIRE / WFIGS", safeURL(in.URL))

		detail := &gridv1.WildfireDetail{
			Acres:       in.Acres,
			Containment: in.PercentContained,
			County:      in.County,
		}
		norm := hazards.NormFireName(in.Name)
		if perim, matched := byName[norm]; matched && !ambiguous[norm] {
			if geom, err := geometryFromTyped(perim.GeometryType, perim.GeometryCoords); err == nil {
				ev.Geometry = geom
				detail.HasPerimeter = true
				used[norm] = true
			} else {
				// Broken perimeter geometry: keep the incident as a point rather
				// than dropping it; the standalone pass skips it the same way.
				logging.Warnw(ctx, "Unusable WFIGS perimeter geometry; wildfire falls back to point",
					"fire", in.Name, "error", err)
				ev.Geometry = GeometryFromPoint(in.Lat, in.Lng)
			}
		} else if perr != nil {
			// WFIGS is down, so the adoption lookup can see no perimeters at
			// all. Downgrading an incident that held a perimeter last tick to a
			// point + has_perimeter=false would write a false "perimeter gone"
			// revision and throw away real spatial extent. Carry the PRIOR
			// geometry and has_perimeter forward instead; the scalar fields
			// (acres, containment, headline) still update from CAL FIRE — those
			// are genuine revisions.
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

	standalone := standalonePerimeterEvents(ctx, perims, used, prior)
	if ierr != nil {
		// CAL FIRE is down, so adoption is uncomputable and the used-map above
		// is empty: perimeters that are normally folded into calfire:* incidents
		// would be minted as NEW standalone wfigs:* ACTIVE events, duplicating
		// the still-active calfire events (whose sweep the PerSource error
		// suppresses) for up to the expire grace. While adoption is
		// uncomputable, emit standalone events ONLY for computed ids the store
		// already tracks as wfigs standalones — never mint new standalone ids.
		// Tradeoff: a genuinely new fire that first appears while CAL FIRE is
		// down stays invisible until CAL FIRE recovers; delayed visibility is
		// preferred over duplicate ACTIVE fires on the map.
		priorIDs := make(map[string]bool)
		for _, pe := range priorForSource(prior, "wfigs") {
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

// gatedPerimeters returns the current WFIGS perimeters, running the expensive,
// 429-prone feature query ONLY when the layer's dataLastEditDate advanced since
// our last successful fetch, or the last-good set has aged past maxPerimCacheAge
// (see the rate-limit note in the wfigs client). While the stamp is unchanged and
// the cache is fresh it serves the in-process last-good set — a genuine success,
// since the data has not changed, so the disappearance sweep can act on it. The
// bounds are static config, so a cached set stays valid for that scope.
//
// On a WFIGS error we do NOT advance the stamp (the next tick retries) and return
// the error so the caller flags the source failed and the sweep is skipped
// (fail-loud). If the cheap metadata check itself fails, we log it and fall back
// to a direct GetPerimeters — exactly the pre-gating behavior, never worse.
func (n *WildfireNormalizer) gatedPerimeters(ctx context.Context, b wfigs.Bounds) ([]wfigs.Perimeter, error) {
	edit, err := n.wfigs.LastEdit(ctx)
	if err != nil {
		// Metadata check failed (rare — it's CDN-cached). Log it so a persistently
		// failing check — which silently reverts to a fetch every tick and disables
		// the gating — is visible; then fall back to a direct fetch so a metadata
		// hiccup never blocks perimeters. Best-effort: if that also errors it fails
		// loud exactly like pre-gating.
		logging.Warnw(ctx, "WFIGS metadata check failed; falling back to a direct perimeter fetch", "error", err)
		return n.wfigs.GetPerimeters(ctx, b)
	}
	// Reuse the in-process last-good set only while the stamp is unchanged AND the
	// cache is still fresh. The maxPerimCacheAge valve forces a refetch if
	// dataLastEditDate ever stalls upstream while the perimeter data changed.
	if n.havePerimCache && edit.Equal(n.lastPerimEdit) && n.now().Sub(n.lastPerimFetch) <= maxPerimCacheAge {
		return n.cachedPerims, nil // unchanged + fresh — skip the expensive origin query
	}
	perims, perr := n.wfigs.GetPerimeters(ctx, b)
	if perr != nil {
		return nil, perr // stamp not advanced → retried next tick; sweep skipped
	}
	n.cachedPerims, n.lastPerimEdit, n.lastPerimFetch, n.havePerimCache = perims, edit, n.now(), true
	return perims, nil
}

// standalonePerimeterEvents emits perimeters no CAL FIRE incident adopted
// (mapped fires the curated list omits). Ids are "wfigs:{normname}"; when
// several distinct perimeters share a normalized name the -2/-3 collision
// suffixes are assigned in centroid (lat, lng) order, so ids are stable across
// polls regardless of upstream feature order (plan decision 3 — NOT slice
// index, which the shipped builder used). When a name is back down to exactly
// ONE candidate, standaloneContinuityID keeps the survivor on the id it
// already holds in the store instead of silently reassigning it the bare id.
func standalonePerimeterEvents(ctx context.Context, perims []wfigs.Perimeter, used map[string]bool, prior Prior) []*gridv1.Event {
	type candidate struct {
		perim wfigs.Perimeter
		geom  *gridv1.Geometry
		norm  string
	}
	var cands []candidate
	for _, p := range perims {
		norm := hazards.NormFireName(p.Name)
		if used[norm] || p.GeometryType == "" {
			continue
		}
		geom, err := geometryFromTyped(p.GeometryType, p.GeometryCoords)
		if err != nil {
			logging.Warnw(ctx, "Skipping standalone WFIGS perimeter with unusable geometry",
				"fire", p.Name, "error", err)
			continue
		}
		cands = append(cands, candidate{perim: p, geom: geom, norm: norm})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].norm != cands[j].norm {
			return cands[i].norm < cands[j].norm
		}
		ci, cj := cands[i].geom.Centroid, cands[j].geom.Centroid
		if ci.Lat != cj.Lat {
			return ci.Lat < cj.Lat
		}
		return ci.Lng < cj.Lng
	})

	counts := make(map[string]int)
	for _, c := range cands {
		counts[c.norm]++
	}

	seq := make(map[string]int)
	out := make([]*gridv1.Event, 0, len(cands))
	for _, c := range cands {
		seq[c.norm]++
		var id string
		if counts[c.norm] == 1 {
			id = standaloneContinuityID(prior, c.norm, c.geom)
		} else {
			id = "wfigs:" + c.norm
			if seq[c.norm] > 1 {
				id = fmt.Sprintf("%s-%d", id, seq[c.norm])
			}
		}
		p := c.perim
		ev := NewEvent(
			id,
			gridv1.Layer_WILDFIRE,
			SeverityFromLabel(hazards.SeverityFromWildfire(p.Acres, p.PercentContained)),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("%s — %.0f ac, %d%% contained", p.Name, p.Acres, p.PercentContained), // shipped headline format
		)
		ev.Category = "wildfire"
		ev.Geometry = c.geom
		ev.Provenance = NewProvenance("wfigs", "NIFC WFIGS", "NIFC / WFIGS", "")
		ev.Detail = &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{
			Acres:        p.Acres,
			Containment:  p.PercentContained,
			Cause:        p.Cause,
			HasPerimeter: true,
		}}
		out = append(out, ev)
	}
	return out
}

// standaloneContinuityID picks the standalone id for a normalized name with
// exactly ONE current candidate. Without continuity, when one of two
// same-named perimeters disappears the survivor always inherits the bare id —
// if the survivor had been "wfigs:{name}-2", the bare id would silently splice
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
// wfigs disappearance policy.
func standaloneContinuityID(prior Prior, norm string, geom *gridv1.Geometry) string {
	bare := "wfigs:" + norm
	var priorIDs []*gridv1.Event
	for _, pe := range priorForSource(prior, "wfigs") {
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

// isStandaloneIDForName reports whether id is "wfigs:{norm}" or
// "wfigs:{norm}-{digits}". Normalized names contain only [a-z0-9], so the "-"
// separator is unambiguous.
func isStandaloneIDForName(id, norm string) bool {
	bare := "wfigs:" + norm
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
