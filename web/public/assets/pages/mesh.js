// pages/mesh.js — /mesh: the MeshCore relay-topology map.
//
// The whole mesh (not a single place): every node from GET /api/v1/events?layer=
// network is a point, colored by role, and each node's resolved relay path
// (telemetry.pathNodes — hops resolved to node pubkeys) becomes edges between
// located nodes. Gateways/observers heard the node; the path is how the advert
// relayed there. Everything is fetched same-origin through api.js.

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

export async function initMeshPage() {
  const errEl = document.getElementById('mesh-errors');
  const statEl = document.getElementById('mesh-stats');

  // 1. fetch every network event (paginate)
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

  // 2. index nodes by pubkey (only located ones can be drawn)
  const nodes = new Map();
  let hopsTotal = 0;
  let hopsResolved = 0;
  for (const ev of events) {
    const n = ev.network || {};
    const pk = (n.publicKey || '').toLowerCase();
    const c = ev.geometry && ev.geometry.centroid;
    const t = n.telemetry || {};
    const pathNodes = (t.pathNodes || []).map((s) => (s || '').toLowerCase());
    for (const hop of t.path || []) hopsTotal++;
    for (const pn of pathNodes) if (pn) hopsResolved++;
    if (!pk || !c || c.lng == null || c.lat == null) continue;
    nodes.set(pk, {
      lng: c.lng, lat: c.lat, role: n.nodeType || 'other', name: n.name || '',
      snr: t.snr, hop: t.hopCount || 0, gw: (t.gateways || []).length, pathNodes,
    });
  }

  // 3. edges from resolved relay chains ([node, ...pathNodes]); break on gaps,
  //    both ends must be located, undirected + deduped.
  const seen = new Set();
  const edgeFeatures = [];
  for (const [pk, node] of nodes) {
    const chain = [pk, ...node.pathNodes];
    for (let i = 0; i < chain.length - 1; i++) {
      const a = chain[i], b = chain[i + 1];
      if (!a || !b || a === b || !nodes.has(a) || !nodes.has(b)) continue;
      const key = a < b ? `${a}|${b}` : `${b}|${a}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const na = nodes.get(a), nb = nodes.get(b);
      edgeFeatures.push({
        type: 'Feature',
        geometry: { type: 'LineString', coordinates: [[na.lng, na.lat], [nb.lng, nb.lat]] },
        properties: {},
      });
    }
  }

  // 4. node features
  const nodeFeatures = [];
  const roleCounts = {};
  for (const [pk, node] of nodes) {
    roleCounts[node.role] = (roleCounts[node.role] || 0) + 1;
    nodeFeatures.push({
      type: 'Feature',
      geometry: { type: 'Point', coordinates: [node.lng, node.lat] },
      properties: { pubkey: pk, role: node.role, name: node.name, snr: node.snr, hop: node.hop, gw: node.gw },
    });
  }

  // 5. stats line + legend
  statEl.textContent = '';
  const pct = hopsTotal ? Math.round((100 * hopsResolved) / hopsTotal) : 0;
  statEl.append(
    el('strong', '', `${nodes.size} located nodes`),
    el('span', 'muted', ` · ${edgeFeatures.length} relay links · ${pct}% of relay hops resolved to a mapped node`)
  );
  const legend = document.getElementById('mesh-legend');
  for (const key of ['repeater', 'room_server', 'companion', 'sensor']) {
    if (!roleCounts[key]) continue;
    const row = el('span', 'mesh-leg');
    const dot = el('span', 'mesh-dot');
    dot.style.background = ROLE[key].color;
    row.append(dot, document.createTextNode(`${ROLE[key].label} (${roleCounts[key]})`));
    legend.append(row);
  }

  // 6. map
  const map = new maplibregl.Map({ container: 'mesh-canvas', style: BASE_STYLE, center: DEFAULT_CENTER, zoom: DEFAULT_ZOOM });
  map.addControl(new maplibregl.NavigationControl(), 'top-right');

  map.on('load', () => {
    map.addSource('mesh-edges', { type: 'geojson', data: { type: 'FeatureCollection', features: edgeFeatures } });
    map.addLayer({
      id: 'mesh-edges', type: 'line', source: 'mesh-edges',
      paint: { 'line-color': '#6aa9c9', 'line-width': 1, 'line-opacity': 0.35 },
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
      const b = new maplibregl.LngLatBounds();
      for (const f of nodeFeatures) b.extend(f.geometry.coordinates);
      map.fitBounds(b, { padding: 44, maxZoom: 11 });
    }

    map.on('mouseenter', 'mesh-nodes', () => (map.getCanvas().style.cursor = 'pointer'));
    map.on('mouseleave', 'mesh-nodes', () => (map.getCanvas().style.cursor = ''));
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
  });
}
