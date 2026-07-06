package caltrans

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/lib/geo"
)

// CaltransFeedType represents the type of Caltrans feed
type CaltransFeedType int

const (
	CHAIN_CONTROL CaltransFeedType = iota
	LANE_CLOSURE
	CHP_INCIDENT
)

// HTTPDoer interface for HTTP clients (for testability)
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// FeedParser processes Caltrans KML feeds
// Implementation per research.md lines 49-67
type FeedParser struct {
	HTTPClient HTTPDoer
	geoUtils   geo.GeoUtils
}

// CaltransIncident represents parsed incident data from KML feeds
// Structure per data-model.md lines 66-78
type CaltransIncident struct {
	FeedType        CaltransFeedType
	Name            string
	DescriptionHtml string
	DescriptionText string
	StyleUrl        string
	Coordinates     *api.Coordinates // Point location (for incidents)
	AffectedArea    *api.Polyline    // Polyline/polygon for closures
	ParsedStatus    string
	ParsedDates     []string
	LastFetched     time.Time
}

// ChainControlData represents parsed chain control information from KML
type ChainControlData struct {
	Highway       string           // e.g., "US 50", "Highway 89"
	Direction     string           // e.g., "Eastbound", "Northbound"
	Level         string           // "R1", "R2", "R3"
	LocationName  string           // e.g., "Twin Bridges", "Emerald Bay"
	Coordinates   *api.Coordinates // Where chain control starts
	EffectiveTime string           // ISO 8601 timestamp
	Description   string           // Human-readable requirements
	LastUpdated   string           // When data was last updated
	MessageID     string           // Caltrans message ID
	District      string           // Caltrans district number
}

// KML XML structures for parsing
type KML struct {
	XMLName  xml.Name `xml:"kml"`
	Document Document `xml:"Document"`
}

type Document struct {
	XMLName    xml.Name    `xml:"Document"`
	Name       string      `xml:"name"`
	Placemarks []Placemark `xml:"Placemark"`
	Folders    []Folder    `xml:"Folder"`
}

type Folder struct {
	XMLName    xml.Name    `xml:"Folder"`
	Name       string      `xml:"name"`
	Placemarks []Placemark `xml:"Placemark"`
}

type Placemark struct {
	XMLName       xml.Name      `xml:"Placemark"`
	Name          string        `xml:"name"`
	Description   string        `xml:"description"`
	StyleURL      string        `xml:"styleUrl"`
	Point         Point         `xml:"Point"`
	LineString    LineString    `xml:"LineString"`
	Polygon       Polygon       `xml:"Polygon"`
	MultiGeometry MultiGeometry `xml:"MultiGeometry"`
}

type Point struct {
	XMLName     xml.Name `xml:"Point"`
	Coordinates string   `xml:"coordinates"`
}

type LineString struct {
	XMLName     xml.Name `xml:"LineString"`
	Coordinates string   `xml:"coordinates"`
}

type Polygon struct {
	XMLName       xml.Name        `xml:"Polygon"`
	OuterBoundary OuterBoundary   `xml:"outerBoundaryIs"`
	InnerBoundary []InnerBoundary `xml:"innerBoundaryIs"`
}

type OuterBoundary struct {
	XMLName    xml.Name   `xml:"outerBoundaryIs"`
	LinearRing LinearRing `xml:"LinearRing"`
}

type InnerBoundary struct {
	XMLName    xml.Name   `xml:"innerBoundaryIs"`
	LinearRing LinearRing `xml:"LinearRing"`
}

type LinearRing struct {
	XMLName     xml.Name `xml:"LinearRing"`
	Coordinates string   `xml:"coordinates"`
}

type MultiGeometry struct {
	XMLName     xml.Name     `xml:"MultiGeometry"`
	Points      []Point      `xml:"Point"`
	LineStrings []LineString `xml:"LineString"`
	Polygons    []Polygon    `xml:"Polygon"`
}

// NewFeedParser creates a new Caltrans KML feed parser
func NewFeedParser() *FeedParser {
	return &FeedParser{
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		geoUtils: geo.NewGeoUtils(),
	}
}

// ParseChainControls processes chain control KML feed
// URL from research.md line 71
func (p *FeedParser) ParseChainControls(ctx context.Context) ([]CaltransIncident, error) {
	return p.parseKMLFeed(ctx, "https://quickmap.dot.ca.gov/data/cc.kml", CHAIN_CONTROL)
}

