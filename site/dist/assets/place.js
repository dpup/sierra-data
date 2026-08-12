// place.js — the active place, resolved once per page and shared by every screen.
//
// Nearly every screen is scoped to a place: the front-page deck, the map layers,
// roads, mesh, the place summary. The design prototype hardcoded a constant
// (`ebbetts-pass`); this resolves it instead, in a fixed order:
//
//   1. ?place= in the URL      — an explicit, shareable choice; always wins
//   2. sessionStorage          — the last place seen, so it survives a nav
//   3. the first kind=AREA     — the service's own primary coverage area
//
// Order matters: a link someone was sent must render what the sender saw, which
// is why the URL beats a stored preference rather than the other way round.
//
// Screens call `activePlace()` and use `.slug`. The place-scoped screens (Map,
// Roads, History, Places) carry their own in-page selector, which sets ?place=
// and reloads — so a screen never has to re-fetch itself mid-life, and every
// screen's URL is its own permalink.
//
// Pure resolution: no DOM access at all.

import { get } from './api.js';
import { layerLabel } from './format.js';

const STORAGE_KEY = 'grid:place';

/** @type {Promise<{places: Array, active: Object|null}>|null} */
let inflight = null;

/** The ?place= value on the current URL, or ''. */
export function placeParam() {
  try {
    return new URLSearchParams(location.search).get('place') || '';
  } catch {
    return '';
  }
}

function stored() {
  try {
    return sessionStorage.getItem(STORAGE_KEY) || '';
  } catch {
    return ''; // private mode / disabled storage — fall through to the AREA default
  }
}

function remember(slug) {
  try {
    sessionStorage.setItem(STORAGE_KEY, slug);
  } catch {
    /* non-fatal */
  }
}

/**
 * Resolve the active place, fetching the AREA directory once per page.
 * Returns `{places, active}`; `active` is null only when the directory fetch
 * failed AND no ?place= was given — callers must treat that as unknown, not as
 * "no places" (see the fail-loud contract: absence is never an all-clear).
 *
 * A ?place= that isn't in the AREA list is still honoured — corridors, towns and
 * counties are all valid `{place}` values, they just aren't in the switcher.
 *
 * @returns {Promise<{places: Array, active: {slug: string, name: string}|null}>}
 */
export function activePlace() {
  if (inflight) return inflight;
  inflight = (async () => {
    const wanted = placeParam() || stored();

    let places = [];
    try {
      const data = await get('/api/v1/places', { kind: 'AREA' });
      places = Array.isArray(data.places) ? data.places : [];
    } catch {
      // Directory unreachable. An explicit ?place= still works — it is just a
      // path segment — so only the *default* is lost.
      if (wanted) return { places: [], active: { slug: wanted, name: wanted } };
      return { places: [], active: null };
    }

    const match = wanted
      ? places.find((p) => p.slug === wanted || p.id === wanted)
      : null;

    let active = null;
    if (match) active = { slug: match.slug || match.id, name: match.name || match.slug };
    else if (wanted) active = { slug: wanted, name: wanted }; // valid non-AREA place
    else if (places.length) {
      const first = places[0];
      active = { slug: first.slug || first.id, name: first.name || first.slug };
    }

    if (active) remember(active.slug);
    return { places, active };
  })();
  return inflight;
}

/* ---------------------------------------------------------------------
 * The place picker
 *
 * Four screens let a reader change the place, and all four had written their
 * own <select> filler: sort by name, label as `Name (slug)`, keep a
 * URL-supplied place the directory has never heard of. They agreed on none of
 * it — one grouped by kind and one did not, one wrote `Name — slug`, and only
 * two preserved an unknown current value, so a deep link to a corridor
 * silently switched two of the screens to somewhere else.
 *
 * These are pure: they return option lists and strings, never touch the DOM,
 * and the caller hands the result to a <grid-menu>.
 * ------------------------------------------------------------------- */

