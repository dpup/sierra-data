# Implementation plan — "Broadsheet" redesign of data.sierragridteam.org

**Source:** `tmp/API documentation site redesign.zip` (Claude Design handoff,
`design_handoff_grid_api_docs/`) — `README.md` is the spec,
`Grid API Docs Broadsheet.dc.html` is the reference prototype (template + logic
class), `Directions.dc.html` / `Grid API Docs.dc.html` are rejected explorations,
`support.js` is the prototype runtime and is **not** ported.

**Date:** 2026-08-06 · **Status:** implemented. See §10 for what the post-implementation
review found — including three fail-loud defects the implementation introduced.

---

## 1. What this actually is

It is a **re-skin plus a structural edit of an existing site**, not a greenfield
build. The handoff assumes it may be landing in an empty repo ("pick Astro or
Next.js"); it is not. `web/` is already Astro 5 SSG → `site/dist` (embedded by
`site/embed.go`), with plain-JS islands under `public/assets/`, vendored fonts +
MapLibre, and a mocked-data Playwright screenshot harness.

The delta, honestly stated:

| Area | Today | Handoff | Verdict |
| --- | --- | --- | --- |
| IA / nav | 10 destinations, 3 groups | identical 10, identical path hints | **no change** |
| Framework | Astro SSG + plain-JS islands | "Astro or Next both fit" | **keep** |
| Palette | dark dev-console (`--bg:#131619`) | paper `#f4f1ea` + ink `#14161a` | **full retheme** |
| Type | IBM Plex Sans/Mono | + **Archivo 600–900** display | **new family to vendor** |
| Severity ramp | `#e5544e … #5aa6e8` (dark-adapted) | paper ramp + on-ink ramp | **retoken, two ramps** |
| Radii/shadows | 5px / 3px radii | 0 everywhere, no shadows | **strip** |
| Fail-loud behaviour | already implemented (evac null, `sourceStatus`, error blocks) | same contract, restated | **preserve, don't rebuild** |
| Front page | cards + primer + status table | black deck + hero sentence + count ledger + feed + 3 numbered sections | **rewrite** |
| Events | table + sticky JSON inspector, separate `/event` page | list + full detail pane, mobile swap | **restructure** |
| Map | multi-layer checkboxes, always-mounted canvas | single-layer chips, **conditionally mounted** canvas | **behaviour change** |
| Docs | one 738-line static reference | 11 collapsible endpoints + RUN buttons + live envelope table | **restructure, keep content** |
| Data layer | `assets/api.js` request log + footer drawer | same idea + 6s AbortController deadline | **extend** |
| Fallback data | none (loud errors only) | cached sample, badged | **decline — see §2.2** |

Roughly: **one new stylesheet, one new shell, ten screen rewrites, three new
shared modules, one store migration.** Content is largely reusable; presentation
is not.

---

## 2. The five open questions, answered

### 2.1 Place scope — resolve dynamically, keep `?place=` in the URL

The prototype hardcodes `PLACE = "ebbetts-pass"`. Today `index.astro` already
does better: `GET /api/v1/places?kind=AREA` → first area → its summary, and
`map.astro` / `roads.astro` already honour `?place=`.

**Do:** add `assets/place.js` — a single resolver used by every screen:
`?place=` in the URL → `sessionStorage` → first `kind=AREA` from the directory.
Every screen writes the active place back into its own URL so links are
shareable, and the context bar gets a compact place switcher (a `<select>` fed by
the AREA list, no new route shape). No route-per-place; `places:resolve` stays a
Places-screen feature.

### 2.2 Cached sample — **drop it**

Its stated purpose is "so the design can be reviewed in a sandbox where outbound
fetches are blocked." That need is already met, better, by
`web/screenshots/fixtures.mjs` + `make site-shots-mock`, which intercepts every
`/api/v1/*` fetch at the Playwright layer with deterministic data. Shipping a
second, drifting copy of "last known values" into the production bundle buys
nothing and risks exactly the failure the handoff itself warns about (a stale
count read as current). Every screen keeps its loud-unknown path, which is the
path that then runs. Fixtures gain any records the new screens need.

### 2.3 `/api/v1/history` — the `from` bug does not reproduce; the **latency does**

Measured against production on 2026-08-06 (`urllib`, no cache):

