# The site (`web/` → `site/dist`) — the broadsheet design system

Astro 5 SSG. Source in `web/`, built into `../site/dist` and embedded by
`site/embed.go`. **`site/dist` is not committed** — it is git-ignored and built
on demand: `make site-ensure` (a prerequisite of `make server`, `run`, `test`)
rebuilds it whenever it is missing or older than the source here, and the
deploy image builds it itself in the Dockerfile's `site-builder` stage. So there
is nothing to commit and no stale-artifact guard to run; just edit and build.

Two committed `.gitkeep` files keep that working, and neither is decoration.
`site/dist/.gitkeep` exists because `//go:embed all:dist` is a *compile error*
when the directory is missing — without it, a fresh clone would not build Go
until someone ran Node. `web/public/.gitkeep` exists because `astro build`
**empties `outDir`**, deleting the first one; Astro copies `public/` in
verbatim, so every build path — `make site`, a bare `npm run build`, the image
stage — puts it straight back.

No client framework: pages are `.astro` files rendering static markup, and the
live parts are plain-JS modules under `public/assets/` loaded with
`<script is:inline type="module">`. Astro ships no runtime JS of its own; keep it
that way.

**There is one header, and it is the sidebar.** Desktop has no context/breadcrumb
row: everything such a row would carry — the wordmark, the current destination
(the highlighted nav item), the clock — the sidebar already holds, and a second
bar repeating "grid.v1 / Docs" under "THE GRID" was pure duplication. The
content column starts at the top of the viewport. Under 900px the sidebar
becomes a drawer and `.topbar` carries the hamburger, the page name and the
health dot. Do not reintroduce a desktop header row; if something needs
persistent chrome, it goes in the sidebar.

**There is no global place switcher.** The service covers one AREA, so a chrome
control offering one choice was furniture. Place scoping is unaffected —
`?place=` works on any URL and `assets/place.js` still resolves it — and the
genuinely place-scoped screens (Map, Roads, History, Places) carry their own
in-page selector. See the note at the foot of `place.js` before adding one back.

## The split: paper for reading, black for data

The site does two jobs — it teaches the API and it is a working client of it —
and they are separated **typographically**, not crammed into one surface.

- **Paper** (`--paper #fbfbfa`) — prose, reference, tables, navigation of ideas.
- **Ink** (`--ink #14161a`) — anything that *is* the API: the front-page deck,
  response panes, code blocks, the sidebar. Map panes keep an ink hairline
  *frame* but are backed with paper — the basemap is light (see `basemap.js`).

The paper family went **neutral on 2026-08-11**: paper `#f4f1ea → #fbfbfa`,
paper-sunken `#e9e5dc → #f0f0ee`, rule `#cdc8be → #d5d5d2`, rule-light
`#e0dcd3 → #e8e8e6`, text-faint-p `#9a958c → #9b9b98`, and the Events row hover
`#efebe2 → #f4f4f2`. Ink, the accent and both severity ramps are untouched, and
`--on-ink` stays `#f4f1ea` — the cream is now a property of *type on black*, not
of the page. `app.css :root` is the palette; there is no other copy of it.

Radii are **0** everywhere (rounded corners break the broadsheet read) and there
are **no drop shadows**. `box-shadow` is a hairline device only: `inset Npx 0 0`
left-edge markers, and the `0 0 0 100vmax rgba(0,0,0,.5)` mobile drawer scrim.

## One set of variable names, resolved by surface

This is the subtlety that bites. The same component — a severity chip, a status
chip, a record row's spine, a heading — appears on paper *and* inside black
panes, and one palette cannot be legible on both. So the tokens are
**surface-neutral names that resolve by context**:

- the paper values are the `:root` default;
- the **`.on-ink` scope** re-points them at their dark-adapted values. It is
  applied to `.deck`, `.resp-pane`, `.code`, `.inspector`, `.primer`,
  `.sidebar`, `.topbar`, `.qbar`, `.env-bar`, `.site-footer`.

