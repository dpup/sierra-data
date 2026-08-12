// metrics.mjs — objective layout measurements for the spacing/width pass.
//
// Screenshots tell you something looks off; they don't tell you WHY. This
// measures the things that actually drive the feel of a page — content width,
// line length in characters, the gaps between sections, and where blocks sit
// relative to the text column — so a spacing pass is a diff of numbers rather
// than an argument about pixels.
//
// Usage:
//   node screenshots/metrics.mjs                     # all pages, all widths
//   node screenshots/metrics.mjs --pages docs        # one page
//   node screenshots/metrics.mjs --widths 1440       # one width

import { chromium } from 'playwright';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, extname } from 'node:path';
import { routeFor } from './fixtures.mjs';

const __dirname = dirname(fileURLToPath(import.meta.url));
const DIST = join(__dirname, '..', '..', 'site', 'dist');

const PAGES = [
  ['home', '/index.html'],
  ['events', '/events.html'],
  ['event-detail', '/event.html?id=evt-wildfire-mudflat'],
  ['map', '/map.html?place=ebbetts-pass&layer=wildfire'],
  ['roads', '/roads.html?place=ebbetts-pass'],
  ['mesh', '/mesh.html'],
  ['places', '/places.html'],
  ['sources', '/sources.html'],
  ['history', '/history.html'],
  ['docs', '/docs.html'],
  ['mcp-guide', '/mcp-guide.html'],
];

const WIDTHS = [390, 414, 768, 1280, 1440, 1920];

const MIME = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8', '.json': 'application/json',
  '.woff2': 'font/woff2', '.png': 'image/png', '.svg': 'image/svg+xml',
};

function startServer() {
  const server = http.createServer(async (req, res) => {
    try {
      const u = new URL(req.url, 'http://localhost');
      let fp = join(DIST, decodeURIComponent(u.pathname));
      if (u.pathname.endsWith('/')) fp = join(fp, 'index.html');
      if (!existsSync(fp) && existsSync(fp + '.html')) fp += '.html';
      if (!existsSync(fp)) { res.writeHead(404); res.end('nf'); return; }
      const body = await readFile(fp);
      res.writeHead(200, { 'content-type': MIME[extname(fp)] || 'application/octet-stream' });
      res.end(body);
    } catch (e) { res.writeHead(500); res.end(String(e)); }
  });
  return new Promise((r) => server.listen(0, () => r(server)));
}

const argOf = (f) => { const i = process.argv.indexOf(f); return i >= 0 ? process.argv[i + 1] : null; };

