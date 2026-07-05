// pages/sources.js — the /sources feed-health board (T16).
//
// Fetches GET /v1/sources (SourceList protojson, snake_case) and renders the
// ops table: unhealthy-first, row-tinted for non-OK, auto-refreshing every
// 30s while the tab is visible. Filter state lives in the URL (?status=).
//
// Pure helpers (sorting, interval humanizing, counting, URL state) have no
// DOM access at import time so node can import and test this module.

import { get, ApiError, apiURL } from '../api.js';
import { timeCell, timeAgo, timeAbs, sourceDot } from '../format.js';
import { initChrome } from '../nav.js';

/* ------------------------------------------------------------------ */
/* Pure helpers (node-testable)                                       */
/* ------------------------------------------------------------------ */

/** Unhealthy-first rank. UNKNOWN (missing/unspecified status) sits between
 * STALE and OK — not proven healthy, not proven broken. */
export const STATUS_RANK = { UNAVAILABLE: 0, STALE: 1, UNKNOWN: 2, OK: 3 };

/** Statuses a ?status= filter may take, in display order. */
export const FILTERABLE = ['UNAVAILABLE', 'STALE', 'UNKNOWN', 'OK'];

/**
 * Normalize a wire status to OK|STALE|UNAVAILABLE|UNKNOWN.
 * protojson omits zero-valued enums, so a missing `status` (or the explicit
 * SOURCE_STATUS_UNSPECIFIED name) maps to UNKNOWN.
 * @param {string|undefined} s
 * @returns {string}
 */
export function normStatus(s) {
  const v = String(s ?? '').toUpperCase();
  return v === 'OK' || v === 'STALE' || v === 'UNAVAILABLE' ? v : 'UNKNOWN';
}

/**
 * Humanize poll_interval_seconds: 300 -> "5m", 5400 -> "1h 30m",
 * 90 -> "1m 30s", 45 -> "45s", 172800 -> "2d". "—" for missing/invalid.
 * @param {number|string|null|undefined} seconds
 * @returns {string}
 */
export function humanizeInterval(seconds) {
  const n = Number(seconds);
  if (seconds === null || seconds === undefined || seconds === '' || Number.isNaN(n) || n <= 0) {
    return '—';
  }
  const total = Math.round(n);
  const d = Math.floor(total / 86400);
  const h = Math.floor((total % 86400) / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const parts = [];
  if (d) parts.push(`${d}d`);
  if (h) parts.push(`${h}h`);
  if (m) parts.push(`${m}m`);
  if (s) parts.push(`${s}s`);
  // Keep it dense: at most the two most significant units.
  return parts.slice(0, 2).join(' ');
}

/**
 * Comparator: unhealthy first (UNAVAILABLE, STALE, UNKNOWN, OK); within a
 * status group by last_success_at ascending — the longest-silent source
 * first, and never-succeeded (missing stamp) ahead of everything in its
 * group. Ties break on id for stable ordering across refreshes.
 * @param {Object} a Source
 * @param {Object} b Source
 * @returns {number}
 */
export function compareSources(a, b) {
  const ra = STATUS_RANK[normStatus(a.status)];
  const rb = STATUS_RANK[normStatus(b.status)];
  if (ra !== rb) return ra - rb;
  const ta = a.last_success_at ? Date.parse(a.last_success_at) : -Infinity;
  const tb = b.last_success_at ? Date.parse(b.last_success_at) : -Infinity;
  const na = Number.isNaN(ta) ? -Infinity : ta;
  const nb = Number.isNaN(tb) ? -Infinity : tb;
  if (na !== nb) return na - nb;
  return String(a.id ?? '').localeCompare(String(b.id ?? ''));
}

/**
 * Sort a copy of the source list unhealthy-first (see compareSources).
 * @param {Array<Object>} sources
 * @returns {Array<Object>}
 */
export function sortSources(sources) {
  return [...sources].sort(compareSources);
}

/**
 * Count sources per normalized status.
 * @param {Array<Object>} sources
 * @returns {{total:number, OK:number, STALE:number, UNAVAILABLE:number, UNKNOWN:number}}
 */
export function summarize(sources) {
  const out = { total: sources.length, OK: 0, STALE: 0, UNAVAILABLE: 0, UNKNOWN: 0 };
  for (const s of sources) out[normStatus(s.status)] += 1;
  return out;
}

/**
 * Apply a status filter ('' = no filter).
 * @param {Array<Object>} sources
 * @param {string} status normalized status or ''
 * @returns {Array<Object>}
 */
export function filterByStatus(sources, status) {
  if (!status) return sources;
  return sources.filter((s) => normStatus(s.status) === status);
}

/**
 * Read the ?status= filter from a query string; invalid values -> ''.
 * @param {string} search e.g. "?status=stale"
 * @returns {string} normalized status or ''
 */
export function statusFromSearch(search) {
  const v = new URLSearchParams(search || '').get('status');
  if (!v) return '';
  const norm = String(v).toUpperCase();
  return FILTERABLE.includes(norm) ? norm : '';
}

/**
 * Produce the query string for a filter value ('' clears it), preserving no
 * other params — ?status= is this page's whole URL state.
 * @param {string} status
 * @returns {string} "" or "?status=STALE"
 */
export function searchForStatus(status) {
  return status ? `?status=${encodeURIComponent(status)}` : '';
}

/**
 * Truncate untrusted text for a table cell; full text goes in title.
 * @param {string} text
 * @param {number=} max
 * @returns {string}
 */
export function truncate(text, max = 90) {
  const t = String(text ?? '');
  if (t.length <= max) return t;
  return `${t.slice(0, max - 1)}…`;
}

/**
 * Only http(s) URLs are linkable — homepage_url is upstream-derived.
 * @param {string} url
 * @returns {boolean}
 */
export function isLinkableURL(url) {
  return typeof url === 'string' && /^https?:\/\//i.test(url);
}

/** Display host of a URL, for compact link text. */
export function hostOf(url) {
  try {
    return new URL(url).host || url;
  } catch {
    return String(url ?? '');
  }
}

/* ------------------------------------------------------------------ */
/* Page wiring (browser only — called from sources.html)              */
/* ------------------------------------------------------------------ */

const REFRESH_MS = 30_000;

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
    el('div', 'error-url', err instanceof ApiError ? `GET ${err.url}` : String(err.message || err))
  );
  if (err instanceof ApiError && err.body && typeof err.body === 'object' && err.body.message) {
    div.append(el('div', 'muted', err.body.message));
  }
  return div;
}