```
/api/v1/history?from=2027-01-01T00:00:00Z&page_size=5   → 200,  0 revisions   ✅ filters
/api/v1/history?to=2026-08-01T00:00:00Z&page_size=3     → 200, only July rows ✅ filters
/api/v1/history?place=ebbetts-pass&from=2026-08-05…     → 200, ROAD_INCIDENT  ✅ not weather-only
```

So **both handoff claims about `from` are stale** — the bound filters, and the
place-scoped call is not weather-only. Remove the `#b3261e` caveat from the
History screen and **restore the date-range controls**.

What the designer actually hit was latency; their 6 s `AbortController` turned it
into an apparent hang:

```
/api/v1/history?page_size=50 → 40.0s (timed out), 39.0s, 18.7s   (145 KB)
/api/v1/history?page_size=5  → 16.0s, 13.3s, 6.0s
/api/v1/events?page_size=60  →  2.78s      /api/v1/places → 0.99s (341 KB)
/api/v1/sources              →  0.32s      …/summary      → 2.28s
```

Root cause is in this repo: `internal/store/schema.sql` indexes
`events(status, severity DESC, observed_at DESC)` but has **no index on
`event_revisions.observed_at`**, while `Store.QueryHistory`
(`internal/store/query.go:245`) does
`ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC` over a join —
a full scan of the revision table plus a proto `Unmarshal` per row. Fix is a
migration (§6). One more real artifact to keep documented: a MESH revision with
`observedAt: 2026-09-16` (a month in the future) sorts to the top of every
history page — node clock skew, exactly the caveat the Mesh screen already
carries. Keep the 6 s deadline regardless of the index; it is correct design.

### 2.4 Tiles — keep CARTO, already vendored-adjacent and attributed

`assets/basemap.js` already uses CARTO `dark_all` over OSM with the required
`© OpenStreetMap contributors © CARTO` attribution wired through MapLibre's
`AttributionControl`. The handoff's placeholder is the same provider. **Keep
CARTO; switch to `dark_nolabels`** per the design, inside black-framed map panes.
**Do not port Leaflet** — the site is on MapLibre 4.7.1, vendored, and the design's
map styling (severity colour, `weight: 2`, `fillOpacity: 0.28`, `circleMarker` r7)
maps cleanly onto MapLibre paint properties.

### 2.5 Docs length — generate the rail

`docs-body.html` already has a `.toc` and stable `id`s on every endpoint heading.
Build the "on this page" rail from those headings and make it sticky on ≥1100px.

---

## 3. Architecture decisions

**D1 — Retheme through tokens, not per-page CSS.** Every page already resolves
colour through `app.css` custom properties (`var(--sev-EXTREME)`,
`var(--border)`, …) and the page-scoped `<style is:global slot="head">` blocks are
disciplined about it. Rewriting the `:root` block therefore repaints ~80% of the
site for free. Land the token flip first, screenshot it, and treat every place the
result looks wrong as a page-level task — that is the cheapest possible inventory
of the real work.

Token map (design → variable), added alongside the existing names so nothing
breaks mid-migration, then the old names are deleted:

```
--paper #f4f1ea   --ink #14161a      --paper-sunken #e9e5dc
--rule-strong #14161a (3px sections, 2px table heads)
--rule #cdc8be     --rule-light #e0dcd3
--ink-rule #2c3037 --ink-raised #1f232a
--signal #b3261e   --signal-on-ink #ff5544
--calm-on-ink #7dd47d   --warn-on-ink #e8a97d
sev on paper: 4 #b3261e · 3 #c2410c · 2 #a16207 · 1 #4d7c0f · 0 #1d4ed8
sev on ink:   4 #ff5544 · 3 #e0913f · 2 #d0a24a · 1 #4d9c3a · 0 #3d8bff
status: OK #4d7c0f · STALE #a16207 · UNAVAILABLE #b3261e
```

**Two ramps is the load-bearing subtlety.** Every component that can appear both
on paper and inside a black pane (severity chips, status dots, the record row's
spine) needs `--sev-*` to resolve differently by context. Implement as a scope
class — `.on-ink { --sev-EXTREME: #ff5544; … }` on the deck, response panes and
sidebar — not as a second set of class names.

