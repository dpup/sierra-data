package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/caltrans"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/lib/alerts"
)

// ListIncidents returns region-wide CHP/Caltrans dispatch incidents for a
// configured area (issue #7). Unlike the alerts embedded in each Road, this is
// a flat list scoped only by geography, with no per-route classification.
// Every incident is AI-enhanced (readable description, condensed summary,
// impact, metadata — with severity driven by the model's impact assessment),
// capped per refresh and served from the 24h content-hash cache, so the whole
// region is still surfaced cheaply (~100 mini-model calls/day).
func (s *RoadsService) ListIncidents(ctx context.Context, req *api.ListIncidentsRequest) (*api.ListIncidentsResponse, error) {
	logging.Infow(ctx, "ListIncidents called", "area", req.Area)

	area, ok := s.resolveIncidentArea(req.Area)
	if !ok {
		// area is a path identity (like a road/location id), so an unknown one
		// is NotFound (404), consistent with GetRoad/GetLocationWeather.
		return nil, status.Errorf(codes.NotFound, "unknown incident area: %q", req.Area)
	}

	cacheKey := fmt.Sprintf("incidents:%s", area.ID)

	// Serve cached data when fresh; the underlying KML feeds change on the order
	// of minutes and are shared with the roads refresh.
	var cachedIncidents []*api.Incident
	entry, found, err := s.cache.GetWithMetadata(cacheKey, &cachedIncidents)
	if err != nil {
		logging.Errorw(ctx, "Cache error", "error", err, "cache_key", cacheKey)
	}
	if found && !s.cache.IsStale(cacheKey) {
		var lastUpdated *timestamppb.Timestamp
		if entry != nil {
			lastUpdated = timestamppb.New(entry.CreatedAt)
		}
		return &api.ListIncidentsResponse{
			Incidents:   cachedIncidents,
			LastUpdated: lastUpdated,
			Area:        area.ID,
		}, nil
	}

	incidents, err := s.refreshIncidents(ctx, area)
	if err != nil {
		// Fall back to stale cache rather than erroring if we have anything.
		if found {
			logging.Errorw(ctx, "Incident refresh failed, returning stale cache", "error", err)
			var lastUpdated *timestamppb.Timestamp
			if entry != nil {
				lastUpdated = timestamppb.New(entry.CreatedAt)
			}
			return &api.ListIncidentsResponse{
				Incidents:   cachedIncidents,
				LastUpdated: lastUpdated,
				Area:        area.ID,
			}, nil
		}
		return nil, fmt.Errorf("failed to refresh incidents: %w", err)
	}

	if err := s.cache.Set(cacheKey, incidents, s.config.Roads.CaltransFeeds.CHPIncidents.RefreshInterval, "incidents"); err != nil {
		logging.Errorw(ctx, "Failed to cache incidents", "error", err)
	}

	return &api.ListIncidentsResponse{
		Incidents:   incidents,
		LastUpdated: timestamppb.Now(),
		Area:        area.ID,
	}, nil
}

// resolveIncidentArea looks up a configured area by id. The id is required (it
// is a path param); there is no default area.
func (s *RoadsService) resolveIncidentArea(id string) (config.IncidentArea, bool) {
	for _, a := range s.config.Roads.IncidentAreas {
		if a.ID == id {
			return a, true
		}
	}
	return config.IncidentArea{}, false
}

// refreshIncidents fetches CHP and lane-closure feeds and converts the ones
// inside the area bounds into structured incidents.
func (s *RoadsService) refreshIncidents(ctx context.Context, area config.IncidentArea) ([]*api.Incident, error) {
	chpIncidents, chpErr := s.caltransClient.ParseCHPIncidents(ctx)
	laneClosures, lcErr := s.caltransClient.ParseLaneClosures(ctx)
	if chpErr != nil && lcErr != nil {
		return nil, fmt.Errorf("both incident feeds failed: chp=%v lanes=%v", chpErr, lcErr)
	}

	incidents := s.normalizeIncidents(ctx, area, s.previouslyEnhancedIncidents(area), chpIncidents, laneClosures)

	logging.Infow(ctx, "Region-wide incidents refreshed",
		"area", area.ID,
		"chp_total", len(chpIncidents),
		"lane_total", len(laneClosures),
		"in_area", len(incidents))

	return incidents, nil
}

