package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/hazards"
)

// EvacuationNormalizer ingests Cal OES evacuation zones (id namespace
// "evac:") over the union bbox of the configured hazard areas. The Cal OES
// aggregation layer is active-events-only; explicitly-inactive statuses
// (lifted/all-clear) are dropped, and an unrecognized or BLANK active status
// conservatively classifies as WARNING (life-safety bias, same as the
// shipped builder — a present row is never silently dropped, because the
// resolve sweep would turn that drop into a fabricated all-clear).
type EvacuationNormalizer struct {
	cfg    *config.Config
	caloes *caloes.Client
}

// NewEvacuationNormalizer wires the normalizer to a Cal OES client (tests
// inject one built with caloes.NewClientWithHTTPDoer).
func NewEvacuationNormalizer(cfg *config.Config, client *caloes.Client) *EvacuationNormalizer {
	return &EvacuationNormalizer{cfg: cfg, caloes: client}
}

// SourceIDs implements Normalizer.
func (n *EvacuationNormalizer) SourceIDs() []string { return []string{"caloes"} }

// evacCand pairs a built event with the raw zone it came from, for the
// collision resolution pass.
type evacCand struct {
	z  caloes.EvacZone
	ev *gridv1.Event
}

// Poll implements Normalizer. A Cal OES failure is a hard error — the caller
// must surface UNAVAILABLE/unknown, never an empty all-clear.
func (n *EvacuationNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return nil, errEmptyScope("hazard areas")
	}
	zones, err := n.caloes.GetActiveEvacuations(ctx, caloes.Bounds{
		MinLatitude:  minLat,
		MaxLatitude:  maxLat,
		MinLongitude: minLng,
		MaxLongitude: maxLng,
	})
	if err != nil {
		return nil, err
	}

	// Two features can share an id (two concurrent events on one zone, or
	// degenerate rows collapsing to one synthetic id), and the store keeps one
	// row per id — emitting both would make them overwrite each other every
	// poll. Group by id first, then resolve collisions deterministically.
	groups := make(map[string][]evacCand)
	var idOrder []string
	for _, z := range zones {
		level := hazards.NormalizeEvacLevel(z.Status)
		if level == "" {
			// Distinguish blank from lifted: only an EXPLICIT inactive status
			// (lifted/normal/all clear/repopulation/no evac) drops the zone. A
			// present row whose STATUS is blank/whitespace is missing data in
			// an active-events-only layer — dropping it would let the resolve
			// sweep fabricate an all-clear for a zone Cal OES still lists as
			// active. Conservative WARNING default instead, loudly logged
			// (the hazards fail-loud bias: an error never becomes a 0).
			if hazards.EvacStatusInactive(z.Status) {
				continue // explicitly lifted/all-clear: genuinely inactive
			}
			level = "WARNING"
			logging.Errorw(ctx, "Cal OES zone present with BLANK status; conservatively keeping it active as WARNING",
				"zone", nonEmpty(z.ZoneID, z.ZoneName), "county", z.County)
		} else if !hazards.EvacStatusRecognized(z.Status) {
			// Conservatively classified as WARNING by the fail-loud default. Log
			// so the phrasing can be added to normalizeEvacLevel explicitly.
			logging.Warnw(ctx, "Unrecognized Cal OES evacuation STATUS; defaulted to WARNING",
				"status", z.Status, "zone", nonEmpty(z.ZoneID, z.ZoneName), "county", z.County)
		}

		ev := n.buildEvent(ctx, z, level)
		if _, seen := groups[ev.Id]; !seen {
			idOrder = append(idOrder, ev.Id)
		}
		groups[ev.Id] = append(groups[ev.Id], evacCand{z: z, ev: ev})
	}

	var events []*gridv1.Event
	for _, id := range idOrder {
		events = append(events, resolveEvacIDCollisions(ctx, prior, id, groups[id])...)
	}
	return &PollResult{Events: events}, nil
}

