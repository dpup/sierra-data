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

import { get, ApiError } from '../api.js';
import {
  timeAgo,
  timeAbs,
  timeCell,
  sevChip,
  sourceDot,
  layerLabel,
  SEVERITY_COLORS,
} from '../format.js';
import { BASE_STYLE } from '../basemap.js';

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
];

/** Default map view when no layer has features and no ?view= is present:
 * Ebbetts Pass corridor, roughly centered. [lng, lat] per GeoJSON axis order. */
export const DEFAULT_CENTER = [-120.55, 38.2];
export const DEFAULT_ZOOM = 8.5;

/** Canonical public origin, for the copyable third-party-client URL. */
export const PUBLIC_ORIGIN = 'https://data.sierragridteam.org';

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
 * MapLibre paint expression coloring by properties.severity on the canonical
 * ramp. Unknown severities fall through to the INFO gray — never uncolored.
 * @returns {Array} ['match', ['get','severity'], 'EXTREME', '#7f1d1d', ...]
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
    panel: document.getElementById('honesty-panel'),
    unlocated: document.getElementById('unlocated'),
    errors: document.getElementById('page-errors'),
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

  /* ---- map ---- */

  // Shared OSM raster basemap (basemap.js) under the hazard geometry. API data
  // is still only ever fetched same-origin from /api/v1/* through api.js.
  const map = new maplibregl.Map({
    container: 'map-canvas',
    style: BASE_STYLE,
    center: initialView ? [initialView.lng, initialView.lat] : DEFAULT_CENTER,
    zoom: initialView ? initialView.zoom : DEFAULT_ZOOM,
  });
  map.addControl(new maplibregl.NavigationControl(), 'top-right');

  map.on('moveend', () => {
    if (state.programmaticMove) {
      state.programmaticMove = false;
      return;
    }
    const c = map.getCenter();
    updateURL({
      view: serializeView({ lat: c.lat, lng: c.lng, zoom: map.getZoom() }),
    });
  });

  function sublayerIds(layer) {
    return ['fill', 'line', 'circle'].map((s) => `grid-${layer}-${s}`);
  }

  function removeLayerFromMap(layer) {
    for (const id of sublayerIds(layer)) {
      if (map.getLayer(id)) map.removeLayer(id);
    }
    if (map.getSource(`grid-${layer}`)) map.removeSource(`grid-${layer}`);
  }

  function addLayerToMap(layer, located) {
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
    if (map.getLayer('grid-area-boundary-line')) map.removeLayer('grid-area-boundary-line');
    if (map.getSource('grid-area-boundary')) map.removeSource('grid-area-boundary');
  }

  // Draw the selected place's coverage polygon as a faint dashed outline. The
  // boundary is decorative context — its fetch never blocks or errors the hazard
  // layers, and a place without polygon geometry simply shows none.
  async function loadAreaBoundary(token) {
    removeAreaBoundary();
    if (!urlState.place) return;
    let geom;
    try {
      const place = await get(`/api/v1/places/${encodeURIComponent(urlState.place)}`);
      if (token !== state.loadToken) return;
      geom = decodePlaceGeometry(place);
    } catch {
      return;
    }
    if (!geom || token !== state.loadToken || map.getSource('grid-area-boundary')) return;
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

    box.append(headline, chips, srcLine, updated);

    new maplibregl.Popup({ maxWidth: '320px' })
      .setLngLat(e.lngLat)
      .setDOMContent(box)
      .addTo(map);
  }

  /* ---- honesty panel ---- */

  // One container per layer, in canonical order; visibility follows the
  // checkbox so entries never reorder.
  const panelEntries = new Map();
  for (const layer of MAP_LAYERS) {
    const entry = document.createElement('div');
    entry.className = 'layer-meta';
    entry.id = `meta-${layer}`;
    entry.hidden = true;
    els.panel.appendChild(entry);
    panelEntries.set(layer, entry);
  }

  function copyButton(text) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn';
    btn.textContent = 'copy';
    btn.addEventListener('click', () => {
      if (!navigator.clipboard) {
        btn.textContent = 'failed';
        return;
      }
      navigator.clipboard.writeText(text).then(
        () => {
          btn.textContent = 'copied';
          setTimeout(() => (btn.textContent = 'copy'), 1200);
        },
        () => {
          btn.textContent = 'failed';
        }
      );
    });
    return btn;
  }

  function urlRow(path) {
    const wrap = document.createElement('div');
    wrap.className = 'geojson-url';
    const caption = document.createElement('div');
    caption.className = 'muted geojson-url-caption';
    caption.textContent = 'what a third-party map client would use:';
    const line = document.createElement('div');
    line.className = 'geojson-url-line';
    const code = document.createElement('code');
    code.textContent = `${PUBLIC_ORIGIN}${path}`;
    line.append(code, copyButton(`${PUBLIC_ORIGIN}${path}`));
    wrap.append(caption, line);
    return wrap;
  }

  function metaRow(label, valueNode) {
    const row = document.createElement('div');
    row.className = 'meta-row';
    const key = document.createElement('span');
    key.className = 'meta-key';
    key.textContent = label;
    row.append(key, ' ');
    row.appendChild(valueNode);
    return row;
  }

  function textNode(cls, text) {
    const span = document.createElement('span');
    if (cls) span.className = cls;
    span.textContent = text;
    return span;
  }

  function renderPanelEntry(layer) {
    const entry = panelEntries.get(layer);
    entry.textContent = '';
    entry.hidden = !selected.includes(layer);
    if (entry.hidden) return;

    const r = state.results.get(layer);

    const head = document.createElement('div');
    head.className = 'layer-meta-head';
    const name = document.createElement('strong');
    name.textContent = layerLabel(layer);
    head.appendChild(name);
    entry.appendChild(head);

    if (!r && !urlState.place) {
      head.appendChild(textNode('muted small', 'no place selected'));
      return;
    }
    const path = r ? r.path : geojsonPath(urlState.place, layer);
    if (!r) {
      head.appendChild(textNode('muted small', 'fetching…'));
      entry.appendChild(urlRow(path));
      return;
    }

    if (r.error) {
      head.appendChild(sourceDot('UNAVAILABLE'));
      const note = document.createElement('div');
      note.className = 'meta-unavailable';
      note.textContent =
        'request failed — showing nothing ≠ all clear. Layer state is unknown.';
      const errLine = document.createElement('div');
      errLine.className = 'error-block meta-error';
      errLine.textContent =
        r.error instanceof ApiError
          ? `GET ${r.error.url} → ${r.error.status || 'network error'}: ${r.error.message}`
          : String(r.error && r.error.message ? r.error.message : r.error);
      entry.append(note, errLine, urlRow(path));
      return;
    }

    const md =
      r.fc && r.fc.metadata && typeof r.fc.metadata === 'object'
        ? r.fc.metadata
        : null;
    const status = md ? String(md.sourceStatus || '').toUpperCase() : '';
    head.appendChild(sourceDot(status || 'UNKNOWN'));

    if (!md) {
      const warn = document.createElement('div');
      warn.className = 'meta-note';
      warn.textContent =
        'response has no metadata member — freshness unknown.';
      entry.appendChild(warn);
    } else if (status === 'UNAVAILABLE') {
      const note = document.createElement('div');
      note.className = 'meta-unavailable';
      note.textContent =
        'UNAVAILABLE — the upstream feed errored. Showing nothing ≠ all clear.';
      entry.appendChild(note);
      const src = safeHttpUrl(md.sourceUrl);
      if (src) {
        const p = document.createElement('div');
        p.className = 'meta-note';
        const a = document.createElement('a');
        a.href = src;
        a.target = '_blank';
        a.rel = 'noopener noreferrer';
        a.textContent = 'check the authoritative source ↗';
        p.appendChild(a);
        entry.appendChild(p);
      }
    } else if (status === 'STALE') {
      const note = document.createElement('div');
      note.className = 'meta-note meta-stale';
      note.textContent = md.lastSourceUpdate
        ? `STALE — serving last-good data; last source update ${timeAgo(
            md.lastSourceUpdate
          )} (${timeAbs(md.lastSourceUpdate)}).`
        : 'STALE — serving last-good data; last source update unknown.';
      entry.appendChild(note);
    }

    if (md) {
      entry.appendChild(metaRow('generatedAt', timeCell(md.generatedAt)));
      if (status === 'STALE' && md.lastSourceUpdate) {
        entry.appendChild(
          metaRow('lastSourceUpdate', timeCell(md.lastSourceUpdate))
        );
      }
      entry.appendChild(
        metaRow('attribution', textNode('', md.attribution || '—'))
      );
    }
    const counts = `${r.located.length} on map · ${r.unlocated.length} unlocated`;
    entry.appendChild(metaRow('features', textNode('', counts)));
    entry.appendChild(urlRow(path));
  }

  function renderPanel() {
    for (const layer of MAP_LAYERS) renderPanelEntry(layer);
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
      addLayerToMap(layer, located);
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
    if (!urlState.place || selected.length === 0) return;
    await Promise.allSettled(selected.map((l) => loadLayer(l, token)));
    if (token === state.loadToken) fitToFirstNonEmptyLayer();
  }

  /* ---- controls ---- */

  function renderLayerChecks() {
    els.layerChecks.textContent = '';
    for (const layer of MAP_LAYERS) {
      const label = document.createElement('label');
      label.className = 'layer-check';
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.value = layer;
      input.checked = selected.includes(layer);
      input.addEventListener('change', () => {
        if (input.checked) {
          selected = MAP_LAYERS.filter(
            (l) => selected.includes(l) || l === layer
          );
          updateURL();
          if (urlState.place) loadLayer(layer, state.loadToken);
          renderPanelEntry(layer);
        } else {
          selected = selected.filter((l) => l !== layer);
          updateURL();
          state.results.delete(layer);
          removeLayerFromMap(layer);
          renderPanelEntry(layer);
          renderUnlocated();
        }
      });
      label.append(input, ` ${layerLabel(layer)}`);
      els.layerChecks.appendChild(label);
    }
  }

  function placeSlug(p) {
    if (p.slug) return p.slug;
    const id = String(p.id || '');
    return id.includes(':') ? id.slice(id.indexOf(':') + 1) : id;
  }

  async function loadPlaces() {
    try {
      const data = await get('/api/v1/places', { kind: 'AREA' });
      const places = Array.isArray(data.places) ? data.places : [];
      const slugs = [];
      els.place.textContent = '';
      for (const p of places) {
        const slug = placeSlug(p);
        if (!slug) continue;
        slugs.push(slug);
        const opt = document.createElement('option');
        opt.value = slug;
        opt.textContent = p.name ? `${p.name} (${slug})` : slug;
        els.place.appendChild(opt);
      }
      if (urlState.place && !slugs.includes(urlState.place)) {
        // Honor a URL-provided place the picker doesn't know (deep link).
        const opt = document.createElement('option');
        opt.value = urlState.place;
        opt.textContent = urlState.place;
        els.place.appendChild(opt);
        slugs.push(urlState.place);
      }
      if (!urlState.place) {
        urlState.place = slugs.includes('ebbetts-pass')
          ? 'ebbetts-pass'
          : slugs[0] || '';
      }
      els.place.value = urlState.place;
      els.place.disabled = slugs.length === 0;
      if (slugs.length === 0) {
        pageError(
          'No AREA places returned by /api/v1/places?kind=AREA — nothing to map.',
          new Error('empty place list')
        );
      }
    } catch (err) {
      pageError('Failed to load the place picker.', err);
      els.place.textContent = '';
      if (urlState.place) {
        // Still try the layers for the place named in the URL.
        const opt = document.createElement('option');
        opt.value = urlState.place;
        opt.textContent = urlState.place;
        els.place.appendChild(opt);
        els.place.value = urlState.place;
      } else {
        els.place.disabled = true;
      }
    }
  }

  els.place.addEventListener('change', () => {
    updateURL({ place: els.place.value });
    reloadAll();
  });

  /* ---- boot ---- */

  renderLayerChecks();
  renderPanel();
  renderUnlocated();

  map.on('load', async () => {
    map.resize(); // fill the container once the style is ready
    await loadPlaces();
    updateURL(); // canonicalize: resolved place (+ layers if non-default)
    await reloadAll();
  });
}

if (typeof document !== 'undefined' && document.getElementById('map-canvas')) {
  init();
}
