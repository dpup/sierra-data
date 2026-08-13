// Package pge provides a client for Pacific Gas and Electric's public ArcGIS
// outage services — the backend behind PG&E's own outage map. It covers two
// independent feeds:
//
//   - OUTAGES (`43/outages`): live electric outages, points on layer 4 and
//     polygons on layer 8, joined on OUTAGE_ID. Authoritatively active-only —
//     a restored outage leaves the layer.
//   - PSPS (`43/psps_public`): Public Safety Power Shutoff coverage polygons,
//     empty outside an event.
//
// Plus `43/lastupdate_time`, PG&E's own ETL stamps per service. The outage
// stamp is what lets a caller tell a live feed from a FROZEN one: a stalled
// ETL keeps serving the last set, so without it "still listed" and "still out"
// are indistinguishable. (This is not hypothetical — the Cal OES statewide
// mirror of this same data was observed 26 h stale while reporting every row
// as Active, which is why we read PG&E directly.)
//
// Like the other ArcGIS clients here, a transport error, a non-2xx, or an
// ArcGIS HTTP-200 error envelope returns an error so the caller can surface
// UNAVAILABLE; a clean fetch with no rows returns an empty slice and nil.
//
// CAVEAT: these endpoints are undocumented and unversioned — PG&E publishes no
// API contract, no terms, and no robots.txt for them. Treat a schema change as
// a matter of when, and fail loud (never empty-as-all-clear) when it happens.
// The `43/psps_staging` service on the same host holds PG&E's TEST events
// (names like PSPS_05312024_SKN9_TEST52, future-dated windows) — never consume
// it.
package pge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxBody caps a response read. PSPS coverage polygons are the large case: the
// raw statewide-shaped geometry runs to megabytes, which is why the PSPS query
// asks for simplified geometry (see pspsMaxAllowableOffset).
const maxBody = 16 << 20 // 16 MiB

// DefaultBaseURL is folder 43 of PG&E's ArcGIS server — the root the outage
// map's own layers hang off.
const DefaultBaseURL = "https://ags.pge.esriemcs.com/arcgis/rest/services/43"

// Public viewer URLs, always surfaced to users (fail-loud rule: attribution and
// an authoritative link render even when we have zero rows).
const (
	// OutageMapURL is PG&E's public outage map.
	OutageMapURL = "https://pgealerts.alerts.pge.com/outage-tools/outage-map/"
	// PSPSUpdatesURL is PG&E's public PSPS status page.
	PSPSUpdatesURL = "https://pgealerts.alerts.pge.com/psps-updates/"
)

// Layer paths within the base. Layer numbers are PG&E's, confirmed against the
// service's own metadata: outages 4 = "Outage Locations" (every outage, as a
// point), 8 = "Outage Polygon" (the affected area, where PG&E has traced one),
// psps_public 1 = "PSPS Coverage".
const (
	outagePointsPath   = "/outages/MapServer/4/query"
	outagePolygonsPath = "/outages/MapServer/8/query"
	pspsCoveragePath   = "/psps_public/MapServer/1/query"
	outageStampPath    = "/lastupdate_time/MapServer/1/query"
)

// geometryPrecision is the coordinate decimal count we ask ArcGIS for. 5 dp is
// ~1.1 m and matches the repo-wide GeoJSON convention (geojson.PointGeoJSON
// trims to the same), so stored geometry is consistent across sources.
const geometryPrecision = "5"

// pspsMaxAllowableOffset simplifies PSPS coverage geometry server-side, in
// degrees (~55 m). A PSPS footprint is a county-scale polygon whose full
// vertex density is meaningless at any zoom a client will render it at, and
// asking for it is expensive: measured against a real PSPS-shaped polygon set,
// dropping this in took the response from 8.0 MB to 222 KB (36x) with the same
// feature count. Outage polygons are NOT simplified — they are small enough
// (a few hundred metres across) that a 55 m tolerance would deform them.
const pspsMaxAllowableOffset = "0.0005"

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries PG&E's public outage + PSPS feature services.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a PG&E client against the live service.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    DefaultBaseURL,
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + service root
// (testing). The root is folder 43; layer paths are appended.
func NewClientWithHTTPDoer(baseURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: strings.TrimSuffix(baseURL, "/")}
}

