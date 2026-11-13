package caltrans

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RoadConditionType represents the type of road condition
type RoadConditionType int

const (
	CONDITION_CLOSURE     RoadConditionType = iota // Full road closure
	CONDITION_CHAIN                                // Chain control requirement
	CONDITION_RESTRICTION                          // Traffic restriction (1-way, etc.)
	CONDITION_INFO                                 // Informational message
)

// RoadCondition represents a parsed condition from roads.dot.ca.gov
type RoadCondition struct {
	Highway     string            // Highway number, e.g., "4"
	Type        RoadConditionType // Type of condition
	Description string            // Full condition text
	Area        string            // Area designation, e.g., "CENTRAL CALIFORNIA AREA"
	Reason      string            // Extracted reason (snow, construction, winter, etc.)
	LastUpdated string            // Timestamp from the page
}

const roadConditionsURLPattern = "https://roads.dot.ca.gov/roadscell.php?roadnumber=%s"

// ParseRoadConditions fetches and parses the Caltrans road conditions page
// for the given highway number (e.g., "4" for Highway 4).
func (p *FeedParser) ParseRoadConditions(ctx context.Context, highwayNumber string) ([]RoadCondition, error) {
	url := fmt.Sprintf(roadConditionsURLPattern, highwayNumber)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch road conditions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error %d fetching road conditions for highway %s", resp.StatusCode, highwayNumber)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read road conditions response: %w", err)
	}

	return ParseRoadConditionsHTML(string(body), highwayNumber)
}

// ParseRoadConditionsHTML parses the HTML content of a road conditions page.
// Exported for testing.
func ParseRoadConditionsHTML(html string, highwayNumber string) ([]RoadCondition, error) {
	var conditions []RoadCondition

	// Extract last updated timestamp
	lastUpdated := extractLastUpdated(html)

	// Find the section for this highway: <h3>SR {number}</h3>
	// The content is between this <h3> and the next <hr> or <h3>
	sectionPattern := regexp.MustCompile(`(?is)<h3>\s*SR\s+` + regexp.QuoteMeta(highwayNumber) + `\s*</h3>(.*?)(?:<hr|<h3)`)
	sectionMatch := sectionPattern.FindStringSubmatch(html)
	if sectionMatch == nil {
		// No conditions found for this highway
		return nil, nil
	}

	sectionHTML := sectionMatch[1]

	// Extract area designation from first <strong> tag
	area := ""
	areaPattern := regexp.MustCompile(`(?i)<strong>\[([^\]]+)\]</strong>`)
	if areaMatch := areaPattern.FindStringSubmatch(sectionHTML); len(areaMatch) > 1 {
		area = strings.TrimSpace(areaMatch[1])
	}

	// Extract individual conditions from <p> tags
	pPattern := regexp.MustCompile(`(?is)<p>(.*?)</p>`)
	pMatches := pPattern.FindAllStringSubmatch(sectionHTML, -1)

	for _, pMatch := range pMatches {
		if len(pMatch) < 2 {
			continue
		}

		// Clean up the text: remove HTML tags, normalize whitespace
		text := extractTextFromHTML(pMatch[1])
		if text == "" {
			continue
		}

		// Strip area designation prefix if present (e.g., "[IN THE CENTRAL CALIFORNIA AREA]")
		// The area label and first condition text can appear in the same <p> tag
		if strings.HasPrefix(text, "[") && strings.Contains(text, "]") {
			idx := strings.Index(text, "]")
			text = strings.TrimSpace(text[idx+1:])
			if text == "" {
				continue
			}
		}

		// Skip informational notices about chain control location updates
		if strings.Contains(strings.ToLower(text), "please research") ||
			strings.Contains(strings.ToLower(text), "caltrans is currently working") {
			continue
		}

		condType := classifyCondition(text)
		reason := extractReason(text)

		conditions = append(conditions, RoadCondition{
			Highway:     highwayNumber,
			Type:        condType,
			Description: text,
			Area:        area,
			Reason:      reason,
			LastUpdated: lastUpdated,
		})
	}

	return conditions, nil
}

// chainRequirement reads every level's wording: R1 "Chains or snow tires are
// required", R2 "Chains are required on all vehicles except 4-wheel-drive
// vehicles with snow tires", R3 "Chains are required on all vehicles", and
// "Chains or traction devices are required".
var chainRequirement = regexp.MustCompile(`\b(?:chains|snow tires|traction devices)\b[^.]{0,40}?\brequired\b`)

