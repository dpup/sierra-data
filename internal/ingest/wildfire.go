package ingest

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/calfire"
	"github.com/dpup/info.ersn.net/server/internal/clients/wfigs"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
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
}

// NewWildfireNormalizer wires the normalizer to its two clients (tests inject
// ones built with NewClientWithHTTPDoer).
func NewWildfireNormalizer(cfg *config.Config, cf *calfire.Client, wf *wfigs.Client) *WildfireNormalizer {
	return &WildfireNormalizer{cfg: cfg, calfire: cf, wfigs: wf}
}

// SourceIDs implements Normalizer. One poller, two source rows.
func (n *WildfireNormalizer) SourceIDs() []string { return []string{"calfire", "wfigs"} }

// Poll implements Normalizer. One source failing degrades to a PerSource
// entry (the survivor's events still return); both failing is a hard error.
func (n *WildfireNormalizer) Poll(ctx context.Context) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return &PollResult{}, nil
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
		perims, perr = n.wfigs.GetPerimeters(ctx, wfigs.Bounds{
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
			SeverityFromLabel(hazards.SeverityFromWildfire(in.PercentContained)),
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
		} else {
			ev.Geometry = GeometryFromPoint(in.Lat, in.Lng)
		}
		ev.Detail = &gridv1.Event_Wildfire{Wildfire: detail}
		events = append(events, ev)
	}

	events = append(events, standalonePerimeterEvents(ctx, perims, used)...)

	if len(perSource) == 0 {
		perSource = nil
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}

// standalonePerimeterEvents emits perimeters no CAL FIRE incident adopted
// (mapped fires the curated list omits). Ids are "wfigs:{normname}"; when
// several distinct perimeters share a normalized name the -2/-3 collision
// suffixes are assigned in centroid (lat, lng) order, so ids are stable across
// polls regardless of upstream feature order (plan decision 3 — NOT slice
// index, which the shipped builder used).
func standalonePerimeterEvents(ctx context.Context, perims []wfigs.Perimeter, used map[string]bool) []*gridv1.Event {
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
	out := make([]*gridv1.Event, 0, len(cands))
	for _, c := range cands {
		counts[c.norm]++
		id := "wfigs:" + c.norm
		if counts[c.norm] > 1 {
			id = fmt.Sprintf("%s-%d", id, counts[c.norm])
		}
		p := c.perim
		ev := NewEvent(
			id,
			gridv1.Layer_WILDFIRE,
			SeverityFromLabel(hazards.SeverityFromWildfire(p.PercentContained)),
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
