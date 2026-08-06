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
import { sevChip, layerLabel, timeAgo, timeAbs, SEVERITY_COLORS } from '../format.js';

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
 * the raw JSON into the inspector; the headline stays a deep link to the event
 * detail page (stopPropagation so it navigates instead of inspecting).
 * @param {Object} ev protojson Event
 * @param {(ev:Object, tr:HTMLElement, sevColor:string)=>void} onSelect
 */
function eventRow(ev, onSelect) {
  // protojson omits default enum values: absent severity is INFO.
  const sev = String(ev.severity || 'INFO').toUpperCase();
  const sevColor = SEVERITY_COLORS[sev] || 'var(--grn-line)';

  const tr = el('tr', 'ev-row');

  const sevTd = document.createElement('td');
  sevTd.append(sevChip(sev));
  tr.append(sevTd);

  const headTd = el('td', 'wrap');
  const head = el('div', 'ev-head');
  if (ev.id) {
    const link = document.createElement('a');
    link.href = `/event?id=${encodeURIComponent(ev.id)}`;
    link.textContent = ev.headline || ev.id; // textContent: upstream text is untrusted
    link.title = 'open event detail';
    link.addEventListener('click', (e) => e.stopPropagation());
    head.append(link);
  } else {
    head.textContent = ev.headline || '(no headline)';
  }
  const sub = el('div', 'ev-sub');
  sub.append(document.createTextNode(layerLabel(ev.layer || '')));
  sub.append(el('span', 'sep', ' · '));
  sub.append(document.createTextNode(ev.areaLabel || '—'));
  sub.append(el('span', 'sep', ' · '));
  sub.append(document.createTextNode(timeAgo(ev.observedAt || '')));
  sub.title = timeAbs(ev.observedAt || '');
  headTd.append(head, sub);
  tr.append(headTd);

  tr.append(el('td', 'src', (ev.provenance && ev.provenance.sourceId) || '—'));
  // revision is a proto uint32; protojson omits 0.
  tr.append(el('td', 'num', 'r' + String(ev.revision ?? 0)));

  // On phones the row reflows into a stacked card (app.css ≤640px); caption the
  // secondary columns so Source/Rev don't clip off-screen. Severity/Event carry
  // their own badge + headline, so they read fine without a caption.
  const tds = tr.querySelectorAll(':scope > td');
  if (tds[2]) tds[2].dataset.label = 'Source';
  if (tds[3]) tds[3].dataset.label = 'Revision';

  tr._select = () => onSelect(ev, tr, sevColor);
  tr.addEventListener('click', tr._select);
  return tr;
}

/** A syntax-highlight <span> (keys/strings/numbers/bools/punctuation). */
function jspan(cls, text) {
  const s = document.createElement('span');
  s.className = cls;
  s.textContent = text;
  return s;
}

/**
 * Pretty-print a value as syntax-colored JSON into a DocumentFragment, built
 * entirely from text nodes and classed spans (never innerHTML). Keys, strings,
 * numbers, and booleans/null are colored; whitespace and structural
 * punctuation fall through to the .jpunc class.
 * @param {*} value
 * @returns {DocumentFragment}
 */
function highlightJSON(value) {
  const frag = document.createDocumentFragment();
  const text = JSON.stringify(value, null, 2);
  if (text === undefined) {
    frag.append(document.createTextNode(String(value)));
    return frag;
  }
  const re = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b/g;
  let last = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) frag.append(jspan('jpunc', text.slice(last, m.index)));
    if (m[1] !== undefined) {
      const isKey = m[2] !== undefined;
      frag.append(jspan(isKey ? 'jkey' : 'jstr', m[1]));
      if (isKey) frag.append(jspan('jpunc', m[2]));
    } else if (m[3] !== undefined) {
      frag.append(jspan('jnum', m[3]));
    } else if (m[4] !== undefined) {
      frag.append(jspan('jbool', m[4]));
    }
    last = re.lastIndex;
  }
  if (last < text.length) frag.append(jspan('jpunc', text.slice(last)));
  return frag;
}

/**
 * Wire up the /events page. Expects the element ids laid out in events.html
 * (ev-place, ev-layers, ev-status, ev-sev, ev-since, ev-size, ev-apply,
 * ev-reset, ev-qchips, ev-curl, ev-copyurl, ev-errors, ev-tbody, ev-empty,
 * ev-more, ev-status-line, ev-insp-body).
 */
