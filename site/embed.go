// Package site embeds the data.sierragridteam.org static site
// (docs/data-sites-spec.md) so the server binary is self-contained: no runtime
// file dependencies, and the Docker image needs no separate site COPY.
//
// The site is built by Astro (source in ../web) into dist/, which is NOT
// committed: the Docker image builds it in its own site-builder stage, and
// locally `make site` (implied by make server/run/test) builds it on demand.
// Only dist/.gitkeep is committed, so that this package still compiles on a
// fresh clone — `//go:embed all:dist` is a compile error if the directory is
// missing, but an empty-but-present dist is fine (the server then 404s the
// site, and cmd/server's site tests skip). `astro build` empties dist/ on every
// run, so the placeholder is kept alive by its twin at web/public/.gitkeep,
// which Astro copies back in.
//
// FS strips the dist/ prefix via fs.Sub, so callers read "index.html", not
// "dist/index.html".
package site

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS is the site root: the built dist/ tree with its prefix removed.
var FS = mustSub(embedded, "dist")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
