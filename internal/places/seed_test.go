package places

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// testConfig mirrors the prefab.yaml shapes the seeder consumes: one area,
// three towns (one outside every county polygon), one corridor.
func testConfig() *config.Config {
	return &config.Config{
		Hazards: config.HazardsConfig{
			Areas: []config.HazardArea{{
				ID:   "calaveras",
				Name: "Calaveras County",
				Bounds: config.GeoBounds{
					MinLatitude: 37.9, MaxLatitude: 38.6,
					MinLongitude: -121.0, MaxLongitude: -119.9,
				},
			}},
		},
		Weather: config.WeatherConfig{
			Locations: []config.WeatherLocation{
				{ID: "murphys", Name: "Murphys, CA", Coordinates: config.Coordinates{Latitude: 38.1377, Longitude: -120.4561}},
				{ID: "sonora", Name: "Sonora, CA", Coordinates: config.Coordinates{Latitude: 37.9841, Longitude: -120.3822}},
				// Pacific Ocean: inside no county, parent stays empty.
				{ID: "offshore", Name: "Offshore Buoy", Coordinates: config.Coordinates{Latitude: 36.0, Longitude: -125.0}},
			},
		},
		Roads: config.RoadsConfig{
			MonitoredRoads: []config.MonitoredRoad{{
				ID:          "hwy4-angels-murphys",
				Name:        "Highway 4",
				Section:     "Angels Camp to Murphys",
				Origin:      config.Coordinates{Latitude: 38.0678, Longitude: -120.5402},
				Destination: config.Coordinates{Latitude: 38.1377, Longitude: -120.4561},
			}},
		},
	}
}

func TestSeed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := testConfig()

	require.NoError(t, Seed(ctx, s, cfg))

	all, err := s.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
	require.NoError(t, err)
	// 1 area + 8 embedded counties + 3 towns + 1 corridor.
	assert.Len(t, all, 13)

	t.Run("idempotent", func(t *testing.T) {
		require.NoError(t, Seed(ctx, s, cfg))
		again, err := s.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
		require.NoError(t, err)
		assert.Len(t, again, len(all))
	})

	t.Run("area by slug", func(t *testing.T) {
		p, err := s.GetPlace(ctx, "calaveras")
		require.NoError(t, err)
		assert.Equal(t, "area:calaveras", p.GetId())
		assert.Equal(t, gridv1.PlaceKind_AREA, p.GetKind())
		assert.Equal(t, "Calaveras County", p.GetName())
		assert.Empty(t, p.GetParentId())
		g, err := geojson.Parse(p.GetGeometry().GetGeojson())
		require.NoError(t, err)
		assert.Equal(t, "Polygon", g.Type)
		assert.InDelta(t, 37.9, p.GetGeometry().GetBbox().GetMinLat(), 1e-9)
		assert.InDelta(t, -119.9, p.GetGeometry().GetBbox().GetMaxLng(), 1e-9)
	})

	t.Run("counties embedded", func(t *testing.T) {
		counties, err := s.ListPlaces(ctx, gridv1.PlaceKind_COUNTY, "")
		require.NoError(t, err)
		require.Len(t, counties, 8)
		p, err := s.GetPlace(ctx, "el-dorado-county")
		require.NoError(t, err)
		assert.Equal(t, "county:el-dorado-county", p.GetId())
		assert.Equal(t, "El Dorado County", p.GetName())
		require.NotNil(t, p.GetGeometry().GetBbox())
		assert.NotEmpty(t, p.GetGeometry().GetGeojson())
	})

	t.Run("town parents resolved", func(t *testing.T) {
		murphys, err := s.GetPlace(ctx, "town:murphys")
		require.NoError(t, err)
		assert.Equal(t, "county:calaveras-county", murphys.GetParentId())

		sonora, err := s.GetPlace(ctx, "sonora")
		require.NoError(t, err)
		assert.Equal(t, "town:sonora", sonora.GetId())
		assert.Equal(t, "county:tuolumne-county", sonora.GetParentId())

		offshore, err := s.GetPlace(ctx, "offshore")
		require.NoError(t, err)
		assert.Empty(t, offshore.GetParentId())
	})

	t.Run("corridor", func(t *testing.T) {
		p, err := s.GetPlace(ctx, "hwy4-angels-murphys")
		require.NoError(t, err)
		assert.Equal(t, "corridor:hwy4-angels-murphys", p.GetId())
		assert.Equal(t, gridv1.PlaceKind_CORRIDOR, p.GetKind())
		assert.Equal(t, "Highway 4 — Angels Camp to Murphys", p.GetName())
		g, err := geojson.Parse(p.GetGeometry().GetGeojson())
		require.NoError(t, err)
		assert.Equal(t, "LineString", g.Type)
		require.Len(t, g.Points, 2)
	})
}