// ParseChainControlsDetailed processes chain control KML feed with detailed parsing
// Returns structured chain control data with level, location, and timing info
func (p *FeedParser) ParseChainControlsDetailed(ctx context.Context) ([]ChainControlData, error) {
	incidents, err := p.ParseChainControls(ctx)
	if err != nil {
		return nil, err
	}
	return p.parseChainControlDetails(incidents), nil
}

// parseChainControlDetails extracts detailed chain control info from incidents
func (p *FeedParser) parseChainControlDetails(incidents []CaltransIncident) []ChainControlData {
	var controls []ChainControlData

	for _, incident := range incidents {
		control := ChainControlData{
			Coordinates: incident.Coordinates,
		}

		// Parse name: "Eastbound US 50 Chain Control level R-2"
		control.Direction, control.Highway, control.Level = parseChainControlName(incident.Name)

		// Parse description HTML for location, effective time, and requirements
		control.LocationName, control.EffectiveTime, control.Description, control.LastUpdated, control.District, control.MessageID = parseChainControlDescription(incident.DescriptionHtml)

		controls = append(controls, control)
	}

	return controls
}

// parseChainControlName extracts direction, highway, and level from the name
// Example: "Eastbound US 50 Chain Control level R-2"
func parseChainControlName(name string) (direction, highway, level string) {
	// Extract direction
	directionPattern := regexp.MustCompile(`(?i)^(Eastbound|Westbound|Northbound|Southbound)\s+`)
	if match := directionPattern.FindStringSubmatch(name); len(match) > 1 {
		direction = match[1]
		name = directionPattern.ReplaceAllString(name, "")
	}

	// Extract level (R-1, R-2, R-3)
	levelPattern := regexp.MustCompile(`(?i)R-?([123])`)
	if match := levelPattern.FindStringSubmatch(name); len(match) > 1 {
		level = "R" + match[1]
	}

	// Extract highway name (everything before "Chain Control")
	highwayPattern := regexp.MustCompile(`(?i)^(.+?)\s+Chain\s+Control`)
	if match := highwayPattern.FindStringSubmatch(name); len(match) > 1 {
		highway = strings.TrimSpace(match[1])
	}

	return direction, highway, level
}

// parseChainControlDescription extracts details from the HTML description
func parseChainControlDescription(html string) (locationName, effectiveTime, description, lastUpdated, district, messageID string) {
	// Extract location name (first <p> tag content after the image)
	// Pattern: <p align="left">Twin Bridges</p>
	locationPattern := regexp.MustCompile(`<p[^>]*align="left"[^>]*>([^<]+)</p>`)
	matches := locationPattern.FindAllStringSubmatch(html, -1)
	if len(matches) > 0 {
		locationName = strings.TrimSpace(matches[0][1])
	}

	// Extract R1/R2 description (second <p> tag with chains info)
	if len(matches) > 1 {
		description = strings.TrimSpace(matches[1][1])
	}

	// Extract effective time
	// Pattern: Chain control effective from: 12/24/2025 08:19
	effectivePattern := regexp.MustCompile(`Chain control effective from:\s*(\d{1,2}/\d{1,2}/\d{4}\s+\d{1,2}:\d{2})`)
	if match := effectivePattern.FindStringSubmatch(html); len(match) > 1 {
		effectiveTime = parseChainControlTime(match[1])
	}

	// Extract last updated
	// Pattern: Last updated: 12/24/2025 9:54am
	lastUpdatedPattern := regexp.MustCompile(`Last updated:\s*(\d{1,2}/\d{1,2}/\d{4}\s+\d{1,2}:\d{2}[ap]m)`)
	if match := lastUpdatedPattern.FindStringSubmatch(html); len(match) > 1 {
		lastUpdated = parseChainControlTime(match[1])
	}

	// Extract district and message ID
	// Pattern: District:3 Message ID:8780
	metaPattern := regexp.MustCompile(`District:(\d+)\s+Message ID:(\d+)`)
	if match := metaPattern.FindStringSubmatch(html); len(match) > 2 {
		district = match[1]
		messageID = match[2]
	}

	return locationName, effectiveTime, description, lastUpdated, district, messageID
}

// parseChainControlTime converts Caltrans time format to ISO 8601
// Input: "12/24/2025 08:19" or "12/24/2025 9:54am"
func parseChainControlTime(timeStr string) string {
	timeStr = strings.TrimSpace(timeStr)

	// Try format with am/pm: "12/24/2025 9:54am"
	if t, err := time.Parse("1/2/2006 3:04pm", timeStr); err == nil {
		return t.Format(time.RFC3339)
	}
	if t, err := time.Parse("1/2/2006 3:04am", timeStr); err == nil {
		return t.Format(time.RFC3339)
	}

	// Try 24-hour format: "12/24/2025 08:19"
	if t, err := time.Parse("1/2/2006 15:04", timeStr); err == nil {
		return t.Format(time.RFC3339)
	}

	// Return original if parsing fails
	return timeStr
}

