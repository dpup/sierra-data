package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// roadsIncidentsAPI is the slice of RoadsService this normalizer consumes
// (decision 6: ingest reuses the AI-enhanced, budgeted, cached incidents
// pipeline rather than re-parsing the KML feeds). IncidentFeedHealth exposes
// the per-feed outcome hidden inside ListIncidents: the service keeps serving
// the surviving feed when only one KML feed fails, so without it a dead feed
// is indistinguishable from a quiet one — and the disappearance sweep would
// RESOLVE the dead feed's still-active events.
type roadsIncidentsAPI interface {
	ListIncidents(ctx context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error)
	IncidentFeedHealth() (chpErr, laneErr error, at time.Time)
}

// enhancedFields are the Event fields the incident AI pipeline generates,
// recorded in Enhancement provenance so clients can badge AI text. headline is
// the AI condensed line, summary the AI narrative; description stays the
// verbatim original (not listed — it is NOT AI-generated).
var enhancedFields = []string{"headline", "summary", "impact"}

// RoadIncidentNormalizer ingests region-wide CHP/Caltrans incidents (id
// namespace "chp:") from every configured incident area. One poller, two
// source rows: "chp" for dispatch incidents, "caltrans" for lane closures
// (decision 4 — the split is by api.AlertType).
type RoadIncidentNormalizer struct {
	cfg   *config.Config
	roads roadsIncidentsAPI
}

// NewRoadIncidentNormalizer wires the normalizer to the roads service (the
// concrete *services.RoadsService satisfies roadsIncidentsAPI).
func NewRoadIncidentNormalizer(cfg *config.Config, roads roadsIncidentsAPI) *RoadIncidentNormalizer {
	return &RoadIncidentNormalizer{cfg: cfg, roads: roads}
}

// SourceIDs implements Normalizer.
func (n *RoadIncidentNormalizer) SourceIDs() []string { return []string{"chp", "caltrans"} }

