package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/site"
)

// requireSiteBuilt skips when site/dist holds only the committed .gitkeep
// placeholder instead of a real Astro build. site/dist is no longer a committed
// artifact — `make site` builds it locally and the Dockerfile's site-builder
// stage builds it for the image — so a plain `go test ./...` on a fresh clone
// (or on a machine with no Node) would otherwise fail on an absence that isn't
// a defect. `make test` depends on site-ensure, so on the normal path these
// tests do run against a real build.
func requireSiteBuilt(t *testing.T) {
	t.Helper()
	if _, err := fs.Stat(site.FS, "index.html"); err != nil {
		t.Skip("site/dist is not built (only the .gitkeep placeholder) — run `make site` or `make test`")
	}
}

// TestSiteEmbedContainsPages pins the embed manifest: every page the nav
// links to must be in the Astro build output (site/dist, embedded via the
// all:dist embed directive) — a missing page should fail here, not 404 in production.
func TestSiteEmbedContainsPages(t *testing.T) {
	requireSiteBuilt(t)
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
		"assets/chrome.js",
		"assets/pages/events.js",
		"lib/maplibre-gl.js",
		"lib/maplibre-gl.css",
	}
	for _, p := range pages {
		body, err := fs.ReadFile(site.FS, p)
		require.NoError(t, err, "embedded FS missing %s", p)
		assert.NotEmpty(t, body, "embedded %s is empty", p)
	}
}

func TestSiteHandler(t *testing.T) {
	requireSiteBuilt(t)
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

	t.Run("extensionless page serves the .html file, no-cache", func(t *testing.T) {
		rec := get("/sources")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		assert.NotEmpty(t, rec.Body.Bytes())
	})

	t.Run(".html redirects to the extensionless URL", func(t *testing.T) {
		rec := get("/sources.html")
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "/sources", rec.Header().Get("Location"))
	})

	t.Run("index.html redirects to /", func(t *testing.T) {
		rec := get("/index.html")
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "/", rec.Header().Get("Location"))
	})

	t.Run(".html redirect preserves the query string", func(t *testing.T) {
		rec := get("/event.html?id=usgs:abc123")
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "/event?id=usgs:abc123", rec.Header().Get("Location"))
	})

	t.Run("extensionless page keeps its query for the client", func(t *testing.T) {
		rec := get("/event?id=usgs:abc123")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	})

	// assets/* revalidate rather than caching: the island and the markup it
	// binds to by element id ship as one unit, and these filenames carry no
	// content hash to tell versions apart.
	t.Run("asset css content-type and revalidates", func(t *testing.T) {
		rec := get("/assets/app.css")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/css; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	})

	t.Run("asset js content-type", func(t *testing.T) {
		rec := get("/assets/api.js")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/javascript; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	})

	// The ETag is what makes revalidation cheap — a matching If-None-Match
	// must cost a bodiless 304, not the file again.
	t.Run("etag revalidation returns 304", func(t *testing.T) {
		first := get("/assets/api.js")
		tag := first.Header().Get("ETag")
		assert.NotEmpty(t, tag, "assets must be ETagged")

		req := httptest.NewRequest(http.MethodGet, "/assets/api.js", nil)
		req.Header.Set("If-None-Match", tag)
		rec := httptest.NewRecorder()
		siteHandler(rec, req)
		assert.Equal(t, http.StatusNotModified, rec.Code)
		assert.Empty(t, rec.Body.String(), "304 carries no body")
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

// TestSiteCacheControl pins the cache policy per file class. The _astro/ case is
// the one that matters most and is easiest to get wrong: Astro names those files
// by content hash, so the bytes at a given URL can never change and they must be
// immutable — while everything else must stay revalidating, because an HTML page
// or a hand-authored asset served stale for a year is unrecoverable.
func TestSiteCacheControl(t *testing.T) {
	cases := []struct{ name, ext, want string }{
		{"index.html", ".html", "no-cache"},
		{"_astro/docs.oe4bHGyF.css", ".css", "public, max-age=31536000, immutable"},
		// assets/* MUST revalidate: the island and the markup it binds to by
		// element id are one unit, and these names carry no content hash. A
		// max-age here let a browser pair new HTML with old JS for the length
		// of the window, which threw "markup is missing #ev-scope-place".
		{"assets/app.css", ".css", "no-cache"},
		{"assets/pages/home.js", ".js", "no-cache"},
		{"lib/maplibre-gl.js", ".js", "public, max-age=86400"},
		{"lib/fonts/archivo-900.woff2", ".woff2", "public, max-age=86400"},
		{"robots.txt", ".txt", "no-cache"},
	}
	for _, c := range cases {
		if got := siteCacheControl(c.name, c.ext); got != c.want {
			t.Errorf("siteCacheControl(%q, %q) = %q, want %q", c.name, c.ext, got, c.want)
		}
	}
}
