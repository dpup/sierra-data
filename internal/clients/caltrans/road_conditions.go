package caltrans

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
