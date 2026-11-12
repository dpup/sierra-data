package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sierradata "github.com/sierragridteam/sierra-data"
)

func siteGet(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	siteHandler(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestSiteHandler(t *testing.T) {
	get := func(target string) *httptest.ResponseRecorder { return siteGet(t, http.MethodGet, target) }

	t.Run("root serves the landing page", func(t *testing.T) {
		rec := get("/")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		body := rec.Body.String()
		for _, want := range []string{
			"<h1>The Grid</h1>",
			"Principles</h2>",
			"Start here</h2>",
			"Give your agent this</h2>",
			`href="/api/openapi.json"`,
			`href="/llms.txt"`,
			`href="/changelog"`,
			`href="https://grid.sierragridteam.org"`,
			`id="agent-prompt"`,
		} {
			assert.Contains(t, body, want)
		}
	})

	t.Run("the page makes no external requests", func(t *testing.T) {
		body := get("/").Body.String()
		for _, attr := range []string{`src="http`, `href="//`, `src="//`, `@import`} {
			assert.NotContains(t, body, attr)
		}
		assert.NotContains(t, get("/style.css").Body.String(), "url(")
	})

	t.Run("the agent prompt stays short", func(t *testing.T) {
		body := get("/").Body.String()
		start := strings.Index(body, `id="agent-prompt"`)
		end := strings.Index(body[start:], "</pre>")
		require.True(t, start > 0 && end > 0)
		assert.LessOrEqual(t, strings.Count(body[start:start+end], "\n"), 40)
	})

	t.Run("index aliases serve the same page", func(t *testing.T) {
		root := get("/")
		for _, p := range []string{"/index", "/index/"} {
			rec := get(p)
			assert.Equal(t, http.StatusOK, rec.Code, p)
			assert.Equal(t, root.Body.String(), rec.Body.String(), p)
		}
	})

	t.Run("index.html redirects to /", func(t *testing.T) {
		rec := get("/index.html")
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "/", rec.Header().Get("Location"))
	})

	t.Run(".html redirects to the extensionless URL with its query", func(t *testing.T) {
		rec := get("/changelog.html?x=1")
		assert.Equal(t, http.StatusMovedPermanently, rec.Code)
		assert.Equal(t, "/changelog?x=1", rec.Header().Get("Location"))
	})

	t.Run("llms.txt is plain text", func(t *testing.T) {
		rec := get("/llms.txt")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
		body := rec.Body.String()
		assert.True(t, strings.HasPrefix(body, "# The Grid\n"), "llms.txt opens with its H1")
		for _, want := range []string{"\n> ", "\n## Docs\n", "\n## API\n", "\n## Optional\n", "/api/openapi.json"} {
			assert.Contains(t, body, want)
		}
	})

	t.Run("stylesheet", func(t *testing.T) {
		rec := get("/style.css")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/css; charset=utf-8", rec.Header().Get("Content-Type"))
	})

	t.Run("changelog renders CHANGELOG.md", func(t *testing.T) {
		for _, p := range []string{"/changelog", "/changelog/"} {
			rec := get(p)
			require.Equal(t, http.StatusOK, rec.Code, p)
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			body := rec.Body.String()
			assert.Contains(t, body, `<h1 id="changelog">Changelog</h1>`)
			assert.Contains(t, body, ">"+firstChangelogHeading(t)+"</h2>", "the newest dated section is a heading")
			assert.Contains(t, body, `<link rel="stylesheet" href="/style.css">`)
		}
	})

	t.Run("etag revalidation returns 304", func(t *testing.T) {
		tag := get("/").Header().Get("ETag")
		require.NotEmpty(t, tag)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", tag)
		rec := httptest.NewRecorder()
		siteHandler(rec, req)
		assert.Equal(t, http.StatusNotModified, rec.Code)
		assert.Empty(t, rec.Body.String())
	})

	t.Run("unknown and removed paths are a minimal html 404", func(t *testing.T) {
		for _, p := range []string{"/nope", "/events", "/map", "/relay", "/docs", "/mcp-guide", "/assets/app.css"} {
			rec := get(p)
			assert.Equal(t, http.StatusNotFound, rec.Code, p)
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"), p)
			assert.Contains(t, rec.Body.String(), "grid.sierragridteam.org", p)
		}
	})

	t.Run("traversal cannot escape", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get("/../go.mod").Code)
		assert.Equal(t, http.StatusNotFound, get("/landing/index.html/..").Code)
	})

	t.Run("HEAD returns headers without body", func(t *testing.T) {
		rec := siteGet(t, http.MethodHead, "/")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Empty(t, rec.Body.Bytes())
	})

	t.Run("POST is 405", func(t *testing.T) {
		assert.Equal(t, http.StatusMethodNotAllowed, siteGet(t, http.MethodPost, "/").Code)
	})
}

// firstChangelogHeading returns the text of CHANGELOG.md's first "## "
// section, e.g. "2026-11-12".
func firstChangelogHeading(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(string(sierradata.Changelog), "\n") {
		if strings.HasPrefix(line, "## ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "## "))
		}
	}
	t.Fatal("CHANGELOG.md has no ## section")
	return ""
}

func TestRenderChangelogDropsRawHTML(t *testing.T) {
	out := string(renderChangelog([]byte("# T\n\n<script>alert(1)</script>\n\nfires in <place>\n")))
	assert.NotContains(t, out, "<script>alert")
	assert.NotContains(t, out, "<place>")
}
