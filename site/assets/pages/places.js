// places.js — /places: directory by kind, place detail with geometry, and the
// resolve tester (secretly the zone-import QA tool).
//
// URL state (all shareable, per site principle 3):
//   ?place={slug|id}   selected place (absent → directory view)
//   ?lat=&lng=         resolve-tester point (map click or manual input)
//   ?address=          resolve-tester address
//
// All network I/O goes through the shared api.js wrapper (same-origin /v1/*
// GETs only). Upstream-derived text is inserted via textContent — never HTML.
//
// Pure helpers (readState, writeState, parseCoord, groupByKind, geomBBox,
// protoBBox, placeRef) have zero DOM access at import time so node can import
// and test this module directly. All DOM work happens inside initPlacesPage().

import { get, ApiError, apiURL } from '../api.js';
import { layerLabel, decodeGeometry } from '../format.js';
import { initChrome } from '../nav.js';
import { BASE_STYLE } from '../basemap.js';

// ------------------------------------------------------------------------
// Pure helpers (node-testable)
// ------------------------------------------------------------------------

/** Canonical display order for PlaceKind groups; unknown kinds sort after,
 * alphabetically. Directory renders whatever kinds the API returns. */
export const KIND_ORDER = [
  'AREA',
  'COUNTY',
  'TOWN',
  'EVAC_ZONE',
  'CORRIDOR',
  'SITE',
];

/**
 * Read page state from a query string.
 * @param {string} search e.g. location.search ("?place=calaveras&lat=38.1")
 * @returns {{place:string, lat:string, lng:string, address:string}}
 */
export function readState(search) {
  const q = new URLSearchParams(search || '');
  return {
    place: q.get('place') || '',
    lat: q.get('lat') || '',
    lng: q.get('lng') || '',
    address: q.get('address') || '',
  };
}

/**
 * Serialize page state back to a query string ("" or "?...").
 * Empty values are omitted so URLs stay canonical and shareable.
 * @param {{place?:string, lat?:string|number, lng?:string|number, address?:string}} state
 * @returns {string}
 */
export function writeState(state) {
  const q = new URLSearchParams();
  if (state.place) q.set('place', state.place);
  if (state.lat !== '' && state.lat !== null && state.lat !== undefined) {
    q.set('lat', String(state.lat));
  }
  if (state.lng !== '' && state.lng !== null && state.lng !== undefined) {
    q.set('lng', String(state.lng));
  }
  if (state.address) q.set('address', state.address);
  const s = q.toString();
  return s ? `?${s}` : '';
}

/**
 * Validate a lat/lng pair from text inputs.
 * @param {string|number} latStr
 * @param {string|number} lngStr
 * @returns {{lat:number, lng:number}|null} null if not a plausible coordinate
 */
export function parseCoord(latStr, lngStr) {
  if (latStr === '' || latStr === null || latStr === undefined) return null;
  if (lngStr === '' || lngStr === null || lngStr === undefined) return null;
  const lat = Number(latStr);
  const lng = Number(lngStr);
  if (!Number.isFinite(lat) || !Number.isFinite(lng)) return null;
  if (lat < -90 || lat > 90 || lng < -180 || lng > 180) return null;
  return { lat, lng };
}

/**
 * Group places by kind for the directory. Kinds in KIND_ORDER first, then
 * unknown kinds alphabetically; places within a kind sorted by name.
 * protojson omits zero-valued enums, so a missing kind groups under
 * PLACE_KIND_UNSPECIFIED.
 * @param {Array<Object>} places
 * @returns {Array<[string, Array<Object>]>}
 */
export function groupByKind(places) {
  const groups = new Map();
  for (const p of places || []) {
    if (!p || typeof p !== 'object') continue;
    const kind = String(p.kind || 'PLACE_KIND_UNSPECIFIED').toUpperCase();
    if (!groups.has(kind)) groups.set(kind, []);
    groups.get(kind).push(p);
  }
  for (const list of groups.values()) {
    list.sort((a, b) =>
      String(a.name || a.slug || a.id || '').localeCompare(
        String(b.name || b.slug || b.id || '')
      )
    );
  }
  const known = KIND_ORDER.filter((k) => groups.has(k));
  const rest = [...groups.keys()]
    .filter((k) => !KIND_ORDER.includes(k))
    .sort();
  return [...known, ...rest].map((k) => [k, groups.get(k)]);
}

