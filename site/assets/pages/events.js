// pages/events.js — /events explorer (site spec §2 /events).
//
// Filter bar maps 1:1 onto GET /v1/events query parameters (place, layer
// repeated, status repeated, severity_min, since, page_size, page_token —
// v2-implementation-plan §2.3/§2.4). All filter state lives in the page URL
// so any view is a shareable permalink; "Load more" follows next_page_token
// and records the token of the current tail in the URL. The exact GET URL
// behind the table renders above the results as a copyable curl line.
//
// Pure helpers (state <-> URL, query building, datetime conversion, place
// grouping) have no DOM access at import time — node can import and test
// this module. All DOM work happens inside initEventsPage().

import { get, apiURL, curlFor, ApiError } from '../api.js';
import { timeCell, sevChip, layerLabel } from '../format.js';
import { initChrome } from '../nav.js';

/**
 * Event layers accepted by /v1/events' `layer` param, as the lowercase
 * slugs the API also accepts (locked in v2-implementation-plan §2.4).
 * Condition-backed layers (road_segment, chain_control) are projections,
 * not events, and never appear in /v1/events.
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

/** EventStatus values the `status` param accepts (repeatable). */
export const STATUS_OPTIONS = ['ACTIVE', 'SCHEDULED', 'RESOLVED', 'EXPIRED'];

/** The API's documented default when no `status` param is sent. */
export const DEFAULT_STATUSES = ['ACTIVE', 'SCHEDULED'];

/** Severity floor options for `severity_min` ('' = no floor). */
export const SEVERITY_OPTIONS = ['INFO', 'MINOR', 'MODERATE', 'SEVERE', 'EXTREME'];

/** Page sizes offered in the control. Empty string = server default. */
export const PAGE_SIZES = ['', '25', '50', '100', '200']; // 200 is the API max (store maxPageSize); >200 is a 400

/** PlaceKind display order for the grouped place select. */
export const KIND_ORDER = ['AREA', 'COUNTY', 'TOWN', 'EVAC_ZONE', 'CORRIDOR', 'SITE'];

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
 * True when `statuses` is exactly the API default set (order-insensitive).
 * The default set is omitted from both the page URL and the request so the
 * URL params stay byte-for-byte what the API receives.
 * @param {string[]} statuses
 * @returns {boolean}
 */
export function isDefaultStatuses(statuses) {
  if (statuses.length !== DEFAULT_STATUSES.length) return false;
  const set = new Set(statuses);
  return DEFAULT_STATUSES.every((s) => set.has(s));
}

/**
 * Read explorer state from a URLSearchParams. Unknown layer/status/severity
 * values are dropped; no `status` params means the API default
 * (ACTIVE+SCHEDULED). `page_token` marks the tail cursor of a paginated view.
 * @param {URLSearchParams} search
 * @returns {{place:string, layers:string[], statuses:string[], severityMin:string,
 *            since:string, pageSize:string, pageToken:string}}
 */
export function readState(search) {
  const layers = search
    .getAll('layer')
    .map((v) => v.trim().toLowerCase())
    .filter((v) => LAYER_OPTIONS.includes(v));
  const statuses = search
    .getAll('status')
    .map((v) => v.trim().toUpperCase())
    .filter((v) => STATUS_OPTIONS.includes(v));
  const sev = (search.get('severity_min') || '').trim().toUpperCase();
  return {
    place: search.get('place') || '',
    layers,
    statuses: statuses.length ? statuses : [...DEFAULT_STATUSES],
    severityMin: SEVERITY_OPTIONS.includes(sev) ? sev : '',
    since: search.get('since') || '',
    pageSize: search.get('page_size') || '',
    pageToken: search.get('page_token') || '',
  };
}

/**
 * State -> canonical query string for the page URL (no leading "?").
 * Empty values and the default status set are omitted so URLs stay
 * canonical and shareable.
 * @param {{place:string, layers:string[], statuses:string[], severityMin:string,
 *          since:string, pageSize:string, pageToken:string}} state
 * @returns {string}
 */
