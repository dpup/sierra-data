// pages/mesh.js — /mesh: the MeshCore relay-topology map.
//
// Nodes come from GET /api/v1/events?layer=mesh (each located node is a point,
// colored by role). Links come from GET /api/v1/mesh/links?window=… — the DURABLE
// server-derived topology (an advert's relay path, rolled up over time), not a
// client-side reconstruction. Each link fades by recency (lastSeen) and is
// weighted by how often it was observed, so a shaky, intermittent mesh reads
// honestly: a quiet-but-recent link is dim, not absent. A window selector trades
// currency for history. Everything is fetched same-origin through api.js.

import { get, ApiError } from '../api.js';
import { timeCell, absentValue } from '../format.js';
import { BASE_STYLE, BASE_ATTRIBUTION_OPTS, ensureBasemap, deferInteraction } from '../basemap.js';

const ROLE = {
  repeater: { color: '#4bbd82', label: 'Repeater', r: 5 },
  room_server: { color: '#e0a33c', label: 'Room server', r: 5 },
  companion: { color: '#5aa0e0', label: 'Companion', r: 3.5 },
  sensor: { color: '#c76ad0', label: 'Sensor', r: 4 },
};
const UNKNOWN = { color: '#8a8f94', label: 'Other', r: 3.5 };
// The ring a node gets when an operator's monitor cannot reach it. Canvas paint
// cannot read a custom property, so this is --st-UNAVAILABLE from the PAPER
// ramp, written out — the basemap is light (see basemap.js). Fill stays the
// node's role colour: the two encodings are independent and must read that way.
const UNREACHABLE_RING = '#b3261e';
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

  // 1. fetch every mesh-node event (paginate) → located node index.
  let events = [];
  try {
    let tok = '';
    for (let i = 0; i < 20; i++) {
      const d = await get('/api/v1/events', { layer: 'mesh', page_size: 200, page_token: tok });
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
  let unreachableCount = 0;
  for (const ev of events) {
    const n = ev.mesh || {};
    const pk = (n.publicKey || '').toLowerCase();
    const c = ev.geometry && ev.geometry.centroid;
    if (!pk || !c || c.lng == null || c.lat == null) continue;
    const t = n.telemetry || {};
    const role = n.nodeType || 'other';
    // Three states, not two: REACHABLE, UNREACHABLE, and "nobody is watching",
    // which is the default and must never be painted as either of the others.
    const reach = String(n.reachability || '').toUpperCase();
    const monitored = reach === 'REACHABLE' || reach === 'UNREACHABLE';
    if (reach === 'UNREACHABLE') unreachableCount++;
    // hopCount is NOT defaulted to 0 here: absent telemetry must stay absent so
    // the roster can render "—". (The map's own paint still treats it as 0.)
    nodes.set(pk, { lng: c.lng, lat: c.lat, role, name: n.name || '', snr: t.snr, hop: t.hopCount, gw: (t.gateways || []).length, reach, admin: t.admin || null, ev, telemetry: t });
    roleCounts[role] = (roleCounts[role] || 0) + 1;
    nodeFeatures.push({
      type: 'Feature',
      geometry: { type: 'Point', coordinates: [c.lng, c.lat] },
      properties: {
        // `?? null`, not `|| 0`: hop_count is a wrapper on the wire now, and 0
        // hops (heard direct) is a real reading that must not share a rendering
        // with "no bridge has heard this node".
        pubkey: pk, role, name: n.name || '', snr: t.snr ?? null, hop: t.hopCount ?? null,
        gw: (t.gateways || []).length,
        // '' rather than a missing key: a MapLibre `get` on an absent property
        // is null, and the match below would then have to spell both cases.
        reach: monitored ? reach : '',
        gauges: gaugeSummary(t.admin),
      },
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

  // The ring is a second encoding on the same dot, so it needs its own key —
  // and only when something on the map actually wears it. A legend entry for a
  // state no node is in reads as a state we are failing to detect.
  if (unreachableCount) {
    const row = el('span', 'mesh-leg');
    const ring = el('span', 'mesh-dot');
    ring.style.background = 'transparent';
    ring.style.borderColor = UNREACHABLE_RING;
    ring.style.borderWidth = '2px';
    row.append(ring, document.createTextNode(`Monitor can’t reach (${unreachableCount})`));
    legend.append(row);
  }

  renderNodeTable(nodes);

  let currentWindow = WINDOWS[0].key;
  const winEl = document.getElementById('mesh-window');
  const winButtons = [];
  // Single-select chip group — the shared .pill control (Events severity_min
  // uses the same radio behaviour). `.on` is the visual state, aria-pressed the
  // announced one.
  for (const wdef of WINDOWS) {
    const on = wdef.key === currentWindow;
    const b = el('button', 'pill' + (on ? ' on' : ''), wdef.label);
    b.type = 'button';
    b.dataset.value = wdef.key;
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
    b.addEventListener('click', () => {
      if (currentWindow === wdef.key) return;
      currentWindow = wdef.key;
      for (const wb of winButtons) {
        const lit = wb.key === wdef.key;
        wb.el.classList.toggle('on', lit);
        wb.el.setAttribute('aria-pressed', lit ? 'true' : 'false');
      }
      loadLinks();
    });
    winButtons.push({ key: wdef.key, el: b });
    winEl.append(b);
  }

  // 3. map + layers.
  const map = new maplibregl.Map({
    container: 'mesh-canvas',
    style: BASE_STYLE,
    center: DEFAULT_CENTER,
    zoom: DEFAULT_ZOOM,
    // Credit comes from the TileJSON; this only makes it compact.
    attributionControl: BASE_ATTRIBUTION_OPTS,
  });
  ensureBasemap(map);
  deferInteraction(map, document.getElementById('mesh-canvas'));
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

  map.on('style.load', () => {
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
        // Fill is role, ring is reachability. An unwatched node keeps the plain
        // ink ring: "nobody is watching" is not a failing node, and painting it
        // as one would make the mesh look broken wherever we simply have no
        // monitor. Never colour alone — the popup says the word too.
        'circle-stroke-color': ['case', ['==', ['get', 'reach'], 'UNREACHABLE'], UNREACHABLE_RING, '#0b0f12'],
        'circle-stroke-width': ['case', ['==', ['get', 'reach'], 'UNREACHABLE'], 2, 1],
        'circle-opacity': 0.95,
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
      // Absent is absent and 0 dB is a reading — the guard used to drop both,
      // which was right for the unheard node and silently wrong for a marginal one.
      if (p.snr !== undefined && p.snr !== null) add('SNR', p.snr + ' dB');
      add('hops', p.hop === undefined || p.hop === null ? null : hopCell(p.hop));
      add('gateways', p.gw);
      // Only where a monitor watches this node. An unwatched node says nothing
      // here rather than "unknown" — the roster's Monitor column is where the
      // absence is named, and a popup that names every absence is a wall.
      add('monitor', p.reach || null);
      add('readings', p.gauges || null);
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

/**
 * The node roster beneath the map.
 *
 * Missing telemetry renders as an em dash, NEVER as 0: a node we have not heard
 * a SNR from is not a node with 0 dB SNR, and -0 dBm is not "no signal". The
 * fail-loud rule applies to ambient data too, even though nothing here is
 * life-safety — a habit of writing 0 for unknown is how a 0 ends up somewhere
 * that matters.
 *
 * "Heard" is the event's observedAt, not telemetry.lastAdvertAt: the latter is
 * stamped by the node's own unsynchronized clock and can be days out.
 *
 * @param {Map<string, Object>} nodes keyed by public key
 */
/**
 * Hop count with its unit: "0 hops", "1 hop", "8 hops" — and "—" when the node
 * reported none. Absent is not zero: zero hops is a real reading (heard
 * direct), and the two must not collapse into one rendering.
 * @param {number|undefined|null} hop
 * @returns {string}
 */
/**
 * The monitor's headline readings on one line: "4.14 V · 97% · 37 °C".
 *
 * Only what it actually read. The gauges are wrapper types on the wire, so an
 * unread one arrives as null and is simply left out — the one thing this must
 * never do is turn a battery nobody could read into a 0 V battery. Returns ''
 * when there is nothing to say, which every caller renders as nothing.
 *
 * @param {Object|null|undefined} admin MeshTelemetry.admin (protojson)
 * @returns {string}
 */
export function gaugeSummary(admin) {
  if (!admin) return '';
  const num = (v) => (v === null || v === undefined || v === '' ? NaN : Number(v));
  const volts = num(admin.batteryVolts);
  const pct = num(admin.batteryPercent);
  const temp = num(admin.temperatureC);
  const bits = [];
  if (Number.isFinite(volts)) bits.push(`${volts.toFixed(2)} V`);
  if (Number.isFinite(pct)) bits.push(`${Math.round(pct)}%`);
  if (Number.isFinite(temp)) bits.push(`${Math.round(temp * 10) / 10} °C`);
  return bits.join(' · ');
}

/**
 * The Monitor column: what an operator-run monitor says about this node.
 *
 * Three states, and the third is the point. A mesh has no goodbye packet, so a
 * node going quiet is ambiguous and everything downstream has to treat it that
 * way — but a monitor that logged in and FAILED is not ambiguous, and that is
 * the one fact this column exists to carry. Which means "nobody is watching
 * this node" can never render like "we tried and could not": most of the mesh
 * is unwatched, and a column that blurred the two would make the honest state
 * (unknown) look like the alarming one, or the reverse.
 *
 * The battery and temperature ride underneath as the sub-line rather than in
 * columns of their own: they exist for a handful of repeaters, and four mostly
 * empty columns is not a table.
 */
function monitorCell(n) {
  const td = el('td');
  if (n.reach !== 'REACHABLE' && n.reach !== 'UNREACHABLE') {
    td.append(absentValue({ text: 'not monitored', cls: 'absent-empty' }));
    return td;
  }
  const ok = n.reach === 'REACHABLE';
  const wrap = el('span', `dot-status ${ok ? 'st-OK' : 'st-UNAVAILABLE'}`);
  const dot = el('span', 'dot');
  dot.setAttribute('aria-hidden', 'true');
  wrap.append(dot, el('span', '', n.reach));
  td.append(wrap);
  // A reachable node whose sample carried no gauge is a real case (a repeater
  // with no sensors), and it is not the same as an unread one.
  td.append(el('div', 'cell-sub', ok ? gaugeSummary(n.admin) || 'reached, no readings' : 'monitor cannot reach it'));
  return td;
}

export function hopCell(hop) {
  if (hop === undefined || hop === null || hop === '' || Number.isNaN(Number(hop))) return '—';
  const n = Number(hop);
  return `${n} ${n === 1 ? 'hop' : 'hops'}`;
}

function renderNodeTable(nodes) {
  const tbody = document.getElementById('mesh-tbody');
  if (!tbody) return;
  tbody.textContent = '';

  const rows = [...nodes.entries()].sort((a, b) => {
    const ta = Date.parse((a[1].ev && a[1].ev.observedAt) || 0) || 0;
    const tb = Date.parse((b[1].ev && b[1].ev.observedAt) || 0) || 0;
    return tb - ta; // most recently heard first
  });

  if (!rows.length) {
    const tr = el('tr');
    const td = el('td');
    td.colSpan = 6;
    td.append(
      el('div', 'mono', 'No located nodes in this window.'),
      el('div', 'muted small',
        'The query succeeded and returned no node with a known location. A node is only ' +
        'listed once we have heard it advertise one — a short list is not a claim that ' +
        'the mesh is small.')
    );
    tr.append(td);
    tbody.append(tr);
    return;
  }

  // Telemetry may be absent, and 0 is a legitimate reading for SNR — so test
  // for null/undefined explicitly rather than falsiness.
  const num = (v, unit) => (v === undefined || v === null ? '—' : `${v}${unit || ''}`);

  for (const [pk, n] of rows) {
    const tr = el('tr');

    const nameCell = el('td');
    // The roster is a summary; the record is the whole thing — every gauge,
    // every counter, the gateway list, the revision history. Until this link
    // existed there was no path from one to the other.
    const name = el(n.ev && n.ev.id ? 'a' : 'div', 'cell-name', n.name || '(unnamed node)');
    if (n.ev && n.ev.id) name.href = `/event?id=${encodeURIComponent(n.ev.id)}`;
    nameCell.append(name, el('div', 'cell-sub', pk.length > 16 ? `${pk.slice(0, 16)}…` : pk));
    tr.append(nameCell);

    const roleDef = ROLE[n.role];
    const typeCell = el('td');
    const dot = el('span', 'mesh-dot');
    dot.style.background = roleDef ? roleDef.color : 'var(--text-tertiary)';
    dot.style.marginRight = '7px';
    typeCell.append(dot, document.createTextNode(roleDef ? roleDef.label : n.role || 'unknown'));
    tr.append(typeCell);

    tr.append(monitorCell(n));

    const t = n.telemetry || {};
    tr.append(el('td', undefined, `${num(t.snr, ' dB')} / ${num(t.rssi, ' dBm')}`));
    // The unit rides in the cell. Every other column carries its own — "9 dB",
    // "5 min ago" — and with the header row dropped (the section cap is the
    // legend) a bare "2" was the one value you had to look up to read.
    tr.append(el('td', 'num', hopCell(n.hop)));

    const heard = (n.ev && n.ev.observedAt) || '';
    const heardCell = el('td');
    heardCell.append(timeCell(heard));
    tr.append(heardCell);

    ['Node', 'Type', 'Monitor', 'SNR / RSSI', 'Hops', 'Heard'].forEach((lbl, i) => {
      if (tr.children[i]) tr.children[i].dataset.label = lbl;
    });
    tbody.append(tr);
  }
}
