package gridapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// serveSources handles GET /v1/sources: the source registry with per-source
// health (the honesty mechanism /v1 clients key layer trust off).
func (s *Service) serveSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.Store.ListSources(r.Context())
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	writeMessage(w, r, &gridv1.SourceList{Sources: sources}, maxAgeEntities)
}

// scannerOut mirrors the shipped /api/v1/scanners/{area} element shape
// exactly (internal/hazards scannerOut) — no raw embed HTML; clients build
// the official Broadcastify iframe from feed_id.
type scannerOut struct {
	FeedID          string `json:"feed_id"`
	ChannelLabel    string `json:"channel_label"`
	Agency          string `json:"agency,omitempty"`
	BroadcastifyURL string `json:"broadcastify_url"`
}

// scannersOut wraps the list: {"scanners":[...]}.
type scannersOut struct {
	Scanners []scannerOut `json:"scanners"`
}

// serveScanners handles GET /v1/scanners?place=. Scanner feeds are operator
// config attached to hazard areas, so: ?place= naming an area serves that
// area's feeds; a non-area place (town, county, ...) or no param serves every
// configured area's feeds deduped by feed_id. An unknown place is a 404.
func (s *Service) serveScanners(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	feeds := s.allScannerFeeds()
	if p := r.URL.Query().Get("place"); p != "" {
		place, err := s.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown place: %q", p))
			return
		}
		if err != nil {
			internal(ctx, w, err)
			return
		}
		if place.GetKind() == gridv1.PlaceKind_AREA {
			// Area places are seeded from config areas with slug == area id; a
			// place row with no surviving config area (stale DB) falls back to
			// the all-areas set rather than silently serving nothing.
			if area, ok := s.areaByID(place.GetSlug()); ok {
				feeds = area.ScannerFeeds
			}
		}
	}

	out := scannersOut{Scanners: make([]scannerOut, 0, len(feeds))}
	for _, f := range feeds {
		out.Scanners = append(out.Scanners, scannerOut{
			FeedID:          f.FeedID,
			ChannelLabel:    f.ChannelLabel,
			Agency:          f.Agency,
			BroadcastifyURL: "https://www.broadcastify.com/listen/feed/" + f.FeedID,
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	writeJSON(w, r, body, contentTypeJSON, maxAgeScanners)
}

// allScannerFeeds is every configured area's feeds deduped by feed_id,
// preserving area order then feed order (deterministic for ETags).
func (s *Service) allScannerFeeds() []config.ScannerFeed {
	seen := make(map[string]bool)
	var out []config.ScannerFeed
	for _, a := range s.Cfg.Hazards.Areas {
		for _, f := range a.ScannerFeeds {
			if seen[f.FeedID] {
				continue
			}
			seen[f.FeedID] = true
			out = append(out, f)
		}
	}
	return out
}

// areaByID finds a configured hazard area by id (== the seeded area place's
// slug).
func (s *Service) areaByID(id string) (config.HazardArea, bool) {
	for _, a := range s.Cfg.Hazards.Areas {
		if a.ID == id {
			return a, true
		}
	}
	return config.HazardArea{}, false
}