**D2 — `renderEventDetail()` becomes a shared module.** The design puts the full
record (badges → headline → geometry → envelope → revision timeline → the two
curls → raw protojson) in the right column of Events. That is exactly what
`assets/pages/event-detail.js` (824 lines) already renders on `/event`. Refactor
it to export `renderEventDetail(container, id, opts)` and call it from both
`/event?id=` (standalone, permalink, mobile target) and `/events` (desktop right
column). No duplicated envelope/diff logic. `/event` stays a real route — losing
deep-linkable records to a client-side selection would be a regression.

**D3 — Fetch deadline in `api.js`, not per page.** Add a 6000 ms
`AbortController` inside `get()`, recording `{status: 'timeout', error: 'no
response within 6000 ms — request abandoned'}` in the request log and throwing
`ApiError(0, …)`. Every screen inherits it; the footer drawer shows it. Make the
deadline a parameter so History can opt into a longer one if §6 lands late.

**D4 — Suppression is a DOM decision.** The Map screen's "absent, not empty"
rule (`§5` of the contract) means the map container is created **only** after a
successful fetch with `sourceStatus !== 'UNAVAILABLE'`, and is `.remove()`d on
layer change. Never `visibility:hidden`, never an overlay. Today `map.astro`
renders `#map-canvas` statically in the page — that markup moves into JS.

**D5 — Docs content survives.** `docs-body.html` holds more reference material
than the design's Docs screen shows (CORS, place id scheme, GeoJSON layer
contract, evacuation contract, enum tables). Restructure it into the design's
collapsible endpoint list + envelope table + severity scale + lifecycle +
geometry note, and keep the remaining sections below as ordinary paper sections.
Lift `ENDPOINTS`, `FIELD_DOCS`, `CONVENTIONS` and `SEV` **verbatim** from the
prototype logic class into a new `assets/spec.js` (a data module, no DOM), and
have both the Docs page and the Events envelope table read from it — one source
of truth for field documentation.

---

## 4. Workstreams

Ordered so each phase is independently verifiable with `make site-shots-mock`.

### Phase 0 — Foundations (blocks everything)

1. **Vendor Archivo** 600/700/800/900 → `web/public/lib/fonts/archivo-{600,700,800,900}.woff2`;
   update `public/lib/README.md` with source + SIL OFL licence, matching the
   existing vendoring note. Preload 800 + 900 (the display weights above the fold)
   alongside the two Plex faces in `Shell.astro`.
2. **Rewrite `app.css` §1 Tokens** to the paper/ink set above; add the `.on-ink`
   scope; set radii to 0 and delete every `box-shadow` that isn't `inset Npx 0 0`
   or the drawer scrim.
3. **Base/typography pass** — `body` becomes IBM Plex Sans 15px/1.55 on paper
   (today it is mono 14px); `h1/h2/h3` become Archivo 900/800/800 at the design's
   sizes with the negative tracking; mono keeps every value, path, id, timestamp,
   field name, chip, eyebrow and label. `font-variant-numeric: tabular-nums` on
   numerals.
4. **Screenshot the wreckage** — `make site-shots-mock` and diff by eye. The
   output is the punch list for Phases 2–3.

### Phase 1 — Shell + shared primitives

5. **`Shell.astro`** → black sticky sidebar, 244px: masthead (THE GRID / host /
   pulsing health line / clock · read-only · no key), three nav groups with
   `inset 3px 0 0 #ff5544` on the active item, footer with schema line + request
   tally. Mobile <900px: sticky black top bar with `≡`, drawer at 260px with the
   `0 0 0 100vmax rgba(0,0,0,.5)` scrim, closing on selection. Content column
   `max-width:1280px`, padding `0 clamp(14px,3vw,32px) 110px`.
   The existing context bar's crumb + clock fold into the masthead/top bar.
6. **`assets/place.js`** — the resolver from §2.1 + the context-bar switcher.
7. **`assets/spec.js`** — `SEV`, `FIELD_DOCS`, `ENDPOINTS`, `CONVENTIONS`, lifted
   verbatim from the prototype; reconciled against `api/grid/v1/grid.proto` and
   `docs-body.html` so no field doc contradicts the proto comments.
