// Package places seeds the grid place directory (plan decision 11): configured
// hazard areas, embedded Census county polygons, weather-location towns, and
// monitored-road corridors. Seeding is idempotent and runs at boot.
package places

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	placesdata "github.com/dpup/sierra-data/data/places"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// county pairs a seeded county place with its parsed polygon so town parent
// resolution (point-in-polygon) doesn't re-parse per town.
type county struct {
	place *gridv1.Place
	geom  *geojson.Geom
}

// Seed upserts the place directory: areas (bbox polygons, slugs preserved so
// existing {area} URL segments carry over), counties from the embedded
// counties.geojson, towns (points, parented to their containing county), and
// corridors (origin->destination linestrings). Slugs must be globally unique;
// collisions abort the seed before any write.
func Seed(ctx context.Context, s *store.Store, cfg *config.Config) error {
	var places []*gridv1.Place

	for _, a := range cfg.Hazards.Areas {
		b := a.Bounds
		geom, err := makeGeometry(geojson.BboxPolygonGeoJSON(
			b.MinLatitude, b.MinLongitude, b.MaxLatitude, b.MaxLongitude))
		if err != nil {
			return fmt.Errorf("places: area %s bounds: %w", a.ID, err)
		}
		places = append(places, &gridv1.Place{
			Id:       "area:" + a.ID,
			Kind:     gridv1.PlaceKind_AREA,
			Name:     a.Name,
			Slug:     a.ID,
			Geometry: geom,
		})
	}

	counties, err := loadCounties()
	if err != nil {
		return err
	}
	for _, c := range counties {
		places = append(places, c.place)
	}

	for _, loc := range cfg.Weather.Locations {
		lat, lng := loc.Coordinates.Latitude, loc.Coordinates.Longitude
		geom, err := makeGeometry(geojson.PointGeoJSON(lat, lng))
		if err != nil {
			return fmt.Errorf("places: town %s point: %w", loc.ID, err)
		}
		places = append(places, &gridv1.Place{
			Id:       "town:" + loc.ID,
			Kind:     gridv1.PlaceKind_TOWN,
			Name:     loc.Name,
			Slug:     loc.ID,
			Geometry: geom,
			ParentId: containingCounty(counties, lat, lng),
		})
	}

	for _, mr := range cfg.Roads.MonitoredRoads {
		geom, err := makeGeometry(geojson.LineStringGeoJSON([][2]float64{
			{mr.Origin.Latitude, mr.Origin.Longitude},
			{mr.Destination.Latitude, mr.Destination.Longitude},
		}))
		if err != nil {
			return fmt.Errorf("places: corridor %s linestring: %w", mr.ID, err)
		}
		places = append(places, &gridv1.Place{
			Id:       "corridor:" + mr.ID,
			Kind:     gridv1.PlaceKind_CORRIDOR,
			Name:     mr.Name + " — " + mr.Section,
			Slug:     mr.ID,
			Geometry: geom,
		})
	}

	if err := checkSlugs(places); err != nil {
		return err
	}
	for _, p := range places {
		if err := s.UpsertPlace(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// loadCounties parses the embedded FeatureCollection into county places,
// geometry passed through as-is (already 5-decimal generalized polygons).
func loadCounties() ([]county, error) {
	var fc struct {
		Features []struct {
			Properties struct {
				Name string `json:"NAME"`
			} `json:"properties"`
			Geometry json.RawMessage `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(placesdata.CountiesGeoJSON, &fc); err != nil {
		return nil, fmt.Errorf("places: parse counties.geojson: %w", err)
	}
	if len(fc.Features) == 0 {
		return nil, fmt.Errorf("places: counties.geojson has no features")
	}
	out := make([]county, 0, len(fc.Features))
	for _, f := range fc.Features {
		if f.Properties.Name == "" {
			return nil, fmt.Errorf("places: counties.geojson feature missing NAME")
		}
		g, err := geojson.Parse(f.Geometry)
		if err != nil {
			return nil, fmt.Errorf("places: county %s geometry: %w", f.Properties.Name, err)
		}
		slug := slugify(f.Properties.Name)
		out = append(out, county{
			place: &gridv1.Place{
				Id:       "county:" + slug,
				Kind:     gridv1.PlaceKind_COUNTY,
				Name:     f.Properties.Name,
				Slug:     slug,
				Geometry: geometryFor(g, []byte(f.Geometry)),
			},
			geom: g,
		})
	}
	return out, nil
}

// containingCounty returns the id of the county whose polygon contains the
// point, or "" when none does (a town outside the county inventory).
func containingCounty(counties []county, lat, lng float64) string {
	for _, c := range counties {
		if geojson.PointInGeometry(lat, lng, c.geom) {
			return c.place.GetId()
		}
	}
	return ""
}

// makeGeometry wraps raw GeoJSON bytes with the derived bbox and centroid
// (the Geometry contract: bbox always populated).
func makeGeometry(raw []byte) (*gridv1.Geometry, error) {
	g, err := geojson.Parse(raw)
	if err != nil {
		return nil, err
	}
	return geometryFor(g, raw), nil
}

// geometryFor builds the proto Geometry from an already-parsed geometry.
func geometryFor(g *geojson.Geom, raw []byte) *gridv1.Geometry {
	minLat, minLng, maxLat, maxLng := g.Bbox()
	cLat, cLng := g.Centroid()
	return &gridv1.Geometry{
		Geojson:  raw,
		Bbox:     &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng},
		Centroid: &gridv1.LatLng{Lat: cLat, Lng: cLng},
	}
}

// checkSlugs enforces the global slug-uniqueness invariant across all kinds
// (slugs are the /v1 {place} URL keys) and reports every collision at once.
func checkSlugs(places []*gridv1.Place) error {
	bySlug := make(map[string][]string, len(places))
	for _, p := range places {
		bySlug[p.GetSlug()] = append(bySlug[p.GetSlug()], p.GetId())
	}
	var collisions []string
	for slug, ids := range bySlug {
		if len(ids) > 1 {
			sort.Strings(ids)
			collisions = append(collisions, fmt.Sprintf("%s (%s)", slug, strings.Join(ids, ", ")))
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	return fmt.Errorf("places: slug collisions: %s", strings.Join(collisions, "; "))
}

// slugify lowercases and joins alphanumeric runs with hyphens
// ("El Dorado County" -> "el-dorado-county").
func slugify(name string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(name) {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isAlnum {
			pendingSep = b.Len() > 0
			continue
		}
		if pendingSep {
			b.WriteByte('-')
			pendingSep = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
