// nav.js — shared app shell: fixed sidebar + sticky context bar + footer drawer.
//
// The shell wraps each page's <main>: pages ship a plain <main>…</main> and call
// initChrome('<key>') once it exists. We move that <main> into a content column,
// prepend the sidebar, and top/tail it with the context bar and footer.
//
// The sidebar health chip and the context-bar clock are live chrome: the chip
// fetches /v1/sources directly (a plain fetch, kept OUT of the per-page request
// drawer so each drawer stays scoped to that page's own data), the clock ticks
// once a second. The footer drawer reuses api.js's request log — every value a
// page shows is a replayable GET.
//
// No DOM access at import time; everything runs inside initChrome().

import { requests, API_REQUEST_EVENT, curlFor } from './api.js';

// key → { label, href, hint, group, crumb }
const NAV = [
  { key: 'home', label: 'Grid Info', href: '/', hint: '/v1', group: 'OVERVIEW', crumb: 'The Grid' },
  { key: 'events', label: 'Events', href: '/events.html', hint: '/events', group: 'EXPLORE', crumb: 'Events' },
  { key: 'map', label: 'Map', href: '/map.html', hint: '/map', group: 'EXPLORE', crumb: 'Map' },
  { key: 'places', label: 'Places', href: '/places.html', hint: '/places', group: 'EXPLORE', crumb: 'Places' },
  { key: 'sources', label: 'Sources', href: '/sources.html', hint: '/sources', group: 'EXPLORE', crumb: 'Sources' },
  { key: 'history', label: 'History', href: '/history.html', hint: '/history', group: 'EXPLORE', crumb: 'History' },
  { key: 'docs', label: 'Docs', href: '/docs.html', hint: '/v1 ref', group: 'REFERENCE', crumb: 'Docs' },
];
const GROUPS = ['OVERVIEW', 'EXPLORE', 'REFERENCE'];

function el(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text;
  return n;
}

/* -------------------------------------------------------------- sidebar */

function buildSidebar(current) {
  const aside = el('aside', 'sidebar');

  // brand
  const brand = el('a', 'brand-block');
  brand.href = '/';
  const line = el('div', 'brand-line');
  line.append(el('span', 'brand-name', 'S.I.E.R.R.A'), el('span', 'brand-sub', 'The Grid'));
  brand.append(line, el('div', 'brand-host', 'data.sierragridteam.org'));

  // health chip (live)
  const health = el('div', 'health-chip');
  const hrow = el('div', 'health-row');
  const hdot = el('span', 'dot st-OK live');
  const htext = el('span', undefined, 'checking sources…');
  hrow.append(hdot, htext);
  health.append(hrow, el('div', 'health-sub', 'read-only · CORS-open · no key'));

  // nav groups
  const nav = el('nav', 'nav');
  nav.setAttribute('aria-label', 'Sections');
  for (const g of GROUPS) {
    nav.append(el('div', 'nav-group-label', g));
    for (const item of NAV.filter((n) => n.group === g)) {
      const a = el('a', 'nav-item' + (item.key === current ? ' current' : ''));
      a.href = item.href;
      if (item.key === current) a.setAttribute('aria-current', 'page');
      a.append(el('span', undefined, item.label), el('span', 'hint', item.hint));
      nav.append(a);
    }
  }

  const foot = el('div', 'sidebar-foot');
  foot.append(el('div', undefined, 'grid.v1 · schema 2026-07'), el('div', undefined, 'deploy from main · us-west'));

  aside.append(brand, health, nav, foot);
  loadHealth(hdot, htext);
  return aside;
}

// Fetch /v1/sources for the chip. Plain fetch (unlogged) — chrome, not page data.
function loadHealth(dot, text) {
  fetch('/v1/sources', { headers: { Accept: 'application/json' } })
    .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
    .then((body) => {
      const list = (body && body.sources) || [];
      const total = list.length;
      const ok = list.filter((s) => (s.status || s.source_status) === 'OK').length;
      text.textContent = `${ok} / ${total} sources OK`;
      dot.className = 'dot live ' + (ok === total ? 'st-OK' : ok === 0 ? 'st-UNAVAILABLE' : 'st-STALE');
    })
    .catch(() => {
      text.textContent = 'source health unavailable';
      dot.className = 'dot st-UNAVAILABLE';
    });
}

/* ---------------------------------------------------------- context bar */

function buildContextBar(current) {
  const bar = el('div', 'context-bar');
  const crumb = el('div', 'crumb');
  const here = (NAV.find((n) => n.key === current) || {}).crumb || 'The Grid';
  crumb.append(el('span', undefined, 'grid.v1'), ' ', el('span', 'sep', '/'), ' ', el('span', 'here', here));

  const right = el('div', 'ctx-right');
  const clock = el('span', 'clock');
  const cdot = el('span', 'dot st-OK live');
  const ctime = el('span', undefined, '––:––:––Z');
  clock.append(cdot, ctime);
  right.append(clock);
  bar.append(crumb, right);

  const tick = () => {
    const d = new Date();
    const p = (n) => String(n).padStart(2, '0');
    ctime.textContent = `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}Z`;
  };
  tick();
  setInterval(tick, 1000);
  return bar;
}

/* --------------------------------------------------------------- footer */

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

function buildFooter() {
  const footer = el('footer', 'site-footer');

  const details = el('details', 'request-log');
  const summary = el('summary');
  const countEl = el('span', 'request-count', '0');
  summary.append('View the ', countEl, ' request(s) behind this page');
  const listEl = el('div', 'request-log-list');
  details.append(summary, listEl);

  const legal = el(
    'p',
    'footer-line',
    'Signal Integrity & Emergency Radio Response Alliance · P.O. Box 2071, Murphys, CA 95247 · ' +
      'a volunteer 501(c)(3). Every value on this site is a browser fetch of the public /v1 API — replay any of them yourself.'
  );

  footer.append(details, legal);
  renderRequestLog(listEl, countEl);
  document.addEventListener(API_REQUEST_EVENT, () => renderRequestLog(listEl, countEl));
  return footer;
}

/* ----------------------------------------------------------------- init */

/**
 * Wrap the page's <main> in the app shell.
 * @param {string} current nav key: home|events|map|places|sources|history|docs
 */
export function initChrome(current) {
  const main = document.querySelector('main');
  const app = el('div', 'app');
  const content = el('div', 'content');
  content.append(buildContextBar(current));
  if (main) content.append(main);
  content.append(buildFooter());
  app.append(buildSidebar(current), content);
  document.body.prepend(app);
}