// classifyCondition determines the type of road condition from its text
func classifyCondition(text string) RoadConditionType {
	lower := strings.ToLower(text)

	// Check for closure
	if strings.Contains(lower, "is closed") || strings.Contains(lower, "are closed") ||
		strings.HasPrefix(lower, "closed") {
		return CONDITION_CLOSURE
	}

	// Check for chain control
	if chainRequirement.MatchString(lower) || strings.Contains(lower, "chain control") {
		return CONDITION_CHAIN
	}

	// Check for restrictions
	if strings.Contains(lower, "controlled traffic") || strings.Contains(lower, "1-way") ||
		strings.Contains(lower, "one-way") || strings.Contains(lower, "restricted") ||
		strings.Contains(lower, "no traffic permitted") {
		return CONDITION_RESTRICTION
	}

	return CONDITION_INFO
}

// extractReason extracts the reason for a condition from its text
func extractReason(text string) string {
	lower := strings.ToLower(text)

	reasonPatterns := []struct {
		pattern string
		reason  string
	}{
		{"due to snow", "snow"},
		{"due to ice", "ice"},
		{"due to construction", "construction"},
		{"due to rock slide", "rockslide"},
		{"due to mudslide", "mudslide"},
		{"due to fire", "fire"},
		{"due to flooding", "flooding"},
		{"due to accident", "accident"},
		{"for the winter", "winter_closure"},
		{"winter closure", "winter_closure"},
		{"for the season", "seasonal_closure"},
	}

	for _, rp := range reasonPatterns {
		if strings.Contains(lower, rp.pattern) {
			return rp.reason
		}
	}

	return ""
}

// extractLastUpdated extracts the last updated timestamp from the page
func extractLastUpdated(html string) string {
	// Pattern: "This highway information is the latest reported as of Tuesday, February 17th, 2026 at 10:25 PM."
	pattern := regexp.MustCompile(`(?i)latest reported as of\s+\w+,\s+(\w+\s+\d+\w*,\s+\d{4})\s+at\s+(\d{1,2}:\d{2}\s*[APap][Mm])`)
	match := pattern.FindStringSubmatch(html)
	if len(match) < 3 {
		return ""
	}

	// Clean ordinal suffixes (1st, 2nd, 3rd, 4th, etc.)
	dateStr := regexp.MustCompile(`(\d+)(st|nd|rd|th)`).ReplaceAllString(match[1], "$1")
	timeStr := strings.TrimSpace(match[2])

	// Parse: "February 17, 2026" + "10:25 PM"
	combined := dateStr + " " + timeStr
	layouts := []string{
		"January 2, 2006 3:04 PM",
		"January 2, 2006 3:04PM",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, combined); err == nil {
			return t.Format(time.RFC3339)
		}
	}

	return dateStr + " " + timeStr
}

// MatchConditionToSegment checks if a road condition applies to a specific
// road segment based on text matching against section names and location keywords.
// Returns true if the condition should be considered ON_ROUTE for this segment.
func MatchConditionToSegment(condition RoadCondition, sectionName string, locationKeywords []string) bool {
	condLower := strings.ToLower(condition.Description)

	// Check section endpoint names (e.g., "Arnold to Bear Valley" → "arnold", "bear valley")
	sectionTokens := extractSectionLocations(sectionName)
	for _, token := range sectionTokens {
		if strings.Contains(condLower, strings.ToLower(token)) {
			return true
		}
	}

	// Check configured location keywords
	for _, keyword := range locationKeywords {
		if strings.Contains(condLower, strings.ToLower(keyword)) {
			return true
		}
	}

	return false
}

// SeasonalClosure names one closure that recurs at the same place every year
// and lies past the end of a monitored segment: the Ebbetts Pass winter
// closure on SR 4, from the Mt Reba turnoff to the SR 89 junction.
type SeasonalClosure struct {
	Name string
	// Start and End are the names Caltrans uses for the two ends. An end of
	// the condition's span matches when it is made of these names and nothing
	// else ("the Jct of SR 207 /Mt Reba Turnoff/").
	Start, End []string
	// The start may carry an offset ("0.5 mi east of ..."), only in
	// StartOffsetDirection (the direction away from the segment) and only up
	// to MaxStartOffsetMiles. With no direction set, no offset is accepted.
	StartOffsetDirection string
	MaxStartOffsetMiles  float64
}

