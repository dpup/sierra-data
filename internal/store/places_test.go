package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// seedPlaceDirectory installs a nested directory: an area covering
// everything, a county inside it, a town point, and a far-away county.
func seedPlaceDirectory(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"area:mother-lode", "mother-lode", "Mother Lode",
		gridv1.PlaceKind_AREA, polyGeometry(37.5, -121.5, 39.0, -119.5))))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:calaveras", "calaveras", "Calaveras County",
		gridv1.PlaceKind_COUNTY, polyGeometry(38.0, -120.9, 38.5, -120.0))))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"town:arnold", "arnold", "Arnold",
		gridv1.PlaceKind_TOWN, pointGeometry(38.2554, -120.3521))))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"county:faraway", "faraway", "Faraway County",
		gridv1.PlaceKind_COUNTY, polyGeometry(40.0, -122.0, 40.5, -121.0))))
}

func TestPlacesContaining(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedPlaceDirectory(t, s)

	// At the town, inside the county and the area. Ordering is
	// most-specific-first: TOWN, then COUNTY, then AREA.
	got, err := s.PlacesContaining(ctx, 38.2554, -120.3521)
	require.NoError(t, err)
	assert.Equal(t, []string{"town:arnold", "county:calaveras", "area:mother-lode"}, placeIDs(got))

	// Inside the county but well away from any town.
	got, err = s.PlacesContaining(ctx, 38.1, -120.8)
	require.NoError(t, err)
	assert.Equal(t, []string{"county:calaveras", "area:mother-lode"}, placeIDs(got))

	// Inside the area but outside every county.
	got, err = s.PlacesContaining(ctx, 38.7, -120.3)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "area:mother-lode", got[0].GetId())

	// Ocean: nothing.
	got, err = s.PlacesContaining(ctx, 36.0, -125.0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func placeIDs(ps []*gridv1.Place) []string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.GetId()
	}
	return ids
}

// Towns are seeded as points, which no point-in-polygon test can match, so
// resolve never returned one. A town now contains a location within the town
// radius of its point; where two towns are that close, both, nearest first.
func TestPlacesContainingTownRadius(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // DefaultTownRadiusMeters (3 km)
	// Sonora and Columbia, ~5.9 km apart: the closest pair in prefab.yaml.
	sonora := [2]float64{37.9841, -120.3827}
	columbia := [2]float64{38.0349, -120.4016}
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"town:sonora", "sonora", "Sonora", gridv1.PlaceKind_TOWN, pointGeometry(sonora[0], sonora[1]))))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"town:columbia", "columbia", "Columbia", gridv1.PlaceKind_TOWN, pointGeometry(columbia[0], columbia[1]))))
	// along returns the point a fraction f of the way from Sonora to Columbia.
	along := func(f float64) (float64, float64) {
		return sonora[0] + f*(columbia[0]-sonora[0]), sonora[1] + f*(columbia[1]-sonora[1])
	}
	const mPerDegLat = 111320.0
	nearColumbiaLat, nearColumbiaLng := along(0.6) // ~3.5 km from Sonora, ~2.4 km from Columbia
	overlapLat, overlapLng := along(0.495)         // ~2.92 km from Sonora, ~2.98 km from Columbia

	cases := []struct {
		name     string
		lat, lng float64
		want     []string
	}{
		{"at the point", sonora[0], sonora[1], []string{"town:sonora"}},
		{"2 km out", sonora[0] - 2000/mPerDegLat, sonora[1], []string{"town:sonora"}},
		{"4 km out", sonora[0] - 4000/mPerDegLat, sonora[1], nil},
		{"nearer Columbia, outside Sonora", nearColumbiaLat, nearColumbiaLng, []string{"town:columbia"}},
		{"inside both: nearest first", overlapLat, overlapLng, []string{"town:sonora", "town:columbia"}},
	}

	for _, tc := range cases {
		got, err := s.PlacesContaining(ctx, tc.lat, tc.lng)
		require.NoError(t, err, tc.name)
		if tc.want == nil {
			assert.Empty(t, got, tc.name)
			continue
		}
		assert.Equal(t, tc.want, placeIDs(got), tc.name)
	}
}

func TestWithTownRadius(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithTownRadius(500))
	require.NoError(t, s.UpsertPlace(ctx, testPlace(
		"town:arnold", "arnold", "Arnold", gridv1.PlaceKind_TOWN, pointGeometry(38.2554, -120.3521))))

	got, err := s.PlacesContaining(ctx, 38.2554+400/111320.0, -120.3521)
	require.NoError(t, err)
	assert.Equal(t, []string{"town:arnold"}, placeIDs(got), "400 m is inside a 500 m radius")
	got, err = s.PlacesContaining(ctx, 38.2554+1000/111320.0, -120.3521)
	require.NoError(t, err)
	assert.Empty(t, got, "1 km is outside it, though inside the 3 km default")
}

func TestGetPlaceBySlugOrID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedPlaceDirectory(t, s)

	// Values containing ':' are ids; anything else is a slug.
	byID, err := s.GetPlace(ctx, "county:calaveras")
	require.NoError(t, err)
	assert.Equal(t, "Calaveras County", byID.GetName())

	bySlug, err := s.GetPlace(ctx, "calaveras")
	require.NoError(t, err)
	assert.Equal(t, "county:calaveras", bySlug.GetId())

	_, err = s.GetPlace(ctx, "nowhere")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.GetPlace(ctx, "county:nowhere")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListPlaces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedPlaceDirectory(t, s)

	all, err := s.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
	require.NoError(t, err)
	assert.Len(t, all, 4)

	counties, err := s.ListPlaces(ctx, gridv1.PlaceKind_COUNTY, "")
	require.NoError(t, err)
	require.Len(t, counties, 2)
	assert.Equal(t, "Calaveras County", counties[0].GetName())

	// q is a case-insensitive substring match on name.
	matched, err := s.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "cAlAvErAs")
	require.NoError(t, err)
	require.Len(t, matched, 1)
	assert.Equal(t, "county:calaveras", matched[0].GetId())

	matched, err = s.ListPlaces(ctx, gridv1.PlaceKind_TOWN, "calaveras")
	require.NoError(t, err)
	assert.Empty(t, matched, "kind and q filters combine")
}

func TestUpsertPlaceReplaces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := testPlace("town:arnold", "arnold", "Arnold", gridv1.PlaceKind_TOWN, pointGeometry(38.25, -120.35))
	require.NoError(t, s.UpsertPlace(ctx, p))

	p.Name = "Arnold, CA"
	p.ParentId = "county:calaveras"
	require.NoError(t, s.UpsertPlace(ctx, p))

	got, err := s.GetPlace(ctx, "town:arnold")
	require.NoError(t, err)
	assert.Equal(t, "Arnold, CA", got.GetName())
	assert.Equal(t, "county:calaveras", got.GetParentId())

	all, err := s.ListPlaces(ctx, gridv1.PlaceKind_TOWN, "")
	require.NoError(t, err)
	assert.Len(t, all, 1, "upsert replaces, never duplicates")
}