func TestSeedSlugCollision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := testConfig()
	// Town id colliding with the area slug: seeding must abort before writes.
	cfg.Weather.Locations = append(cfg.Weather.Locations, config.WeatherLocation{
		ID: "calaveras", Name: "Collides", Coordinates: config.Coordinates{Latitude: 38.1, Longitude: -120.5},
	})

	err := Seed(ctx, s, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slug collisions")
	assert.Contains(t, err.Error(), "calaveras (area:calaveras, town:calaveras)")

	all, err := s.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
	require.NoError(t, err)
	assert.Empty(t, all)
}

// TestLoadAreaPolygons_EbbettsPassCoverage validates the shipped coverage wedge:
// the corridor towns fall inside, and the corners the old square over-reached
// (San Andreas/Jackson NW, Farmington SW) fall outside.
func TestLoadAreaPolygons_EbbettsPassCoverage(t *testing.T) {
	polys, err := loadAreaPolygons()
	require.NoError(t, err)
	raw, ok := polys["ebbetts-pass"]
	require.True(t, ok, "ebbetts-pass polygon must be present in areas.geojson")
	g, err := geojson.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "Polygon", g.Type)

	inside := map[string][2]float64{ // lat, lng
		"Arnold":      {38.265, -120.334},
		"Bear Valley": {38.461, -120.042},
		"Sonora":      {37.984, -120.383},
		"Pinecrest":   {38.193, -119.983},
	}
	for name, ll := range inside {
		assert.Truef(t, geojson.PointInGeometry(ll[0], ll[1], g), "%s should be inside the wedge", name)
	}
	outside := map[string][2]float64{
		"San Andreas": {38.196, -120.681},
		"Jackson":     {38.349, -120.774},
		"Farmington":  {37.930, -120.900},
	}
	for name, ll := range outside {
		assert.Falsef(t, geojson.PointInGeometry(ll[0], ll[1], g), "%s should be outside the wedge", name)
	}
}

// TestSeed_AreaPrefersPolygon: when a checked-in polygon exists for an area id,
// the seeded geometry is the polygon (tight bbox), not the config bbox rectangle.
func TestSeed_AreaPrefersPolygon(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := testConfig()
	cfg.Hazards.Areas[0].ID = "ebbetts-pass"
	cfg.Hazards.Areas[0].Name = "Ebbetts Pass Corridor"
	// A deliberately huge config bbox that the polygon must override.
	cfg.Hazards.Areas[0].Bounds = config.GeoBounds{
		MinLatitude: 30, MaxLatitude: 45, MinLongitude: -125, MaxLongitude: -115,
	}
	require.NoError(t, Seed(ctx, s, cfg))

	p, err := s.GetPlace(ctx, "ebbetts-pass")
	require.NoError(t, err)
	g, err := geojson.Parse(p.GetGeometry().GetGeojson())
	require.NoError(t, err)
	require.Equal(t, "Polygon", g.Type)
	bb := p.GetGeometry().GetBbox()
	assert.InDelta(t, 37.90, bb.GetMinLat(), 0.001)
	assert.InDelta(t, 38.53, bb.GetMaxLat(), 0.001)
	assert.InDelta(t, -120.60, bb.GetMinLng(), 0.001)
	assert.InDelta(t, -119.90, bb.GetMaxLng(), 0.001)
}
