// Package calfire provides a client for the CAL FIRE active-incidents feed
// (incidents.fire.ca.gov). Public, no API key, and crucially NOT CORS-enabled —
// so a browser cannot read it directly; this server proxies it.
package calfire

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxBody = 4 << 20 // 4 MiB (statewide list is a few KB)

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client fetches CAL FIRE active incidents.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a CAL FIRE client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		baseURL:    "https://incidents.fire.ca.gov",
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + base URL (testing).
func NewClientWithHTTPDoer(baseURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: baseURL}
}

// Incident is a normalized CAL FIRE incident.
type Incident struct {
	UniqueID         string
	Name             string
	County           string
	Location         string
	Acres            float64
	PercentContained int32
	Lat, Lng         float64
	Started          time.Time
	Updated          time.Time
	URL              string
	IsActive         bool
}

// GetActiveIncidents returns the current statewide active incidents. CAL FIRE
// offers no server-side geo filter; the payload is tiny, so callers filter by
// bbox/county themselves.
func (c *Client) GetActiveIncidents(ctx context.Context) ([]Incident, error) {
	url := c.baseURL + "/umbraco/api/IncidentApi/List?inactive=false"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create CAL FIRE request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute CAL FIRE request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("CAL FIRE API error %d: %s", resp.StatusCode, string(body))
	}

	var raw []incidentJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode CAL FIRE response: %w", err)
	}
	out := make([]Incident, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.normalize())
	}
	return out, nil
}

// incidentJSON mirrors the (undocumented) CAL FIRE incident shape — only the
// fields we use. Parse defensively; the API is unsupported and may shift.
type incidentJSON struct {
	UniqueID         string   `json:"UniqueId"`
	Name             string   `json:"Name"`
	County           string   `json:"County"`
	Location         string   `json:"Location"`
	AcresBurned      *float64 `json:"AcresBurned"`
	PercentContained *float64 `json:"PercentContained"`
	Latitude         *float64 `json:"Latitude"`
	Longitude        *float64 `json:"Longitude"`
	Started          string   `json:"Started"`
	Updated          string   `json:"Updated"`
	URL              string   `json:"Url"`
	IsActive         bool     `json:"IsActive"`
}

func (r incidentJSON) normalize() Incident {
	// Trim the free-text fields. CAL FIRE ships trailing whitespace on some
	// incident names, and Name goes straight into the event headline — one live
	// fire rendered as "Dennis Fire  — 16 ac, 40% contained" with a double space
	// while its siblings rendered correctly, which reads as our formatting bug.
	in := Incident{
		UniqueID: r.UniqueID,
		Name:     strings.TrimSpace(r.Name),
		County:   strings.TrimSpace(r.County),
		Location: strings.TrimSpace(r.Location),
		URL:      strings.TrimSpace(r.URL),
		IsActive: r.IsActive,
		Started:  parseTime(r.Started),
		Updated:  parseTime(r.Updated),
	}
	if r.AcresBurned != nil {
		in.Acres = *r.AcresBurned
	}
	if r.PercentContained != nil {
		in.PercentContained = int32(*r.PercentContained)
	}
	if r.Latitude != nil {
		in.Lat = *r.Latitude
	}
	if r.Longitude != nil {
		in.Lng = *r.Longitude
	}
	return in
}

// pacific is the zone for CAL FIRE's zoneless timestamps. Like the Caltrans/CHP
// feeds (see internal/clients/CLAUDE.md), no-offset California timestamps are
// Pacific, not UTC; cmd/server blank-imports time/tzdata so this resolves.
var pacific, _ = time.LoadLocation("America/Los_Angeles")

// parseTime accepts the CAL FIRE timestamp formats seen in the wild. Zone-aware
// layouts are honored as-is; a zoneless timestamp is interpreted as Pacific.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Zone-aware formats first.
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	// Zoneless: interpret as Pacific (falling back to UTC if tzdata is missing).
	loc := pacific
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