// maxIncidentEnhancementsPerRefresh bounds OpenAI calls (and request latency)
// on a single incidents refresh. Content-hash cache hits don't count against
// it, so a backlog larger than the cap finishes enhancing over the next few
// refresh cycles rather than never.
const maxIncidentEnhancementsPerRefresh = 5

// previouslyEnhancedIncidents returns the ids that carried an AI enhancement
// in the last served list (stale cache included). Used to prioritize the
// per-refresh budget: active CHP incidents get new detail lines appended,
// which changes their content hash and makes already-enhanced incidents
// compete for budget again — without prioritization they starve incidents
// that have never been enhanced at all.
func (s *RoadsService) previouslyEnhancedIncidents(area config.IncidentArea) map[string]bool {
	enhanced := make(map[string]bool)
	var prev []*api.Incident
	if _, found, _ := s.cache.GetWithMetadata(fmt.Sprintf("incidents:%s", area.ID), &prev); found {
		for _, p := range prev {
			if p.CondensedSummary != "" {
				enhanced[p.Id] = true
			}
		}
	}
	return enhanced
}

// normalizeIncidents builds a clean, one-entry-per-incident list from the raw
// feeds. It drops geometry-only placemarks and collapses duplicates: the
// Caltrans lane-closure feed emits a separate LineString "path" placemark per
// closure (no description) and repeats closures across directions, neither of
// which belongs in a flat list. CHP incidents come first, then lane closures.
// Incidents are AI-enhanced after dedupe (so repeats don't spend budget),
// never-enhanced ones first so budget-hungry re-enhancements of updated
// incidents can't starve them.
func (s *RoadsService) normalizeIncidents(ctx context.Context, area config.IncidentArea, previouslyEnhanced map[string]bool, lists ...[]caltrans.CaltransIncident) []*api.Incident {
	type pendingEnhancement struct {
		inc *api.Incident
		in  caltrans.CaltransIncident
	}
	var incidents []*api.Incident
	var pending []pendingEnhancement
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, in := range list {
			inc := s.buildIncident(in, area)
			if inc == nil || inc.Description == "" {
				continue // outside bounds, no coordinates, or a geometry-only placemark
			}
			if inc.Id != "" {
				if seen[inc.Id] {
					continue
				}
				seen[inc.Id] = true
			}
			incidents = append(incidents, inc)
			pending = append(pending, pendingEnhancement{inc: inc, in: in})
		}
	}

	// Spend the enhancement budget on never-enhanced incidents first (stable,
	// so feed order is preserved within each group). Cache hits are free
	// regardless of position.
	sort.SliceStable(pending, func(i, j int) bool {
		return !previouslyEnhanced[pending[i].inc.Id] && previouslyEnhanced[pending[j].inc.Id]
	})
	apiCalls := 0
	for _, p := range pending {
		s.enhanceIncident(ctx, p.inc, p.in, &apiCalls)
	}

	return incidents
}

