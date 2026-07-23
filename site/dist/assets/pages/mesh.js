// pages/mesh.js — /mesh: the MeshCore relay-topology map.
//
// Nodes come from GET /api/v1/events?layer=network (each located node is a point,
// colored by role). Links come from GET /api/v1/mesh/links?window=… — the DURABLE
// server-derived topology (an advert's relay path, rolled up over time), not a
// client-side reconstruction. Each link fades by recency (lastSeen) and is
// weighted by how often it was observed, so a shaky, intermittent mesh reads
// honestly: a quiet-but-recent link is dim, not absent. A window selector trades
// currency for history. Everything is fetched same-origin through api.js.

import { get, ApiError } from '../api.js';
import { BASE_STYLE } from '../basemap.js';

const ROLE = {
  repeater: { color: '#4bbd82', label: 'Repeater', r: 5 },
  room_server: { color: '#e0a33c', label: 'Room server', r: 5 },
  companion: { color: '#5aa0e0', label: 'Companion', r: 3.5 },
  sensor: { color: '#c76ad0', label: 'Sensor', r: 4 },
};
const UNKNOWN = { color: '#8a8f94', label: 'Other', r: 3.5 };
const DEFAULT_CENTER = [-121.7, 37.9]; // Bay Area → Sierra
const DEFAULT_ZOOM = 6.5;

// Window presets: the label trades currency (Live) for history (All). Values are
// Go durations the /mesh/links endpoint parses + clamps.
const WINDOWS = [
  { key: '72h', label: 'Live · 72h' },
  { key: '168h', label: '7 days' },
  { key: '720h', label: '30 days' },
  { key: '8760h', label: 'All' },
];

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
function errorBlock(err) {
  const div = el('div', 'error-block');
  div.append(
    el('div', '', err instanceof ApiError ? `Request failed (HTTP ${err.status || 'network error'}):` : 'Request failed:'),
    el('div', 'error-url', err instanceof ApiError ? `GET ${err.url}` : String(err.message || err))
  );
  return div;
}
const roleOf = (r) => ROLE[r] || UNKNOWN;

