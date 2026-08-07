// chrome.js — live behaviors for the app shell (Shell.astro renders the static
// markup; this fills the placeholders).
//
// Five live bits:
//   - source-health line   → #health-dot / #health-text (+ the mobile top-bar
//     pair) from a plain, unlogged fetch of /api/v1/sources; kept OUT of the
//     per-page request drawer since it is chrome, not the page's own data
//   - UTC clock            → #sidebar-clock, ticking once a second
//   - mobile nav drawer    → #nav-toggle slides #sidebar in, Esc / outside click
//     / selecting a destination closes it
//   - footer request log   → #req-log-list / #req-count / #side-req-count; every
//     value a page shows is a replayable GET

import { requests, API_REQUEST_EVENT, curlFor } from './api.js';

const $ = (id) => document.getElementById(id);
const el = (tag, className, text) => {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text;
  return n;
};

/* ---------------------------------------------------- source-health line */

function loadHealth() {
  const targets = [
    [$('health-dot'), $('health-text')],
    [$('health-dot-m'), $('health-text-m')],
  ].filter(([d, t]) => d && t);
  // The top-bar copy is a visual duplicate of the sidebar's line; announcing
  // both would read the same status twice.
  const mobileText = $('health-text-m');
  if (mobileText && mobileText.parentElement) {
    mobileText.parentElement.setAttribute('aria-hidden', 'true');
  }
  if (!targets.length) return;

  const set = (cls, text) => {
    for (const [dot, label] of targets) {
      dot.className = 'dot ' + cls;
      label.textContent = text;
    }
  };

  fetch('/api/v1/sources', { headers: { Accept: 'application/json' } })
    .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
    .then((body) => {
      const list = (body && body.sources) || [];
      const total = list.length;
      const ok = list.filter((s) => s.status === 'OK').length;
      if (total === 0) {
        // An empty registry is not a healthy one: 0 === 0 would otherwise
        // satisfy the all-OK test and paint the pill green while we in fact
        // know nothing about any feed.
        set('st-UNAVAILABLE', 'no sources reported');
        return;
      }
      set(
        'live ' + (ok === total ? 'st-OK' : ok === 0 ? 'st-UNAVAILABLE' : 'st-STALE'),
        `${ok} / ${total} sources OK`
      );
    })
    // A failed health fetch is itself a fact worth stating — never leave the
    // dot on its optimistic initial green.
    .catch(() => set('st-UNAVAILABLE', 'source health unavailable'));
}

/* ------------------------------------------------------------ UTC clock */

function startClock() {
  const faces = [$('sidebar-clock')].filter(Boolean);
  if (!faces.length) return;
  const p = (n) => String(n).padStart(2, '0');
  const tick = () => {
    const d = new Date();
    const t = `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}Z`;
    for (const f of faces) f.textContent = t;
  };
  tick();
  setInterval(tick, 1000);
}

/* ----------------------------------------------------- mobile nav drawer */

function startNavDrawer() {
  const toggle = $('nav-toggle');
  const sidebar = $('sidebar');
  if (!toggle || !sidebar) return;

  // Below 900px the drawer is hidden by transform alone, which does NOT remove
  // it from the tab order or the accessibility tree: without `inert` its 11
  // links stay focusable behind the page and a screen reader announces the
  // whole nav (plus a second copy of the health line) while it is off-screen.
  const mobile = window.matchMedia('(max-width: 900px)');

  const applyInert = () => {
    const hidden = mobile.matches && !sidebar.classList.contains('open');
    sidebar.inert = hidden;
    // aria-hidden mirrors inert for the (older) AT that ignore inert.
    if (hidden) sidebar.setAttribute('aria-hidden', 'true');
    else sidebar.removeAttribute('aria-hidden');
  };

  const setOpen = (open) => {
    sidebar.classList.toggle('open', open);
    toggle.setAttribute('aria-expanded', String(open));
    applyInert();
    if (open) {
      // Move focus into the drawer so the next Tab walks the nav rather than
      // the page behind the scrim.
      const first = sidebar.querySelector('a, button, select');
      if (first) first.focus();
    } else if (document.activeElement && sidebar.contains(document.activeElement)) {
      // Closing from inside the drawer must not strand focus on a hidden node.
      toggle.focus();
    }
  };

  applyInert();
  mobile.addEventListener('change', applyInert);

  toggle.addEventListener('click', (e) => {
    e.stopPropagation();
    setOpen(!sidebar.classList.contains('open'));
  });

  // Selecting a destination closes the drawer (the navigation itself would too,
  // but the close must be immediate so the tap feels answered).
  sidebar.addEventListener('click', (e) => {
    if (e.target.closest('a')) setOpen(false);
  });

  // The scrim is a box-shadow, not an element, so "click outside" is a document
  // listener rather than a scrim handler.
  document.addEventListener('click', (e) => {
    if (!sidebar.classList.contains('open')) return;
    if (!sidebar.contains(e.target)) setOpen(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !sidebar.classList.contains('open')) return;
    setOpen(false);
    toggle.focus(); // Esc returns focus to the control that opened it
  });
}

/* -------------------------------------------------------- request log */

function renderRequestLog(listEl, countEl, sideCountEl) {
  listEl.textContent = '';
  if (requests.length === 0) {
    listEl.append(el('div', 'request-log-empty', 'No API requests made by this page yet.'));
  }
  for (const entry of requests) {
    const row = el('div', 'request-log-row' + (entry.ok ? '' : ' failed'));
    const status = el(
      'span',
      'request-log-status',
      entry.status === null
        ? '…'
        : entry.timedOut
          ? 'T/O'
          : entry.status === 0
            ? 'ERR'
            : String(entry.status)
    );
    if (entry.error) row.title = entry.error;
    const code = el('code', 'request-log-curl', curlFor(entry.url));
    const copy = el('button', 'copy-btn', 'copy');
    copy.type = 'button';
    copy.addEventListener('click', () => {
      navigator.clipboard.writeText(curlFor(entry.url)).then(
        () => {
          copy.textContent = 'copied';
          setTimeout(() => (copy.textContent = 'copy'), 1400);
        },
        () => (copy.textContent = 'failed')
      );
    });
    row.append(status, code, copy);
    listEl.append(row);
  }
  countEl.textContent = String(requests.length);
  if (sideCountEl) sideCountEl.textContent = String(requests.length);
}

function startRequestLog() {
  const listEl = $('req-log-list');
  const countEl = $('req-count');
  const sideCountEl = $('side-req-count');
  if (!listEl || !countEl) return;
  renderRequestLog(listEl, countEl, sideCountEl);
  document.addEventListener(API_REQUEST_EVENT, () =>
    renderRequestLog(listEl, countEl, sideCountEl)
  );
}

/* ----------------------------------------------------------------- init */

loadHealth();
startClock();
startNavDrawer();
startRequestLog();
