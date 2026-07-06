# Vendored libraries

## MapLibre GL JS v4.7.1

- Files: `maplibre-gl.js` (UMD build, ~803 KB), `maplibre-gl.css` (~65 KB)
- Source: `https://unpkg.com/maplibre-gl@4.7.1/dist/maplibre-gl.js` and
  `https://unpkg.com/maplibre-gl@4.7.1/dist/maplibre-gl.css`
- License: BSD 3-Clause —
  https://github.com/maplibre/maplibre-gl-js/blob/v4.7.1/LICENSE.txt

Vendored locally because the site loads no third-party assets at runtime
(unprivileged-client principle: the only network the site touches is the
same-origin `/v1/*` API). To upgrade, fetch the matching `dist/` files for the
new tag from unpkg (or the GitHub release) and update this note.