// ParseLaneClosures processes lane closures KML feed
// URL from research.md line 72
func (p *FeedParser) ParseLaneClosures(ctx context.Context) ([]CaltransIncident, error) {
	return p.parseKMLFeed(ctx, "https://quickmap.dot.ca.gov/data/lcs2way.kml", LANE_CLOSURE)
}

// ParseCHPIncidents processes CHP incidents KML feed
// URL from research.md line 73
func (p *FeedParser) ParseCHPIncidents(ctx context.Context) ([]CaltransIncident, error) {
	return p.parseKMLFeed(ctx, "https://quickmap.dot.ca.gov/data/chp-only.kml", CHP_INCIDENT)
}

// parseKMLFeed downloads and parses a KML feed
func (p *FeedParser) parseKMLFeed(ctx context.Context, url string, feedType CaltransFeedType) ([]CaltransIncident, error) {
	// Download KML file
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Default to a new HTTP client if none is set
	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download KML: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error %d downloading KML from %s", resp.StatusCode, url)
	}

	// Parse KML using standard encoding/xml
	kmlData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read KML response: %w", err)
	}

	var kml KML
	err = xml.Unmarshal(kmlData, &kml)
	if err != nil {
		return nil, fmt.Errorf("failed to parse KML: %w", err)
	}

	// Process KML placemarks
	var incidents []CaltransIncident
	now := time.Now()

	// Process placemarks directly in document
	for _, placemark := range kml.Document.Placemarks {
		if incident := p.processPlacemark(&placemark, feedType, now); incident != nil {
			incidents = append(incidents, *incident)
		}
	}

	// Process placemarks in folders
	for _, folder := range kml.Document.Folders {
		for _, placemark := range folder.Placemarks {
			if incident := p.processPlacemark(&placemark, feedType, now); incident != nil {
				incidents = append(incidents, *incident)
			}
		}
	}

	return incidents, nil
}

// ParseKMLContent parses KML content directly for testing purposes
// This allows unit tests to work with test fixtures without making HTTP calls
func (p *FeedParser) ParseKMLContent(kmlData []byte, feedType CaltransFeedType) ([]CaltransIncident, error) {
	var kml KML
	err := xml.Unmarshal(kmlData, &kml)
	if err != nil {
		return nil, fmt.Errorf("failed to parse KML: %w", err)
	}

	// Process KML placemarks
	var incidents []CaltransIncident
	now := time.Now()

	// Process placemarks directly in document
	for _, placemark := range kml.Document.Placemarks {
		if incident := p.processPlacemark(&placemark, feedType, now); incident != nil {
			incidents = append(incidents, *incident)
		}
	}

	// Process placemarks in folders
	for _, folder := range kml.Document.Folders {
		for _, placemark := range folder.Placemarks {
			if incident := p.processPlacemark(&placemark, feedType, now); incident != nil {
				incidents = append(incidents, *incident)
			}
		}
	}

	return incidents, nil
}

// FilterByGeography filters incidents by proximity to route coordinates
// This method is extracted for testing purposes
func (p *FeedParser) FilterByGeography(incidents []CaltransIncident, routeCoordinates []geo.Point, radiusMeters float64) []CaltransIncident {
	filteredIncidents := make([]CaltransIncident, 0)

	for _, incident := range incidents {
		// Skip incidents without valid coordinates
		if incident.Coordinates == nil {
			continue
		}

		incidentPoint := geo.Point{
			Latitude:  incident.Coordinates.Latitude,
			Longitude: incident.Coordinates.Longitude,
		}

		// Check if incident is within radius of any route coordinate
		isNearRoute := false
		for _, coord := range routeCoordinates {
			distance, err := p.geoUtils.DistanceFromCoords(
				coord.Latitude, coord.Longitude,
				incidentPoint.Latitude, incidentPoint.Longitude,
			)
			if err != nil {
				continue // Skip invalid coordinates
			}
			if distance <= radiusMeters {
				isNearRoute = true
				break // Found within range, no need to check other coordinates
			}
		}

		if isNearRoute {
			filteredIncidents = append(filteredIncidents, incident)
		}
	}

	return filteredIncidents
}

