// basemap.js — the shared MapLibre base style.
//
// A raster basemap (OpenStreetMap standard tiles) so the map reads as an actual
// map: the resolve tester needs a recognizable backdrop to click against, and
// the layer previews need geographic context under the hazard geometry. Tiles
// are map furniture (images), not API data — the site principle that DATA is
// only ever fetched from same-origin /v1/* through api.js still holds; every
// /v1 call goes through api.js and nothing else talks to the API.
//
// Attribution is required by the OSM tile usage policy and is rendered by
// MapLibre's default AttributionControl from the source's `attribution`.
//
// Pure data, no DOM or network access at import time — node can import this.

/**
 * MapLibre style object: one OSM raster source + layer. Pages add their own
 * geojson sources/layers (place geometry, hazard footprints, markers) on top.
 * Raster-only base needs no glyphs/sprites (no symbol layers here).
 */
export const BASE_STYLE = {
  version: 8,
  name: 'grid-osm-raster',
  sources: {
    osm: {
      type: 'raster',
      tiles: ['https://tile.openstreetmap.org/{z}/{x}/{y}.png'],
      tileSize: 256,
      maxzoom: 19,
      attribution: '© OpenStreetMap contributors',
    },
  },
  layers: [
    {
      id: 'osm-base',
      type: 'raster',
      source: 'osm',
    },
  ],
};
