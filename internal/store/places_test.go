package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
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

	// Inside both the county and the area; point places can't contain.
	// Ordering is most-specific-first: COUNTY before AREA.
	got, err := s.PlacesContaining(ctx, 38.2554, -120.3521)
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i, p := range got {
		ids[i] = p.GetId()
	}
	assert.Equal(t, []string{"county:calaveras", "area:mother-lode"}, ids)

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
