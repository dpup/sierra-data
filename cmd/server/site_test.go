package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/info.ersn.net/server/site"
)

// TestSiteEmbedContainsPages pins the embed manifest: every page the nav
// links to must be in the binary — a missing page should fail here (and at
// build time via the explicit go:embed list), not 404 in production.
func TestSiteEmbedContainsPages(t *testing.T) {
	pages := []string{
		"index.html",
		"sources.html",
		"events.html",
		"event.html",
		"places.html",
		"map.html",
		"history.html",
		"docs.html",
		// Shared assets and the vendored map lib must embed too.
		"assets/app.css",
		"assets/api.js",
		"assets/pages/events.js",
		"lib/maplibre-gl.js",
		"lib/maplibre-gl.css",
	}
	for _, p := range pages {
		body, err := site.FS.ReadFile(p)
		require.NoError(t, err, "embedded FS missing %s", p)
		assert.NotEmpty(t, body, "embedded %s is empty", p)
	}
}

func TestSiteHandler(t *testing.T) {
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		siteHandler(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	t.Run("root serves index.html no-cache", func(t *testing.T) {
		rec := get("/")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		assert.NotEmpty(t, rec.Body.Bytes())
	})

	t.Run("html page no-cache", func(t *testing.T) {
		rec := get("/sources.html")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	})

	t.Run("asset css content-type and 5m cache", func(t *testing.T) {
		rec := get("/assets/app.css")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/css; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	})

	t.Run("asset js content-type", func(t *testing.T) {
		rec := get("/assets/api.js")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	})

	t.Run("vendored lib caches a day", func(t *testing.T) {
		rec := get("/lib/maplibre-gl.js")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "public, max-age=86400", rec.Header().Get("Cache-Control"))
	})

	t.Run("missing file is a minimal html 404", func(t *testing.T) {
		rec := get("/nope")
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Contains(t, rec.Body.String(), "404")
	})

	t.Run("directory path 404s (no listings)", func(t *testing.T) {
		rec := get("/assets")
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("traversal cannot escape the embed root", func(t *testing.T) {
		rec := get("/../go.mod")
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("HEAD returns headers without body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		siteHandler(rec, httptest.NewRequest(http.MethodHead, "/", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Empty(t, rec.Body.Bytes())
	})

	t.Run("POST is 405", func(t *testing.T) {
		rec := httptest.NewRecorder()
		siteHandler(rec, httptest.NewRequest(http.MethodPost, "/", nil))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}
