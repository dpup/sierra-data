// Package caloes provides a client for the Cal OES California Evacuation
// Aggregation Layer (ArcGIS, public, keyless). It is an ACTIVE-EVENTS-ONLY layer:
// it holds only zones currently in Order/Warning/Advisory. The client separates a
// failed fetch from a clean-but-empty one: a transport error, non-2xx, or an
// ArcGIS HTTP-200 error-envelope returns an error (the caller surfaces
// UNAVAILABLE/"unknown"), while a clean fetch with no zones returns an empty
// slice and nil error (the caller surfaces a caveated "no active zones"). The
// safety invariant upstream is that an error never becomes a "0" — see
// docs/hazard-aggregation-design.md §6.4.
package caloes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const maxBody = 16 << 20 // 16 MiB (zone polygons)

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries the Cal OES evacuation aggregation feature service.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a Cal OES client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		baseURL:    "https://services.arcgis.com/BLN4oKB0N1YSgvY8/arcgis/rest/services/CA_EVACUATIONS_CalOESHosted_view/FeatureServer/0/query",
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + query URL (testing).
func NewClientWithHTTPDoer(queryURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: queryURL}
}

// EvacZone is a normalized active evacuation zone. GeometryType/GeometryCoords
// carry the upstream GeoJSON geometry verbatim.
type EvacZone struct {
	ZoneID         string
	ZoneName       string
	County         string
	Status         string // raw upstream status text
	EventType      string
	PublicInfo     string
	LastUpdated    time.Time
	GeometryType   string
	GeometryCoords json.RawMessage
}

// SourceURL is the authoritative public viewer, always surfaced to users. It is
// the generic entry point used as the layer-level source_url (fail-loud: it must
// be valid even when there are zero zones) and as the per-event fallback for
// zones we can't deep-link.
const SourceURL = "https://protect.genasys.com/"

// genasysZoneRe matches the Genasys/Zonehaven zone-id scheme
// (US-CA-X{county}-{agency}-{zone}, e.g. US-CA-XCA-CCU-153 Calaveras,
// US-CA-XTU-PVL-E032 Tulare) that protect.genasys.com/zones/{id} resolves. Cal
// OES aggregates zones from several county platforms and passes each id through
// verbatim, so only Genasys-scheme ids deep-link there — other counties (e.g.
// Tuolumne's US-CA-Toulumne101, a different vendor) are not hosted on Genasys.
var genasysZoneRe = regexp.MustCompile(`^US-CA-X[A-Z]+-`)

// ZoneURL returns the most specific public viewer URL for a zone: a
// protect.genasys.com deep link into the zone when ZONE_ID is a Genasys-scheme
// id, otherwise the generic Genasys viewer (SourceURL). We deliberately do not
// map the non-Genasys counties' bespoke viewers (opaque per-county ArcGIS apps)
// — the generic viewer is the honest, maintainable fallback.
func ZoneURL(zoneID string) string {
	if genasysZoneRe.MatchString(zoneID) {
		return "https://protect.genasys.com/zones/" + url.PathEscape(zoneID)
	}
	return SourceURL
}

// Bounds is a lat/lng bounding box for the spatial query.
type Bounds struct {
	MinLatitude  float64
	MaxLatitude  float64
	MinLongitude float64
	MaxLongitude float64
}

// GetActiveEvacuations returns active evacuation zones intersecting the given
// bounding box. Filtering geographically (rather than by COUNTY string) catches
// in-area zones tagged to a neighboring county and avoids county-name casing
// mismatches. An empty (non-error) result is ambiguous — the caller must treat
// it as "unknown", not "no evacuations".
func (c *Client) GetActiveEvacuations(ctx context.Context, b Bounds) ([]EvacZone, error) {
	envelope := fmt.Sprintf(`{"xmin":%s,"ymin":%s,"xmax":%s,"ymax":%s,"spatialReference":{"wkid":4326}}`,
		ftoa(b.MinLongitude), ftoa(b.MinLatitude), ftoa(b.MaxLongitude), ftoa(b.MaxLatitude))
	params := url.Values{}
	params.Set("f", "geojson")
	params.Set("where", "1=1")
	params.Set("geometry", envelope)
	params.Set("geometryType", "esriGeometryEnvelope")
	params.Set("inSR", "4326")
	params.Set("spatialRel", "esriSpatialRelIntersects")
	params.Set("outFields", "ZONE_ID,ZONE_NAME,COUNTY,STATUS,EVENT_TYPE,PUBLIC_INFO,STATEWIDE_LAST_UPDATED")
	params.Set("returnGeometry", "true")
	params.Set("outSR", "4326")

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Cal OES request: %w", err)
	}
	req.Header.Set("Accept", "application/geo+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute Cal OES request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Cal OES API error %d: %s", resp.StatusCode, string(body))
	}

	var parsed evacResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode Cal OES response: %w", err)
	}
	// ArcGIS signals quota/throttle/token errors with HTTP 200 + an error
	// envelope. For this life-safety feed that must surface as an error (caller
	// treats it as UNAVAILABLE/unknown), never as an empty all-clear.
	if parsed.Error != nil {
		return nil, fmt.Errorf("Cal OES ArcGIS error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	out := make([]EvacZone, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		out = append(out, EvacZone{
			ZoneID:         f.Properties.ZoneID,
			ZoneName:       f.Properties.ZoneName,
			County:         f.Properties.County,
			Status:         f.Properties.Status,
			EventType:      f.Properties.EventType,
			PublicInfo:     f.Properties.PublicInfo,
			LastUpdated:    msToTime(f.Properties.LastUpdated),
			GeometryType:   f.Geometry.Type,
			GeometryCoords: f.Geometry.Coordinates,
		})
	}
	return out, nil
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

type evacResponse struct {
	Features []evacFeature `json:"features"`
	Error    *arcgisError  `json:"error"`
}

// arcgisError is the error envelope ArcGIS returns with an HTTP 200 status.
type arcgisError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type evacFeature struct {
	Properties evacProps    `json:"properties"`
	Geometry   geometryJSON `json:"geometry"`
}

type evacProps struct {
	ZoneID      string `json:"ZONE_ID"`
	ZoneName    string `json:"ZONE_NAME"`
	County      string `json:"COUNTY"`
	Status      string `json:"STATUS"`
	EventType   string `json:"EVENT_TYPE"`
	PublicInfo  string `json:"PUBLIC_INFO"`
	LastUpdated int64  `json:"STATEWIDE_LAST_UPDATED"`
}

type geometryJSON struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}
