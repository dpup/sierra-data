// pages/roads.js — /roads: the road-conditions view.
//
// Roads surface through three layers the API projects for a place, each a live
// GeoJSON FeatureCollection from GET /api/v1/places/{place}/map/{layer}.geojson:
//   road_segment  — per-road travel time, delay, congestion, status (Google Routes + Caltrans)
//   chain_control — active chain controls, R1/R2/R3 (Caltrans)
//   road_incident — CHP/Caltrans incidents, AI-enhanced (projected events)
//
// This page renders each layer's per-kind properties — the data the generic map
// popup drops — and each layer's metadata.sourceStatus honestly: an UNAVAILABLE
// layer is shown loud, an empty OK layer reads as "a clean report, not an
// all-clear". State is just the selected place, kept in the URL as a permalink.

import { get, curlFor, ApiError } from '../api.js';
import { sevChip, sourceDot, timeAgo, timeAbs } from '../format.js';

const DEFAULT_PLACE = 'ebbetts-pass';

const SECTIONS = [
  { layer: 'road_segment', status: 'rd-seg-status', content: 'rd-seg', render: renderSegments,
    empty: { head: 'No monitored roads in this area.', sub: 'This area has no configured road segments.' } },
  { layer: 'chain_control', status: 'rd-chain-status', content: 'rd-chain', render: renderChain,
    empty: { head: 'No chain controls in effect.', sub: 'Caltrans reports no active chain controls right now — a clean report, not a source failure.' } },
  { layer: 'road_incident', status: 'rd-inc-status', content: 'rd-inc', render: renderIncidents,
    empty: { head: 'No road incidents reported.', sub: 'No active CHP/Caltrans incidents on these corridors. Not an all-clear — a failed feed shows as UNAVAILABLE above, never as a quiet empty list.' } },
];

/* ---------- small DOM helpers ---------- */

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
  if (err instanceof ApiError && err.body && typeof err.body === 'object' && err.body.message) {
    div.append(el('div', 'muted', err.body.message));
  }
  return div;
}

function curlLine(path) {
  const wrap = el('div', 'rd-curl');
  wrap.append(el('code', '', curlFor(path)));
  const btn = el('button', 'rd-copy', 'copy');
  btn.type = 'button';
  btn.addEventListener('click', () => {
    navigator.clipboard.writeText(curlFor(path)).then(
      () => { btn.textContent = 'copied'; setTimeout(() => (btn.textContent = 'copy'), 1200); },
      () => { btn.textContent = 'failed'; }
    );
  });
  wrap.append(btn);
  return wrap;
}

/** Enum-ish string → readable ("HEAVY" → "Heavy", "SHELTER_IN_PLACE" → "Shelter in place"). */
function pretty(s) {
  if (!s) return '';
  const t = String(s).replace(/_/g, ' ').toLowerCase();
  return t.charAt(0).toUpperCase() + t.slice(1);
}

/** "12 min" / "—" for a possibly-absent numeric property. */
function num(v, suffix) {
  return v === undefined || v === null ? '—' : `${v}${suffix}`;
}

function statusTd(status) {
  const td = el('td');
  if (!status) { td.textContent = '—'; return td; }
  const cls = { OPEN: 'ok', RESTRICTED: 'warn', CLOSED: 'bad' }[status] || 'unknown';
  td.append(el('span', 'rd-badge rd-' + cls, pretty(status)));
  return td;
}

function delayTd(delay) {
  const td = el('td', 'num');
  if (delay === undefined || delay === null) td.textContent = '—';
  else if (delay <= 0) { td.textContent = 'on time'; td.classList.add('muted'); }
  else { td.textContent = `+${delay} min`; td.classList.add('rd-delay'); }
  return td;
}

function chainBadge(level) {
  const cls = { R1: 'warn', R2: 'warn2', R3: 'bad' }[level] || 'unknown';
  return el('span', 'rd-badge rd-' + cls, level || '—');
}

/* ---------- per-layer renderers ---------- */

// On phones each row reflows into a stacked card (roads.astro ≤640px); caption
// every cell by its column via data-label so travel/delay/distance stay visible.
function labelRow(tr, labels) {
  tr.querySelectorAll(':scope > td').forEach((td, i) => {
    if (labels[i]) td.dataset.label = labels[i];
  });
}

