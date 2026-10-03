// pages/roads.js — /roads: the road-conditions view.
//
// The screen teaches one thing before it shows anything: roads split across two
// idioms. Road INCIDENTS are events; road CONDITIONS are not. Both are read here
// from the layers the API projects for a place — a live GeoJSON FeatureCollection
// from GET /api/v1/places/{place}/map/{layer}.geojson:
//
//   road_incident — CHP incidents (AI-enhanced) and Caltrans closures
//                   (projected events). Read ONCE and split by status: what is
//                   on the road now, and planned roadwork windows (SCHEDULED).
//   road_segment  — per-road travel time, delay, congestion, status (Google Routes + Caltrans)
//   chain_control — active chain controls, R1/R2/R3 (Caltrans)
//
// Incidents render as the shared record row and link to /event?id=, so they read
// as what they are — events with an id and a revision history.
//
// WHY THE INCIDENT LIST READS THE LAYER, NOT /events. The design specifies
// `/events?layer=road_incident`, and that endpoint returns the same records. But
// an EventList carries no per-source health, while the projected layer carries
// `metadata.sourceStatus` — the difference between "no incidents reported" and
// "the CHP feed is down". Trading that away for endpoint fidelity would break the
// contract this page exists to demonstrate, so the layer wins and the prose says
// so. Each row still links to the event record itself.
//
// State is just the selected place, kept in the URL as a permalink.

import { get, ApiError, PUBLIC_ORIGIN } from '../api.js';
import { sourceDot, timeAgo, timeAbs, recordRow } from '../format.js';
import { activePlace, placeMenuOptions, placeMenuLabel } from '../place.js';
import { requireEls, copyOnClick } from '../ui.js';
import '../components/menu.js'; // registers <grid-menu>

/** A Caltrans roadwork window that is planned but not set up yet. */
const isPlanned = (f) => String((f.properties || {}).status || '').toUpperCase() === 'SCHEDULED';

