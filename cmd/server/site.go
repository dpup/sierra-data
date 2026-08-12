package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/dpup/sierra-data/site"
)

// siteETags memoizes each embedded file's content hash. The embedded FS is
// immutable at runtime, so a file's ETag is computed once and reused.
var (
	siteETagMu sync.RWMutex
	siteETags  = map[string]string{}
)

// siteETag returns a strong ETag for an embedded file's bytes.
func siteETag(name string, body []byte) string {
	siteETagMu.RLock()
	tag, ok := siteETags[name]
	siteETagMu.RUnlock()
	if ok {
		return tag
	}
	sum := sha256.Sum256(body)
	tag = `"` + hex.EncodeToString(sum[:16]) + `"`
	siteETagMu.Lock()
	siteETags[name] = tag
	siteETagMu.Unlock()
	return tag
}

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

	// Every file is ETagged, which is what makes `no-cache` cheap: the browser
	// revalidates and gets a bodiless 304 rather than the file again.
	tag := siteETag(name, body)
	w.Header().Set("ETag", tag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// etagMatches reports whether an If-None-Match header selects `tag`.
// Handles the comma-separated list form and the "*" wildcard; weak comparison
// is fine here because the site never serves a semantically-equal variant.
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

// siteCacheControl picks the Cache-Control policy for an embedded file by
// location.
//
// `assets/` MUST REVALIDATE, and this is not a tuning choice. The page HTML and
// the island JavaScript it loads are one unit: the island binds to the markup by
// element id. Serving HTML `no-cache` while `assets/*` sat on `max-age=300`
// meant that for five minutes after a deploy a browser could hold NEW markup and
// OLD script — which threw
//
//	events.js: markup is missing #ev-scope-place, #ev-sort
//
// against a correctly-built, self-consistent deploy. The two files are
// versioned together or not at all. Unlike `_astro/*` these names carry no
// content hash (they are copied verbatim from `web/public/`), so the URL cannot
// distinguish versions and revalidation is the only thing that can.
//
// Revalidation is cheap because every response is ETagged: an unchanged file
// costs one conditional request and a bodiless 304.
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
		return "no-cache"
	case strings.HasPrefix(name, "lib/"):
		// Vendored third-party libraries. Not coupled to our markup — MapLibre
		// does not know our element ids — so a stale copy is merely old, not
		// broken, and these are the biggest files on the site.
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
