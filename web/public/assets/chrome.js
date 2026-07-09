// chrome.js — live behaviors for the app shell (Shell.astro renders the static
// markup; this fills the placeholders). Split out of the old nav.js initChrome()
// so the shell no longer has to be built client-side.
//
// Three live bits, matching the previous behavior exactly:
//   - sidebar health chip  → #health-dot / #health-text (a plain, unlogged fetch
//     of /api/v1/sources; kept OUT of the per-page request drawer since it's chrome)
//   - context-bar clock    → #ctx-clock (ticks once a second)
//   - footer request log    → #req-log-list / #req-count (the api.js request log;
//     every value a page shows is a replayable GET)

import { requests, API_REQUEST_EVENT, curlFor } from './api.js';

const $ = (id) => document.getElementById(id);
const el = (tag, className, text) => {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text;
  return n;
};

/* -------------------------------------------------- sidebar health chip */

function loadHealth() {
  const dot = $('health-dot');
  const text = $('health-text');
  if (!dot || !text) return;
  fetch('/api/v1/sources', { headers: { Accept: 'application/json' } })
    .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
    .then((body) => {
      const list = (body && body.sources) || [];
      const total = list.length;
      const ok = list.filter((s) => s.status === 'OK').length;
      text.textContent = `${ok} / ${total} sources OK`;
      dot.className = 'dot live ' + (ok === total ? 'st-OK' : ok === 0 ? 'st-UNAVAILABLE' : 'st-STALE');
    })
    .catch(() => {
      text.textContent = 'source health unavailable';
      dot.className = 'dot st-UNAVAILABLE';
    });
}

/* ------------------------------------------------------ context-bar clock */

function startClock() {
  const ctime = $('ctx-clock');
  if (!ctime) return;
  const p = (n) => String(n).padStart(2, '0');
  const tick = () => {
    const d = new Date();
    ctime.textContent = `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}Z`;
  };
  tick();
  setInterval(tick, 1000);
}

/* -------------------------------------------------------- footer request log */

function renderRequestLog(listEl, countEl) {
  listEl.textContent = '';
  if (requests.length === 0) {
    listEl.append(el('div', 'request-log-empty', 'No API requests made by this page yet.'));
  }
  for (const entry of requests) {
    const row = el('div', 'request-log-row' + (entry.ok ? '' : ' failed'));
    const status = el(
      'span',
      'request-log-status',
      entry.status === null ? '…' : entry.status === 0 ? 'ERR' : String(entry.status)
    );
    const code = el('code', 'request-log-curl', curlFor(entry.url));
    const copy = el('button', 'copy-btn', 'copy');
    copy.type = 'button';
    copy.addEventListener('click', () => {
      navigator.clipboard.writeText(curlFor(entry.url)).then(
        () => {
          copy.textContent = 'copied';
          setTimeout(() => (copy.textContent = 'copy'), 1200);
        },
        () => (copy.textContent = 'failed')
      );
    });
    row.append(status, code, copy);
    listEl.append(row);
  }
  countEl.textContent = String(requests.length);
}

function startRequestLog() {
  const listEl = $('req-log-list');
  const countEl = $('req-count');
  if (!listEl || !countEl) return;
  renderRequestLog(listEl, countEl);
  document.addEventListener(API_REQUEST_EVENT, () => renderRequestLog(listEl, countEl));
}

/* ----------------------------------------------------------------- init */

loadHealth();
startClock();
startRequestLog();
