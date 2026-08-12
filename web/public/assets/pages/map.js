// map.js — /map layer previews (spec §2 /map, T19).
//
// Renders per-layer GeoJSON from GET /api/v1/places/{place}/map/{layer}.geojson on
// a MapLibre map, with the honesty panel — each layer's `metadata` member
// (sourceStatus / generatedAt / attribution) displayed prominently, plus the
// exact .geojson URL a third-party map client would use.
//
// All view state (place, layers, map view) lives in the query string:
//   ?place=ebbetts-pass&layer=wildfire&layer=evacuation&view=38.20000,-120.55000,9.00
// (`layer` repeats, matching the /events and /history pages' URL state and
// the API's repeatable `layer` parameter.)
//
// Pure helpers (URL state, bbox math, feature splitting, color expression)
// are exported and have zero DOM/network access at import time, so node can
// import and test this module. All network I/O goes through ../api.js.
// MapLibre is loaded by map.html as a classic script (window.maplibregl) —
// this module never imports it, keeping node imports clean.

import { get, ApiError, PUBLIC_ORIGIN } from '../api.js';
import {
  timeAgo,
  timeAbs,
  timeCell,
  sevChip,
  sourceDot,
  layerLabel,
  SEVERITY_COLORS,
} from '../format.js';
import { BASE_STYLE, BASE_ATTRIBUTION_OPTS, ensureBasemap, deferInteraction } from '../basemap.js';
import { placeMenuOptions, placeMenuLabel } from '../place.js';
import { el, copyOnClick } from '../ui.js';
// Registers <grid-chip-row> and <grid-menu>; guarded so node imports of this
// module stay clean.
import '../components/chip-row.js';
import '../components/menu.js';

/* ------------------------------------------------------------------ */
/* Pure helpers (node-testable)                                       */
/* ------------------------------------------------------------------ */

/** The eight GeoJSON layers served under /map/{layer}.geojson, in canonical
 * display order (hazard-aggregation-design §4.4 + conditions projections). */
export const MAP_LAYERS = [
  'road_incident',
  'chain_control',
  'road_segment',
  'weather_alert',
  'fire_weather',
  'earthquake',
  'wildfire',
  'evacuation',
  // The relay topology as a self-contained subgraph (nodes in-region + their
  // 1-hop neighbours, plus the edges). Last so it never drives the auto-fit —
  // its neighbour nodes can sit well outside the place. The rich role-colored /
  // recency-faded view is /mesh; here it renders generically like every layer.
  'mesh_link',
];

/** Default map view when no layer has features and no ?view= is present:
 * Ebbetts Pass corridor, roughly centered. [lng, lat] per GeoJSON axis order. */
export const DEFAULT_CENTER = [-120.55, 38.2];
export const DEFAULT_ZOOM = 8.5;

/** Canonical public origin, for the copyable third-party-client URL. */
export { PUBLIC_ORIGIN } from '../api.js'; // re-exported: tests import it from here

/**
 * Path for a layer's FeatureCollection.
 * @param {string} place layer place slug, e.g. "ebbetts-pass"
 * @param {string} layer e.g. "wildfire"
 * @returns {string} "/api/v1/places/ebbetts-pass/map/wildfire.geojson"
 */
export function geojsonPath(place, layer) {
  return `/api/v1/places/${encodeURIComponent(place)}/map/${encodeURIComponent(
    layer
  )}.geojson`;
}

/**
 * Parse repeated ?layer= params (URLSearchParams.getAll('layer')). No params
 * → all layers (the page's default). "none" is the explicit empty selection
 * (an absent param means the default, so empty needs its own token).
 * Comma-separated values inside one param are tolerated for hand-typed URLs.
 * Unknown names are dropped; order is normalized to MAP_LAYERS order.
 * @param {string[]|string|null} values
 * @returns {string[]}
 */
export function parseLayers(values) {
  const list = Array.isArray(values) ? values : values ? [values] : [];
  const parts = list
    .flatMap((v) => String(v).split(','))
    .map((s) => s.trim())
    .filter(Boolean);
  if (parts.length === 0) return [...MAP_LAYERS];
  if (parts.includes('none')) return [];
  const wanted = new Set(parts);
  return MAP_LAYERS.filter((l) => wanted.has(l));
}

/**
 * Serialize a layer selection as repeated ?layer= values. Full selection →
 * [] (params omitted: default state keeps URLs canonical); empty → ['none'].
 * @param {string[]} selected
 * @returns {string[]}
 */
export function serializeLayers(selected) {
  const set = new Set(selected);
  const normalized = MAP_LAYERS.filter((l) => set.has(l));
  if (normalized.length === MAP_LAYERS.length) return [];
  if (normalized.length === 0) return ['none'];
  return normalized;
}

/**
 * Parse ?view=lat,lng,zoom. Returns null for missing/invalid input.
 * @param {string|null} param
 * @returns {{lat:number,lng:number,zoom:number}|null}
 */
export function parseView(param) {
  if (!param) return null;
  const parts = String(param).split(',').map(Number);
  if (parts.length !== 3 || parts.some((n) => !Number.isFinite(n))) return null;
  const [lat, lng, zoom] = parts;
  if (lat < -90 || lat > 90 || lng < -180 || lng > 180) return null;
  if (zoom < 0 || zoom > 24) return null;
  return { lat, lng, zoom };
}

/**
 * Serialize a map view for the URL (5 decimals ≈ 1.1 m).
 * @param {{lat:number,lng:number,zoom:number}} v
 * @returns {string}
 */
export function serializeView(v) {
  return `${v.lat.toFixed(5)},${v.lng.toFixed(5)},${v.zoom.toFixed(2)}`;
}

/**
 * Split a FeatureCollection into located features (renderable on the map)
 * and null-geometry features (banner alerts — rendered in the list below
 * the map, per hazard-aggregation-design §4.3).
 * @param {Object} fc FeatureCollection (may be malformed; handled defensively)
 * @returns {{located: Object[], unlocated: Object[]}}
 */