// Recency fade + weight, computed per link so MapLibre can key paint on them.
const opacityForAge = (ageH) => Math.max(0.07, 0.6 * Math.exp(-ageH / 72));
const widthForObs = (n) => Math.min(4, 0.6 + Math.log10(1 + (n || 0)));
function relAge(ms) {
  if (!isFinite(ms)) return 'unknown';
  const h = ms / 3.6e6;
  if (h < 1) return `${Math.round(h * 60)}m ago`;
  if (h < 48) return `${Math.round(h)}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

export async function initMeshPage() {
  const errEl = document.getElementById('mesh-errors');
  const statEl = document.getElementById('mesh-stats');

  // 1. fetch every network event (paginate) → located node index.
  let events = [];
  try {
    let tok = '';
    for (let i = 0; i < 20; i++) {
      const d = await get('/api/v1/events', { layer: 'network', page_size: 200, page_token: tok });
      events = events.concat(Array.isArray(d.events) ? d.events : []);
      tok = d.nextPageToken || '';
      if (!tok) break;
    }
  } catch (err) {
    errEl.append(errorBlock(err));
    statEl.textContent = 'Could not load the mesh.';
    return;
  }

  const nodes = new Map();
  const roleCounts = {};
  const nodeFeatures = [];
  for (const ev of events) {
    const n = ev.network || {};
    const pk = (n.publicKey || '').toLowerCase();
    const c = ev.geometry && ev.geometry.centroid;
    if (!pk || !c || c.lng == null || c.lat == null) continue;
    const t = n.telemetry || {};
    const role = n.nodeType || 'other';
    nodes.set(pk, { lng: c.lng, lat: c.lat, role, name: n.name || '', snr: t.snr, hop: t.hopCount || 0, gw: (t.gateways || []).length });
    roleCounts[role] = (roleCounts[role] || 0) + 1;
    nodeFeatures.push({
      type: 'Feature',
      geometry: { type: 'Point', coordinates: [c.lng, c.lat] },
      properties: { pubkey: pk, role, name: n.name || '', snr: t.snr, hop: t.hopCount || 0, gw: (t.gateways || []).length },
    });
  }

  // 2. legend + window selector.
  const legend = document.getElementById('mesh-legend');
  for (const key of ['repeater', 'room_server', 'companion', 'sensor']) {
    if (!roleCounts[key]) continue;
    const row = el('span', 'mesh-leg');
    const dot = el('span', 'mesh-dot');
    dot.style.background = ROLE[key].color;
    row.append(dot, document.createTextNode(`${ROLE[key].label} (${roleCounts[key]})`));
    legend.append(row);
  }

  let currentWindow = WINDOWS[0].key;
  const winEl = document.getElementById('mesh-window');
  const winButtons = [];
  for (const wdef of WINDOWS) {
    const b = el('button', 'mesh-win-btn', wdef.label);
    if (wdef.key === currentWindow) b.classList.add('active');
    b.addEventListener('click', () => {
      if (currentWindow === wdef.key) return;
      currentWindow = wdef.key;
      for (const wb of winButtons) wb.el.classList.toggle('active', wb.key === wdef.key);
      loadLinks();
    });
    winButtons.push({ key: wdef.key, el: b });
    winEl.append(b);
  }

  // 3. map + layers.
  const map = new maplibregl.Map({ container: 'mesh-canvas', style: BASE_STYLE, center: DEFAULT_CENTER, zoom: DEFAULT_ZOOM });
  map.addControl(new maplibregl.NavigationControl(), 'top-right');

  async function loadLinks() {
    let links = [];
    try {
      const d = await get('/api/v1/mesh/links', { window: currentWindow });
      links = Array.isArray(d.links) ? d.links : [];
    } catch (err) {
      errEl.replaceChildren(errorBlock(err));
      return;
    }
    errEl.replaceChildren();
    const now = Date.now();
    const feats = [];
    for (const lk of links) {
      const a = nodes.get((lk.a || '').toLowerCase());
      const b = nodes.get((lk.b || '').toLowerCase());
      if (!a || !b) continue; // an endpoint we haven't located — can't draw it
      const ageH = lk.lastSeen ? (now - Date.parse(lk.lastSeen)) / 3.6e6 : 1e9;
      feats.push({
        type: 'Feature',
        geometry: { type: 'LineString', coordinates: [[a.lng, a.lat], [b.lng, b.lat]] },
        properties: {
          a: lk.a, b: lk.b, observations: lk.observations, daysActive: lk.daysActive,
          lastSeen: lk.lastSeen, bestSnr: lk.bestSnr,
          op: opacityForAge(ageH), w: widthForObs(lk.observations),
        },
      });
    }
    const src = map.getSource('mesh-edges');
    if (src) src.setData({ type: 'FeatureCollection', features: feats });

    statEl.replaceChildren(
      el('strong', '', `${nodes.size} located nodes`),
      el('span', 'muted', ` · ${feats.length} of ${links.length} relay links drawn · faded by recency, weighted by traffic`)
    );
  }

  map.on('load', () => {
    map.addSource('mesh-edges', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } });
    map.addLayer({
      id: 'mesh-edges', type: 'line', source: 'mesh-edges',
      layout: { 'line-cap': 'round' },
      paint: { 'line-color': '#6aa9c9', 'line-width': ['get', 'w'], 'line-opacity': ['get', 'op'] },
    });

    map.addSource('mesh-nodes', { type: 'geojson', data: { type: 'FeatureCollection', features: nodeFeatures } });
    map.addLayer({
      id: 'mesh-nodes', type: 'circle', source: 'mesh-nodes',
      paint: {
        'circle-radius': ['match', ['get', 'role'], 'repeater', 5, 'room_server', 5, 'sensor', 4, 3.5],
        'circle-color': ['match', ['get', 'role'],
          'repeater', ROLE.repeater.color, 'room_server', ROLE.room_server.color,
          'companion', ROLE.companion.color, 'sensor', ROLE.sensor.color, UNKNOWN.color],
        'circle-stroke-color': '#0b0f12', 'circle-stroke-width': 1, 'circle-opacity': 0.95,
      },
    });

    if (nodeFeatures.length) {
      const bnds = new maplibregl.LngLatBounds();
      for (const f of nodeFeatures) bnds.extend(f.geometry.coordinates);
      map.fitBounds(bnds, { padding: 44, maxZoom: 11 });
    }

    map.on('mouseenter', 'mesh-nodes', () => (map.getCanvas().style.cursor = 'pointer'));
    map.on('mouseleave', 'mesh-nodes', () => (map.getCanvas().style.cursor = ''));
    map.on('mouseenter', 'mesh-edges', () => (map.getCanvas().style.cursor = 'pointer'));
    map.on('mouseleave', 'mesh-edges', () => (map.getCanvas().style.cursor = ''));

    map.on('click', 'mesh-nodes', (e) => {
      const p = (e.features && e.features[0] && e.features[0].properties) || {};
      const box = el('div', 'map-popup');
      box.append(el('div', 'popup-headline', p.name || (p.pubkey || '').slice(0, 12) + '…'));
      const meta = roleOf(p.role);
      const chips = el('div', 'popup-chips');
      const tag = el('span', 'mesh-roletag');
      tag.style.color = meta.color;
      tag.textContent = meta.label;
      chips.append(tag);
      box.append(chips);
      const dl = el('dl', 'popup-details-dl');
      const add = (k, v) => { if (v !== undefined && v !== null && v !== '') { dl.append(el('dt', '', k), el('dd', '', String(v))); } };
      if (p.snr !== undefined && p.snr !== 0) add('SNR', p.snr + ' dB');
      add('hops', p.hop);
      add('gateways', p.gw);
      add('pubkey', (p.pubkey || '').slice(0, 16) + '…');
      box.append(dl);
      new maplibregl.Popup({ maxWidth: '260px' }).setLngLat(e.lngLat).setDOMContent(box).addTo(map);
    });

    map.on('click', 'mesh-edges', (e) => {
      const p = (e.features && e.features[0] && e.features[0].properties) || {};
      const box = el('div', 'map-popup');
      box.append(el('div', 'popup-headline', 'Relay link'));
      const dl = el('dl', 'popup-details-dl');
      const add = (k, v) => { if (v !== undefined && v !== null && v !== '') { dl.append(el('dt', '', k), el('dd', '', String(v))); } };
      add('a', (p.a || '').slice(0, 12) + '…');
      add('b', (p.b || '').slice(0, 12) + '…');
      add('observations', p.observations);
      add('days active', p.daysActive);
      if (p.bestSnr !== undefined && p.bestSnr !== 0) add('best SNR', p.bestSnr + ' dB');
      add('last seen', relAge(Date.now() - Date.parse(p.lastSeen)));
      box.append(dl);
      new maplibregl.Popup({ maxWidth: '260px' }).setLngLat(e.lngLat).setDOMContent(box).addTo(map);
    });

    loadLinks();
  });
}