/**
 * Compute [minLng, minLat, maxLng, maxLat] from an RFC 7946 geometry object
 * (any type, including GeometryCollection). Returns null if no positions.
 * @param {Object|null} geom decoded GeoJSON geometry
 * @returns {[number,number,number,number]|null}
 */
export function geomBBox(geom) {
  if (!geom || typeof geom !== 'object') return null;
  let minLng = Infinity;
  let minLat = Infinity;
  let maxLng = -Infinity;
  let maxLat = -Infinity;

  const visit = (coords) => {
    if (!Array.isArray(coords)) return;
    if (typeof coords[0] === 'number') {
      const [lng, lat] = coords;
      if (!Number.isFinite(lng) || !Number.isFinite(lat)) return;
      if (lng < minLng) minLng = lng;
      if (lng > maxLng) maxLng = lng;
      if (lat < minLat) minLat = lat;
      if (lat > maxLat) maxLat = lat;
      return;
    }
    for (const c of coords) visit(c);
  };

  if (geom.type === 'GeometryCollection') {
    for (const g of geom.geometries || []) {
      const b = geomBBox(g);
      if (!b) continue;
      if (b[0] < minLng) minLng = b[0];
      if (b[1] < minLat) minLat = b[1];
      if (b[2] > maxLng) maxLng = b[2];
      if (b[3] > maxLat) maxLat = b[3];
    }
  } else {
    visit(geom.coordinates);
  }

  if (!Number.isFinite(minLng) || !Number.isFinite(minLat)) return null;
  return [minLng, minLat, maxLng, maxLat];
}

/**
 * Convert a protojson BoundingBox ({min_lat,min_lng,max_lat,max_lng}) to
 * [minLng, minLat, maxLng, maxLat]; null when absent/incomplete. protojson
 * may omit zero-valued doubles, so missing members default to 0 only if at
 * least one member is present.
 * @param {Object|null} bbox
 * @returns {[number,number,number,number]|null}
 */
export function protoBBox(bbox) {
  if (!bbox || typeof bbox !== 'object') return null;
  const keys = ['min_lng', 'min_lat', 'max_lng', 'max_lat'];
  if (!keys.some((k) => bbox[k] !== undefined)) return null;
  const v = keys.map((k) => Number(bbox[k] ?? 0));
  if (v.some((n) => !Number.isFinite(n))) return null;
  return [v[0], v[1], v[2], v[3]];
}

/**
 * Preferred URL reference for a place: slug when present, else id.
 * (Places are addressable by slug or id per the API spec.)
 * @param {Object} place
 * @returns {string}
 */
export function placeRef(place) {
  if (!place || typeof place !== 'object') return '';
  return place.slug || place.id || '';
}

// ------------------------------------------------------------------------
// Page (DOM from here down; only runs when initPlacesPage() is called)
// ------------------------------------------------------------------------

const DEFAULT_CENTER = [-120.45, 38.1]; // central Sierra service area
const DEFAULT_ZOOM = 8;
const ACCENT = '#38bdf8';

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/** Inline error block naming the failed URL — never a blank page. */
function errorBlock(err) {
  const div = el('div', 'error-block');
  div.append(
    el(
      'div',
      '',
      err instanceof ApiError
        ? `Request failed (HTTP ${err.status || 'network error'}):`
        : 'Request failed:'
    )
  );
  div.append(
    el(
      'div',
      'error-url',
      err instanceof ApiError ? `GET ${err.url}` : String(err.message || err)
    )
  );
  if (
    err instanceof ApiError &&
    err.body &&
    typeof err.body === 'object' &&
    err.body.message
  ) {
    div.append(el('div', 'muted', err.body.message));
  }
  return div;
}

/** Neutral kind badge: outline chip, raw enum text (label always present). */
function kindChip(kind) {
  return el(
    'span',
    'chip sev-unknown',
    String(kind || 'UNKNOWN').toUpperCase()
  );
}

