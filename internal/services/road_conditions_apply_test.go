package services

import (
	"context"
	"testing"

	"github.com/dpup/prefab/logging"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/lib/alerts"
	"github.com/sierragridteam/sierra-data/internal/lib/geo"
	"github.com/sierragridteam/sierra-data/internal/lib/routing"
)

// TestApplyRoadConditions_OnlyOnRoute verifies a route-wide condition is attached
// to a segment only when its text matches that segment — so the same statewide
// SR-X advisory isn't duplicated onto every monitored segment of the highway
// (the Old River Bridge / Delta case showing on the Calaveras segments).
func TestApplyRoadConditions_OnlyOnRoute(t *testing.T) {
	ctx := logging.EnsureLogger(context.Background())
	s := &RoadsService{}
	mr := config.MonitoredRoad{
		ID:               "hwy4-arnold-bearvalley",
		Section:          "Arnold to Bear Valley",
		LocationKeywords: []string{"Camp Connell", "Dorrington"},
	}

	// Off-route: a real SR-4 condition, but at the Delta — matches no Calaveras
	// keyword. Must NOT be attached, and must NOT change status.
	off := caltrans.RoadCondition{
		Highway:     "4",
		Type:        caltrans.CONDITION_RESTRICTION,
		Description: "1-way controlled traffic at the Contra Costa/San Joaquin Co Line /at Old River Bridge/ - Due to construction",
	}
	status := api.RoadStatus_OPEN
	var cc api.ChainControlStatus
	var expl string
	var alerts []*api.RoadAlert
	var causes []*api.RoadStatusCause
	s.applyRoadConditions(ctx, mr, []caltrans.RoadCondition{off}, &status, &cc, &expl, &alerts, &causes)
	if len(alerts) != 0 {
		t.Errorf("off-route condition attached %d alerts, want 0", len(alerts))
	}
	if status != api.RoadStatus_OPEN {
		t.Errorf("off-route condition changed status to %v, want OPEN", status)
	}

	// On-route: mentions a configured keyword for this segment → attached + ON_ROUTE
	// + drives status.
	on := caltrans.RoadCondition{
		Highway:     "4",
		Type:        caltrans.CONDITION_CLOSURE,
		Description: "SR 4 closed at Camp Connell due to a downed tree",
	}
	if len(causes) != 0 {
		t.Errorf("off-route condition recorded %d causes, want 0", len(causes))
	}
	s.applyRoadConditions(ctx, mr, []caltrans.RoadCondition{on}, &status, &cc, &expl, &alerts, &causes)
	// The condition is text only: it is recorded as the cause, with no position.
	if len(causes) != 1 || causes[0].GetSource() != "road_condition" || causes[0].GetBegin() != nil || causes[0].GetStatus() != api.RoadStatus_CLOSED {
		t.Errorf("causes = %v, want one positionless road_condition CLOSED", causes)
	}
	if len(alerts) != 1 {
		t.Fatalf("on-route condition attached %d alerts, want 1", len(alerts))
	}
	if alerts[0].GetClassification() != api.AlertClassification_ON_ROUTE {
		t.Errorf("classification = %v, want ON_ROUTE", alerts[0].GetClassification())
	}
	if status != api.RoadStatus_CLOSED {
		t.Errorf("on-route closure status = %v, want CLOSED", status)
	}
}

// A lane closure that closes the road is recorded with its begin and end;
// a CHP incident with its point.
func TestAlertCause(t *testing.T) {
	closure := routing.ClassifiedAlert{UnclassifiedAlert: routing.UnclassifiedAlert{
		Title: "Route 4 Full Closure", Type: "closure", Location: geo.Point{Latitude: 38.40, Longitude: -120.10},
		AffectedPolyline: &geo.Polyline{Points: []geo.Point{{Latitude: 38.40, Longitude: -120.10}, {Latitude: 38.45, Longitude: -120.05}}},
	}}
	enh := &alerts.EnhancedAlert{StructuredDescription: alerts.StructuredDescription{RoadStatus: "closed", RestrictionDetails: "Both directions closed"}}
	c := alertCause(closure, &api.RoadAlert{Id: "C4QB"}, enh)
	if c.GetSource() != "lane_closure" || c.GetStatus() != api.RoadStatus_CLOSED || c.GetSourceId() != "C4QB" {
		t.Errorf("cause = %v", c)
	}
	if c.GetEnd().GetLatitude() != 38.45 || c.GetBegin().GetLatitude() != 38.40 || c.GetDescription() != "Both directions closed" {
		t.Errorf("extent = %v .. %v (%q)", c.GetBegin(), c.GetEnd(), c.GetDescription())
	}

	incident := routing.ClassifiedAlert{UnclassifiedAlert: routing.UnclassifiedAlert{Title: "CHP Incident 261110MR0204", Type: "incident", Location: geo.Point{Latitude: 38.3, Longitude: -120.3}}}
	enh = &alerts.EnhancedAlert{StructuredDescription: alerts.StructuredDescription{RoadStatus: "restricted"}}
	c = alertCause(incident, &api.RoadAlert{Id: "261110MR0204", Description: "One lane blocked"}, enh)
	if c.GetSource() != "incident" || c.GetStatus() != api.RoadStatus_RESTRICTED || c.GetEnd() != nil || c.GetDescription() != "One lane blocked" {
		t.Errorf("incident cause = %v", c)
	}
}