function scrollTable(headers) {
  const wrap = el('div', 'rd-scroll');
  const table = el('table', 'rd');
  const thead = el('thead');
  const htr = el('tr');
  for (const h of headers) htr.append(el('th', '', h));
  thead.append(htr);
  const tbody = el('tbody');
  table.append(thead, tbody);
  wrap.append(table);
  return { wrap, tbody };
}

function renderSegments(container, feats) {
  feats.sort((a, b) =>
    (b.properties.severityRank || 0) - (a.properties.severityRank || 0) ||
    String(a.properties.headline || '').localeCompare(String(b.properties.headline || ''))
  );
  const { wrap, tbody } = scrollTable(['', 'Road', 'Status', 'Congestion', 'Travel', 'Delay', 'Distance']);
  for (const f of feats) {
    const p = f.properties || {};
    const r = p.road || {};
    const tr = el('tr');
    const sevTd = el('td'); sevTd.append(sevChip(p.severity)); tr.append(sevTd);

    const roadTd = el('td', 'rd-wrap');
    roadTd.append(el('div', 'rd-road', p.headline || r.roadId || '—'));
    // The AI status explanation only rides along when a road isn't fully open.
    if (p.description && p.status && p.status !== 'OPEN') {
      roadTd.append(el('div', 'rd-explain muted small', p.description));
    }
    tr.append(roadTd);

    tr.append(statusTd(p.status));
    tr.append(el('td', '', r.congestion ? pretty(r.congestion) : '—'));
    tr.append(el('td', 'num', num(r.durationMinutes, ' min')));
    tr.append(delayTd(r.delayMinutes));
    tr.append(el('td', 'num', num(r.distanceKm, ' km')));
    labelRow(tr, ['', 'Road', 'Status', 'Congestion', 'Travel', 'Delay', 'Distance']);
    tbody.append(tr);
  }
  container.append(wrap);
}

function renderChain(container, feats) {
  const order = { R3: 3, R2: 2, R1: 1 };
  feats.sort((a, b) => (order[b.properties.chainControl?.level] || 0) - (order[a.properties.chainControl?.level] || 0));
  const { wrap, tbody } = scrollTable(['Level', 'Highway', 'Direction', 'Note']);
  for (const f of feats) {
    const p = f.properties || {};
    const c = p.chainControl || {};
    const tr = el('tr');
    const lvlTd = el('td'); lvlTd.append(chainBadge(c.level)); tr.append(lvlTd);
    tr.append(el('td', '', c.highway || '—'));
    tr.append(el('td', '', c.direction || '—'));
    tr.append(el('td', 'rd-wrap muted', p.headline || '—'));
    labelRow(tr, ['Level', 'Highway', 'Direction', 'Note']);
    tbody.append(tr);
  }
  container.append(wrap);
}

function renderIncidents(container, feats) {
  feats.sort((a, b) =>
    (b.properties.severityRank || 0) - (a.properties.severityRank || 0) ||
    String(b.properties.updatedAt || '').localeCompare(String(a.properties.updatedAt || ''))
  );
  const list = el('div', 'rd-incidents');
  for (const f of feats) {
    const p = f.properties || {};
    const card = el('div', 'rd-incident');
    const head = el('div', 'rd-inc-head');
    head.append(sevChip(p.severity));
    head.append(el('span', 'rd-inc-headline', p.headline || p.id || '(incident)'));
    card.append(head);
    const body = p.summary || p.description;
    if (body) card.append(el('div', 'rd-inc-body muted small', body));
    const meta = el('div', 'rd-inc-meta muted small');
    const bits = [];
    if (p.incident && p.incident.logNumber) bits.push('log ' + p.incident.logNumber);
    if (p.updatedAt) bits.push('updated ' + timeAgo(p.updatedAt));
    if (p.source && p.source.name) bits.push(p.source.name);
    meta.textContent = bits.join(' · ') || '—';
    if (p.updatedAt) meta.title = timeAbs(p.updatedAt);
    card.append(meta);
    list.append(card);
  }
  container.append(list);
}

/* ---------- section loading (fail-loud) ---------- */

