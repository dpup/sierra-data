package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// roadsIncidentsAPI is the slice of RoadsService this normalizer consumes
// (decision 6: ingest reuses the AI-enhanced, budgeted, cached incidents
// pipeline rather than re-parsing the KML feeds).
type roadsIncidentsAPI interface {
	ListIncidents(ctx context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error)
}

// enhancedFields are the Event fields the incident AI pipeline generates,
// recorded in Enhancement provenance so clients can badge AI text.
var enhancedFields = []string{"summary", "description", "impact"}

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
func (n *RoadIncidentNormalizer) Poll(ctx context.Context) (*PollResult, error) {
	areas := n.cfg.Roads.IncidentAreas
	if len(areas) == 0 {
		return &PollResult{}, nil
	}

	var (
		events []*gridv1.Event
		seen   = make(map[string]bool)
		errs   []error
	)
	for _, area := range areas {
		resp, err := n.roads.ListIncidents(ctx, &api.ListIncidentsRequest{Area: area.ID})
		if err != nil {
			errs = append(errs, fmt.Errorf("area %s: %w", area.ID, err))
			continue
		}
		for _, in := range resp.GetIncidents() {
			ev := n.buildEvent(in)
			if ev == nil || seen[ev.Id] {
				continue // locationless, or duplicated across overlapping areas
			}
			seen[ev.Id] = true
			events = append(events, ev)
		}
	}

	if len(errs) == len(areas) {
		return nil, fmt.Errorf("all incident areas failed: %w", errors.Join(errs...))
	}
	var perSource map[string]error
	if len(errs) > 0 {
		err := fmt.Errorf("partial incident coverage: %w", errors.Join(errs...))
		perSource = map[string]error{"chp": err, "caltrans": err}
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

	ev := NewEvent(
		"chp:"+in.GetId(),
		gridv1.Layer_ROAD_INCIDENT,
		SeverityFromLabel(hazards.SeverityFromAlertSeverity(in.GetSeverity())),
		gridv1.EventStatus_ACTIVE, // the feeds only list active incidents
		in.GetDescription(),       // shipped headline: the (possibly AI-enhanced) description
	)
	category := strings.ToLower(strings.TrimPrefix(in.GetType().String(), "ALERT_TYPE_"))
	ev.Category = category
	ev.Summary = in.GetCondensedSummary()
	ev.AreaLabel = in.GetLocationDescription()
	ev.Geometry = GeometryFromPoint(loc.GetLatitude(), loc.GetLongitude())
	ev.Effective = in.GetStarted()
	ev.ObservedAt = in.GetLastUpdated()
	ev.Provenance = incidentProvenance(in.GetType())
	ev.Detail = &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
		LogNumber:           in.GetLogNumber(),
		IncidentType:        category,
		LocationDescription: in.GetLocationDescription(),
		Impact:              impactSlug(in.GetImpact()),
		Duration:            in.GetMetadata()["duration"], // AI extra when the model provided one
		CondensedSummary:    in.GetCondensedSummary(),
		Metadata:            in.GetMetadata(),
	}}
	// Enhancement provenance only when the incident actually went through the
	// AI pipeline — impact is set exclusively by the model's assessment
	// (services/incidents.go), so UNSPECIFIED means structural fields only.
	if in.GetImpact() != api.AlertImpact_ALERT_IMPACT_UNSPECIFIED {
		ev.Enhancement = &gridv1.Enhancement{
			Model:  n.cfg.OpenAI.Model,
			Fields: enhancedFields,
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

// impactSlug renders the AI-assessed impact enum as its wire slug
// ("none" | "light" | "moderate" | "severe"), empty when not enhanced.
func impactSlug(i api.AlertImpact) string {
	if i == api.AlertImpact_ALERT_IMPACT_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(i.String(), "IMPACT_"))
}