What the scope re-points: the severity ramp (`--sev-*`, `#b3261e…` →
`#ff5544…`), the mode and source-status ramps, **the whole type ramp**
(`--text-primary`, `--text-body`, `--text-2nd`, `--text-muted-p`,
`--text-tertiary`, `--text-faint-p`), the rules (`--rule`, `--rule-light`, and
`--rule-strong`, which inverts — it *is* the ink on paper, so on ink it must
become `--on-ink` or a 3px section rule vanishes), `--paper-sunken`, and
`--signal` → `--signal-on-ink`.

**Never add a second set of class names for this.** To put a component on a black
surface, put it inside something carrying `.on-ink`.

JS that paints canvas/WebGL can't inherit CSS variables, so `format.js` exports
both ramps explicitly: `SEVERITY_COLORS` (paper) and `SEVERITY_COLORS_ON_INK`
(anything drawn on a black surface). Picking the wrong one is the common
mistake, and **the basemap decides it for maps**: the base style is now
OpenFreeMap Positron, a LIGHT style, so all map geometry — the layer previews,
event detail, place outlines — takes the **paper** ramp. It took the ink ramp
for as long as the basemap was CARTO dark; changing the basemap is precisely the
moment this flips, and four call sites had to move together.

## Three families, one job each

| Family        | Job                                                                    |
| ------------- | ---------------------------------------------------------------------- |
| Archivo 600–900 | Display: headings, record headlines, the hero sentence, big numerals |
| IBM Plex Sans | Prose, body copy, table cell text. Base 15px/1.55, `text-wrap: pretty`  |
| IBM Plex Mono | Every value, path, id, timestamp, field name, chip, eyebrow, label      |

**If it came from the API, or names something in it, it is mono.** Archivo is
always tight (`letter-spacing` −0.01em to −0.05em, largest sizes tightest) and
has no 400 weight — body text is Plex Sans, deliberately.

All faces are vendored in `public/lib/fonts/` (see `public/lib/README.md`); the
site loads no third-party asset at runtime.

## Measure and rhythm — the two spacing rules

**Line length is a token, and it is in `em`, never `ch` or `px`.**

```
--measure: 36em        ~70 characters   body prose, notes, table intros
--measure-lead: 32em   ~62              large lead text
--measure-tight: 28em  ~55              captions, column text
--measure-mono: 42em   ~70 for MONO     mono is a wider face — see below
```

Three things to know, each of which was a real bug before it was a rule:

1. **CSS `ch` is not a character.** It is the advance width of `0`, which in
   both Plex faces is markedly narrower than the average glyph — `max-width:
   72ch` rendered ~94 characters per line, well past where the eye loses the
   line start. Measured against these faces one character averages ~0.51em.
2. **`em` keeps characters-per-line constant across font sizes.** One token
   holds a 12px note and an 18px lead both near 70 characters. A px measure
   cannot do this, and is why every size used to need its own value.
3. **Mono needs its own token.** IBM Plex Mono runs ~0.60em per character
   against ~0.51em proportional, so `--measure` on a mono block yields ~60
   characters and reads cramped. `--measure-mono` is the same ~70, for mono.

Running text gets the measure automatically: `app.css` applies it to direct
children of `main` and of its sections (`main > section > p`, `ul`, `ol`, …).
That is exactly what running prose is, which is why the long-form partials
(`docs-body.html`, the MCP guide) can stay plain semantic HTML. A `<p>` inside a
card, record row, response pane, table cell or grid column is **not** running
text — it is sized by its container, which is why this is not a blanket
`main p` rule.

**Vertical rhythm is three steps, owned centrally:**

```
--space-section: 44px   between top-level sections
--space-sub: 28px       between sub-sections (h3 blocks)
--space-block: 14px     heading to body, block to block
```

Sections must not invent their own gaps. Before this existed the reference page
measured 0, 12, 18, 24 and 26px between consecutive sections — the gap was
whatever the first and last child's margins happened to collapse into, which
reads as accidental because it was.

