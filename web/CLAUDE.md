# The site (`web/` → `site/dist`) — the broadsheet design system

Astro 5 SSG. Source in `web/`, built into `../site/dist`, which is a **committed
build artifact** embedded by `site/embed.go`. Run `make site` after editing
anything here and commit the result — `make check-site` (a `docker-build`
prerequisite) fails the deploy if `site/dist` is stale.

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

- **Paper** (`--paper #f4f1ea`) — prose, reference, tables, navigation of ideas.
- **Ink** (`--ink #14161a`) — anything that *is* the API: the front-page deck,
  response panes, code blocks, the sidebar, map frames.

Radii are **0** everywhere (rounded corners break the broadsheet read) and there
are **no drop shadows**. `box-shadow` is a hairline device only: `inset Npx 0 0`
left-edge markers, and the `0 0 0 100vmax rgba(0,0,0,.5)` mobile drawer scrim.

## Two severity ramps, one set of variable names

This is the subtlety that bites. The same component — a severity chip, a status
dot, a record row's spine — appears on paper *and* inside black panes, and one
ramp cannot be legible on both. So `--sev-*` resolves **by context**:

- the paper ramp is the `:root` default (`EXTREME #b3261e … INFO #1d4ed8`);
- the **`.on-ink` scope** re-points the same variables at the dark-adapted ramp
  (`#ff5544 … #3d8bff`). It is applied to `.deck`, `.resp-pane`, `.code`,
  `.inspector`, `.primer`, `.sidebar`, `.topbar`, `.qbar`, `.env-bar`, `.site-footer`.

**Never add a second set of class names for this.** To put a component on a black
surface, put it inside something carrying `.on-ink`.

JS that paints canvas/WebGL can't inherit CSS variables, so `format.js` exports
both ramps explicitly: `SEVERITY_COLORS` (paper) and `SEVERITY_COLORS_ON_INK`
(maps, over the dark basemap). Picking the wrong one is the common mistake — map
geometry and event-detail geometry both use the **ink** ramp.

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

## Shared primitives (app.css §14)

Used by five or more screens — reach for these before writing page CSS:

- **`.deck`** — the full-bleed black band: `.dateline`, `.hero` (with `.figure`
  spans in the state accent), `.subline`, `.deck-lede`, `.ledger`.
- **`.ledger`** — the count strip. **Four fixed tracks.** Never `auto-fit`: it
  wraps to 3 + 1 and paints the empty fourth slot as a dark void. The 1px grid
  gap over an `--ink-rule` background is what paints the hairlines.
- **`.rec`** — the record row (Events, Roads, History, the front feed). Built by
  `format.js`'s `recordRow()`: 6px severity spine, mono meta line, display-face
  headline, mono id.
- **`.loud-banner`** / **`.error-block`** — the fail-loud surfaces.
- **`.resp-pane`** — a live request and its actual response, in black.
- **`.numbered-head`**, **`.def-row`**, **`.env-row`**.

## Legacy token aliases

`:root` still defines the pre-broadsheet dark palette's names (`--bg-card`,
`--text-hi`, `--grn`, `--blu`, …) re-pointed at the paper system. That is what let
the retheme repaint ten screens at once instead of requiring a simultaneous
rewrite. They are being retired screen by screen — **do not use them in new
rules**, use the tokens above.

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
make site                    # build (required before commit — dist is committed)
make site-shots-mock         # all pages × mobile/tablet/desktop, mocked data
make site-shots-mock PAGES=map,home ONLY=desktop
make check-site              # fails if site/dist is stale

# Layout metrics — characters per line, section gaps, horizontal overflow,
# at 390/768/1280/1440/1920. Screenshots show that something looks wrong;
# this says what and by how much, so a spacing change is a diff of numbers.
node screenshots/metrics.mjs
node screenshots/metrics.mjs --pages docs --widths 1440
```

Two numbers are regressions whatever they look like: any page whose widest line
exceeds ~85 characters, and any page whose `docWidth > winW` at 390px (a phone
scrolling sideways).

`screenshots/fixtures.mjs` answers every `/api/v1/*` fetch deterministically, so
runs diff cleanly by eye. It deliberately includes degraded states (an
UNAVAILABLE layer, a STALE source) — when you add a fail-loud path, add the
fixture that exercises it, or nothing will ever screenshot it.

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
