// Package site embeds the data.sierragridteam.org static site
// (docs/data-sites-spec.md) so the server binary is self-contained: no
// runtime file dependencies, and the Docker image needs no separate site
// COPY. The `all:` prefix includes every file under assets/ and lib/
// (including subdirectories such as assets/pages/); the HTML pages are
// listed explicitly so a missing page fails the build rather than 404ing
// in production.
package site

import "embed"

//go:embed all:assets all:lib index.html sources.html events.html event.html places.html map.html history.html docs.html
var FS embed.FS