// processPlacemark converts KML Placemark to CaltransIncident
// Structure mapping per data-model.md lines 80-90
func (p *FeedParser) processPlacemark(placemark *Placemark, feedType CaltransFeedType, fetchTime time.Time) *CaltransIncident {
	// Extract geometry data (coordinates and polylines)
	coordinates, polyline := p.extractGeometry(placemark)

	// Skip placemarks with no valid geometry
	if coordinates == nil && polyline == nil {
		return nil
	}

	// Extract description HTML
	descriptionHtml := placemark.Description

	// Extract plain text from HTML description
	descriptionText := extractTextFromHTML(descriptionHtml)

	// Extract status and dates from description
	parsedStatus := extractStatus(descriptionText)
	parsedDates := extractDates(descriptionText)

	// As of 2026 the quickmap feeds ship a blank <name> and carry the incident
	// label inside the description's iw-* markup. Backfill a meaningful name so
	// downstream consumers (road alert titles, the incidents feed) keep working.
	name := strings.TrimSpace(placemark.Name)
	if name == "" {
		name = deriveNameFromDescription(descriptionHtml, feedType)
	}

	return &CaltransIncident{
		FeedType:        feedType,
		Name:            name,
		DescriptionHtml: descriptionHtml,
		DescriptionText: descriptionText,
		StyleUrl:        placemark.StyleURL,
		Coordinates:     coordinates,
		AffectedArea:    polyline,
		ParsedStatus:    parsedStatus,
		ParsedDates:     parsedDates,
		LastFetched:     fetchTime,
	}
}

// extractGeometry extracts coordinate and polyline data from a placemark
func (p *FeedParser) extractGeometry(placemark *Placemark) (*api.Coordinates, *api.Polyline) {
	var pointCoord *api.Coordinates
	var polyline *api.Polyline

	// Handle Point geometry
	if placemark.Point.Coordinates != "" {
		pointCoord = p.parseCoordinates(placemark.Point.Coordinates)
	}

	// Handle LineString geometry (for linear closures)
	if placemark.LineString.Coordinates != "" {
		coords := p.parseCoordinateList(placemark.LineString.Coordinates)
		if len(coords) > 0 {
			polyline = &api.Polyline{Points: coords}
			// Use first point as primary coordinate if no point geometry
			if pointCoord == nil && len(coords) > 0 {
				pointCoord = coords[0]
			}
		}
	}

	// Handle Polygon geometry (for area closures)
	if placemark.Polygon.OuterBoundary.LinearRing.Coordinates != "" {
		coords := p.parseCoordinateList(placemark.Polygon.OuterBoundary.LinearRing.Coordinates)
		if len(coords) > 0 {
			polyline = &api.Polyline{Points: coords}
			// Use first point as primary coordinate if no point geometry
			if pointCoord == nil && len(coords) > 0 {
				pointCoord = coords[0]
			}
		}
	}

	// Handle MultiGeometry (complex geometries)
	if len(placemark.MultiGeometry.Points) > 0 ||
		len(placemark.MultiGeometry.LineStrings) > 0 ||
		len(placemark.MultiGeometry.Polygons) > 0 {

		var allCoords []*api.Coordinates

		// Collect all points from MultiGeometry
		for _, point := range placemark.MultiGeometry.Points {
			if coord := p.parseCoordinates(point.Coordinates); coord != nil {
				allCoords = append(allCoords, coord)
				if pointCoord == nil {
					pointCoord = coord
				}
			}
		}

		// Collect all coordinates from LineStrings
		for _, lineString := range placemark.MultiGeometry.LineStrings {
			coords := p.parseCoordinateList(lineString.Coordinates)
			allCoords = append(allCoords, coords...)
			if pointCoord == nil && len(coords) > 0 {
				pointCoord = coords[0]
			}
		}

		// Collect all coordinates from Polygons
		for _, polygon := range placemark.MultiGeometry.Polygons {
			coords := p.parseCoordinateList(polygon.OuterBoundary.LinearRing.Coordinates)
			allCoords = append(allCoords, coords...)
			if pointCoord == nil && len(coords) > 0 {
				pointCoord = coords[0]
			}
		}

		// Create polyline from all collected coordinates
		if len(allCoords) > 1 {
			polyline = &api.Polyline{Points: allCoords}
		}
	}

	return pointCoord, polyline
}

