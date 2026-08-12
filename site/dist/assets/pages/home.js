// home.js — Grid Info, the front page.
//
// Two jobs on one screen: say what is happening in the region right now (the
// black deck), then teach the reader to fetch it themselves (the numbered
// sections on paper).
//
// Every figure in the deck is live. Nothing here has a placeholder value: a
// fabricated number in a hazard summary is precisely the failure the API's
// fail-loud contract exists to prevent, so an unknown renders as the word
// UNKNOWN and a failed fetch renders as a banner naming the request that failed.
//
// Requests:
//   /api/v1/places?kind=AREA                     (place.js, shared)
//   /api/v1/places/{place}/summary               mode, evacuations, source health
//   /api/v1/events?place={place}&page_size=200   severity counts + the feed
//   /api/v1/events?place={place}&page_size=1     the "01 Your first request" pane

import { get, ApiError, apiURL, curlFor } from '../api.js';
import { activePlace } from '../place.js';
import { CONVENTIONS } from '../spec.js';
import { recordRow, timeAgo, fmtNum } from '../format.js';
import { wireCopyButton } from '../ui.js';

const $ = (id) => document.getElementById(id);
const el = (t, c, x) => {
  const n = document.createElement(t);
  if (c) n.className = c;
  if (x !== undefined) n.textContent = x;
  return n;
};

/** A failed request, stated as a fact with the URL that produced it. */
function errBlock(err, what) {
  const d = el('div', 'error-block');
  d.append(el('strong', null, what ? `${what} — request failed. ` : 'Request failed. '));
  if (err instanceof ApiError) {
    const u = el('span', 'error-url', `GET ${err.url}`);
    d.append(u, ` → ${err.timedOut ? 'no response in 6s' : err.status || 'network error'}`);
  } else {
    d.append(String((err && err.message) || err));
  }
  return d;
}

/* ------------------------------------------------------------------ deck */

/**
 * Is the region calm? Calm is a POSITIVE ASSERTION, so it requires every input
 * to be known — any unknown means not calm. All seven conditions, per the
 * fail-loud contract:
 *   summary present, 0 EXTREME, 0 SEVERE, activeEvacuations non-null,
 *   evacuationStatus OK, no UNAVAILABLE source, and the event fetch succeeded.
 * Returning false on missing data is the safe direction and is deliberate.
 */
function isCalm(summary, counts, eventsOk, truncated) {
  if (!summary || !eventsOk) return false;
  // A partial page cannot support "nothing above MODERATE" — the next page is
  // exactly where the EXTREME event would be, since the server sorts by
  // severity descending. Unknown ⇒ not calm.
  if (truncated) return false;
  const s = summary.summary || {};
  if ((counts.EXTREME || 0) > 0 || (counts.SEVERE || 0) > 0) return false;
  if (s.activeEvacuations === null || s.activeEvacuations === undefined) return false;
  // evacuationStatus is the authoritative field (SummaryStats in grid.proto);
  // its absence is itself an unknown, so require it explicitly rather than
  // treating a missing value as OK.
  if (String(s.evacuationStatus || '').toUpperCase() !== 'OK') return false;
  const domains = Array.isArray(summary.domains) ? summary.domains : [];
  const evac = domains.find((d) => String(d.domain || d.name || '').toLowerCase() === 'evacuation');
  if (evac && String(evac.status || '').toUpperCase() !== 'OK') return false;
  const sources = Array.isArray(summary.sources) ? summary.sources : [];
  if (sources.some((x) => String(x.status || '').toUpperCase() === 'UNAVAILABLE')) return false;
  return true;
}

/** Count events by severity name. */
function severityCounts(events) {
  const counts = {};
  for (const e of events) {
    const s = String(e.severity || '').toUpperCase();
    if (s) counts[s] = (counts[s] || 0) + 1;
  }
  return counts;
}

