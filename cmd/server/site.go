package main

import (
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/dpup/info.ersn.net/server/site"
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

// siteHandler serves the embedded site at "/". It only ever sees paths that
// no longer prefab mount claimed (/api/, /v1/, the swagger docs are separate
// longer-prefix mounts), so it doesn't need to route around them.
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

	// Map the URL path to an embedded file. path.Clean collapses any ".."
	// segments so traversal can't escape the embed root (embed.FS would
	// reject such paths anyway; this keeps the 404 path tidy).
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if name == "." || name == "" {
		name = "index.html"
	}

	body, err := site.FS.ReadFile(name)
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
