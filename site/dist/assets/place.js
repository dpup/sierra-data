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
