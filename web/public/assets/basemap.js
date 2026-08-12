// basemap.js — the shared MapLibre base style.
//
// A LIGHT vector basemap: OpenFreeMap's "Positron", built on OpenStreetMap via
// OpenMapTiles. The site's surface is cream paper, and a dark map cut a black
// rectangle into every page that carried one — the map read as a hole rather
// than as part of the document. Positron is the light, desaturated counterpart
// to the CARTO dark style this replaces, so geographic context stays muted and
// the hazard geometry on top stays the loudest thing on the map.
//
// CONSEQUENCE, AND IT IS THE EASY THING TO MISS: geometry drawn over this now
// takes the PAPER severity ramp, not the on-ink one. Every map on the site was
// painting with `SEVERITY_COLORS_ON_INK` because the backdrop was black; those
// values are tuned to be legible against dark and go pale and washed against
// light. See the note in format.js — this is the ramp mistake the design system
// warns about, and swapping the basemap is exactly when it happens.
//
// Tiles are map furniture (images and vector tiles), not API data — the site
// principle that DATA is only ever fetched from same-origin /api/v1/* through
// api.js still holds; every /api/v1 call goes through api.js and nothing else
// talks to the API.
//
// ATTRIBUTION COMES FROM THE TILEJSON — DO NOT ADD YOUR OWN.
//
// The style document declares no `attribution` on either source, which looks
// like it is missing. It is not: the `openmaptiles` source is a *reference* to
// https://tiles.openfreemap.org/planet, and that TileJSON carries
// "OpenFreeMap © OpenMapTiles Data from OpenStreetMap" with the OSM copyright
// page linked. MapLibre merges it into the AttributionControl once tiles load.
//
// A `customAttribution` was added here first, on the evidence of the style
// document alone — and because the screenshot harness blocks external hosts,
// the TileJSON never loaded there and the duplicate never showed up in testing.
// In a real browser it rendered both, twice as long as it needed to be.
//
// Pure data, no DOM or network access at import time — node can import this.

/**
 * The hosted style document. MapLibre accepts a URL here and fetches the style,
 * its glyphs and its sprites; unlike the previous raster source there is no
 * inline layer list to keep in step.
 *
 * No API key and no signup — OpenFreeMap serves this publicly. It is also the
 * only third-party runtime request the site makes, and it is deliberately a
 * *tile* request: nothing about the API's data passes through it.
 */
export const BASE_STYLE = 'https://tiles.openfreemap.org/styles/positron';

/**
 * Backdrop shown while the style loads, and behind it if it never does.
 *
 * Paper, not ink: a map frame that flashes black before resolving to a light
 * map is a worse first paint than one that starts the colour it will end up,
 * and offline (the screenshot harness blocks external hosts) this is the colour
 * the frame keeps.
 */
export const BASE_BACKDROP = '#f0f0ee';

/**
 * Attribution control options shared by every map.
 *
 * `compact` collapses the credit to an ⓘ that expands on click. The expanded
 * string names three parties and is long enough to run most of the width of a
 * map pane; permanently parked over the geography it is the loudest thing on a
 * light basemap, and it is required to be *present*, not *large*.
 *
 * No `customAttribution` — see the note at the top of this file. The TileJSON
 * already provides it, and adding our own printed it twice.
 */
export const BASE_ATTRIBUTION_OPTS = { compact: true };

/**
 * A background-only style, used when the hosted one cannot be fetched.
 *
 * THIS EXISTS BECAUSE THE HAZARD GEOMETRY IS THE DATA AND THE BASEMAP IS NOT.
 * With an inline style (the previous raster setup) the style always "loaded",
 * so `map.on('load')` fired and the geometry drew even when tile images failed.
 * A hosted style is fetched: if it 404s, times out, or is blocked, `load` never
 * fires and every layer the page meant to add is silently never added — the map
 * comes up empty. Losing the incident footprints because a tile CDN is down is
 * exactly backwards for this service, and an empty map that looks deliberate is
 * the "all clear" reading §5 of the contract forbids.
 *
 * So: if the real style has not loaded shortly, swap to this. The map loses its
 * geography and keeps its data.
 */
export const FALLBACK_STYLE = {
  version: 8,
  name: 'grid-fallback',
  sources: {},
  layers: [{ id: 'bg', type: 'background', paint: { 'background-color': BASE_BACKDROP } }],
};

/**
 * Guarantee that SOME style loads, so the caller's style.load handler runs and
 * the geometry gets drawn.
 *
 * @param {Object} map a MapLibre Map
 * @param {number=} timeoutMs how long to wait for the hosted style
 */
export function ensureBasemap(map, timeoutMs = 5000) {
  let swapped = false;
  const swap = () => {
    if (swapped || map.isStyleLoaded()) return;
    swapped = true;
    try {
      map.setStyle(FALLBACK_STYLE);
    } catch {
      /* nothing further to try; the pane keeps its backdrop */
    }
  };
  // A failed style fetch surfaces as an `error` with no sourceId attached.
  map.on('error', (e) => {
    if (!map.isStyleLoaded() && !(e && e.sourceId)) swap();
  });
  setTimeout(swap, timeoutMs);
}

/**
 * Make a map inert until the reader chooses to use it.
 *
 * A map is a big rectangle in the middle of a scrolling document, and MapLibre
 * binds the wheel by default — so scrolling the page over one zooms the map
 * instead and the reader is stranded. (The design spec called for scroll-zoom to
 * be off; it never was, on any of the four maps.)
 *
 * The fix is not to cripple the map permanently but to make interaction
 * deliberate: gestures are disabled until the reader clicks or tabs into the
 * pane, and disabled again when they leave it or press Escape. A CLICK still
 * does its normal job on the way in — opening a feature popup, resolving a
 * point on Places — so nothing needs two clicks; it is the *gestures* (wheel,
 * drag, arrow keys) that wait to be invited.
 *
 * Not `cooperativeGestures`: that demands ctrl/cmd on every scroll forever,
 * which is a tax on the reader who does want to use the map.
 *
 * @param {Object} map a MapLibre Map
 * @param {HTMLElement} container the element holding it
 */
export function deferInteraction(map, container) {
  if (!map || !container) return;
  const gestures = [
    'scrollZoom', 'dragPan', 'dragRotate',
    'touchZoomRotate', 'touchPitch', 'keyboard', 'doubleClickZoom', 'boxZoom',
  ];
  const set = (on) => {
    for (const g of gestures) {
      const handler = map[g];
      if (handler && typeof handler[on ? 'enable' : 'disable'] === 'function') {
        handler[on ? 'enable' : 'disable']();
      }
    }
  };

  const activate = () => {
    if (!container.classList.contains('map-inert')) return;
    container.classList.remove('map-inert');
    set(true);
  };
  const deactivate = () => {
    if (container.classList.contains('map-inert')) return;
    container.classList.add('map-inert');
    set(false);
  };

  set(false);
  container.classList.add('map-inert');
  // Tabbable, so the pane can be reached and released without a mouse.
  if (!container.hasAttribute('tabindex')) container.tabIndex = 0;

  container.addEventListener('pointerdown', activate);
  container.addEventListener('focusin', activate);
  container.addEventListener('mouseleave', deactivate);
  container.addEventListener('focusout', (e) => {
    if (!container.contains(e.relatedTarget)) deactivate();
  });
  container.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      deactivate();
      if (typeof container.blur === 'function') container.blur();
    }
  });
}
