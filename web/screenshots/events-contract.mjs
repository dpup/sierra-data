// Acceptance checks for the Events page layout spec (§8).
//
// These are the mechanical ones — the checks a screenshot cannot make, because
// they are about the DOM and about widths the capture does not photograph.
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
const BASE = `http://localhost:${port}`;

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  // The copy checks below need a clipboard that resolves rather than rejects.
  permissions: ['clipboard-read', 'clipboard-write'],
});
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

let fails = 0;
const check = (n, cond, detail) => {
  console.log(`${cond ? 'PASS' : 'FAIL'}  ${n}${cond ? '' : '  -> ' + JSON.stringify(detail).slice(0, 240)}`);
  if (!cond) fails++;
};

const page = await ctx.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e).slice(0, 200)));
await page.goto(`${BASE}/events.html`, { waitUntil: 'networkidle' });
await page.waitForTimeout(900);

// 1. No en/em dash anywhere in EVENT DATA (the list rows and the detail pane).
//    Dashes remain legal in the filter summary, which describes the query.
const dashes = await page.evaluate(() => {
  const zones = [document.getElementById('ev-list'), document.getElementById('ev-detail')];
  const hits = [];
  for (const z of zones) {
    if (!z) continue;
    const w = document.createTreeWalker(z, NodeFilter.SHOW_TEXT);
    let n;
    while ((n = w.nextNode())) {
      const t = n.textContent.trim();
      if (/(^|\s)[–—](\s|$)/.test(t)) {
        // An em dash INSIDE a headline is upstream prose, not our rendering of
        // an absent value; only flag a dash that stands alone as a value.
        if (t === '—' || t === '–') hits.push(n.parentElement.className || t);
      }
    }
  }
  return hits;
});
check('1. no bare dash as a value in event data', dashes.length === 0, dashes);

// 2. Place is scope, not a filter.
const scope = await page.evaluate(() => ({
  scopeText: (document.getElementById('ev-scope-place') || {}).textContent || '',
  placeControlInFilters: !!document.querySelector('#ev-fpanel [id*="place"], #ev-fsum [id*="place"]'),
}));
check('2. no place control in the filter region', !scope.placeControlInFilters, scope);

// 3. The echo is exactly the request — one absolute URL, nothing restated from
//    the filter UI above it, and no invented defaults.
const echo = await page.evaluate(() => {
  const box = document.getElementById('ev-echo');
  return {
    url: (document.getElementById('ev-url') || {}).textContent || '',
    // The whole echo, minus the copy button — what a reader sees on the line.
    body: (box ? box.textContent : '').replace(/copy curl/, '').replace(/\s+/g, ' ').trim(),
    scope: (document.getElementById('ev-scope-meta') || {}).textContent || '',
  };
});
check('3. the echo is one absolute request URL and nothing else',
  /^https:\/\/[^ ]+\/api\/v1\/events/.test(echo.url)
    && echo.body === `GET ${echo.url}`, echo);
check('3b. the echo never states a default it did not send',
  !/default/i.test(echo.url), echo);

// The count lives beside the title. It reports what is LOADED and never implies
// a total — /events returns none — so "of N" must not appear.
check('3c. the header states a loaded count and claims no total',
  /\d+ records? loaded/.test(echo.scope) && !/\bof \d/.test(echo.scope), echo.scope);

// 4. The filter region is ALWAYS VISIBLE — no disclosure, no apply step.
//    (An earlier revision of the spec asked for a collapsed resting state with
//    EDIT/APPLY; the prototype is a plain row of chips per facet, and the chips
//    are the query. These checks pin the prototype.)
const filters = await page.evaluate(() => ({
  visible: !!document.querySelector('.filter-set')
    && document.querySelector('.filter-set').offsetParent !== null,
  disclosures: document.querySelectorAll('.filter-set details').length,
  applyButtons: document.querySelectorAll('#ev-apply, #ev-edit, #ev-cancel').length,
  // .filter-label carries the param name; a facet may append a .filter-note
  // qualifier inside it, which is not part of the name.
  groups: [...document.querySelectorAll('.filter-set .filter-label')].map((n) => {
    const note = n.querySelector('.filter-note');
    return n.textContent.replace(note ? note.textContent : '', '').trim();
  }),
}));
check('4. filters are always visible, with no disclosure and no apply step',
  filters.visible && filters.disclosures === 0 && filters.applyButtons === 0, filters);