// Bounds is a lat/lng bounding box for the spatial query.
type Bounds struct {
	MinLatitude  float64
	MaxLatitude  float64
	MinLongitude float64
	MaxLongitude float64
}

// Outage is one PG&E electric outage, with the affected-area polygon joined in
// when PG&E published one (otherwise the reported point).
type Outage struct {
	ID                   string
	Cause                string // raw PG&E code: "PLNND SHUTDOWN", "TREE CONTACT", "FIRE", often blank
	CrewStatus           string // "Awaiting T-Man", "Crew On Site", "No Access", ...
	CustomersAffected    int32
	Start                time.Time
	LastUpdate           time.Time
	EstimatedRestoration time.Time
	// HasPolygon is true when geometry came from the polygon layer rather than
	// the point layer.
	HasPolygon     bool
	GeometryType   string
	GeometryCoords json.RawMessage
}

// plannedCauses are the PG&E cause codes that mark a pre-notified, scheduled
// de-energization rather than an unplanned failure. Matched case-insensitively
// on a prefix so PG&E's abbreviation variants ("PLNND SHUTDOWN") all land.
var plannedCauses = []string{"plnnd", "planned", "sched"}

// Planned reports whether the outage is a scheduled shutdown. PG&E encodes
// this only in the free-text cause code — there is no boolean on the feed.
func (o Outage) Planned() bool {
	c := strings.ToLower(strings.TrimSpace(o.Cause))
	for _, p := range plannedCauses {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// PSPSArea is ONE POLYGON ROW of the PSPS coverage layer, not a whole event: a
// single de-energization window is published as many polygon rows that share
// every attribute. Grouping them into one event is the caller's job (see
// internal/ingest), because the grouping key is an event-model decision.
type PSPSArea struct {
	EventID                 string
	EventName               string
	TimePeriod              string
	Stage                   string // Watch | Warning
	CustomersAffected       int32
	MedicalBaselineAffected int32
	DeEnergizationStart     time.Time
	DeEnergizationEnd       time.Time
	AllClear                time.Time
	EstimatedRestoration    time.Time
	LastUpdated             time.Time
	GeometryType            string
	GeometryCoords          json.RawMessage
}

// GetOutages returns active outages intersecting the bounding box, with the
// affected-area polygon joined onto each point by OUTAGE_ID.
//
// BOTH layer fetches must succeed. Degrading to point-only geometry when the
// polygon layer fails would look harmless but is not: an outage's geometry
// would flip polygon -> point -> polygon across ticks, and geometry is part of
// the event content hash, so every blip would mint a spurious revision pair in
// the history of an outage that never changed.
func (c *Client) GetOutages(ctx context.Context, b Bounds) ([]Outage, error) {
	points, err := c.queryOutageLayer(ctx, outagePointsPath, b)
	if err != nil {
		return nil, fmt.Errorf("PG&E outage points: %w", err)
	}
	polys, err := c.queryOutageLayer(ctx, outagePolygonsPath, b)
	if err != nil {
		return nil, fmt.Errorf("PG&E outage polygons: %w", err)
	}

	// An outage can be published as several polygon rows (a multi-part affected
	// area). Combine them into one MultiPolygon so the outage stays ONE event.
	// Ids are trimmed at parse (outageProps.id()) so the join key, the map key
	// and the emitted Outage.ID are the SAME string — keying the map on the raw
	// value while emitting a trimmed one would both miss the join and let two
	// rows collapse onto one emitted id.
	byID := make(map[string][]outageFeature, len(polys))
	var polyOrder []string
	skippedPolys := 0
	for _, f := range polys {
		id := f.Properties.id()
		if id == "" {
			skippedPolys++
			continue
		}
		if _, seen := byID[id]; !seen {
			polyOrder = append(polyOrder, id)
		}
		byID[id] = append(byID[id], f)
	}

	out := make([]Outage, 0, len(points))
	seen := make(map[string]bool, len(points))
	skippedPoints := 0
	for _, f := range points {
		id := f.Properties.id()
		if id == "" {
			// No stable identity: it could not be tracked across polls.
			skippedPoints++
			continue
		}
		if seen[id] {
			// Two point rows for one outage. The polygon layer does publish
			// multi-row outages, so this is plausible here too — and emitting
			// both would hand the store two events under the SAME id, which it
			// would then have overwrite each other every tick, flip-flopping the
			// served record forever (ArcGIS promises no row order). One outage,
			// one row.
			continue
		}
		seen[id] = true
		o := outageFromFeature(f)
		if group, ok := byID[o.ID]; ok {
			if t, coords, err := combineGeometry(group); err == nil {
				o.GeometryType, o.GeometryCoords, o.HasPolygon = t, coords, true
			}
		}
		out = append(out, o)
	}

	// The polygon layer occasionally carries an outage the point layer has not
	// caught up on. Dropping it would under-report an outage PG&E is publishing,
	// so keep it, sourced from its own rows.
	for _, id := range polyOrder {
		if seen[id] {
			continue
		}
		group := byID[id]
		// Same reasoning as combineGeometry's sort: these attributes (cause,
		// crew status, customer count, ETOR) are in the event content hash, and
		// ArcGIS promises no row order — so picking "whichever row came first"
		// would mint a spurious revision pair every time a multi-row
		// polygon-only outage came back reordered.
		sort.SliceStable(group, func(i, j int) bool {
			return string(group[i].Geometry.Coordinates) < string(group[j].Geometry.Coordinates)
		})
		o := outageFromFeature(group[0])
		if t, coords, err := combineGeometry(group); err == nil {
			o.GeometryType, o.GeometryCoords, o.HasPolygon = t, coords, true
		}
		out = append(out, o)
	}

	// SCHEMA-BREAK GUARD, applied PER LAYER. If a layer returned rows but not
	// one of them yielded a usable id, `OUTAGE_ID` has been renamed or retyped
	// there — on endpoints whose own package doc calls that a matter of when,
	// not if. encoding/json ignores the unknown key silently, so the rows are
	// skipped above and nothing else in the response looks wrong.
	//
	// It has to be per layer, not "did we end up with zero outages":
	//
	//   - A break on the POINT layer alone yields an empty result. The caller's
	//     policy is `resolve`, so that publishes "power restored" for every
	//     stored outage in the region.
	//   - A break on the POLYGON layer alone is quieter and just as bad: the
	//     point rows still carry ids, so `out` is non-empty and a whole-result
	//     check stays silent — while every outage silently reverts polygon ->
	//     point geometry. Geometry is in the event content hash, so that mints a
	//     spurious revision for EVERY stored outage, and another when the layer
	//     recovers. It is the exact flip the both-layers-must-succeed rule above
	//     exists to prevent.
	if len(points) > 0 && skippedPoints == len(points) {
		return nil, fmt.Errorf("PG&E outage points: all %d row(s) lack an OUTAGE_ID — the feed schema has probably changed", len(points))
	}
	if len(polys) > 0 && skippedPolys == len(polys) {
		return nil, fmt.Errorf("PG&E outage polygons: all %d row(s) lack an OUTAGE_ID — the feed schema has probably changed", len(polys))
	}
	return out, nil
}

// GetPSPSAreas returns PSPS coverage polygon rows intersecting the bounding
// box. An empty, non-error result is a genuine "no shutoff in progress or
// planned" — this layer is empty outside an event — but it is only meaningful
// because a failed fetch errors instead.
func (c *Client) GetPSPSAreas(ctx context.Context, b Bounds) ([]PSPSArea, error) {
	params := spatialParams(b)
	params.Set("maxAllowableOffset", pspsMaxAllowableOffset)

	var parsed pspsResponse
	if err := c.fetch(ctx, pspsCoveragePath, params, &parsed); err != nil {
		return nil, fmt.Errorf("PG&E PSPS coverage: %w", err)
	}
	out := make([]PSPSArea, 0, len(parsed.Features))
	for _, f := range parsed.Features {
		p := f.Properties
		out = append(out, PSPSArea{
			EventID:    strings.TrimSpace(p.EventID),
			EventName:  strings.TrimSpace(p.EventName),
			TimePeriod: strings.TrimSpace(p.TimePeriod),
			Stage:      strings.TrimSpace(p.Stage),
			// PG&E publishes these counts as STRINGS on this layer (they are
			// numbers on the outage layer) — parse, don't assume.
			CustomersAffected:       atoi32(p.TotCustAff),
			MedicalBaselineAffected: atoi32(p.TotMBLAff),
			DeEnergizationStart:     parseISO(p.DeEngStart),
			DeEnergizationEnd:       parseISO(p.DeEngEnd),
			AllClear:                parseISO(p.AllClear),
			EstimatedRestoration:    parseISO(p.ETOR),
			LastUpdated:             parseISO(p.LstUpdated),
			GeometryType:            f.Geometry.Type,
			GeometryCoords:          f.Geometry.Coordinates,
		})
	}
	return out, nil
}

// GetOutagesLastUpdate returns PG&E's own ETL stamp for the outage service —
// when the outage layers were last refreshed from the outage management system,
// NOT when we fetched them. A caller compares it to now to tell a live feed
// from a frozen one; see the package doc.
//
// Observed live behaviour: it advances every few minutes, trailing real time by
// ~3 minutes.
func (c *Client) GetOutagesLastUpdate(ctx context.Context) (time.Time, error) {
	params := url.Values{}
	params.Set("f", "pjson")
	params.Set("where", "1=1")
	params.Set("outFields", "*")
	params.Set("returnGeometry", "false")

	var parsed stampResponse
	if err := c.fetch(ctx, outageStampPath, params, &parsed); err != nil {
		return time.Time{}, fmt.Errorf("PG&E outage stamp: %w", err)
	}
	if len(parsed.Features) == 0 {
		return time.Time{}, fmt.Errorf("PG&E outage stamp: no rows returned")
	}
	// This table holds one row today, but ArcGIS promises no row ordering, so
	// take the NEWEST rather than whichever came first. Newest is also the safe
	// direction for a gate that fails open: an arbitrary older row would declare
	// a healthy feed frozen and permanently suppress its disappearance sweep.
	raw := ""
	var newest time.Time
	for _, f := range parsed.Features {
		s := strings.TrimSpace(f.Attributes.LastUpdate)
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
		if err != nil {
			continue
		}
		if ts.After(newest) {
			newest, raw = ts, s
		}
	}
	if raw == "" {
		// Nothing parsed: surface the first value so the error names what we saw.
		raw = strings.TrimSpace(parsed.Features[0].Attributes.LastUpdate)
	}
	// This table reports a bare "2006-01-02 15:04:05" with NO zone marker,
	// unlike the outage layers' epoch-ms fields. It is UTC: sampled against a
	// known clock 2.5 h apart it tracked UTC both times (a Pacific reading would
	// have been 7 h out). Parse it as UTC explicitly rather than letting the
	// local zone decide.
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("PG&E outage stamp: unparseable timestamp %q: %w", raw, err)
	}
	return ts, nil
}

// --- query plumbing ---

// queryOutageLayer fetches one of the two outage layers. They share a schema,
// so one parser serves both.
func (c *Client) queryOutageLayer(ctx context.Context, path string, b Bounds) ([]outageFeature, error) {
	params := spatialParams(b)
	// Both layers carry a large per-row push-notification blob
	// (blueSkyNotificationSubscription) that we never read; name the fields we
	// use instead of pulling `*`.
	params.Set("outFields", "OUTAGE_ID,OUTAGE_CAUSE,CREW_CURRENT_STATUS,EST_CUSTOMERS,OUTAGE_START,LAST_UPDATE,CURRENT_ETOR")

	var parsed outageResponse
	if err := c.fetch(ctx, path, params, &parsed); err != nil {
		return nil, err
	}
	return parsed.Features, nil
}

// spatialParams builds the shared GeoJSON envelope-intersect query. Filtering
// geographically (rather than by the COUNTY field) is deliberate: PG&E leaves
// COUNTY null on most outage rows.
func spatialParams(b Bounds) url.Values {
	envelope := fmt.Sprintf(`{"xmin":%s,"ymin":%s,"xmax":%s,"ymax":%s,"spatialReference":{"wkid":4326}}`,
		ftoa(b.MinLongitude), ftoa(b.MinLatitude), ftoa(b.MaxLongitude), ftoa(b.MaxLatitude))
	params := url.Values{}
	params.Set("f", "geojson")
	params.Set("where", "1=1")
	params.Set("geometry", envelope)
	params.Set("geometryType", "esriGeometryEnvelope")
	params.Set("inSR", "4326")
	params.Set("spatialRel", "esriSpatialRelIntersects")
	params.Set("outFields", "*")
	params.Set("returnGeometry", "true")
	params.Set("outSR", "4326")
	params.Set("geometryPrecision", geometryPrecision)
	return params
}

// fetch performs one query and decodes it, applying the shared failure rules.
func (c *Client) fetch(ctx context.Context, path string, params url.Values, out arcgisEnveloped) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/geo+json, application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	// ArcGIS signals quota/throttle/token failures with HTTP 200 + an error
	// envelope. It must surface as an error: an outage layer that silently reads
	// empty is an all-clear we did not earn.
	if e := out.arcgisError(); e != nil {
		return fmt.Errorf("ArcGIS error %d: %s", e.Code, e.Message)
	}
	// TRUNCATION GUARD. Past the layer's maxRecordCount (2000 here) ArcGIS
	// returns the first N rows and sets this flag — a partial answer that looks
	// exactly like a complete one. The caller treats what it gets as the whole
	// current set and resolves anything missing from it, so serving a truncated
	// page would RESOLVE the entire tail: during the widespread outage that
	// caused the truncation, precisely the rows that matter most.
	//
	// This service reports supportsPagination: None, so there is no
	// resultOffset paging to fall back on. Failing loud is the honest option:
	// the stored events survive, the sweep is skipped, and the layer degrades to
	// STALE rather than silently shrinking.
	if out.truncated() {
		return fmt.Errorf("response truncated by the server's maxRecordCount (exceededTransferLimit); refusing to treat a partial set as complete")
	}
	return nil
}

