// pages/event-detail.js — /event?id=... event detail (site spec §2
// /events/{id}).
//
// Two requests: GET /api/v1/events/{id} (current revision) and
// GET /api/v1/events/{id}/history (revisions, newest first). Renders the full
// envelope and the typed detail block as .kv definition lists, geometry on
// a small MapLibre map (textual bbox/centroid fallback), the provenance
// block, the AI-enhancement badge with the verbatim original text, and the
// revision timeline with a client-side field-level diff (diff.js) between
// consecutive revisions. Every section carries a raw-protojson toggle.
//
// Pure helpers (detail extraction, bounds, diff-value formatting, revision
// sorting) have no DOM access at import time — node can import and test
// this module. All DOM work happens inside initEventPage(). MapLibre is
// loaded by event.html as a classic script (window.maplibregl); this module
// only touches it inside functions and degrades to text when it is absent.

import { get, apiURL, curlFor, ApiError } from '../api.js';
import {
  timeAgo,
  timeCell,
  sevChip,
  layerLabel,
  decodeGeometry,
  SEVERITY_COLORS,
  fmtNum,
  ABSENT,
  absentValue,
} from '../format.js';
import { copyOnClick, copyButton } from '../ui.js';
import { FIELD_DOCS } from '../spec.js';
import { diffObjects } from '../diff.js';
import { BASE_STYLE, BASE_ATTRIBUTION_OPTS, ensureBasemap, deferInteraction } from '../basemap.js';

/**
 * The Event.detail oneof's protojson field names (grid.proto fields 20–30).
 * Exactly one may be present on an event; protojson uses the lowerCamelCase
 * proto field name as the JSON key.
 */
export const DETAIL_FIELDS = [
  'wildfire',
  'evacuation',
  'weatherAlert',
  'fireWeather',
  'earthquake',
  'roadIncident',
  'power',
  'gauge',
  'airQuality',
  'mesh',
  'announcement',
];

/**
 * Envelope fields rendered in the definition list, in proto order. Types:
 * how the value renders (text / time / link / list / chip).
 */
export const ENVELOPE_FIELDS = [
  ['id', 'text'],
  ['layer', 'text'],
  ['category', 'text'],
  ['severity', 'severity'],
  ['status', 'text'],
  ['headline', 'text'],
  ['summary', 'text'],
  ['description', 'text'],
  ['areaLabel', 'text'],
  ['canonicalUrl', 'link'],
  ['placeIds', 'list'],
  ['effective', 'time'],
  ['expires', 'time'],
  ['observedAt', 'time'],
  ['ingestedAt', 'time'],
  ['revision', 'number'],
];

/**
 * Find the populated detail oneof field on an event.
 * @param {Object} event protojson Event (camelCase)
 * @returns {{field: string, value: Object}|null}
 */
export function detailOf(event) {
  if (!event || typeof event !== 'object') return null;
  for (const field of DETAIL_FIELDS) {
    const value = event[field];
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      return { field, value };
    }
  }
  return null;
}

/** Recursively visit every [lng, lat] position in a GeoJSON geometry. */
function walkPositions(node, cb) {
  if (!node) return;
  if (Array.isArray(node)) {
    if (node.length >= 2 && typeof node[0] === 'number' && typeof node[1] === 'number') {
      cb(node);
      return;
    }
    for (const child of node) walkPositions(child, cb);
    return;
  }
  if (typeof node === 'object') {
    if (Array.isArray(node.geometries)) {
      for (const g of node.geometries) walkPositions(g, cb);
    }
    if (node.coordinates !== undefined) walkPositions(node.coordinates, cb);
  }
}

/**
 * Bounds for an event geometry: prefers the proto bbox (always populated at
 * ingest per the spec), falls back to walking the decoded GeoJSON's
 * coordinates. Note protojson omits zero-valued doubles, so absent bbox
 * corners read as 0; an all-zero bbox is treated as unpopulated.
 * @param {Object|undefined} geometry protojson Geometry ({geojson, bbox, centroid})
 * @param {Object|null} decoded decoded GeoJSON geometry object (or null)
 * @returns {{minLat:number,minLng:number,maxLat:number,maxLng:number}|null}
 */
export function geometryBounds(geometry, decoded) {
  const bbox = geometry && geometry.bbox;
  if (bbox && typeof bbox === 'object') {
    const b = {
      minLat: Number(bbox.minLat ?? 0),
      minLng: Number(bbox.minLng ?? 0),
      maxLat: Number(bbox.maxLat ?? 0),
      maxLng: Number(bbox.maxLng ?? 0),
    };
    const values = [b.minLat, b.minLng, b.maxLat, b.maxLng];
    if (values.every(Number.isFinite) && values.some((v) => v !== 0)) return b;
  }
  if (decoded) {
    let minLat = Infinity;
    let minLng = Infinity;
    let maxLat = -Infinity;
    let maxLng = -Infinity;
    walkPositions(decoded, ([lng, lat]) => {
      if (lat < minLat) minLat = lat;
      if (lat > maxLat) maxLat = lat;
      if (lng < minLng) minLng = lng;
      if (lng > maxLng) maxLng = lng;
    });
    if (Number.isFinite(minLat) && Number.isFinite(minLng)) {
      return { minLat, minLng, maxLat, maxLng };
    }
  }
  return null;
}