export function stateToSearch(state) {
  const search = new URLSearchParams();
  if (state.place) search.set('place', state.place);
  for (const layer of state.layers) search.append('layer', layer);
  if (!isDefaultStatuses(state.statuses)) {
    for (const status of state.statuses) search.append('status', status);
  }
  if (state.severityMin) search.set('severity_min', state.severityMin);
  if (state.since) search.set('since', state.since);
  if (state.pageSize) search.set('page_size', state.pageSize);
  if (state.pageToken) search.set('page_token', state.pageToken);
  return search.toString();
}

/**
 * State -> params object for api.get('/v1/events', ...). Empty values are
 * skipped by apiURL; layers/statuses become repeated params. The default
 * status set is omitted (it is the server's documented default).
 * @param {{place:string, layers:string[], statuses:string[], severityMin:string,
 *          since:string, pageSize:string}} state
 * @param {string=} pageToken cursor from a previous response's next_page_token
 * @returns {Object}
 */
export function buildQuery(state, pageToken) {
  return {
    place: state.place,
    layer: state.layers,
    status: isDefaultStatuses(state.statuses) ? [] : state.statuses,
    severity_min: state.severityMin,
    since: state.since,
    page_size: state.pageSize,
    page_token: pageToken || '',
  };
}

/**
 * Group a PlaceList's places by kind for the place <select>, in KIND_ORDER
 * (unknown kinds last, in first-seen order), each group name-sorted.
 * @param {Array<Object>} places protojson Place messages (snake_case)
 * @returns {Array<{kind: string, places: Array<Object>}>}
 */
export function groupPlaces(places) {
  const byKind = new Map();
  for (const p of places || []) {
    const kind = p.kind || 'PLACE_KIND_UNSPECIFIED';
    if (!byKind.has(kind)) byKind.set(kind, []);
    byKind.get(kind).push(p);
  }
  const kinds = [
    ...KIND_ORDER.filter((k) => byKind.has(k)),
    ...[...byKind.keys()].filter((k) => !KIND_ORDER.includes(k)),
  ];
  return kinds.map((kind) => ({
    kind,
    places: byKind
      .get(kind)
      .slice()
      .sort((a, b) =>
        String(a.name || a.slug || a.id).localeCompare(String(b.name || b.slug || b.id))
      ),
  }));
}

/* ------------------------------------------------------------------ */
/* DOM code below — only runs when initEventsPage() is called.         */
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

/** One results row for an Event (protojson, snake_case). */
function eventRow(ev) {
  const tr = document.createElement('tr');

  const sevTd = document.createElement('td');
  // protojson omits default enum values: absent severity is INFO.
  sevTd.append(sevChip(ev.severity || 'INFO'));
  tr.append(sevTd);

  const headTd = el('td', 'wrap');
  if (ev.id) {
    const link = document.createElement('a');
    link.href = `/event.html?id=${encodeURIComponent(ev.id)}`;
    link.textContent = ev.headline || ev.id; // textContent: upstream text is untrusted
    headTd.append(link);
  } else {
    headTd.textContent = ev.headline || '(no headline)';
  }
  tr.append(headTd);

  tr.append(el('td', '', layerLabel(ev.layer || '')));
  tr.append(el('td', 'wrap', ev.area_label || '—'));
  tr.append(el('td', 'mono', (ev.provenance && ev.provenance.source_id) || '—'));

  const timeTd = document.createElement('td');
  timeTd.append(timeCell(ev.observed_at || ''));
  tr.append(timeTd);

  // revision is a proto uint32; protojson omits 0.
  tr.append(el('td', 'num', String(ev.revision ?? 0)));

  return tr;
}

/**
 * Wire up the /events page. Expects the element ids laid out in events.html
 * (ev-place, ev-layers, ev-status, ev-sev, ev-since, ev-size, ev-apply,
 * ev-query, ev-errors, ev-tbody, ev-empty, ev-more, ev-status-line).
 */
