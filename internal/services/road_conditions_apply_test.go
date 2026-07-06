package services

import (
	"context"
	"testing"

	"github.com/dpup/prefab/logging"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/config"
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
	s.applyRoadConditions(ctx, mr, []caltrans.RoadCondition{off}, &status, &cc, &expl, &alerts)
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
	s.applyRoadConditions(ctx, mr, []caltrans.RoadCondition{on}, &status, &cc, &expl, &alerts)
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