// Poll implements Normalizer. Both sources come from the same per-area
// ListIncidents calls, so a partial area failure degrades both source rows
// (the surviving areas' events still return); every area failing is a hard
// error.
func (n *RoadIncidentNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	areas := n.cfg.Roads.IncidentAreas
	if len(areas) == 0 {
		return nil, errEmptyScope("incident areas")
	}

	var (
		events []*gridv1.Event
		seen   = make(map[string]bool)
		errs   []error
	)
	for _, area := range areas {
		// EnhancementIo: the store persists the model I/O, so ingest needs it even
		// though the external /api/v1/incidents response omits it by default.
		resp, err := n.roads.ListIncidents(ctx, &api.ListIncidentsRequest{Area: area.ID, EnhancementIo: true})
		if err != nil {
			errs = append(errs, fmt.Errorf("area %s: %w", area.ID, err))
			continue
		}
		for _, in := range resp.GetIncidents() {
			ev := n.buildEvent(in)
			if ev == nil || seen[ev.Id] {
				continue // locationless, or duplicated across overlapping areas
			}
			// Restart churn suppression: the roads pipeline's AI cache is
			// in-memory, so after a restart incidents beyond its 5-per-refresh
			// enhancement budget are served UNENHANCED for a few refreshes.
			// Upserting that raw copy over a stored enhanced event would
			// hash-differ (raw headline/detail) => a spurious revision, then a
			// SECOND revision when re-enhancement lands. When the incoming
			// incident hasn't been AI-processed yet (impact UNSPECIFIED is set
			// exclusively by the model) and the store already holds an
			// enhanced version, carry the prior event forward verbatim for
			// this tick: hash-equal => no revision, and it still counts as
			// seen. Genuinely new incidents (no prior) emit raw immediately —
			// availability first; the enhanced revision follows.
			if in.GetImpact() == api.AlertImpact_ALERT_IMPACT_UNSPECIFIED {
				if pe := priorByID(prior, ev.Id); pe != nil && pe.GetEnhancement() != nil {
					ev = pe
				}
			}
			seen[ev.Id] = true
			events = append(events, ev)
		}
	}

	// Per-feed health: ListIncidents alone can't show a single dead feed (the
	// service keeps serving the survivor), and the sweep must never resolve a
	// dead feed's events. A hard error stays reserved for the every-area AND
	// both-feeds-down case, as before.
	chpErr, laneErr, _ := n.roads.IncidentFeedHealth()
	if len(errs) == len(areas) && chpErr != nil && laneErr != nil {
		return nil, fmt.Errorf("all incident areas failed: %w", errors.Join(errs...))
	}
	perSource := make(map[string]error)
	if len(errs) > 0 {
		// Area-level failures degrade both source rows — they share the calls.
		err := fmt.Errorf("partial incident coverage: %w", errors.Join(errs...))
		perSource["chp"] = err
		perSource["caltrans"] = err
	}
	if chpErr != nil {
		perSource["chp"] = errors.Join(perSource["chp"], fmt.Errorf("chp feed: %w", chpErr))
	}
	if laneErr != nil {
		perSource["caltrans"] = errors.Join(perSource["caltrans"], fmt.Errorf("lane-closure feed: %w", laneErr))
	}
	if len(perSource) == 0 {
		perSource = nil
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}

// buildEvent converts one api.Incident, returning nil for locationless
// entries (the shipped builder skips them the same way).
func (n *RoadIncidentNormalizer) buildEvent(in *api.Incident) *gridv1.Event {
	loc := in.GetLocation()
	if loc == nil {
		return nil
	}

	// Field roles (grid model §3.1). Three texts flow from the incidents
	// pipeline: a verbatim original (raw CHP text), and — for AI-enhanced
	// incidents — a short condensed line and a longer narrative.
	//   headline    = the short card line: the AI condensed summary, or the
	//                 readable text when unenhanced.
	//   summary     = the AI narrative (enhanced only) — badged AI text.
	//   description = the verbatim original, ALWAYS preserved so a client can
	//                 show it alongside the enhanced text ("translate, never
	//                 assert" transparency). Dropped only when it would just
	//                 duplicate the headline (unenhanced, no distinct original).
	condensed := in.GetCondensedSummary() // AI short (enhanced only)
	narrative := in.GetDescription()      // AI narrative (enhanced) or readable type line
	original := in.GetOriginalText()      // raw verbatim feed text
	var headline, summary string
	if condensed != "" {
		headline = condensed
		summary = narrative
	} else {
		headline = narrative
	}
	description := original
	if description == headline {
		description = "" // no distinct original to show beyond the headline
	}

	ev := NewEvent(
		"chp:"+in.GetId(),
		gridv1.Layer_ROAD_INCIDENT,
		SeverityFromLabel(hazards.SeverityFromAlertSeverity(in.GetSeverity())),
		gridv1.EventStatus_ACTIVE, // the feeds only list active incidents
		headline,
	)
	category := strings.ToLower(strings.TrimPrefix(in.GetType().String(), "ALERT_TYPE_"))
	ev.Category = category
	ev.Summary = summary
	ev.Description = description
	ev.AreaLabel = in.GetLocationDescription()
	ev.Geometry = GeometryFromPoint(loc.GetLatitude(), loc.GetLongitude())
	ev.Effective = in.GetStarted()
	ev.ObservedAt = in.GetLastUpdated()
	ev.Provenance = incidentProvenance(in.GetType())
	ev.Detail = &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
		LogNumber: in.GetLogNumber(),
		Impact:    impactSlug(in.GetImpact()),
		Duration:  in.GetMetadata()["duration"], // AI extra when the model provided one
		Metadata:  publicMetadata(in.GetMetadata()),
	}}
	// Enhancement provenance only when the incident actually went through the
	// AI pipeline — impact is set exclusively by the model's assessment
	// (services/incidents.go), so UNSPECIFIED means structural fields only.
	if in.GetImpact() != api.AlertImpact_ALERT_IMPACT_UNSPECIFIED {
		ev.Enhancement = &gridv1.Enhancement{
			Model:      n.cfg.OpenAI.Model,
			EnhancedAt: in.GetAiEnhancedAt(), // when the model ran (provenance)
			Fields:     enhancedFields,
			Request:    in.GetAiRequest(),  // the model input, for transparency
			Response:   in.GetAiResponse(), // the raw structured model output
		}
	}
	return ev
}

// incidentProvenance splits provenance by feed: CHP dispatch incidents vs
// Caltrans lane closures (source names match the shipped envelope's blocks).
func incidentProvenance(t api.AlertType) *gridv1.Provenance {
	if t == api.AlertType_CLOSURE {
		return NewProvenance("caltrans", "Caltrans", "quickmap.dot.ca.gov", "")
	}
	return NewProvenance("chp", "CHP / Caltrans", "quickmap.dot.ca.gov", "")
}

// internalMetadataKeys are metadata entries that are upstream/parsing artifacts,
// not public incident detail — stripped before the map reaches the /v1 API.
//   - style_url: the Caltrans KML placemark style ref ("#chp"), a rendering
//     artifact of the feed parser
//   - source: duplicates provenance.source_name
//   - duration: promoted to the typed RoadIncidentDetail.duration field
var internalMetadataKeys = map[string]bool{"style_url": true, "source": true, "duration": true}

// publicMetadata copies the AI metadata map, dropping internal/redundant keys.
// Returns nil when nothing public remains (so protojson omits the field).
func publicMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if internalMetadataKeys[k] || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// impactSlug renders the AI-assessed impact enum as its wire slug
// ("none" | "light" | "moderate" | "severe"), empty when not enhanced.
func impactSlug(i api.AlertImpact) string {
	if i == api.AlertImpact_ALERT_IMPACT_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(i.String(), "IMPACT_"))
}
