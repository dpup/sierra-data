// Behaviour probe for the Map screen's fail-loud mount rule.
//
// The post-implementation review found two defects on paths that no screenshot
// reaches: unticking the last drawable layer, and a PARTIAL failure (one layer
// down, others OK-but-empty). Both produced a rendered basemap or a confident
// "confirmed empty" where the contract demands a loud unknown. Converting the
// checkboxes to chips rewrote exactly that handler, so re-walk both paths.
import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { routeFor } from './fixtures.mjs';

const ROOT = '/workspace/site/dist';
const TYPES = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript',
  '.json': 'application/json', '.woff2': 'font/woff2', '.svg': 'image/svg+xml', '.png': 'image/png' };

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
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
await ctx.route('**/*', async (route) => {
  const url = new URL(route.request().url());
  const same = url.host === `localhost:${port}`;
  if (same && /^\/(api\/)?v1\//.test(url.pathname)) {
    const data = routeFor(url.pathname, url.searchParams);
    if (data === null) return route.fulfill({ status: 404, contentType: 'application/json', body: '{}' });
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  }
  if (!same) return route.abort();
  return route.continue();
});

const page = await ctx.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e)));

const state = async () => page.evaluate(() => ({
  mapPresent: !!document.getElementById('map-canvas'),
  banner: (document.querySelector('#map-suppressed .loud-banner .loud-title') || {}).textContent || '',
  bannerBody: (document.querySelector('#map-suppressed .loud-banner p') || {}).textContent || '',
  chips: [...document.querySelectorAll('#layer-checks .chip-toggle')]
    .filter((b) => b.classList.contains('on')).map((b) => b.dataset.value),
}));

const chip = (v) => page.click(`#layer-checks .chip-toggle[data-value="${v}"]`);
let fails = 0;
const check = (name, cond, detail) => {
  console.log(`${cond ? 'PASS' : 'FAIL'}  ${name}${cond ? '' : '  -> ' + JSON.stringify(detail)}`);
  if (!cond) fails++;
};

// 1. One OK layer with features → the map is mounted.
await page.goto(`http://localhost:${port}/map.html?place=ebbetts-pass&layer=wildfire`, { waitUntil: 'networkidle' });
await page.waitForTimeout(500);
let s = await state();
check('OK layer with features mounts the map', s.mapPresent && s.chips.join() === 'wildfire', s);

// 2. Untick the ONLY drawable layer → the element must be GONE and say why.
await chip('wildfire');
await page.waitForTimeout(500);
s = await state();
check('unticking the last layer removes the map element', !s.mapPresent, s);
check('...and states the reason out loud', /\S/.test(s.banner), s);

// 3. Re-tick it → the map comes back and the banner clears.
await chip('wildfire');
await page.waitForTimeout(800);
s = await state();
check('re-ticking remounts the map', s.mapPresent, s);
check('...and clears the banner', !/\S/.test(s.banner), s);

// 4. PARTIAL FAILURE: one UNAVAILABLE layer alongside an OK one. The OK layer
//    has features, so the map stays — but the failure must still be named.
//    The bug this replaces reported partial failure as a confirmed empty.
await chip('evacuation');
await page.waitForTimeout(800);
s = await state();
check('partial failure keeps the drawable layer on the map', s.mapPresent, s);
const ledger = await page.evaluate(() =>
  (document.getElementById('honesty-panel') || {}).textContent || '');
check('partial failure names the UNAVAILABLE layer in the ledger',
  /UNAVAILABLE/.test(ledger), ledger.slice(0, 200));
check('partial failure never claims a confirmed-empty all-clear',
  !/confirmed empty/i.test(ledger + s.bannerBody), { ledger: ledger.slice(0, 200), body: s.bannerBody });

// 5. Now untick the OK layer, leaving ONLY the UNAVAILABLE one.
await chip('wildfire');
await page.waitForTimeout(800);
s = await state();
check('only-UNAVAILABLE-left suppresses the map', !s.mapPresent, s);
check('...and shows the loud banner', /\S/.test(s.banner), s);

// 6. THE MAP MUST NOT STEAL THE WHEEL.
//    A map is a large rectangle in a scrolling document; MapLibre binds the
//    wheel by default, so scrolling the page over one used to zoom the map and
//    strand the reader. Gestures wait until the map is clicked or focused.
await page.goto(`http://localhost:${port}/map.html?place=ebbetts-pass&layer=wildfire`, { waitUntil: 'networkidle' });
await page.waitForTimeout(900);

const gate = async () => page.evaluate(() => {
  const c = document.getElementById('map-canvas');
  return c ? { inert: c.classList.contains('map-inert'), tabbable: c.tabIndex >= 0 } : null;
});

let g = await gate();
check('6. a fresh map starts inert and focusable', !!g && g.inert && g.tabbable, g);

// No instructional label: it collided with the attribution, which is also
// bottom-anchored and is not optional. What must hold is that the inert and
// live states are DISTINGUISHABLE, not that either is captioned.
const states = await page.evaluate(() => {
  const c = document.getElementById('map-canvas');
  if (!c) return null;
  const inertShadow = getComputedStyle(c).boxShadow;
  c.classList.remove('map-inert');
  const liveShadow = getComputedStyle(c).boxShadow;
  c.classList.add('map-inert');
  return { inertShadow, liveShadow, label: getComputedStyle(c, '::after').content };
});
check('6b. inert and live maps look different', !!states && states.inertShadow !== states.liveShadow, states);
check('6b-ii. no instructional label overlapping the attribution',
  !!states && (states.label === 'none' || states.label === 'normal'), states);

// Scrolling over an inert map must move the PAGE, not the map.
const beforeY = await page.evaluate(() => window.scrollY);
await page.hover('#map-canvas');
await page.mouse.wheel(0, 400);
await page.waitForTimeout(400);
const afterY = await page.evaluate(() => window.scrollY);
check('6c. the wheel scrolls the page over an inert map', afterY > beforeY, { beforeY, afterY });

// Clicking activates it, and Escape releases it again.
await page.click('#map-canvas', { position: { x: 60, y: 60 } });
await page.waitForTimeout(300);
g = await gate();
check('6d. clicking the map activates its gestures', !!g && !g.inert, g);

await page.keyboard.press('Escape');
await page.waitForTimeout(300);
g = await gate();
check('6e. escape releases the map again', !!g && g.inert, g);

check('no page errors', errs.length === 0, errs);

await browser.close();
server.close();
process.exit(fails ? 1 : 0);
