// pages/history.js — /history archive browser (site spec §2 /history).
//
// Query controls map 1:1 onto GET /v1/history (place, layer, from, to,
// page_size — v2-implementation-plan §2.3/§2.4); results render as a
// chronological feed of event revisions, newest first, grouped by calendar
// day (UTC, matching timeAbs). All query state lives in the URL so a
// date-range permalink of an incident's arc is the shareable artifact.
//
// Pure helpers (state <-> URL, datetime conversion, day grouping) have no
// DOM access at import time — node can import and test this module. All DOM
// work happens inside initHistoryPage().

import { get, apiURL, curlFor, ApiError } from '../api.js';
import { timeCell, sevChip, layerLabel } from '../format.js';

/**
 * Event layers accepted by /v1/history's `layer` param, as the lowercase
 * slugs the API also accepts (locked in v2-implementation-plan §2.4).
 * Condition-backed layers (road_segment, chain_control) are projections,
 * not events, so they never appear in revision history.
 */
export const LAYER_OPTIONS = [
  'wildfire',
  'evacuation',
  'weather_alert',
  'fire_weather',
  'earthquake',
  'road_incident',
  'power',
  'gauge',
  'air_quality',
  'network',
  'announcement',
];

/** Page sizes offered in the control. Empty string = server default. */
export const PAGE_SIZES = ['', '25', '50', '100', '250'];

const DAY_MS = 86400000;

/**
 * RFC 3339 timestamp `days` days before `now`, truncated to the minute so
 * URLs stay readable.
 * @param {number} days
 * @param {Date=} now injectable clock for tests
 * @returns {string}
 */
export function isoDaysAgo(days, now) {
  const ref = now instanceof Date ? now.getTime() : Date.now();
  const d = new Date(ref - days * DAY_MS);
  d.setUTCSeconds(0, 0);
  return d.toISOString().replace('.000Z', 'Z');
}

/**
 * RFC 3339 (UTC) -> value for <input type="datetime-local"> ("YYYY-MM-DDTHH:MM"
 * in the viewer's local time). Returns '' for missing/unparseable input.
 * @param {string} iso
 * @returns {string}
 */
export function toLocalInput(iso) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, '0');
  return (
    `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}` +
    `T${p(d.getHours())}:${p(d.getMinutes())}`
  );
}

/**
 * <input type="datetime-local"> value (viewer-local) -> RFC 3339 UTC.
 * Returns null for empty/unparseable input.
 * @param {string} value
 * @returns {string|null}
 */
export function fromLocalInput(value) {
  if (!value) return null;
  const t = Date.parse(value); // local-time interpretation of the bare stamp
  if (Number.isNaN(t)) return null;
  const d = new Date(t);
  d.setUTCSeconds(0, 0);
  return d.toISOString().replace('.000Z', 'Z');
}

/**
 * Read query state from a URLSearchParams. `from` defaults to 7 days ago
 * when absent (spec: default range is the last 7 days); `to` empty means
 * "now" and is omitted from requests.
 * @param {URLSearchParams} search
 * @param {Date=} now injectable clock for tests
 * @returns {{place:string, layers:string[], from:string, to:string, pageSize:string}}
 */
export function readState(search, now) {
  const layers = search
    .getAll('layer')
    .map((v) => v.trim().toLowerCase())
    .filter((v) => LAYER_OPTIONS.includes(v));
  return {
    place: search.get('place') || '',
    layers,
    from: search.get('from') || isoDaysAgo(7, now),
    to: search.get('to') || '',
    pageSize: search.get('page_size') || '',
  };
}

/**
 * State -> canonical query string for the page URL (no leading "?"; empty
 * values omitted). `from` is always written so the permalink pins the range.
 * @param {{place:string, layers:string[], from:string, to:string, pageSize:string}} state
 * @returns {string}
 */
export function stateToSearch(state) {
  const search = new URLSearchParams();
  if (state.place) search.set('place', state.place);
  for (const layer of state.layers) search.append('layer', layer);
  if (state.from) search.set('from', state.from);
  if (state.to) search.set('to', state.to);
  if (state.pageSize) search.set('page_size', state.pageSize);
  return search.toString();
}

/**
 * State -> params object for api.get('/v1/history', ...). Empty values are
 * skipped by apiURL; layers become repeated `layer` params.
 * @param {{place:string, layers:string[], from:string, to:string, pageSize:string}} state
 * @param {string=} pageToken cursor from a previous response's next_page_token
 * @returns {Object}
 */
export function buildQuery(state, pageToken) {
  return {
    place: state.place,
    layer: state.layers,
    from: state.from,
    to: state.to,
    page_size: state.pageSize,
    page_token: pageToken || '',
  };
}

/**
 * UTC calendar-day key ("YYYY-MM-DD") for grouping; '' for bad input.
 * UTC to stay consistent with timeAbs(), which renders Z times.
 * @param {string} iso
 * @returns {string}
 */
