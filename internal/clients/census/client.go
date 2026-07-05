// Package census provides a client for the US Census Bureau onelineaddress
// geocoder. Public, keyless; used only for /v1/places/resolve address lookups.
package census

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxBody caps the upstream response (a single-address lookup is tiny).
const maxBody = 2 << 20 // 2 MiB

// ErrNoMatch is returned when the geocoder finds no candidate for the address.
var ErrNoMatch = errors.New("census geocoder: no address match")

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries the Census onelineaddress geocoder.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a Census geocoder client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://geocoding.geo.census.gov",
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + base URL (testing).
func NewClientWithHTTPDoer(baseURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: baseURL}
}

// Geocode resolves a one-line address to a coordinate. Returns ErrNoMatch
// (errors.Is-able) when the geocoder has no candidates.
func (c *Client) Geocode(ctx context.Context, oneline string) (lat, lng float64, matchedAddress string, err error) {
	params := url.Values{}
	params.Set("address", oneline)
	params.Set("benchmark", "Public_AR_Current")
	params.Set("format", "json")

	requestURL := fmt.Sprintf("%s/geocoder/locations/onelineaddress?%s", c.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return 0, 0, "", fmt.Errorf("failed to create census geocoder request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, 0, "", fmt.Errorf("failed to execute census geocoder request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, 0, "", fmt.Errorf("census geocoder error %d: %s", resp.StatusCode, string(body))
	}

	var parsed geocodeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&parsed); err != nil {
		return 0, 0, "", fmt.Errorf("failed to decode census geocoder response: %w", err)
	}
	if len(parsed.Result.AddressMatches) == 0 {
		return 0, 0, "", fmt.Errorf("%w: %q", ErrNoMatch, oneline)
	}
	m := parsed.Result.AddressMatches[0]
	return m.Coordinates.Y, m.Coordinates.X, m.MatchedAddress, nil
}

// Response JSON (only the fields we use).
type geocodeResponse struct {
	Result geocodeResult `json:"result"`
}

type geocodeResult struct {
	AddressMatches []addressMatch `json:"addressMatches"`
}

type addressMatch struct {
	MatchedAddress string      `json:"matchedAddress"`
	Coordinates    coordinates `json:"coordinates"`
}

// coordinates: x is longitude, y is latitude (Census convention).
type coordinates struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