/**
 * Compact single-line rendering of a diff value for the timeline table.
 * JSON-encoded (so "" and absent are distinguishable) and truncated —
 * base64 geometry blobs stay one opaque token; the raw toggle has the rest.
 * @param {*} v
 * @param {number=} max maximum characters (default 120)
 * @returns {string} '' for undefined (absent side of added/removed)
 */
export function fmtDiffValue(v, max = 120) {
  if (v === undefined) return '';
  let s;
  // A string renders as ITSELF, not as its JSON encoding. A changed headline is
  // the most common diff on this page and it was showing as
  // `"Mudflat Fire — 2,340 acres, 0% contained"` — the quotes are noise on the
  // one row a reader most wants to skim, and they cost two of the characters
  // before truncation. Objects and arrays keep JSON, which is what they are.
  if (typeof v === 'string') {
    s = v;
  } else {
    try {
      s = JSON.stringify(v);
    } catch {
      s = String(v);
    }
  }
  if (typeof s !== 'string') s = String(s);
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

/**
 * Full (untruncated) rendering of a diff value, matching fmtDiffValue's encoding.
 * @param {*} v
 * @returns {string} '' for undefined
 */
export function fmtDiffValueFull(v) {
  return fmtDiffValue(v, Infinity);
}

/**
 * A diff value as text: truncated for the row, full in the title.
 *
 * This replaced a `<td>` builder with click-to-expand. The values in a diff are
 * a headline, a timestamp, and a serialized detail object — lengths that have
 * nothing in common — and a table column sized for one made the others useless.
 * They now flow inline and wrap, with the untruncated value one hover away.
 *
 * @param {*} v
 * @returns {string}
 */
function diffText(v) {
  return fmtDiffValue(v);
}

/**
 * Sort EventRevision messages newest-first by revision number (the API
 * already returns descending; this makes the ordering a client guarantee).
 * @param {Array<Object>} revisions
 * @returns {Array<Object>} new array
 */
export function sortRevisionsDesc(revisions) {
  return (revisions || []).slice().sort((a, b) => (b.revision ?? 0) - (a.revision ?? 0));
}

/* ------------------------------------------------------------------ */
/* DOM code below — only runs when initEventPage() is called.          */
/* ------------------------------------------------------------------ */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function errorBlock(err, context) {
  const div = el('div', 'error-block');
  if (context) div.append(el('div', '', context));
  div.append(
    el(
      'div',
      '',
      err instanceof ApiError
        ? `Request failed (HTTP ${err.status || 'network error'}):`
        : 'Request failed:'
    ),
    el(
      'div',
      'error-url',
      err instanceof ApiError ? `GET ${err.url}` : String(err.message || err)
    )
  );
  if (err instanceof ApiError && err.body && typeof err.body === 'object' && err.body.message) {
    div.append(el('div', 'muted', err.body.message));
  }
  return div;
}

/**
 * The exact request behind this pane, in the site's one echo treatment.
 *
 * This was a seventh variant (`.query-row`): 12.5px, `white-space: nowrap` and
 * `overflow-x: auto`, so at 390px the URL and its copy button ran 230px past
 * the column and the reader had to find a horizontal scrollbar to see the end
 * of the thing they were being invited to copy. `.req-echo` wraps.
 */
function requestLine(url) {
  const row = el('div', 'req-echo');
  row.append(
    el('span', 'method', 'GET'),
    el('span', '', url),
    copyButton(curlFor(url), 'copy curl')
  );
  return row;
}

/**
 * A section of the record: a small all-caps heading over a thick rule, then the
 * rows. Every section gets the same treatment — Envelope, Detail, Geometry,
 * Provenance are peers, and all of them are open.
 *
 * Two earlier attempts were wrong in opposite directions. Bordered cards of
 * equal weight made the pane a stack of boxes with no reading order; collapsing
 * the secondary ones behind disclosures hid content the reader came for and
 * made the pane's shape depend on what they had clicked. A shared heading and a
 * rule give the order without hiding anything.
 *
 * The per-section `raw` disclosures are still gone: there was one per card, each
 * showing a slice of the same object, so "show me the JSON" had six answers.
 * There is one now, at the foot of the pane.
 *
 * @param {string} title
 * @returns {{panel: HTMLElement, body: HTMLElement}}
 */
function section(title) {
  const panel = el('section', 'ed-section');
  panel.append(el('h2', '', title));
  const body = el('div', 'sec-body');
  panel.append(body);
  return { panel, body };
}

/**
 * Which KIND of absence is this?
 *
 * The distinction is the whole reason the fail-loud contract exists, so it has
 * to be visible in the envelope rather than flattened into one dash:
 *
 *  - a FAULT — the value must exist and does not. `ingestedAt` is stamped by
 *    the store on every write, so its absence is a bug in us; `observedAt`
 *    missing on an ACTIVE life-safety record means we cannot say when it was
 *    true, which a reader has to be told loudly.
 *  - NOT APPLICABLE — the field belongs to a different layer.
 *  - NOT PROVIDED — the source simply did not send it this time.
 *
 * @param {string} key envelope field name
 * @param {Object} ev  the event, for layer/status context
 * @returns {{text:string, cls:string, sourceUrl?:string}}
 */
function absenceFor(key, ev) {
  const sourceUrl = (ev.provenance && ev.provenance.sourceUrl) || '';
  if (key === 'ingestedAt') return ABSENT.missing(sourceUrl);
  if (key === 'observedAt') {
    const active = String(ev.status || '').toUpperCase() === 'ACTIVE';
    return active ? ABSENT.missing(sourceUrl) : ABSENT.notProvided();
  }
  // `expires`/`effective` are meaningful only for the layers that schedule.
  if (key === 'expires' || key === 'effective') {
    const layer = String(ev.layer || '').toLowerCase();
    if (layer && !['weather_alert', 'fire_weather', 'evacuation', 'road_incident'].includes(layer)) {
      return ABSENT.notApplicable(layer);
    }
  }
  return ABSENT.notProvided();
}

/** Pretty-print a JSON string if it parses; otherwise return it unchanged. */
function prettyJSON(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

/**
 * Append one dt/dd row to a .kv list; value may be a node or string.
 *
 * `doc` is the field's spec sentence from assets/spec.js, printed under the
 * value. This is the site's whole thesis applied to one record: the reference
 * for a field and an actual value of that field, in the same place, so you are
 * never reading the docs in one tab and guessing at the shape in another.
 */
function kvRow(dl, key, value, doc) {
  dl.append(el('dt', '', key));
  const dd = document.createElement('dd');
  if (value instanceof Node) dd.append(value);
  else dd.textContent = value; // textContent: upstream text is untrusted
  // FIELD_DOCS entries are [type, sentence]; the sentence is what belongs here.
  if (doc && doc[1]) dd.append(el('div', 'kv-doc', doc[1]));
  dl.append(dd);
  return dd;
}

/**
 * Render an arbitrary detail-field value (primitive / array / object).
 *
 * An empty value is NAMED, never left blank and never a dash. protojson omits
 * empty strings, so a detail field the source did not fill arrived here as
 * `undefined` and rendered as nothing at all — `eventType` on an evacuation
 * record was a blank row, which reads as a rendering bug rather than as the
 * fact it is. The envelope has said this properly for a while; the typed detail
 * block was still using a dash, and then not even that.
 */
function valueNode(v) {
  if (v === null || v === undefined || v === '') return absentValue(ABSENT.notProvided());
  // Thousands separators on magnitudes. On a service whose whole claim is that
  // it normalizes upstream feeds, "10374" reads as a value we never parsed.
  // Small integers (containment %, depth, magnitude, counts under 1000) are
  // left alone — a separator there would be noise.
  if (typeof v === 'number' && Number.isFinite(v) && Math.abs(v) >= 1000) {
    return fmtNum(v, 2);
  }
  if (Array.isArray(v)) {
    if (v.length === 0) return absentValue(ABSENT.notProvided());
    if (v.every((x) => typeof x !== 'object' || x === null)) return v.map(String).join(', ');
    const pre = el('pre', 'code');
    pre.textContent = JSON.stringify(v, null, 2);
    return pre;
  }
  if (typeof v === 'object') {
    const pre = el('pre', 'code');
    pre.textContent = JSON.stringify(v, null, 2);
    return pre;
  }
  return String(v);
}

function fmtCoord(n) {
  return Number.isFinite(Number(n)) ? Number(n).toFixed(5) : '—';
}

/**
 * Render the geometry on a small MapLibre map, styled by severity color.
 * Returns false when MapLibre is unavailable or fails to start — the
 * caller keeps the textual bbox/centroid rendering either way.
 */
function renderEventMap(container, decoded, bounds, severity) {
  const lib = typeof window !== 'undefined' ? window.maplibregl : undefined;
  if (!lib || typeof lib.Map !== 'function') return false;
  // On the dark basemap — the ink ramp, not the paper one.
  // PAPER ramp: the basemap is light (see basemap.js).
  const color = SEVERITY_COLORS[String(severity || '').toUpperCase()] || SEVERITY_COLORS.INFO;
  try {
    const center = bounds
      ? [(bounds.minLng + bounds.maxLng) / 2, (bounds.minLat + bounds.maxLat) / 2]
      : [-120.35, 38.2];
    // Shared light OSM vector basemap (basemap.js) for geographic context under the
    // event geometry; the bbox/centroid text above the map is the fallback when
    // the map library fails to load. API data stays same-origin /api/v1/* via api.js.
    const map = new lib.Map({
      container,
      style: BASE_STYLE,
      center,
      zoom: 9,
      // Credit comes from the TileJSON; this only makes it compact.
      attributionControl: BASE_ATTRIBUTION_OPTS,
    });
    ensureBasemap(map);
    deferInteraction(map, container);
    map.addControl(new lib.NavigationControl(), 'top-right');
    // Insurance against a container that gains its final size just after init:
    // resize once the style is ready so the canvas fills the 24rem box.
    map.on('style.load', () => {
      map.resize();
      map.addSource('event-geom', {
        type: 'geojson',
        data: { type: 'Feature', geometry: decoded, properties: {} },
      });
      map.addLayer({
        id: 'event-fill',
        type: 'fill',
        source: 'event-geom',
        filter: ['==', '$type', 'Polygon'],
        paint: { 'fill-color': color, 'fill-opacity': 0.25 },
      });
      map.addLayer({
        id: 'event-line',
        type: 'line',
        source: 'event-geom',
        filter: ['!=', '$type', 'Point'],
        paint: { 'line-color': color, 'line-width': 2 },
      });
      map.addLayer({
        id: 'event-point',
        type: 'circle',
        source: 'event-geom',
        filter: ['==', '$type', 'Point'],
        paint: {
          'circle-color': color,
          'circle-radius': 7,
          'circle-stroke-color': '#ffffff',
          'circle-stroke-width': 1.5,
        },
      });
    });
    if (bounds) {
      map.fitBounds(
        [
          [bounds.minLng, bounds.minLat],
          [bounds.maxLng, bounds.maxLat],
        ],
        { padding: 48, maxZoom: 13, duration: 0 }
      );
    }
    return true;
  } catch {
    return false;
  }
}

/**
 * Wire up the event detail page. Expects the element ids laid out in
 * event.html (ed-loading, ed-errors, ed-head, ed-chips, ed-headline,
 * ed-sub, ed-query, ed-sections).
 */
/**
 * Render one event's full record into `root`: badge row, headline, envelope,
 * typed detail, geometry (with a map when the geometry decodes), the revision
 * timeline with client-side field diffs, the requests that produced the pane,
 * and the raw protojson.
 *
 * This is THE event detail renderer. Two callers share it:
 *   - /event?id=…  — the standalone permalink page (initEventPage below)
 *   - /events      — the desktop right-hand column of the browser
 * Both must show the same record identically; duplicating the envelope and diff
 * logic across two screens is how they drift apart.
 *
 * The skeleton is built here rather than read from page markup, so the caller
 * only supplies an empty container. `setTitle` is opt-out because the browser
 * selecting a record should not retitle the whole Events page.
 *
 * @param {HTMLElement} root       container; its contents are replaced
 * @param {string} id              event id
 * @param {{setTitle?: boolean, headingLevel?: number}=} opts
 * @returns {Promise<void>} resolves once the pane has rendered (or failed loud)
 */
export async function renderEventDetail(root, id, opts = {}) {
  const setTitle = opts.setTitle !== false;
  // The permalink page's record IS the page, so it gets the <h1>. Inside the
  // Events browser the page already has one, and a second would outrank the
  // page's own heading in the document outline — so the caller drops it to h2.
  const headingTag = `h${opts.headingLevel || 1}`;
  root.textContent = '';

  const errorsEl = el('div', 'ed-errors');
  const loadingEl = el('div', 'loading', `Loading GET /api/v1/events/${id} …`);
  const headEl = el('header', 'ed-head-block');
  // When this pane is EMBEDDED in the /events browser it has no address of its
  // own, so the record it is showing was unreachable as a link — you could see
  // it but not send it to anyone. This is the record's own URL.
  if (!setTitle && id) {
    const perma = el('a', 'ed-permalink', 'open permalink \u2197');
    perma.href = `/event?id=${encodeURIComponent(id)}`;
    headEl.append(perma);
  }
  headEl.hidden = true;
  const chipsEl = el('div', 'chip-row');
  const headlineEl = el(headingTag, 'ed-headline');
  const subEl = el('div', 'muted mono small');
  headEl.append(chipsEl, headlineEl, subEl);
  // The two GET lines sit at the FOOT of the pane, not between the headline and
  // the record. They say how this was fetched — a footnote to the record, not a
  // preamble to it, and putting them first pushed the geometry below the fold.
  const queryEl = el('div', 'ed-requests');
  queryEl.setAttribute('aria-label', 'Requests behind this pane');
  const sectionsEl = el('div', 'ed-sections');
  root.append(errorsEl, loadingEl, headEl, sectionsEl, queryEl);

  if (!id) {
    loadingEl.remove();
    const block = el('div', 'error-block');
    block.append(el('div', '', 'Missing ?id= — no event selected.'));
    const back = el('div', '');
    const a = el('a', '', 'Open the /events explorer and pick an event.');
    a.href = '/events';
    back.append(a);
    block.append(back);
    errorsEl.append(block);
    return;
  }

  const eventPath = `/api/v1/events/${encodeURIComponent(id)}`;
  const historyPath = `/api/v1/events/${encodeURIComponent(id)}/history`;
  // Opt into the model I/O (enhancement.request/response) — the detail page
  // shows it; list endpoints omit it by default to stay lean.
  //
  // DO NOT strip this from the echoed requests to tidy them. It is a real API
  // parameter — `GetEventRequest.enhancement_io` (field 2) and
  // `GetEventHistoryRequest` (field 4), not a proxy artifact — and without it
  // the server omits `enhancement.request/response`, which is exactly what the
  // AI-enhancement card below renders. A copyable request that returns a
  // different record than the pane shows breaks the one promise this site
  // makes: the printed request is the request that ran.
  const ioParams = { enhancement_io: 'true' };

  queryEl.append(
    requestLine(apiURL(eventPath, ioParams)),
    requestLine(apiURL(historyPath, ioParams))
  );

  await load();

  async function load() {
    const [evRes, histRes] = await Promise.allSettled([
      get(eventPath, ioParams),
      get(historyPath, ioParams),
    ]);
    loadingEl.remove();

    let event = evRes.status === 'fulfilled' ? evRes.value : null;
    const hist = histRes.status === 'fulfilled' ? histRes.value : null;
    let revisions = sortRevisionsDesc(hist && Array.isArray(hist.revisions) ? hist.revisions : []);
    let histNext = (hist && hist.nextPageToken) || '';

    if (evRes.status === 'rejected') {
      errorsEl.append(errorBlock(evRes.reason, 'Could not load the current revision.'));
      if (revisions.length > 0 && revisions[0].event) {
        // Fall back to the newest revision from history so the page still
        // renders something honest rather than nothing.
        event = revisions[0].event;
        errorsEl.append(
          el(
            'div',
            'notice',
            'Showing the newest revision from /api/v1/history instead of the /api/v1/events/{id} response.'
          )
        );
      }
    }

    if (!event && revisions.length === 0) {
      if (histRes.status === 'rejected') {
        errorsEl.append(errorBlock(histRes.reason, 'Could not load the revision history.'));
      }
      return; // both error blocks are on screen; nothing renderable
    }

    if (event) renderEvent(event);
    renderTimeline(event);
    // ONE raw view, at the foot, for the whole record. There used to be one per
    // card, each showing a slice of the same object, so "show me the JSON" had
    // six different answers depending on which disclosure you happened to open.
    if (event) {
      const raw = el('details', 'ed-section ed-raw');
      raw.append(el('summary', '', 'Raw protojson'));
      const rawBody = el('div', 'sec-body');
      const pre = el('pre', 'code');
      pre.textContent = JSON.stringify(event, null, 2);
      rawBody.append(pre);
      raw.append(rawBody);
      sectionsEl.append(raw);
    }

    /* ---- header + sections ---- */

    function renderEvent(ev) {
      headEl.hidden = false;
      if (setTitle) document.title = `${ev.headline || ev.id || id} · Event · The Grid`;

      chipsEl.textContent = '';
      chipsEl.append(sevChip(ev.severity || 'INFO'));
      chipsEl.append(el('span', 'meta-chip mono', ev.status || 'EVENT_STATUS_UNSPECIFIED'));
      chipsEl.append(el('span', 'meta-chip mono', layerLabel(ev.layer || '')));
      // No AI-ENHANCED chip in the badge row. Enhancement is not a property of
      // the event on a par with its severity or status, and putting it there
      // implied the whole record was generated. The AI ENHANCEMENT section
      // below states it precisely — which fields, which model, and the verbatim
      // original beside them.

      // The headline, as the row shows it — see the note on composeTitle's
      // removal in format.js.
      headlineEl.textContent = ev.headline || ev.id || '(no headline)';
      // The id leads the sub-line and is the long part: clip it and let a click
      // hand over the whole thing, rather than wrapping a 70-character
      // `meshcore:…` across two lines under the headline.
      subEl.textContent = '';
      if (ev.id) {
        const idEl = el('span', 'id-clip ed-subid', ev.id);
        copyOnClick(idEl, ev.id, 'id');
        subEl.append(idEl);
      }
      const rest = [ev.category, ev.areaLabel].filter(Boolean).join(' · ');
      if (rest) subEl.append(document.createTextNode(`${ev.id ? ' · ' : ''}${rest}`));

      // GEOMETRY FIRST. Where a thing is, is the first question a reader has
      // about a hazard record, and the mock puts the map immediately under the
      // headline. An envelope table above it buried the one part of the record
      // that is not text.
      // Geometry — map when possible, textual bbox/centroid always.
      // Decode first: the summary line names the geometry type, so a reader can
      // tell a Polygon from a null without opening the section.
      const geomDecodedForSummary = ev.geometry ? decodeGeometry(ev.geometry.geojson) : null;
      const geomType = geomDecodedForSummary ? geomDecodedForSummary.type || 'untyped' : '';
      const geomSec = section(geomType ? `Geometry — ${geomType}` : 'Geometry — null (valid)');
      if (!ev.geometry) {
        geomSec.body.append(
          el('p', 'muted small', 'No geometry on this event (county-wide advisories often carry none).')
        );
      } else {
        const decoded = decodeGeometry(ev.geometry.geojson);
        const bounds = geometryBounds(ev.geometry, decoded);
        const c = ev.geometry.centroid;
        // One muted line under the map, not a three-row table: these are
        // captions for the picture above them, and a kv grid gave them the
        // weight of envelope fields they do not have.
        const bits = [];
        if (bounds) {
          bits.push(
            `bbox ${fmtCoord(bounds.minLat)}, ${fmtCoord(bounds.minLng)}, ` +
              `${fmtCoord(bounds.maxLat)}, ${fmtCoord(bounds.maxLng)}`
          );
        }
        if (c) bits.push(`centroid ${fmtCoord(c.lat ?? 0)}, ${fmtCoord(c.lng ?? 0)}`);
        bits.push(decoded ? 'decoded from geometry.geojson (base64)' : 'geometry.geojson did not decode');
        var geomCaption = el('div', 'geom-caption', bits.join(' · '));
        // No decodable geometry means no map, so the caption is all there is.
        if (!decoded) geomSec.body.append(geomCaption);

        // The map is created AFTER the panel is attached to the document below,
        // so MapLibre measures the container at its real laid-out size (24rem ×
        // full width) rather than 0×0 on a detached node (which renders a tiny
        // square). renderMap holds that deferred work.
        var renderMap = null;
        if (decoded) {
          const mapBox = el('div', 'map-canvas');
          geomSec.body.append(mapBox);
          geomSec.body.append(geomCaption);
          renderMap = () => {
            if (!renderEventMap(mapBox, decoded, bounds, ev.severity || 'INFO')) {
              mapBox.remove();
              geomSec.body.append(
                el(
                  'p',
                  'muted small',
                  'Map unavailable (MapLibre failed to start in this browser) — bbox and centroid above are the geometry.'
                )
              );
            }
          };
        }
      }
      sectionsEl.append(geomSec.panel);
      if (typeof renderMap === 'function') renderMap();

      // Envelope — every common field, in proto order.
      const envelope = section('Envelope');
      const dl = el('dl', 'kv');
      for (const [key, type] of ENVELOPE_FIELDS) {
        const v = ev[key];
        const empty = v === undefined || v === null || v === '' ||
          (Array.isArray(v) && v.length === 0);
        if (empty) {
          kvRow(dl, key, absentValue(absenceFor(key, ev)), FIELD_DOCS[key]);
        } else if (type === 'severity') {
          kvRow(dl, key, sevChip(v), FIELD_DOCS[key]);
        } else if (type === 'time') {
          kvRow(dl, key, timeCell(v), FIELD_DOCS[key]);
        } else if (type === 'number') {
          kvRow(dl, key, fmtNum(v, 2), FIELD_DOCS[key]);
        } else if (type === 'list') {
          kvRow(dl, key, v.join(', '), FIELD_DOCS[key]);
        } else if (type === 'link' && typeof v === 'string' && /^https?:\/\//i.test(v)) {
          const a = el('a', '', v);
          a.href = v;
          a.rel = 'noopener';
          a.target = '_blank';
          kvRow(dl, key, a, FIELD_DOCS[key]);
        } else {
          kvRow(dl, key, String(v), FIELD_DOCS[key]);
        }
      }
      envelope.body.append(dl);
      sectionsEl.append(envelope.panel);

      // Typed detail — whichever oneof field is present.
      const detail = detailOf(ev);
      if (detail) {
        const sec = section(`Detail — ${detail.field}`);
        const ddl = el('dl', 'kv');
        for (const [k, v] of Object.entries(detail.value)) kvRow(ddl, k, valueNode(v));
        sec.body.append(ddl);
        sectionsEl.append(sec.panel);
      } else {
        const sec = section('Detail');
        sec.body.append(
          el('p', 'muted small', 'No typed detail block on this event (none of the detail oneof fields is set).')
        );
        sectionsEl.append(sec.panel);
      }


      // Provenance.
      const prov = ev.provenance || {};
      const provSec = section('Provenance');
      const pdl = el('dl', 'kv');
      kvRow(pdl, 'sourceId', prov.sourceId || '—');
      kvRow(pdl, 'sourceName', prov.sourceName || '—');
      kvRow(pdl, 'attribution', prov.attribution || '—');
      if (typeof prov.sourceUrl === 'string' && /^https?:\/\//i.test(prov.sourceUrl)) {
        const a = el('a', '', prov.sourceUrl);
        a.href = prov.sourceUrl;
        a.rel = 'noopener';
        a.target = '_blank';
        kvRow(pdl, 'sourceUrl', a);
      } else {
        kvRow(pdl, 'sourceUrl', prov.sourceUrl || '—');
      }
      kvRow(pdl, 'fetchedAt', prov.fetchedAt ? timeCell(prov.fetchedAt) : '—');
      provSec.body.append(pdl);
      sectionsEl.append(provSec.panel);

      // AI enhancement — badge, the verbatim original, and the model I/O
      // (what was sent / what came back), only when present. No whole-envelope
      // raw dump: the request and response are rendered explicitly below.
      if (ev.enhancement) {
        const enh = ev.enhancement;
        const sec = section('AI enhancement');
        sec.body.classList.add('enh-body');
        const badge = el('div', 'ai-badge');
        badge.append(
          el('span', 'ai-badge-tag', 'AI-enhanced'),
          el(
            'span',
            'mono',
            `${enh.model || '(model unspecified)'} · fields: ` +
              `${Array.isArray(enh.fields) && enh.fields.length ? enh.fields.join(', ') : '(unspecified)'}`
          )
        );
        sec.body.append(badge);
        if (enh.enhancedAt) {
          const when = el('div', 'muted small');
          when.append('enhanced ', timeCell(enh.enhancedAt));
          sec.body.append(when);
        }

        // Translate, never assert: the model rewrites the source text into plain
        // language, it does not add facts. Verbatim original first, structured
        // model output second, so the two can be checked against each other.
        sec.body.append(
          el(
            'p',
            'muted small',
            'The model rewrites the source feed’s wording into plain language — it translates, it does not ' +
              'add facts. The verbatim original is shown first; the structured model output follows, so you ' +
              'can check one against the other.'
          )
        );

        // Verbatim original (spec §3.1) — always alongside the AI text.
        sec.body.append(el('h3', '', 'Original text (verbatim, from the source)'));
        if (ev.description) {
          sec.body.append(el('pre', 'original-text', ev.description));
        } else {
          sec.body.append(
            el('p', 'muted small', '(no original description text on this revision)')
          );
        }

        // The structured model response, pretty-printed. The prompt is
        // deliberately not shown here — the original text above plus the
        // structured output is the useful before/after.
        if (enh.response) {
          sec.body.append(el('h3', '', 'Model response (what the model returned)'));
          sec.body.append(el('pre', 'code', prettyJSON(enh.response)));
        } else {
          sec.body.append(
            el('p', 'muted small', '(model response not captured for this event)')
          );
        }
        sectionsEl.append(sec.panel);
      }
    }

    /* ---- revision timeline ---- */

    function renderTimeline(currentEvent) {
      const n = Array.isArray(revisions) ? revisions.length : 0;
      const sec = section(
        n ? `Revision timeline · ${n} revision${n === 1 ? '' : 's'}` : 'Revision timeline'
      );
      sec.panel.id = 'ed-timeline';

      // This timeline is THIS record's history. The same data across every
      // event is the History screen, and a reader looking at one event's arc is
      // one click from wanting the rest — but nothing on this page said the
      // other screen existed. Scoped to the same layer so the link lands on
      // something related rather than the whole archive.
      const layer = currentEvent && currentEvent.layer;
      const archive = el('p', 'ed-archive-link muted small');
      const a = el('a', '', 'every revision of every event →');
      a.href = layer
        ? `/history?layer=${encodeURIComponent(String(layer).toLowerCase())}`
        : '/history';
      archive.append('This is one event\u2019s arc. ', a);
      sec.body.append(archive);

      if (histRes.status === 'rejected') {
        sec.body.append(errorBlock(histRes.reason, 'Could not load the revision history.'));
        sectionsEl.append(sec.panel);
        return;
      }

      const list = el('div', 'timeline');
      sec.body.append(list);

      const moreBtn = el('button', '', 'Load older revisions');
      moreBtn.type = 'button';
      const foot = el('div', 'timeline-foot');
      foot.append(moreBtn);
      sec.body.append(foot);

      function draw() {
        list.textContent = '';
        if (revisions.length === 0) {
          list.append(
            el(
              'p',
              'muted small',
              'History responded OK but returned no revisions' +
                (currentEvent ? ' — the current revision above is all there is.' : '.')
            )
          );
        }
        revisions.forEach((rev, i) => {
          list.append(revisionCard(rev, revisions[i + 1], i === revisions.length - 1));
        });
        moreBtn.hidden = !histNext;
      }

      moreBtn.addEventListener('click', async () => {
        moreBtn.disabled = true;
        try {
          const more = await get(historyPath, { ...ioParams, page_token: histNext });
          revisions = sortRevisionsDesc(
            revisions.concat(Array.isArray(more.revisions) ? more.revisions : [])
          );
          histNext = more.nextPageToken || '';
          draw();
        } catch (err) {
          foot.append(errorBlock(err, 'Could not load more revisions.'));
          moreBtn.hidden = true;
        } finally {
          moreBtn.disabled = false;
        }
      });

      draw();
      sectionsEl.append(sec.panel);

      function revisionCard(rev, prevRev, isOldestLoaded) {
        const card = el('div', 'rev-card');
        const revEvent = rev.event || {};

        // Header: the revision number, then everything about it on ONE line.
        // The severity and status chips that used to sit here are gone — they
        // describe the record, which the envelope above already covers, and
        // they pushed the thing a reader is here for (what changed) down.
        const head = el('div', 'rev-card-head');
        head.append(el('span', 'rev-n', `rev ${rev.revision ?? 0}`));
        const when = el('span', 'rev-when');
        when.textContent =
          `observed ${timeAgo(rev.observedAt || '')} · ingested ${timeAgo(rev.ingestedAt || '')}`;
        when.title = `observed ${rev.observedAt || 'unknown'}\ningested ${rev.ingestedAt || 'unknown'}`;
        head.append(when);

        const entries = prevRev ? diffObjects(prevRev.event || {}, revEvent) : [];
        const note = el('span', 'rev-note');
        if (!prevRev) {
          note.textContent = isOldestLoaded && histNext
            ? 'oldest loaded revision — load older to diff further back'
            : 'first recorded revision — nothing to diff against';
        } else if (entries.length === 0) {
          note.textContent = `no field-level changes vs rev ${prevRev.revision ?? 0}`;
        } else {
          note.textContent = `${entries.length} field${entries.length === 1 ? '' : 's'} changed`;
        }
        head.append(note);
        card.append(head);

        if (!entries.length) return card;

        // The diff: one row per field, indented behind a hairline so the run of
        // changes reads as belonging to the revision above it. Each row is the
        // field name, then `old → new` as ONE flowing line that wraps — not a
        // three-column table. The values here are a headline, a timestamp and a
        // serialized detail object, lengths with nothing in common, and a column
        // sized for one made the others unreadable.
        const body = el('div', 'rev-diff');
        for (const entry of entries) {
          const row = el('div', 'diff-row');
          row.append(el('div', 'diff-field', entry.path));

          const vals = el('div', 'diff-vals');
          if (entry.kind !== 'added') {
            const before = el('span', 'diff-before', diffText(entry.before));
            before.title = fmtDiffValueFull(entry.before);
            vals.append(before, ' ');
          }
          // The arrow carries direction; `added` and `removed` say so in words
          // too, because an arrow alone cannot tell "this appeared" from "this
          // changed from empty".
          vals.append(el('span', 'diff-arrow', entry.kind === 'added' ? '+' : '\u2192'), ' ');
          if (entry.kind === 'removed') {
            vals.append(el('span', 'diff-removed-note', 'removed'));
          } else {
            const after = el('span', 'diff-after', diffText(entry.after));
            after.title = fmtDiffValueFull(entry.after);
            vals.append(after);
          }
          row.append(vals);
          body.append(row);
        }
        card.append(body);

        // (no per-revision raw: one RAW disclosure lives at the foot of the pane)
        return card;
      }
    }
  }
}

/**
 * The /event?id=… page: read the id from the URL and render into the page's
 * container. All the work is renderEventDetail's; this only supplies the page
 * chrome (which the Events browser provides for itself).
 */
export function initEventPage() {
  const root = document.getElementById('ed-root');
  if (!root) return;
  const id = new URLSearchParams(location.search).get('id') || '';
  renderEventDetail(root, id, { setTitle: true });
}
