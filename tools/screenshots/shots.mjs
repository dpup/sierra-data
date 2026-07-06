// shots.mjs — render every page in headless Chromium and capture (a) a full-page
// screenshot and (b) a layout-metrics JSON, at desktop and mobile widths.
//
// Purpose: diagnose spacing/layout issues that curl/grep can't see (they only
// show HTML structure; spacing is a rendered property). The PNGs are read back
// visually; the metrics JSON enables a numeric before/after diff — run this
// against the current build AND the pre-migration build (on two ports, two
// LABELs) and diff the JSON to classify each spacing issue as a migration
// regression vs. pre-existing, and to pinpoint the exact element + property.
//
// Uses the Playwright + Chromium provided by the Moat `playwright` dependency
// (see moat.yaml) — no local install. Because that Playwright is installed
// globally and ESM `import` ignores NODE_PATH, resolve it with a CommonJS
// require (which does honor NODE_PATH); `make site-shots` sets NODE_PATH to the
// global npm root.
//
// Usage (server must already be running at BASE_URL):
//   BASE_URL=http://localhost:8190 LABEL=after node shots.mjs
//   BASE_URL=http://localhost:8191 LABEL=before node shots.mjs   # old build
// Output: ./out/<LABEL>/<page>-<viewport>.png  and  .metrics.json

import { createRequire } from 'node:module';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

const { chromium } = createRequire(import.meta.url)('playwright');

const BASE_URL = process.env.BASE_URL || 'http://localhost:8190';
const LABEL = process.env.LABEL || 'current';
const OUT = join('out', LABEL);

const PAGES = ['/', '/sources', '/events', '/event', '/places', '/map', '/history', '/docs'];
const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'mobile', width: 390, height: 844 },
];

// Layout metrics: for a stable, comparable set of elements, capture position +
// box spacing. We sample <main>'s subtree at the block level plus the shell
// landmarks, keyed by a structural path so before/after entries line up.
const METRICS_JS = () => {
  const props = ['marginTop', 'marginBottom', 'paddingTop', 'paddingBottom', 'rowGap', 'gap'];
  const label = (el) => {
    const cls = (el.getAttribute('class') || '').trim().split(/\s+/).slice(0, 3).join('.');
    const id = el.id ? `#${el.id}` : '';
    return `${el.tagName.toLowerCase()}${id}${cls ? '.' + cls : ''}`;
  };
  const rows = [];
  const roots = ['aside.sidebar', '.context-bar', 'main', 'footer.site-footer'];
  for (const sel of roots) {
    const root = document.querySelector(sel);
    if (!root) continue;
    // root + its block-level descendants, capped so the JSON stays diffable
    const els = [root, ...root.querySelectorAll(':scope > *, :scope > * > *')];
    let i = 0;
    for (const el of els) {
      if (i++ > 120) break;
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      const box = {};
      for (const p of props) box[p] = cs[p];
      rows.push({
        // index-prefixed so generic tags (h2, div, p) get unique, position-
        // stable keys that line up between before/after (parallel DOM order).
        path: `${sel} [${String(i - 1).padStart(3, '0')}] ${label(el)}`,
        rect: { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) },
        box,
      });
    }
  }
  return rows;
};

async function write(path, data) {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, data);
}

const browser = await chromium.launch();
console.log(`base=${BASE_URL} label=${LABEL} → ${OUT}/`);

for (const vp of VIEWPORTS) {
  const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  for (const route of PAGES) {
    const url = BASE_URL + route;
    const slug = route === '/' ? 'index' : route.slice(1).replace(/\//g, '_');
    const stem = join(OUT, `${slug}-${vp.name}`);
    try {
      await page.goto(url, { waitUntil: 'networkidle', timeout: 20000 });
      await page.evaluate(() => document.fonts && document.fonts.ready);
      await page.waitForTimeout(600); // let async data + late layout settle
      await page.screenshot({ path: `${stem}.png`, fullPage: true });
      const metrics = await page.evaluate(METRICS_JS);
      await write(`${stem}.metrics.json`, JSON.stringify(metrics, null, 2));
      console.log(`  ✓ ${vp.name} ${route}`);
    } catch (err) {
      console.log(`  ✗ ${vp.name} ${route} — ${err.message}`);
    }
  }
  await context.close();
}

await browser.close();
console.log('done');
