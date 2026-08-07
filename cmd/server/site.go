package main

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/dpup/sierra-data/site"
)

// siteContentTypes pins Content-Type for every extension the site ships.
// Checked BEFORE mime.TypeByExtension: the OS mime database
// (/etc/mime.types) varies by host — minimal containers lack it entirely and
// desktop distros remap .js — and the types the site depends on must not.
var siteContentTypes = map[string]string{
	".html":    "text/html; charset=utf-8",
	".js":      "text/javascript; charset=utf-8",
	".css":     "text/css; charset=utf-8",
	".geojson": "application/geo+json",
	".json":    "application/json",
	".md":      "text/markdown; charset=utf-8",
	".svg":     "image/svg+xml",
	".png":     "image/png",
	".ico":     "image/x-icon",
}

// siteHandler serves the embedded site at "/". It only ever sees paths that no
// more-specific prefab mount claimed (/api/ gateway, /mcp, /api/openapi.json),
// so it doesn't need to route around them.
//
// Caching: HTML is no-cache (pages are the deploy unit — a new deploy must
// show up on reload); /assets/* revalidates every 5 minutes (app code
// iterates); /lib/* is vendored third-party code that only changes with a
// deliberate upgrade, so it caches for a day.
func siteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Clean URLs: pages are addressed without the .html suffix. Any explicit
	// ".html" request 301s to the extensionless form (/events.html -> /events,
	// /index.html -> /) so there is one canonical URL, and old bookmarks and
	// any stray links still resolve. Query strings are preserved.
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

	// Map the URL path to an embedded file. path.Clean collapses any ".."
	// segments so traversal can't escape the embed root (embed.FS would
	// reject such paths anyway; this keeps the 404 path tidy).
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if name == "." || name == "" {
		name = "index.html"
	}

	body, err := fs.ReadFile(site.FS, name)
	if err != nil && path.Ext(name) == "" {
		// Extensionless page URL: /events -> events.html. Only files (not the
		// asset/lib paths, which carry their own extensions) hit this fallback.
		if b, e := fs.ReadFile(site.FS, name+".html"); e == nil {
			name, body, err = name+".html", b, nil
		}
	}
	if err != nil {
		// Includes directory paths (ReadFile on a dir errors) — the site has
		// no directory listings.
		siteNotFound(w)
		return
	}

	ext := path.Ext(name)
	ctype := siteContentTypes[ext]
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", siteCacheControl(name, ext))

	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// siteCacheControl picks the Cache-Control policy for an embedded file by
// location: HTML never caches, first-party assets revalidate quickly,
// vendored libs cache long.
func siteCacheControl(name, ext string) string {
	switch {
	case ext == ".html":
		return "no-cache"
	case strings.HasPrefix(name, "_astro/"):
		// Astro's own bundled output, named by CONTENT HASH
		// (_astro/docs.oe4bHGyF.css). The filename changes whenever the bytes
		// do, so the file at a given URL can never change — the one case where
		// immutable is exactly right rather than merely convenient.
		return "public, max-age=31536000, immutable"
	case strings.HasPrefix(name, "assets/"):
		return "public, max-age=300"
	case strings.HasPrefix(name, "lib/"):
		return "public, max-age=86400"
	default:
		return "no-cache"
	}
}

// siteNotFound writes a minimal HTML 404 consistent with the site's plain
// aesthetic (the JSON-error convention belongs to /v1, not the site).
func siteNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("<!doctype html><title>404</title><h1>404 Not Found</h1><p><a href=\"/\">data.sierragridteam.org</a></p>\n"))
}