Verify both with `node screenshots/metrics.mjs` (see *Verifying a change*).

## Components: Astro for markup, custom elements for controls

There is no UI framework and there should not be one — eleven pages, mostly
static, a handful of live regions. Repetition is handled at two levels instead:

**Astro components** (`src/components/`) for markup that repeats across pages —
`FilterGroup`, `SectionHead`, `PageHead`, `GetChip`, `EventDetailStyles`. Build
time only; they emit HTML and disappear.

**Custom elements** (`public/assets/components/`) for the *live* controls:

| Element | Replaces |
| --- | --- |
| `<grid-chip-row>` | four hand-rolled chip builders (Events facets, Map layers, History layers, Places kinds) |
| `<grid-menu>` | the native `<select>`, and two copies of its keyboard handling |

**Light DOM only. Never attach a shadow root.** The entire design system is
custom properties inherited through the tree — `--sev-*` re-pointed by
`.on-ink`, the measure and rhythm tokens, the chip and record-row classes. A
shadow boundary cuts every one of those off, and the fix for that is copying
tokens into the component, which is precisely the drift these elements exist to
end.

**A shared element's CSS belongs in `app.css`, not in a page's style block.**
`.chip-toggle` was first defined inside `events.astro`'s `<style is:global>`;
the moment `<grid-chip-row>` was reused, the same component rendered unstyled
on three other pages. Page style blocks are for what is genuinely page-local.

### Islands bind to markup by id — and that binding is checked

`make site` runs `screenshots/wiring-check.mjs`, which fails the build if an
island looks up an id its page does not define. This exists because renaming an
id on one side shipped as `Cannot read properties of null (reading
'addEventListener')` in a browser, thrown halfway through `init()`, far from the
edit that caused it and with the page left half-wired.

Islands should resolve their elements through `requireEls()` (`assets/ui.js`),
which fails once, up front, **naming the missing id** — not on whichever line
happens to touch it first.

## The four page-level components

Every page is built from these. If you find yourself writing a variant, the
variant is the bug — five of these existed in three-to-six flavours each, and
the result was that moving between pages felt like moving between sites.

| Concern | Use | Not |
| --- | --- | --- |
| Hero | `<PageHead kicker title …>` + a one-line `.lead` | a hand-rolled `<h1>`; a paragraph |
| Control mechanics | `.rail-caption` under the hero's rule | prose in the hero |
| Section intro | `<p class="section-intro">` | `class="prose" style="font-size:13px…"` |
| Echoed request | `.req-echo` | `.echo-query`, `.query-line`, `.query-row`, a bespoke curl line |
| Table | `.data-table` (+ `.cell-name` / `.cell-sub`) | a page-local `<table>` |
| Table under a column legend | add `.capped-head` | a header row that repeats the cap |
| Filter set | `.filter-set` > `<FilterGroup param note>` | a `.filters` label/`.fcell` grid |
| Facet control | `<grid-chip-row>`, `<grid-menu>`, `.filter-input` | a native `<select>` |
| Place picker | `placeMenuOptions()` / `placeMenuLabel()` (`place.js`) | a hand-filled option list |
| Copy control | `copyButton()` / `wireCopyButton()` / `copyOnClick()` | a click handler that retitles the button |

### The filter set

One shape on every screen that filters — Events, Map, History, Places, Roads:

```astro
<div class="filter-set">
  <FilterGroup param="layer" note="any of · empty = all layers"
               control="grid-chip-row" id="hist-layers" mode="multi" />
  <div class="filter-row">
    <FilterGroup param="place">
      <grid-menu id="hist-place" trigger-label="any place"></grid-menu>
    </FilterGroup>
  </div>
</div>
```

The label is the **query parameter's name**, because the reader is composing a
URL. `.filter-set` owns the 3px rule that divides the query from its results —
do not add one. Facets sharing a `.filter-row` land their controls on one line:
a label with a `note` is two lines tall and one without is one, so a row that
mixes them reserves both lines for every label (`:has(.filter-note)`), and every
control — chip, menu trigger, input, button — is the same 29px tall.