function renderDeck(placeName, summary, events, eventsOk, query, truncated) {
  const counts = severityCounts(events);
  const calm = isCalm(summary, counts, eventsOk, truncated);
  const s = (summary && summary.summary) || {};
  const evac = s.activeEvacuations;
  const evacUnknown = evac === null || evac === undefined;

  // ---- dateline: place · MODE, the mode in the state accent
  const dl = $('deck-dateline');
  dl.textContent = '';
  dl.append(el('span', null, (placeName || 'unknown place').toUpperCase()));
  dl.append(el('span', 'pipe', '|'));
  const mode = summary ? String(summary.mode || 'UNKNOWN').toUpperCase() : 'UNKNOWN';
  dl.append(el('span', 'accent' + (calm ? ' calm' : ''), `MODE ${mode}`));

  // ---- hero: the sentence, with the two figures in the state accent
  const hero = $('deck-hero');
  hero.textContent = '';
  hero.className = 'hero' + (calm ? ' calm' : '');
  if (!eventsOk) {
    // A blocked fetch never becomes a zero. State the unknown at heading size.
    hero.append(
      el('span', 'figure', 'UNKNOWN'),
      ' — the event query did not answer, so the current state of ',
      placeName || 'this place',
      ' is unknown, not clear.'
    );
  } else {
    const total = events.length;
    hero.append(el('span', 'figure', (truncated ? 'at least ' : '') + fmtNum(total)));
    hero.append(` event${total === 1 && !truncated ? ' is' : 's are'} active in ${placeName || 'this place'} right now, `);
    const extreme = counts.EXTREME || 0;
    if (extreme > 0) {
      hero.append(el('span', 'figure', `${fmtNum(extreme)} of them EXTREME`), '.');
    } else if (calm) {
      hero.append(el('span', 'figure', 'nothing above MODERATE'), '.');
    } else {
      const severe = counts.SEVERE || 0;
      if (severe > 0) hero.append(el('span', 'figure', `${fmtNum(severe)} of them SEVERE`), '.');
      else hero.append(el('span', 'figure', 'severity of the region unconfirmed'), '.');
    }
  }

  // ---- subline: the counts, then the exact request that produced them
  const sub = $('deck-subline');
  sub.textContent = '';
  const row1 = el('div');
  row1.append(
    evacUnknown
      ? 'evacuation count unknown — not zero'
      : `${fmtNum(evac)} evacuation zone${Number(evac) === 1 ? '' : 's'}`
  );
  if (!eventsOk) {
    // The event query is what produces every figure below, so when it fails they
    // are all unknown. Printing "0 severe · 0 moderate" here would put three
    // reassuring zeros directly under a hero that says UNKNOWN — the precise
    // shape of "absence read as an all-clear" the contract forbids.
    row1.append(' · severity counts unknown — the event query did not answer');
  } else {
    row1.append(` · ${fmtNum(counts.SEVERE || 0)} severe · ${fmtNum(counts.MODERATE || 0)} moderate`);
    const dayAgo = Date.now() - 86400_000;
    const past24 = events.filter((e) => {
      const t = Date.parse(e.observedAt || '');
      return !Number.isNaN(t) && t >= dayAgo;
    }).length;
    row1.append(` · ${fmtNum(past24)} in the past 24h`);
  }
  const row2 = el('div', null, `${query}   ·   generatedAt ${(summary && summary.generatedAt) || '—'}`);
  sub.append(row1, row2);

  // ---- ledger: four fixed cells
  // The first three are one axis: events by severity. Labelled so.
  const cells = [
    ['Events · extreme', counts.EXTREME || 0],
    ['Events · severe', counts.SEVERE || 0],
    ['Events · moderate', counts.MODERATE || 0],
  ];
  const ledger = $('deck-ledger');
  ledger.textContent = '';
  for (const [label, value] of cells) {
    const cell = el('div', 'ledger-cell');
    const v = el('div', 'ledger-v' + (!eventsOk ? ' unknown' : value === 0 && calm ? ' zero' : ''),
      eventsOk ? fmtNum(value) : '?');
    cell.append(v, el('div', 'ledger-l', label));
    ledger.append(cell);
  }
  // Evacuations is the fail-loud cell: null is a word, never a 0 and never blank.
  // Marked as a SEPARATE AXIS. The first three cells count events by severity;
  // this one counts evacuation zones, which is a different kind of thing —
  // presenting all four as peers invited reading them as one series that sums.
  const evacCell = el('div', 'ledger-cell ledger-cell-alt');
  const evacV = el('div', 'ledger-v' + (evacUnknown ? ' unknown' : Number(evac) === 0 && calm ? ' zero' : ''),
    evacUnknown ? 'UNKNOWN' : fmtNum(evac));
  if (evacUnknown) evacV.style.fontSize = '18px';
  evacCell.append(evacV, el('div', 'ledger-l', evacUnknown ? 'Evacuation zones — not zero' : 'Evacuation zones'));
  ledger.append(evacCell);
}

/* ------------------------------------------------------------------ feed */

function renderFeed(events) {
  const lead = $('feed-lead');
  const rows = $('feed-rows');
  lead.textContent = '';
  rows.textContent = '';

  if (!events.length) {
    lead.append(
      el('p', 'sec-body',
        'No active events in this place right now. That is a confirmed empty result from ' +
        'a successful query — not a failed one; the request and its response are in the ' +
        'drawer at the foot of the page.')
    );
    return;
  }

  // Canonical client sort: severity desc, then observedAt desc.
  const rank = { EXTREME: 4, SEVERE: 3, MODERATE: 2, MINOR: 1, INFO: 0 };
  const sorted = [...events].sort(
    (a, b) =>
      (rank[String(b.severity).toUpperCase()] ?? -1) - (rank[String(a.severity).toUpperCase()] ?? -1) ||
      Date.parse(b.observedAt || 0) - Date.parse(a.observedAt || 0)
  );

  const [first, ...rest] = sorted;
  const box = el('div', 'lead-rec');
  // The lead record carries the same severity spine as every row beneath it.
  // Without it the most severe item on the page was the only unmarked one.
  const sev = String(first.severity || '').toUpperCase();
  box.classList.add('sev-' + (sev || 'UNKNOWN'));
  const link = el('a');
  link.href = `/event?id=${encodeURIComponent(first.id || '')}`;
  const head = el('div', 'lead-head', first.headline || '(no headline)');
  link.append(head);
  const meta = el('div', 'lead-meta');
  meta.textContent =
    `${first.id || '—'} · ${String(first.layer || '').toLowerCase()} · rev ${first.revision ?? '—'} · ` +
    `${(first.provenance && first.provenance.attribution) || 'source unattributed'} · ${timeAgo(first.observedAt)}`;
  box.append(link, meta);
  lead.append(box);

  for (const ev of rest.slice(0, 4)) {
    rows.append(recordRow(ev, { href: `/event?id=${encodeURIComponent(ev.id || '')}` }));
  }
}

