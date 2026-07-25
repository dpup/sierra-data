package gridapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/store"
)

// defaultMeshWindow is the recency window for GET /api/v1/mesh/links when the
// caller sends none — ~6 advert cycles of a 12h backbone repeater, so a couple
// of missed adverts on a shaky night still render the link.
const defaultMeshWindow = 72 * time.Hour

// maxMeshWindow clamps an absurd window to the rollup retention ceiling.
const maxMeshWindow = 400 * 24 * time.Hour

// meshLinkJSON is one edge on the wire (camelCase, matching the rest of /api/v1).
type meshLinkJSON struct {
	A            string  `json:"a"`
	B            string  `json:"b"`
	Observations int     `json:"observations"`
	DaysActive   int     `json:"daysActive"`
	FirstSeen    string  `json:"firstSeen"`
	LastSeen     string  `json:"lastSeen"`
	BestSnr      float64 `json:"bestSnr"`
}

type meshLinksResponse struct {
	Window      string         `json:"window"`
	GeneratedAt string         `json:"generatedAt"`
	Links       []meshLinkJSON `json:"links"`
}

// serveMeshLinks handles GET /api/v1/mesh/links — the derived MeshCore relay
// topology as an undirected, weighted edge list over a recency window (Tier 1
// rollup history merged with the un-compacted Tier 0 tail). It is global, not
// place-scoped: a mesh spans places. A maps client joins each edge's a/b pubkeys
// to node coordinates from the mesh_node layer (or GET /api/v1/events?layer=
// network) to draw the links; `lastSeen`/`daysActive` drive recency fade and a
// reliability weight so an intermittent mesh reads honestly.
func (s *Service) serveMeshLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := parseMeshWindow(r.URL.Query().Get("window"))
	now := s.Now()
	links, err := s.Store.MeshLinks(ctx, now.Add(-window))
	if err != nil {
		internal(ctx, w, err)
		return
	}
	resp := meshLinksResponse{
		Window:      windowLabel(window),
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Links:       make([]meshLinkJSON, 0, len(links)),
	}
	for _, l := range links {
		resp.Links = append(resp.Links, meshLinkJSON{
			A:            l.A,
			B:            l.B,
			Observations: l.Observations,
			DaysActive:   l.DaysActive,
			FirstSeen:    l.FirstSeen.Format(time.RFC3339),
			LastSeen:     l.LastSeen.Format(time.RFC3339),
			BestSnr:      l.BestSNR,
		})
	}
	body, err := json.Marshal(resp)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	writeJSON(w, r, body, "application/json", maxAgeConditions)
}

// windowLabel renders the effective window as a clean "<n>h" for the common
// whole-hour case (presets/defaults all are), falling back to the Go duration
// string for anything sub-hour.
func windowLabel(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}

// serveMeshLinkLayer handles GET /api/v1/places/{place}/map/mesh_link.geojson —
// the relay topology scoped to a place. Unlike the global /api/v1/mesh/links, it
// is a self-contained subgraph: the nodes located INSIDE the place plus the 1-hop
// neighbours they link to (even outside the place, so a link to the wider mesh
// isn't amputated at the boundary), and the edges among them. Node Points carry
// network.inRegion (true = inside the place, false = pulled-in neighbour); edges
// are LineStrings with the mesh_link block. Reuses MeshLinks() + the recency
// window (?window=, default 72h).
func (s *Service) serveMeshLinkLayer(w http.ResponseWriter, r *http.Request, place *gridv1.Place) {
	ctx := r.Context()
	window := parseMeshWindow(r.URL.Query().Get("window"))

	// Every located mesh node (global) → its source event, keyed by pubkey; then
	// the subset attached to this place.
	allNodes, err := s.queryNetworkEvents(ctx, "")
	if err != nil {
		internal(ctx, w, err)
		return
	}
	byPub := make(map[string]*gridv1.Event, len(allNodes))
	for _, ev := range allNodes {
		pk := strings.ToLower(ev.GetMesh().GetPublicKey())
		if pk != "" && ev.GetGeometry().GetCentroid() != nil {
			byPub[pk] = ev
		}
	}
	regionNodes, err := s.queryNetworkEvents(ctx, place.GetId())
	if err != nil {
		internal(ctx, w, err)
		return
	}
	inRegion := make(map[string]bool, len(regionNodes))
	for _, ev := range regionNodes {
		if pk := strings.ToLower(ev.GetMesh().GetPublicKey()); pk != "" {
			inRegion[pk] = true
		}
	}

	links, err := s.Store.MeshLinks(ctx, s.Now().Add(-window))
	if err != nil {
		internal(ctx, w, err)
		return
	}

	// Keep edges touching the region; the node set is the located in-region nodes
	// plus every neighbour those kept edges reach.
	nodeSet := make(map[string]bool, len(inRegion))
	for pk := range inRegion {
		if _, ok := byPub[pk]; ok {
			nodeSet[pk] = true
		}
	}
	var features []hazards.Feature
	for _, l := range links {
		a, b := strings.ToLower(l.A), strings.ToLower(l.B)
		if !inRegion[a] && !inRegion[b] {
			continue // edge doesn't touch this place
		}
		na, aok := byPub[a]
		nb, bok := byPub[b]
		if !aok || !bok {
			continue // an endpoint we can't place — no drawable line
		}
		nodeSet[a] = true
		nodeSet[b] = true
		features = append(features, meshLinkFeature(l, na, nb))
	}
	// Node Points (region ∪ neighbours), region-flagged.
	for pk := range nodeSet {
		f := projectNetwork(byPub[pk])
		reg := inRegion[pk]
		if f.Properties.Mesh != nil {
			f.Properties.Mesh.InRegion = &reg
		}
		features = append(features, f)
	}

	sources, err := s.Store.ListSources(ctx)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	status, lastUpdate := LayerSourceStatus(sources, hazards.LayerMeshLink)
	// Same fail-loud degrade as the store-backed layers: a down source with data
	// still on hand is STALE, never a fabricated OK-empty.
	status, lastUpdate = hazards.DegradeStoreStatus(status, len(features) > 0, lastUpdate)
	s.writeFeatureCollection(w, r, features, &hazards.Metadata{
		Layer:            hazards.LayerMeshLink,
		Area:             place.GetSlug(),
		GeneratedAt:      s.Now().UTC().Format(time.RFC3339),
		SourceStatus:     status,
		LastSourceUpdate: timeOrEmpty(lastUpdate),
		SchemaVersion:    mapSchemaVersion,
	})
}

