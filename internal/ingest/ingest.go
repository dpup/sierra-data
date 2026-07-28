// Package ingest normalizes upstream hazard feeds into canonical grid.v1
// Events for the event store (docs/v2-implementation-plan.md Tier C). Each
// Normalizer owns one poller scope and reproduces the shipped
// /api/v1/hazards envelope semantics — id namespaces, headline formats, and
// severity mappings (delegated to internal/hazards' exported wrappers) — so
// the store-backed GeoJSON projection can stay byte-compatible with the live
// builders.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// Normalizer is one poller's contract: fetch its upstream(s) and return the
// full current set of events in scope. The scheduler diffs that set against
// the store to apply per-source disappearance policy.
type Normalizer interface {
	// SourceIDs lists the source-registry rows this poller updates. A poller
	// may update several (wildfire → calfire + firis); poller ≠ source.
	SourceIDs() []string
	// Poll fetches the upstream(s). prior is the store's current active set
	// for this poller's sources — normalizers use it to carry identity and
	// state across ticks (e.g. keeping a joined fire's id stable while one
	// sibling feed is down). It is never nil when called by the scheduler.
	Poll(ctx context.Context, prior Prior) (*PollResult, error)
}

// Prior is a read-only view of the active events already in the store for a
// poller's sources, built by the scheduler from ActiveEventsBySource before
// each tick.
type Prior interface {
	// ByID returns the active/scheduled event with this id, or nil.
	ByID(id string) *gridv1.Event
	// ForSource returns the active/scheduled events for one source id.
	ForSource(sourceID string) []*gridv1.Event
}

// PollResult carries a poll's events plus per-source partial failures. A
// source id maps to an error only when that source failed while another in
// the same poller succeeded (its events are still returned); a fully-failed
// poll returns an error from Poll instead. Nil/absent entries mean success.
type PollResult struct {
	Events    []*gridv1.Event
	PerSource map[string]error
	// SweepSuppress lists source ids whose fetch SUCCEEDED this tick but
	// whose disappearance sweep must be skipped anyway: the poller could not
	// compute that source's full current set (e.g. wildfire can't tell which
	// perimeters are standalone while the sibling CAL FIRE feed is down), so
	// an event missing from Events proves nothing. RecordAttempt still
	// records the success — health and lifecycle are deliberately separate.
	SweepSuppress []string
	// MeshObservations is the mesh-node reception firehose drained from the push
	// source's buffer this tick (nil for every other poller). Receptions are
	// measurements, not events — the scheduler batch-inserts them into the
	// append-only observation store (Tier 0) in the same writer context as the
	// presence upserts, never touching the revisioned event path. See
	// docs/mesh-topology-design.md.
	MeshObservations []store.MeshObservation
}

// NewEvent builds an event with the envelope fields every normalizer sets.
func NewEvent(id string, layer gridv1.Layer, sev gridv1.Severity, status gridv1.EventStatus, headline string) *gridv1.Event {
	return &gridv1.Event{
		Id:       id,
		Layer:    layer,
		Severity: sev,
		Status:   status,
		Headline: headline,
	}
}

// SeverityFromLabel maps a unified severity label ("INFO".."EXTREME", the
// shipped string scale from internal/hazards) onto the proto enum. Unknown
// labels map to INFO (rank 0), mirroring severityRank's default.
func SeverityFromLabel(label string) gridv1.Severity {
	if v, ok := gridv1.Severity_value[label]; ok {
		return gridv1.Severity(v)
	}
	return gridv1.Severity_INFO
}

// GeometryFromGeoJSON wraps a raw RFC 7946 geometry object with its derived
// bbox + centroid (promoted at ingest per spec §3, so the store can index
// without re-parsing).
func GeometryFromGeoJSON(raw []byte) (*gridv1.Geometry, error) {
	g, err := geojson.Parse(raw)
	if err != nil {
		return nil, err
	}
	minLat, minLng, maxLat, maxLng := g.Bbox()
	lat, lng := g.Centroid()
	return &gridv1.Geometry{
		Geojson:  raw,
		Bbox:     &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng},
		Centroid: &gridv1.LatLng{Lat: lat, Lng: lng},
	}, nil
}

