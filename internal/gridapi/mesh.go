package gridapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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
