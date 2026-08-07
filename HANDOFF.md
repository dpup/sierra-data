# Handoff — `feat/web-broadsheet-redesign`

The "broadsheet" redesign of **data.sierragridteam.org**, plus one backend fix it
turned up. Nothing here is merged; the branch is a complete, verified unit of
work that you can pick up on another machine.

**Source of the design:** `docs/design/api-docs-site-redesign-handoff.zip`
(the original Claude Design bundle — README is the spec, the `.dc.html` files are
prototypes; `support.js` is their runtime and was deliberately **not** ported).
**The implementation plan and what a review of it found:**
`docs/site-broadsheet-redesign-plan.md`.
**The design system itself:** `web/CLAUDE.md` — read this before touching `web/`.

---

## Get running

```bash
git checkout feat/web-broadsheet-redesign
make site                 # builds web/ (Astro) → site/dist
make site-shots-mock      # 42 screenshots, mocked data, no server needed
go test ./...
make check-site           # fails if site/dist drifted from web/ source
```

**One surprise to know about first.** `make site` makes `web/node_modules` a
**symlink** into `$(SITE_DEPS)` (default `~/.cache/grid-web-deps`) and runs
`npm ci` *there*, not in `web/`. That is a workaround for the dev sandbox this
was built in: `/workspace` is a virtiofs bind mount and files written to it
**bit-corrupt in place** — same fault class as the SQLite corruption in
`internal/store/CLAUDE.md`. It cost hours before it was diagnosed, because the
corrupted file keeps its exact byte length and hashes *stably*, so every "is the
file OK" check says yes; only a hash against the npm registry reveals it. The
build then dies with `WebAssembly.instantiate(): section was shorter than
expected` naming an innocent `.astro` file.

On a normal filesystem the indirection is unnecessary but harmless. If you'd
rather not have it, `SITE_DEPS` is overridable, or drop the `site-modules`
prerequisite from the `site` / `site-install` / `site-dev` / `site-shots-mock`
targets and delete the symlink. **Do not** just run `npm ci` from `web/` while
the symlink exists — `npm ci` deletes and recreates `node_modules`, replacing the
link with a real directory. Full write-up in `web/CLAUDE.md`.

---

## What's in the branch

### Backend (independent of the redesign, safe to cherry-pick)

Store **migration v4** adds `idx_revisions_observed` on
`event_revisions(observed_at DESC, event_id ASC, revision DESC)`, matching
`QueryHistory`'s `ORDER BY` exactly.

`/api/v1/history` was measured against production at **6–40s** (`page_size=50`
took 18.7s, 39.0s, and once timed out at 40s). `event_revisions` had no index on
`observed_at`, so the cross-event query scanned the whole table and sorted it in
a temp B-tree on every call. The plan flips from
`SCAN … USE TEMP B-TREE FOR ORDER BY` to `SEARCH r USING INDEX
idx_revisions_observed`; `TestQueryHistoryUsesObservedIndex` pins both the index
use and the absence of the sort, since a regression here is invisible except as
latency. Migrations are an append-only ladder — never edit a shipped entry.

Also: `_astro/*` (content-hashed) now gets `immutable` caching, with
`TestSiteCacheControl` pinning the whole policy table.

### Site

All ten screens on the paper/ink system. Highlights:

- **Grid Info** — black deck, hero sentence, count ledger, top-of-feed, and the
  three numbered teaching sections.
- **Events** — filter bar per the design spec (labelled chip groups, live
  refetch, one echoed `GET`), list + full record. The detail pane is rendered by
  the **same** `renderEventDetail()` the `/event` permalink uses, so the two
  cannot drift.
- **Map** — the map element is *absent from the DOM* when nothing can honestly
  be drawn (see the contract below).
- **Docs** — collapsible endpoints built from `assets/spec.js`, with **RUN**
  buttons that fetch the example live, and the Event envelope shown beside a
  real record.

### Two things that are load-bearing, not cosmetic