Chips refetch on click everywhere except History, whose Apply exists because a
date-range query over the revision archive is expensive. There, `state` holds
the **applied** query and the controls hold the draft; do not let a control's
change handler write to `state`, or "Load more" pages a query nobody ran.

### Copying never changes the page

The whole point of a copy click is that nothing happens on screen, so the
feedback is an icon that becomes a tick — never a text swap. Replacing a
seventy-character id with "copied id" hid the value and collapsed the line;
retitling a button to "copied" resized it and nudged its neighbours. Contract
checks 11/12 in `events-contract.mjs` pin both: same text, same box.

The icons are inline SVG. ✓ and ⧉ are not in the vendored Plex faces, so as
glyphs they fall back to whatever the OS has, at a different weight per
machine.

### The hero: a kicker, a title, and ONE LINE

```
KICKER                                                     [badges]
Title  ·  meta
One-line standfirst.
───────────────────────────────────────────────────────────  3px rule
FILTERS  = mechanics, mono 10.5px, attached to the rail below
```

**Every page's hero carries a kicker**, and the kicker is not the title in other
words — Map's said "Layer previews" above the title "Map — layer previews",
which is the same thing three times counting the nav.

**The standfirst is one line** (`17px/500`, capped at `46ch`). A hero says what
the page is; the moment it runs to a paragraph it competes with the page. Seven
pages had four-line leads, which at the reading measure left ~600px of empty
paper beside them and read as stranded. Putting the lead in a second column
beside the title fixed the emptiness by making the whole block heavier — the
answer was to shorten the sentence.

`46ch`, not the `44ch` first specified: measured, `ch` is 10px at this size and
the reference sentence is 452px, so 44ch wrapped it to two lines by 12px. This
is the one place `ch` belongs — it is a ceiling on display copy, not a reading
measure (see the measure note above).

**Mechanics do not go in the hero.** How the filters map to query parameters,
what the URL means, what a layer's `sourceStatus` implies — all of that belongs
to the thing it describes, as a `.rail-caption` directly under the rule:

```astro
<p class="rail-caption">
  <b>Filters</b>= query params of <code class="inline">/api/v1/events</code> ·
  every click refetches and rewrites the URL — the address bar is the permalink
</p>
```

Mono 10.5px, the label in ink, and **`max-width: none`** — it is a direct child
of `main`, so it would otherwise inherit `--measure`, which is in `em` and
resolves to 378px at 10.5px, wrapping one line into three. That is the same
em-measure trap `--measure-intro` exists for.

Contract sentences that lived in a hero — "an UNAVAILABLE layer draws nothing,
and nothing on the map is not an all-clear" — move into the caption or the
section intro. They are obligations (see *Fail-loud is a UI obligation*); do not
drop one while shortening a lead.

### Running text has three sizes and ONE column edge

`--measure-lead`, `--measure` and `--measure-intro` are tuned so the page lead
(18px), body prose (15px) and a section intro (13.5px) all render **540px wide**
at desktop. Measure is in `em`, which holds characters-per-line constant within
a size — but across sizes that means a 13px paragraph and an 18px lead under the
same token come out 468px and 576px, and a page carrying both looks ragged for
no reason a reader can name. Constant characters within a size; a constant edge
across them.

**Never set `font-size` on running text inline.** That is what broke it: ten
paragraphs across five pages each picked their own size and margin, which
silently changed their width too.

**A new size needs a token, or it lands on its own edge.** `.notice` was 13px
inheriting `--measure` (36em) and came out **468px** in a stack of 540px
intros — two right edges 72px apart, which is what makes a column read as a
zig-zag. It is 13.5px on `--measure-intro` now. Before adding a fourth size,
try one of the three; if you must add one, tune its measure to 540px and say
so where you define it.

Blocks are one of exactly two widths: **full column** (a rail, a table, a
record list, a `.req-echo`, a `.rail-caption`) or **the 540px measure** (prose,
a `.notice`, a `.section-intro`). Nothing is in between.

