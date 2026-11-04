// relay-contract.mjs — the /relay page's fail-loud rules, driven rather than
// photographed (the map-contract.mjs reasoning: none of these states is
// reachable by loading the page against fixtures).
//
//   node screenshots/relay-contract.mjs      (after `make site`)
//
// 1. Relay down on first load  -> an error naming the request, the word UNKNOWN,
//    and NO board and NO counts (never "0 ready", never an empty table).
// 2. Relay down after a good read -> the last-good board stays, under a notice
//    that says it may be out of date.
// 3. Zero gateways -> a confirmed-empty notice, distinct from 1.
// 4. Happy path -> one row per gateway, least usable first.

import { chromium } from 'playwright';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, extname } from 'node:path';
import { routeFor } from './fixtures.mjs';

const DIST = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'site', 'dist');
const MIME = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.woff2': 'font/woff2', '.svg': 'image/svg+xml' };

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://localhost');
  let fp = join(DIST, decodeURIComponent(u.pathname));
  if (!existsSync(fp) && existsSync(fp + '.html')) fp += '.html';
  if (!existsSync(fp)) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'content-type': MIME[extname(fp)] || 'application/octet-stream' });
  res.end(await readFile(fp));
});
await new Promise((r) => server.listen(0, r));
const base = `http://localhost:${server.address().port}`;

let mode = 'ok'; // ok | down | empty
const browser = await chromium.launch();
const ctx = await browser.newContext();
await ctx.route('**/*', async (route) => {
  const u = new URL(route.request().url());
  if (u.host !== new URL(base).host) return route.abort();
  if (u.pathname === '/mesh/relay/api/status') {
    if (mode === 'down') return route.fulfill({ status: 502, contentType: 'text/html', body: '<html>502 Bad Gateway</html>' });
    const body = mode === 'empty' ? { generated_at: new Date().toISOString(), gateways: [] } : routeFor(u.pathname, u.searchParams);
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
  }
  if (/^\/api\/v1\//.test(u.pathname)) {
    const d = routeFor(u.pathname, u.searchParams);
    return route.fulfill({ status: d ? 200 : 404, contentType: 'application/json', body: JSON.stringify(d ?? {}) });
  }
  return route.continue();
});

let failed = 0;
function check(name, ok, detail = '') {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok ? '' : `  ${detail}`}`);
  if (!ok) failed++;
}
async function snapshot(page) {
  return page.evaluate(() => ({
    errors: document.getElementById('relay-errors').innerText,
    errorIsBlock: Boolean(document.querySelector('#relay-errors .error-block')),
    errorIsNotice: Boolean(document.querySelector('#relay-errors .notice')),
    rows: document.querySelectorAll('#relay-board tbody tr').length,
    board: document.getElementById('relay-board').innerText,
    summary: document.getElementById('relay-summary').innerText,
    firstState: document.querySelector('#relay-board tbody tr .status-chip')?.textContent || '',
  }));
}
async function refresh(page) {
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await page.waitForTimeout(400);
}

const page = await ctx.newPage();
const pageErrors = [];
page.on('pageerror', (e) => pageErrors.push(String(e)));

mode = 'down';
await page.goto(`${base}/relay.html`, { waitUntil: 'networkidle' });
let s = await snapshot(page);
check('down on first load: loud error block', s.errorIsBlock, JSON.stringify(s));
check('down on first load: says UNKNOWN', /UNKNOWN/.test(s.errors), s.errors);
check('down on first load: names the request', s.errors.includes('/mesh/relay/api/status'), s.errors);
check('down on first load: no board', s.rows === 0, `${s.rows} rows`);
check('down on first load: no counts', s.summary.trim() === '', s.summary);

mode = 'ok';
await refresh(page);
s = await snapshot(page);
check('recovers: board drawn, error cleared', s.rows === 6 && s.errors === '', JSON.stringify(s));
check('least usable first', s.firstState === 'OFFLINE', s.firstState);

mode = 'down';
await refresh(page);
s = await snapshot(page);
check('down after a good read: last-good board kept', s.rows === 6, `${s.rows} rows`);
check('down after a good read: flagged as possibly stale', s.errorIsNotice && /last good/.test(s.errors), s.errors);

mode = 'empty';
await refresh(page);
s = await snapshot(page);
check('zero gateways: confirmed-empty notice', /No gateways are registered/.test(s.board) && s.rows === 0, s.board);

check('no page errors', pageErrors.length === 0, pageErrors.join('; '));

await browser.close();
server.close();
console.log(failed ? `\n${failed} check(s) failed` : '\nall relay contract checks passed');
process.exit(failed ? 1 : 0);