export function initEventsPage() {
  initChrome('events');

  const $ = (id) => document.getElementById(id);
  const placeSel = $('ev-place');
  const layersBox = $('ev-layers');
  const statusBox = $('ev-status');
  const sevSel = $('ev-sev');
  const sinceInput = $('ev-since');
  const sizeSel = $('ev-size');
  const applyBtn = $('ev-apply');
  const queryEl = $('ev-query');
  const errorsEl = $('ev-errors');
  const tbody = $('ev-tbody');
  const emptyEl = $('ev-empty');
  const moreBtn = $('ev-more');
  const statusLine = $('ev-status-line');

  let state = readState(new URLSearchParams(location.search));
  let nextToken = '';
  let loadedCount = 0;
  let loading = false;

  // ---- controls ----------------------------------------------------

  function makeChecks(box, values, checked, labelOf) {
    for (const value of values) {
      const label = el('label', 'opt-check');
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.value = value;
      input.checked = checked.includes(value);
      label.append(input, ' ', labelOf(value));
      box.append(label);
    }
  }

  makeChecks(layersBox, LAYER_OPTIONS, state.layers, (v) => layerLabel(v));
  makeChecks(statusBox, STATUS_OPTIONS, state.statuses, (v) => v);

  const anySev = document.createElement('option');
  anySev.value = '';
  anySev.textContent = '(no floor)';
  sevSel.append(anySev);
  for (const sev of SEVERITY_OPTIONS) {
    const opt = document.createElement('option');
    opt.value = sev;
    opt.textContent = sev;
    sevSel.append(opt);
  }
  sevSel.value = state.severityMin;

  for (const size of PAGE_SIZES) {
    const opt = document.createElement('option');
    opt.value = size;
    opt.textContent = size === '' ? 'default' : size;
    sizeSel.append(opt);
  }
  sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';

  sinceInput.value = toLocalInput(state.since);

  // Place select: "(any place)" first; options filled from /v1/places.
  // Until (or if) that load fails, the URL-provided place is kept as a
  // provisional option so shared links never lose their filter.
  const anyPlace = document.createElement('option');
  anyPlace.value = '';
  anyPlace.textContent = '(any place)';
  placeSel.append(anyPlace);
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
      const groups = groupPlaces(Array.isArray(data.places) ? data.places : []);
      const current = placeSel.value;
      placeSel.textContent = '';
      placeSel.append(anyPlace);
      let currentFound = current === '';
      for (const { kind, places } of groups) {
        const og = document.createElement('optgroup');
        og.label = layerLabel(kind);
        for (const p of places) {
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
      errorsEl.append(errorBlock(err));
    }
  }

  function readControls() {
    const statuses = [...statusBox.querySelectorAll('input:checked')].map((b) => b.value);
    if (statuses.length === 0) {
      // No boxes checked = the API default; reflect it back in the UI.
      for (const box of statusBox.querySelectorAll('input[type=checkbox]')) {
        box.checked = DEFAULT_STATUSES.includes(box.value);
      }
    }
    return {
      place: placeSel.value,
      layers: [...layersBox.querySelectorAll('input:checked')].map((b) => b.value),
      statuses: statuses.length ? statuses : [...DEFAULT_STATUSES],
      severityMin: sevSel.value,
      since: fromLocalInput(sinceInput.value) || '',
      pageSize: sizeSel.value,
      pageToken: '', // Apply always restarts from the first page
    };
  }

  function syncControls() {
    placeSel.value = state.place;
    for (const box of layersBox.querySelectorAll('input[type=checkbox]')) {
      box.checked = state.layers.includes(box.value);
    }
    for (const box of statusBox.querySelectorAll('input[type=checkbox]')) {
      box.checked = state.statuses.includes(box.value);
    }
    sevSel.value = state.severityMin;
    sinceInput.value = toLocalInput(state.since);
    sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';
  }

  function writeURL() {
    const qs = stateToSearch(state);
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
  }

  // ---- results ------------------------------------------------------

  function showQuery(params) {
    const url = apiURL('/v1/events', params);
    queryEl.textContent = '';
    const code = el('code', 'inline', `GET ${url}`);
    const copy = el('button', 'copy-btn', 'copy');
    copy.type = 'button';
    copy.addEventListener('click', () => {
      navigator.clipboard.writeText(curlFor(url)).then(
        () => {
          copy.textContent = 'copied';
          setTimeout(() => (copy.textContent = 'copy'), 1200);
        },
        () => {
          copy.textContent = 'failed';
        }
      );
    });
    const curl = el('span', 'muted small mono', curlFor(url));
    queryEl.append(code, ' ', copy, ' ', curl);
  }

  /**
   * Fetch one page. `token` is the page_token to request ('' = first page);
   * `append` keeps existing rows (Load more) instead of restarting the table.
   */
  async function runQuery(token, append) {
    if (loading) return;
    loading = true;
    applyBtn.disabled = true;
    moreBtn.disabled = true;

    if (!append) {
      tbody.textContent = '';
      emptyEl.textContent = '';
      loadedCount = 0;
      nextToken = '';
    }
    errorsEl.textContent = '';

    const params = buildQuery(state, token);
    showQuery(params);
    statusLine.textContent = 'Loading /v1/events…';
    statusLine.className = 'loading';

    try {
      const data = await get('/v1/events', params);
      const events = Array.isArray(data.events) ? data.events : [];
      for (const ev of events) tbody.append(eventRow(ev));
      loadedCount += events.length;
      nextToken = data.next_page_token || '';
      moreBtn.hidden = !nextToken;
      moreBtn.disabled = false;

      if (loadedCount === 0) {
        // Distinct from an API error: the query succeeded, nothing matched.
        const empty = el('div', 'notice');
        empty.append(
          el('div', 'mono', 'No events match.'),
          el(
            'div',
            'muted small',
            'The API responded OK but returned no events for these filters. ' +
              'Loosen the status/severity filters, clear `since`, or widen the place.'
          )
        );
        emptyEl.append(empty);
        statusLine.textContent = '0 events';
      } else {
        statusLine.textContent =
          `${loadedCount} event(s) loaded` +
          (state.pageToken ? ' (resumed from page_token in URL)' : '') +
          (nextToken ? ' — more available' : ' — end of results');
      }
      statusLine.className = 'muted small mono';
    } catch (err) {
      // API error is never a blank page and never mistaken for "no data".
      errorsEl.append(errorBlock(err));
      statusLine.textContent = 'Request failed — see error above.';
      statusLine.className = 'small mono';
      moreBtn.hidden = true;
    } finally {
      loading = false;
      applyBtn.disabled = false;
    }
  }

  // ---- events ---------------------------------------------------------

  applyBtn.addEventListener('click', () => {
    state = readControls();
    writeURL(); // the permalink is the artifact: Apply writes the full query
    runQuery('', false);
  });

  moreBtn.addEventListener('click', async () => {
    if (!nextToken) return;
    const token = nextToken;
    await runQuery(token, true);
    // Record the tail cursor so this exact view (rows from `token` on) is
    // shareable; Apply clears it back to page one.
    state.pageToken = token;
    writeURL();
  });

  window.addEventListener('popstate', () => {
    state = readState(new URLSearchParams(location.search));
    syncControls();
    runQuery(state.pageToken, false);
  });

  // ---- go -------------------------------------------------------------

  if (state.pageToken) {
    const note = el('div', 'notice');
    note.append(
      el('div', 'mono', 'Resuming from a pagination cursor.'),
      el(
        'div',
        'muted small',
        'This URL carries a page_token, so the table starts mid-stream at ' +
          'that cursor. Press Apply to restart from the first page.'
      )
    );
    errorsEl.after(note);
  }

  loadPlaces();
  runQuery(state.pageToken, false);
}