/** Canonical PlaceKind order — broadest to most specific. Drives the picker's
 * group headings, the Places directory sort and its kind chips. Unknown kinds
 * sort after, in first-seen order. There were two copies of this list. */
export const KIND_ORDER = ['AREA', 'COUNTY', 'TOWN', 'EVAC_ZONE', 'CORRIDOR', 'SITE'];

/** A place's addressable value: its slug, falling back to its id. */
export function placeValue(p) {
  return (p && (p.slug || p.id)) || '';
}

/**
 * Group a directory by kind, in KIND_ORDER, each group sorted by name.
 * @param {Array} places protojson Place messages (camelCase)
 * @returns {Array<{kind: string, places: Array}>}
 */
export function groupPlaces(places) {
  const byKind = new Map();
  for (const p of places || []) {
    const kind = p.kind || 'PLACE_KIND_UNSPECIFIED';
    if (!byKind.has(kind)) byKind.set(kind, []);
    byKind.get(kind).push(p);
  }
  const kinds = [
    ...KIND_ORDER.filter((k) => byKind.has(k)),
    ...[...byKind.keys()].filter((k) => !KIND_ORDER.includes(k)),
  ];
  return kinds.map((kind) => ({ kind, places: byKind.get(kind).slice().sort(byName) }));
}

const byName = (a, b) =>
  String(a.name || placeValue(a)).localeCompare(String(b.name || placeValue(b)));

/**
 * Build the option list for a place picker.
 *
 * @param {Array} places      the directory, as returned by /api/v1/places
 * @param {object} [opts]
 * @param {string} [opts.anyLabel]  first entry with an empty value, e.g. 'All places'
 * @param {string} [opts.current]   the selected value, preserved even if unlisted
 * @param {boolean} [opts.group]    group by PlaceKind under headings
 * @returns {Array<{value?: string, label?: string, group?: string}>}
 */
export function placeMenuOptions(places, { anyLabel = '', current = '', group = false } = {}) {
  const opts = [];
  if (anyLabel) opts.push({ value: '', label: anyLabel });

  const list = (Array.isArray(places) ? places : []).filter((p) => placeValue(p));
  let listed = current === '';
  const push = (p) => {
    const value = placeValue(p);
    if (value === current) listed = true;
    opts.push({ value, label: p.name ? `${p.name} (${value})` : value });
  };

  if (group) {
    for (const { kind, places: members } of groupPlaces(list)) {
      opts.push({ group: layerLabel(kind) });
      members.forEach(push);
    }
  } else {
    list.slice().sort(byName).forEach(push);
  }

  // A ?place= the directory does not list is still a valid {place} — corridors,
  // towns and counties all are, and the AREA-only pickers list none of them.
  // Dropping it would quietly re-scope a shared link to somewhere else.
  if (current && !listed) opts.push({ value: current, label: current });
  return opts;
}

/**
 * What to print on the picker's trigger: the place's name, or the raw value
 * when the directory does not know it.
 * @param {Array} places
 * @param {string} value
 * @param {string} [fallback]  shown when nothing is selected
 */
export function placeMenuLabel(places, value, fallback = 'all places') {
  if (!value) return fallback;
  const hit = (Array.isArray(places) ? places : []).find(
    (p) => p.slug === value || p.id === value
  );
  return (hit && (hit.name || placeValue(hit))) || value;
}

// NOTE — there is no place switcher in the chrome. The service covers a single
// AREA today, so a global control offering one choice was furniture; it was
// removed rather than auto-hidden, because the screenshot fixtures deliberately
// define a second (quiet) area for the calm-state test and an auto-hiding
// control would still have rendered in every capture.
//
// Everything needed to bring one back lives above: activePlace() already
// returns the full `places` list alongside the active one. A switcher is a
// <select> over that list which sets ?place= and reloads — roughly 30 lines,
// and worth writing the day a second AREA is configured, not before.
