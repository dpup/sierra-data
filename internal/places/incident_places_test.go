package places

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// TestRoadIncidentPointsRespectCorridorPolygon is the acceptance test for the
// consumer-reported leak: region-wide CHP incidents must attach to
// area:ebbetts-pass only when their point is inside the corridor polygon — the
// same point-in-polygon test /v1/places/resolve (PlacesContaining) uses. Road
// incidents carry a point geometry and NO preset place_ids, so attachment is
// purely geometric via store.matchPlaces; this pins that it agrees with resolve.
func TestRoadIncidentPointsRespectCorridorPolygon(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	require.NoError(t, s.SeedSources(ctx, []store.SourceSeed{{ID: "chp", Name: "CHP"}}))

	cfg := testConfig()
	cfg.Hazards.Areas[0].ID = "ebbetts-pass"
	cfg.Hazards.Areas[0].Name = "Ebbetts Pass Corridor"
	require.NoError(t, Seed(ctx, s, cfg))

	// upsert a point event and return the place_ids the store attached. layer is
	// varied to cover both point-source, no-preset layers (road_incident and
	// earthquake share the same purely-geometric matchPlaces path).
	attachedPlaces := func(id string, layer gridv1.Layer, lat, lng float64) []string {
		ev := &gridv1.Event{
			Id:       id,
			Layer:    layer,
			Status:   gridv1.EventStatus_ACTIVE,
			Severity: gridv1.Severity_MINOR,
			Headline: id,
			Geometry: &gridv1.Geometry{
				Geojson:  geojson.PointGeoJSON(lat, lng),
				Bbox:     &gridv1.BoundingBox{MinLat: lat, MinLng: lng, MaxLat: lat, MaxLng: lng},
				Centroid: &gridv1.LatLng{Lat: lat, Lng: lng},
			},
			Provenance: &gridv1.Provenance{SourceId: "chp"},
		}
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
		got, err := s.GetEvent(ctx, id)
		require.NoError(t, err)
		return got.GetPlaceIds()
	}

	// Out-of-corridor point events (Amador County) must not receive
	// area:ebbetts-pass — resolve already places them in Amador only. Covers both
	// the reported CHP incidents and a USGS earthquake at the same longitude band.
	for _, c := range []struct {
		id       string
		layer    gridv1.Layer
		lat, lng float64
	}{
		{"chp:amador-124-49", gridv1.Layer_ROAD_INCIDENT, 38.454, -120.873},
		{"chp:amador-laverone", gridv1.Layer_ROAD_INCIDENT, 38.478, -120.847},
		{"usgs:amador-quake", gridv1.Layer_EARTHQUAKE, 38.460, -120.860},
	} {
		assert.NotContainsf(t, attachedPlaces(c.id, c.layer, c.lat, c.lng), "area:ebbetts-pass",
			"%s (%.3f,%.3f) is west of the corridor — must not tag ebbetts-pass", c.id, c.lat, c.lng)
	}

	// Genuinely in-corridor incidents (lng >= -120.6) must still attach.
	for _, c := range []struct {
		id       string
		lat, lng float64
	}{
		{"chp:arnold", 38.265, -120.334},
		{"chp:bearvalley", 38.461, -120.042},
	} {
		assert.Containsf(t, attachedPlaces(c.id, gridv1.Layer_ROAD_INCIDENT, c.lat, c.lng), "area:ebbetts-pass",
			"%s (%.3f,%.3f) is in-corridor — must tag ebbetts-pass", c.id, c.lat, c.lng)
	}
}