### A cap and a header row should not say the same thing twice

When a section's `cap` lists the table's columns in order, the header row eight
pixels below it is a second copy — add `.capped-head` and it is hidden.
**Hidden, not removed:** a screen reader still needs the `<th>` association to
say which column a cell is in, and losing that to save a line of paint is a bad
trade.

Only where the cap is a **complete** legend. Roads' caps listed four of six
columns and three of four; dropping those header rows would have left a sighted
reader with no way to learn what the others were, so the caps were completed
first. Where a cap is prose rather than a legend — Map's "a degraded row says
why", Sources' board line — the header row stays.

### The right-hand void, and the two primitives for it

That two-width rule has a consequence: prose fills 44–56% of the column, so
**every paragraph has a void beside it**. One paragraph is fine. A *run* of them
is a hole, and that is what reads as unfinished — the page appears to have
stopped halfway across.

`metrics.mjs` reports **`tallestHole`** (the tallest contiguous run of blocks
filling <70% of their column) and `pctNarrow`. **Keep `tallestHole` under
~240px.** Nothing measured this before, which is exactly why it kept being found
by eye, fixed on one page, and coming back on the next. Captures now include a
**1920 `wide`** viewport too: the void is widest above 1680 and the harness had
never looked past 1440.

Do not widen the prose to fix it — 70 characters is the measure. Put something
in the space:

| | `.with-rail` | `.with-notes` |
| --- | --- | --- |
| The right column belongs to | the whole **page** | individual **paragraphs** |
| Behaviour | sticky, 300px (`--rail-w`) | scrolls, hairline left border |
| Holds | request, response shape, standing caveats | asides on the prose beside them |
| Use when | the data is a **list** — a long scroll the reference should outlive | the data is a **wide table** that cannot give up 300px |

That last row is the deciding question, and it is not a matter of taste. History
took the rail: its data is a record list, so the narrower column costs nothing
and the clock caveat is still on screen 400 revisions down. Sources took
sidenotes: its board is six columns of per-source health, and a rail would have
taken a third of it.

**MapLibre: never gate an add on `map.isStyleLoaded()`.** It reports whether
every SOURCE has finished loading, not whether the style exists — so binding the
first layer made it false and every later layer in the same pass early-returned.
Re-running the pass rebound the first layer and invalidated the style again: a
livelock that shipped as "the map is empty until you toggle a layer off and on"
(3 of 9 layers bound on load, measured live). Gate on a `styleReady` flag set in
`style.load`, reconcile through one idempotent `syncMapLayers()`, and wrap each
layer so one bad geometry cannot strand the rest. The fixtures could never catch
it: the harness blocks tile requests, so the fallback style loads instantly and
the race never opens. `screenshots/live-check.mjs` drives the built page against
the real API with real tiles, and reproduced it first try.

`position: sticky` in a grid needs `align-self: start` — the default `stretch`
gives the item the row's full height, and a full-height box has nothing to stick
within. Both collapse to one column under 1100px, rail first, because the rail
holds the request and that is what a reader wants before the rows.

**A sidenote is not a narrow block.** `metrics.mjs` descends into `.rail-main`
and `.with-notes`, measures their children against *that column's* width, and
skips `.sidenote` — otherwise wrapping a page in a two-column region would
report a clean page while the hole simply moved inside it.

## Shared primitives (app.css §14)

Used by five or more screens — reach for these before writing page CSS:

- **`.deck`** — the full-bleed black band: `.dateline`, `.hero` (with `.figure`
  spans in the state accent), `.subline`, `.deck-lede`, `.ledger`. **It renders
  through Shell's `deck` slot, outside `main`** (`<div class="deck" slot="deck">`),
  because `main` is a centred `--content-max` column and nothing inside it can
  reach past that however far its negative margins break out of the padding —
  the band stopped at 1280px with paper down both sides on a wide screen. The
  padding lives on `.deck-inner`, which re-establishes the column so the hero
  still lines up with main's text. Same split as `.site-footer`/`.footer-inner`;
  any new full-bleed band follows it.
