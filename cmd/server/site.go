package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	sierradata "github.com/sierragridteam/sierra-data"
)

// The landing page is plain files in cmd/server/landing, embedded as-is: no
// build step and no JavaScript beyond the optional copy button. The data
// explorer lives in the separate sierra-grid project at grid.sierragridteam.org.
//
//go:embed landing/index.html landing/style.css landing/llms.txt
var landingFS embed.FS

// sitePage is one response the "/" handler can serve, with its ETag computed
// once: every body is immutable for the life of the process.
type sitePage struct {
	ctype string
	body  []byte
	etag  string
}

func newSitePage(ctype string, body []byte) *sitePage {
	sum := sha256.Sum256(body)
	return &sitePage{ctype: ctype, body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
}

var (
	sitePagesOnce sync.Once
	sitePages     map[string]*sitePage
)

// loadSitePages builds the route table on first use. The changelog is
// rendered from the embedded CHANGELOG.md once per process.
func loadSitePages() map[string]*sitePage {
	sitePagesOnce.Do(func() {
		read := func(name string) []byte {
			b, err := landingFS.ReadFile("landing/" + name)
			if err != nil {
				panic(err) // embedded at compile time; cannot fail
			}
			return b
		}
		index := newSitePage("text/html; charset=utf-8", read("index.html"))
		sitePages = map[string]*sitePage{
			"":          index,
			"index":     index,
			"style.css": newSitePage("text/css; charset=utf-8", read("style.css")),
			"llms.txt":  newSitePage("text/plain; charset=utf-8", read("llms.txt")),
			"changelog": newSitePage("text/html; charset=utf-8", renderChangelog(sierradata.Changelog)),
		}
	})
	return sitePages
}

// renderChangelog turns CHANGELOG.md into a page using the landing stylesheet.
// goldmark's default renderer drops raw HTML, so the Markdown cannot inject
// markup; GFM adds the tables and autolinks the file uses.
func renderChangelog(src []byte) []byte {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	var buf bytes.Buffer
	buf.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Changelog · The Grid</title>
<meta name="color-scheme" content="dark light">
<link rel="stylesheet" href="/style.css">
</head>
<body>
<main class="md">
<p class="prompt">sierra@grid:~$ <span class="cmd">cat CHANGELOG.md</span> · <a href="/">data.sierragridteam.org</a></p>
`)
	if err := md.Convert(src, &buf); err != nil {
		// Unreachable for an in-memory source; fall back to the raw text.
		buf.Reset()
		buf.WriteString("<!doctype html><title>Changelog</title><pre>")
		buf.WriteString(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(string(src)))
		buf.WriteString("</pre>")
		return buf.Bytes()
	}
	buf.WriteString("</main>\n</body>\n</html>\n")
	return buf.Bytes()
}

// siteHandler serves the landing page at "/", plus /style.css, /llms.txt and
// /changelog. It only sees paths no more specific mount claimed (the /api/
// gateway, /api/openapi.json, /mcp, health), so it does not route around them.
// Everything is no-cache with a strong ETag, so a deploy shows on reload and a
// revalidation costs a bodiless 304.
func siteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Clean URLs: an explicit ".html" 301s to the extensionless form
	// (/changelog.html -> /changelog, /index.html -> /), preserving the query,
	// so old links into the former site still land somewhere canonical.
	if p := r.URL.Path; strings.HasSuffix(p, ".html") {
		clean := strings.TrimSuffix(p, ".html")
		if clean == "/index" {
			clean = "/"
		}
		if r.URL.RawQuery != "" {
			clean += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, clean, http.StatusMovedPermanently)
		return
	}

	// path.Clean collapses ".." and a trailing slash, so /changelog/ is
	// /changelog and traversal has nowhere to go.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	page, ok := loadSitePages()[name]
	if !ok {
		siteNotFound(w)
		return
	}

	w.Header().Set("Content-Type", page.ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", page.etag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, page.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(page.body)
}

// etagMatches reports whether an If-None-Match header selects `tag`.
// Handles the comma-separated list form and the "*" wildcard; weak comparison
// is fine here because no page has a semantically-equal variant.
func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}

// siteNotFound writes a minimal HTML 404 (the JSON error envelope belongs to
// /api). The former explorer pages (/events, /map, /relay, ...) land here, so
// it says where they went.
func siteNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>404 · The Grid</title><link rel="stylesheet" href="/style.css"></head>
<body><main>
<p class="prompt">sierra@grid:~$ <span class="cmd">cat this-page</span></p>
<h1>404 Not Found</h1>
<p>Nothing is served at this path. The data explorer is at
<a href="https://grid.sierragridteam.org">grid.sierragridteam.org</a>; the API is described at
<a href="/api/openapi.json">/api/openapi.json</a>.</p>
<p><a href="/">data.sierragridteam.org</a></p>
</main></body>
</html>
`))
}