const SECTIONS = [
  { layer: 'road_incident', filter: (f) => !isPlanned(f),
    status: 'rd-inc-status', content: 'rd-inc', render: renderIncidents,
    count: 'rd-inc-h', countLabel: 'Incidents',
    empty: {
      head: 'No active road incidents in this place.',
      sub: 'Conditions still exist — an open road is baseline state, carried by the road_segment layer, not by /events.',
    } },
  { layer: 'road_incident', filter: isPlanned,
    status: 'rd-plan-status', content: 'rd-plan', render: renderPlanned,
    count: 'rd-plan-h', countLabel: 'Planned closures',
    empty: {
      head: 'No planned closures in this place.',
      sub: 'Caltrans lists no upcoming roadwork windows here. Its schedule runs about a week ahead.',
    } },
  { layer: 'road_segment', status: 'rd-seg-status', content: 'rd-seg', render: renderSegments,
    empty: { head: 'No monitored roads in this area.', sub: 'This area has no configured road segments.' } },
  { layer: 'chain_control', status: 'rd-chain-status', content: 'rd-chain', render: renderChain,
    empty: { head: 'No chain controls in effect.', sub: 'Caltrans reports no active chain controls right now — a clean report, not a source failure.' } },
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

// The absolute URL, copied on click — the same treatment as the Map's feed
// metadata table. It was `curl -s '<url>'` in an ink-black code box: three of
// those stacked down a near-white page were the heaviest thing on it, and it
// was the last echo variant left after the rest were unified.
function curlLine(path) {
  const url = absoluteURL(path);
  const line = el('div', 'rd-curl geojson-url', url);
  copyOnClick(line, url, 'URL');
  return line;
}

/** The public absolute form of an API path, for a third-party client. */
function absoluteURL(path) {
  try {
    return new URL(path, PUBLIC_ORIGIN).href;
  } catch {
    return path;
  }
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
  const cls = { OPEN: 'open', RESTRICTED: 'warn', CLOSED: 'bad' }[status] || 'unknown';
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

function dataTable(headers) {
  const wrap = el('div', 'table-wrap');
  // capped-head: the section cap above lists these columns in order, so the
  // visible header row is a second copy. Hidden, not removed — see app.css.
  const table = el('table', 'data-table rd-table capped-head');
  const thead = el('thead');
  const htr = el('tr');
  for (const h of headers) htr.append(el('th', h === 'Travel' || h === 'Delay' || h === 'Distance' ? 'num' : '', h));
  thead.append(htr);
  const tbody = el('tbody');
  table.append(thead, tbody);
  wrap.append(table);
  return { wrap, tbody };
}

/**
 * Incidents as the shared record row. The layer's feature `properties` envelope
 * is event-shaped (id, layer, severity, headline) — `updatedAt` stands in for
 * `observedAt`, which the projection does not carry.
 */
function renderIncidents(container, feats) {
  feats.sort((a, b) =>
    (b.properties.severityRank || 0) - (a.properties.severityRank || 0) ||
    String(b.properties.updatedAt || '').localeCompare(String(a.properties.updatedAt || ''))
  );
  const list = el('div', 'rec-list');
  for (const f of feats) {
    const p = f.properties || {};
    const row = recordRow(
      {
        id: p.id,
        layer: p.layer || 'road_incident',
        severity: p.severity,
        headline: p.headline,
        observedAt: p.observedAt || p.updatedAt,
      },
      { href: p.id ? `/event?id=${encodeURIComponent(p.id)}` : undefined }
    );
    // The projection carries context a bare record row does not: the AI summary
    // and the CHP log number are the two things an ops reader actually scans for.
    const extra = [];
    const body = p.summary || p.description;
    if (body) extra.push(body);
    const bits = [];
    if (p.incident && p.incident.logNumber) bits.push('log ' + p.incident.logNumber);
    // A set-up closure's planned end is an estimate: past it, the closure is
    // still in place until Caltrans records the pick-up.
    const closure = (p.incident && p.incident.closure) || null;
    if (closure && closure.plannedEnd) bits.push('planned end ' + timeAbs(closure.plannedEnd));
    if (p.source && p.source.name) bits.push(p.source.name);
    const recBody = row.querySelector('.rec-body');
    if (recBody && body) {
      const sub = el('div', 'rec-sub muted small', body);
      recBody.insertBefore(sub, recBody.querySelector('.rec-id'));
    }
    if (recBody && bits.length) {
      const idEl = recBody.querySelector('.rec-id');
      if (idEl) idEl.textContent = [p.id, ...bits].filter(Boolean).join(' · ');
    }
    list.append(row);
  }
  container.append(list);
}

/**
 * Planned closures: Caltrans roadwork windows, soonest first. The row's time
 * is the planned start ("in 2 days"); the id line carries the whole window.
 */
function renderPlanned(container, feats) {
  const closureOf = (f) => ((f.properties || {}).incident || {}).closure || {};
  const startOf = (f) => closureOf(f).plannedStart || (f.properties || {}).effective || '';
  feats.sort((a, b) => String(startOf(a)).localeCompare(String(startOf(b))));
  const list = el('div', 'rec-list');
  for (const f of feats) {
    const p = f.properties || {};
    const c = closureOf(f);
    const row = recordRow(
      {
        id: p.id,
        layer: p.layer || 'road_incident',
        severity: p.severity,
        headline: p.headline,
        observedAt: startOf(f),
      },
      { href: p.id ? `/event?id=${encodeURIComponent(p.id)}` : undefined, tags: ['scheduled'] }
    );
    const recBody = row.querySelector('.rec-body');
    const sub = [p.areaLabel, p.description].filter(Boolean).join(' — ');
    if (recBody && sub) {
      recBody.insertBefore(el('div', 'rec-sub muted small', sub), recBody.querySelector('.rec-id'));
    }
    const idEl = recBody && recBody.querySelector('.rec-id');
    if (idEl) {
      const span = c.plannedStart
        ? `window ${timeAbs(c.plannedStart)} – ${c.plannedEnd ? timeAbs(c.plannedEnd) : 'open-ended'}`
        : '';
      idEl.textContent = [p.id, span, p.source && p.source.name].filter(Boolean).join(' · ');
    }
    list.append(row);
  }
  container.append(list);
}

function renderSegments(container, feats) {
  feats.sort((a, b) =>
    (b.properties.severityRank || 0) - (a.properties.severityRank || 0) ||
    String(a.properties.headline || '').localeCompare(String(b.properties.headline || ''))
  );
  const labels = ['Road', 'Status', 'Congestion', 'Travel', 'Delay', 'Distance'];
  const { wrap, tbody } = dataTable(labels);
  for (const f of feats) {
    const p = f.properties || {};
    const r = p.road || {};
    const tr = el('tr');

    const roadTd = el('td', 'wrap');
    roadTd.append(el('div', 'cell-name', p.headline || r.roadId || '—'));
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
    labelRow(tr, labels);
    tbody.append(tr);
  }
  container.append(wrap);
}

function renderChain(container, feats) {
  const order = { R3: 3, R2: 2, R1: 1 };
  feats.sort((a, b) => (order[b.properties.chainControl?.level] || 0) - (order[a.properties.chainControl?.level] || 0));
  const labels = ['Level', 'Highway', 'Direction', 'Note'];
  const { wrap, tbody } = dataTable(labels);
  for (const f of feats) {
    const p = f.properties || {};
    const c = p.chainControl || {};
    const tr = el('tr');
    const lvlTd = el('td'); lvlTd.append(chainBadge(c.level)); tr.append(lvlTd);
    tr.append(el('td', '', c.highway || '—'));
    tr.append(el('td', '', c.direction || '—'));
    tr.append(el('td', 'wrap muted', p.headline || '—'));
    labelRow(tr, labels);
    tbody.append(tr);
  }
  container.append(wrap);
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
  const n = el('div', 'loud-banner');
  n.append(el('div', 'loud-title', 'Source unavailable — state unknown, not clear'));
  n.append(el('p', '', 'The upstream feed failed, so the Grid returns no features rather than a fabricated clear state (metadata.sourceStatus = UNAVAILABLE). Check the official source directly.'));
  const url = md && md.sourceUrl;
  if (url && /^https?:\/\//.test(url)) {
    const p = el('p');
    const a = el('a', '', url);
    a.href = url; a.target = '_blank'; a.rel = 'noopener noreferrer';
    p.append(a);
    n.append(p);
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

/**
 * Update a section's heading with its live count — "Incidents · 3".
 * A failed or unavailable section gets no number at all: a count of 0 beside a
 * broken feed is exactly the false all-clear the contract forbids.
 */
function setCount(section, n) {
  if (!section.count) return;
  const h = document.getElementById(section.count);
  if (h) h.textContent = n === null ? section.countLabel : `${section.countLabel} · ${n}`;
}

async function loadSection(place, section, fetchLayer) {
  const statusEl = document.getElementById(section.status);
  const contentEl = document.getElementById(section.content);
  const path = `/api/v1/places/${encodeURIComponent(place)}/map/${section.layer}.geojson`;
  statusEl.textContent = 'loading…';
  contentEl.textContent = '';
  setCount(section, null);

  let fc, err;
  try { fc = await fetchLayer(path); } catch (e) { err = e; }
  statusHeader(statusEl, path, fc, err);
  if (err) { contentEl.append(errorBlock(err)); return; }

  const md = fc.metadata || {};
  const status = String(md.sourceStatus || '').toUpperCase();
  let feats = Array.isArray(fc.features) ? fc.features : [];
  if (section.filter) feats = feats.filter(section.filter);

  if (status === 'UNAVAILABLE') { contentEl.append(unavailableNotice(md)); return; }

  // Only now is a count an honest claim: the feed answered, and answered OK or
  // STALE (a STALE count is last-good, which the line beneath says out loud).
  setCount(section, feats.length);

  if (feats.length === 0) { contentEl.append(emptyNotice(section.empty, status === 'STALE')); return; }
  if (status === 'STALE') {
    contentEl.append(el('div', 'meta-stale small', 'Source STALE — showing last-good cached data.'));
  }
  section.render(contentEl, feats);
}

/* ---------- init ---------- */

export function initRoadsPage() {
  const { placeMenu } = requireEls('roads.js', { placeMenu: 'rd-place' });
  let place = '';
  let places = [];

  function writeURL() {
    history.replaceState(null, '', place ? `?place=${encodeURIComponent(place)}` : location.pathname);
  }

  function showPlace() {
    placeMenu.value = place;
    placeMenu.triggerLabel = placeMenuLabel(places, place, 'no place');
  }

  function loadAll() {
    if (!place) return;
    // One request per layer, however many sections read it (incidents and
    // planned closures are two views of road_incident).
    const shared = new Map();
    const fetchLayer = (path) => {
      if (!shared.has(path)) shared.set(path, get(path));
      return shared.get(path);
    };
    for (const s of SECTIONS) loadSection(place, s, fetchLayer);
  }

  placeMenu.addEventListener('change', (e) => {
    place = e.detail.value;
    showPlace();
    writeURL();
    loadAll();
  });
  window.addEventListener('popstate', () => {
    const next = new URLSearchParams(location.search).get('place');
    if (next && next !== place) { place = next; showPlace(); loadAll(); }
  });

  // The place resolver owns the ?place= → sessionStorage → first-AREA order, so
  // this screen never hardcodes a default. A directory failure with no ?place=
  // leaves `active` null — which is unknown, not "no places", and must say so.
  activePlace().then((dir) => {
    const { active } = dir;
    places = dir.places;
    if (!active) {
      placeMenu.options = [];
      placeMenu.triggerLabel = 'unavailable';
      document.getElementById('rd-errors').append(
        (() => {
          const b = el('div', 'loud-banner');
          b.append(el('div', 'loud-title', 'Place directory unavailable'));
          b.append(el('p', '', 'GET /api/v1/places?kind=AREA failed, so no place could be resolved and nothing below was requested. This is an unknown state, not an empty one — reload, or name a place explicitly with ?place=.'));
          return b;
        })()
      );
      return;
    }

    // placeMenuOptions keeps `current` even when the directory does not list
    // it: a ?place= naming a corridor or a town is a valid {place}, and
    // dropping it would silently re-scope the page to somewhere else.
    place = active.slug;
    placeMenu.options = placeMenuOptions(places, { current: place, group: true });
    showPlace();
    loadAll();

    // activePlace() resolves the DEFAULT, and to do that it only needs the
    // areas. The picker wants the whole directory — a corridor is the most
    // natural thing to scope roads to, and it was not on the menu. Loaded
    // second so the page is usable before it lands.
    get('/api/v1/places')
      .then((data) => {
        const all = Array.isArray(data.places) ? data.places : [];
        if (!all.length) return;
        places = all;
        placeMenu.options = placeMenuOptions(places, { current: place, group: true });
        showPlace();
      })
      .catch(() => {
        /* The AREA menu above still works; a fuller one is not worth a banner. */
      });
  });
}