**1. Fail-loud.** The API's contract is that absence is never an all-clear, and
the UI is bound by it: `null` ≠ `0`, a failed fetch never renders as a zero,
"calm" requires *every* input to be known, and an `UNAVAILABLE` map layer is
**removed from the page** rather than drawn empty (a rendered basemap with zero
features reads as "all clear"). The `map-suppressed` screenshot fixture exists
solely to catch a regression here. All seven rules are in `web/CLAUDE.md` §
*Fail-loud is a UI obligation*.

**2. Two severity ramps, one set of variable names.** `--sev-*` resolves by
context via the `.on-ink` scope; JS that paints canvas/WebGL can't inherit CSS
variables, so `format.js` exports `SEVERITY_COLORS` (paper) and
`SEVERITY_COLORS_ON_INK` (maps). Using the wrong one is the easy mistake.

### Measure and rhythm

Line length is a token in **`em`, never `ch`** — CSS `ch` is the width of `0`,
which is much narrower than the average glyph, so `max-width: 72ch` was
rendering ~94 characters per line. The docs page had no measure at all and ran
to **187 characters**. Now every page reads 50–83 characters at every width from
390 to 1920, and no page scrolls sideways. Mono needs its own token (~0.60em per
character vs ~0.51em proportional). Vertical rhythm is three tokens; before them
the docs sections were separated by 0, 12, 18, 24 and 26px — whatever the
children's margins happened to collapse into.

---

## Verification state

| Check | Status |
| --- | --- |
| `go test ./...` | passes |
| `make check-site` | `site/dist` in sync with `web/` |
| `make site-shots-mock` | 42 screenshots, no page errors, no unmocked API paths |
| `node web/screenshots/metrics.mjs` | no page over 88 chars/line; no horizontal scroll at 390px |

`web/screenshots/metrics.mjs` is new and worth knowing about: it reports
characters-per-line, section gaps and horizontal overflow at five widths.
Screenshots tell you something looks wrong; this tells you what and by how much.
Two numbers are regressions regardless of appearance: any page over ~85
characters, and any page where `docWidth > winW` at 390px.

A six-lens adversarial review ran over the finished diff — **47 findings raised,
38 survived independent refutation, all 38 fixed**. The three worst were
fail-loud defects that all sat in *seams* rather than logic (a sub-line printing
"0 severe · 0 moderate" while the hero above it said UNKNOWN; a layer toggle
that never re-ran the map's mount rule; a partial failure reported as a
"confirmed empty result"). §10 of the plan document has the full account,
including the fixture-fidelity problems — several fixtures did not match the
proto they stood in for, so screens were being "verified" against shapes the
server never emits.

---

## Open / not done

- **Nothing is merged and nothing is deployed.** `site/dist` is a committed
  build artifact and is in sync, so the branch is deployable as-is.
- **The `CHANGELOG.md` entry is written but dated 2026-08-06.** Re-date it if
  this lands later.
- **The place selector was removed** — the service covers one AREA, so a control
  offering one choice was furniture. `?place=` still works everywhere and the
  place-scoped screens keep their own in-page selector. `place.js` has a note at
  the foot on bringing a global one back when a second AREA is configured.
- **The sample payload on the front page can show `"summary": ""`.** That is
  real (the gateway uses `EmitUnpopulated` and the record isn't AI-enhanced).
  Design flagged it; I left the payload verbatim rather than swapping in a
  prettier record, because the pane prints the exact request directly above it
  and that honesty is the site's whole thesis. Worth addressing as
  *documentation* of why empty strings appear, not by changing the data.
- **The place `<select>` is still a native control.** Replacing it means a
  custom listbox with its own keyboard and a11y surface — deliberate work, not a
  bolt-on.
- **Live-data check still pending.** Everything above was verified against the
  mocked fixtures and against production *reads*; the redesigned pages have not
  been driven against a locally running server with live upstreams. `make run-bg`
  + the `verify` skill is the way in.
