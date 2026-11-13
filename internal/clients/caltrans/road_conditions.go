package caltrans

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
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

// classifyCondition determines the type of road condition from its text
func classifyCondition(text string) RoadConditionType {
	lower := strings.ToLower(text)

	// Check for closure
	if strings.Contains(lower, "is closed") || strings.Contains(lower, "are closed") ||
		strings.HasPrefix(lower, "closed") {
		return CONDITION_CLOSURE
	}

	// Check for chain control
	if strings.Contains(lower, "chains are required") || strings.Contains(lower, "chains required") ||
		strings.Contains(lower, "chain control") {
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

// SegmentRelation says where a road condition sits relative to a monitored
// segment of the same highway.
type SegmentRelation int

const (
	// SegmentUnrelated: the text names no place known for this segment.
	SegmentUnrelated SegmentRelation = iota
	// SegmentOn: the condition is on the segment (or cannot be shown to lie
	// off it). It sets the segment's status.
	SegmentOn
	// SegmentPastEnd: every place the text names lies at or past the
	// segment's destination, so it starts where the segment ends or further
	// on. It does not set the segment's status.
	SegmentPastEnd
	// SegmentBeforeStart: the same, at or before the segment's origin.
	SegmentBeforeStart
)

// SegmentPlaces are the place names that locate a text-only condition
// relative to a monitored segment. roads.dot.ca.gov conditions have no
// geometry, so the names are all there is.
type SegmentPlaces struct {
	// Origin and Destination are the segment's two ends ("Arnold", "Bear
	// Valley"). An end is a boundary: on its own it does not say which side a
	// condition is on.
	Origin, Destination string
	// On are places along the segment, strictly between its ends.
	On []string
	// BeforeStart are places past the origin, off the segment.
	BeforeStart []string
	// PastEnd are places past the destination, off the segment.
	PastEnd []string
}

// SectionPlaces builds SegmentPlaces from a section name ("Arnold to Bear
// Valley") and its keyword lists.
func SectionPlaces(section string, on, beforeStart, pastEnd []string) SegmentPlaces {
	p := SegmentPlaces{On: on, BeforeStart: beforeStart, PastEnd: pastEnd}
	if ends := extractSectionLocations(section); len(ends) > 0 {
		p.Origin = ends[0]
		if len(ends) > 1 {
			p.Destination = ends[len(ends)-1]
		}
	}
	return p
}

// RelateConditionToSegment places a condition relative to a segment from the
// places its text names.
//
// The rule errs towards SegmentOn: a condition is put past an end only when
// the text names a place known to be past that end and nothing on the segment
// or on its other side. So the Ebbetts Pass winter closure ("closed from 0.5
// mi east of ... /Mt Reba Turnoff/ to ... /Ebbetts Pass/", both past Bear
// Valley) is SegmentPastEnd for Arnold to Bear Valley, while "closed from
// Arnold to the Mt Reba turnoff" runs the length of it and is SegmentOn.
func RelateConditionToSegment(condition RoadCondition, places SegmentPlaces) SegmentRelation {
	// Longest names first, each match blanked out, so "Sonora" is not found
	// inside "Sonora Pass" nor "Arnold" inside a longer place that contains it.
	type known struct {
		name string
		kind *bool
	}
	var on, origin, destination, before, past bool
	var all []known
	add := func(kind *bool, list ...string) {
		for _, name := range list {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				all = append(all, known{name, kind})
			}
		}
	}
	add(&on, places.On...)
	add(&origin, places.Origin)
	add(&destination, places.Destination)
	add(&before, places.BeforeStart...)
	add(&past, places.PastEnd...)
	sort.SliceStable(all, func(i, j int) bool { return len(all[i].name) > len(all[j].name) })
	text := strings.ToLower(condition.Description)
	for _, k := range all {
		if strings.Contains(text, k.name) {
			*k.kind = true
			text = strings.ReplaceAll(text, k.name, " ")
		}
	}

	switch {
	case on:
		return SegmentOn
	case (before || origin) && (past || destination):
		return SegmentOn // names both sides: it runs through the segment
	case past:
		return SegmentPastEnd
	case before:
		return SegmentBeforeStart
	case origin || destination:
		return SegmentOn // at one end, extent unknown
	}
	return SegmentUnrelated
}

// MatchConditionToSegment checks if a road condition applies to a specific
// road segment based on text matching against section names and location keywords.
// Returns true if the condition should be considered ON_ROUTE for this segment.
func MatchConditionToSegment(condition RoadCondition, sectionName string, locationKeywords []string) bool {
	return RelateConditionToSegment(condition, SectionPlaces(sectionName, locationKeywords, nil, nil)) == SegmentOn
}

var (
	conditionClauseEnd = regexp.MustCompile(`\s+-\s+|-\s+(?:due|for)\b`)
	conditionCounty    = regexp.MustCompile(`\s*\([^)]*\bCo\)\s*$`)
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
