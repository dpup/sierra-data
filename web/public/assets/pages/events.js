// pages/events.js — /events explorer (site spec §2 /events).
//
// Filter bar maps 1:1 onto GET /api/v1/events query parameters (place, layer
// repeated, status repeated, severity_min, since, page_size, page_token —
// v2-implementation-plan §2.3/§2.4). All filter state lives in the page URL
// so any view is a shareable permalink; "Load more" follows nextPageToken
// and records the token of the current tail in the URL. The exact GET URL
// behind the table renders above the results as a copyable curl line.
//
// Pure helpers (state <-> URL, query building, datetime conversion, place
// grouping) have no DOM access at import time — node can import and test
// this module. All DOM work happens inside initEventsPage().

import { get, apiURL, curlFor, ApiError } from '../api.js';
import { layerLabel, recordRow } from '../format.js';
import { renderEventDetail } from './event-detail.js';

/**
 * Event layers accepted by /api/v1/events' `layer` param, as the lowercase
 * slugs the API also accepts (locked in v2-implementation-plan §2.4).
 * Condition-backed layers (road_segment, chain_control) are projections,
 * not events, and never appear in /api/v1/events.
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
  'mesh',
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
 * State -> params object for api.get('/api/v1/events', ...). Empty values are
 * skipped by apiURL; layers/statuses become repeated params. The default
 * status set is omitted (it is the server's documented default).
 * @param {{place:string, layers:string[], statuses:string[], severityMin:string,
 *          since:string, pageSize:string}} state
 * @param {string=} pageToken cursor from a previous response's nextPageToken
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
 * @param {Array<Object>} places protojson Place messages (camelCase)
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

/**
 * One results row for an Event (protojson, camelCase). Four columns:
 * severity badge · two-line headline+subline (layer · area · relative time) ·
 * source · r<rev>. Clicking the row runs `onSelect(ev, tr, sevColor)` to load
 * Build one result row using the SHARED record row (format.js), the same item
 * the front-page feed, Roads and History render — so a record looks identical
 * wherever it appears.
 *
 * The row is a real <a> to the /event permalink, so middle-click and
 * cmd/ctrl-click open it in a new tab as any link should. A PLAIN left click is
 * intercepted instead and selects the record in-page, which is what the
 * two-column browser is for. Losing the permalink to make selection work would
 * be a regression; losing selection to keep the link would be a different one.
 *
 * @param {Object} ev protojson Event
 * @param {(ev:Object, row:HTMLElement)=>void} onSelect
 */
function eventRow(ev, onSelect) {
  const row = recordRow(ev, { href: `/event?id=${encodeURIComponent(ev.id || '')}` });
  row._select = () => onSelect(ev, row);
  row.addEventListener('click', (e) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return; // let the link win
    e.preventDefault();
    row._select();
  });
  return row;
}

/**
 * Wire up the /events page. Expects the element ids laid out in events.astro
 * (ev-place, ev-layers, ev-status, ev-sev, ev-since, ev-size, ev-apply,
 * ev-reset, ev-qchips, ev-curl, ev-copyurl, ev-errors, ev-list, ev-empty,
 * ev-more, ev-status-line, ev-grid, ev-detail-col, ev-detail, ev-back).
 */
