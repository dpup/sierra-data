// wiring-check.mjs — fail the build when an island and its page have drifted.
//
// Islands bind to markup through id strings. Rename or remove an id on one side
// and nothing complains until a browser runs the page, where it surfaces as
// `Cannot read properties of null (reading 'addEventListener')` somewhere in
// the middle of init — with the page half-wired and the error thrown a long way
// from the edit that caused it. That is a real bug that shipped.
//
// This is the check that catches it: every `getElementById('x')` / `$('x')` in
// an island must have a matching `id="x"` in the page it belongs to (or in a
// partial that page includes). Static, fast, no browser.
//
// It is a lint, not a proof — an island could build an id at runtime, and one
// that does should be added to DYNAMIC below with a reason.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const WEB = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const PAGES = path.join(WEB, 'src/pages');
const ISLANDS = path.join(WEB, 'public/assets/pages');
const PARTIALS = path.join(WEB, 'src/partials');
const LAYOUTS = path.join(WEB, 'src/layouts');
const COMPONENTS = path.join(WEB, 'src/components');

/** ids an island creates itself, with the reason it is not in the markup. */
const DYNAMIC = {
  // event-detail.js builds its whole subtree, including this anchor.
  'event-detail.js': ['ed-timeline'],
};

/** Which page(s) an island can be mounted into. */
const MOUNTS = {
  'event-detail.js': ['event.astro', 'events.astro'],
};

const read = (f) => fs.readFileSync(f, 'utf8');
const idsIn = (src) => [
  ...[...src.matchAll(/id="([^"{}]+)"/g)].map((m) => m[1]),
  // An id can reach the DOM through a component prop — <PageHead
  // metaId="ev-scope-meta"> renders `id={metaId}` — so the literal never appears
  // as `id="…"` in the page. Any `*Id="…"` attribute counts as an id the page
  // provides; without this, moving markup into a component looks like drift.
  ...[...src.matchAll(/\b\w*[iI]d="([^"{}]+)"/g)].map((m) => m[1]),
];
const lookupsIn = (src) => {
  const ids = [
    ...src.matchAll(/getElementById\('([^']+)'\)/g),
    ...src.matchAll(/\$\('([^']+)'\)/g),
  ].map((m) => m[1]);
  // requireEls('who', { key: 'the-id', ... }) — matched only INSIDE the call,
  // because a bare `key: 'value'` pattern also matches MapLibre layer specs
  // (`type: 'fill'`, `id: 'event-line'`), which are not DOM ids at all.
  for (const call of src.matchAll(/requireEls\([^,]+,\s*\{([\s\S]*?)\}\s*\)/g)) {
    for (const m of call[1].matchAll(/\w+:\s*'([^']+)'/g)) ids.push(m[1]);
  }
  return ids;
};

// Markup an island can see: its own page, plus everything every page shares.
const shared = [PARTIALS, LAYOUTS, COMPONENTS]
  .filter((d) => fs.existsSync(d))
  .flatMap((d) => fs.readdirSync(d).map((f) => read(path.join(d, f))))
  .flatMap(idsIn);

let failures = 0;
for (const file of fs.readdirSync(ISLANDS).filter((f) => f.endsWith('.js'))) {
  const src = read(path.join(ISLANDS, file));
  const pageNames = MOUNTS[file] || [`${file.replace(/\.js$/, '')}.astro`];
  const pages = pageNames.filter((p) => fs.existsSync(path.join(PAGES, p)));
  if (!pages.length) continue; // an island with no page of its own; nothing to check

  const available = new Set([
    ...shared,
    ...pages.flatMap((p) => idsIn(read(path.join(PAGES, p)))),
    ...(DYNAMIC[file] || []),
  ]);

  const missing = [...new Set(lookupsIn(src))].filter((id) => !available.has(id));
  if (missing.length) {
    failures++;
    console.error(
      `✗ ${file} looks up ${missing.map((m) => `#${m}`).join(', ')} — ` +
        `not present in ${pageNames.join(' / ')}`
    );
  }
}

// The BUILT ARTIFACTS, not just the source.
//
// Source can be perfectly consistent while the thing a browser loads is not: an
// id can reach the page through a component or a partial, so the pairing that
// actually ships is HTML-to-JS, not .astro-to-JS. That pairing is what threw
// "markup is missing #ev-scope-place" against a repo where both source files
// were correct. Checking dist as well as src makes the deployable unit the thing
// under test.
//
// site/dist is no longer committed, so this half is conditional: `make site`
// runs the check AFTER the build (so it examines the build it just produced),
// and a standalone run against an unbuilt tree checks source only rather than
// crashing. Resolved relative to web/ — never assume the checkout is at
// /workspace, which the Docker site-builder stage is not.
const DIST = path.resolve(WEB, '../site/dist');
if (fs.existsSync(path.join(DIST, 'assets/pages'))) {
  const distShared = ['assets', 'assets/pages'].flatMap(() => []);
  void distShared;
  for (const file of fs.readdirSync(path.join(DIST, 'assets/pages')).filter((f) => f.endsWith('.js'))) {
    const src = read(path.join(DIST, 'assets/pages', file));
    const pageNames = (MOUNTS[file] || [`${file.replace(/\.js$/, '')}.astro`])
      .map((p) => p.replace(/\.astro$/, '.html'));
    const pages = pageNames.filter((p) => fs.existsSync(path.join(DIST, p)));
    if (!pages.length) continue;
    const available = new Set([
      ...pages.flatMap((p) => idsIn(read(path.join(DIST, p)))),
      ...(DYNAMIC[file] || []),
    ]);
    const missing = [...new Set(lookupsIn(src))].filter((id) => !available.has(id));
    if (missing.length) {
      failures++;
      console.error(
        `✗ site/dist: ${file} looks up ${missing.map((m) => `#${m}`).join(', ')} — ` +
          `not present in ${pageNames.join(' / ')}. The built site is STALE; run \`make site\`.`
      );
    }
  }
}

if (failures) {
  console.error(
    `\n${failures} island(s) out of step with their markup. ` +
      'Renaming an id needs both sides changed.'
  );
  process.exit(1);
}
console.log(
  fs.existsSync(path.join(DIST, 'assets/pages'))
    ? '✅ islands and markup agree on every id (src and site/dist)'
    : '✅ islands and markup agree on every id (src only — site/dist not built)'
);