8. **Shared components** in `app.css` + small JS builders, since five screens use
   them: **record row** (6px severity spine, meta line, Archivo 700 headline, mono
   id, `1px solid #cdc8be` bottom, `#e9e5dc` selected), **loud banner**
   (`#fdf1ef` / `1px solid #b3261e` / `inset 4px 0 0 #b3261e`, three cases),
   **response pane** (black, `GET` in `#7dd47d`, copy-curl, scrolling `<pre>`,
   status · ms · bytes footer), **chips** (`4px 10px`, `1px solid #cdc8be`
   resting / inverted active), **numbered section head** (`#b3261e` ordinal +
   Archivo 800 22px + `3px solid #14161a`).
9. **`api.js`** — the 6 s deadline (D3) + a `timeout` status in the footer drawer.

### Phase 2 — Screens

Each item is "rewrite the `.astro` markup + its island against the new primitives";
data logic is mostly retained.

10. **Grid Info** (`index.astro`) — black deck (dateline, hero sentence with the
    two accent-coloured figures, two-row mono sub-line, lede, **four fixed-track**
    count ledger — not `auto-fit`), then paper: TOP OF THE FEED (lead record +
    up to 4 rows), `01 Your first request` (live response pane),
    `02 Three contracts` (three columns, FAIL LOUD centre marked), `03
    Conventions` (definition rows from `spec.js`). Calm-state flip to `#7dd47d`
    gated on the **full** predicate: summary present ∧ 0 EXTREME ∧ 0 SEVERE ∧
    `activeEvacuations != null` ∧ `evacuationStatus === 'OK'` ∧ no UNAVAILABLE
    source ∧ the live fetch succeeded. Keep the existing donate/support card — the
    design omits it, but it is the site's fundraising ask; place it after `03` on
    paper.
11. **Events** (`events.astro` + `event.astro`) — filter bar (three chip groups +
    echoed `GET` URL), record rows, `minmax(280px,380px) minmax(0,1fr)` when
    selected, single column when not, list/detail swap + "← back to list" under
    900px. Detail column calls the shared `renderEventDetail` (D2); on narrow
    viewports selection navigates to `/event?id=` instead.
12. **Map** (`map.astro`) — nine layer chips, single active layer, conditional
    mount (D4), three-case loud banner with `metadata.sourceUrl` as a real link,
    STALE data-age indicator from `generatedAt − lastSourceUpdate`, metadata
    table. This *removes* today's multi-layer checkbox overlay — call that out in
    the changelog as a UX change.
13. **Roads** — the two-idiom prose (incidents are events; conditions are the
    `road_segment` / `chain_control` layers), incident record rows, and the
    carefully-worded empty state verbatim from the handoff.
14. **Mesh** — node table (Node/Type/SNR·RSSI/Hops/Heard), `—` never `0` for
    missing telemetry, the "answer from `observedAt`, not `telemetry.lastAdvertAt`"
    caveat, and the INFO-is-excluded-from-mode note.
15. **Places** — directory table, client-side kind chips, id-scheme prose. Keep
    the existing `places:resolve` tester; the design doesn't cover it and it is
    the only interactive demo of that endpoint. Note the 341 KB payload — render
    progressively, don't block the table on geometry.
16. **Sources** — health board with filled status chips, staleness-rule prose,
    attribution + evacuation reference-only closing line.
17. **History** — record rows with `rev {n}`; **date-range controls restored**
    and the upstream-bug caveat **removed** per §2.3; a visible note that
    `observedAt` is upstream-stamped and can be future-dated.

### Phase 3 — Reference

18. **Docs** (`docs.astro` + `docs-body.html` + `assets/pages/docs.js`, new) —
    collapsible endpoint entries from `spec.js` with parameter tables and **RUN**
    buttons (live fetch → black pane with status/ms/bytes, green ok / `#e8a97d`
    not-ok), one open by default; envelope table with the live sampled record and
    a "next event" cycler; severity scale (74px filled blocks, with the
    "urgency not magnitude, EXTREME evacuation outranks an M5" copy); lifecycle
    diagram; geometry base64 note. Remaining reference sections restyled below.
    Sticky "on this page" rail from existing heading ids (§2.5).
19. **MCP** (`mcp-guide.astro`) — restyle only; the content already matches the
    design's description (endpoint, `mcpServers` block, `mcp-remote` bridge,
    `claude mcp add`, the eight-tool table, the ask→tool-call table). Verify the
    tool table still matches `internal/mcp` before shipping.

