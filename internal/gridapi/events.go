package gridapi

import (
	"fmt"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// stripEventsIO clears enhancement.request/response on each event unless kept.
// Mutates in place — safe because the store returns freshly unmarshaled events
// per query (never shared with the caching layer).
func stripEventsIO(events []*gridv1.Event, keep bool) {
	if keep {
		return
	}
	for _, ev := range events {
		if e := ev.GetEnhancement(); e != nil {
			e.Request = ""
			e.Response = ""
		}
	}
}

// stripRevisionsIO is stripEventsIO over a revision list.
func stripRevisionsIO(revs []*gridv1.EventRevision, keep bool) {
	if keep {
		return
	}
	for _, rev := range revs {
		if e := rev.GetEvent().GetEnhancement(); e != nil {
			e.Request = ""
			e.Response = ""
		}
	}
}

// --- query-param parsers (shared by events, history, places) ---

// layerAlias maps friendly query tokens onto their canonical enum name. The
// mesh-node presence layer's enum is MESH, but the map layer is slugged
// "mesh_node" and the layer was historically "network" (pre-rename); both
// resolve here so one query token addresses the .geojson layer and ?layer=
// alike, and pre-rename clients passing "network" keep working.
var layerAlias = map[string]string{
	"NETWORK":   "MESH", // legacy: the layer's enum was NETWORK before the mesh rename
	"MESH_NODE": "MESH", // the map-layer routing slug
}

// parseLayers accepts repeated layer params; each value is an enum name
// matched case-insensitively, which also covers the shipped lowercase layer
// slugs ("wildfire", "road_incident") since those uppercase onto the enum
// names exactly. A small alias table (layerAlias) covers the mesh layer whose
// slug/legacy name diverge from its enum name — "mesh_node" and the legacy
// "network" both resolve to MESH. Comma-separated lists inside one param are
// accepted too. LAYER_UNSPECIFIED and unknown values are rejected.
func parseLayers(vals []string) ([]gridv1.Layer, error) {
	var out []gridv1.Layer
	for _, raw := range vals {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			key := strings.ToUpper(v)
			if a, ok := layerAlias[key]; ok {
				key = a
			}
			n, ok := gridv1.Layer_value[key]
			if !ok || n == int32(gridv1.Layer_LAYER_UNSPECIFIED) {
				return nil, fmt.Errorf("unknown layer: %q", v)
			}
			out = append(out, gridv1.Layer(n))
		}
	}
	return out, nil
}

// parseStatuses accepts repeated status params (enum names,
// case-insensitive). Empty input returns nil, which the store defaults to
// ACTIVE+SCHEDULED — the "what's happening now" read.
func parseStatuses(vals []string) ([]gridv1.EventStatus, error) {
	var out []gridv1.EventStatus
	for _, raw := range vals {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			n, ok := gridv1.EventStatus_value[strings.ToUpper(v)]
			if !ok || n == int32(gridv1.EventStatus_EVENT_STATUS_UNSPECIFIED) {
				return nil, fmt.Errorf("unknown status: %q", v)
			}
			out = append(out, gridv1.EventStatus(n))
		}
	}
	return out, nil
}

// parseSeverity maps a severity name (case-insensitive) onto the enum.
// "INFO" is valid and means no minimum.
func parseSeverity(v string) (gridv1.Severity, error) {
	n, ok := gridv1.Severity_value[strings.ToUpper(strings.TrimSpace(v))]
	if !ok {
		return 0, fmt.Errorf("unknown severity_min: %q", v)
	}
	return gridv1.Severity(n), nil
}

// parseRFC3339 parses one timestamp param, naming it in the error.
func parseRFC3339(name, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %q is not RFC 3339", name, v)
	}
	return t, nil
}

// isBadToken distinguishes a client-supplied garbage page_token (400) from a
// genuine store failure (500). The store wraps token decode failures with
// this stable marker; the cursor type itself is unexported there, so a
// message probe is the seam we have.
func isBadToken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid page token")
}
