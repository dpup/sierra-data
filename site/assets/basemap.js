// basemap.js — the shared local MapLibre base style.
//
// Site principle (non-negotiable): the only network this site touches is
// same-origin /v1/* fetches through api.js — no tile servers, no CDNs. The
// base map is therefore a plain local background with zero external
// requests; geographic context comes from the rendered geometry itself
// (place outlines, event footprints, bbox/centroid text).
//
// Pure data, no DOM or network access at import time — node can import this.

/**
 * Minimal MapLibre style object: one flat background layer, no sources.
 * Dark-panel tone matching app.css --bg-inset so the canvas reads as part
 * of the instrument panel in both themes.
 */
export const BASE_STYLE = {
  version: 8,
  name: 'grid-local-base',
  sources: {},
  layers: [
    {
      id: 'background',
      type: 'background',
      paint: { 'background-color': '#0e141b' },
    },
  ],
};
