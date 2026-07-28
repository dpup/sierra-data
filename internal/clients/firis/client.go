// Package firis provides a client for the "CA Perimeters CAL FIRE NIFC FIRIS
// public view" ArcGIS feature service — the layer CAL FIRE's own public incident
// map uses. It combines CAL FIRE Intel remote-sensing, FIRIS IR-flight perimeters,
// and WFIGS into one CA-wide layer updated ~every 5 min, so it leads the NIFC
// WFIGS interagency upload by hours (the Dove Fire had a perimeter here while
// WFIGS returned none). Keyless, public, on the CAL FIRE-Forestry ArcGIS org — a
// different (healthier) quota than NIFC's 429-saturated org. See
// docs/firis-perimeter-source-design.md.
//
// Rate limiting: like WFIGS, feature queries are metered per owning-org request
// units and can 429, but the metadata endpoint (FeatureServer/0?f=json) is
// public-CDN-cached (max-age=3600) and exposes editingInfo.dataLastEditDate, so
// callers gate the expensive perimeter query on LastEdit — only fetching when the
// data changed (see internal/ingest wildfire poller). This is the CAL FIRE org,
// not NIFC's, so it is not the pool that saturates in fire season.
package firis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBody = 32 << 20 // 32 MiB (CA-wide active perimeters, simplified)

// HTTPDoer is the injectable HTTP client (for tests).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries the combo perimeter feature service.
type Client struct {
	httpClient HTTPDoer
	baseURL    string // .../FeatureServer/0/query
}

const defaultQueryURL = "https://services1.arcgis.com/jUJYIo9tSA7EHvfZ/arcgis/rest/services/CA_Perimeters_NIFC_FIRIS_public_view/FeatureServer/0/query"

// NewClient creates a client pointed at the public combo feature service.
func NewClient() *Client {
	return &Client{httpClient: &http.Client{Timeout: 25 * time.Second}, baseURL: defaultQueryURL}
}

// NewClientWithHTTPDoer creates a client with a custom doer + query URL (tests).
func NewClientWithHTTPDoer(queryURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: queryURL}
}

// Bounds is a lat/lng bounding box.
type Bounds struct {
	MinLatitude, MaxLatitude, MinLongitude, MaxLongitude float64
}

// Perimeter is one raw perimeter row from the combo feed. A fire has MANY rows
// (successive IR flights + a FIRIS mission); the caller dedups them to one per
// fire (see docs §4). GeometryType/GeometryCoords are the upstream GeoJSON
// geometry verbatim ([lon,lat], server-simplified).
type Perimeter struct {
	IncidentName   string // may be empty (FIRIS mission rows) — fall back to Mission
	Mission        string // e.g. "CA-TCU-DOVE-N57B"
	IncidentNumber string // stable uuid on CAL FIRE Intel rows; empty on FIRIS rows
	Acres          float64
	DateCurrent    time.Time // perimeter currency (poly_DateCurrent) — dedup picks the latest
	Source         string    // "CAL FIRE INTEL FLIGHT DATA" | "FIRIS" | WFIGS
	Status         string    // "Active" | "Inactive"
	GeometryType   string
	GeometryCoords json.RawMessage
}

// GetPerimeters returns ACTIVE perimeters intersecting bounds, geometry simplified
// server-side. The caller dedups (many rows per fire).
func (c *Client) GetPerimeters(ctx context.Context, b Bounds) ([]Perimeter, error) {
	params := url.Values{}
	params.Set("f", "geojson")
	params.Set("where", "displayStatus='Active'")
	params.Set("geometry", fmt.Sprintf("%s,%s,%s,%s",
		ftoa(b.MinLongitude), ftoa(b.MinLatitude), ftoa(b.MaxLongitude), ftoa(b.MaxLatitude)))
	params.Set("geometryType", "esriGeometryEnvelope")
	params.Set("inSR", "4326")
	params.Set("spatialRel", "esriSpatialRelIntersects")
	params.Set("outFields", "incident_name,mission,incident_number,area_acres,poly_DateCurrent,source,displayStatus")
	params.Set("orderByFields", "OBJECTID") // stable feature order across polls (dedup determinism)
	params.Set("returnGeometry", "true")
	params.Set("maxAllowableOffset", "0.001") // ~100m vertex tolerance

	body, err := c.get(ctx, c.baseURL+"?"+params.Encode(), "application/geo+json", maxBody)
	if err != nil {
		return nil, err
	}
	var parsed perimeterResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode FIRIS response: %w", err)
	}
	// ArcGIS signals throttle/quota errors with HTTP 200 + an error envelope.
	if parsed.Error != nil {
		return nil, fmt.Errorf("FIRIS ArcGIS error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	out := make([]Perimeter, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		p := f.Properties
		out = append(out, Perimeter{
			IncidentName:   strings.TrimSpace(p.IncidentName),
			Mission:        strings.TrimSpace(p.Mission),
			IncidentNumber: strings.TrimSpace(p.IncidentNumber),
			Acres:          p.Acres,
			DateCurrent:    epochMs(p.DateCurrent),
			Source:         p.Source,
			Status:         p.Status,
			GeometryType:   f.Geometry.Type,
			GeometryCoords: f.Geometry.Coordinates,
		})
	}
	return out, nil
}

// LastEdit returns the layer's dataLastEditDate — when ANY perimeter last changed.
// Off the cheap, CDN-cached metadata endpoint; callers gate GetPerimeters on it.
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
		return time.Time{}, fmt.Errorf("failed to decode FIRIS metadata: %w", err)
	}
	if meta.Error != nil {
		return time.Time{}, fmt.Errorf("FIRIS ArcGIS error %d: %s", meta.Error.Code, meta.Error.Message)
	}
	if meta.EditingInfo.DataLastEditDate == 0 {
		return time.Time{}, fmt.Errorf("FIRIS metadata: no dataLastEditDate")
	}
	return epochMs(meta.EditingInfo.DataLastEditDate), nil
}

// ErrNotFound signals an HTTP 404 (e.g. the service was moved).
var ErrNotFound = errors.New("firis: not found")

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func (c *Client) get(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create FIRIS request: %w", err)
	}
	req.Header.Set("Accept", accept)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute FIRIS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("FIRIS API error 404 (%s): %w", url, ErrNotFound)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("FIRIS API error %d: %s", resp.StatusCode, string(b))
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func epochMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

type perimeterResponse struct {
	Features []perimeterFeature `json:"features"`
	Error    *arcgisError       `json:"error"`
}

type arcgisError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type perimeterFeature struct {
	Properties perimeterProps `json:"properties"`
	Geometry   geometryJSON   `json:"geometry"`
}

type perimeterProps struct {
	IncidentName   string  `json:"incident_name"`
	Mission        string  `json:"mission"`
	IncidentNumber string  `json:"incident_number"`
	Acres          float64 `json:"area_acres"`
	DateCurrent    int64   `json:"poly_DateCurrent"`
	Source         string  `json:"source"`
	Status         string  `json:"displayStatus"`
}

type geometryJSON struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}
