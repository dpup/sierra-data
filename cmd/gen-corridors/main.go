// Command gen-corridors regenerates data/places/corridors.geojson: the actual
// road path (the decoded Google Routes polyline) for each configured monitored
// road, so corridor places hug the real road instead of a straight
// origin->destination chord — which lets point events (road incidents) attach to
// a corridor by proximity accurately even on winding sections.
//
// Run once, with a Google Routes API key, whenever the monitored-road set
// changes; the output is committed and embedded via internal/places. A road that
// fails to route falls back to the straight chord so the file always covers every
// configured corridor.
//
//	PF__GOOGLE_ROUTES__API_KEY=... go run ./cmd/gen-corridors
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geo"
)

const outPath = "data/places/corridors.geojson"

type feature struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Geometry   any            `json:"geometry"`
}

type featureCollection struct {
	Type     string    `json:"type"`
	Features []feature `json:"features"`
}

func main() {
	ctx := context.Background()
	cfg := config.LoadConfig()
	if cfg.GoogleRoutes.APIKey == "" {
		log.Fatal("gen-corridors: PF__GOOGLE_ROUTES__API_KEY is required")
	}
	client := google.NewClient(cfg.GoogleRoutes.APIKey)
	gu := geo.NewGeoUtils()

	fc := featureCollection{Type: "FeatureCollection"}
	for _, mr := range cfg.Roads.MonitoredRoads {
		coords, err := routePath(ctx, client, gu, mr)
		if err != nil {
			log.Printf("WARN %s: %v — falling back to the straight chord", mr.ID, err)
			coords = [][2]float64{
				{mr.Origin.Longitude, mr.Origin.Latitude},
				{mr.Destination.Longitude, mr.Destination.Latitude},
			}
		}
		fc.Features = append(fc.Features, feature{
			Type:       "Feature",
			Properties: map[string]any{"id": mr.ID, "name": mr.Name + " — " + mr.Section},
			Geometry:   map[string]any{"type": "LineString", "coordinates": coords},
		})
		log.Printf("OK %s: %d points", mr.ID, len(coords))
	}

	out, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		log.Fatalf("gen-corridors: marshal: %v", err)
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		log.Fatalf("gen-corridors: write %s: %v", outPath, err)
	}
	fmt.Printf("wrote %s (%d corridors)\n", outPath, len(fc.Features))
}

// routePath computes the road route and returns its path as GeoJSON [lng, lat]
// coordinate pairs.
func routePath(ctx context.Context, client *google.Client, gu geo.GeoUtils, mr config.MonitoredRoad) ([][2]float64, error) {
	rd, err := client.ComputeRoutes(ctx,
		&api.Coordinates{Latitude: mr.Origin.Latitude, Longitude: mr.Origin.Longitude},
		&api.Coordinates{Latitude: mr.Destination.Latitude, Longitude: mr.Destination.Longitude})
	if err != nil {
		return nil, err
	}
	if rd.Polyline == "" {
		return nil, fmt.Errorf("no polyline in route response")
	}
	pts, err := gu.DecodePolyline(rd.Polyline)
	if err != nil {
		return nil, fmt.Errorf("decode polyline: %w", err)
	}
	if len(pts) < 2 {
		return nil, fmt.Errorf("polyline has only %d point(s)", len(pts))
	}
	coords := make([][2]float64, len(pts))
	for i, p := range pts {
		coords[i] = [2]float64{p.Longitude, p.Latitude} // GeoJSON is [lng, lat]
	}
	return coords, nil
}
