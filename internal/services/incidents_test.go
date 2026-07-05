package services

import (
	"context"
	"strings"
	"testing"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/cache"
	"github.com/dpup/info.ersn.net/server/internal/clients/caltrans"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/lib/alerts"
)

func motherLode() config.IncidentArea {
	return config.IncidentArea{
		ID:   "mother-lode",
		Name: "Mother Lode",
		Bounds: config.GeoBounds{
			MinLatitude:  37.3,
			MaxLatitude:  39.0,
			MinLongitude: -121.15,
			MaxLongitude: -119.5,
		},
	}
}

// chpDescription mirrors the real quickmap CHP CDATA structure.
const chpDescription = `
<div style="font-size:1.15em;"><img src="x" style="float:left"><p align="left">Sep 16 2025  8:36AM <br> 1182-Trfc Collision-No Inj <br> Hwy 49 / Parrotts Ferry Rd </p>
<p align="left">Sep 16 2025  8:37AM [1] UNITS EN ROUTE <br /> </p><p>Information courtesy of CHP</p>
<p class="update-stamp">Last updated: 09/16/2025 9:17am </p></div>`

func TestBuildIncident_CHPParsing(t *testing.T) {
	s := &RoadsService{}
	in := caltrans.CaltransIncident{
		FeedType:        caltrans.CHP_INCIDENT,
		Name:            "CHP Incident 250916ST0066",
		DescriptionHtml: chpDescription,
		DescriptionText: "Trfc Collision",
		StyleUrl:        "#chp",
		Coordinates:     &api.Coordinates{Latitude: 38.0671, Longitude: -120.5402}, // Angels Camp
	}

	inc := s.buildIncident(in, motherLode())
	if inc == nil {
		t.Fatal("expected incident, got nil")
	}
	if inc.LogNumber != "250916ST0066" {
		t.Errorf("log number = %q, want 250916ST0066", inc.LogNumber)
	}
	if inc.Id != "250916ST0066" {
		t.Errorf("id = %q, want 250916ST0066", inc.Id)
	}
	if inc.Type != api.AlertType_INCIDENT {
		t.Errorf("type = %v, want INCIDENT", inc.Type)
	}
	if inc.LocationDescription != "Hwy 49 / Parrotts Ferry Rd" {
		t.Errorf("location = %q, want 'Hwy 49 / Parrotts Ferry Rd'", inc.LocationDescription)
	}
	if inc.Description != "Traffic Collision-No Injury" {
		t.Errorf("description = %q (want humanized)", inc.Description)
	}
	if inc.Severity != api.AlertSeverity_WARNING {
		t.Errorf("severity = %v, want WARNING (collision)", inc.Severity)
	}
	if inc.Started == nil {
		t.Error("expected Started to be parsed")
	}
	if inc.LastUpdated == nil {
		t.Error("expected LastUpdated to be parsed")
	}
	if inc.Area != "mother-lode" {
		t.Errorf("area = %q", inc.Area)
	}
}

func TestBuildIncident_OutsideBoundsExcluded(t *testing.T) {
	s := &RoadsService{}
	in := caltrans.CaltransIncident{
		FeedType:    caltrans.CHP_INCIDENT,
		Name:        "CHP Incident 250916ST0099",
		Coordinates: &api.Coordinates{Latitude: 38.4951, Longitude: -121.4413}, // Sacramento (Central Valley)
	}
	if inc := s.buildIncident(in, motherLode()); inc != nil {
		t.Errorf("expected nil for out-of-bounds incident, got %+v", inc)
	}
}

func TestBuildIncident_NilCoordinates(t *testing.T) {
	s := &RoadsService{}
	in := caltrans.CaltransIncident{FeedType: caltrans.CHP_INCIDENT, Name: "no coords"}
	if inc := s.buildIncident(in, motherLode()); inc != nil {
		t.Error("expected nil for incident without coordinates")
	}
}

