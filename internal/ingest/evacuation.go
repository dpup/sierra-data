package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
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
		events = append(events, resolveEvacIDCollisions(ctx, id, groups[id])...)
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
	ev.Provenance = NewProvenance("caloes", "Cal OES", "Cal OES — reference only", caloes.SourceURL)
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
func resolveEvacIDCollisions(ctx context.Context, id string, cands []evacCand) []*gridv1.Event {
	if len(cands) == 1 {
		return []*gridv1.Event{cands[0].ev}
	}

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
	if len(winners) == 1 {
		return winners
	}

	// Residual collision: distinct zones under one id. Deterministic -2/-3
	// suffixes by (county, centroid lat, centroid lng) order — stable across
	// polls regardless of upstream feature order.
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
	for i, ev := range winners[1:] {
		ev.Id = fmt.Sprintf("%s-%d", id, i+2)
	}
	logging.Warnw(ctx, "Distinct Cal OES zones collided on one id; suffixed deterministically",
		"id", id, "count", len(winners))
	return winners
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