// Runs in the page. Returns the numbers that drive perceived spacing.
function collect() {
  const px = (v) => Math.round(parseFloat(v) || 0);
  const main = document.querySelector('main');
  if (!main) return { error: 'no main' };
  const cs = getComputedStyle(main);
  const mainBox = main.getBoundingClientRect();

  // Average character width, measured in the ELEMENT'S OWN font — a 18px lead
  // and 15px body have different character widths, so one page-level constant
  // would over-report the measure of larger text and hide the real problem.
  const probe = document.createElement('span');
  probe.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;';
  probe.textContent = 'abcdefghijklmnopqrstuvwxyz'.repeat(4);
  // Inherit rather than copy the font: setting the `font` shorthand from
  // getComputedStyle silently falls back to a narrower default when the
  // shorthand is empty, which under-reports width and flatters the measure.
  const charWidthOf = (el) => {
    el.appendChild(probe);
    const w = probe.getBoundingClientRect().width / 104;
    probe.remove();
    return w;
  };
  const chWidth = charWidthOf(main);

  // Every visible block-level child of main, with its top gap to the previous.
  const blocks = [];
  let prevBottom = null;
  for (const el of main.children) {
    const r = el.getBoundingClientRect();
    if (r.height === 0) continue;
    const s = getComputedStyle(el);
    blocks.push({
      tag: el.tagName.toLowerCase(),
      cls: (el.className || '').toString().split(/\s+/).filter(Boolean).slice(0, 2).join('.'),
      w: Math.round(r.width),
      h: Math.round(r.height),
      left: Math.round(r.left),
      gapAbove: prevBottom === null ? null : Math.round(r.top - prevBottom),
      mt: px(s.marginTop), mb: px(s.marginBottom),
      pt: px(s.paddingTop), pb: px(s.paddingBottom),
    });
    prevBottom = r.bottom;
  }

  // Prose measure: how many characters wide is the longest body paragraph?
  const proseSel = 'main p, main .lead, main .prose, main .cap-p, main .ep-detail, main .sec-body, main .env-doc, main .support-p, main .mission';
  // Only elements whose OWN text nodes are substantial. A flex container that
  // merely wraps a paragraph and two code panes is not prose, and counting it
  // reports a "line length" no reader ever sees.
  const ownText = (el) =>
    [...el.childNodes]
      .filter((n) => n.nodeType === 3 || (n.nodeType === 1 && /^(a|code|strong|em|b|i|span|abbr)$/i.test(n.tagName)))
      .map((n) => n.textContent)
      .join('')
      .trim();
  // A .rail-caption is a full-width MONO scan-line — the design puts it there
  // deliberately (one line under the hero's rule) and it is read by jumping to
  // the label, not by sweeping back to a line start. Measuring it as prose put
  // a 174-character reading on six pages and buried the real numbers. The
  // guideline is about reading columns; this is not one.
  const proses = [...document.querySelectorAll(proseSel)]
    .filter((p) => !p.classList.contains('rail-caption'))
    .filter((p) => p.getBoundingClientRect().height > 0 && ownText(p).length > 80)
    .map((p) => {
      const r = p.getBoundingClientRect();
      const st = getComputedStyle(p);
      return {
        cls: (p.className || '').toString().split(/\s+/)[0] || p.tagName.toLowerCase(),
        w: Math.round(r.width),
        ch: Math.round(r.width / charWidthOf(p)),
        maxW: st.maxWidth,
        fs: px(st.fontSize),
      };
    });

  // Anything wider than main's content box is a horizontal-overflow bug.
  const overflow = [];
  for (const el of document.querySelectorAll('main *')) {
    const r = el.getBoundingClientRect();
    if (r.width === 0) continue;
    if (r.right > mainBox.right + 1.5 || r.left < mainBox.left - 1.5) {
      const s = getComputedStyle(el);
      // Scroll containers are fine. (The full-bleed deck used to need an
      // exemption here; it renders outside main now, so this walk never sees
      // it — the skip stays only as a guard if one is ever nested again.)
      if (el.classList.contains('deck')) continue;
      if (s.overflowX === 'auto' || s.overflowX === 'scroll') continue;
      if (el.closest('[style*="overflow"], .table-wrap, .resp-pane, .code, pre, .ep-pane')) continue;
      overflow.push({
        cls: (el.className || '').toString().split(/\s+/).slice(0, 2).join('.') || el.tagName.toLowerCase(),
        left: Math.round(r.left), right: Math.round(r.right),
      });
    }
  }

  // THE RIGHT-HAND VOID. Prose is capped at the reading measure (540px) while
  // the column is 972-1216px, so every prose block leaves 44-56% of the column
  // empty beside it. One short intro is unremarkable; a RUN of them is a hole
  // in the page, and that is what reads as unfinished.
  //
  // So the number that matters is not "is this block narrow" but "how tall is
  // the tallest contiguous stretch of page with nothing on the right". Nothing
  // measured this before, which is why it kept being found by eye, one page at
  // a time, and kept coming back.
  const NARROW = 0.7; // of the column
  const visible = (n) => {
    const b = n.getBoundingClientRect();
    return b.height > 4 && b.width > 0 && getComputedStyle(n).display !== 'none';
  };
  // A two-column region moves the hole INSIDE it: `.with-rail` fills the page,
  // so measuring only main's children would report a clean page while the left
  // column still had a void beside its prose. Descend into the text column and
  // measure its children against ITS width, not the page's.
  const colWidth = new Map();
  const pageCol = mainBox.width - px(cs.paddingLeft) - px(cs.paddingRight);
  const topBlocks = [];
  for (const n of [...main.children].filter(visible)) {
    const inner = n.querySelector(':scope > .rail-main, :scope > .with-notes');
    const isSplit = n.classList.contains('with-rail') || n.classList.contains('with-notes');
    if (isSplit) {
      const col = n.classList.contains('with-notes') ? n : inner;
      const kids = col ? [...col.children].filter(visible) : [];
      const w = col ? col.getBoundingClientRect().width : pageCol;
      for (const k of kids) {
        // A sidenote lives in column two by design; it is not a narrow block in
        // column one, and counting it would penalise the very fix for this.
        if (k.classList.contains('sidenote')) continue;
        topBlocks.push(k);
        colWidth.set(k, w);
      }
      continue;
    }
    topBlocks.push(n);
    colWidth.set(n, pageCol);
  }
  let tallestHole = 0, holeAt = null, run = 0, runFrom = null, narrowH = 0, totalH = 0;
  for (const n of topBlocks) {
    const b = n.getBoundingClientRect();
    totalH += b.height;
    if (b.width / colWidth.get(n) < NARROW) {
      narrowH += b.height;
      if (!run) runFrom = (n.className || n.tagName).toString().split(/\s+/)[0];
      run += b.height;
      if (run > tallestHole) { tallestHole = run; holeAt = runFrom; }
    } else { run = 0; runFrom = null; }
  }

  return {
    // THE horizontal-scroll verdict, computed here so no caller can check the
    // wrong field. `document.documentElement.scrollWidth` is the one that
    // matters: a 13px icon overflowing a clipped line pushed <html> to 555px at
    // a 390px viewport while `body.scrollWidth` stayed 390, so a check against
    // body reported clean while the phone scrolled sideways.
    hScroll: document.documentElement.scrollWidth > window.innerWidth + 1,
    tallestHole: Math.round(tallestHole),
    holeAt,
    pctNarrow: totalH ? Math.round((100 * narrowH) / totalH) : 0,
    docWidth: document.documentElement.scrollWidth,
    mainW: Math.round(mainBox.width),
    mainInnerW: Math.round(mainBox.width - px(cs.paddingLeft) - px(cs.paddingRight)),
    mainPadL: px(cs.paddingLeft), mainPadR: px(cs.paddingRight),
    mainPadTop: px(cs.paddingTop), mainPadBottom: px(cs.paddingBottom),
    chWidth: Math.round(chWidth * 100) / 100,
    blocks,
    proses,
    overflow: overflow.slice(0, 8),
    bodyScrollW: document.body.scrollWidth,
    winW: window.innerWidth,
  };
}