export function initPlacesPage() {
  initChrome('places');

  const $ = (id) => document.getElementById(id);
  const state = readState(location.search);

  const placeView = $('place-view');
  const placeNameEl = $('place-name');
  const placeDetailEl = $('place-detail');
  const directoryView = $('directory-view');
  const directoryEl = $('directory');
  const mapHintEl = $('map-hint');
  const resultsEl = $('resolve-results');
  const latInput = $('lat-input');
  const lngInput = $('lng-input');
  const addressInput = $('address-input');

  function syncURL() {
    history.replaceState(null, '', location.pathname + writeState(state));
  }

  /** Link into this page with a different selected place, preserving the
   * resolve-tester state so QA context survives navigation. */
  function hrefFor(ref) {
    return location.pathname + writeState({ ...state, place: ref });
  }

  // ---- Map (single instance: place geometry + resolve click target) ------
  let map = null;
  let mapReady = null;
  let marker = null;

  function setupMap() {
    const container = $('map');
    if (typeof maplibregl === 'undefined') {
      const note = el(
        'div',
        'notice',
        'Map library failed to load — the resolve tester still works via the lat/lng and address inputs below.'
      );
      container.replaceWith(note);
      return;
    }
    // Shared OSM raster basemap (basemap.js) for geographic context — the
    // resolve tester needs a recognizable backdrop to click against. API data
    // is still only ever fetched same-origin from /v1/* through api.js.
    map = new maplibregl.Map({
      container,
      style: BASE_STYLE,
      center: DEFAULT_CENTER,
      zoom: DEFAULT_ZOOM,
    });
    map.addControl(new maplibregl.NavigationControl(), 'top-right');
    mapReady = new Promise((resolve) => map.on('load', resolve));
    map.on('click', (e) => {
      const lat = Number(e.lngLat.lat.toFixed(5));
      const lng = Number(e.lngLat.lng.toFixed(5));
      latInput.value = String(lat);
      lngInput.value = String(lng);
      resolvePoint(lat, lng);
    });
  }

  function setMarker(lng, lat) {
    if (!map) return;
    if (!marker) marker = new maplibregl.Marker({ color: ACCENT });
    marker.setLngLat([lng, lat]).addTo(map);
  }

  async function showGeometry(geom, bboxArr) {
    if (!map) return;
    await mapReady;
    map.addSource('place-geom', {
      type: 'geojson',
      data: { type: 'Feature', geometry: geom, properties: {} },
    });
    map.addLayer({
      id: 'place-fill',
      type: 'fill',
      source: 'place-geom',
      filter: ['==', ['geometry-type'], 'Polygon'],
      paint: { 'fill-color': ACCENT, 'fill-opacity': 0.2 },
    });
    map.addLayer({
      id: 'place-outline',
      type: 'line',
      source: 'place-geom',
      filter: ['==', ['geometry-type'], 'Polygon'],
      paint: { 'line-color': ACCENT, 'line-width': 2 },
    });
    map.addLayer({
      id: 'place-line',
      type: 'line',
      source: 'place-geom',
      filter: ['==', ['geometry-type'], 'LineString'],
      paint: { 'line-color': ACCENT, 'line-width': 3 },
    });
    map.addLayer({
      id: 'place-point',
      type: 'circle',
      source: 'place-geom',
      filter: ['==', ['geometry-type'], 'Point'],
      paint: {
        'circle-radius': 6,
        'circle-color': ACCENT,
        'circle-stroke-color': '#ffffff',
        'circle-stroke-width': 1.5,
      },
    });
    const b = bboxArr || geomBBox(geom);
    if (b) {
      map.fitBounds(
        [
          [b[0], b[1]],
          [b[2], b[3]],
        ],
        { padding: 48, maxZoom: 13, duration: 0 }
      );
    }
  }

  // ---- Directory view: GET /v1/places ------------------------------------
  async function loadDirectory() {
    let places;
    try {
      const data = await get('/v1/places');
      places = Array.isArray(data.places) ? data.places : [];
    } catch (err) {
      directoryEl.textContent = '';
      directoryEl.append(errorBlock(err));
      return;
    }
    directoryEl.textContent = '';
    if (places.length === 0) {
      directoryEl.append(el('p', 'muted', 'No places in the directory yet.'));
      return;
    }
    const byId = new Map(places.filter((p) => p.id).map((p) => [p.id, p]));

    for (const [kind, list] of groupByKind(places)) {
      const heading = el('h3');
      heading.append(
        layerLabel(kind === 'PLACE_KIND_UNSPECIFIED' ? 'UNSPECIFIED' : kind),
        ' ',
        el('span', 'muted small', `(${list.length})`)
      );
      directoryEl.append(heading);

      const wrap = el('div', 'table-wrap');
      const table = el('table', 'data-table');
      const thead = el('thead');
      const hrow = el('tr');
      for (const h of ['Name', 'Slug', 'Id', 'Parent']) {
        hrow.append(el('th', '', h));
      }
      thead.append(hrow);
      const tbody = el('tbody');

      for (const p of list) {
        const tr = el('tr');

        const nameTd = el('td');
        const ref = placeRef(p);
        if (ref) {
          const a = el('a', '', p.name || ref);
          a.href = hrefFor(ref);
          nameTd.append(a);
        } else {
          nameTd.textContent = p.name || '—';
        }
        tr.append(nameTd);

        tr.append(el('td', 'mono', p.slug || '—'));
        tr.append(el('td', 'mono', p.id || '—'));

        const parentTd = el('td');
        if (p.parent_id) {
          const parent = byId.get(p.parent_id);
          const a = el(
            'a',
            '',
            parent ? parent.name || placeRef(parent) : p.parent_id
          );
          a.href = hrefFor(parent ? placeRef(parent) : p.parent_id);
          a.title = p.parent_id;
          parentTd.append(a);
        } else {
          parentTd.textContent = '—';
        }
        tr.append(parentTd);

        tbody.append(tr);
      }
      table.append(thead, tbody);
      wrap.append(table);
      directoryEl.append(wrap);
    }
  }

  // ---- Selected view: GET /v1/places/{place} ------------------------------
  async function loadPlace(ref) {
    placeView.hidden = false;
    directoryView.hidden = true;
    let place;
    try {
      place = await get(`/v1/places/${encodeURIComponent(ref)}`);
    } catch (err) {
      placeNameEl.textContent = ref;
      placeDetailEl.textContent = '';
      placeDetailEl.append(errorBlock(err));
      return;
    }

    const name = place.name || place.slug || place.id || ref;
    document.title = `${name} — Places — SIERRA Grid Data`;
    placeNameEl.textContent = '';
    placeNameEl.append(name, ' ', kindChip(place.kind || 'UNSPECIFIED'));
    placeDetailEl.textContent = '';

    const kv = el('dl', 'kv');
    const row = (label, valueNode) => {
      kv.append(el('dt', '', label));
      const dd = el('dd');
      if (valueNode instanceof Node) dd.append(valueNode);
      else dd.textContent = valueNode;
      kv.append(dd);
    };
    row('id', place.id || '—');
    row('slug', place.slug || '—');
    row('kind', String(place.kind || 'PLACE_KIND_UNSPECIFIED'));
    if (place.parent_id) {
      const a = el('a', '', place.parent_id);
      a.href = hrefFor(place.parent_id);
      row('parent', a);
    } else {
      row('parent', '—');
    }
    const centroid = place.geometry && place.geometry.centroid;
    if (centroid && (centroid.lat !== undefined || centroid.lng !== undefined)) {
      row('centroid', `${centroid.lat ?? 0}, ${centroid.lng ?? 0}`);
    }
    const bb = protoBBox(place.geometry && place.geometry.bbox);
    if (bb) {
      row('bbox', `[${bb[1]}, ${bb[0]}] → [${bb[3]}, ${bb[2]}] (lat, lng)`);
    }
    placeDetailEl.append(kv);

    // Live links — the place page is a jumping-off point into the data.
    const slug = placeRef(place) || ref;
    const links = el('p', 'row');
    const evLink = el('a', '', 'Active events here');
    evLink.href = `/events.html?place=${encodeURIComponent(slug)}`;
    const sumLink = el('a', 'mono', `GET /v1/places/${slug}/summary`);
    sumLink.href = `/v1/places/${encodeURIComponent(slug)}/summary`;
    sumLink.title = 'Raw summary JSON (mode, domains, evacuation status)';
    const mapLink = el('a', '', 'Map layers');
    mapLink.href = `/map.html?place=${encodeURIComponent(slug)}`;
    links.append(evLink, ' · ', sumLink, ' · ', mapLink);
    placeDetailEl.append(links);

    // Geometry → map. Place geometry rides the protojson bytes field
    // (base64) — decode client-side; bbox from the promoted proto field
    // when present, else computed from the decoded geometry.
    const geom = decodeGeometry(place.geometry && place.geometry.geojson);
    if (geom) {
      showGeometry(geom, bb);
    } else {
      placeDetailEl.append(
        el(
          'p',
          'muted small',
          place.geometry && place.geometry.geojson
            ? 'Geometry present but failed to decode as GeoJSON.'
            : 'No geometry recorded for this place.'
        )
      );
    }
  }

  // ---- Resolve tester: GET /v1/places/resolve -----------------------------
  async function runResolve(params, emptyText) {
    resultsEl.textContent = '';
    resultsEl.append(el('div', 'loading', 'Resolving…'));
    const url = apiURL('/v1/places/resolve', params);
    let places;
    try {
      const data = await get('/v1/places/resolve', params);
      places = Array.isArray(data.places) ? data.places : [];
    } catch (err) {
      resultsEl.textContent = '';
      resultsEl.append(errorBlock(err));
      return;
    }
    resultsEl.textContent = '';
    resultsEl.append(el('div', 'small muted mono', `GET ${url}`));
    if (places.length === 0) {
      resultsEl.append(el('div', 'notice', emptyText));
      return;
    }

    const wrap = el('div', 'table-wrap');
    const table = el('table', 'data-table');
    const thead = el('thead');
    const hrow = el('tr');
    for (const h of ['Kind', 'Name', 'Slug', 'Id']) hrow.append(el('th', '', h));
    thead.append(hrow);
    const tbody = el('tbody');
    for (const p of places) {
      const tr = el('tr');
      const kindTd = el('td');
      kindTd.append(kindChip(p.kind || 'UNSPECIFIED'));
      tr.append(kindTd);
      const nameTd = el('td');
      const ref = placeRef(p);
      if (ref) {
        const a = el('a', '', p.name || ref);
        a.href = hrefFor(ref);
        nameTd.append(a);
      } else {
        nameTd.textContent = p.name || '—';
      }
      tr.append(nameTd);
      tr.append(el('td', 'mono', p.slug || '—'));
      tr.append(el('td', 'mono', p.id || '—'));
      tbody.append(tr);
    }
    table.append(thead, tbody);
    wrap.append(table);
    resultsEl.append(wrap);
    resultsEl.append(
      el(
        'p',
        'muted small',
        'Results in API order — the contract is most-specific first ' +
          '(SITE, EVAC_ZONE, TOWN, CORRIDOR, COUNTY, AREA); anything out of ' +
          'order here is a zone-import bug worth reporting.'
      )
    );
  }

  function resolvePoint(lat, lng) {
    state.lat = String(lat);
    state.lng = String(lng);
    state.address = '';
    addressInput.value = '';
    syncURL();
    setMarker(lng, lat);
    runResolve(
      { lat, lng },
      'No places contain this point — outside the imported zone coverage.'
    );
  }

  function resolveAddress(address) {
    state.address = address;
    state.lat = '';
    state.lng = '';
    syncURL();
    runResolve(
      { address },
      'No match for this address — the resolver could not geocode it to a covered place.'
    );
  }

  // ---- Wire up forms -------------------------------------------------------
  document.getElementById('point-form').addEventListener('submit', (e) => {
    e.preventDefault();
    const coord = parseCoord(latInput.value.trim(), lngInput.value.trim());
    if (!coord) {
      resultsEl.textContent = '';
      resultsEl.append(
        el(
          'div',
          'error-block',
          'Invalid coordinates — lat must be in [-90, 90] and lng in [-180, 180].'
        )
      );
      return;
    }
    resolvePoint(coord.lat, coord.lng);
    if (map) map.flyTo({ center: [coord.lng, coord.lat], duration: 0 });
  });

  document.getElementById('address-form').addEventListener('submit', (e) => {
    e.preventDefault();
    const address = addressInput.value.trim();
    if (!address) return;
    resolveAddress(address);
  });

  // ---- Boot ----------------------------------------------------------------
  setupMap();

  if (state.place) {
    loadPlace(state.place);
    mapHintEl.textContent =
      'Geometry of the selected place. Click anywhere to resolve that ' +
      'point to its containing places.';
  } else {
    placeView.hidden = true;
    directoryView.hidden = false;
    loadDirectory();
  }

  // Restore resolve-tester state from the URL (shareable QA links).
  const coord = parseCoord(state.lat, state.lng);
  if (coord) {
    latInput.value = state.lat;
    lngInput.value = state.lng;
    setMarker(coord.lng, coord.lat);
    if (map && !state.place) {
      map.jumpTo({ center: [coord.lng, coord.lat], zoom: 10 });
    }
    runResolve(
      { lat: coord.lat, lng: coord.lng },
      'No places contain this point — outside the imported zone coverage.'
    );
  } else if (state.address) {
    addressInput.value = state.address;
    runResolve(
      { address: state.address },
      'No match for this address — the resolver could not geocode it to a covered place.'
    );
  }
}