- **`.ledger`** — the count strip. **Four fixed tracks.** Never `auto-fit`: it
  wraps to 3 + 1 and paints the empty fourth slot as a dark void. The 1px grid
  gap over an `--ink-rule` background is what paints the hairlines.
- **`.rec`** — the record row (Events, Roads, History, the front feed). Built by
  `format.js`'s `recordRow()`: 6px severity spine, mono meta line, display-face
  headline, mono id.
- **`.loud-banner`** / **`.error-block`** — the fail-loud surfaces.
- **`.resp-pane`** — a live request and its actual response, in black.
- **`.numbered-head`**, **`.def-row`**, **`.env-row`**.

## There are no legacy aliases left

The first pass shipped an alias layer in `:root` mapping the pre-broadsheet dark
palette's names (`--bg-card`, `--text-hi`, `--grn`, `--blu`, …) onto the paper
system, so untouched screens would repaint coherently. **It has been deleted.**
Every screen now resolves colour through the tokens above.

Two reasons it had to go, beyond tidiness. It hid how much was unconverted — six
screens looked done while still being the old markup in new colours. And the
mapping quietly lied in places: `--grn` resolved to `--signal` (red), so the AI
enhancement badge — whose whole job is to read as "a machine wrote this, here is
the original" — rendered in the alert accent.

If you find one of those names, it resolves to nothing. Replace it with the real
token rather than reinstating the alias.

## Fail-loud is a UI obligation, not a backend one

The API's contract is that absence is never an all-clear, and the UI is bound by
it. In particular:

1. **`null` ≠ `0`.** `activeEvacuations: null` renders as the word UNKNOWN
   ("evacuation count unknown — not zero"), never `0`, never blank.
2. **A failed or timed-out fetch never renders as zero.** It becomes `—`/UNKNOWN
   at heading size plus a banner naming the request.
3. **Calm is a positive assertion** and needs *every* input known: summary
   present ∧ 0 EXTREME ∧ 0 SEVERE ∧ `activeEvacuations != null` ∧
   `evacuationStatus === 'OK'` ∧ no UNAVAILABLE source ∧ the live fetch
   succeeded. See `isCalm()` in `pages/home.js`. Any unknown ⇒ not calm.
4. **An UNAVAILABLE map layer is suppressed, not drawn.** The map element is
   **absent from the DOM** — `map.js` mounts `#map-canvas` into `#map-mount`
   only once a selected layer returns drawable features, and `map.remove()`s it
   otherwise. Never `visibility: hidden`, never an overlay: a rendered basemap
   with zero features reads as "all clear". The `map-suppressed` screenshot
   fixture exists to catch a regression here.
5. **STALE shows its age** — last-good features plus a data-age indicator.
6. **Directive life-safety text is never rewritten.** Evacuation `description`
   appears verbatim; AI text is badged and the original stays reachable.
7. **Attribution renders wherever data does**; evacuation always carries its
   reference-only framing and the Genasys link.

**Enumerate the paths, not the states.** An adversarial review of the first pass
found three high-severity fail-loud defects and all three sat in a *seam* — two
correct pieces meeting — not in the logic:

- The front page gated the hero and the ledger on `eventsOk` but not the
  sub-line between them, so a failed fetch painted `0 severe · 0 moderate · 0 in
  the past 24h` across the most prominent surface on the site.
- The Map's conditional-mount rule ran correctly on every path *inside*
  `reloadAll()`. Unticking a layer is not one of those, so removing the last
  drawable layer left a basemap with zero features — two clicks from the
  screenshot that proves the rule works.
- The "everything failed" branch required `failed.length === results.length`, so
  one failed layer among eight OK-but-empty ones fell through to the branch that
  asserts a confirmed-empty all-clear.