// parseCoordinates parses KML coordinate string "longitude,latitude,altitude"
func (p *FeedParser) parseCoordinates(coordString string) *api.Coordinates {
	coordString = strings.TrimSpace(coordString)
	if coordString == "" {
		return nil
	}

	// KML coordinates format: "longitude,latitude,altitude"
	parts := strings.Split(coordString, ",")
	if len(parts) < 2 {
		return nil
	}

	longitude, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return nil
	}

	latitude, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return nil
	}

	return &api.Coordinates{
		Latitude:  latitude,
		Longitude: longitude,
	}
}

// parseCoordinateList parses KML coordinate string with multiple coordinates
// Format: "lon1,lat1,alt1 lon2,lat2,alt2 lon3,lat3,alt3"
func (p *FeedParser) parseCoordinateList(coordString string) []*api.Coordinates {
	coordString = strings.TrimSpace(coordString)
	if coordString == "" {
		return nil
	}

	var coordinates []*api.Coordinates

	// Split by whitespace or newlines to get individual coordinate sets
	coordSets := regexp.MustCompile(`\s+`).Split(coordString, -1)

	for _, coordSet := range coordSets {
		coordSet = strings.TrimSpace(coordSet)
		if coordSet == "" {
			continue
		}

		// Parse individual coordinate set "longitude,latitude,altitude"
		parts := strings.Split(coordSet, ",")
		if len(parts) < 2 {
			continue
		}

		longitude, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			continue
		}

		latitude, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			continue
		}

		coordinates = append(coordinates, &api.Coordinates{
			Latitude:  latitude,
			Longitude: longitude,
		})
	}

	return coordinates
}

// Regexes for the 2026 quickmap "infowindow" (iw-*) description markup.
var (
	iwTitlePattern    = regexp.MustCompile(`(?is)<h2[^>]*class="iw-title"[^>]*>(.*?)</h2>`)
	chpIncidentLabel  = regexp.MustCompile(`(?i)CHP Incident\s+([A-Za-z0-9]+)`)
	iwHeaderLeftMatch = regexp.MustCompile(`(?is)<div[^>]*class="iw-header-left"[^>]*>(.*?)</div>`)
)

// deriveNameFromDescription builds a human-readable incident name from the
// description markup when the KML <name> is blank. CHP incidents are labelled
// by their log header ("CHP Incident 260625SA1034"); other feeds use the
// info-window title (e.g. "Route 1 One-way Traffic Operation").
func deriveNameFromDescription(descHTML string, feedType CaltransFeedType) string {
	if feedType == CHP_INCIDENT {
		if m := chpIncidentLabel.FindString(descHTML); m != "" {
			return strings.Join(strings.Fields(m), " ")
		}
	}
	if m := iwTitlePattern.FindStringSubmatch(descHTML); len(m) > 1 {
		if title := strings.TrimSpace(extractTextFromHTML(m[1])); title != "" {
			return title
		}
	}
	if m := iwHeaderLeftMatch.FindStringSubmatch(descHTML); len(m) > 1 {
		if header := strings.TrimSpace(extractTextFromHTML(m[1])); header != "" {
			return header
		}
	}
	return ""
}

// extractTextFromHTML removes HTML tags and decodes HTML entities
func extractTextFromHTML(htmlContent string) string {
	// Remove HTML tags
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(htmlContent, " ")

	// Decode HTML entities
	text = html.UnescapeString(text)

	// Clean up whitespace
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// extractStatus attempts to extract status information from description text
func extractStatus(text string) string {
	// Common status patterns in Caltrans descriptions
	statusPatterns := []string{
		`(?i)(closed?)`,
		`(?i)(chain control in effect)`,
		`(?i)(restrictions?)`,
		`(?i)(incident)`,
		`(?i)(construction)`,
	}

	for _, pattern := range statusPatterns {
		re := regexp.MustCompile(pattern)
		if match := re.FindString(text); match != "" {
			return strings.ToLower(match)
		}
	}

	return ""
}

// extractDates attempts to extract date/time information from description text
func extractDates(text string) []string {
	// Pattern for dates like "12/25/2024" or "Dec 25, 2024"
	datePattern := regexp.MustCompile(`\d{1,2}[/\-]\d{1,2}[/\-]\d{4}|[A-Za-z]{3}\s+\d{1,2},\s+\d{4}`)
	matches := datePattern.FindAllString(text, -1)

	// Deduplicate dates
	seen := make(map[string]bool)
	var uniqueDates []string
	for _, date := range matches {
		if !seen[date] {
			seen[date] = true
			uniqueDates = append(uniqueDates, date)
		}
	}

	// Return empty slice instead of nil for consistency
	if uniqueDates == nil {
		return []string{}
	}
	return uniqueDates
}
