// Package wfigs provides a client for the NIFC WFIGS interagency fire-perimeter
// ArcGIS feature service (public, keyless, CORS-enabled, GeoJSON). We proxy it
// for caching + a consistent shape, and to simplify geometry server-side.
package wfigs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBody = 16 << 20 // 16 MiB (simplified polygons; bbox-scoped)

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries the WFIGS current-perimeters feature service.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a WFIGS client pointed at the public feature service.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 25 * time.Second},
		baseURL:    "https://services3.arcgis.com/T4QMspbfLg3qTGWY/arcgis/rest/services/WFIGS_Interagency_Perimeters_Current/FeatureServer/0/query",
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + query URL (testing).
func NewClientWithHTTPDoer(queryURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: queryURL}
}

// Bounds is a lat/lng bounding box.
type Bounds struct {
	MinLatitude, MaxLatitude, MinLongitude, MaxLongitude float64
}

// Perimeter is a normalized fire perimeter. GeometryType/GeometryCoords carry
// the upstream GeoJSON geometry verbatim ([lon,lat] order, already simplified
// server-side); the caller wraps them into its own geometry type.
type Perimeter struct {
	Name             string
	Acres            float64
	PercentContained int32
	Cause            string
	GeometryType     string
	GeometryCoords   json.RawMessage
}

// --- Rate limiting: the 429s are NIFC's shared ORG quota, not our request rate ---
//
// Findings from live header inspection (2026-07-27). Re-verify any time with:
//
//	curl -sD- '<baseURL>?where=1=1&returnCountOnly=true&f=json' | grep -i x-esri
//
// The recurring "429 Too many requests" is NOT a per-IP/app limit on us. ArcGIS
// Online meters queries in "request units per minute" against the org that OWNS
// the layer (NIFC), and anonymous public access is billed to that owner. The
// service says so in its own response headers:
//
//	X-Esri-Org-Request-Units-Per-Min: usage=71253; max=192000
//	X-Esri-Query-Request-Units: 2      (cost of one query)
//
// When NIFC's org hits that ceiling, ArcGIS opens a ~60s cooling-off and 429s
// every non-cacheable query for ALL consumers. In fire season the ceiling is
// saturated by the whole world; we poll ~6x/hour at 2 units — a rounding error.
// So throttling our own poll interval barely helps (it didn't, 5m->10m); we are
// collateral, not the cause.
//
// Cache behaviour differs sharply by request type (verified live):
//   - Feature query (returnGeometry=true, ANY f=): Cache-Control "private",
//     X-Cache PRIVATE_NOSTORE — always hits origin, costs units, 429-exposed.
//     (f=json vs f=geojson makes no difference; the split is feature-data vs
//     metadata, not output format.)
//   - Layer metadata (FeatureServer/0?f=json): Cache-Control "public,
//     max-age=300", X-Cache TCP_HIT, carries an ETag and
//     editingInfo.dataLastEditDate — served from the CDN, ~free, survives 429s.
//
// Mitigation — gate the expensive, uncacheable feature query behind the free,
// cached metadata check. Implemented as Client.LastEdit (below) +
// ingest.WildfireNormalizer.gatedPerimeters:
//  1. Each tick, LastEdit() reads editingInfo.dataLastEditDate off the cached
//     metadata endpoint (FeatureServer/0?f=json — ~free, CDN-served).
//  2. GetPerimeters (this expensive, uncacheable query) runs ONLY when that stamp
//     advanced since our last successful fetch. Perimeters update ~daily (after an
//     IR/mapping flight), so the expensive call drops from ~6/hr to ~1-2/day.
//  3. On a WFIGS feature-query error gatedPerimeters returns the error and does NOT
//     advance the stamp, so the source is flagged failed (its disappearance sweep
//     is SKIPPED — fail-loud) and the next tick retries. Under the `expire` policy
//     that ages the existing perimeter (carried forward) rather than dropping it;
//     it does NOT serve the cache on error. The last-good set is reused only while
//     the stamp is unchanged (a genuine success), bounded by maxPerimCacheAge. (If
//     the cheap metadata check itself fails, we fall back to a direct GetPerimeters
//     — never worse than pre-gating.) The whole national dataset is ~208 features.
//
// What does NOT help: an API key / auth (public usage is charged to the OWNER,
// and API keys can't be used with an ArcGIS Online account); switching output
// format; polling less often. A fallback source, if ever needed, is the NIFC
// Open Data GeoJSON export (data-nifc.opendata.arcgis.com) — a pre-generated
// export with a different cache/quota profile, but it can lag the live service.
//
// GetPerimeters returns perimeters intersecting bounds, geometry simplified
// server-side (maxAllowableOffset).
func (c *Client) GetPerimeters(ctx context.Context, b Bounds) ([]Perimeter, error) {
	params := url.Values{}
	params.Set("f", "geojson")
	params.Set("where", "1=1")
	params.Set("geometry", fmt.Sprintf("%s,%s,%s,%s",
		ftoa(b.MinLongitude), ftoa(b.MinLatitude), ftoa(b.MaxLongitude), ftoa(b.MaxLatitude)))
	params.Set("geometryType", "esriGeometryEnvelope")
	params.Set("inSR", "4326")
	params.Set("spatialRel", "esriSpatialRelIntersects")
	params.Set("outFields", "poly_IncidentName,attr_IncidentSize,attr_PercentContained,attr_FireCause")
	params.Set("returnGeometry", "true")
	params.Set("maxAllowableOffset", "0.001") // ~100m vertex tolerance

	body, err := c.get(ctx, c.baseURL+"?"+params.Encode(), "application/geo+json", maxBody)
	if err != nil {
		return nil, err
	}
	var parsed perimeterResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode WFIGS response: %w", err)
	}
	// ArcGIS signals quota/throttle/token errors with HTTP 200 + an error
	// envelope; without this check that decodes to empty features (a false clear).
	if parsed.Error != nil {
		return nil, fmt.Errorf("WFIGS ArcGIS error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	out := make([]Perimeter, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		out = append(out, Perimeter{
			Name:             f.Properties.Name,
			Acres:            f.Properties.Acres,
			PercentContained: int32(f.Properties.PercentContained),
			Cause:            f.Properties.Cause,
			GeometryType:     f.Geometry.Type,
			GeometryCoords:   f.Geometry.Coordinates,
		})
	}
	return out, nil
}

