package gridapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/clients/census"
)

// placeList decodes a PlaceList body.
type placeList struct {
	Places []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"places"`
}

func (pl placeList) ids() []string {
	var out []string
	for _, p := range pl.Places {
		out = append(out, p.ID)
	}
	return out
}

func TestPlacesList(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/places")
	require.Equal(t, http.StatusOK, rec.Code)
	var out placeList
	decode(t, rec, &out)
	// 2 areas + 8 embedded counties + 2 towns + 2 corridors.
	assert.Len(t, out.Places, 14)

	t.Run("kind lowercase", func(t *testing.T) {
		rec := get(t, s, "/v1/places?kind=town")
		require.Equal(t, http.StatusOK, rec.Code)
		var out placeList
		decode(t, rec, &out)
		assert.ElementsMatch(t, []string{"town:arnold", "town:murphys"}, out.ids())
	})
	t.Run("kind enum name", func(t *testing.T) {
		rec := get(t, s, "/v1/places?kind=CORRIDOR")
		require.Equal(t, http.StatusOK, rec.Code)
		var out placeList
		decode(t, rec, &out)
		assert.ElementsMatch(t, []string{"corridor:hwy-4", "corridor:hwy-108"}, out.ids())
	})
	t.Run("kind bogus 400", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/places?kind=galaxy"), http.StatusBadRequest, 3)
		requireStatus(t, get(t, s, "/v1/places?kind=PLACE_KIND_UNSPECIFIED"), http.StatusBadRequest, 3)
	})
	t.Run("q substring", func(t *testing.T) {
		rec := get(t, s, "/v1/places?q=murph")
		require.Equal(t, http.StatusOK, rec.Code)
		var out placeList
		decode(t, rec, &out)
		assert.Equal(t, []string{"town:murphys"}, out.ids())
	})
	t.Run("kind and q combined", func(t *testing.T) {
		rec := get(t, s, "/v1/places?kind=county&q=calaveras")
		require.Equal(t, http.StatusOK, rec.Code)
		var out placeList
		decode(t, rec, &out)
		assert.Equal(t, []string{"county:calaveras-county"}, out.ids())
	})
}

func TestPlaceGet(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/places/arnold")
	require.Equal(t, http.StatusOK, rec.Code)
	var bySlug struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Slug string `json:"slug"`
	}
	decode(t, rec, &bySlug)
	assert.Equal(t, "town:arnold", bySlug.ID)
	assert.Equal(t, "TOWN", bySlug.Kind, "enums render as proto names")

	rec = get(t, s, "/v1/places/"+url.PathEscape("town:arnold"))
	require.Equal(t, http.StatusOK, rec.Code)

	requireStatus(t, get(t, s, "/v1/places/atlantis"), http.StatusNotFound, 5)
}

// resolveBody decodes the hand-built resolve shape:
// {"query":{lat,lng,matched_address?},"places":[Place protojson...]}.
type resolveBody struct {
	Query  map[string]any    `json:"query"`
	Places []json.RawMessage `json:"places"`
}

func (rb resolveBody) placeIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, raw := range rb.Places {
		var p struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(raw, &p))
		out = append(out, p.ID)
	}
	return out
}

func TestResolveLatLng(t *testing.T) {
	s := newTestService(t)

	// Arnold: inside Calaveras County (embedded TIGER polygon) and both
	// configured area boxes. Town/corridor geometries are points/linestrings
	// and can never contain a point.
	rec := get(t, s, "/v1/places/resolve?lat=38.2552&lng=-120.3512")
	require.Equal(t, http.StatusOK, rec.Code)
	var out resolveBody
	decode(t, rec, &out)

	assert.Equal(t, []string{"county:calaveras-county", "area:calaveras", "area:high-country"},
		out.placeIDs(t), "most-specific-first: COUNTY before AREA, name-ordered within a kind")
	assert.Equal(t, 38.2552, out.Query["lat"])
	assert.Equal(t, -120.3512, out.Query["lng"])
	_, hasMatched := out.Query["matched_address"]
	assert.False(t, hasMatched, "matched_address omitted on the lat/lng path")

	t.Run("ocean point matches nothing", func(t *testing.T) {
		rec := get(t, s, "/v1/places/resolve?lat=0&lng=0")
		require.Equal(t, http.StatusOK, rec.Code)
		var out resolveBody
		decode(t, rec, &out)
		assert.NotNil(t, out.Places)
		assert.Empty(t, out.Places, `"places":[] — an empty match is a valid answer, not an error`)
	})
}

func TestResolveValidation(t *testing.T) {
	s := newTestService(t)
	for _, path := range []string{
		"/v1/places/resolve",                          // no params
		"/v1/places/resolve?lat=38.2",                 // lng missing
		"/v1/places/resolve?lng=-120.3",               // lat missing
		"/v1/places/resolve?lat=abc&lng=-120.3",       // non-numeric
		"/v1/places/resolve?lat=38.2&lng=xyz",         // non-numeric
		"/v1/places/resolve?lat=91&lng=-120.3",        // lat out of range
		"/v1/places/resolve?lat=-91&lng=-120.3",       // lat out of range
		"/v1/places/resolve?lat=38.2&lng=181",         // lng out of range
		"/v1/places/resolve?lat=38.2&lng=1&address=x", // both modes
		// ParseFloat accepts NaN/Inf spellings; these must be 400s, not the
		// 500 a NaN used to cause when marshalling the query echo.
		"/v1/places/resolve?lat=NaN&lng=-120.3",
		"/v1/places/resolve?lat=38.2&lng=NaN",
		"/v1/places/resolve?lat=nan&lng=nan",
		"/v1/places/resolve?lat=Inf&lng=-120.3",
		"/v1/places/resolve?lat=38.2&lng=-Inf",
	} {
		requireStatus(t, get(t, s, path), http.StatusBadRequest, 3)
	}
}

func TestResolveAddress(t *testing.T) {
	s := newTestService(t)
	s.Census = &fakeCensus{lat: 38.2552, lng: -120.3512, matched: "1234 HIGHWAY 4, ARNOLD, CA, 95223"}

	rec := get(t, s, "/v1/places/resolve?address="+url.QueryEscape("1234 Highway 4, Arnold CA"))
	require.Equal(t, http.StatusOK, rec.Code)
	var out resolveBody
	decode(t, rec, &out)
	assert.Equal(t, []string{"county:calaveras-county", "area:calaveras", "area:high-country"}, out.placeIDs(t))
	assert.Equal(t, "1234 HIGHWAY 4, ARNOLD, CA, 95223", out.Query["matched_address"])
	assert.Equal(t, 38.2552, out.Query["lat"], "resolved coordinates echoed for the client's map pin")

	t.Run("no match 404", func(t *testing.T) {
		s.Census = &fakeCensus{err: census.ErrNoMatch}
		rec := get(t, s, "/v1/places/resolve?address=nowhere")
		requireStatus(t, rec, http.StatusNotFound, 5)
	})
	t.Run("geocoder down 503", func(t *testing.T) {
		s.Census = &fakeCensus{err: errors.New("connection refused")}
		rec := get(t, s, "/v1/places/resolve?address=somewhere")
		sb := requireStatus(t, rec, http.StatusServiceUnavailable, 14)
		assert.NotContains(t, sb.Message, "connection refused", "upstream detail must not leak")
	})
}