// queryNetworkEvents returns the ACTIVE/SCHEDULED NETWORK events, optionally
// scoped to a place (empty placeID = global). Drains keyset pagination.
func (s *Service) queryNetworkEvents(ctx context.Context, placeID string) ([]*gridv1.Event, error) {
	q := store.EventQuery{
		PlaceID:  placeID,
		Layers:   []gridv1.Layer{gridv1.Layer_MESH},
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED},
		PageSize: 200,
	}
	var out []*gridv1.Event
	for {
		page, next, err := s.Store.QueryEvents(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			break
		}
		q.PageToken = next
	}
	return out, nil
}

// meshLinkFeature builds a LineString edge between two located nodes.
func meshLinkFeature(l store.MeshLink, na, nb *gridv1.Event) hazards.Feature {
	ca := na.GetGeometry().GetCentroid()
	cb := nb.GetGeometry().GetCentroid()
	p := hazards.Properties{
		ID:           "mesh_link:" + l.A + ":" + l.B,
		Layer:        strings.ToUpper(hazards.LayerMeshLink),
		Kind:         "Relay link",
		Severity:     gridv1.Severity_INFO.String(),
		SeverityRank: int(gridv1.Severity_INFO.Number()),
		Headline:     meshLinkHeadline(na, nb),
		UpdatedAt:    timeOrEmpty(l.LastSeen),
		Source:       hazards.Source{ID: "meshcore", Name: "MeshCore Mesh", Attribution: "MeshCore community mesh"},
		MeshLink: &hazards.MeshLinkProps{
			A: l.A, B: l.B, Observations: l.Observations, DaysActive: l.DaysActive,
			FirstSeen: timeOrEmpty(l.FirstSeen), LastSeen: timeOrEmpty(l.LastSeen), BestSnr: l.BestSNR,
		},
	}
	geom := hazards.LineStringGeom([]hazards.LatLng{
		{Lat: ca.GetLat(), Lng: ca.GetLng()},
		{Lat: cb.GetLat(), Lng: cb.GetLng()},
	})
	return hazards.Feature{Type: "Feature", Geometry: geom, Properties: p}
}

// meshLinkHeadline names a link by its endpoints' node names (short pubkey fallback).
func meshLinkHeadline(na, nb *gridv1.Event) string {
	return meshNodeLabel(na) + " ↔ " + meshNodeLabel(nb)
}

func meshNodeLabel(ev *gridv1.Event) string {
	if name := ev.GetMesh().GetName(); name != "" {
		return name
	}
	pk := ev.GetMesh().GetPublicKey()
	if len(pk) > 8 {
		return pk[:8]
	}
	return pk
}

// parseMeshWindow parses the ?window= duration, defaulting and clamping.
func parseMeshWindow(s string) time.Duration {
	if s == "" {
		return defaultMeshWindow
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultMeshWindow
	}
	if d > maxMeshWindow {
		return maxMeshWindow
	}
	return d
}