// enhanceIncident enriches an incident through the shared AI pipeline (same
// content-hash 24h cache as road alerts, so an incident that also appears as a
// road alert costs one OpenAI call, not two). Every incident is eligible: the
// model's impact assessment then drives `severity`, replacing the keyword
// heuristic (which remains only as the placeholder for incidents not yet
// enhanced — deferred by the per-refresh budget or failed). Enhancement
// failures keep the structural fields untouched — enrichment is strictly
// additive, never load-bearing.
func (s *RoadsService) enhanceIncident(ctx context.Context, inc *api.Incident, in caltrans.CaltransIncident, apiCalls *int) {
	if s.alertEnhancer == nil {
		return
	}

	raw := alerts.RawAlert{
		ID:          inc.Id,
		Title:       inc.Description, // humanized type line, e.g. "Traffic Collision-Injury"
		Description: in.DescriptionText,
		Location:    fmt.Sprintf("%s (%.4f, %.4f)", inc.LocationDescription, inc.Location.Latitude, inc.Location.Longitude),
		StyleUrl:    in.StyleUrl,
		Timestamp:   time.Now(),
	}

	enhanced, calledAPI, err := s.enhanceRawAlert(ctx, raw, *apiCalls < maxIncidentEnhancementsPerRefresh)
	if calledAPI {
		*apiCalls++
	}
	if err != nil {
		if errors.Is(err, errEnhancementBudget) {
			logging.Infow(ctx, "Incident enhancement deferred to next refresh (per-refresh budget reached)",
				"id", inc.Id, "budget", maxIncidentEnhancementsPerRefresh)
		} else {
			logging.Errorw(ctx, "Incident enhancement failed, keeping structural fields", "id", inc.Id, "error", err)
		}
		return
	}

	if details := strings.TrimSpace(enhanced.StructuredDescription.Details); details != "" {
		inc.Description = details
	}
	inc.CondensedSummary = enhanced.CondensedSummary
	inc.Impact = mapAlertImpact(enhanced.StructuredDescription.Impact)
	if sev := severityFromImpact(enhanced.StructuredDescription.Impact); sev != api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED {
		inc.Severity = sev
	}
	if loc := strings.TrimSpace(enhanced.StructuredDescription.Location.Description); loc != "" {
		inc.LocationDescription = loc
	}
	if len(enhanced.StructuredDescription.AdditionalInfo) > 0 {
		inc.Metadata = make(map[string]string, len(enhanced.StructuredDescription.AdditionalInfo))
		for k, v := range enhanced.StructuredDescription.AdditionalInfo {
			inc.Metadata[k] = v
		}
	}
}

// severityFromImpact maps the AI-assessed impact onto the shared severity
// scale, mirroring the roads mapping in determineAlertSeverity. An unknown
// impact returns UNSPECIFIED so the caller keeps the heuristic severity.
func severityFromImpact(impact string) api.AlertSeverity {
	switch strings.ToLower(strings.TrimSpace(impact)) {
	case "severe":
		return api.AlertSeverity_CRITICAL
	case "moderate", "light":
		return api.AlertSeverity_WARNING
	case "none":
		return api.AlertSeverity_INFO
	default:
		return api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED
	}
}

// buildIncident converts a Caltrans incident into the API representation,
// returning nil if it has no coordinates or falls outside the area bounds.
func (s *RoadsService) buildIncident(in caltrans.CaltransIncident, area config.IncidentArea) *api.Incident {
	if in.Coordinates == nil {
		return nil
	}
	if !area.Bounds.Contains(in.Coordinates.Latitude, in.Coordinates.Longitude) {
		return nil
	}

	d := parseIncidentDetail(in)

	description := humanizeIncidentType(d.title)
	if description == "" {
		description = in.DescriptionText
	}
	locationDesc := d.location
	if locationDesc == "" {
		locationDesc = in.Name
	}

	inc := &api.Incident{
		Id:                  incidentID(in, d.logNumber),
		Type:                incidentType(in),
		Severity:            incidentSeverity(in, d.title),
		Location:            &api.Coordinates{Latitude: in.Coordinates.Latitude, Longitude: in.Coordinates.Longitude},
		LocationDescription: locationDesc,
		Description:         description,
		Status:              api.IncidentStatus_ACTIVE,
		LogNumber:           d.logNumber,
		Area:                area.ID,
	}
	if !d.started.IsZero() {
		inc.Started = timestamppb.New(d.started)
	}
	if !d.lastUpdated.IsZero() {
		inc.LastUpdated = timestamppb.New(d.lastUpdated)
	} else {
		inc.LastUpdated = timestamppb.New(in.LastFetched)
	}
	return inc
}