// buildEvent converts one active zone into an event with its base id.
func (n *EvacuationNormalizer) buildEvent(ctx context.Context, z caloes.EvacZone, level string) *gridv1.Event {
	ev := NewEvent(
		evacEventID(z),
		gridv1.Layer_EVACUATION,
		SeverityFromLabel(hazards.SeverityFromEvacLevel(level)),
		gridv1.EventStatus_ACTIVE,
		fmt.Sprintf("Evacuation %s — %s", humanEvacLevel(level), nonEmpty(z.ZoneName, z.County)), // shipped headline format
	)
	ev.Category = strings.ToLower(level)
	// Life-safety: PublicInfo is directive text and is carried VERBATIM.
	// Enhancement may add context around it, never a paraphrase of it
	// (spec §3.1 policy 4 — we don't paraphrase orders).
	ev.Description = z.PublicInfo
	ev.AreaLabel = nonEmpty(z.ZoneName, z.County)
	ev.ObservedAt = tsProto(z.LastUpdated)
	// Deep-link the per-event source_url into the specific zone when it's a
	// Genasys-hosted county (else the generic viewer). The layer-level
	// metadata.source_url stays generic for the fail-loud contract.
	ev.Provenance = NewProvenance("caloes", "Cal OES", "Cal OES — reference only", caloes.ZoneURL(z.ZoneID))
	ev.Detail = &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{
		ZoneId:    z.ZoneID,
		Level:     level,
		EventType: z.EventType,
		County:    z.County,
	}}
	if geom, gerr := geometryFromTyped(z.GeometryType, z.GeometryCoords); gerr == nil {
		ev.Geometry = geom
	} else {
		// Never drop an active zone over bad geometry — emit it as a
		// geometry-less banner event (valid per the model). But a
		// geometry-less event gets no geometric place matches, which would
		// make an ACTIVE evacuation invisible to every place-scoped read.
		// The poll is already bbox-scoped to the configured hazard areas, so
		// conservatively attach the event to every configured area place —
		// over-attachment is the correct life-safety bias; the store UNIONs
		// these preset ids with geometric matches.
		for _, area := range n.cfg.Hazards.Areas {
			ev.PlaceIds = append(ev.PlaceIds, "area:"+area.ID)
		}
		logging.Errorw(ctx, "Cal OES zone has unusable geometry; emitting without geometry, attached to every configured area",
			"zone", nonEmpty(z.ZoneID, z.ZoneName), "areas", len(n.cfg.Hazards.Areas), "error", gerr)
	}
	return ev
}

// evacEventID derives a zone's event id: ZONE_ID, else ZONE_NAME, else (both
// blank — the degenerate rows that used to collapse to the single id "evac:")
// a synthetic content id from the county + raw geometry bytes, so distinct
// unnamed zones stay distinct across polls.
func evacEventID(z caloes.EvacZone) string {
	if nonEmpty(z.ZoneID, z.ZoneName) != "" {
		return "evac:" + nonEmpty(z.ZoneID, z.ZoneName)
	}
	sum := sha256.Sum256(append([]byte(z.County), z.GeometryCoords...))
	return "evac:zone-" + hex.EncodeToString(sum[:4])
}

// evacLevelRank orders levels by severity for collision resolution:
// ORDER > SHELTER_IN_PLACE > WARNING > ADVISORY.
func evacLevelRank(level string) int {
	switch level {
	case "ORDER":
		return 4
	case "SHELTER_IN_PLACE":
		return 3
	case "WARNING":
		return 2
	case "ADVISORY":
		return 1
	default:
		return 0
	}
}

// resolveEvacIDCollisions turns the candidates sharing one id into distinct
// events:
//
//   - Candidates that are the SAME zone (two concurrent events on one zone,
//     e.g. a Fire warning upgraded by a Flood order) collapse to one event
//     keeping the higher-severity level; both statuses are logged.
//   - Residual collisions — genuinely different zones that still collided on
//     the id — get deterministic -2/-3 suffixes in (county, centroid) order,
//     so neither zone is dropped and ids are stable across polls.
func resolveEvacIDCollisions(ctx context.Context, prior Prior, id string, cands []evacCand) []*gridv1.Event {
	// Cluster same-zone candidates, preserving encounter order.
	var clusters [][]evacCand
next:
	for _, c := range cands {
		for i, cl := range clusters {
			if sameEvacZone(cl[0].z, c.z) {
				clusters[i] = append(cl, c)
				continue next
			}
		}
		clusters = append(clusters, []evacCand{c})
	}

	// Collapse each cluster to its highest-severity event.
	winners := make([]*gridv1.Event, 0, len(clusters))
	for _, cl := range clusters {
		best := cl[0]
		if len(cl) > 1 {
			statuses := make([]string, len(cl))
			for i, c := range cl {
				statuses[i] = c.z.Status
				if evacLevelRank(c.ev.GetEvacuation().GetLevel()) > evacLevelRank(best.ev.GetEvacuation().GetLevel()) {
					best = c
				}
			}
			logging.Warnw(ctx, "Multiple concurrent Cal OES events on one zone; keeping the highest severity",
				"id", id, "statuses", statuses, "kept_status", best.z.Status,
				"kept_level", best.ev.GetEvacuation().GetLevel())
		}
		winners = append(winners, best.ev)
	}

	// Deterministic base order by (county, centroid) so new zones get stable
	// suffixes regardless of upstream feature order.
	sort.SliceStable(winners, func(i, j int) bool {
		ci, cj := winners[i].GetEvacuation().GetCounty(), winners[j].GetEvacuation().GetCounty()
		if ci != cj {
			return ci < cj
		}
		gi, gj := winners[i].GetGeometry().GetCentroid(), winners[j].GetGeometry().GetCentroid()
		if gi.GetLat() != gj.GetLat() {
			return gi.GetLat() < gj.GetLat()
		}
		return gi.GetLng() < gj.GetLng()
	})
	// Assign ids with cross-poll continuity. This runs for a single winner too:
	// a surviving zone whose colliding sibling was lifted must KEEP its suffixed
	// id (matched from prior) rather than flip to the bare id — otherwise the
	// resolve sweep reads the old id as disappeared and writes a spurious
	// all-clear for a zone that is still actively evacuating.
	assignEvacContinuityIDs(prior, id, winners)
	if len(winners) > 1 {
		logging.Warnw(ctx, "Distinct Cal OES zones collided on one id; suffixed with cross-poll continuity",
			"id", id, "count", len(winners))
	}
	return winners
}

