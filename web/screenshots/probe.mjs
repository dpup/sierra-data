// probe.mjs — ask the built page what it actually renders.
//
// WHY THIS EXISTS. Screenshots are for judging whether something looks right;
// they are useless for judging *how big* or *what colour* it is, because the
// image you look at has been scaled. Reading a downscaled capture produced four
// wrong conclusions in one session — a black deck read as white, left-aligned
// keys read as right-aligned, and a type scale that was 30% too small read as
// fine. Every one of them was settled in seconds by asking the DOM.
//
// Reach for this BEFORE claiming anything dimensional: font sizes, colours,
// spacing, widths, whether an element is present or visible.
//
//   node screenshots/probe.mjs /events.html '.rec-head' '.rec-id' '.chip-toggle'
//   node screenshots/probe.mjs /event.html?id=evt-wildfire-mudflat '.kv dt'
//   node screenshots/probe.mjs --width 390 /events.html '.ev-filters'
//
// Prints, per selector: how many matched, then for the first match its font,
// colour, background, box and whether it is visible. Data comes from
// fixtures.mjs, so it matches what `make site-shots-mock` captured.

import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { routeFor } from './fixtures.mjs';

const ROOT = '/workspace/site/dist';
const TYPES = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript',
  '.json': 'application/json', '.woff2': 'font/woff2', '.svg': 'image/svg+xml', '.png': 'image/png' };

const argv = process.argv.slice(2);
let width = 1440;
const wi = argv.indexOf('--width');
if (wi !== -1) {
  width = Number(argv[wi + 1]);
  argv.splice(wi, 2);
}
const [pageUrl, ...selectors] = argv;
if (!pageUrl || !selectors.length) {
  console.error('usage: node screenshots/probe.mjs [--width N] /page.html <selector>...');
  process.exit(2);
}

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
const ctx = await browser.newContext({ viewport: { width, height: 1000 } });
await ctx.route('**/*', async (route) => {
  const url = new URL(route.request().url());
  const same = url.host === `localhost:${port}`;
  if (same && /^\/(api\/)?v1\//.test(url.pathname)) {
    const data = routeFor(url.pathname, url.searchParams);
    if (data === null) return route.fulfill({ status: 404, contentType: 'application/json', body: '{}' });
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  }
  if (!same) return route.abort(); // external tiles/fonts stay offline
  return route.continue();
});

const page = await ctx.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e).slice(0, 200)));
await page.goto(`http://localhost:${port}${pageUrl}`, { waitUntil: 'networkidle' });
await page.waitForTimeout(800);

console.log(`${pageUrl}  @${width}px\n`);
for (const sel of selectors) {
  const info = await page.evaluate((s) => {
    const all = document.querySelectorAll(s);
    const n = all[0];
    if (!n) return { count: 0 };
    const c = getComputedStyle(n);
    const r = n.getBoundingClientRect();
    return {
      count: all.length,
      text: (n.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 52),
      font: `${c.fontSize}/${c.lineHeight} ${c.fontFamily.split(',')[0].replace(/"/g, '')} ${c.fontWeight}`,
      color: c.color,
      background: c.backgroundColor,
      // All four sides. Reporting only top+left hid a border-BOTTOM entirely,
      // which is the one edge this design system uses most (section rules).
      border: [c.borderTopWidth, c.borderRightWidth, c.borderBottomWidth, c.borderLeftWidth]
        .every((w) => w === '0px')
        ? 'none'
        : `T ${c.borderTopWidth} R ${c.borderRightWidth} B ${c.borderBottomWidth} ` +
          `L ${c.borderLeftWidth} · ${c.borderBottomStyle} ${c.borderBottomColor}`,
      box: `${Math.round(r.width)}x${Math.round(r.height)} at ${Math.round(r.left)},${Math.round(r.top)}`,
      padding: `${c.paddingTop} ${c.paddingRight} ${c.paddingBottom} ${c.paddingLeft}`,
      margin: `${c.marginTop} ${c.marginRight} ${c.marginBottom} ${c.marginLeft}`,
      // offsetParent is null for display:none and for fixed elements; the rect
      // check disambiguates, so "visible" means it actually occupies space.
      visible: r.width > 0 && r.height > 0 && c.visibility !== 'hidden' && c.display !== 'none',
    };
  }, sel);

  if (!info.count) {
    console.log(`${sel}\n  NOT FOUND\n`);
    continue;
  }
  console.log(`${sel}   (${info.count} match${info.count === 1 ? '' : 'es'})`);
  console.log(`  text       ${info.text || '(empty)'}`);
  console.log(`  font       ${info.font}`);
  console.log(`  color      ${info.color}   bg ${info.background}`);
  console.log(`  border     ${info.border}`);
  console.log(`  box        ${info.box}   visible ${info.visible}`);
  console.log(`  padding    ${info.padding}`);
  console.log(`  margin     ${info.margin}\n`);
}
if (errs.length) console.log('PAGE ERRORS:', errs.join(' | '));

await browser.close();
server.close();
