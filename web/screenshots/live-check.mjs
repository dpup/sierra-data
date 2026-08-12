// live-check.mjs — drive the BUILT page against the real API.
//
// WHY THIS EXISTS. `fixtures.mjs` answers every request deterministically, which
// is what makes screenshots diffable — and is also a blind spot. Two real bugs
// hid in it:
//
//   * The map bound 3 of 9 layers on load. The fixture harness blocks tile
//     requests, so the fallback basemap style loaded instantly and the race
//     against `style.load` never opened. With real tiles it reproduced first try.
//   * A copy icon overflowed a clipped id line and scrolled the phone sideways.
//     It only escapes when the id is long enough to fill the line, and live ids
//     (`calfire:cec0e1f9-…`) are while fixture ids (`evt-evac-e043`) are not.
//
// So: content-driven and network-driven faults need real content and a real
// network. Reach for this after touching the map, anything that measures, or
// anything whose size depends on the data.
//
//   node screenshots/live-check.mjs map            # desktop
//   node screenshots/live-check.mjs index 390      # a phone
//   node screenshots/live-check.mjs events 1600 --shot
//
// Reports: every API call with its status/latency/size, page errors, whether
// the document scrolls horizontally, and what overflows if it does.

import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';

const ORIGIN = process.env.GRID_ORIGIN || 'https://data.sierragridteam.org';
const ROOT = '/workspace/site/dist';
const TYPES = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript',
  '.json': 'application/json', '.woff2': 'font/woff2', '.svg': 'image/svg+xml', '.png': 'image/png' };

const argv = process.argv.slice(2);
const shot = argv.includes('--shot');
const [page = 'index', widthArg] = argv.filter((a) => !a.startsWith('--'));
const width = Number(widthArg || 1600);

const server = http.createServer((req, res) => {
  let p = decodeURIComponent(req.url.split('?')[0]);
  if (p.endsWith('/')) p += 'index.html';
  const f = path.join(ROOT, p);
  if (!fs.existsSync(f) || fs.statSync(f).isDirectory()) { res.writeHead(404); res.end('nf'); return; }
  res.writeHead(200, { 'content-type': TYPES[path.extname(f)] || 'application/octet-stream' });
  fs.createReadStream(f).pipe(res);
});
await new Promise((r) => server.listen(0, r));
const port = server.address().port;

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width, height: 900 }, deviceScaleFactor: 2 });

const calls = [];
await ctx.route('**/*', async (route) => {
  const u = new URL(route.request().url());
  if (u.host === `localhost:${port}` && /^\/(api\/)?v1\//.test(u.pathname)) {
    const t0 = Date.now();
    try {
      const res = await fetch(`${ORIGIN}${u.pathname}${u.search}`, { headers: { accept: 'application/json' } });
      const body = await res.text();
      calls.push({ status: res.status, ms: Date.now() - t0, kb: Math.round(body.length / 1024), path: u.pathname + u.search });
      return route.fulfill({ status: res.status, contentType: res.headers.get('content-type') || 'application/json', body });
    } catch (e) {
      calls.push({ status: 'ERR', ms: Date.now() - t0, kb: 0, path: u.pathname + ' ' + e.message });
      return route.abort();
    }
  }
  // Everything else — tiles, fonts — goes to the real network on purpose.
  return route.continue();
});

const pg = await ctx.newPage();
const errors = [];
pg.on('pageerror', (e) => errors.push('pageerror: ' + String(e).slice(0, 220)));
pg.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text().slice(0, 220)); });

await pg.goto(`http://localhost:${port}/${page}.html`, { waitUntil: 'domcontentloaded' });
await pg.waitForTimeout(9000); // live upstreams are slower than fixtures

const report = await pg.evaluate(() => {
  const winW = window.innerWidth;
  const rows = [];
  for (const el of document.querySelectorAll('body *')) {
    const r = el.getBoundingClientRect();
    if (!r.width || !r.height || r.right <= winW + 1) continue;
    // Inside a scroll container it scrolls there, not the page.
    if (el.closest('.table-wrap, .resp-pane, .code, pre, .ep-pane, .request-log-list')) continue;
    const p = [];
    for (let n = el; n && n !== document.body; n = n.parentElement) {
      p.unshift(n.tagName.toLowerCase() + (n.className && n.className.toString ? '.' + n.className.toString().split(/\s+/)[0] : ''));
    }
    rows.push({ right: Math.round(r.right), path: p.slice(-5).join(' > ') });
  }
  const seen = new Set();
  return {
    winW,
    docWidth: document.documentElement.scrollWidth,
    hScroll: document.documentElement.scrollWidth > winW + 1,
    offenders: rows.filter((o) => (seen.has(o.path) ? false : seen.add(o.path))).slice(0, 8),
  };
});

console.log(`${page}.html @${width}px against ${ORIGIN}\n`);
console.log(`  hScroll: ${report.hScroll ? `YES — document ${report.docWidth}px in a ${report.winW}px viewport` : 'no'}`);
for (const o of report.offenders) console.log(`    right=${o.right}  ${o.path}`);
console.log(`\n  ${calls.length} API call(s):`);
for (const c of calls) console.log(`    ${String(c.status).padStart(3)} ${String(c.ms).padStart(5)}ms ${String(c.kb).padStart(4)}kB  ${c.path}`);
if (errors.length) {
  console.log('\n  PAGE ERRORS:');
  for (const e of errors) console.log('    ' + e);
}
if (shot) {
  await pg.screenshot({ path: `screenshots/out/live-${page}.png`, fullPage: true });
  console.log(`\n  screenshots/out/live-${page}.png`);
}

await browser.close();
server.close();
process.exit(report.hScroll || errors.length ? 1 : 0);
