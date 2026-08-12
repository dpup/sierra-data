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

## Fonts (`fonts/`)

Three families, each with one job (see `web/CLAUDE.md`): **Archivo** for display,
**IBM Plex Sans** for prose, **IBM Plex Mono** for every value that came from the
API. All latin subsets, `font-display: swap`, self-hosted for the same reason as
MapLibre — no Google Fonts request at runtime.

| Family        | Weights           | Source                                                     | License |
| ------------- | ----------------- | ---------------------------------------------------------- | ------- |
| Archivo       | 600 700 800 900   | `unpkg.com/@fontsource/archivo@5.3.0/files/archivo-latin-{w}-normal.woff2` | SIL OFL 1.1 |
| IBM Plex Sans | 400 500 600       | @fontsource/ibm-plex-sans                                   | SIL OFL 1.1 |
| IBM Plex Mono | 400 500 600       | @fontsource/ibm-plex-mono                                   | SIL OFL 1.1 |

Archivo carries 800/900 only at display sizes; it has no 400, deliberately — body
text is Plex Sans. Preload in `Shell.astro` is limited to the four faces above the
fold (mono 400, sans 400, archivo 800, archivo 900); adding more costs more than
the FOUT it saves.