func TestIncidentSeverity(t *testing.T) {
	tests := []struct {
		name     string
		feed     caltrans.CaltransFeedType
		typeText string
		style    string
		want     api.AlertSeverity
	}{
		{"injury collision", caltrans.CHP_INCIDENT, "1183-Trfc Collision-Injury", "#chp", api.AlertSeverity_CRITICAL},
		{"no-injury collision", caltrans.CHP_INCIDENT, "1182-Trfc Collision-No Inj", "#chp", api.AlertSeverity_WARNING},
		{"assist", caltrans.CHP_INCIDENT, "Assist CT with Maintenance", "#chp", api.AlertSeverity_INFO},
		{"lane closure", caltrans.LANE_CLOSURE, "", "#closure", api.AlertSeverity_WARNING},
		{"full closure", caltrans.LANE_CLOSURE, "", "#full-closure", api.AlertSeverity_CRITICAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := caltrans.CaltransIncident{FeedType: tt.feed, StyleUrl: tt.style}
			got := incidentSeverity(in, tt.typeText)
			if got != tt.want {
				t.Errorf("severity = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeIncidents_DedupAndSkipGeometry(t *testing.T) {
	s := &RoadsService{}
	area := motherLode()
	coord := &api.Coordinates{Latitude: 38.07, Longitude: -120.54}

	chp := []caltrans.CaltransIncident{
		{FeedType: caltrans.CHP_INCIDENT, Name: "CHP Incident 260625SA0982",
			DescriptionHtml: chpDescription2026, Coordinates: coord},
	}
	lanes := []caltrans.CaltransIncident{
		// Info placemark (kept).
		{FeedType: caltrans.LANE_CLOSURE, Name: "Route 4 One-way Traffic Operation",
			DescriptionHtml: laneClosure2026, Coordinates: coord},
		// Same closure repeated for the other direction (deduped by id C4TA).
		{FeedType: caltrans.LANE_CLOSURE, Name: "Route 4 One-way Traffic Operation",
			DescriptionHtml: laneClosure2026, Coordinates: coord},
		// Geometry-only "path" placemark: no description -> dropped.
		{FeedType: caltrans.LANE_CLOSURE, Name: "C4TA Log 42 path",
			DescriptionHtml: "", DescriptionText: "", Coordinates: coord},
	}

	got := s.normalizeIncidents(testCtx(), area, nil, chp, lanes)
	if len(got) != 2 {
		t.Fatalf("got %d incidents, want 2 (1 CHP + 1 deduped closure)", len(got))
	}
	// CHP first, then the single closure.
	if got[0].Type != api.AlertType_INCIDENT || got[1].Type != api.AlertType_CLOSURE {
		t.Errorf("ordering/types wrong: %v, %v", got[0].Type, got[1].Type)
	}
	for _, inc := range got {
		if inc.Description == "" {
			t.Errorf("incident %s has empty description", inc.Id)
		}
	}
}

func TestHumanizeIncidentType(t *testing.T) {
	cases := map[string]string{
		"1182-Trfc Collision-No Inj":        "Traffic Collision-No Injury",
		"1183-Trfc Collision-Unkn Inj":      "Traffic Collision-Unknown Injury",
		"1125-Traffic Hazard":               "Traffic Hazard",
		"CFIRE-Car Fire":                    "Car Fire",
		"CZP-Assist with Construction":      "Assist with Construction",
		"Route 4 One-way Traffic Operation": "Route 4 One-way Traffic Operation",
		"":                                  "",
	}
	for in, want := range cases {
		if got := humanizeIncidentType(in); got != want {
			t.Errorf("humanizeIncidentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractLogNumber(t *testing.T) {
	cases := map[string]string{
		"CHP Incident 250916ST0066": "250916ST0066",
		"CHP Incident 250911GG0206": "250911GG0206",
		"Some Lane Closure":         "",
	}
	for name, want := range cases {
		in := caltrans.CaltransIncident{Name: name}
		if got := extractLogNumber(in, ""); got != want {
			t.Errorf("extractLogNumber(%q) = %q, want %q", name, got, want)
		}
	}
}

// chpDescription2026 mirrors the 2026 quickmap "infowindow" CHP markup, where
// <name> is blank and details live in iw-* elements.
const chpDescription2026 = `<div class="infowindow-content">
  <div class="iw-header"><div class="iw-header-left">
    <img class="iw-icon" src="x" /> CHP Incident 260625SA1034
  </div></div>
  <div class="iw-body">
    <h2 class="iw-title">1183-Trfc Collision-Injury</h2>
    <p class="iw-text">Jun 25 2026  6:24PM <br> Hwy 49 / Parrotts Ferry Rd</p>
    <p class="iw-text">Jun 25 2026  6:20PM [2] units en route<br /></p>
    <p class="iw-attribution">Information courtesy of <strong>CHP</strong></p>
  </div>
  <div class="iw-footer"><span class="iw-timestamp">Last updated: <strong>06/25/2026</strong> 6:27pm</span></div>
</div>`

func TestBuildIncident_CHP2026Format(t *testing.T) {
	s := &RoadsService{}
	// Name blank as in the live feed - relies on description parsing.
	in := caltrans.CaltransIncident{
		FeedType:        caltrans.CHP_INCIDENT,
		Name:            "CHP Incident 260625SA1034", // backfilled by the client
		DescriptionHtml: chpDescription2026,
		Coordinates:     &api.Coordinates{Latitude: 38.0671, Longitude: -120.5402},
	}
	inc := s.buildIncident(in, motherLode())
	if inc == nil {
		t.Fatal("expected incident")
	}
	if inc.LogNumber != "260625SA1034" {
		t.Errorf("log = %q, want 260625SA1034", inc.LogNumber)
	}
	if inc.Description != "Traffic Collision-Injury" {
		t.Errorf("description = %q (want humanized)", inc.Description)
	}
	if inc.LocationDescription != "Hwy 49 / Parrotts Ferry Rd" {
		t.Errorf("location = %q", inc.LocationDescription)
	}
	if inc.Severity != api.AlertSeverity_CRITICAL {
		t.Errorf("severity = %v, want CRITICAL (injury)", inc.Severity)
	}
	if inc.Started == nil {
		t.Error("expected Started parsed from iw-text")
	}
	if inc.LastUpdated == nil {
		t.Error("expected LastUpdated parsed from iw-timestamp")
	}
}

// laneClosure2026 mirrors the 2026 lane-closure markup.
const laneClosure2026 = `<div class="infowindow-content">
  <div class="iw-header"><div class="iw-header-left"><img class="iw-icon" src="x" /> Lane Closure</div></div>
  <div class="iw-body">
    <h2 class="iw-title">Route 4 One-way Traffic Operation</h2>
    <p class="iw-text">From 0.5 mi E of Murphys to 0.8 mi E / Expect 20-minute delays</p>
    <p class="iw-text"> Due to Emergency Work</p>
    <div style='font-size:xx-small;'>Closure ID: C4TA, Log Number: 42</div>
  </div>
</div>`

func TestBuildIncident_LaneClosure2026Format(t *testing.T) {
	s := &RoadsService{}
	in := caltrans.CaltransIncident{
		FeedType:        caltrans.LANE_CLOSURE,
		Name:            "Route 4 One-way Traffic Operation",
		DescriptionHtml: laneClosure2026,
		Coordinates:     &api.Coordinates{Latitude: 38.139, Longitude: -120.456},
	}
	inc := s.buildIncident(in, motherLode())
	if inc == nil {
		t.Fatal("expected incident")
	}
	if inc.Type != api.AlertType_CLOSURE {
		t.Errorf("type = %v, want CLOSURE", inc.Type)
	}
	if inc.LogNumber != "C4TA" {
		t.Errorf("log = %q, want C4TA", inc.LogNumber)
	}
	if inc.Description != "Route 4 One-way Traffic Operation" {
		t.Errorf("description = %q", inc.Description)
	}
	if !strings.Contains(inc.LocationDescription, "Murphys") {
		t.Errorf("location = %q, want to contain Murphys", inc.LocationDescription)
	}
	if inc.Severity != api.AlertSeverity_WARNING {
		t.Errorf("severity = %v, want WARNING", inc.Severity)
	}
}

// mockEnhancer counts EnhanceAlert calls and returns a canned enhancement.
type mockEnhancer struct {
	calls int
}

func (m *mockEnhancer) EnhanceAlert(ctx context.Context, raw alerts.RawAlert) (alerts.EnhancedAlert, error) {
	m.calls++
	return alerts.EnhancedAlert{
		ID:               raw.ID,
		CondensedSummary: "Injury collision, expect delays.",
		StructuredDescription: alerts.StructuredDescription{
			Details: "A collision with injuries occurred; emergency services are on scene.",
			Impact:  "moderate",
			AdditionalInfo: map[string]string{
				"injuries": "reported",
			},
		},
	}, nil
}

func (m *mockEnhancer) HealthCheck(ctx context.Context) error { return nil }

func enhancementTestService(enhancer alerts.AlertEnhancer) *RoadsService {
	return &RoadsService{
		alertEnhancer: enhancer,
		cache:         cache.NewCache(),
		contentHasher: alerts.NewContentHasher(),
	}
}

// Every incident goes through AI enhancement, and the model's impact
// assessment overrides the keyword-heuristic severity. Repeat content is
// served from the content-hash cache.
func TestEnhanceIncident_AllSeveritiesAndCached(t *testing.T) {
	enhancer := &mockEnhancer{}
	s := enhancementTestService(enhancer)
	raw := caltrans.CaltransIncident{DescriptionText: "1183-Trfc Collision-Injury [2] AMB ENRT", StyleUrl: "#chp"}
	loc := &api.Coordinates{Latitude: 38.1, Longitude: -120.5}

	// Heuristic said CRITICAL; the model's "moderate" impact overrides to WARNING.
	crit := &api.Incident{Id: "c1", Severity: api.AlertSeverity_CRITICAL, Description: "Traffic Collision-Injury", Location: loc}
	apiCalls := 0
	s.enhanceIncident(testCtx(), crit, raw, &apiCalls)
	if enhancer.calls != 1 || apiCalls != 1 {
		t.Fatalf("expected one OpenAI call, got calls=%d budgetUsed=%d", enhancer.calls, apiCalls)
	}
	if crit.Description != "A collision with injuries occurred; emergency services are on scene." {
		t.Errorf("description not enhanced: %q", crit.Description)
	}
	if crit.CondensedSummary == "" || crit.Impact != api.AlertImpact_IMPACT_MODERATE || crit.Metadata["injuries"] != "reported" {
		t.Errorf("enhancement fields not applied: %+v", crit)
	}
	if crit.Severity != api.AlertSeverity_WARNING {
		t.Errorf("severity = %v, want WARNING (model impact 'moderate' overrides heuristic)", crit.Severity)
	}

	// Lower-severity incidents are enhanced too (distinct content -> new call).
	warn := &api.Incident{Id: "w1", Severity: api.AlertSeverity_INFO, Description: "Traffic Hazard", Location: loc}
	s.enhanceIncident(testCtx(), warn, caltrans.CaltransIncident{DescriptionText: "1125-Traffic Hazard", StyleUrl: "#chp"}, &apiCalls)
	if enhancer.calls != 2 || warn.CondensedSummary == "" {
		t.Fatalf("non-critical incident should be enhanced: calls=%d inc=%+v", enhancer.calls, warn)
	}

	// Same content again (e.g. next refresh): served from cache, no new call,
	// and no budget spent.
	crit2 := &api.Incident{Id: "c1", Severity: api.AlertSeverity_CRITICAL, Description: "Traffic Collision-Injury", Location: loc}
	s.enhanceIncident(testCtx(), crit2, raw, &apiCalls)
	if enhancer.calls != 2 || apiCalls != 2 {
		t.Fatalf("repeat content should hit cache: calls=%d budgetUsed=%d", enhancer.calls, apiCalls)
	}
	if crit2.CondensedSummary == "" || crit2.Severity != api.AlertSeverity_WARNING {
		t.Error("cached enhancement (incl. severity override) should still be applied")
	}
}

// When updated (re-hashed) incidents and never-enhanced incidents compete for
// the per-refresh budget, never-enhanced ones win — otherwise active incidents
// that keep appending detail lines starve the tail of the list forever.
func TestNormalizeIncidents_NeverEnhancedGetBudgetFirst(t *testing.T) {
	enhancer := &mockEnhancer{}
	s := enhancementTestService(enhancer)
	s.config = &config.Config{}
	area := motherLode()
	coord := &api.Coordinates{Latitude: 38.07, Longitude: -120.54}

	// 7 CHP incidents with distinct content; feed order 0..6.
	var chp []caltrans.CaltransIncident
	previouslyEnhanced := map[string]bool{}
	ids := []string{"260704AA0001", "260704AA0002", "260704AA0003", "260704AA0004", "260704AA0005", "260704AA0006", "260704AA0007"}
	for n, id := range ids {
		chp = append(chp, caltrans.CaltransIncident{
			FeedType:        caltrans.CHP_INCIDENT,
			Name:            "CHP Incident " + id,
			DescriptionHtml: `<h2 class="iw-title">1183-Trfc Collision-Injury</h2><p class="iw-text">Jun 25 2026  6:24PM <br> Location ` + id + `</p>`,
			DescriptionText: "collision update " + id, // distinct content -> distinct hash -> budget spent
			Coordinates:     coord,
		})
		// First 5 were enhanced on the previous refresh (their content has since
		// changed, so they'd re-consume budget); last 2 never were.
		if n < 5 {
			previouslyEnhanced[id] = true
		}
	}

	got := s.normalizeIncidents(testCtx(), area, previouslyEnhanced, chp)
	if len(got) != 7 {
		t.Fatalf("got %d incidents, want 7", len(got))
	}
	byID := map[string]*api.Incident{}
	for _, inc := range got {
		byID[inc.Id] = inc
	}

	// The two never-enhanced incidents must have gotten budget.
	for _, id := range ids[5:] {
		if byID[id].CondensedSummary == "" {
			t.Errorf("never-enhanced incident %s should have priority for the budget", id)
		}
	}
	// Budget is 5: exactly 3 of the previously-enhanced five get re-enhanced.
	reEnhanced := 0
	for _, id := range ids[:5] {
		if byID[id].CondensedSummary != "" {
			reEnhanced++
		}
	}
	if reEnhanced != maxIncidentEnhancementsPerRefresh-2 {
		t.Errorf("re-enhanced = %d, want %d (budget minus the two priority incidents)", reEnhanced, maxIncidentEnhancementsPerRefresh-2)
	}
	// List order is unchanged by the budget priority (feed order preserved).
	for n, inc := range got {
		if inc.Id != ids[n] {
			t.Fatalf("output order changed: position %d = %s, want %s", n, inc.Id, ids[n])
		}
	}
}

func TestSeverityFromImpact(t *testing.T) {
	cases := map[string]api.AlertSeverity{
		"severe":   api.AlertSeverity_CRITICAL,
		"moderate": api.AlertSeverity_WARNING,
		"light":    api.AlertSeverity_WARNING,
		"none":     api.AlertSeverity_INFO,
		" Severe ": api.AlertSeverity_CRITICAL, // case/whitespace tolerant
		"unknown":  api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED,
		"":         api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED,
	}
	for impact, want := range cases {
		if got := severityFromImpact(impact); got != want {
			t.Errorf("severityFromImpact(%q) = %v, want %v", impact, got, want)
		}
	}
}

// Once the per-refresh budget is exhausted, cache-miss enhancements are
// deferred (structural fields kept) rather than calling OpenAI.
func TestEnhanceIncident_BudgetCap(t *testing.T) {
	enhancer := &mockEnhancer{}
	s := enhancementTestService(enhancer)
	loc := &api.Coordinates{Latitude: 38.1, Longitude: -120.5}

	apiCalls := maxIncidentEnhancementsPerRefresh // budget already spent
	inc := &api.Incident{Id: "c9", Severity: api.AlertSeverity_CRITICAL, Description: "Car Fire", Location: loc}
	s.enhanceIncident(testCtx(), inc, caltrans.CaltransIncident{DescriptionText: "CFIRE-Car Fire", StyleUrl: "#chp"}, &apiCalls)

	if enhancer.calls != 0 {
		t.Fatalf("budget-capped enhancement must not call OpenAI, got %d calls", enhancer.calls)
	}
	if inc.Description != "Car Fire" || inc.CondensedSummary != "" {
		t.Errorf("capped incident should keep structural fields: %+v", inc)
	}
}