export function dayKey(iso) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  return new Date(t).toISOString().slice(0, 10);
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

/**
 * Human label for a day key: "2026-07-04 Sat (UTC)"; unknown days get a
 * fixed label so unparseable timestamps still group visibly.
 * @param {string} key from dayKey()
 * @returns {string}
 */
export function dayLabel(key) {
  if (!key) return 'Unknown date';
  const t = Date.parse(`${key}T00:00:00Z`);
  if (Number.isNaN(t)) return 'Unknown date';
  return `${key} ${WEEKDAYS[new Date(t).getUTCDay()]} (UTC)`;
}

/* ------------------------------------------------------------------ */
/* DOM code below — only runs when initHistoryPage() is called.        */
/* ------------------------------------------------------------------ */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function errorBlock(err) {
  const div = el('div', 'error-block');
  div.append(
    el(
      'div',
      '',
      err instanceof ApiError
        ? `Request failed (HTTP ${err.status || 'network error'}):`
        : 'Request failed:'
    ),
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

/** One feed row for an EventRevision (protojson, snake_case). */
function revisionRow(rev) {
  const ev = rev.event || {};
  const row = el('div', 'rev-row');

  // observed_at: revision-level stamp, falling back to the event's.
  row.append(timeCell(rev.observed_at || ev.observed_at || ''));

  // Default enum values are omitted by protojson: absent severity is INFO.
  row.append(sevChip(ev.severity || 'INFO'));

  const meta = el('span', 'rev-meta mono');
  meta.append(
    el('span', 'rev-layer', layerLabel(ev.layer || '')),
    // revision is a proto uint32; protojson omits 0.
    el('span', 'rev-num', `rev ${rev.revision ?? 0}`),
    el('span', 'rev-status muted', ev.status || '—')
  );
  row.append(meta);

  const head = el('span', 'rev-headline');
  if (ev.id) {
    const link = document.createElement('a');
    link.href = `/event.html?id=${encodeURIComponent(ev.id)}`;
    link.textContent = ev.headline || ev.id; // textContent: upstream text is untrusted
    head.append(link);
  } else {
    head.textContent = ev.headline || '(no headline)';
  }
  row.append(head);

  return row;
}

/**
 * Wire up the /history page. Expects the element ids laid out in
 * history.html (hist-place, hist-layers, hist-from, hist-to, hist-size,
 * hist-apply, hist-query, hist-feed, hist-more, hist-status).
 */
export function initHistoryPage() {
  const $ = (id) => document.getElementById(id);
  const placeSel = $('hist-place');
  const layersBox = $('hist-layers');
  const fromInput = $('hist-from');
  const toInput = $('hist-to');
  const sizeSel = $('hist-size');
  const applyBtn = $('hist-apply');
  const queryEl = $('hist-query');
  const feed = $('hist-feed');
  const moreBtn = $('hist-more');
  const statusEl = $('hist-status');

  let state = readState(new URLSearchParams(location.search));
  let nextToken = '';
  let lastDayKey = null;
  let loadedCount = 0;
  let loading = false;

  // ---- controls ----------------------------------------------------

  // Layer multi-select as checkboxes (dense, all visible at once).
  for (const slug of LAYER_OPTIONS) {
    const label = el('label', 'layer-check');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.value = slug;
    box.checked = state.layers.includes(slug);
    label.append(box, ' ', layerLabel(slug));
    layersBox.append(label);
  }

  for (const size of PAGE_SIZES) {
    const opt = document.createElement('option');
    opt.value = size;
    opt.textContent = size === '' ? 'default' : size;
    sizeSel.append(opt);
  }
  sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';

  fromInput.value = toLocalInput(state.from);
  toInput.value = toLocalInput(state.to);

  // Place select: "(any place)" first; options filled from /v1/places.
  // Until (or if) that load fails, the URL-provided place is kept as a
  // provisional option so shared links never lose their filter.
  const anyOpt = document.createElement('option');
  anyOpt.value = '';
  anyOpt.textContent = '(any place)';
  placeSel.append(anyOpt);
  if (state.place) {
    const opt = document.createElement('option');
    opt.value = state.place;
    opt.textContent = state.place;
    placeSel.append(opt);
    placeSel.value = state.place;
  }

  async function loadPlaces() {
    try {
      const data = await get('/v1/places');
      const places = Array.isArray(data.places) ? data.places : [];
      // Group by kind for scanability; PlaceKind renders as proto enum names.
      const byKind = new Map();
      for (const p of places) {
        const kind = p.kind || 'PLACE_KIND_UNSPECIFIED';
        if (!byKind.has(kind)) byKind.set(kind, []);
        byKind.get(kind).push(p);
      }
      const current = placeSel.value;
      placeSel.textContent = '';
      placeSel.append(anyOpt);
      let currentFound = current === '';
      for (const [kind, group] of byKind) {
        const og = document.createElement('optgroup');
        og.label = layerLabel(kind);
        group.sort((a, b) => String(a.name || a.slug).localeCompare(String(b.name || b.slug)));
        for (const p of group) {
          const opt = document.createElement('option');
          opt.value = p.slug || p.id || '';
          opt.textContent = p.name ? `${p.name} (${opt.value})` : opt.value;
          if (opt.value === current) currentFound = true;
          og.append(opt);
        }
        placeSel.append(og);
      }
      if (!currentFound && current) {
        // Keep a URL-supplied place the directory doesn't know about.
        const opt = document.createElement('option');
        opt.value = current;
        opt.textContent = current;
        placeSel.append(opt);
      }
      placeSel.value = current;
    } catch (err) {
      // The select still works with the URL-provided value; just say why
      // the directory is empty.
      placeSel.insertAdjacentElement('afterend', errorBlock(err));
    }
  }

  function readControls() {
    return {
      place: placeSel.value,
      layers: [...layersBox.querySelectorAll('input:checked')].map((b) => b.value),
      from: fromLocalInput(fromInput.value) || '',
      to: fromLocalInput(toInput.value) || '',
      pageSize: sizeSel.value,
    };
  }

  // ---- feed rendering ----------------------------------------------

  function describeRange() {
    const from = state.from || '(unset)';
    const to = state.to || 'now';
    return `${from} → ${to}`;
  }

  function showQuery(params) {
    const url = apiURL('/v1/history', params);
    queryEl.textContent = '';
    const code = el('code', 'inline', `GET ${url}`);
    const curl = el('span', 'muted small mono', `  ${curlFor(url)}`);
    queryEl.append(code, ' ', curl);
  }

  function appendRevisions(revisions) {
    for (const rev of revisions) {
      const key = dayKey(rev.observed_at || (rev.event && rev.event.observed_at));
      if (key !== lastDayKey) {
        lastDayKey = key;
        feed.append(el('div', 'day-sep', dayLabel(key)));
      }
      feed.append(revisionRow(rev));
      loadedCount++;
    }
  }

  async function runQuery(pageToken) {
    if (loading) return;
    loading = true;
    applyBtn.disabled = true;
    moreBtn.disabled = true;
    moreBtn.hidden = !pageToken;

    const isFirstPage = !pageToken;
    if (isFirstPage) {
      feed.textContent = '';
      lastDayKey = null;
      loadedCount = 0;
      nextToken = '';
    }
    const params = buildQuery(state, pageToken);
    showQuery(params);
    statusEl.textContent = 'Loading /v1/history…';
    statusEl.className = 'loading';

    try {
      const data = await get('/v1/history', params);
      const revisions = Array.isArray(data.revisions) ? data.revisions : [];
      appendRevisions(revisions);
      nextToken = data.next_page_token || '';
      moreBtn.hidden = !nextToken;
      moreBtn.disabled = false;

      if (loadedCount === 0) {
        // Distinct from an API error: the query succeeded, the range is empty.
        const empty = el('div', 'notice');
        empty.append(
          el('div', 'mono', 'No revisions in range.'),
          el(
            'div',
            'muted small',
            `The API responded OK but returned no revisions for ${describeRange()}` +
              (state.place ? ` in place "${state.place}"` : '') +
              (state.layers.length ? ` for layer(s) ${state.layers.join(', ')}` : '') +
              '. Widen the time range or clear filters.'
          )
        );
        feed.append(empty);
        statusEl.textContent = '0 revisions';
      } else {
        statusEl.textContent =
          `${loadedCount} revision(s) loaded` + (nextToken ? ' — more available' : '');
      }
      statusEl.className = 'muted small mono';
    } catch (err) {
      // API error is never a blank page and never mistaken for "no data".
      feed.append(errorBlock(err));
      statusEl.textContent = 'Request failed — see error above.';
      statusEl.className = 'small mono';
      moreBtn.hidden = true;
    } finally {
      loading = false;
      applyBtn.disabled = false;
    }
  }

  // ---- events --------------------------------------------------------

  applyBtn.addEventListener('click', () => {
    state = readControls();
    // The permalink is the artifact: Apply writes the full query to the URL.
    const qs = stateToSearch(state);
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
    runQuery();
  });

  moreBtn.addEventListener('click', () => {
    if (nextToken) runQuery(nextToken);
  });

  window.addEventListener('popstate', () => {
    state = readState(new URLSearchParams(location.search));
    placeSel.value = state.place;
    for (const box of layersBox.querySelectorAll('input[type=checkbox]')) {
      box.checked = state.layers.includes(box.value);
    }
    fromInput.value = toLocalInput(state.from);
    toInput.value = toLocalInput(state.to);
    sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';
    runQuery();
  });

  // ---- go ------------------------------------------------------------

  loadPlaces();
  runQuery();
}