/**
 * Replace very long JSON string values with a marked, measured elision.
 * Never silent: the reader is told exactly what was cut and how big it was.
 */
function elideLongStrings(json, limit = 120) {
  const long = new RegExp(`"([^"\\\\]{${limit},})"`, 'g');
  return json.replace(long, (_m, v) =>
    `"${v.slice(0, 24)}… ${fmtNum(v.length)} chars elided for display …"`);
}

/* -------------------------------------------------- 01 your first request */

async function renderFirstRequest(place) {
  const path = apiURL('/api/v1/events', { place, page_size: 1 });
  $('first-req-path').textContent = path;
  wireCopyButton($('first-req-copy'), () => curlFor(path));

  const t0 = Date.now();
  const body = $('first-req-body');
  const foot = $('first-req-foot');
  try {
    const data = await get('/api/v1/events', { place, page_size: 1 });
    const raw = JSON.stringify(data, null, 2);
    // geometry.geojson is base64 bytes — often several KB on one line, which
    // pushed the pane into a long sideways scroll with no sign it had been cut.
    // Elide it VISIBLY and say how much was removed; the byte count in the
    // footer still reports the real response size, and the copy-curl button
    // reproduces the untouched request.
    const text = elideLongStrings(raw);
    body.textContent = text;
    foot.textContent = '';
    foot.append(
      el('span', 'ok', '200 OK'),
      el('span', null, `${Date.now() - t0} ms`),
      el('span', null, `${fmtNum(new Blob([text]).size)} bytes`),
      el('span', null, 'application/json')
    );
  } catch (err) {
    body.textContent =
      err instanceof ApiError && err.timedOut
        ? 'no response within 6000 ms — request abandoned'
        : String((err && err.message) || err);
    foot.textContent = '';
    foot.append(
      el('span', 'bad', err instanceof ApiError ? String(err.status || 'network error') : 'error'),
      el('span', null, `${Date.now() - t0} ms`)
    );
  }
}

/* ----------------------------------------------------- 03 conventions */

function renderConventions() {
  const dl = $('conventions');
  for (const [term, def] of CONVENTIONS) {
    const row = el('div', 'def-row');
    row.append(el('dt', null, term), el('dd', null, def));
    dl.append(row);
  }
}

/* ----------------------------------------------------------------- boot */

async function main() {
  renderConventions();

  const { active } = await activePlace();
  if (!active) {
    // No place and no directory: say so loudly rather than rendering an empty
    // deck that would read as "nothing is happening".
    $('deck-dateline').textContent = 'PLACE DIRECTORY UNREACHABLE';
    $('deck-hero').textContent =
      'The place directory did not answer, so there is nothing to report on — this is an unknown state, not a clear one.';
    $('feed-lead').textContent = '';
    $('deck-error').append(errBlock(new Error('GET /api/v1/places?kind=AREA'), 'Place directory'));
    return;
  }

  const place = active.slug;
  const eventsQuery = apiURL('/api/v1/events', { place, page_size: 200 });

  const [summaryRes, eventsRes] = await Promise.allSettled([
    get(`/api/v1/places/${encodeURIComponent(place)}/summary`),
    get('/api/v1/events', { place, page_size: 200 }),
  ]);

  const summary = summaryRes.status === 'fulfilled' ? summaryRes.value : null;
  const eventsOk = eventsRes.status === 'fulfilled';
  const events = eventsOk && Array.isArray(eventsRes.value.events) ? eventsRes.value.events : [];
  // A nextPageToken means the server had MORE than this page. Counting the page
  // and calling it the region's total would understate the situation, so say
  // "at least N" rather than quietly truncating.
  const truncated = eventsOk && Boolean(eventsRes.value.nextPageToken);

  renderDeck(active.name, summary, events, eventsOk, eventsQuery, truncated);

  const errs = $('deck-error');
  if (summaryRes.status === 'rejected') errs.append(errBlock(summaryRes.reason, 'Place summary'));
  if (!eventsOk) errs.append(errBlock(eventsRes.reason, 'Event query'));

  if (eventsOk) renderFeed(events);
  else {
    $('feed-lead').textContent = '';
    $('feed-lead').append(
      el('p', 'sec-body', 'The feed is unavailable because the event query above failed. This is not an empty region.')
    );
  }

  renderFirstRequest(place);
}

main();
