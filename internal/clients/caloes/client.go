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
	"strings"
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
//
// WHICH FIELDS CAL OES ACTUALLY POPULATES CHANGES. Measured across all 37
// active rows statewide on 2026-08-13: ZoneName, EventType, PublicInfo (and the
// CITY/CRITICAL_INFO columns we never read) were empty on EVERY row, while
// Notes and EditedAt were populated on every row — the public directive text
// that used to arrive in PUBLIC_INFO now arrives in NOTES ("...is issuing an
// immediate EVACUATION ORDER... Leave Now."). The old fields are still read, so
// they work if Cal OES repopulates them; consumers must treat both as optional
// and fall back. Same lesson as the 2026 Caltrans KML relayout: an upstream can
// change shape without ever failing a request, because null parses fine.
type EvacZone struct {
	ZoneID    string
	ZoneName  string
	County    string
	Status    string // raw upstream status text
	EventType string
	// PublicInfo is the legacy directive field (PUBLIC_INFO). Empty on every
	// live row as of 2026-08-13 — read Notes as well, never instead.
	PublicInfo string
	// Notes is the NOTES column, where the public directive text now lives. Its
	// content varies by county: a street ("Southgate Dr" — Tuolumne), the full
	// sheriff's instruction (Monterey, Humboldt), or the event type
	// ("Flooding" — Tulare). Free text: carry it, don't parse it.
	Notes string
	// LastUpdated is STATEWIDE_LAST_UPDATED — null on every live row as of
	// 2026-08-13. Prefer EditedAt when this is zero.
	LastUpdated time.Time
	// EditedAt is ArcGIS editor tracking (EditDate): when Cal OES's own script
	// last touched this row. It is the ONLY reliable freshness signal on this
	// layer, and it is what exposes an ORPHANED row — a zone the county lifted
	// but the aggregation never retracted. Observed live: every county's rows
	// within 1.8 days except one Tuolumne row frozen at 6.3 days.
	EditedAt       time.Time
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

// countyViewers maps a Cal OES COUNTY value to that county's own authoritative
// evacuation-zone viewer, for counties that are NOT on Genasys.
//
// This table exists because the previous fallback was actively misleading: a
// non-Genasys zone got the generic protect.genasys.com viewer, which does not
// contain that county's zones at all. The regex above already identifies these
// counties correctly — the code knew Tuolumne was "a different vendor" and then
// linked to Genasys anyway, sending a resident to a viewer their zone will
// never appear in.
//
// Keyed on COUNTY, not on the zone id, because a non-Genasys id has no scheme
// to parse (Tuolumne's is literally "US-CA-Toulumne117" — including upstream's
// misspelling of the county).
//
// CAVEAT worth knowing when reading a link from here: these county viewers show
// only CURRENTLY LIVE zones. Our record can outlast them, because Cal OES's
// aggregation sometimes keeps a row a county has already lifted (see
// EvacZone.EditedAt). A zone that is absent from the viewer is evidence the
// order was lifted — which is information, but it is not a deep link, so we
// never construct one.
var countyViewers = map[string]string{
	"TUOLUMNE": "https://experience.arcgis.com/experience/701c7fd899574b6ea6c1596cbbd1dcc6/page/Page?org=Tuolumne",
}

// ZoneURL returns the most specific public viewer URL for a zone:
//
//  1. a protect.genasys.com deep link, when ZONE_ID is a Genasys-scheme id;
//  2. the county's own viewer, for a non-Genasys county we have mapped;
//  3. the generic Genasys viewer, otherwise.
//
// Case 3 remains imperfect for an unmapped non-Genasys county — but every such
// county is outside this service's footprint (in-area: Calaveras is Genasys,
// Tuolumne is mapped), and the layer-level metadata.sourceUrl still points at
// Cal OES itself in every state.
func ZoneURL(zoneID, county string) string {
	if genasysZoneRe.MatchString(zoneID) {
		return "https://protect.genasys.com/zones/" + url.PathEscape(zoneID)
	}
	if v, ok := countyViewers[strings.ToUpper(strings.TrimSpace(county))]; ok {
		return v
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
	// NOTES and EditDate are not optional extras: as of 2026-08-13 they are the
	// only populated text and freshness columns on this layer (see EvacZone).
	params.Set("outFields", "ZONE_ID,ZONE_NAME,COUNTY,STATUS,EVENT_TYPE,PUBLIC_INFO,NOTES,STATEWIDE_LAST_UPDATED,EditDate")
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
			Notes:          f.Properties.Notes,
			LastUpdated:    msToTime(f.Properties.LastUpdated),
			EditedAt:       msToTime(f.Properties.EditDate),
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
	Notes       string `json:"NOTES"`
	LastUpdated int64  `json:"STATEWIDE_LAST_UPDATED"`
	// ArcGIS editor tracking, epoch ms. Case-sensitive: the column is
	// `EditDate`, distinct from the layer's own always-null `EDIT_DATE`.
	EditDate int64 `json:"EditDate"`
}

type geometryJSON struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}
