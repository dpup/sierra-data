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
import { recordRow, fmtNum, timeAgo } from '../format.js';
import { placeMenuOptions, placeMenuLabel } from '../place.js';
import { el, requireEls, errorBlock, errorBand, wireCopyButton } from '../ui.js';
import { renderEventDetail } from './event-detail.js';
// Importing these registers <grid-chip-row> and <grid-menu>.
import '../components/chip-row.js';
import '../components/menu.js';

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
 * Wire up the /events page.
 */
export function initEventsPage() {
  // One resolution, and it FAILS BY NAME. This island used to look up ~25 ids
  // one at a time; a renamed id surfaced as a null-deref somewhere in the
  // middle of init, with the page half-wired.
  const $ = requireEls('events.js', {
    scopeMetaEl: 'ev-scope-meta',
    placeMenu: 'ev-place',
    layersBox: 'ev-layers',
    sevBox: 'ev-sev',
    statusBox: 'ev-status',
    sinceInput: 'ev-since',
    sinceEcho: 'ev-since-echo',
    echoEl: 'ev-echo',
    urlEl: 'ev-url',
    copyUrlBtn: 'ev-copyurl',
    errorsEl: 'ev-errors',
    countEl: 'ev-count',
    listEl: 'ev-list',
    emptyEl: 'ev-empty',
    pageEl: 'ev-page',
    gridEl: 'ev-grid',
    detailCol: 'ev-detail-col',
    detailEl: 'ev-detail',
    backBtn: 'ev-back',
    wrapEl: 'ev-results',
  });
  const {
    scopeMetaEl, placeMenu, layersBox, sevBox, statusBox,
    sinceInput, sinceEcho, echoEl, urlEl, copyUrlBtn, errorsEl,
    countEl, listEl, emptyEl, pageEl, gridEl, detailCol, detailEl,
    backBtn, wrapEl,
  } = $;

  // Must agree with the @container thresholds in events.astro, and be measured
  // the same way: this grid is inset by the 244px sidebar, so a viewport query
  // would disagree with the CSS between ~760 and ~1000px.
  const NARROW_AT = 760;
  const isNarrow = () => (wrapEl ? wrapEl.clientWidth < NARROW_AT : window.innerWidth < 1004);

  let state = readState(new URLSearchParams(location.search));
  let places = [];
  let loadedEvents = [];
  let nextToken = '';
  let pageIndex = 1;
  let loading = false;
  let lastReqUrl = '';
  let lastGoodAt = '';
  let selectedRow = null;
  let selectedId = '';

  /* ---- the echo's height feeds the sticky list column ---------------- */
  function measureEcho() {
    const h = echoEl ? echoEl.getBoundingClientRect().height : 0;
    document.documentElement.style.setProperty('--ev-echo-h', `${Math.round(h) + 8}px`);
  }

  /* ---- A. scope ------------------------------------------------------ */

  const placeLabel = (slug) => placeMenuLabel(places, slug, 'all places');

  /** `summary.totalActive` for the scoped place, or null when not obtainable. */
  let activeCount = null;

  /**
   * Beside the title: how many records are on screen, and how many are active
   * in the scoped place.
   *
   * `/events` returns no total — it is keyset pagination over an opaque cursor —
   * so for a long time this said "live count unknown". That is true of the event
   * list but not of the service: `GET /places/{place}/summary` reports
   * `totalActive` for a place, which is a real, live count. So when a place is
   * scoped, say the number; when it is not, there is no place to count over and
   * the line simply does not claim one.
   *
   * It is never inferred from the loaded page. A count derived from whatever
   * happened to fit in one response would be a fabricated total, which is the
   * one thing this site must not print.
   */
  function renderScope() {
    const n = loadedEvents.length;
    const bits = [`${fmtNum(n)} record${n === 1 ? '' : 's'} loaded`];
    if (state.place) {
      bits.push(
        activeCount === null
          ? 'active count unavailable'
          : `${fmtNum(activeCount)} active in ${placeLabel(state.place)}`
      );
    }
    scopeMetaEl.textContent = bits.join(' · ');
    placeMenu.triggerLabel = placeLabel(state.place).toLowerCase();
    placeMenu.classList.toggle('is-set', Boolean(state.place));
  }

  /**
   * Fetch the scoped place's active count. A failure leaves `activeCount` null,
   * which renders as "active count unavailable" — never as 0, which would read
   * as "nothing is happening here".
   */
  async function loadActiveCount() {
    if (!state.place) {
      activeCount = null;
      renderScope();
      return;
    }
    const want = state.place;
    try {
      const data = await get(`/api/v1/places/${encodeURIComponent(want)}/summary`);
      if (want !== state.place) return; // the scope changed while in flight
      const total = data && data.summary && data.summary.totalActive;
      activeCount = typeof total === 'number' ? total : null;
    } catch {
      if (want === state.place) activeCount = null;
    }
    renderScope();
  }

  function renderPlaceMenu() {
    placeMenu.options = placeMenuOptions(places, {
      anyLabel: 'All places',
      current: state.place,
      group: true,
    });
    placeMenu.value = state.place;
  }

  placeMenu.addEventListener('change', (e) => {
    state.place = e.detail.value;
    activeCount = null;
    renderScope();
    loadActiveCount();
    writeURL();
    runQuery('', false);
  });

  /* ---- B. filter region ----------------------------------------------
     Plain and always visible. The chips ARE the query: every click refetches,
     so there is no draft state, no APPLY and nothing to expand. An earlier pass
     put all of this behind an EDIT panel with a commit step; it was more
     machinery than four facets deserve. */

  // Each facet is a <grid-chip-row>: the element owns the chips, the selection
  // and the aria state, and tells us when it changed. What is left here is the
  // only part that is actually about events — which options exist, and what a
  // change means for the query.

  function wire(row, onChange) {
    row.addEventListener('change', (e) => {
      onChange(e.detail.value);
      applyNow();
    });
  }

  layersBox.options = [
    { value: '', label: 'all' },
    ...LAYER_OPTIONS.map((v) => ({ value: v, label: v })),
  ];
  wire(layersBox, (v) => { state.layers = v; });

  sevBox.options = [
    { value: '', label: 'any' },
    ...SEVERITY_OPTIONS.map((v) => ({ value: v, label: v })),
  ];
  wire(sevBox, (v) => { state.severityMin = v; });

  // STATUS is two states, not four checkboxes. The endpoint takes any subset of
  // ACTIVE/SCHEDULED/RESOLVED/EXPIRED, but the question a reader has is "am I
  // seeing things that are over?". `open only` is the API default (and so is
  // omitted from the request); the full subset stays expressible in the URL,
  // which the echo prints.
  statusBox.options = [
    { value: 'open', label: 'open only' },
    { value: 'all', label: 'include closed' },
  ];
  wire(statusBox, (v) => {
    state.statuses = v === 'all' ? [...STATUS_OPTIONS] : [...DEFAULT_STATUSES];
  });

  /** Push the current state into the controls (boot, popstate, empty-state chips). */
  function buildControls() {
    layersBox.value = state.layers;
    sevBox.value = state.severityMin;
    statusBox.value = isDefaultStatuses(state.statuses) ? 'open' : 'all';
    sinceInput.value = toLocalInput(state.since);
    renderSinceEcho();
  }

  /**
   * SINCE echoes the exact value it will send and the offset applied — it is
   * the only local-time control on a page that is otherwise strictly Z.
   */
  function renderSinceEcho() {
    const iso = fromLocalInput(sinceInput.value);
    if (!iso) {
      sinceEcho.textContent = '→ since omitted';
      sinceEcho.classList.add('omitted');
      return;
    }
    sinceEcho.classList.remove('omitted');
    const mins = -new Date().getTimezoneOffset();
    const sign = mins >= 0 ? '+' : '-';
    const a = Math.abs(mins);
    const off = `${sign}${String(Math.floor(a / 60)).padStart(2, '0')}:${String(a % 60).padStart(2, '0')}`;
    sinceEcho.textContent = `→ sends since=${iso} (your time ${off})`;
  }
  sinceInput.addEventListener('input', renderSinceEcho);
  sinceInput.addEventListener('change', () => {
    state.since = fromLocalInput(sinceInput.value) || '';
    applyNow();
  });

  /** A control changed: rewrite the permalink and refetch from page one. */
  function applyNow() {
    state.pageToken = '';
    buildControls();
    writeURL();
    runQuery('', false);
  }

  /* ---- C. request echo ------------------------------------------------ */

  function absURL(url) {
    return /^https?:\/\//.test(url) ? url : `https://data.sierragridteam.org${url}`;
  }

  /**
   * Print the exact request, with defaulted params dimmed and explicit ones in
   * cream — the same distinction the collapsed summary makes, carried into the
   * URL so the two can never disagree.
   */
  function renderEcho(params) {
    lastReqUrl = apiURL('/api/v1/events', params);
    // The ABSOLUTE url: this line exists to be copied, and a relative path is
    // not something anyone can paste anywhere. Defaults never appear in it —
    // they are omitted from the request, so stating them here would be a lie
    // about the wire; line 2 names them instead.
    urlEl.textContent = absURL(lastReqUrl);
  }

  /**
   * The echo is ONE line: the request.
   *
   * It carried a second line — "50 records loaded · page_size 50 · cursor page 1
   * · more available · server sort … · shown newest ingested first" — which
   * restated the filter controls sitting directly above it and the count sitting
   * beside the title. Three surfaces describing the same query is two too many.
   *
   * NOTE ON THE ABSENT TOTAL. `EventList` is {events, nextPageToken} — keyset
   * pagination over an opaque cursor, with no total and no page count. Nothing
   * here or in the header may imply otherwise; where a real count IS obtainable
   * it comes from the place summary (see renderScope).
   */

  // The URL tracks the filters, so the payload is read at click time.
  wireCopyButton(copyUrlBtn, () => curlFor(absURL(lastReqUrl)));

  /* ---- D. list -------------------------------------------------------- */

  /**
   * Newest first, always. There is no sort control: the server orders by
   * severity and the page re-orders by recency, which is the order a feed is
   * read in. `ingestedAt` is the key rather than `observedAt` because the store
   * stamps it on every write — it always exists and is monotonic, where an
   * upstream `observedAt` can be absent or (mesh nodes) dated in the future.
   * It is also the age the row displays, so the sort matches what you can see.
   */
  function sortLoaded(list) {
    return list
      .slice()
      .sort(
        (a, b) =>
          (Date.parse(b.ingestedAt || 0) || 0) - (Date.parse(a.ingestedAt || 0) || 0) ||
          (Date.parse(b.observedAt || 0) || 0) - (Date.parse(a.observedAt || 0) || 0) ||
          String(a.id || '').localeCompare(String(b.id || ''))
      );
  }

  function eventRow(ev) {
    // A real <a> to the permalink, so middle-click and cmd-click behave. A plain
    // left click is intercepted and selects in-page instead — losing the
    // permalink to make selection work would be a regression, and so would the
    // reverse.
    const row = recordRow(ev, {
      href: `/event?id=${encodeURIComponent(ev.id || '')}`,
      timeBlock: true,
      selected: ev.id === selectedId,
    });
    row.addEventListener('click', (e) => {
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
      e.preventDefault();
      selectEvent(ev, row);
    });
    return row;
  }

  function renderList() {
    listEl.textContent = '';
    selectedRow = null;
    for (const ev of sortLoaded(loadedEvents)) {
      const row = eventRow(ev);
      if (ev.id === selectedId) selectedRow = row;
      listEl.append(row);
    }
    countEl.textContent = loadedEvents.length
      ? `${fmtNum(loadedEvents.length)} RESULT${loadedEvents.length === 1 ? '' : 'S'}`
      : ' ';
    // The header's count reads from the same list, so it cannot disagree with
    // the one above the rows — and it refreshes here rather than only at boot,
    // where it raced the places fetch and printed 0 beside seven results.
    renderScope();
    renderPagination();
  }

  function renderPagination() {
    pageEl.textContent = '';
    if (!loadedEvents.length) return;
    if (!nextToken && pageIndex === 1) {
      pageEl.append(el('span', '', `ALL ${fmtNum(loadedEvents.length)} RESULTS SHOWN`));
      return;
    }
    // No total exists (see the note above renderEcho), so this counts loaded and
    // says whether more follow — it never claims "of N".
    pageEl.append(
      el('span', '', `SHOWING ${fmtNum(loadedEvents.length)} FROM ${fmtNum(pageIndex)} CURSOR PAGE${pageIndex === 1 ? '' : 'S'}`)
    );
    if (nextToken) {
      const b = el('button', 'ev-next', 'NEXT PAGE →');
      b.type = 'button';
      b.addEventListener('click', () => {
        const token = nextToken;
        runQuery(token, true).then(() => {
          state.pageToken = token;
          writeURL();
        });
      });
      pageEl.append(b);
    } else {
      pageEl.append(el('span', '', '· END OF RESULTS'));
    }
  }

  /* ---- selection ------------------------------------------------------ */

  function selectEvent(ev, row) {
    if (selectedRow) selectedRow.classList.remove('selected');
    selectedRow = row;
    selectedId = ev.id || '';
    if (row) row.classList.add('selected');

    gridEl.classList.add('has-selection');
    detailCol.hidden = false;
    backBtn.textContent = `← BACK TO ${fmtNum(loadedEvents.length)} RESULTS`;
    if (isNarrow()) {
      gridEl.classList.add('narrow-detail');
      backBtn.hidden = false;
      detailCol.scrollIntoView({ block: 'start' });
    }
    // setTitle:false — selecting inside the browser must not retitle the page
    // away from "Events"; only the permalink page owns the document title.
    renderEventDetail(detailEl, selectedId, { setTitle: false, headingLevel: 2 });
  }

  backBtn.addEventListener('click', () => {
    gridEl.classList.remove('narrow-detail');
    backBtn.hidden = true;
    if (isNarrow()) {
      detailCol.hidden = true;
      gridEl.classList.remove('has-selection');
    }
  });

  const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(() => onWidthChange()) : null;
  if (ro && wrapEl) ro.observe(wrapEl);
  else window.addEventListener('resize', onWidthChange);

  function onWidthChange() {
    measureEcho();
    if (!selectedId) return;
    if (!isNarrow()) {
      gridEl.classList.remove('narrow-detail');
      backBtn.hidden = true;
      detailCol.hidden = false;
      gridEl.classList.add('has-selection');
    }
  }

  /* ---- fetching -------------------------------------------------------- */

  function skeleton() {
    const wrap = el('div');
    wrap.append(el('div', 'ev-fetching', 'FETCHING /events…'));
    for (let i = 0; i < 3; i++) {
      const row = el('div', 'ev-skel-row');
      const body = el('div', 'ev-skel-body');
      body.append(el('div', 'ev-skel-bar w1'), el('div', 'ev-skel-bar w2'), el('div', 'ev-skel-bar w3'));
      row.append(el('div', 'ev-skel-spine'), body);
      wrap.append(row);
    }
    return wrap;
  }

  function renderEmpty() {
    emptyEl.textContent = '';
    const box = el('div');
    box.append(el('div', 'ev-empty-head', '0 RESULTS'));
    box.append(
      el('p', 'prose', 'The request succeeded and matched nothing. That is an empty result, not a source failure — a failed feed surfaces as a red band above, never as a quiet empty list.')
    );
    const chips = el('div');
    const removable = [];
    if (state.severityMin) {
      removable.push([`remove severity ≥ ${state.severityMin}`, () => { state.severityMin = ''; }]);
    }
    for (const l of state.layers) {
      removable.push([`remove layer ${l}`, () => { state.layers = state.layers.filter((x) => x !== l); }]);
    }
    if (state.since) removable.push([`remove since ${state.since}`, () => { state.since = ''; }]);
    if (!isDefaultStatuses(state.statuses)) {
      removable.push(['reset status to default', () => { state.statuses = [...DEFAULT_STATUSES]; }]);
    }
    if (state.place) removable.push([`widen scope from ${placeLabel(state.place)}`, () => { state.place = ''; renderScope(); }]);

    for (const [label, apply] of removable) {
      const b = el('button', 'ev-remove-chip', label);
      b.type = 'button';
      b.addEventListener('click', () => {
        apply();
        applyNow();
      });
      chips.append(b);
    }
    if (removable.length) box.append(chips);
    emptyEl.append(box);
  }

  function writeURL() {
    const qs = stateToSearch(state);
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
  }

  async function runQuery(token, append) {
    if (loading) return;
    loading = true;

    if (!append) {
      listEl.textContent = '';
      emptyEl.textContent = '';
      pageEl.textContent = '';
      loadedEvents = [];
      pageIndex = 1;
      nextToken = '';
      selectedRow = null;
      selectedId = '';
      detailEl.textContent = '';
      detailCol.hidden = true;
      backBtn.hidden = true;
      gridEl.classList.remove('has-selection', 'narrow-detail');
      listEl.append(skeleton());
    }
    errorsEl.textContent = '';
    listEl.classList.remove('ev-stale-body');

    const params = buildQuery(state, token);
    renderEcho(params);

    try {
      const data = await get('/api/v1/events', params);
      const events = Array.isArray(data.events) ? data.events : [];
      loadedEvents = append ? [...loadedEvents, ...events] : events;
      if (append) pageIndex += 1;
      nextToken = data.nextPageToken || '';
      lastGoodAt = new Date().toISOString();

      renderList();

      if (!loadedEvents.length) {
        renderEmpty();
      } else if (!append && !isNarrow()) {
        // Open the top record so the pane is never an empty prompt — but not on
        // a phone, where the detail replaces the list and auto-selecting would
        // hide the results the reader just asked for.
        const first = listEl.querySelector('.rec');
        if (first) first.click();
      }
    } catch (err) {
      // The last good data stays on screen, dimmed and labelled. A failure that
      // blanks the list is indistinguishable from a genuinely empty result.
      listEl.querySelectorAll('.ev-skel-row, .ev-fetching').forEach((n) => n.remove());
      errorsEl.append(errorBand(err, lastGoodAt, () => runQuery(token, append)));
      if (loadedEvents.length) {
        listEl.classList.add('ev-stale-body');
        errorsEl.append(el('div', 'ev-lastgood', `LAST GOOD RESPONSE · ${timeAgo(lastGoodAt)}`));
      }
    } finally {
      loading = false;
    }
  }

  /* ---- boot ------------------------------------------------------------ */

  window.addEventListener('popstate', () => {
    state = readState(new URLSearchParams(location.search));
    renderScope();
    buildControls();
    runQuery(state.pageToken, false);
  });
  window.addEventListener('resize', measureEcho);

  (async () => {
    renderScope();
    buildControls();
    measureEcho();
    try {
      const data = await get('/api/v1/places');
      places = Array.isArray(data.places) ? data.places : [];
    } catch (err) {
      // The scope still works from the URL; say why the picker is bare.
      errorsEl.append(errorBand(err, '', () => location.reload()));
    }
    renderScope();
    renderPlaceMenu();
    loadActiveCount();
  })();

  runQuery(state.pageToken, false);
}