/** One-time page init. */
export function initSourcesPage() {
  initChrome('sources');

  const summaryBox = document.getElementById('summary-line');
  const alertBox = document.getElementById('degradation-alert');
  const errorBox = document.getElementById('page-errors');
  const boardBox = document.getElementById('board');
  const refreshLine = document.getElementById('refresh-line');

  const state = {
    sources: null, // last-good source list (null until first success)
    loadedAt: null, // ISO stamp of last successful load
    filter: statusFromSearch(location.search),
    loading: false,
  };

  function setFilter(status) {
    state.filter = status === state.filter ? '' : status; // click again to clear
    history.replaceState(null, '', location.pathname + searchForStatus(state.filter));
    render();
  }

  function renderSummary() {
    summaryBox.textContent = '';
    if (!state.sources) return;
    const counts = summarize(state.sources);
    summaryBox.append(el('span', 'mono', `${counts.total} source${counts.total === 1 ? '' : 's'} — `));
    const chips = [
      ['OK', `${counts.OK} ok`],
      ['STALE', `${counts.STALE} stale`],
      ['UNAVAILABLE', `${counts.UNAVAILABLE} unavailable`],
    ];
    if (counts.UNKNOWN > 0) chips.push(['UNKNOWN', `${counts.UNKNOWN} unknown`]);
    chips.forEach(([status, label], i) => {
      if (i > 0) summaryBox.append(el('span', 'muted mono', ', '));
      const btn = el('button', `chip-filter st-${status}`, label);
      btn.type = 'button';
      btn.setAttribute('aria-pressed', state.filter === status ? 'true' : 'false');
      btn.title =
        state.filter === status
          ? 'Click to clear this filter'
          : `Show only ${status} sources (?status=${status})`;
      btn.addEventListener('click', () => setFilter(status));
      summaryBox.append(btn);
    });
    if (state.filter) {
      const clear = el('button', 'chip-filter clear', 'clear filter');
      clear.type = 'button';
      clear.addEventListener('click', () => setFilter(''));
      summaryBox.append(el('span', 'muted mono', ' '), clear);
    }
  }

  function renderAlert() {
    alertBox.textContent = '';
    if (!state.sources) return;
    const bad = sortSources(state.sources).filter((s) => normStatus(s.status) !== 'OK');
    if (bad.length === 0) return;
    const worst = normStatus(bad[0].status);
    const block = el('div', worst === 'UNAVAILABLE' ? 'error-block' : 'notice');
    block.setAttribute('role', 'alert');
    block.textContent =
      `${bad.length} source${bad.length === 1 ? '' : 's'} degraded: ` +
      bad.map((s) => `${s.id || s.name || '?'} ${normStatus(s.status)}`).join(', ');
    alertBox.append(block);
  }

  const COLUMNS = [
    'Status',
    'ID',
    'Name',
    'Poll',
    'Last success',
    'Last attempt',
    'Last error',
    'Attribution',
    'Upstream',
  ];

  function renderRow(s) {
    const status = normStatus(s.status);
    const tr = el('tr', status === 'OK' ? '' : `row-${status}`);

    const statusTd = el('td');
    statusTd.append(sourceDot(s.status));
    tr.append(statusTd);

    const idTd = el('td');
    idTd.append(el('code', '', s.id ?? '—'));
    tr.append(idTd);

    tr.append(el('td', 'wrap', s.name || '—'));

    const pollTd = el('td', 'num', humanizeInterval(s.poll_interval_seconds));
    if (s.poll_interval_seconds !== undefined && s.poll_interval_seconds !== null) {
      pollTd.title = `${s.poll_interval_seconds}s`;
    }
    tr.append(pollTd);

    for (const iso of [s.last_success_at, s.last_attempt_at]) {
      const td = el('td');
      if (iso) td.append(timeCell(iso));
      else td.append(el('span', 'muted', 'never'));
      tr.append(td);
    }

    const errTd = el('td', 'wrap err-cell');
    if (s.last_error) {
      errTd.textContent = truncate(s.last_error); // textContent: untrusted
      errTd.title = s.last_error;
    }
    tr.append(errTd);

    tr.append(el('td', 'wrap muted', s.attribution || ''));

    const linkTd = el('td');
    if (isLinkableURL(s.homepage_url)) {
      const a = el('a', '', hostOf(s.homepage_url));
      a.href = s.homepage_url;
      a.rel = 'noopener';
      a.target = '_blank';
      linkTd.append(a);
    } else {
      linkTd.append(el('span', 'muted', '—'));
    }
    tr.append(linkTd);

    return tr;
  }

  function renderBoard() {
    boardBox.textContent = '';
    if (!state.sources) return; // first load still pending or failed: error block speaks
    if (state.sources.length === 0) {
      boardBox.append(el('div', 'notice', 'No sources registered.'));
      return;
    }
    const rows = filterByStatus(sortSources(state.sources), state.filter);
    if (rows.length === 0) {
      const none = el('div', 'notice');
      none.append(
        el('span', '', `No sources with status ${state.filter}. `),
        (() => {
          const b = el('button', 'chip-filter clear', 'clear filter');
          b.type = 'button';
          b.addEventListener('click', () => setFilter(''));
          return b;
        })()
      );
      boardBox.append(none);
      return;
    }

    const wrap = el('div', 'table-wrap');
    const table = el('table', 'data-table');
    const thead = el('thead');
    const headRow = el('tr');
    for (const c of COLUMNS) headRow.append(el('th', c === 'Poll' ? 'num' : '', c));
    thead.append(headRow);
    const tbody = el('tbody');
    for (const s of rows) tbody.append(renderRow(s));
    table.append(thead, tbody);
    wrap.append(table);
    boardBox.append(wrap);
  }

  function renderRefreshLine() {
    refreshLine.textContent = '';
    if (state.loadedAt) {
      refreshLine.append(
        el(
          'span',
          '',
          `Loaded ${timeAgo(state.loadedAt)} (${timeAbs(state.loadedAt)}) from `
        ),
        el('code', 'inline', apiURL('/v1/sources')),
        el('span', '', ` — auto-refreshes every ${REFRESH_MS / 1000}s while this tab is visible.`)
      );
    } else {
      refreshLine.textContent = `Auto-refreshes every ${REFRESH_MS / 1000}s while this tab is visible.`;
    }
  }

  function render() {
    renderSummary();
    renderAlert();
    renderBoard();
    renderRefreshLine();
  }

  async function load() {
    if (state.loading) return;
    state.loading = true;
    try {
      const data = await get('/v1/sources');
      state.sources = Array.isArray(data.sources) ? data.sources : [];
      state.loadedAt = new Date().toISOString();
      errorBox.textContent = '';
      render();
    } catch (err) {
      // Fail loud, never blank: name the URL; keep the last-good table with a
      // staleness note rather than wiping the board mid-incident.
      errorBox.textContent = '';
      errorBox.append(errorBlock(err));
      if (!state.sources) {
        // First load failed: clear the "Loading…" placeholder so the error
        // block above is the page's whole (honest) state.
        boardBox.textContent = '';
      }
      if (state.sources && state.loadedAt) {
        errorBox.append(
          el(
            'div',
            'notice',
            `Showing the last successful load from ${timeAgo(state.loadedAt)} (${timeAbs(state.loadedAt)}).`
          )
        );
      }
    } finally {
      state.loading = false;
    }
  }

  // Initial load + 30s auto-refresh, paused while the tab is hidden.
  load();
  setInterval(() => {
    if (!document.hidden) load();
  }, REFRESH_MS);
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    const age = state.loadedAt ? Date.now() - Date.parse(state.loadedAt) : Infinity;
    if (age >= REFRESH_MS) load();
  });

  // Keep the filter in sync if the user navigates history (e.g. pasted URL).
  window.addEventListener('popstate', () => {
    state.filter = statusFromSearch(location.search);
    render();
  });
}
