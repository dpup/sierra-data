// Package site embeds the data.sierragridteam.org static site
// (docs/data-sites-spec.md) so the server binary is self-contained: no runtime
// file dependencies, and the Docker image needs no separate site COPY.
//
// The site is now built by Astro (source in ../web) into dist/, which is a
// committed build artifact — like the generated *.pb.go and *.swagger.json — so
// the Docker `go build` stays Node-free. Regenerate with `make site` after
// editing anything under web/. FS strips the dist/ prefix via fs.Sub, so callers
// read "index.html", not "dist/index.html".
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