var conditionOffset = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(?:mi|mile|miles)\.?\s+(north|south|east|west)\s+of\s+`)

// MatchSeasonalClosure reports whether a condition is exactly the named
// seasonal closure: a closure whose span starts at the configured start (bare,
// or offset a short way in the configured direction) and ends at the
// configured end. Anything else is not it: a chain requirement over the same
// span, a different end, an offset the other way or a longer one, a closure
// "at" the start with no span, or any wording this cannot read. The caller
// then treats the condition as any other, on the segment, so a miss here
// errs towards closed.
func MatchSeasonalClosure(condition RoadCondition, closure SeasonalClosure) bool {
	if condition.Type != CONDITION_CLOSURE {
		return false
	}
	from, to := ConditionSpan(condition)
	if from == "" || to == "" {
		return false
	}
	if m := conditionOffset.FindStringSubmatch(from); m != nil {
		miles, err := strconv.ParseFloat(m[1], 64)
		if err != nil || closure.StartOffsetDirection == "" ||
			!strings.EqualFold(m[2], strings.TrimSpace(closure.StartOffsetDirection)) ||
			miles > closure.MaxStartOffsetMiles {
			return false
		}
		from = from[len(m[0]):]
	}
	return onlyNames(from, closure.Start) && onlyNames(to, closure.End)
}

// onlyNames reports whether text is one or more of names and nothing else
// but "the" and punctuation.
func onlyNames(text string, names []string) bool {
	text = strings.ToLower(text)
	sorted := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			sorted = append(sorted, n)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	found := false
	for _, n := range sorted {
		if strings.Contains(text, n) {
			found = true
			text = strings.ReplaceAll(text, n, " ")
		}
	}
	if !found {
		return false
	}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if word != "the" {
			return false
		}
	}
	return true
}

var (
	conditionClauseEnd = regexp.MustCompile(`\s+-\s+|-\s+(?:due|for)\b`)
	conditionCounty    = regexp.MustCompile(`\s*\([^)]*\bCo\)`)
	conditionClosedAt  = regexp.MustCompile(`(?i)^(?:is|are)?\s*closed\s+(?:from\s+|at\s+)?`)
	conditionFromTo    = regexp.MustCompile(`(?i)\bfrom\s+(.+?)\s+to\s+(.+)$`)
	conditionTo        = regexp.MustCompile(`(?i)\s+to\s+`)
	conditionHours     = regexp.MustCompile(`(?i)\s+(?:from\s+\d{3,4}\s+hrs|\d+\s+hrs\s+a\s+day)\b.*$`)
)

// ConditionSpan returns where a condition applies, in Caltrans's own words:
// "Is closed from 0.5 mi east of the Jct of SR 207 /Mt Reba Turnoff/ to the
// Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow ..." gives from "0.5
// mi east of the Jct of SR 207 /Mt Reba Turnoff/" and to "the Jct of SR 89
// /Ebbetts Pass/". A closure at one place has only from. Both are empty when
// the text does not read as a place or a span.
func ConditionSpan(condition RoadCondition) (from, to string) {
	clause := strings.TrimSpace(condition.Description)
	if loc := conditionClauseEnd.FindStringIndex(clause); loc != nil {
		clause = clause[:loc[0]]
	}
	clause = conditionHours.ReplaceAllString(clause, "")
	clean := func(s string) string {
		return strings.TrimSpace(conditionCounty.ReplaceAllString(strings.TrimSpace(s), ""))
	}

	if condition.Type == CONDITION_CLOSURE {
		loc := conditionClosedAt.FindStringIndex(clause)
		if loc == nil {
			return "", ""
		}
		rest := clause[loc[1]:]
		if cut := conditionTo.FindStringIndex(rest); cut != nil {
			return clean(rest[:cut[0]]), clean(rest[cut[1]:])
		}
		return clean(rest), ""
	}
	if m := conditionFromTo.FindStringSubmatch(clause); m != nil {
		return clean(m[1]), clean(m[2])
	}
	return "", ""
}

// extractSectionLocations extracts location names from a section description
// e.g., "Arnold to Bear Valley" → ["Arnold", "Bear Valley"]
// e.g., "Angels Camp to Murphys" → ["Angels Camp", "Murphys"]
func extractSectionLocations(section string) []string {
	// Split by common separators: "to", "-", "–"
	parts := regexp.MustCompile(`(?i)\s+to\s+|\s*[-–]\s*`).Split(section, -1)

	var locations []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			locations = append(locations, trimmed)
		}
	}
	return locations
}
