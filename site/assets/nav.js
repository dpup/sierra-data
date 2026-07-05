// nav.js — shared page chrome: header nav + unprivileged-client footer.
//
// Pages call initChrome('events') after their <main> exists; the header is
// prepended to <body>, the footer appended. The footer's request log renders
// live from api.js `requests` and updates on every API_REQUEST_EVENT.
//
// No DOM access at import time — everything happens inside initChrome().

import { requests, API_REQUEST_EVENT, curlFor } from './api.js';

const NAV_LINKS = [
  { key: 'home', label: 'Home', href: '/' },
  { key: 'sources', label: 'Sources', href: '/sources.html' },
  { key: 'events', label: 'Events', href: '/events.html' },
  { key: 'places', label: 'Places', href: '/places.html' },
  { key: 'map', label: 'Map', href: '/map.html' },
  { key: 'history', label: 'History', href: '/history.html' },
  { key: 'docs', label: 'Docs', href: '/docs.html' },
];

function buildHeader(current) {
  const header = document.createElement('header');
  header.className = 'site-header';

  const brand = document.createElement('a');
  brand.className = 'brand';
  brand.href = '/';
  const name = document.createElement('span');
  name.className = 'brand-name';
  name.textContent = 'SIERRA Grid Data';
  const host = document.createElement('span');
  host.className = 'brand-host';
  host.textContent = 'data.sierragridteam.org';
  brand.append(name, host);

  const nav = document.createElement('nav');
  nav.className = 'site-nav';
  nav.setAttribute('aria-label', 'Site');
  for (const link of NAV_LINKS) {
    const a = document.createElement('a');
    a.href = link.href;
    a.textContent = link.label;
    if (link.key === current) {
      a.className = 'current';
      a.setAttribute('aria-current', 'page');
    }
    nav.appendChild(a);
  }

  header.append(brand, nav);
  return header;
}

function renderRequestLog(listEl, countEl) {
  listEl.textContent = '';
  if (requests.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'request-log-empty';
    empty.textContent = 'No API requests made by this page yet.';
    listEl.appendChild(empty);
  }
  for (const entry of requests) {
    const row = document.createElement('div');
    row.className = 'request-log-row' + (entry.ok ? '' : ' failed');

    const status = document.createElement('span');
    status.className = 'request-log-status';
    status.textContent =
      entry.status === null ? '…' : entry.status === 0 ? 'ERR' : String(entry.status);

    const code = document.createElement('code');
    code.className = 'request-log-curl';
    code.textContent = curlFor(entry.url);

    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'copy-btn';
    copy.textContent = 'copy';
    copy.addEventListener('click', () => {
      navigator.clipboard.writeText(curlFor(entry.url)).then(
        () => {
          copy.textContent = 'copied';
          setTimeout(() => (copy.textContent = 'copy'), 1200);
        },
        () => {
          copy.textContent = 'failed';
        }
      );
    });

    row.append(status, code, copy);
    listEl.appendChild(row);
  }
  countEl.textContent = String(requests.length);
}

function buildFooter() {
  const footer = document.createElement('footer');
  footer.className = 'site-footer';

  const line = document.createElement('p');
  line.className = 'footer-line';
  line.textContent =
    'This site is an unprivileged client of the public API — every number on ' +
    'this page came from a browser fetch of /v1/* that you can replay yourself.';

  const details = document.createElement('details');
  details.className = 'request-log';
  const summary = document.createElement('summary');
  summary.append('View the ');
  const countEl = document.createElement('span');
  countEl.className = 'request-count';
  countEl.textContent = '0';
  summary.append(countEl, ' request(s) behind this page');
  const listEl = document.createElement('div');
  listEl.className = 'request-log-list';
  details.append(summary, listEl);

  footer.append(line, details);

  renderRequestLog(listEl, countEl);
  document.addEventListener(API_REQUEST_EVENT, () =>
    renderRequestLog(listEl, countEl)
  );

  return footer;
}

/**
 * Render the shared header and footer.
 * @param {string} current nav key of the current page:
 *   'home'|'sources'|'events'|'places'|'map'|'history'|'docs'
 */
export function initChrome(current) {
  document.body.prepend(buildHeader(current));
  document.body.append(buildFooter());
}