export function initEventsPage() {

  const $ = (id) => document.getElementById(id);
  const placeSel = $('ev-place');
  const layersBox = $('ev-layers');
  const statusBox = $('ev-status');
  const sevSel = $('ev-sev');
  const sinceInput = $('ev-since');
  const sizeSel = $('ev-size');
  const applyBtn = $('ev-apply');
  const resetBtn = $('ev-reset');
  const qchipsEl = $('ev-qchips');
  const curlEl = $('ev-curl');
  const copyUrlBtn = $('ev-copyurl');
  const errorsEl = $('ev-errors');
  const tbody = $('ev-tbody');
  const emptyEl = $('ev-empty');
  const moreBtn = $('ev-more');
  const statusLine = $('ev-status-line');
  const inspBody = $('ev-insp-body');

  let state = readState(new URLSearchParams(location.search));
  let nextToken = '';
  let loadedCount = 0;
  let loading = false;
  let lastReqUrl = '';
  let selectedTr = null;

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
      });
      box.append(b);
    }
  }

  makePills(layersBox, LAYER_OPTIONS, state.layers, (v) => layerLabel(v));
  makePills(statusBox, STATUS_OPTIONS, state.statuses, (v) => v);

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
      severityMin: sevSel.value,
      since: fromLocalInput(sinceInput.value) || '',
      pageSize: sizeSel.value,
      pageToken: '', // Apply always restarts from the first page
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
    sevSel.value = state.severityMin;
    sinceInput.value = toLocalInput(state.since);
    sizeSel.value = PAGE_SIZES.includes(state.pageSize) ? state.pageSize : '';
  }

  function writeURL() {
    const qs = stateToSearch(state);
    history.replaceState(null, '', qs ? `?${qs}` : location.pathname);
  }

  // ---- query bar ----------------------------------------------------

  function absURL(url) {
    return /^https?:\/\//.test(url) ? url : `https://data.sierragridteam.org${url}`;
  }

  /**
   * Removing a filter chip mutates state in place, resets pagination, mirrors
   * the change back into the controls, rewrites the permalink, and re-runs the
   * query from page one — the chip set is the live, editable query.
   */
  function removeFilter(mutate) {
    mutate();
    state.pageToken = '';
    syncControls();
    writeURL();
    runQuery('', false);
  }

  /**
   * Render the request behind the current results: the endpoint, one removable
   * .qchip per active filter (status/layer repeat), and the copyable curl line.
   * The default status set (ACTIVE,SCHEDULED) is omitted — it is the API's own
   * default and never a chip, so the chips are exactly the URL's params.
   */
  function renderQueryBar(params) {
    lastReqUrl = apiURL('/api/v1/events', params);
    curlEl.textContent = curlFor(lastReqUrl);

    qchipsEl.textContent = '';
    const chip = (key, value, onRemove) => {
      const c = el('span', 'qchip');
      c.append(el('span', 'k', key + '='), document.createTextNode(value));
      const x = el('span', 'x', '✕');
      x.setAttribute('role', 'button');
      x.setAttribute('tabindex', '0');
      x.setAttribute('aria-label', `remove ${key}=${value} filter`);
      x.title = `remove ${key} filter`;
      x.addEventListener('click', onRemove);
      x.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onRemove();
        }
      });
      c.append(x);
      qchipsEl.append(c);
    };

    if (state.place) {
      chip('place', state.place, () => removeFilter(() => { state.place = ''; }));
    }
    for (const layer of state.layers) {
      chip('layer', layer, () =>
        removeFilter(() => {
          state.layers = state.layers.filter((l) => l !== layer);
        })
      );
    }
    if (!isDefaultStatuses(state.statuses)) {
      for (const st of state.statuses) {
        chip('status', st, () =>
          removeFilter(() => {
            state.statuses = state.statuses.filter((s) => s !== st);
            if (!state.statuses.length) state.statuses = [...DEFAULT_STATUSES];
          })
        );
      }
    }
    if (state.severityMin) {
      chip('severity_min', state.severityMin, () => removeFilter(() => { state.severityMin = ''; }));
    }
    if (state.since) {
      chip('since', state.since, () => removeFilter(() => { state.since = ''; }));
    }
    if (state.pageSize) {
      chip('page_size', state.pageSize, () => removeFilter(() => { state.pageSize = ''; }));
    }
    if (!qchipsEl.children.length) {
      qchipsEl.append(el('span', 'qbar-hint', 'no filters · status defaults to ACTIVE,SCHEDULED'));
    }
  }

  copyUrlBtn.addEventListener('click', () => {
    navigator.clipboard.writeText(absURL(lastReqUrl)).then(
      () => {
        copyUrlBtn.textContent = 'copied';
        setTimeout(() => (copyUrlBtn.textContent = 'copy url'), 1200);
      },
      () => {
        copyUrlBtn.textContent = 'failed';
      }
    );
  });

  // ---- inspector ----------------------------------------------------

  function inspectorPlaceholder() {
    inspBody.textContent = '';
    inspBody.append(
      jspan(
        'insp-placeholder',
        'Select a row to inspect its raw grid.v1.Event JSON —\n' +
          'the exact bytes GET /api/v1/events returned for that occurrence.'
      )
    );
  }

  function selectEvent(ev, tr, sevColor) {
    if (selectedTr) {
      selectedTr.classList.remove('ev-selected');
      selectedTr.style.removeProperty('--row-sev');
    }
    selectedTr = tr;
    tr.classList.add('ev-selected');
    tr.style.setProperty('--row-sev', sevColor);
    inspBody.textContent = '';
    inspBody.append(highlightJSON(ev));
  }

  inspectorPlaceholder();

  // ---- results ------------------------------------------------------

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
      selectedTr = null;
      inspectorPlaceholder();
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
        tbody.append(row);
      }
      // Populate the inspector immediately with the top (highest-severity,
      // newest) row, so the raw JSON is visible without a first click.
      if (!append && firstRow) firstRow._select();
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
      applyBtn.disabled = false;
    }
  }

  // ---- events ---------------------------------------------------------

  applyBtn.addEventListener('click', () => {
    state = readControls();
    writeURL(); // the permalink is the artifact: Apply writes the full query
    runQuery('', false);
  });

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
        'This URL carries a page_token, so the table starts mid-stream at ' +
          'that cursor. Press Apply to restart from the first page.'
      )
    );
    errorsEl.after(note);
  }

  loadPlaces();
  runQuery(state.pageToken, false);
}