async function main() {
  const pagesArg = argOf('--pages');
  const widthsArg = argOf('--widths');
  const pages = pagesArg ? PAGES.filter((p) => pagesArg.split(',').includes(p[0])) : PAGES;
  const widths = widthsArg ? widthsArg.split(',').map(Number) : WIDTHS;

  const server = await startServer();
  const { port } = server.address();
  const browser = await chromium.launch();

  const out = {};
  for (const w of widths) {
    const ctx = await browser.newContext({ viewport: { width: w, height: 900 } });
    await ctx.route('**/*', async (route) => {
      const u = new URL(route.request().url());
      const same = u.host === `localhost:${port}`;
      if (same && /^\/(api\/)?v1\//.test(u.pathname)) {
        const d = routeFor(u.pathname, u.searchParams);
        return route.fulfill({ status: d === null ? 404 : 200, contentType: 'application/json', body: JSON.stringify(d ?? {}) });
      }
      if (!same) return route.abort();
      return route.continue();
    });
    for (const [name, url] of pages) {
      const page = await ctx.newPage();
      await page.goto(`http://localhost:${port}${url}`, { waitUntil: 'networkidle', timeout: 15000 }).catch(() => {});
      await page.waitForTimeout(500);
      out[`${name}@${w}`] = await page.evaluate(collect);
      await page.close();
    }
    await ctx.close();
  }
  await browser.close();
  server.close();
  console.log(JSON.stringify(out, null, 1));
}

main().catch((e) => { console.error(e); process.exit(1); });
