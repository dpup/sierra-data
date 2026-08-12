// Package nws provides a client for the National Weather Service (api.weather.gov)
// public alerts API. It is the authoritative source for zone-based watches and
// warnings (including fire-weather products) for the S.I.E.R.R.A service area.
//
// The NWS API requires no API key but does require a descriptive User-Agent
// identifying the application (https://www.weather.gov/documentation/services-web-api).
// It returns GeoJSON; we only consume the alert `properties`.
package nws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client provides access to the NWS active-alerts + gridpoint-forecast API.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
	userAgent  string
	now        func() time.Time // injectable clock for the forecast horizon (tests); nil → time.Now
}

// NewClient creates a new NWS client. userAgent should identify the app and
// include a contact, e.g. "data.sierragridteam.org (contact@sierragridteam.org)".
func NewClient(userAgent string) *Client {
	if userAgent == "" {
		userAgent = "data.sierragridteam.org"
	}
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.weather.gov",
		userAgent:  userAgent,
	}
}

// NewClientWithHTTPDoer creates a client with a custom HTTP doer and base URL
// (for testing).
func NewClientWithHTTPDoer(userAgent, baseURL string, httpClient HTTPDoer) *Client {
	if userAgent == "" {
		userAgent = "data.sierragridteam.org"
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    baseURL,
		userAgent:  userAgent,
	}
}

// Alert is a normalized NWS alert.
type Alert struct {
	ID          string
	Event       string // e.g. "Red Flag Warning", "Winter Storm Warning"
	Severity    string // Extreme | Severe | Moderate | Minor | Unknown
	Certainty   string
	Urgency     string
	Headline    string // CAP properties.headline — boilerplate, see ShortHeadline
	NWSHeadline string // parameters.NWSheadline — ALL CAPS, carries the reason
	Description string
	Instruction string
	SenderName  string
	AreaDesc    string
	// The four CAP times, and they are two different pairs. Effective/Expires
	// describe the PRODUCT — when it was issued and when the office must
	// re-issue it. Onset/Ends describe the HAZARD. They routinely disagree: a
	// Fire Weather Watch issued Tuesday for Thursday's storms carries
	// effective=Tue 09:57, expires=Wed 16:00, onset=Thu 05:00, ends=Thu 21:00.
	// Use Begins/EndsAt for anything a reader would call "when"; Effective is
	// kept raw because NWSAlertID's fallback is keyed on issuance.
	Effective time.Time
	Expires   time.Time
	Onset     time.Time
	Ends      time.Time
	Zones     []string // UGC zone codes, e.g. ["CAZ064", "CAZ065"]
}

// Begins is when the hazard starts: CAP `onset`, falling back to `effective`
// (issuance) for the products that publish no onset.
func (a Alert) Begins() time.Time {
	if !a.Onset.IsZero() {
		return a.Onset
	}
	return a.Effective
}

// EndsAt is when the hazard stops: CAP `ends`, falling back to `expires`.
//
// `expires` alone was wrong and visibly so: the Watch above advertised
// expires=Wed 16:00 for a hazard running to Thu 21:00, so the record said the
// alert was over a day and a half before the storms it warns about. It is the
// product's re-issue deadline, not a hazard end — an office that lets it lapse
// has stopped updating, not stopped warning.
func (a Alert) EndsAt() time.Time {
	if !a.Ends.IsZero() {
		return a.Ends
	}
	return a.Expires
}

// GetActiveZoneAlerts returns active alerts for the given NWS zone codes
// (e.g. "CAZ064"). An empty zone list returns no alerts (no statewide fetch).
func (c *Client) GetActiveZoneAlerts(ctx context.Context, zones []string) ([]Alert, error) {
	zones = cleanZones(zones)
	if len(zones) == 0 {
		return nil, nil
	}

	params := url.Values{}
	params.Set("zone", strings.Join(zones, ","))
	params.Set("status", "actual")
	requestURL := fmt.Sprintf("%s/alerts/active?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create NWS request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/geo+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute NWS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("NWS API error %d: %s", resp.StatusCode, string(body))
	}

	var parsed alertsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode NWS response: %w", err)
	}

	return parsed.toAlerts(), nil
}

// cleanZones trims, upper-cases, and de-duplicates zone codes.
func cleanZones(zones []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, z := range zones {
		// Allow comma-separated entries to be passed through.
		for _, part := range strings.Split(z, ",") {
			zc := strings.ToUpper(strings.TrimSpace(part))
			if zc == "" || seen[zc] {
				continue
			}
			seen[zc] = true
			out = append(out, zc)
		}
	}
	return out
}

// GeoJSON response types (only the fields we use).
type alertsResponse struct {
	Features []alertFeature `json:"features"`
}

type alertFeature struct {
	Properties alertProperties `json:"properties"`
}

type alertProperties struct {
	ID          string              `json:"id"`
	Event       string              `json:"event"`
	Severity    string              `json:"severity"`
	Certainty   string              `json:"certainty"`
	Urgency     string              `json:"urgency"`
	Headline    string              `json:"headline"`
	Description string              `json:"description"`
	Instruction string              `json:"instruction"`
	SenderName  string              `json:"senderName"`
	AreaDesc    string              `json:"areaDesc"`
	Effective   string              `json:"effective"`
	Expires     string              `json:"expires"`
	Onset       string              `json:"onset"`
	Ends        string              `json:"ends"`
	Geocode     alertGeo            `json:"geocode"`
	Parameters  map[string][]string `json:"parameters"` // NWSheadline lives here
}

type alertGeo struct {
	UGC []string `json:"UGC"`
}

func (r alertsResponse) toAlerts() []Alert {
	var alerts []Alert
	for _, f := range r.Features {
		p := f.Properties
		alerts = append(alerts, Alert{
			ID:          p.ID,
			Event:       p.Event,
			Severity:    p.Severity,
			Certainty:   p.Certainty,
			Urgency:     p.Urgency,
			Headline:    p.Headline,
			NWSHeadline: firstParam(p.Parameters, "NWSheadline"),
			Description: p.Description,
			Instruction: p.Instruction,
			SenderName:  p.SenderName,
			AreaDesc:    p.AreaDesc,
			Effective:   parseTime(p.Effective),
			Expires:     parseTime(p.Expires),
			Onset:       parseTime(p.Onset),
			Ends:        parseTime(p.Ends),
			Zones:       p.Geocode.UGC,
		})
	}
	return alerts
}

// firstParam returns the first value of a CAP `parameters` entry, or "".
// The block is map[string][]string in the wire format even where a single
// value is the only sensible shape.
func firstParam(params map[string][]string, key string) string {
	if v, ok := params[key]; ok && len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
