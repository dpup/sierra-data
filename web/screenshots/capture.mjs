// capture.mjs — screenshot harness for data.sierragridteam.org.
//
// Serves the built static site (../../site/dist) over HTTP, drives it with
// headless Chromium, and answers every same-origin /api/v1/* fetch from
// fixtures.mjs so each page renders realistic, populated state — no server, no
// API keys, no live upstreams. Captures every page at a set of viewports
// (phone / tablet / desktop) into ./out/<viewport>/<page>.png.
//
// Usage:
//   node capture.mjs                      # all pages, all viewports
//   node capture.mjs --pages events,map   # subset of pages
//   node capture.mjs --only mobile        # subset of viewports
//
// External map tiles (OSM) are blocked so runs stay offline and deterministic;
// MapLibre still draws the data layers over an empty basemap.

import { chromium } from 'playwright';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, extname } from 'node:path';
import { routeFor, FIXTURE_META } from './fixtures.mjs';

const __dirname = dirname(fileURLToPath(import.meta.url));
const DIST = join(__dirname, '..', '..', 'site', 'dist');
const OUT = join(__dirname, 'out');

// Pages to shoot: url path -> output basename. The clean-URL form (/events)
// maps to the built <name>.html the Go siteHandler serves.
const PAGES = [
  { name: 'home', url: '/index.html' },
  { name: 'events', url: '/events.html' },
  { name: 'event-detail', url: '/event.html?id=evt-wildfire-mudflat' },
  { name: 'map', url: '/map.html?place=ebbetts-pass&view=wildfire' },
  { name: 'roads', url: '/roads.html?place=ebbetts-pass' },
  { name: 'mesh', url: '/mesh.html' },
  { name: 'places', url: '/places.html' },
  { name: 'place-detail', url: '/places.html?place=ebbetts-pass' },
  { name: 'sources', url: '/sources.html' },
  { name: 'history', url: '/history.html' },
  { name: 'docs', url: '/docs.html' },
];

const VIEWPORTS = [
  { name: 'mobile', width: 390, height: 844, dpr: 3 },   // iPhone 14 Pro-ish
  { name: 'tablet', width: 768, height: 1024, dpr: 2 },  // iPad portrait
  { name: 'desktop', width: 1440, height: 900, dpr: 1 },
];

const MIME = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2',
  '.png': 'image/png', '.webmanifest': 'application/manifest+json',
};

// ---- tiny static file server for the built dist ---------------------------
function startServer() {
  const server = http.createServer(async (req, res) => {
    try {
      const u = new URL(req.url, 'http://localhost');
      let fp = join(DIST, decodeURIComponent(u.pathname));
      if (u.pathname.endsWith('/')) fp = join(fp, 'index.html');
      if (!existsSync(fp) && existsSync(fp + '.html')) fp += '.html';
      if (!existsSync(fp)) { res.writeHead(404); res.end('not found'); return; }
      const body = await readFile(fp);
      res.writeHead(200, { 'content-type': MIME[extname(fp)] || 'application/octet-stream' });
      res.end(body);
    } catch (err) {
      res.writeHead(500); res.end(String(err));
    }
  });
  return new Promise((resolve) => server.listen(0, () => resolve(server)));
}

const argOf = (flag) => {
  const i = process.argv.indexOf(flag);
  return i >= 0 ? process.argv[i + 1] : null;
};

async function main() {
  const pagesArg = argOf('--pages');
  const onlyArg = argOf('--only');
  const pages = pagesArg ? PAGES.filter((p) => pagesArg.split(',').includes(p.name)) : PAGES;
  const viewports = onlyArg ? VIEWPORTS.filter((v) => onlyArg.split(',').includes(v.name)) : VIEWPORTS;

  if (!existsSync(DIST)) {
    console.error(`No build at ${DIST}. Run \`npm run build\` (or \`make site\`) first.`);
    process.exit(1);
  }

  const server = await startServer();
  const { port } = server.address();
  const base = `http://localhost:${port}`;
  console.log(`Serving ${DIST} at ${base}`);
  console.log(`Fixtures: ${FIXTURE_META.events} events, ${FIXTURE_META.places} places, ${FIXTURE_META.sources} sources`);

  const browser = await chromium.launch();
  let shots = 0;
  const apiMisses = new Set();

  for (const vp of viewports) {
    const context = await browser.newContext({
      viewport: { width: vp.width, height: vp.height },
      deviceScaleFactor: vp.dpr,
      // A phone UA nudges any UA-sniffing; layout is driven by width here.
      isMobile: vp.name === 'mobile',
    });

    // Answer same-origin API calls from fixtures; block external map tiles.
    await context.route('**/*', async (route) => {
      const url = new URL(route.request().url());
      const sameOrigin = url.host === `localhost:${port}`;
      if (sameOrigin && /^\/(api\/)?v1\//.test(url.pathname)) {
        const data = routeFor(url.pathname, url.searchParams);
        if (data === null) {
          apiMisses.add(url.pathname);
          return route.fulfill({ status: 404, contentType: 'application/json',
            body: JSON.stringify({ code: 5, codeName: 'NOT_FOUND', message: 'no fixture' }) });
        }
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
      }
      if (!sameOrigin) return route.abort(); // external tiles/fonts — keep offline
      return route.continue();
    });

    const dir = join(OUT, vp.name);
    for (const pg of pages) {
      const page = await context.newPage();
      const errs = [];
      page.on('pageerror', (e) => errs.push(String(e)));
      await page.goto(base + pg.url, { waitUntil: 'networkidle', timeout: 15000 }).catch(() => {});
      // Let async renders (events table auto-select, mesh fitBounds) settle.
      await page.waitForTimeout(700);
      const file = join(dir, `${pg.name}.png`);
      await page.screenshot({ path: file, fullPage: true });
      shots++;
      const flag = errs.length ? `  ⚠ ${errs.length} page error(s): ${errs[0].slice(0, 80)}` : '';
      console.log(`  ${vp.name.padEnd(7)} ${pg.name.padEnd(13)} → screenshots/out/${vp.name}/${pg.name}.png${flag}`);
      await page.close();
    }
    await context.close();
  }

  await browser.close();
  server.close();
  if (apiMisses.size) console.log(`\n⚠ Unmocked API paths (served 404): ${[...apiMisses].join(', ')}`);
  console.log(`\nDone — ${shots} screenshot(s) in screenshots/out/`);
}

main().catch((e) => { console.error(e); process.exit(1); });