// LastEdit returns the layer's dataLastEditDate — when WFIGS last changed ANY
// perimeter nationally. It hits the cheap, CDN-cached metadata endpoint (see the
// rate-limit note above), so a caller can gate the expensive GetPerimeters query
// on it and only refetch when the data actually changed.
func (c *Client) LastEdit(ctx context.Context) (time.Time, error) {
	metaURL := strings.TrimSuffix(c.baseURL, "/query") + "?f=json"
	body, err := c.get(ctx, metaURL, "application/json", 1<<20)
	if err != nil {
		return time.Time{}, err
	}
	var meta struct {
		EditingInfo struct {
			DataLastEditDate int64 `json:"dataLastEditDate"`
		} `json:"editingInfo"`
		Error *arcgisError `json:"error"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return time.Time{}, fmt.Errorf("failed to decode WFIGS metadata: %w", err)
	}
	if meta.Error != nil {
		return time.Time{}, fmt.Errorf("WFIGS ArcGIS error %d: %s", meta.Error.Code, meta.Error.Message)
	}
	if meta.EditingInfo.DataLastEditDate == 0 {
		return time.Time{}, fmt.Errorf("WFIGS metadata: no dataLastEditDate")
	}
	return time.UnixMilli(meta.EditingInfo.DataLastEditDate), nil
}

// get executes a GET and returns the size-limited body. HTTP >=400 (incl. a 429
// throttle) becomes an error; the ArcGIS 200-with-error envelope is left for the
// caller to detect (its shape is endpoint-specific).
func (c *Client) get(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create WFIGS request: %w", err)
	}
	req.Header.Set("Accept", accept)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute WFIGS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("WFIGS API error %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

type perimeterResponse struct {
	Features []perimeterFeature `json:"features"`
	Error    *arcgisError       `json:"error"`
}

// arcgisError is the error envelope ArcGIS returns with an HTTP 200 status.
type arcgisError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type perimeterFeature struct {
	Properties perimeterProps `json:"properties"`
	Geometry   geometryJSON   `json:"geometry"`
}

type perimeterProps struct {
	Name             string  `json:"poly_IncidentName"`
	Acres            float64 `json:"attr_IncidentSize"`
	PercentContained float64 `json:"attr_PercentContained"`
	Cause            string  `json:"attr_FireCause"`
}

type geometryJSON struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}