export function splitFeatures(fc) {
  const located = [];
  const unlocated = [];
  const feats = fc && Array.isArray(fc.features) ? fc.features : [];
  for (const f of feats) {
    if (!f || typeof f !== 'object') continue;
    if (f.geometry && typeof f.geometry === 'object' && f.geometry.type) {
      located.push(f);
    } else {
      unlocated.push(f);
    }
  }
  return { located, unlocated };
}

/**
 * Bounding box of an array of GeoJSON features.
 * @param {Object[]} features
 * @returns {[number,number,number,number]|null} [minLng, minLat, maxLng, maxLat]
 */
export function bboxOfFeatures(features) {
  let minLng = Infinity;
  let minLat = Infinity;
  let maxLng = -Infinity;
  let maxLat = -Infinity;
  let seen = false;

  function visitCoords(c) {
    if (!Array.isArray(c)) return;
    if (typeof c[0] === 'number' && typeof c[1] === 'number') {
      const [lng, lat] = c;
      if (!Number.isFinite(lng) || !Number.isFinite(lat)) return;
      seen = true;
      if (lng < minLng) minLng = lng;
      if (lat < minLat) minLat = lat;
      if (lng > maxLng) maxLng = lng;
      if (lat > maxLat) maxLat = lat;
      return;
    }
    for (const child of c) visitCoords(child);
  }

  function visitGeometry(g) {
    if (!g || typeof g !== 'object') return;
    if (g.type === 'GeometryCollection' && Array.isArray(g.geometries)) {
      for (const gg of g.geometries) visitGeometry(gg);
      return;
    }
    visitCoords(g.coordinates);
  }

  for (const f of features || []) {
    if (f && typeof f === 'object') visitGeometry(f.geometry);
  }
  return seen ? [minLng, minLat, maxLng, maxLat] : null;
}

/**
 * MapLibre paint expression coloring by properties.severity. Uses the PAPER
 * ramp: the basemap is Positron, a light style, and the on-ink values this used
 * to take are tuned for a black backdrop — over light they wash out. Unknown
 * severities fall through to INFO — never uncolored.
 * @returns {Array} ['match', ['get','severity'], 'EXTREME', '#ff5544', ...]
 */
export function severityColorExpression() {
  const expr = ['match', ['get', 'severity']];
  for (const [label, color] of Object.entries(SEVERITY_COLORS)) {
    expr.push(label, color);
  }
  expr.push(SEVERITY_COLORS.INFO); // default
  return expr;
}

/**
 * Recover the `source` object from a clicked feature's properties. MapLibre
 * stringifies nested GeoJSON properties, so it may arrive as a JSON string.
 * @param {Object} props feature properties
 * @returns {Object|null}
 */
export function featureSource(props) {
  const s = props ? props.source : null;
  if (!s) return null;
  if (typeof s === 'string') {
    try {
      const obj = JSON.parse(s);
      return obj && typeof obj === 'object' ? obj : null;
    } catch {
      return null;
    }
  }
  return typeof s === 'object' ? s : null;
}

/**
 * Parse a possibly-stringified nested property. MapLibre stringifies nested
 * GeoJSON property objects/arrays, so a per-kind block may arrive as JSON text.
 * @param {*} v
 * @returns {Object|null}
 */
function asObj(v) {
  if (v && typeof v === 'object') return v;
  if (typeof v === 'string') {
    try { const o = JSON.parse(v); return o && typeof o === 'object' ? o : null; } catch { return null; }
  }
  return null;
}

/**
 * Per-kind detail rows for the popup — the typed block for props.layer plus the
 * envelope status/description, so each layer shows its distinctive data (road
 * congestion/delay/status, chain level, quake magnitude, mesh telemetry, …) and
 * not just the shared envelope. Returns a <div class="popup-details"> or null.
 * Values via textContent (upstream text is untrusted).
 * @param {Object} props feature properties
 * @returns {HTMLElement|null}
 */
export function kindDetails(props) {
  const rows = [];
  const add = (k, v) => { if (v !== undefined && v !== null && v !== '') rows.push([k, String(v)]); };
  add('status', props.status);
  const layer = String(props.layer || '').toUpperCase();

  if (layer === 'ROAD_SEGMENT') {
    const r = asObj(props.road) || {};
    add('congestion', r.congestion);
    if (r.durationMinutes != null) add('travel', r.durationMinutes + ' min');
    if (r.delayMinutes != null) add('delay', (r.delayMinutes > 0 ? '+' : '') + r.delayMinutes + ' min');
    if (r.distanceKm != null) add('distance', r.distanceKm + ' km');
  } else if (layer === 'CHAIN_CONTROL') {
    const c = asObj(props.chainControl) || {};
    add('level', c.level); add('highway', c.highway); add('direction', c.direction);
  } else if (layer === 'ROAD_INCIDENT') {
    add('log #', (asObj(props.incident) || {}).logNumber);
  } else if (layer === 'WILDFIRE') {
    const w = asObj(props.wildfire) || {};
    if (w.acres) add('acres', w.acres);
    if (w.containment != null) add('containment', w.containment + '%');
    add('county', w.county); add('cause', w.cause);
  } else if (layer === 'EVACUATION') {
    const e = asObj(props.evacuation) || {};
    add('level', e.level); add('zone', e.zoneId); add('county', e.county); add('type', e.eventType);
  } else if (layer === 'EARTHQUAKE') {
    const q = asObj(props.earthquake) || {};
    if (q.magnitude != null) add('magnitude', 'M' + q.magnitude);
    if (q.depthKm != null) add('depth', q.depthKm + ' km');
    if (q.felt) add('felt reports', q.felt);
  } else if (layer === 'WEATHER_ALERT') {
    const w = asObj(props.weather) || {};
    add('event', w.event);
    if (Array.isArray(w.zones) && w.zones.length) add('zones', w.zones.join(', '));
  } else if (layer === 'FIRE_WEATHER') {
    const w = asObj(props.fireWeather) || {};
    add('state', w.state);
    if (Array.isArray(w.zones) && w.zones.length) add('zones', w.zones.join(', '));
  } else if (layer === 'MESH_NODE') {
    const n = asObj(props.mesh) || {};
    add('type', n.nodeType); add('node', n.name);
    if (n.publicKey) add('pubkey', String(n.publicKey).slice(0, 12) + '…');
    // On the mesh_link subgraph, nodes carry inRegion (true = inside the place,
    // false = a 1-hop neighbour pulled in because it links to one).
    if (n.inRegion === false) add('scope', 'neighbour (outside area)');
    else if (n.inRegion === true) add('scope', 'in area');
    if (n.snr != null) add('SNR', n.snr + ' dB');
    if (n.rssi != null) add('RSSI', n.rssi + ' dBm');
    if (n.hopCount != null) add('hops', n.hopCount);
    if (Array.isArray(n.gateways) && n.gateways.length) add('gateways', n.gateways.join(', '));
  } else if (layer === 'MESH_LINK') {
    const m = asObj(props.meshLink) || {};
    if (m.a) add('a', String(m.a).slice(0, 12) + '…');
    if (m.b) add('b', String(m.b).slice(0, 12) + '…');
    if (m.observations != null) add('observations', m.observations);
    if (m.daysActive != null) add('days active', m.daysActive);
    if (m.bestSnr != null && m.bestSnr !== 0) add('best SNR', m.bestSnr + ' dB');
    if (m.lastSeen) add('last seen', timeAgo(m.lastSeen));
  }

  if (!rows.length && !props.description) return null;
  const box = document.createElement('div');
  box.className = 'popup-details';
  if (rows.length) {
    const dl = document.createElement('dl');
    for (const [k, v] of rows) {
      const dt = document.createElement('dt'); dt.textContent = k;
      const dd = document.createElement('dd'); dd.textContent = v;
      dl.append(dt, dd);
    }
    box.append(dl);
  }
  if (props.description) {
    const d = document.createElement('div');
    d.className = 'popup-desc muted small';
    d.textContent = props.description;
    box.append(d);
  }
  return box;
}