// combineGeometry merges a group of polygon rows into one geometry: a single
// row passes through unchanged, several become a MultiPolygon. This is a
// STRUCTURAL merge (concatenating coordinate arrays), not a geometric union —
// overlapping parts stay separate rings, which renders identically and needs no
// geometry library.
func combineGeometry(group []outageFeature) (string, json.RawMessage, error) {
	parts := make([]json.RawMessage, 0, len(group))
	for _, f := range group {
		if len(f.Geometry.Coordinates) == 0 {
			continue
		}
		switch f.Geometry.Type {
		case "Polygon":
			parts = append(parts, f.Geometry.Coordinates)
		case "MultiPolygon":
			// Splice a MultiPolygon's members in individually so the result is a
			// flat MultiPolygon, never a nested one.
			var members []json.RawMessage
			if err := json.Unmarshal(f.Geometry.Coordinates, &members); err != nil {
				continue
			}
			parts = append(parts, members...)
		default:
			continue
		}
	}
	switch len(parts) {
	case 0:
		return "", nil, fmt.Errorf("no usable polygon geometry")
	case 1:
		return "Polygon", parts[0], nil
	default:
		// Order the members deterministically. ArcGIS makes no ordering promise
		// across queries, and the geometry bytes are part of the stored event's
		// CONTENT HASH — so if the same two polygons came back swapped on the
		// next poll, an outage that never changed would mint a revision, and
		// keep flip-flopping one every time the order did.
		sort.Slice(parts, func(i, j int) bool { return string(parts[i]) < string(parts[j]) })
		coords, err := json.Marshal(parts)
		if err != nil {
			return "", nil, err
		}
		return "MultiPolygon", coords, nil
	}
}