Two rules follow, and `map-contract.mjs` exists to enforce the first: the gate
must run on **every path that changes its inputs**, and **any unknown among the
inputs poisons the whole claim**. Reaching for a screenshot proves nothing here —
none of those three states is reachable by loading a page.

## Network

Every data fetch goes through `assets/api.js` — same-origin `GET /api/v1/*`,
logged into the request drawer in the footer. **No other module performs network
I/O** (map tiles are furniture, not data).

Requests run under a **6 s `AbortController` deadline** (`DEFAULT_TIMEOUT_MS`); a
hang is logged as a timeout and thrown as an `ApiError` so it takes the same loud
path as any other failure. A screen stuck on "fetching…" is neither a value nor
an admission of unknown, which is why the deadline exists.

`assets/place.js` resolves the active place once per page: `?place=` → session
storage → first `kind=AREA`. The URL wins so a shared link renders what the
sender saw. It is pure resolution — no DOM access.

## Verifying a change

```bash
make site                    # build site/dist (git-ignored; nothing to commit)
make site-shots-mock         # all pages × mobile/tablet/desktop, mocked data
make site-shots-mock PAGES=map,home ONLY=desktop

# Layout metrics — characters per line, section gaps, horizontal overflow,
# at 390/768/1280/1440/1920. Screenshots show that something looks wrong;
# this says what and by how much, so a spacing change is a diff of numbers.
node screenshots/metrics.mjs
node screenshots/metrics.mjs --pages docs --widths 1440

# The Map screen's fail-loud mount rule, driven rather than photographed.
node screenshots/map-contract.mjs
```

`map-contract.mjs` exists because the defects the review found on that screen
were all on paths **no screenshot reaches**: unticking the last drawable layer,
and a partial failure (one layer down, others OK-but-empty). It walks those
transitions and asserts the map element is absent, the banner is present, and no
partial failure is ever reported as a confirmed-empty. Run it after touching
`map.js`; a screenshot of the happy path proves nothing about them.

### Measure it; do not look at it

```bash
node screenshots/probe.mjs /events.html '.rec-head' '.rec-id'   # font, colour, box
```

**Never state a dimension, colour or alignment from reading a screenshot.** The
captures are scaled before you see them, and reading them produced four
confidently wrong conclusions in one session: a black deck called white,
left-aligned keys called right-aligned, and twice a type scale that was 30% too
small called fine. `probe.mjs` answers all of those in seconds from the DOM.
Screenshots are for *does this look right*; the probe is for *what is it*.

**When a written spec and a picture disagree, the picture wins.** The original
Claude Design bundle and its `.dc.html` prototypes have been deleted — the site
has diverged from them (the palette, the filter set, the copy affordances, the
deck) and a stale mock misleads more than it guides. The lesson it taught
outlives it: the filter region was first built from prose as a collapsed
EDIT/APPLY panel with dashed chips and segmented rails, and one look at the
prototype settled it as a label and a row of chips. If a future change arrives
as a mock, look at the mock before building from the words next to it.

### Pin properties, not implementations

A contract test that asserts `resting height <= 44px`, `border-style: dashed` or
`box-shadow: inset 0 0 0 2px` breaks the moment the design changes, and it
breaks *as a failure* — which reads as a regression when it is a stale
assertion. Three of these had to be rewritten. Assert the thing that must remain
true: the selected row **differs from its siblings**; the filters are **visible
without a disclosure**; a failed layer is **not reported as empty**.

Two numbers are regressions whatever they look like: any page whose widest line
exceeds ~85 characters, and any page whose **`hScroll`** is true (a phone
scrolling sideways).

**Check `hScroll`, not `body.scrollWidth`.** `metrics.mjs` computes the verdict
itself now, because the distinction bites: a 13px copy icon overflowing a
clipped nowrap line pushed `documentElement.scrollWidth` to 555px at a 390px
viewport while `body.scrollWidth` stayed 390. A check against body reported the
page clean while the phone scrolled.

**And overflow of this kind is content-driven, so fixtures may not show it.**
The icon only escaped because a live event id is long enough to fill the line
first. Sweep the real API before believing a mobile pass:

```bash
node screenshots/live-check.mjs index 390     # a phone, real data, real tiles
node screenshots/live-check.mjs map           # the map, with tiles that load
```

`live-check.mjs` drives the built page against `data.sierragridteam.org` and
exits non-zero on horizontal scroll or a page error. It exists because the
fixture harness has two structural blind spots — it blocks tile requests (so the
basemap never races) and its ids are short (so nothing overflows). Both cost
real bugs.

`screenshots/fixtures.mjs` answers every `/api/v1/*` fetch deterministically, so
runs diff cleanly by eye. It deliberately includes degraded states (an
UNAVAILABLE layer, a STALE source) — when you add a fail-loud path, add the
fixture that exercises it, or nothing will ever screenshot it.

**A fixture is a claim about the API, and must be checked against
`api/grid/v1/grid.proto` — not against what makes the page look populated.** This
is the harness's characteristic failure, and it has bitten twice. A field the
proto always populates but the fixture omits makes the code that reads it dead in
every screenshot, so it looks correct while never having run: `EventRevision.
ingested_at`, `Provenance.{sourceName,attribution,sourceUrl,fetchedAt}` and
`Source.attribution` were all absent, which meant the attribution obligation
(rule 7 above) was unverifiable in a screenshot of a page whose entire subject is
provenance.

### node_modules must live OFF the bind mount

`web/node_modules` is a **symlink** into `$(SITE_DEPS)` (default
`~/.cache/grid-web-deps`), created by the `site-modules` Make target that
`site`, `site-install`, `site-dev` and `site-shots-mock` all depend on. Do not
replace it with a real directory, and **do not run `npm ci`/`npm install` from
`web/`** — run `make site-install`.

**Why.** Files written into `/workspace` — a virtiofs bind mount — **bit-corrupt
in place**. `npm` writes ~188 MB, and a single flipped byte is enough to kill
the build. It happened three times in one session before the cause was found.

**The install has to happen off-mount too**, which is the part that is easy to
get wrong: `npm ci` **deletes and recreates** `node_modules`, so running it from
`web/` replaces the symlink with a real directory straight back on the
corrupting filesystem — and the next build fails again. `site-modules` therefore
keeps a copy of `package.json` + `package-lock.json` in `$(SITE_DEPS)`, runs
`npm ci` **there**, and only then links. npm never writes to the mount.

**What it looks like.** `astro build` dies with

```
WebAssembly.instantiate(): section was shorter than expected size (… expected, … decoded)
file: /workspace/web/src/pages/docs.astro:0:0
```

or `SyntaxError: The requested module './frontmatter.js' does not provide an
export named 'extractFrontmatter'`, or a bare `SyntaxError: Invalid or unexpected
token`. **The named file is a red herring** — it is whichever module was being
loaded when the corrupt bytes were hit, never the cause. Deleting pages one at a
time to bisect will never isolate it, because no page is responsible.

The corrupted file keeps its **exact byte length** and hashes **stably**, so
nothing looks truncated and re-reading gives identical bytes. Size checks and
repeat reads both say "fine". Only comparing against the registry reveals it:

```bash
V=$(node -p "require('./node_modules/@astrojs/compiler/package.json').version")
curl -sL "https://registry.npmjs.org/@astrojs/compiler/-/compiler-$V.tgz" | tar xz package/dist/astro.wasm
sha256sum package/dist/astro.wasm node_modules/@astrojs/compiler/dist/astro.wasm
```

Equal length + different hash ⇒ this bug. `rm -rf node_modules && npm ci` "fixes"
it only until the next corruption, and each round costs a full reinstall — which
is why the off-mount install is the actual fix. If you see the symptom, first
check `ls -ld web/node_modules`: if it is a real directory rather than a
symlink, something ran npm from `web/`; `make site-modules` restores it. Same fault class as the SQLite corruption
documented in `internal/store/CLAUDE.md`, which is why that file also insists the
dev database live outside `/workspace`.