// GeometryFromPoint builds a Point geometry from internal (lat, lng). The
// encoded GeoJSON is trimmed to 5 decimals (~1.1 m, the repo convention);
// bbox/centroid keep full precision — they are indexing anchors, not wire data.
func GeometryFromPoint(lat, lng float64) *gridv1.Geometry {
	return &gridv1.Geometry{
		Geojson:  geojson.PointGeoJSON(lat, lng),
		Bbox:     &gridv1.BoundingBox{MinLat: lat, MinLng: lng, MaxLat: lat, MaxLng: lng},
		Centroid: &gridv1.LatLng{Lat: lat, Lng: lng},
	}
}

// geometryFromTyped assembles a geometry object from an upstream type + raw
// coordinates pair (the ArcGIS clients return them split) and derives
// bbox/centroid. Coordinates are passed through verbatim ([lng, lat] order,
// already simplified server-side).
func geometryFromTyped(geomType string, coords json.RawMessage) (*gridv1.Geometry, error) {
	if geomType == "" || len(coords) == 0 {
		return nil, fmt.Errorf("ingest: empty geometry")
	}
	t, err := json.Marshal(geomType)
	if err != nil {
		return nil, err
	}
	var raw []byte
	raw = append(raw, `{"type":`...)
	raw = append(raw, t...)
	raw = append(raw, `,"coordinates":`...)
	raw = append(raw, coords...)
	raw = append(raw, '}')
	return GeometryFromGeoJSON(raw)
}

// tsProto converts a time to a proto timestamp, nil for the zero value (so
// absent upstream times stay absent on the wire).
func tsProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// NewProvenance builds an event's provenance. Per-source constants (name,
// attribution) live with each normalizer and match the shipped envelope's
// Source blocks exactly. FetchedAt is stamped now — the content hash zeroes
// it, so a re-poll without content change never creates a revision.
func NewProvenance(sourceID, sourceName, attribution, sourceURL string) *gridv1.Provenance {
	return &gridv1.Provenance{
		SourceId:    sourceID,
		SourceName:  sourceName,
		Attribution: attribution,
		SourceUrl:   sourceURL,
		FetchedAt:   timestamppb.Now(),
	}
}

// errEmptyScope is what a poller returns when its configured scope is empty
// (no hazard/incident areas). It MUST be a hard Poll error, never a
// success-empty PollResult: a "successful" empty poll would let the
// scheduler's disappearance sweep RESOLVE every stored active event (a
// fabricated all-clear written into history) and RecordAttempt mark the
// source healthy — all from a config regression, with no fetch ever made.
func errEmptyScope(what string) error {
	return fmt.Errorf("ingest: no %s configured; refusing to poll an empty scope", what)
}

// unionBounds is the union bbox of the configured hazard areas — the single
// spatial scope ingest polls (per-area scoping happens at query time via the
// store's place joins). ok is false when no areas are configured.
func unionBounds(areas []config.HazardArea) (minLat, minLng, maxLat, maxLng float64, ok bool) {
	for i, a := range areas {
		b := a.Bounds
		if i == 0 {
			minLat, minLng, maxLat, maxLng = b.MinLatitude, b.MinLongitude, b.MaxLatitude, b.MaxLongitude
			continue
		}
		minLat = math.Min(minLat, b.MinLatitude)
		minLng = math.Min(minLng, b.MinLongitude)
		maxLat = math.Max(maxLat, b.MaxLatitude)
		maxLng = math.Max(maxLng, b.MaxLongitude)
	}
	return minLat, minLng, maxLat, maxLng, len(areas) > 0
}

// zonesMatch reports whether a zone-carrying record belongs to an area, by
// NWS forecast zone (same semantics as the shipped hazards builder):
//   - area has no configured zones -> matches (unscoped single-area deployment)
//   - record has no zones          -> matches (can't be scoped; keep it)
//   - otherwise                    -> the zone sets intersect
func zonesMatch(areaZones, recordZones []string) bool {
	if len(areaZones) == 0 || len(recordZones) == 0 {
		return true
	}
	set := make(map[string]bool, len(areaZones))
	for _, z := range areaZones {
		set[z] = true
	}
	for _, z := range recordZones {
		if set[z] {
			return true
		}
	}
	return false
}

// safeURL returns u only if it is an http(s) URL; upstream data is untrusted
// and a javascript:/data: URL rendered by a client is an XSS vector (the
// shipped hazards rule).
func safeURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}

// nonEmpty returns the first value that isn't blank.
func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