// assignEvacContinuityIDs gives each colliding winner a stable id ACROSS polls.
// Positional suffixes alone are unstable: when one colliding zone is lifted, the
// survivor's positional rank changes and its id flips (e.g. evac:X-2 → evac:X),
// which the resolve-policy sweep reads as the old id disappearing and writes a
// spurious RESOLVED all-clear into the history of a zone that is STILL actively
// evacuating (a life-safety false all-clear). So each winner first inherits the
// id of its nearest same-county prior event sharing this base id; only genuinely
// new zones take the lowest free suffix. Mirrors the wildfire standalone-
// perimeter continuity (standaloneContinuityID).
func assignEvacContinuityIDs(prior Prior, base string, winners []*gridv1.Event) {
	var priors []*gridv1.Event
	for _, pe := range priorForSource(prior, "caloes") {
		if pid := pe.GetId(); pid == base || strings.HasPrefix(pid, base+"-") {
			priors = append(priors, pe)
		}
	}

	takenPrior := make(map[string]bool)
	assigned := make(map[*gridv1.Event]string, len(winners))
	// Pass 1: each winner claims its nearest same-county unused prior id.
	for _, w := range winners {
		best, bestDist := "", math.MaxFloat64
		for _, pe := range priors {
			if takenPrior[pe.GetId()] || pe.GetEvacuation().GetCounty() != w.GetEvacuation().GetCounty() {
				continue
			}
			if d := centroidDistSq(pe.GetGeometry(), w.GetGeometry()); d < bestDist {
				best, bestDist = pe.GetId(), d
			}
		}
		if best != "" {
			takenPrior[best] = true
			assigned[w] = best
		}
	}
	// Pass 2: winners with no prior match take the lowest free suffix (bare id
	// first, then -2, -3, …), avoiding any id already claimed from a prior.
	taken := make(map[string]bool, len(winners))
	for _, id := range assigned {
		taken[id] = true
	}
	for _, w := range winners {
		if _, ok := assigned[w]; ok {
			continue
		}
		id := base
		for i := 2; taken[id]; i++ {
			id = fmt.Sprintf("%s-%d", base, i)
		}
		assigned[w] = id
		taken[id] = true
	}
	for w, id := range assigned {
		w.Id = id
	}
}

// sameEvacZone reports whether two colliding rows describe the same physical
// zone (=> concurrent events to collapse) rather than two different zones
// that happen to share an id (=> residual collision to suffix). Matching is
// by ZONE_ID when either row has one, else by (ZONE_NAME, COUNTY), else by
// (COUNTY, raw geometry bytes) — the same keys the id derivation used.
func sameEvacZone(a, b caloes.EvacZone) bool {
	if a.ZoneID != "" || b.ZoneID != "" {
		return a.ZoneID == b.ZoneID
	}
	if a.ZoneName != "" || b.ZoneName != "" {
		return a.ZoneName == b.ZoneName && a.County == b.County
	}
	return a.County == b.County && bytes.Equal(a.GeometryCoords, b.GeometryCoords)
}

// humanEvacLevel renders a coded level as the title-cased human phrase used
// in the shipped headline, e.g. "SHELTER_IN_PLACE" → "Shelter In Place".
func humanEvacLevel(level string) string {
	words := strings.Fields(strings.ToLower(strings.ReplaceAll(level, "_", " ")))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
