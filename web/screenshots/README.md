# Screenshot harness (mocked data)

Self-contained visual review of the whole site — **no server, no API keys, no
live upstreams**. It builds the static site, serves `site/dist` over a throwaway
HTTP server, intercepts every same-origin `/api/v1/*` fetch and answers it from
[`fixtures.mjs`](./fixtures.mjs), and screenshots each page at phone / tablet /
desktop widths. Because the data is fixed, runs are deterministic and safe to
diff by eye across a change.

This is the companion to `tools/screenshots` (`make site-shots`), which points a
browser at a **running** server with **real** data for layout-metric diffs. Reach
for this one when you want to see the rendered UI — especially mobile — without
standing up the Go server and its upstream feeds.

## Run it

```bash
make site-shots-mock                 # all pages, all viewports → web/screenshots/out/
make site-shots-mock ONLY=mobile     # one viewport
make site-shots-mock PAGES=events,map

# or directly (assumes the site is already built into ../../site/dist):
cd web
npm run screenshots          # astro build + capture
npm run screenshots:only     # capture only (skip the build)
node screenshots/capture.mjs --pages sources,roads --only mobile
```

Output: `web/screenshots/out/<viewport>/<page>.png` (git-ignored). Viewports are
`mobile` (390×844 @3x), `tablet` (768×1024 @2x), `desktop` (1440×900 @1x).

## How it works

- `capture.mjs` starts a tiny static server rooted at `site/dist`, so absolute
  `/assets/*` and `/lib/*` URLs resolve exactly as they do in production.
- A Playwright route handler answers any request whose path matches
  `/(api/)?v1/…` from `routeFor()` in `fixtures.mjs`; **external** requests
  (OpenStreetMap tiles for the MapLibre pages) are aborted so runs stay offline
  and deterministic — the map still draws its data layers over an empty basemap.
- Unmocked `/api/v1` paths are served a `404` and printed at the end, so a new
  endpoint that isn't in the fixtures is visible rather than silently blank.
- Chromium comes from the container's pre-installed Playwright browsers
  (`/ms-playwright`); no per-run download.

## Editing the scenario

The fixtures model one busy-but-plausible Ebbetts Pass day (active wildfire +
evacuation warning, Red Flag fire weather, Hwy 4 chain controls and a CHP
incident, a winter-weather alert, a quake, a small MeshCore relay net) so every
layout is exercised with real-shaped data. Shapes mirror what the page render
code in `public/assets/pages/*.js` consumes (protojson camelCase). To add a page
or an endpoint, extend `PAGES` in `capture.mjs` and `routeFor()` in
`fixtures.mjs`.