### Phase 4 — Verification & docs

20. **Screenshot harness** — add `mcp-guide` to `PAGES` in
    `screenshots/capture.mjs`; extend `fixtures.mjs` with an UNAVAILABLE layer, a
    STALE layer, a null-`activeEvacuations` summary, and a calm-state summary so
    all four fail-loud states are captured, not just the happy path. Capture all
    three viewports before/after.
21. **`make check-site`** — `site/dist` is a committed artifact; the final commit
    must include a fresh `make site`, or `docker-build` fails.
22. **Docs** — new `web/CLAUDE.md` recording the design system (paper/ink split,
    the two severity ramps and why, the `.on-ink` scope, "if it came from the API
    it is mono", radii-0 rule) so the next change doesn't drift. Update
    `docs/data-sites-spec.md`. `CHANGELOG.md` entry — the API surface is
    unchanged, so this is a site entry plus the §6 latency fix.

---

## 5. Fail-loud regression checklist

These already work today. The redesign must not quietly lose them — verify each
by fixture after Phase 2:

- `activeEvacuations: null` → "evacuation count unknown — not zero", never `0`, never blank.
- Failed fetch → `—`/UNKNOWN at heading size + red health pill + a banner naming the reason.
- Calm requires the full seven-part predicate (§4.10).
- `sourceStatus: UNAVAILABLE` → map element absent from the DOM + `sourceUrl` link.
- `sourceStatus: STALE` → last-good features **plus** a data-age indicator.
- Evacuation `description` verbatim; AI text badged; original always reachable.
- Attribution rendered wherever data is; evacuation carries reference-only framing + Genasys link.
- Severity colour never alone — always with its text label.

---

## 6. Backend work (independent of the redesign, do it anyway)

**`event_revisions.observed_at` index.** Append a migration to
`internal/store/migrations` (append-only ladder — never edit a shipped entry):

```sql
CREATE INDEX idx_revisions_observed
  ON event_revisions(observed_at DESC, event_id ASC, revision DESC);
```

matching `QueryHistory`'s exact sort so SQLite can walk the index instead of
scanning + sorting. Update `schema.sql`'s trailing comment per the store's
convention. Re-measure `/api/v1/history?page_size=50` before/after; target is
sub-second, and it is the difference between the History screen being usable and
it living permanently behind a 6 s abort. If the index alone is not enough, the
next lever is not rehydrating full proto blobs for list rows — but measure first.

---

## 7. Risks

| Risk | Mitigation |
| --- | --- |
| Token flip regresses a page in a way screenshots don't show (hover, focus, selected) | Explicit hover/focus/selected pass per screen; `:focus-visible` must stay visible on paper |
| `renderEventDetail` refactor (D2) breaks the existing `/event` page | Refactor **before** Phase 2.11, screenshot `/event` unchanged, then consume it from Events |
| Contrast on paper — `#8a8f98` tertiary on `#f4f1ea` is ~3.3:1 | Restrict tertiary to non-essential labels; anything load-bearing uses `#4b5058`+ |
| Archivo 900 at `clamp(…,56px)` shifts layout on slow fonts | `font-display: swap` + preload 800/900; hero has a fixed `max-width: 30ch` |
| `site/dist` churn makes the diff unreviewable | Review `web/` in the PR; regenerate `dist` in one final commit |
| History screen ships while still 18 s | Land §6 first; it is small and independently testable |
| Mobile drawer scrim (`0 0 0 100vmax`) + `overflow-x: hidden` on body | Verify on the 390px viewport in the harness, both drawer states |

---

## 8. Explicitly out of scope

Leaflet (staying on MapLibre) · the prototype runtime `support.js` · a
route-per-place URL scheme · the cached-sample fallback (§2.2) · any change to
`/api/v1` response shapes · dark mode (the design is a committed single look).

---

## 9. Suggested sequencing

1. §6 index migration (small, independent, unblocks History).
2. Phase 0 → screenshot → punch list.
3. Phase 1 (shell, `place.js`, `spec.js`, primitives, `api.js` deadline).
4. D2 refactor of `event-detail.js`.
5. Phase 2 screens: Grid Info → Events → Map → Roads → Mesh → Places → Sources → History.
6. Phase 3 Docs → MCP.
7. Phase 4 harness, `make site`, `web/CLAUDE.md`, CHANGELOG.

Phases 0–1 are one PR (nothing works properly until the tokens and shell land
together). Phases 2–3 split per screen. Phase 4 is the shipping commit.

---

## 10. What the post-implementation review found

A six-lens adversarial review ran over the finished diff (fail-loud, JS
correctness, design fidelity, fixtures/harness, a11y/responsive, docs-truth),
with every raised finding handed to an independent skeptic prompted to refute
it. **47 raised, 38 survived refutation, all 38 fixed.** The nine that were
refuted were mostly style preferences or scenarios blocked by a guard elsewhere.

Worth recording, because the pattern generalises:

### The fail-loud bugs were all in the *seams*, not the logic

Every one of the three high-severity fail-loud defects sat where two correct
pieces met:

1. **`home.js` — the deck sub-line.** The hero said `UNKNOWN` and every ledger
   cell said `?` when the event query failed, but the sub-line between them
   printed `· 0 severe · 0 moderate · 0 in the past 24h`. Two of the three
   surfaces were gated on `eventsOk`; the third was never wired up. A failed
   fetch painted three reassuring zeros on the most prominent surface of the
   site.
2. **`map.js` — the layer toggle.** The conditional-mount rule was correct and
   ran on every path *inside `reloadAll()`*. Unticking a layer is not one of
   those paths, so removing the last drawable layer left a rendered basemap with
   zero features and no banner — the exact state §5 forbids, reachable in two
   clicks from the screenshot that proves the rule works.
3. **`map.js` — partial failure.** The "everything failed" branch required
   `failed.length === results.length`. A mix of *one failed* layer and eight
   OK-but-empty ones therefore fell through to the final `else`, which asserts
   "Every selected layer answered OK … a confirmed empty result — not a
   failure". Any partial failure produced a confident false all-clear.

The lesson for anyone extending this: **the mount/gate rule must run on every
path that changes the inputs, and any unknown among the inputs poisons the whole
claim.** Enumerate the paths, not the states.

### The fixtures were validating the UI against a fiction

The harness is the only automated check this site has, and several fixtures did
not match the API they stood in for — so screens were "verified" against shapes
the server never emits:

- `PlaceSummary.summary` is a `SummaryStats` **message**; the fixture had a
  string, with `activeEvacuations` at the top level. The front page therefore
  rendered "evacuation count unknown" in every screenshot **for the wrong
  reason** — the fail-loud path looked correct while actually being fed
  `undefined`. This is the worst failure mode a fixture can have: it makes a
  broken thing look right *and* a right thing look broken.
- `Enhancement` carried `{summary, impact, duration}` — none of which exist on
  the message (`{model, enhancedAt, fields, request, response}`).
- No event carried a `detail` oneof, so the event detail pane rendered its
  "no typed detail block" branch for every record in every screenshot.
- `SummaryDomain.status` had invented values (`ACTIVE`, `WATCH`); it is a
  *source* status (`OK|STALE|UNAVAILABLE`).
- `md()` never emitted `lastSourceUpdate`, so the STALE data-age branch was
  unreachable.
- The GeoJSON fixtures defined `mesh_node` (which no page requests) and omitted
  `mesh_link` (which `map.js` does).

**Rule: a fixture is a claim about the API and must be checked against the proto,
not against what makes the page look populated.**

### Everything else

Design fidelity (`.on-ink` missing from three ink surfaces, severity chips
hardcoding `#fff` against the inverted ramp, `--signal` not re-pointed so the
focus ring fell below 3:1 on black), responsive (the Events two-column swap keyed
on *viewport* width while the grid is inset by the 244px sidebar — fixed with a
container query on a wrapper, since an element cannot query its own container),
accessibility (the closed mobile drawer stayed focusable and announced — now
`inert`; no focus management on open/Esc), and docs-truth (`telemetry.path[]` is
a **reserved** proto tag, "nine layer slugs" is ten, `GET /api/v1/mesh/links` was
undocumented, two dead `#ep-*` anchors, and the severity table printed the old
dark-console hexes beside swatches showing the new paper ramp).