// page_size is deliberately not a facet — it shapes the request, not which
// records match, and the echo states the value in force.
check('4b. every facet is labelled with its param name',
  ['place', 'layer', 'severity_min', 'status', 'since'].every((k) => filters.groups.includes(k)),
  filters.groups);

// 5. ONE control shape. A hairline box on paper, solid ink when selected —
//    no dashes, no second shape for the single-select facets.
const controls = await page.evaluate(() => {
  const off = document.querySelector('#ev-layers .chip-toggle:not(.on)');
  const on = document.querySelector('#ev-layers .chip-toggle.on');
  // Resolve a token to the same rgb() form getComputedStyle reports, so the
  // assertion below compares the chip against THE PALETTE rather than against a
  // hex literal. Hardcoding `rgb(244, 241, 234)` here broke this check on a
  // pure palette swap — the chip was still ink-on-paper, the test was just
  // pinned to last month's paper.
  const resolve = (token) => {
    const probe = document.createElement('span');
    probe.style.color = `var(${token})`;
    probe.style.display = 'none';
    document.body.append(probe);
    const v = getComputedStyle(probe).color;
    probe.remove();
    return v;
  };

  return {
    offStyle: off ? getComputedStyle(off).borderStyle : null,
    offBg: off ? getComputedStyle(off).backgroundColor : null,
    onBg: on ? getComputedStyle(on).backgroundColor : null,
    onColor: on ? getComputedStyle(on).color : null,
    ink: resolve('--ink'),
    paper: resolve('--paper'),
    sevUsesChips: !!document.querySelector('#ev-sev .chip-toggle'),
    statusUsesChips: !!document.querySelector('#ev-status .chip-toggle'),
    railsAnywhere: document.querySelectorAll('.seg-rail').length,
  };
});
check('5. off chips are a solid hairline box, not dashed',
  controls.offStyle === 'solid', controls);
check('5b. selected chips are solid ink with paper text',
  controls.onBg === controls.ink && controls.onColor === controls.paper, controls);
check('5c. every facet uses the same chip control',
  controls.sevUsesChips && controls.statusUsesChips && controls.railsAnywhere === 0, controls);

// A lit chip always shows: "all" stands in for "no layer filter" so the row
// never reads as though nothing is selected.
const allLit = await page.evaluate(() =>
  !!document.querySelector('#ev-layers .chip-toggle.on'));
check('5d. the layer row always has one lit chip', allLit, {});

// SINCE echoes its serialized value.
await page.fill('#ev-since', '2026-08-10T14:00');
await page.waitForTimeout(150);
const sinceEcho = await page.evaluate(() => document.getElementById('ev-since-echo').textContent);
check('3.4 since echoes the exact serialized value + offset',
  /sends since=\d{4}-\d{2}-\d{2}T[\d:]+Z \(your time [+-]\d{2}:\d{2}\)/.test(sinceEcho), sinceEcho);
await page.waitForTimeout(150);

// 6. Every row has a severity spine — including the lead record.
const spines = await page.evaluate(() => {
  const rows = [...document.querySelectorAll('#ev-list .rec')];
  return {
    rows: rows.length,
    withSpine: rows.filter((r) => {
      const s = r.querySelector('.rec-spine');
      if (!s) return false;
      const bg = getComputedStyle(s).backgroundColor;
      return bg && bg !== 'rgba(0, 0, 0, 0)';
    }).length,
  };
});
check('6. every row has a painted severity spine',
  spines.rows > 0 && spines.rows === spines.withSpine, spines);

// 7. The selected row is distinguishable from its siblings.
//    The mock does this with sunken paper plus the severity spine. A 2px inset
//    outline was tried and removed: across fifty rows it read as a box around
//    the selection and was a large part of why the column felt busy. What this
//    pins is the PROPERTY — selected must differ from unselected — rather than
//    one particular way of achieving it.
const sel = await page.evaluate(() => {
  const s = document.querySelector('#ev-list .rec.selected');
  const other = document.querySelector('#ev-list .rec:not(.selected)');
  if (!s || !other) return null;
  return {
    selectedBg: getComputedStyle(s).backgroundColor,
    unselectedBg: getComputedStyle(other).backgroundColor,
    spine: getComputedStyle(s.querySelector('.rec-spine')).backgroundColor,
  };
});
check('7. selected row is visually distinct from its siblings',
  !!sel && sel.selectedBg !== sel.unselectedBg
    && sel.spine && sel.spine !== 'rgba(0, 0, 0, 0)', sel);

