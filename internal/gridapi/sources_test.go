package gridapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/info.ersn.net/server/internal/store"
)

func TestSources(t *testing.T) {
	s := newTestService(t) // registry seeded by seedEvents: calfire, chp, nws, usgs
	ctx := context.Background()
	require.NoError(t, s.Store.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, s.Store.RecordAttempt(ctx, "nws", errors.New("api.weather.gov: 503")))

	rec := get(t, s, "/v1/sources")
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Sources []map[string]json.RawMessage `json:"sources"`
	}
	decode(t, rec, &out)
	require.Len(t, out.Sources, 4)

	byID := map[string]map[string]json.RawMessage{}
	for _, src := range out.Sources {
		var id string
		require.NoError(t, json.Unmarshal(src["id"], &id))
		byID[id] = src
	}

	// Ordered by id.
	assert.JSONEq(t, `"calfire"`, string(out.Sources[0]["id"]))
	assert.JSONEq(t, `"usgs"`, string(out.Sources[3]["id"]))

	assert.JSONEq(t, `"UNAVAILABLE"`, string(byID["nws"]["status"]), "never-succeeded + error => UNAVAILABLE")
	assert.Contains(t, byID["nws"], "last_error")
	assert.Contains(t, byID["nws"], "last_attempt_at")

	assert.JSONEq(t, `"OK"`, string(byID["usgs"]["status"]))
	assert.Contains(t, byID["usgs"], "last_success_at")
	assert.JSONEq(t, `300`, string(byID["usgs"]["poll_interval_seconds"]))
}

func TestSourcesEmptyRegistry(t *testing.T) {
	// A store with nothing seeded: the endpoint still answers (protojson
	// omits the empty repeated field entirely).
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	s := NewService(st, &fakeRoads{}, &fakeWeather{}, &fakeCensus{}, testConfig(), nil)

	rec := get(t, s, "/v1/sources")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{}`, rec.Body.String())
}

// scannersBody is the hand-built scanners shape (matches the shipped
// /api/v1/scanners element exactly).
type scannersBody struct {
	Scanners []map[string]string `json:"scanners"`
}

func (sb scannersBody) feedIDs() []string {
	var out []string
	for _, s := range sb.Scanners {
		out = append(out, s["feed_id"])
	}
	return out
}

func TestScanners(t *testing.T) {
	s := newTestService(t)

	t.Run("area place serves its feeds", func(t *testing.T) {
		rec := get(t, s, "/v1/scanners?place=calaveras")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"))
		var out scannersBody
		decode(t, rec, &out)
		assert.Equal(t, []string{"13524", "28469"}, out.feedIDs())

		first := out.Scanners[0]
		assert.Equal(t, "Sheriff / CAL FIRE Dispatch", first["channel_label"])
		assert.Equal(t, "Calaveras SO", first["agency"])
		assert.Equal(t, "https://www.broadcastify.com/listen/feed/13524", first["broadcastify_url"])
	})

	t.Run("no place param dedupes all areas", func(t *testing.T) {
		rec := get(t, s, "/v1/scanners")
		require.Equal(t, http.StatusOK, rec.Code)
		var out scannersBody
		decode(t, rec, &out)
		assert.Equal(t, []string{"13524", "28469", "90001"}, out.feedIDs(),
			"28469 is shared between areas and appears once")
	})

	t.Run("non-area place gets the deduped union", func(t *testing.T) {
		rec := get(t, s, "/v1/scanners?place=arnold")
		require.Equal(t, http.StatusOK, rec.Code)
		var out scannersBody
		decode(t, rec, &out)
		assert.Equal(t, []string{"13524", "28469", "90001"}, out.feedIDs())
	})

	t.Run("agency omitted when unset", func(t *testing.T) {
		rec := get(t, s, "/v1/scanners?place=high-country")
		require.Equal(t, http.StatusOK, rec.Code)
		var out scannersBody
		decode(t, rec, &out)
		require.Equal(t, []string{"28469", "90001"}, out.feedIDs())
		_, hasAgency := out.Scanners[1]["agency"]
		assert.False(t, hasAgency, "empty agency key is omitted (shipped scannerOut shape)")
	})

	t.Run("unknown place 404", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/scanners?place=atlantis"), http.StatusNotFound, 5)
	})
}