// incidentID builds a stable identifier, preferring the CHP log number.
func incidentID(in caltrans.CaltransIncident, logNumber string) string {
	if logNumber != "" {
		return logNumber
	}
	// Fall back to a slug of the name for lane closures without a log number.
	slug := strings.ToLower(strings.TrimSpace(in.Name))
	slug = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

// chpCodePrefixRe matches a leading CHP dispatch code (numeric like "1182" or
// all-caps like "CZP"/"CFIRE") followed by a hyphen.
var chpCodePrefixRe = regexp.MustCompile(`^(?:[0-9]+|[A-Z]{2,})-`)

// chpAbbreviations expands the abbreviations CHP uses in incident type text.
// Applied with word boundaries so street names aren't mangled.
var chpAbbreviations = []struct {
	re   *regexp.Regexp
	full string
}{
	{regexp.MustCompile(`(?i)\bTrfc\b`), "Traffic"},
	{regexp.MustCompile(`(?i)\bUnkn\b`), "Unknown"},
	{regexp.MustCompile(`(?i)\bInj\b`), "Injury"},
	{regexp.MustCompile(`(?i)\bVehs\b`), "Vehicles"},
	{regexp.MustCompile(`(?i)\bVeh\b`), "Vehicle"},
}

// humanizeIncidentType turns raw CHP type text into something readable, e.g.
// "1182-Trfc Collision-No Inj" -> "Traffic Collision - No Injury",
// "CFIRE-Car Fire" -> "Car Fire". Lane-closure titles (no code prefix) pass
// through largely untouched.
func humanizeIncidentType(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Drop a leading dispatch code ("1182-", "CZP-").
	s = chpCodePrefixRe.ReplaceAllString(s, "")
	// Expand known abbreviations.
	for _, ab := range chpAbbreviations {
		s = ab.re.ReplaceAllString(s, ab.full)
	}
	return strings.TrimSpace(s)
}

func incidentType(in caltrans.CaltransIncident) api.AlertType {
	switch in.FeedType {
	case caltrans.LANE_CLOSURE:
		return api.AlertType_CLOSURE
	case caltrans.CHP_INCIDENT:
		return api.AlertType_INCIDENT
	default:
		return api.AlertType_ALERT_TYPE_UNSPECIFIED
	}
}

// incidentSeverity is a lightweight, non-AI keyword heuristic based on the
// incident text and feed type. It is only the placeholder severity for
// incidents that haven't been AI-enhanced yet (deferred by the per-refresh
// budget, or enhancement failed) — once enhanced, severityFromImpact
// overrides it with the model's assessment.
func incidentSeverity(in caltrans.CaltransIncident, typeText string) api.AlertSeverity {
	lower := strings.ToLower(typeText + " " + in.DescriptionText + " " + in.StyleUrl)

	switch {
	case strings.Contains(lower, "full-closure"),
		strings.Contains(lower, "fatal"),
		strings.Contains(lower, "injury"),
		strings.Contains(lower, "fire"),
		strings.Contains(lower, "overturn"):
		return api.AlertSeverity_CRITICAL
	case in.FeedType == caltrans.LANE_CLOSURE,
		strings.Contains(lower, "collision"),
		strings.Contains(lower, "hazard"),
		strings.Contains(lower, "closure"):
		return api.AlertSeverity_WARNING
	case strings.Contains(lower, "assist"),
		strings.Contains(lower, "maintenance"),
		strings.Contains(lower, "traffic advisory"):
		return api.AlertSeverity_INFO
	default:
		return api.AlertSeverity_WARNING
	}
}

// incidentDetail holds the structured fields extracted from an incident's
// description markup.
type incidentDetail struct {
	logNumber   string
	title       string // incident type / headline text
	location    string // human-readable location
	started     time.Time
	lastUpdated time.Time
}

var (
	// 2026 "infowindow" markup.
	iwTitleRe     = regexp.MustCompile(`(?is)<h2[^>]*class="iw-title"[^>]*>(.*?)</h2>`)
	iwTextRe      = regexp.MustCompile(`(?is)<p[^>]*class="iw-text"[^>]*>(.*?)</p>`)
	chpLabelRe    = regexp.MustCompile(`(?i)CHP Incident\s+([A-Za-z0-9]+)`)
	logNumberRe   = regexp.MustCompile(`(?i)Log Number:\s*([A-Za-z0-9]+)`)
	closureIDRe   = regexp.MustCompile(`(?i)Closure ID:\s*([A-Za-z0-9]+)`)
	chpLogTokenRe = regexp.MustCompile(`([0-9]{6}[A-Z]{2}[0-9]{4})`)

	// Legacy (pre-2026) markup, kept for the older test fixtures.
	legacyParaRe = regexp.MustCompile(`(?is)<p[^>]*align="left"[^>]*>(.*?)</p>`)

	brRe          = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRe         = regexp.MustCompile(`<[^>]*>`)
	lastUpdatedRe = regexp.MustCompile(`(?i)Last updated:\s*(?:<strong>\s*)?([0-9]{1,2}/[0-9]{1,2}/[0-9]{4})(?:\s*</strong>)?\s*([0-9]{1,2}:[0-9]{2}[ap]m)`)
)

// parseIncidentDetail extracts structured fields from a Caltrans incident,
// handling both the 2026 iw-* markup and the legacy format.
func parseIncidentDetail(in caltrans.CaltransIncident) incidentDetail {
	html := in.DescriptionHtml
	d := incidentDetail{lastUpdated: parseLastUpdatedTime(html)}

	// Log number: CHP label, then explicit "Log Number" / "Closure ID".
	d.logNumber = extractLogNumber(in, html)

	// Title from iw-title (new) if present.
	if m := iwTitleRe.FindStringSubmatch(html); len(m) > 1 {
		d.title = cleanSegment(m[1])
	}

	if texts := iwTextRe.FindAllStringSubmatch(html, -1); len(texts) > 0 {
		// New format. CHP first text is "<time> <br> <location>"; lane closures
		// put the location/extent directly in the first text.
		segs := splitBR(texts[0][1])
		if in.FeedType == caltrans.CHP_INCIDENT && len(segs) > 0 {
			d.started = parseCHPTime(segs[0])
			if len(segs) > 1 {
				d.location = segs[1]
			}
		} else if len(segs) > 0 {
			d.location = strings.Join(segs, " ")
		}
	} else if m := legacyParaRe.FindStringSubmatch(html); len(m) > 1 {
		// Legacy format: "<time> <br> <type> <br> <location>".
		segs := splitBR(m[1])
		if len(segs) > 0 {
			d.started = parseCHPTime(segs[0])
		}
		if len(segs) > 1 && d.title == "" {
			d.title = segs[1]
		}
		if len(segs) > 2 {
			d.location = segs[2]
		}
	}

	return d
}

// extractLogNumber pulls the incident's identifier from its name or description.
func extractLogNumber(in caltrans.CaltransIncident, html string) string {
	return logNumberFromText(in.Name, html)
}

// logNumberFromText extracts the stable CHP log / closure id from a title
// (e.g. "CHP Incident 250916ST0066") and/or description HTML. Shared by the
// incidents feed and per-road alerts so the same event carries the same id.
func logNumberFromText(title, html string) string {
	// CHP log token in the title (e.g. "CHP Incident 250916ST0066").
	if m := chpLogTokenRe.FindString(title); m != "" {
		return m
	}
	if m := chpLabelRe.FindStringSubmatch(title); len(m) > 1 {
		return m[1]
	}
	if m := chpLabelRe.FindStringSubmatch(html); len(m) > 1 {
		return m[1]
	}
	// Lane closure identifiers.
	if m := closureIDRe.FindStringSubmatch(html); len(m) > 1 {
		return m[1]
	}
	if m := logNumberRe.FindStringSubmatch(html); len(m) > 1 {
		return m[1]
	}
	return ""
}

// pacificTime is the timezone Caltrans/CHP feeds report times in. Times are
// parsed in this location so the resulting timestamps are accurate.
var pacificTime = mustLoadPacific()

func mustLoadPacific() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.UTC
}

func parseLastUpdatedTime(html string) time.Time {
	m := lastUpdatedRe.FindStringSubmatch(html)
	if len(m) < 3 {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("1/2/2006 3:04pm", strings.TrimSpace(m[1]+" "+m[2]), pacificTime); err == nil {
		return t
	}
	return time.Time{}
}

// parseCHPTime parses CHP timestamps like "Jun 25 2026  6:24PM" (note the
// irregular double space and 12-hour clock), interpreted as Pacific time.
func parseCHPTime(s string) time.Time {
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace
	for _, layout := range []string{"Jan 2 2006 3:04PM", "Jan 2 2006 3:04pm"} {
		if t, err := time.ParseInLocation(layout, s, pacificTime); err == nil {
			return t
		}
	}
	return time.Time{}
}

// splitBR splits an HTML fragment on <br> and returns cleaned, non-empty segments.
func splitBR(fragment string) []string {
	var segs []string
	for _, p := range brRe.Split(fragment, -1) {
		if clean := cleanSegment(p); clean != "" {
			segs = append(segs, clean)
		}
	}
	return segs
}

func cleanSegment(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(tagRe.ReplaceAllString(s, " ")), " "))
}