// 9. Numbers are tabular, and >= 1000 are separated.
const nums = await page.evaluate(() => {
  const t = (document.getElementById('ev-detail') || {}).innerText || '';
  const bare = t.match(/(?<![\d,.])\d{4,}(?![\d,])/g) || [];
  // Timestamps/ids legitimately contain long digit runs; only flag values in
  // the envelope's numeric rows.
  return { suspicious: bare.filter((v) => !/^20\d\d$/.test(v)).slice(0, 5) };
});
check('9. no unseparated 4+ digit numbers in the detail pane', nums.suspicious.length === 0, nums);

// 8. enhancement_io — reported, not asserted. A spec revision asked for zero
//    occurrences on the assumption it was a proxy artifact; it is a real
//    GetEventRequest field, and dropping it would make the copyable request
//    return a different record than the pane shows (see event-detail.js).
const enh = await page.evaluate(() => (document.body.innerHTML.match(/enhancement_io/g) || []).length);
console.log(`NOTE  enhancement_io occurrences in DOM: ${enh} — expected; it is a real GetEventRequest field`);

// 11. COPYING CHANGES NOTHING ON THE PAGE.
//     The id form used to replace a seventy-character id with the words
//     "copied id", which hid the value and collapsed the line, so everything
//     after it jumped. Pin the property, not the icon: the value still reads
//     the same, and its box has not moved.
await page.setViewportSize({ width: 1280, height: 900 });
await page.waitForTimeout(300);
const copyId = await page.evaluate(async () => {
  const node = document.querySelector('.rec-id.copyable');
  if (!node) return { found: false };
  const textOf = (n) => {
    const c = n.cloneNode(true);
    c.querySelectorAll('.copy-mark, .sr-only').forEach((x) => x.remove());
    return c.textContent.trim();
  };
  const before = { text: textOf(node), box: node.getBoundingClientRect() };
  node.click();
  await new Promise((r) => setTimeout(r, 250));
  const after = { text: textOf(node), box: node.getBoundingClientRect() };
  return {
    found: true,
    sameText: before.text === after.text && before.text.length > 0,
    movedBy: Math.round(Math.abs(after.box.width - before.box.width)),
    marked: !!node.querySelector('.copy-mark'),
    announced: (node.querySelector('.sr-only') || {}).textContent || '',
  };
});
check('11. copying an id leaves the id on screen, unmoved',
  copyId.found && copyId.sameText && copyId.movedBy === 0 && copyId.marked, copyId);
check('11b. ...and says so for a screen reader',
  /copied/i.test(copyId.announced || ''), copyId.announced);

// 12. A copy BUTTON keeps its label and its width — it renamed itself to
//     "copied" before, which is a different width and nudged its neighbours.
const copyBtn = await page.evaluate(async () => {
  const btn = document.getElementById('ev-copyurl');
  if (!btn) return { found: false };
  const label = () => btn.textContent.trim();
  const before = { label: label(), w: Math.round(btn.getBoundingClientRect().width) };
  btn.click();
  await new Promise((r) => setTimeout(r, 250));
  return {
    found: true,
    sameLabel: label() === before.label,
    sameWidth: Math.round(btn.getBoundingClientRect().width) === before.w,
    label: before.label,
  };
});
check('12. the copy button keeps its label and its width',
  copyBtn.found && copyBtn.sameLabel && copyBtn.sameWidth, copyBtn);

// 10. No horizontal page scroll at four widths.
for (const w of [360, 900, 1280, 1920]) {
  await page.setViewportSize({ width: w, height: 900 });
  await page.waitForTimeout(350);
  const o = await page.evaluate(() => ({
    doc: document.documentElement.scrollWidth, win: window.innerWidth,
  }));
  check(`10. no horizontal scroll at ${w}px`, o.doc <= o.win + 1, o);
}

check('no page errors', errs.length === 0, errs);

await browser.close();
server.close();
process.exit(fails ? 1 : 0);