// CombineAreaGeometry merges PSPS coverage rows sharing an event window into
// one geometry, the same structural merge GetOutages applies to multi-part
// outage polygons. Exported because the PSPS grouping key is the caller's
// decision (see internal/ingest), so the caller assembles the groups.
func CombineAreaGeometry(areas []PSPSArea) (string, json.RawMessage, error) {
	group := make([]outageFeature, 0, len(areas))
	for _, a := range areas {
		group = append(group, outageFeature{
			Geometry: geometryJSON{Type: a.GeometryType, Coordinates: a.GeometryCoords},
		})
	}
	return combineGeometry(group)
}

func outageFromFeature(f outageFeature) Outage {
	p := f.Properties
	return Outage{
		ID:                   p.id(),
		Cause:                strings.TrimSpace(p.Cause),
		CrewStatus:           strings.TrimSpace(p.CrewStatus),
		CustomersAffected:    p.Customers,
		Start:                msToTime(p.Start),
		LastUpdate:           msToTime(p.LastUpdate),
		EstimatedRestoration: msToTime(p.ETOR),
		GeometryType:         f.Geometry.Type,
		GeometryCoords:       f.Geometry.Coordinates,
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// parseISO parses the RFC 3339 stamps the PSPS layer publishes as strings.
// Unparseable or blank yields the zero time (absent stays absent on the wire).
func parseISO(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// atoi32 parses PSPS's stringified counts; anything unparseable is 0.
func atoi32(s string) int32 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

// --- wire types ---

// arcgisEnveloped is the shared "may carry an HTTP-200 error envelope, and may
// quietly be a partial page" shape.
type arcgisEnveloped interface {
	arcgisError() *arcgisErr
	truncated() bool
}

// foreignMembers is the RFC 7946 foreign-member block ArcGIS hangs off a
// FeatureCollection. The truncation flag appears BOTH at the top level and
// nested here on PG&E's current server, but other ArcGIS versions emit only the
// nested form — and a guard that silently stops firing is worse than no guard,
// because what it protects against (a partial set read as complete, resolving
// the truncated tail) is invisible from the response.
type foreignMembers struct {
	ExceededTransferLimit bool `json:"exceededTransferLimit"`
}

type arcgisErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type outageResponse struct {
	Features              []outageFeature `json:"features"`
	Error                 *arcgisErr      `json:"error"`
	ExceededTransferLimit bool            `json:"exceededTransferLimit"`
	Properties            foreignMembers  `json:"properties"`
}

func (r *outageResponse) arcgisError() *arcgisErr { return r.Error }
func (r *outageResponse) truncated() bool {
	return r.ExceededTransferLimit || r.Properties.ExceededTransferLimit
}

type outageFeature struct {
	Properties outageProps  `json:"properties"`
	Geometry   geometryJSON `json:"geometry"`
}

// id is the trimmed OUTAGE_ID. Every consumer must go through this so the join
// key, the dedup key and the emitted id are byte-identical.
func (p outageProps) id() string { return strings.TrimSpace(p.OutageID) }

type outageProps struct {
	OutageID   string `json:"OUTAGE_ID"`
	Cause      string `json:"OUTAGE_CAUSE"`
	CrewStatus string `json:"CREW_CURRENT_STATUS"`
	Customers  int32  `json:"EST_CUSTOMERS"`
	Start      int64  `json:"OUTAGE_START"`
	LastUpdate int64  `json:"LAST_UPDATE"`
	ETOR       int64  `json:"CURRENT_ETOR"`
}

type pspsResponse struct {
	Features              []pspsFeature  `json:"features"`
	Error                 *arcgisErr     `json:"error"`
	ExceededTransferLimit bool           `json:"exceededTransferLimit"`
	Properties            foreignMembers `json:"properties"`
}

func (r *pspsResponse) arcgisError() *arcgisErr { return r.Error }
func (r *pspsResponse) truncated() bool {
	return r.ExceededTransferLimit || r.Properties.ExceededTransferLimit
}

type pspsFeature struct {
	Properties pspsProps    `json:"properties"`
	Geometry   geometryJSON `json:"geometry"`
}

type pspsProps struct {
	EventID    string `json:"EventID"`
	EventName  string `json:"EventName"`
	TimePeriod string `json:"TimePeriod"`
	Stage      string `json:"Stage"`
	TotCustAff string `json:"TotCustAff"`
	TotMBLAff  string `json:"TotMBLAff"`
	DeEngStart string `json:"DeEngStart"`
	DeEngEnd   string `json:"DeEngEnd"`
	AllClear   string `json:"AllClear"`
	ETOR       string `json:"ETOR"`
	LstUpdated string `json:"LstUpdated"`
}

type stampResponse struct {
	Features []struct {
		Attributes struct {
			LastUpdate string `json:"LAST_UPDATE"`
		} `json:"attributes"`
	} `json:"features"`
	Error *arcgisErr `json:"error"`
}

func (r *stampResponse) arcgisError() *arcgisErr { return r.Error }

// The stamp table is a single row; truncation is not a meaningful state for it.
func (r *stampResponse) truncated() bool { return false }

type geometryJSON struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}