function statusHeader(statusEl, path, fc, err) {
  statusEl.textContent = '';
  if (err) {
    statusEl.append(sourceDot('UNAVAILABLE'), el('span', 'muted small', 'request failed'), curlLine(path));
    return;
  }
  const md = (fc && fc.metadata) || null;
  const status = md ? String(md.sourceStatus || '').toUpperCase() : 'UNKNOWN';
  statusEl.append(sourceDot(status || 'UNKNOWN'));
  if (md && md.generatedAt) {
    const g = el('span', 'muted small', 'generated ' + timeAgo(md.generatedAt));
    g.title = timeAbs(md.generatedAt);
    statusEl.append(g);
  }
  if (md && md.attribution) statusEl.append(el('span', 'muted small', '· ' + md.attribution));
  statusEl.append(curlLine(path));
}

function unavailableNotice(md) {
  const n = el('div', 'notice notice-bad');
  n.append(el('div', 'mono', 'Source UNAVAILABLE — this is not an all-clear.'));
  n.append(el('div', 'muted small',
    'The upstream feed failed, so the Grid returns no features rather than a fabricated clear state (metadata.sourceStatus = UNAVAILABLE). Check the official source directly.'));
  const url = md && md.sourceUrl;
  if (url && /^https?:\/\//.test(url)) {
    const a = el('a', 'small', 'official source ↗');
    a.href = url; a.target = '_blank'; a.rel = 'noopener noreferrer';
    n.append(a);
  }
  return n;
}

function emptyNotice(empty, stale) {
  const n = el('div', 'notice');
  n.append(el('div', 'mono', empty.head));
  n.append(el('div', 'muted small', empty.sub));
  if (stale) n.append(el('div', 'meta-stale small', 'Serving last-good cached data (source is STALE).'));
  return n;
}

async function loadSection(place, section) {
  const statusEl = document.getElementById(section.status);
  const contentEl = document.getElementById(section.content);
  const path = `/api/v1/places/${encodeURIComponent(place)}/map/${section.layer}.geojson`;
  statusEl.textContent = 'loading…';
  contentEl.textContent = '';

  let fc, err;
  try { fc = await get(path); } catch (e) { err = e; }
  statusHeader(statusEl, path, fc, err);
  if (err) { contentEl.append(errorBlock(err)); return; }

  const md = fc.metadata || {};
  const status = String(md.sourceStatus || '').toUpperCase();
  const feats = Array.isArray(fc.features) ? fc.features : [];

  if (status === 'UNAVAILABLE') { contentEl.append(unavailableNotice(md)); return; }
  if (feats.length === 0) { contentEl.append(emptyNotice(section.empty, status === 'STALE')); return; }
  if (status === 'STALE') {
    const s = el('div', 'meta-stale small', 'Source STALE — showing last-good cached data.');
    contentEl.append(s);
  }
  section.render(contentEl, feats);
}

/* ---------- init ---------- */

export function initRoadsPage() {
  const placeSel = document.getElementById('rd-place');
  let place = new URLSearchParams(location.search).get('place') || DEFAULT_PLACE;

  // Seed the select with the current place so a shared ?place= link works even
  // before /api/v1/places resolves.
  placeSel.textContent = '';
  const seed = el('option', '', place);
  seed.value = place;
  placeSel.append(seed);
  placeSel.value = place;

  function writeURL() {
    const qs = place && place !== DEFAULT_PLACE ? `?place=${encodeURIComponent(place)}` : '';
    history.replaceState(null, '', qs || location.pathname);
  }

  function loadAll() {
    for (const s of SECTIONS) loadSection(place, s);
  }

  async function loadPlaces() {
    try {
      const data = await get('/api/v1/places', { kind: 'AREA' });
      const places = (Array.isArray(data.places) ? data.places : [])
        .slice()
        .sort((a, b) => String(a.name || a.slug).localeCompare(String(b.name || b.slug)));
      placeSel.textContent = '';
      let found = false;
      for (const p of places) {
        const opt = el('option', '', p.name ? `${p.name} (${p.slug || p.id})` : (p.slug || p.id));
        opt.value = p.slug || p.id || '';
        if (opt.value === place) found = true;
        placeSel.append(opt);
      }
      if (!found) {
        const opt = el('option', '', place);
        opt.value = place;
        placeSel.append(opt);
      }
      placeSel.value = place;
    } catch (err) {
      document.getElementById('rd-errors').append(errorBlock(err));
    }
  }

  placeSel.addEventListener('change', () => { place = placeSel.value; writeURL(); loadAll(); });
  window.addEventListener('popstate', () => {
    place = new URLSearchParams(location.search).get('place') || DEFAULT_PLACE;
    placeSel.value = place;
    loadAll();
  });

  loadPlaces();
  loadAll();
}