/**
 * Upstream URLs are untrusted; only http(s) may become a link
 * (hazard-aggregation-design §4.1 rule).
 * @param {*} u
 * @returns {string|null}
 */
export function safeHttpUrl(u) {
  return typeof u === 'string' && /^https?:\/\//i.test(u) ? u : null;
}

/* ------------------------------------------------------------------ */
/* Page wiring (browser only — nothing below runs at import time)     */
/* ------------------------------------------------------------------ */

function init() {

  const els = {
    place: document.getElementById('place-select'),
    layerChecks: document.getElementById('layer-checks'),
    queryEcho: document.getElementById('map-query'),
    panel: document.getElementById('honesty-panel'),
    unlocated: document.getElementById('unlocated'),
    errors: document.getElementById('page-errors'),
    mapMount: document.getElementById('map-mount'),
    suppressed: document.getElementById('map-suppressed'),
  };

  const params = new URLSearchParams(location.search);
  const urlState = {
    place: params.get('place') || '',
    view: parseView(params.get('view')) ? params.get('view') : '',
  };
  let selected = parseLayers(params.getAll('layer'));
  const initialView = parseView(params.get('view'));

  const state = {
    loadToken: 0,
    /** layer -> { path, fc, located, unlocated, error } */
    results: new Map(),
    fitted: Boolean(initialView), // an explicit ?view= wins over auto-fit
    programmaticMove: false,
    boundLayerIds: new Set(),
    /** last decoded coverage outline, replayed on remount */
    areaBoundary: null,
    /** last camera position, so a remount does not reset the view */
    lastView: null,
  };

  /* ---- URL state ---- */

  function updateURL(changes) {
    Object.assign(urlState, changes || {});
    const p = new URLSearchParams();
    if (urlState.place) p.set('place', urlState.place);
    for (const l of serializeLayers(selected)) p.append('layer', l);
    if (urlState.view) p.set('view', urlState.view);
    const qs = p.toString();
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
  }

  /* ---- errors ---- */

  function pageError(context, err) {
    const block = document.createElement('div');
    block.className = 'error-block';
    const head = document.createElement('div');
    head.textContent = context;
    const urlLine = document.createElement('div');
    urlLine.className = 'error-url';
    urlLine.textContent =
      err instanceof ApiError ? `GET ${err.url} → ${err.status || 'network error'}` : '';
    const msg = document.createElement('div');
    msg.textContent = err && err.message ? err.message : String(err);
    block.append(head, urlLine, msg);
    els.errors.appendChild(block);
  }

  /* ---- map: mounted only when there is something true to draw ---- */

  // THE CONTRACT (API spec §5): an empty basemap reads as "all clear", and this
  // API never lets absence mean that. So the map element does not exist until a
  // selected layer has actually returned features, and it is REMOVED again when
  // none do — not hidden, not overlaid, not faded. `map` is therefore null much
  // of the time and every caller must guard.
  //
  // Layers already loaded while the map was absent are replayed by ensureMap()
  // from state.results, so mounting is idempotent and order-independent.
  /** @type {maplibregl.Map|null} */
  let map = null;
  let pendingResize = false;
  /** A draw was dropped because the style was not ready; flush on the next event. */
  let pendingDraw = false;
  /**
   * Has the style loaded at least once? NOT the same question as
   * `map.isStyleLoaded()`, which also reports whether every SOURCE has finished
   * loading — so adding the first layer's source made it return false, and
   * every later layer in the same pass early-returned. One layer bound per
   * pass, and re-running the pass rebound the same first layer and invalidated
   * the style again: a livelock that showed up as "only road_incident draws".
   * What we actually need to know is whether the style object exists to add to.
   */
  let styleReady = false;

  /** True when a layer's result is something a map could honestly draw. */
  function drawable(r) {
    return Boolean(r && !r.error && r.located && r.located.length > 0);
  }

  /** Any selected layer with drawable features? The mount predicate. */
  function anyDrawable() {
    return selected.some((l) => drawable(state.results.get(l)));
  }

  /** Create the map element + instance. No-op if already mounted. */
  function ensureMap() {
    if (map || !els.mapMount) return;
    const canvas = document.createElement('div');
    canvas.id = 'map-canvas';
    els.mapMount.appendChild(canvas);

    // Remounting must not yank the camera back to where the page loaded. The
    // last known camera (recorded on every moveend) wins; initialView is only
    // the seed for the very first mount.
    const view = state.lastView || initialView;

    // Shared light OSM vector basemap (basemap.js) under the hazard geometry. API data
    // is still only ever fetched same-origin from /api/v1/* through api.js.
    map = new maplibregl.Map({
      container: canvas,
      style: BASE_STYLE,
      center: view ? [view.lng, view.lat] : DEFAULT_CENTER,
      zoom: view ? view.zoom : DEFAULT_ZOOM,
      // Credit comes from the TileJSON; this only makes it compact.
      attributionControl: BASE_ATTRIBUTION_OPTS,
    });
    ensureBasemap(map);
    deferInteraction(map, canvas);
    map.addControl(new maplibregl.NavigationControl(), 'top-right');
    wireMapEvents(map);

    map.on('style.load', () => {
      styleReady = true;
      map.resize();
      // setStyle discarded every source we had added, so the bookkeeping is
      // stale before the reconciler runs.
      state.boundLayerIds.clear();
      syncMapLayers();
      if (pendingResize) {
        fitToFirstNonEmptyLayer();
        pendingResize = false;
      }
    });

    // The style can finish without another style.load — a fallback swap that
    // completes between a drop and the next event would otherwise strand it.
    map.on('idle', () => {
      if (pendingDraw) syncMapLayers();
    });
  }

  /** Tear the map down entirely and say why. */
  function suppressMap(reason) {
    if (map) {
      map.remove();
      map = null;
      styleReady = false;
    }
    if (els.mapMount) els.mapMount.textContent = '';
    state.boundLayerIds.clear();
    renderSuppressedBanner(reason);
  }

  /**
   * The three loud cases, per the design's banner spec. Each names what is
   * unknown and links the authoritative source when the metadata gave us one.
   */
  function renderSuppressedBanner(reason) {
    if (!els.suppressed) return;
    els.suppressed.textContent = '';
    if (!reason) return;
    const box = document.createElement('div');
    box.className = 'loud-banner';
    const title = document.createElement('div');
    title.className = 'loud-title';
    title.textContent = reason.title;
    const body = document.createElement('div');
    body.textContent = reason.body;
    box.append(title, body);
    if (reason.sourceUrl) {
      const p = document.createElement('div');
      p.style.marginTop = '6px';
      const a = document.createElement('a');
      a.href = reason.sourceUrl;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = 'check the authoritative source ↗';
      p.appendChild(a);
      box.appendChild(p);
    }
    els.suppressed.appendChild(box);
  }

  /**
   * Decide the map's existence from the current results. Called after every
   * load; this is the single place the mount rule lives.
   */
  function syncMapPresence() {
    if (anyDrawable()) {
      renderSuppressedBanner(null);
      ensureMap();
      return;
    }

    // Nothing drawable — work out which loud case this is.
    if (!urlState.place || selected.length === 0) {
      suppressMap({
        title: 'no layer selected',
        body: 'Pick a place and at least one layer. Nothing is drawn until a layer answers.',
      });
      return;
    }
    const results = selected.map((l) => state.results.get(l));
    if (results.some((r) => !r)) return; // still loading; leave the map as-is

    const failed = results.filter((r) => r && r.error);
    const unavailable = results.filter((r) => {
      const md = r && r.fc && r.fc.metadata;
      return md && String(md.sourceStatus || '').toUpperCase() === 'UNAVAILABLE';
    });
    const withUrl = [...unavailable, ...results].find(
      (r) => r && r.fc && r.fc.metadata && safeHttpUrl(r.fc.metadata.sourceUrl)
    );
    const sourceUrl = withUrl ? safeHttpUrl(withUrl.fc.metadata.sourceUrl) : null;

    if (failed.length) {
      // ANY failure poisons the claim. A partial failure is still an unknown:
      // the layer that errored is exactly where the thing we are not seeing
      // would be, so "some layers answered OK and were empty" must never be
      // reported as a confirmed empty region.
      const all = failed.length === results.length;
      suppressMap({
        title: 'layer unavailable — state unknown, not clear',
        body: all
          ? 'Every selected layer failed to fetch or timed out. Nothing is drawn, because ' +
            'an empty map would claim there is nothing there — and we do not know that. ' +
            'The exact requests are listed in the feed metadata below; replay them yourself.'
          : `${failed.length} of ${results.length} selected layers failed to fetch or timed out, ` +
            'and the rest returned no features. That is not a confirmed empty region — the ' +
            'failed layers are unknown, and nothing is drawn rather than implying they are clear. ' +
            'Per-layer status is in the feed metadata below.',
        sourceUrl,
      });
    } else if (unavailable.length) {
      suppressMap({
        title: 'sourceStatus: UNAVAILABLE — layer suppressed',
        body:
          'The upstream feed errored, so this layer arrives with empty features by contract. ' +
          'It is suppressed rather than drawn: showing nothing is not the same as reporting nothing.',
        sourceUrl,
      });
    } else {
      suppressMap({
        title: 'no features in range',
        body:
          'Every selected layer answered OK and returned no features for this place. That is a ' +
          'confirmed empty result — not a failure — but the map is left unmounted so an empty ' +
          'basemap is never mistaken for a surveyed all-clear.',
        sourceUrl,
      });
    }
  }

  /** Map-instance event wiring, re-run on each mount. */
  function wireMapEvents(m) {
    m.on('moveend', () => {
      if (state.programmaticMove) {
        state.programmaticMove = false;
        return;
      }
      const c = m.getCenter();
      state.lastView = { lat: c.lat, lng: c.lng, zoom: m.getZoom() };
      updateURL({ view: serializeView(state.lastView) });
    });
  }

  function sublayerIds(layer) {
    return ['fill', 'line', 'circle'].map((s) => `grid-${layer}-${s}`);
  }

  function removeLayerFromMap(layer) {
    if (!map) return; // map unmounted — nothing bound, nothing to remove
    for (const id of sublayerIds(layer)) {
      if (map.getLayer(id)) map.removeLayer(id);
    }
    if (map.getSource(`grid-${layer}`)) map.removeSource(`grid-${layer}`);
  }

  /**
   * Bind exactly the layers that should be drawn, and unbind the rest.
   *
   * Idempotent and cheap, so it can be called after ANY change — a result
   * arriving, a chip toggling, the style reloading — instead of each of those
   * paths remembering to add its own layer. The previous split (add on arrival,
   * replay on style.load) had a window between the last style.load and a late
   * result where an add was silently dropped and nothing ever retried it.
   */
  function syncMapLayers() {
    if (!map || !styleReady) {
      pendingDraw = true;
      return;
    }
    pendingDraw = false;
    for (const layer of MAP_LAYERS) {
      // One layer failing must not strand the rest — that is precisely how a
      // whole map ends up blank because of a single malformed geometry.
      try {
        const r = state.results.get(layer);
        if (selected.includes(layer) && drawable(r)) addLayerToMap(layer, r.located);
        else removeLayerFromMap(layer);
      } catch (err) {
        pendingDraw = true;
        // eslint-disable-next-line no-console
        console.warn(`map: could not bind ${layer}`, err);
      }
    }
    if (state.areaBoundary) addAreaBoundary(state.areaBoundary);
  }

  function addLayerToMap(layer, located) {
    // A layer can resolve while the style is mid-swap (ensureBasemap falls back
    // to FALLBACK_STYLE when the tile host is unreachable, and setStyle discards
    // every added source). Dropping it used to be "safe" on the theory that
    // style.load replays state.results — but style.load may ALREADY have fired,
    // and then the drop is permanent. That is the "no data until you toggle a
    // layer off and on" bug: measured live, 3 of 9 layers bound on load and 6
    // after one toggle. Record the miss and flush it on the next style event.
    if (!map || !styleReady) {
      pendingDraw = true;
      return;
    }
    removeLayerFromMap(layer);
    if (!located.length) return;
    const color = severityColorExpression();
    const src = `grid-${layer}`;
    map.addSource(src, {
      type: 'geojson',
      data: { type: 'FeatureCollection', features: located },
    });
    map.addLayer({
      id: `grid-${layer}-fill`,
      type: 'fill',
      source: src,
      filter: ['==', ['geometry-type'], 'Polygon'],
      paint: { 'fill-color': color, 'fill-opacity': 0.35 },
    });
    map.addLayer({
      id: `grid-${layer}-line`,
      type: 'line',
      source: src,
      filter: [
        'any',
        ['==', ['geometry-type'], 'Polygon'],
        ['==', ['geometry-type'], 'LineString'],
      ],
      paint: { 'line-color': color, 'line-width': 2 },
    });
    map.addLayer({
      id: `grid-${layer}-circle`,
      type: 'circle',
      source: src,
      filter: ['==', ['geometry-type'], 'Point'],
      paint: {
        'circle-color': color,
        'circle-radius': 6,
        'circle-opacity': 0.9,
        'circle-stroke-color': '#ffffff',
        'circle-stroke-width': 1,
      },
    });
    for (const id of sublayerIds(layer)) {
      if (state.boundLayerIds.has(id)) continue;
      state.boundLayerIds.add(id);
      map.on('click', id, onFeatureClick);
      map.on('mouseenter', id, () => {
        map.getCanvas().style.cursor = 'pointer';
      });
      map.on('mouseleave', id, () => {
        map.getCanvas().style.cursor = '';
      });
    }
  }

  /* ---- area boundary (coverage footprint) ---- */

  // Decode a Place's geometry.geojson (protojson base64 bytes) into a GeoJSON
  // geometry object, or null if absent/unparseable.
  function decodePlaceGeometry(place) {
    const raw = place && place.geometry && place.geometry.geojson;
    if (!raw) return null;
    try {
      const geom = typeof raw === 'string' ? JSON.parse(atob(raw)) : raw;
      if (geom && geom.type && geom.coordinates) return geom;
    } catch {
      /* decorative only — a bad decode just means no outline */
    }
    return null;
  }

  function removeAreaBoundary() {
    if (!map) return;
    if (map.getLayer('grid-area-boundary-line')) map.removeLayer('grid-area-boundary-line');
    if (map.getSource('grid-area-boundary')) map.removeSource('grid-area-boundary');
  }

  // Draw the selected place's coverage polygon as a faint dashed outline. The
  // boundary is decorative context — its fetch never blocks or errors the hazard
  // layers, and a place without polygon geometry simply shows none.
  async function loadAreaBoundary(token) {
    removeAreaBoundary();
    state.areaBoundary = null; // the old place's outline must not survive a remount
    if (!urlState.place) return;
    let geom;
    try {
      const place = await get(`/api/v1/places/${encodeURIComponent(urlState.place)}`);
      if (token !== state.loadToken) return;
      geom = decodePlaceGeometry(place);
    } catch {
      return;
    }
    if (!geom || token !== state.loadToken) return;
    // Remember it so a later mount can redraw the outline (see ensureMap()).
    state.areaBoundary = geom;
    addAreaBoundary(geom);
  }

  /** Draw the remembered coverage outline. No-op without a live map. */
  function addAreaBoundary(geom) {
    if (!map || !styleReady || map.getSource('grid-area-boundary')) return;
    map.addSource('grid-area-boundary', {
      type: 'geojson',
      data: { type: 'Feature', geometry: geom, properties: {} },
    });
    map.addLayer({
      id: 'grid-area-boundary-line',
      type: 'line',
      source: 'grid-area-boundary',
      paint: {
        'line-color': '#4b6b78',
        'line-width': 1.5,
        'line-dasharray': [3, 2],
        'line-opacity': 0.65,
      },
    });
  }

  /** Popup: headline, severity chip (canonical ramp + label), source
   * attribution, updatedAt. All upstream text via textContent. */
  function onFeatureClick(e) {
    const f = e.features && e.features[0];
    if (!f) return;
    const props = f.properties || {};

    const box = document.createElement('div');
    box.className = 'map-popup';

    const headline = document.createElement('div');
    headline.className = 'popup-headline';
    headline.textContent = props.headline || props.id || '(no headline)';

    const chips = document.createElement('div');
    chips.className = 'popup-chips';
    chips.appendChild(sevChip(props.severity));
    const layerTag = document.createElement('span');
    layerTag.className = 'muted small';
    layerTag.textContent = layerLabel(props.layer);
    chips.appendChild(layerTag);

    const srcLine = document.createElement('div');
    srcLine.className = 'popup-src muted small';
    const source = featureSource(props);
    const attribution =
      (source && (source.attribution || source.name)) || 'unknown source';
    srcLine.textContent = attribution;
    const href = safeHttpUrl(source && source.url);
    if (href) {
      srcLine.append(' · ');
      const a = document.createElement('a');
      a.href = href;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = 'source ↗';
      srcLine.appendChild(a);
    }

    const updated = document.createElement('div');
    updated.className = 'popup-updated muted small';
    updated.textContent = props.updatedAt
      ? `updated ${timeAgo(props.updatedAt)} · ${timeAbs(props.updatedAt)}`
      : 'updated —';

    const details = kindDetails(props);
    box.append(headline, chips);
    if (details) box.append(details);
    box.append(srcLine, updated);

    new maplibregl.Popup({ maxWidth: '320px' })
      .setLngLat(e.lngLat)
      .setDOMContent(box)
      .addTo(map);
  }

  /* ---- honesty panel ---- */

  // One container per layer, in canonical order; visibility follows the
  // checkbox so entries never reorder.
  /* ---- feed metadata: one table, one row per selected layer ----------
   *
   * This was nine stacked cards, each with a key/value list and its absolute
   * URL in an ink-black code box. On near-white paper those nine black bars
   * were the heaviest thing on the page, the values never lined up column to
   * column, and the whole block read as a different site to the Unlocated
   * table 200px below it — which is the same information shape and looks
   * right. So it is that: a .data-table.
   *
   * The URL rides under the layer name as a .cell-sub and copies on click
   * (ui.js copyOnClick), which is how every other long value on this site
   * behaves. No black box, no per-row button.
   *
   * Fail-loud is NOT flattened into a cell. A layer that errored, is
   * UNAVAILABLE, is STALE, or answered without a metadata block gets a tinted
   * row plus a full-width note row under it saying which, in words. A table is
   * allowed to be scannable; it is not allowed to make a failure look like a
   * value. */

  function metaHeadRow() {
    const tr = document.createElement('tr');
    for (const h of ['Layer', 'Status', 'Generated', 'Features', 'Attribution']) {
      const th = document.createElement('th');
      th.textContent = h;
      tr.appendChild(th);
    }
    return tr;
  }

  /** A full-width note beneath a layer's row — the loud channel. */
  function noteRow(cls, text, extra) {
    const tr = document.createElement('tr');
    tr.className = `row-note ${cls}`;
    const td = document.createElement('td');
    td.colSpan = 5;
    td.className = 'wrap';
    td.textContent = text;
    if (extra) td.append(' ', extra);
    tr.appendChild(td);
    return tr;
  }

  /** The layer cell: its name, and the absolute URL a client would point at. */
  function layerCell(layer, path) {
    const td = document.createElement('td');
    td.appendChild(el('div', 'cell-name', layerLabel(layer)));
    if (path) {
      const url = `${PUBLIC_ORIGIN}${path}`;
      const sub = el('div', 'cell-sub geojson-url', url);
      copyOnClick(sub, url, 'URL');
      td.appendChild(sub);
    }
    return td;
  }

  function renderPanel() {
    els.panel.textContent = '';
    const shown = MAP_LAYERS.filter((l) => selected.includes(l));
    if (!shown.length) {
      els.panel.append(el('p', 'muted small', 'No layer selected — nothing was requested.'));
      return;
    }

    const wrap = el('div', 'table-wrap');
    const table = el('table', 'data-table meta-table');
    const thead = document.createElement('thead');
    thead.appendChild(metaHeadRow());
    const tbody = document.createElement('tbody');

    for (const layer of shown) {
      const r = state.results.get(layer);
      const path = r ? r.path : (urlState.place ? geojsonPath(urlState.place, layer) : '');
      const tr = document.createElement('tr');
      tr.id = `meta-${layer}`;

      // --- not fetched yet, or nothing to fetch ---
      if (!r) {
        tr.appendChild(layerCell(layer, path));
        const td = document.createElement('td');
        td.colSpan = 4;
        td.className = 'muted';
        td.textContent = urlState.place ? 'fetching…' : 'no place selected';
        tr.appendChild(td);
        tbody.appendChild(tr);
        continue;
      }

      // --- the request itself failed ---
      if (r.error) {
        tr.className = 'row-UNAVAILABLE';
        tr.appendChild(layerCell(layer, path));
        const tdStatus = document.createElement('td');
        tdStatus.appendChild(sourceDot('UNAVAILABLE'));
        const tdRest = document.createElement('td');
        tdRest.colSpan = 3;
        tdRest.className = 'muted';
        tdRest.textContent = 'unknown — the request did not answer';
        tr.append(tdStatus, tdRest);
        tbody.appendChild(tr);
        tbody.appendChild(noteRow('note-fault',
          'Request failed — showing nothing ≠ all clear. Layer state is unknown. ' +
          (r.error instanceof ApiError
            ? `GET ${r.error.url} → ${r.error.status || 'network error'}: ${r.error.message}`
            : String((r.error && r.error.message) || r.error))));
        continue;
      }

      const md = r.fc && r.fc.metadata && typeof r.fc.metadata === 'object' ? r.fc.metadata : null;
      const status = md ? String(md.sourceStatus || '').toUpperCase() : '';
      if (status === 'UNAVAILABLE') tr.className = 'row-UNAVAILABLE';
      else if (status === 'STALE') tr.className = 'row-STALE';

      tr.appendChild(layerCell(layer, path));

      const tdStatus = document.createElement('td');
      tdStatus.appendChild(sourceDot(status || 'UNKNOWN'));
      tr.appendChild(tdStatus);

      const tdGen = document.createElement('td');
      tdGen.appendChild(timeCell(md && md.generatedAt));
      tr.appendChild(tdGen);

      const tdFeat = document.createElement('td');
      tdFeat.textContent = `${r.located.length} on map · ${r.unlocated.length} unlocated`;
      tr.appendChild(tdFeat);

      // Attribution is an obligation, not an optional column: when a layer
      // does not state one, say that rather than printing a dash that reads as
      // "nothing to credit".
      const tdAttr = document.createElement('td');
      tdAttr.className = 'wrap';
      if (md && md.attribution) {
        tdAttr.textContent = md.attribution;
      } else {
        tdAttr.appendChild(el('span', 'attr-absent', 'not stated by source'));
      }
      tr.appendChild(tdAttr);
      tbody.appendChild(tr);

      // --- the loud channel, beneath the row ---
      if (!md) {
        tbody.appendChild(noteRow('note-fault',
          'Response carried no metadata member — freshness and source health are unknown.'));
      } else if (status === 'UNAVAILABLE') {
        let link = null;
        const src = safeHttpUrl(md.sourceUrl);
        if (src) {
          link = document.createElement('a');
          link.href = src;
          link.target = '_blank';
          link.rel = 'noopener noreferrer';
          link.textContent = 'check the authoritative source ↗';
        }
        tbody.appendChild(noteRow('note-fault',
          'UNAVAILABLE — the upstream feed errored. Showing nothing ≠ all clear.', link));
      } else if (status === 'STALE') {
        tbody.appendChild(noteRow('note-stale', md.lastSourceUpdate
          ? `STALE — serving last-good data; last source update ${timeAgo(md.lastSourceUpdate)} (${timeAbs(md.lastSourceUpdate)}).`
          : 'STALE — serving last-good data; last source update unknown.'));
      }
    }

    table.append(thead, tbody);
    wrap.appendChild(table);
    els.panel.appendChild(wrap);
  }

  // Every entry lives in one table, so a single layer changing re-renders it.
  function renderPanelEntry() {
    renderPanel();
  }

  /* ---- unlocated (null-geometry) feature list ---- */

  function renderUnlocated() {
    els.unlocated.textContent = '';
    const rows = [];
    for (const layer of MAP_LAYERS) {
      if (!selected.includes(layer)) continue;
      const r = state.results.get(layer);
      if (!r || r.error) continue;
      for (const f of r.unlocated) rows.push([layer, f]);
    }
    if (rows.length === 0) {
      const p = document.createElement('p');
      p.className = 'muted small';
      p.textContent =
        'None — every feature in the fetched layers has geometry.';
      els.unlocated.appendChild(p);
      return;
    }

    const wrap = document.createElement('div');
    wrap.className = 'table-wrap';
    const table = document.createElement('table');
    table.className = 'data-table';
    const thead = document.createElement('thead');
    const hr = document.createElement('tr');
    for (const h of ['Severity', 'Headline', 'Layer', 'Source', 'Updated']) {
      const th = document.createElement('th');
      th.textContent = h;
      hr.appendChild(th);
    }
    thead.appendChild(hr);
    const tbody = document.createElement('tbody');
    for (const [layer, f] of rows) {
      const props = f.properties || {};
      const tr = document.createElement('tr');

      const tdSev = document.createElement('td');
      tdSev.appendChild(sevChip(props.severity));

      const tdHeadline = document.createElement('td');
      tdHeadline.className = 'wrap';
      tdHeadline.textContent = props.headline || props.id || '(no headline)';

      const tdLayer = document.createElement('td');
      tdLayer.textContent = layerLabel(props.layer || layer);

      const tdSrc = document.createElement('td');
      const source = featureSource(props);
      tdSrc.textContent = (source && (source.name || source.id)) || '—';

      const tdUpd = document.createElement('td');
      tdUpd.appendChild(timeCell(props.updatedAt));

      tr.append(tdSev, tdHeadline, tdLayer, tdSrc, tdUpd);
      tbody.appendChild(tr);
    }
    table.append(thead, tbody);
    wrap.appendChild(table);
    els.unlocated.appendChild(wrap);
  }

  /* ---- data loading ---- */

  async function loadLayer(layer, token) {
    state.results.delete(layer);
    renderPanelEntry(layer);
    const path = geojsonPath(urlState.place, layer);
    try {
      const fc = await get(path);
      if (token !== state.loadToken) return;
      const { located, unlocated } = splitFeatures(fc);
      state.results.set(layer, { path, fc, located, unlocated, error: null });
      // Reconcile rather than add: if the style is not ready this records the
      // miss and the next style/idle event flushes it.
      syncMapLayers();
    } catch (err) {
      if (token !== state.loadToken) return;
      state.results.set(layer, { path, error: err });
      removeLayerFromMap(layer);
    }
    renderPanelEntry(layer);
    renderUnlocated();
  }

  function fitToFirstNonEmptyLayer() {
    if (state.fitted) return;
    if (!map) {
      // Nothing to fit yet; ensureMap() re-runs this once the style loads.
      pendingResize = true;
      return;
    }
    state.fitted = true; // one shot: later toggles never yank the view
    for (const layer of MAP_LAYERS) {
      if (!selected.includes(layer)) continue;
      const r = state.results.get(layer);
      if (!r || r.error || r.located.length === 0) continue;
      const bbox = bboxOfFeatures(r.located);
      if (!bbox) continue;
      state.programmaticMove = true;
      map.fitBounds(
        [
          [bbox[0], bbox[1]],
          [bbox[2], bbox[3]],
        ],
        { padding: 48, maxZoom: 12, duration: 0 }
      );
      return;
    }
    // No non-empty layer: stay at the Calaveras default center.
  }

  async function reloadAll() {
    state.loadToken += 1;
    const token = state.loadToken;
    state.results.clear();
    for (const layer of MAP_LAYERS) removeLayerFromMap(layer);
    renderPanel();
    renderUnlocated();
    loadAreaBoundary(token); // coverage footprint — independent of hazard layers
    if (!urlState.place || selected.length === 0) {
      syncMapPresence();
      return;
    }
    await Promise.allSettled(selected.map((l) => loadLayer(l, token)));
    if (token !== state.loadToken) return;
    // The mount rule runs once, here, on the settled result set — never
    // per-layer, or a slow UNAVAILABLE layer would tear down a map that a
    // fast OK layer had legitimately populated.
    syncMapPresence();
    fitToFirstNonEmptyLayer();
  }

  /* ---- controls ---- */

  function renderLayerChecks() {
    els.layerChecks.options = MAP_LAYERS.map((l) => ({ value: l, label: layerLabel(l) }));
    els.layerChecks.value = selected;
    renderQueryEcho();
  }

  // The chip row reports the whole new selection, so this reconciles rather
  // than handling one chip: whatever was added gets loaded, whatever was
  // removed gets torn down.
  els.layerChecks.addEventListener('change', async (e) => {
    const next = MAP_LAYERS.filter((l) => e.detail.value.includes(l));
    const added = next.filter((l) => !selected.includes(l));
    const removed = selected.filter((l) => !next.includes(l));
    selected = next;
    updateURL();

    for (const layer of removed) {
      state.results.delete(layer);
      removeLayerFromMap(layer);
      renderPanelEntry(layer);
    }
    if (removed.length) renderUnlocated();

    for (const layer of added) {
      // await: loadLayer must have settled before the mount rule can judge the
      // result set, or a newly-ticked layer with real features would be judged
      // as "still loading" and leave the map suppressed.
      if (urlState.place) await loadLayer(layer, state.loadToken);
      renderPanelEntry(layer);
    }

    // THE MOUNT RULE MUST RUN ON EVERY PATH THAT CHANGES THE RESULT SET.
    // Untoggling the last drawable layer would otherwise leave a rendered
    // basemap with zero features on screen — the "all clear" the contract
    // forbids — and toggling one on while suppressed would leave a stale banner
    // over real data.
    syncMapPresence();
    renderQueryEcho();
  });

  /**
   * Echo the request behind what is drawn: the TEMPLATE, and how many times it
   * ran. One line.
   *
   * This screen makes one request per selected layer, and the layer names are
   * on the page twice already — the lit chips directly above, and the ledger
   * below, which prints every URL in full, absolute and copyable. Listing them
   * here too was a third copy: first as nine stacked lines (267px, pushing the
   * map below the fold), then as a wrapped run of nine `.geojson` names, which
   * read as a wall of near-identical text. The template plus a count says the
   * same thing and adds nothing the reader must scan.
   *
   * With nothing selected it says so in words rather than printing an empty
   * line: no request was made, which is a different statement from a request
   * that returned nothing.
   */
  function renderQueryEcho() {
    const box = els.queryEcho;
    if (!box) return;
    box.textContent = '';
    if (!urlState.place) return;
    if (!selected.length) {
      box.append(el('span', 'muted', 'no layer selected — no request made'));
      return;
    }

    const m = document.createElement('span');
    m.className = 'method';
    m.textContent = 'GET';
    const place = encodeURIComponent(urlState.place);
    box.append(m, document.createTextNode(`/api/v1/places/${place}/map/{layer}.geojson`));

    const n = selected.length;
    box.append(el('span', 'muted',
      n === 1 ? 'one selected layer' : `${n} selected layers, one request each`));
  }

  function placeSlug(p) {
    if (p.slug) return p.slug;
    const id = String(p.id || '');
    return id.includes(':') ? id.slice(id.indexOf(':') + 1) : id;
  }

  /** The AREA directory, kept so the trigger can name the selection. */
  let placeDir = [];

  /** Paint the picker's current selection onto its trigger. */
  function showPlace() {
    els.place.value = urlState.place;
    els.place.triggerLabel = placeMenuLabel(placeDir, urlState.place, 'no place');
  }

  async function loadPlaces() {
    try {
      // The WHOLE directory, grouped by kind — not just `kind=AREA`. Every map
      // layer is addressable by any {place}, so scoping to a corridor, a town
      // or a county is a legitimate thing to want, and the AREA-only fetch
      // offered a menu with one entry in it. The DEFAULT is still an area: a
      // reader who has chosen nothing wants the whole region, and an evacuation
      // zone would be an odd place to open on.
      const data = await get('/api/v1/places');
      placeDir = (Array.isArray(data.places) ? data.places : []).filter(placeSlug);
      const areas = placeDir.filter((p) => p.kind === 'AREA').map(placeSlug);
      if (!urlState.place) {
        urlState.place = areas.includes('ebbetts-pass')
          ? 'ebbetts-pass'
          : areas[0] || placeSlug(placeDir[0] || {}) || '';
      }
      // A ?place= the directory does not list is kept by placeMenuOptions — a
      // deep link must not be re-scoped to somewhere else.
      els.place.options = placeMenuOptions(placeDir, { current: urlState.place, group: true });
      showPlace();
      if (placeDir.length === 0) {
        pageError(
          'No places returned by /api/v1/places — nothing to map.',
          new Error('empty place list')
        );
      }
    } catch (err) {
      pageError('Failed to load the place picker.', err);
      // The directory is gone, but a place named in the URL is still a valid
      // path segment — keep it selected and let its layers be tried.
      els.place.options = placeMenuOptions([], { current: urlState.place, group: true });
      showPlace();
      if (!urlState.place) els.place.triggerLabel = 'unavailable';
    }
  }

  els.place.addEventListener('change', (e) => {
    updateURL({ place: e.detail.value });
    showPlace();
    renderQueryEcho(); // the echoed paths carry the place — they change with it
    reloadAll();
  });

  /* ---- boot ---- */

  renderLayerChecks();
  renderPanel();
  renderUnlocated();

  (async () => {
    await loadPlaces();
    updateURL(); // canonicalize: resolved place (+ layers if non-default)
    renderQueryEcho(); // only now is there a place to name in the paths
    await reloadAll();
  })();
}

if (typeof document !== 'undefined' && document.getElementById('map-mount')) {
  init();
}
