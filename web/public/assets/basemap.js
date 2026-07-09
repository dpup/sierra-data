// basemap.js — the shared MapLibre base style.
//
// A DARK raster basemap (CARTO "dark_all", built on OpenStreetMap data) so the
// map reads as an actual map that also matches the dark dev-console: the resolve
// tester needs a recognizable backdrop to click against, and the layer previews
// need muted geographic context under the brightly-colored hazard geometry.
// Tiles are map furniture (images), not API data — the site principle that DATA
// is only ever fetched from same-origin /api/v1/* through api.js still holds; every
// /api/v1 call goes through api.js and nothing else talks to the API.
//
// Attribution (OSM + CARTO) is required by the tile usage policies and is
// rendered by MapLibre's default AttributionControl from the source's
// `attribution`.
//
// Pure data, no DOM or network access at import time — node can import this.

/**
 * MapLibre style object: one dark raster source + layer. Pages add their own
 * geojson sources/layers (place geometry, hazard footprints, markers) on top.
 * Raster-only base needs no glyphs/sprites (no symbol layers here). Four CARTO
 * subdomains (a–d) spread tile requests per their usage guidance.
 */
export const BASE_STYLE = {
  version: 8,
  name: 'grid-dark-raster',
  sources: {
    basemap: {
      type: 'raster',
      tiles: [
        'https://a.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png',
        'https://b.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png',
        'https://c.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png',
        'https://d.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png',
      ],
      tileSize: 256,
      maxzoom: 20,
      attribution: '© OpenStreetMap contributors © CARTO',
    },
  },
  layers: [
    {
      id: 'basemap',
      type: 'raster',
      source: 'basemap',
    },
  ],
};