export function initEventsPage() {

  const $ = (id) => document.getElementById(id);
  const placeSel = $('ev-place');
  const layersBox = $('ev-layers');
  const statusBox = $('ev-status');
  const sevBox = $('ev-sev');
  const sinceInput = $('ev-since');
  const sizeSel = $('ev-size');
  const resetBtn = $('ev-reset');
  const urlEl = $('ev-url');
  const copyUrlBtn = $('ev-copyurl');
  const errorsEl = $('ev-errors');
  const listEl = $('ev-list');
  const emptyEl = $('ev-empty');
  const moreBtn = $('ev-more');
  const statusLine = $('ev-status-line');
  const gridEl = $('ev-grid');
  const detailCol = $('ev-detail-col');
  const detailEl = $('ev-detail');
  const backBtn = $('ev-back');
  // Must agree with the @container threshold in events.astro — and be measured
  // the same way. A viewport media query would disagree with the CSS between
  // ~760 and ~1000px (the sidebar's 244px), leaving the layout in two columns
  // while the JS believed it was in one.
  const NARROW_AT = 760;
  const wrapEl = $('ev-results');
  const isNarrow = () => ((wrapEl || gridEl) ? (wrapEl || gridEl).clientWidth < NARROW_AT : window.innerWidth < 1004);
  const narrow = { get matches() { return isNarrow(); } };

  let state = readState(new URLSearchParams(location.search));
  let nextToken = '';
  let loadedCount = 0;
  let loading = false;
  let lastReqUrl = '';
  let selectedRow = null;
  let selectedId = '';

  // ---- controls ----------------------------------------------------

  // Layer and status are multi-select toggle pills (spec §2 /events): each
  // pill is a button carrying its enum value; `.on` = selected.
  function makePills(box, values, selected, labelOf) {
    for (const value of values) {
      const on = selected.includes(value);
      const b = el('button', 'pill' + (on ? ' on' : ''), labelOf(value));
      b.type = 'button';
      b.dataset.value = value;
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
      b.addEventListener('click', () => {
        const now = b.classList.toggle('on');
        b.setAttribute('aria-pressed', now ? 'true' : 'false');
        applyNow();
      });
      box.append(b);
    }
  }

  makePills(layersBox, LAYER_OPTIONS, state.layers, (v) => layerLabel(v));
  makePills(statusBox, STATUS_OPTIONS, state.statuses, (v) => v);

  // severity_min is a FLOOR, so exactly one value applies — a single-select
  // chip group rather than the multi-select pills used for layer and status.
  // '' is "any", which is the API's own default rather than a special case.
  function makeRadioPills(box, values, selected) {
    for (const value of values) {
      const on = value === selected;
      const b = el('button', 'pill' + (on ? ' on' : ''), value === '' ? 'any' : value);
      b.type = 'button';
      b.dataset.value = value;
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
      b.addEventListener('click', () => {
        for (const other of box.querySelectorAll('.pill')) setPill(other, other === b);
        applyNow();
      });
      box.append(b);
    }
  }
  makeRadioPills(sevBox, ['', ...SEVERITY_OPTIONS], state.severityMin);

  for (const size of PAGE_SIZES) {
    const opt = document.createElement('option');
    opt.value = size;
    opt.textContent = size === '' ? 'default' : size;
    sizeSel.append(opt);
  }
  sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';

  sinceInput.value = toLocalInput(state.since);

  // Place select: "(any place)" first; options filled from /api/v1/places.
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
      const data = await get('/api/v1/places');
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

  function setPill(b, on) {
    b.classList.toggle('on', on);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  }

  function readControls() {
    const statuses = [...statusBox.querySelectorAll('.pill.on')].map((b) => b.dataset.value);
    if (statuses.length === 0) {
      // No pills lit = the API default; reflect it back in the UI.
      for (const b of statusBox.querySelectorAll('.pill')) {
        setPill(b, DEFAULT_STATUSES.includes(b.dataset.value));
      }
    }
    return {
      place: placeSel.value,
      layers: [...layersBox.querySelectorAll('.pill.on')].map((b) => b.dataset.value),
      statuses: statuses.length ? statuses : [...DEFAULT_STATUSES],
      severityMin: (sevBox.querySelector('.pill.on') || {}).dataset?.value || '',
      since: fromLocalInput(sinceInput.value) || '',
      pageSize: sizeSel.value,
      pageToken: '', // a filter change always restarts from the first page
    };
  }

  function syncControls() {
    placeSel.value = state.place;
    for (const b of layersBox.querySelectorAll('.pill')) {
      setPill(b, state.layers.includes(b.dataset.value));
    }
    for (const b of statusBox.querySelectorAll('.pill')) {
      setPill(b, state.statuses.includes(b.dataset.value));
    }
    for (const b of sevBox.querySelectorAll('.pill')) {
      setPill(b, b.dataset.value === state.severityMin);
    }
    sinceInput.value = toLocalInput(state.since);
    sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';
  }

  function writeURL() {
    const qs = stateToSearch(state);
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
  }

  /**
   * A control changed: adopt it, rewrite the permalink, refetch from page one.
   * Per the design spec the chips ARE the query — there is no Apply step, so
   * every control funnels through here.
   */
  function applyNow() {
    state = readControls();
    writeURL();
    runQuery('', false);
  }

  placeSel.addEventListener('change', applyNow);
  sinceInput.addEventListener('change', applyNow);
  sizeSel.addEventListener('change', applyNow);

  // ---- echoed query --------------------------------------------------

  function absURL(url) {
    return /^https?:\/\//.test(url) ? url : `https://data.sierragridteam.org${url}`;
  }

  /**
   * Print the exact request behind the results (design spec §2: the GET URL in
   * mono beneath the filter rule).
   *
   * There used to be a second row of removable filter chips here. It duplicated
   * the pills directly above it — which are now live — so the same filter was
   * removable in two places and the panel restated a query the controls already
   * showed. One line, one truth.
   */
  function renderQueryBar(params) {
    lastReqUrl = apiURL('/api/v1/events', params);
    urlEl.textContent = lastReqUrl;
  }

  copyUrlBtn.addEventListener('click', () => {
    navigator.clipboard.writeText(curlFor(absURL(lastReqUrl))).then(
      () => {
        copyUrlBtn.textContent = 'copied';
        setTimeout(() => (copyUrlBtn.textContent = 'copy curl'), 1400);
      },
      () => (copyUrlBtn.textContent = 'failed')
    );
  });

  // ---- selection ------------------------------------------------------

  /**
   * Show one record in the detail column, rendered by the same function the
   * /event permalink page uses. On a narrow viewport the detail REPLACES the
   * list (the design's swap) and a back button appears; on desktop the two sit
   * side by side and the grid gains its second track.
   */
  function selectEvent(ev, row) {
    if (selectedRow) selectedRow.classList.remove('selected');
    selectedRow = row;
    selectedId = ev.id || '';
    if (row) row.classList.add('selected');

    gridEl.classList.add('has-selection');
    detailCol.hidden = false;
    if (narrow.matches) {
      gridEl.classList.add('narrow-detail');
      backBtn.hidden = false;
      detailCol.scrollIntoView({ block: 'start' });
    }
    // setTitle:false — selecting inside the browser must not retitle the page
    // away from "Events"; only the permalink page owns the document title.
    renderEventDetail(detailEl, selectedId, { setTitle: false, headingLevel: 2 });
  }

  /** Narrow-viewport "back to list": drop the detail, restore the list. */
  function clearSelection() {
    gridEl.classList.remove('narrow-detail');
    backBtn.hidden = true;
    if (narrow.matches) {
      // Keep the record selected (so returning to it is one tap) but hide the
      // pane; on desktop the pane stays visible beside the list.
      detailCol.hidden = true;
      gridEl.classList.remove('has-selection');
    }
  }

  backBtn.addEventListener('click', clearSelection);

  // Crossing the 900px boundary changes which pane is authoritative; re-apply
  // so a resize never leaves both hidden or both fighting for the column.
  // No matchMedia to listen to now; watch the grid's own box instead.
  const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(() => onWidthChange()) : null;
  if (ro && (wrapEl || gridEl)) ro.observe(wrapEl || gridEl);
  else window.addEventListener('resize', () => onWidthChange());

  function onWidthChange() {
    if (!selectedId) return;
    gridEl.classList.remove('narrow-detail');
    backBtn.hidden = true;
    detailCol.hidden = false;
    gridEl.classList.add('has-selection');
  }

  // ---- results ------------------------------------------------------

  /**
   * Fetch one page. `token` is the page_token to request ('' = first page);
   * `append` keeps existing rows (Load more) instead of restarting the table.
   */
  async function runQuery(token, append) {
    if (loading) return;
    loading = true;
    moreBtn.disabled = true;

    if (!append) {
      listEl.textContent = '';
      emptyEl.textContent = '';
      loadedCount = 0;
      nextToken = '';
      selectedRow = null;
      selectedId = '';
      detailEl.textContent = '';
      detailCol.hidden = true;
      backBtn.hidden = true;
      gridEl.classList.remove('has-selection', 'narrow-detail');
    }
    errorsEl.textContent = '';

    const params = buildQuery(state, token);
    renderQueryBar(params);
    statusLine.textContent = 'Loading /api/v1/events…';
    statusLine.className = 'loading';

    try {
      const data = await get('/api/v1/events', params);
      const events = Array.isArray(data.events) ? data.events : [];
      let firstRow = null;
      for (const ev of events) {
        const row = eventRow(ev, selectEvent);
        if (!firstRow) firstRow = row;
        listEl.append(row);
      }
      // Open the top (highest-severity, newest) record straight away so the
      // pane is never an empty prompt — but only on desktop: on a phone the
      // detail replaces the list, and auto-selecting would hide the results the
      // reader just asked for.
      if (!append && firstRow && !narrow.matches) firstRow._select();
      loadedCount += events.length;
      nextToken = data.nextPageToken || '';
      moreBtn.hidden = !nextToken;
      moreBtn.disabled = false;

      if (loadedCount === 0) {
        // Distinct from an API error: the query succeeded, nothing matched.
        const empty = el('div', 'notice');
        empty.append(
          el('div', 'mono', 'No events match — this is not an all-clear.'),
          el(
            'div',
            'muted small',
            'The request succeeded (HTTP 200) and returned zero events for these ' +
              'filters — an empty result, not a source failure. A failed feed surfaces ' +
              'as an error above, never as a quiet empty table. Loosen the ' +
              'status/severity filters, clear `since`, or widen the place.'
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
    }
  }

  // ---- events ---------------------------------------------------------

  resetBtn.addEventListener('click', () => {
    // Clear layers/severity/since/place, restore the default status set, and
    // rerun from page one — the canonical empty query.
    state = {
      place: '',
      layers: [],
      statuses: [...DEFAULT_STATUSES],
      severityMin: '',
      since: '',
      pageSize: '',
      pageToken: '',
    };
    syncControls();
    writeURL();
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
        'This URL carries a page_token, so the list starts mid-stream at ' +
          'that cursor. Change any filter to restart from the first page.'
      )
    );
    errorsEl.after(note);
  }

  loadPlaces();
  runQuery(state.pageToken, false);
}
