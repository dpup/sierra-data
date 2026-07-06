// pages/event-detail.js — /event?id=... event detail (site spec §2
// /events/{id}).
//
// Two requests: GET /v1/events/{id} (current revision) and
// GET /v1/events/{id}/history (revisions, newest first). Renders the full
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
  timeCell,
  sevChip,
  layerLabel,
  decodeGeometry,
  SEVERITY_COLORS,
} from '../format.js';
import { diffObjects } from '../diff.js';
import { BASE_STYLE } from '../basemap.js';

/**
 * The Event.detail oneof's protojson field names (grid.proto fields 20–30).
 * Exactly one may be present on an event; protojson uses the snake_case
 * proto field name as the JSON key.
 */
export const DETAIL_FIELDS = [
  'wildfire',
  'evacuation',
  'weather_alert',
  'fire_weather',
  'earthquake',
  'road_incident',
  'power',
  'gauge',
  'air_quality',
  'network',
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
  ['area_label', 'text'],
  ['canonical_url', 'link'],
  ['place_ids', 'list'],
  ['effective', 'time'],
  ['expires', 'time'],
  ['observed_at', 'time'],
  ['ingested_at', 'time'],
  ['revision', 'number'],
];

/**
 * Find the populated detail oneof field on an event.
 * @param {Object} event protojson Event (snake_case)
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
      minLat: Number(bbox.min_lat ?? 0),
      minLng: Number(bbox.min_lng ?? 0),
      maxLat: Number(bbox.max_lat ?? 0),
      maxLng: Number(bbox.max_lng ?? 0),
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
  try {
    s = JSON.stringify(v);
  } catch {
    s = String(v);
  }
  if (typeof s !== 'string') s = String(s);
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
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

/** Copyable request line: GET <url> [copy] curl … */
function requestLine(url) {
  const row = el('div', 'query-row');
  const code = el('code', 'inline', `GET ${url}`);
  const copy = el('button', 'copy-btn', 'copy');
  copy.type = 'button';
  copy.addEventListener('click', () => {
    navigator.clipboard.writeText(curlFor(url)).then(
      () => {
        copy.textContent = 'copied';
        setTimeout(() => (copy.textContent = 'copy'), 1200);
      },
      () => {
        copy.textContent = 'failed';
      }
    );
  });
  row.append(code, ' ', copy);
  return row;
}

/**
 * Section panel with a heading and a raw-protojson toggle at the bottom.
 * @returns {{panel: HTMLElement, body: HTMLElement}}
 */
function section(title, rawObj) {
  const panel = el('section', 'panel');
  panel.append(el('h2', '', title));
  const body = el('div', 'sec-body');
  panel.append(body);
  if (rawObj !== undefined) panel.append(rawToggle(rawObj));
  return { panel, body };
}

/** Pretty-print a JSON string if it parses; otherwise return it unchanged. */
function prettyJSON(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

/** <details> raw toggle with pretty-printed protojson. */
function rawToggle(obj) {
  const details = el('details', 'raw-toggle');
  details.append(el('summary', '', 'raw'));
  const pre = el('pre', 'code');
  pre.textContent = JSON.stringify(obj, null, 2);
  details.append(pre);
  return details;
}

/** Append one dt/dd row to a .kv list; value may be a node or string. */
function kvRow(dl, key, value) {
  dl.append(el('dt', '', key));
  const dd = document.createElement('dd');
  if (value instanceof Node) dd.append(value);
  else dd.textContent = value; // textContent: upstream text is untrusted
  dl.append(dd);
  return dd;
}

/** Render an arbitrary detail-field value (primitive / array / object). */
function valueNode(v) {
  if (v === null || v === undefined) return '—';
  if (Array.isArray(v)) {
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
  const color = SEVERITY_COLORS[String(severity || '').toUpperCase()] || SEVERITY_COLORS.INFO;
  try {
    const center = bounds
      ? [(bounds.minLng + bounds.maxLng) / 2, (bounds.minLat + bounds.maxLat) / 2]
      : [-120.35, 38.2];
    // Shared OSM raster basemap (basemap.js) for geographic context under the
    // event geometry; the bbox/centroid text above the map is the fallback when
    // the map library fails to load. API data stays same-origin /v1/* via api.js.
    const map = new lib.Map({
      container,
      style: BASE_STYLE,
      center,
      zoom: 9,
    });
    map.addControl(new lib.NavigationControl(), 'top-right');
    // Insurance against a container that gains its final size just after init:
    // resize once the style is ready so the canvas fills the 24rem box.
    map.on('load', () => {
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
export function initEventPage() {

  const $ = (id) => document.getElementById(id);
  const loadingEl = $('ed-loading');
  const errorsEl = $('ed-errors');
  const headEl = $('ed-head');
  const chipsEl = $('ed-chips');
  const headlineEl = $('ed-headline');
  const subEl = $('ed-sub');
  const queryEl = $('ed-query');
  const sectionsEl = $('ed-sections');

  const id = new URLSearchParams(location.search).get('id');
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

  const eventPath = `/v1/events/${encodeURIComponent(id)}`;
  const historyPath = `/v1/events/${encodeURIComponent(id)}/history`;
  // Opt into the model I/O (enhancement.request/response) — the detail page
  // shows it; list endpoints omit it by default to stay lean.
  const ioParams = { enhancement_io: 'true' };

  queryEl.append(
    requestLine(apiURL(eventPath, ioParams)),
    requestLine(apiURL(historyPath, ioParams))
  );

  load();

  async function load() {
    const [evRes, histRes] = await Promise.allSettled([
      get(eventPath, ioParams),
      get(historyPath, ioParams),
    ]);
    loadingEl.remove();

    let event = evRes.status === 'fulfilled' ? evRes.value : null;
    const hist = histRes.status === 'fulfilled' ? histRes.value : null;
    let revisions = sortRevisionsDesc(hist && Array.isArray(hist.revisions) ? hist.revisions : []);
    let histNext = (hist && hist.next_page_token) || '';

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
            'Showing the newest revision from /history instead of the /events/{id} response.'
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

    /* ---- header + sections ---- */

    function renderEvent(ev) {
      headEl.hidden = false;
      document.title = `${ev.headline || ev.id || id} · Event · The Grid`;

      chipsEl.textContent = '';
      chipsEl.append(sevChip(ev.severity || 'INFO'));
      chipsEl.append(el('span', 'meta-chip mono', ev.status || 'EVENT_STATUS_UNSPECIFIED'));
      chipsEl.append(el('span', 'meta-chip mono', layerLabel(ev.layer || '')));
      if (ev.enhancement) chipsEl.append(el('span', 'meta-chip ai-chip mono', 'AI-enhanced'));

      headlineEl.textContent = ev.headline || ev.id || '(no headline)';
      subEl.textContent = [ev.id, ev.category, ev.area_label].filter(Boolean).join(' · ');

      // Envelope — every common field, in proto order.
      const envelope = section('Envelope', ev);
      const dl = el('dl', 'kv');
      for (const [key, type] of ENVELOPE_FIELDS) {
        const v = ev[key];
        if (type === 'severity') {
          kvRow(dl, key, sevChip(v || 'INFO'));
        } else if (type === 'time') {
          kvRow(dl, key, v ? timeCell(v) : '—');
        } else if (type === 'number') {
          kvRow(dl, key, String(v ?? 0));
        } else if (type === 'list') {
          kvRow(dl, key, Array.isArray(v) && v.length ? v.join(', ') : '—');
        } else if (type === 'link' && typeof v === 'string' && /^https?:\/\//i.test(v)) {
          const a = el('a', '', v);
          a.href = v;
          a.rel = 'noopener';
          a.target = '_blank';
          kvRow(dl, key, a);
        } else {
          kvRow(dl, key, v === undefined || v === null || v === '' ? '—' : String(v));
        }
      }
      envelope.body.append(dl);
      sectionsEl.append(envelope.panel);

      // Typed detail — whichever oneof field is present.
      const detail = detailOf(ev);
      if (detail) {
        const sec = section(`Detail — ${detail.field}`, detail.value);
        const ddl = el('dl', 'kv');
        for (const [k, v] of Object.entries(detail.value)) kvRow(ddl, k, valueNode(v));
        sec.body.append(ddl);
        sectionsEl.append(sec.panel);
      } else {
        const sec = section('Detail', undefined);
        sec.body.append(
          el('p', 'muted small', 'No typed detail block on this event (none of the detail oneof fields is set).')
        );
        sectionsEl.append(sec.panel);
      }

      // Geometry — map when possible, textual bbox/centroid always.
      const geomSec = section('Geometry', ev.geometry);
      if (!ev.geometry) {
        geomSec.body.append(
          el('p', 'muted small', 'No geometry on this event (county-wide advisories often carry none).')
        );
      } else {
        const decoded = decodeGeometry(ev.geometry.geojson);
        const bounds = geometryBounds(ev.geometry, decoded);
        const gdl = el('dl', 'kv');
        kvRow(gdl, 'type', decoded ? decoded.type || '(untyped)' : '(geojson bytes did not decode)');
        kvRow(
          gdl,
          'bbox',
          bounds
            ? `lat ${fmtCoord(bounds.minLat)} … ${fmtCoord(bounds.maxLat)}, ` +
                `lng ${fmtCoord(bounds.minLng)} … ${fmtCoord(bounds.maxLng)}`
            : '—'
        );
        const c = ev.geometry.centroid;
        kvRow(
          gdl,
          'centroid',
          c ? `${fmtCoord(c.lat ?? 0)}, ${fmtCoord(c.lng ?? 0)}` : '—'
        );
        geomSec.body.append(gdl);

        // The map is created AFTER the panel is attached to the document below,
        // so MapLibre measures the container at its real laid-out size (24rem ×
        // full width) rather than 0×0 on a detached node (which renders a tiny
        // square). renderMap holds that deferred work.
        var renderMap = null;
        if (decoded) {
          const mapBox = el('div', 'map-canvas');
          geomSec.body.append(mapBox);
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

      // Provenance.
      const prov = ev.provenance || {};
      const provSec = section('Provenance', ev.provenance);
      const pdl = el('dl', 'kv');
      kvRow(pdl, 'source_id', prov.source_id || '—');
      kvRow(pdl, 'source_name', prov.source_name || '—');
      kvRow(pdl, 'attribution', prov.attribution || '—');
      if (typeof prov.source_url === 'string' && /^https?:\/\//i.test(prov.source_url)) {
        const a = el('a', '', prov.source_url);
        a.href = prov.source_url;
        a.rel = 'noopener';
        a.target = '_blank';
        kvRow(pdl, 'source_url', a);
      } else {
        kvRow(pdl, 'source_url', prov.source_url || '—');
      }
      kvRow(pdl, 'fetched_at', prov.fetched_at ? timeCell(prov.fetched_at) : '—');
      provSec.body.append(pdl);
      sectionsEl.append(provSec.panel);

      // AI enhancement — badge, the verbatim original, and the model I/O
      // (what was sent / what came back), only when present. No whole-envelope
      // raw dump: the request and response are rendered explicitly below.
      if (ev.enhancement) {
        const enh = ev.enhancement;
        const sec = section('AI enhancement', undefined);
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
        if (enh.enhanced_at) {
          const when = el('div', 'muted small');
          when.append('enhanced ', timeCell(enh.enhanced_at));
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
      const sec = section('Revision timeline', undefined);
      sec.panel.id = 'ed-timeline';

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
          histNext = more.next_page_token || '';
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

        const head = el('div', 'rev-card-head');
        head.append(el('strong', 'mono', `rev ${rev.revision ?? 0}`));
        const observed = el('span', 'muted small');
        observed.append('observed ', timeCell(rev.observed_at || ''));
        const ingested = el('span', 'muted small');
        ingested.append('ingested ', timeCell(rev.ingested_at || ''));
        head.append(observed, ingested);
        const revEvent = rev.event || {};
        head.append(sevChip(revEvent.severity || 'INFO'));
        head.append(el('span', 'muted small mono', revEvent.status || '—'));
        card.append(head);

        if (!prevRev) {
          card.append(
            el(
              'p',
              'muted small',
              isOldestLoaded && histNext
                ? 'Oldest loaded revision — load older revisions to diff further back.'
                : 'First recorded revision — nothing earlier to diff against.'
            )
          );
        } else {
          const entries = diffObjects(prevRev.event || {}, revEvent);
          if (entries.length === 0) {
            card.append(
              el('p', 'muted small', `No field-level changes vs rev ${prevRev.revision ?? 0}.`)
            );
          } else {
            const wrap = el('div', 'table-wrap diff-wrap');
            const table = el('table', 'data-table diff-table');
            const thead = document.createElement('thead');
            const hr = document.createElement('tr');
            for (const h of ['', 'field', `rev ${prevRev.revision ?? 0}`, `rev ${rev.revision ?? 0}`]) {
              hr.append(el('th', '', h));
            }
            thead.append(hr);
            table.append(thead);
            const tbody = document.createElement('tbody');
            for (const entry of entries) {
              const tr = document.createElement('tr');
              const marks = { added: '+ added', removed: '− removed', changed: 'Δ changed' };
              tr.append(el('td', `diff-kind k-${entry.kind}`, marks[entry.kind] || entry.kind));
              tr.append(el('td', 'mono diff-path', entry.path));
              tr.append(el('td', 'mono wrap diff-val', fmtDiffValue(entry.before)));
              tr.append(el('td', 'mono wrap diff-val', fmtDiffValue(entry.after)));
              tbody.append(tr);
            }
            table.append(tbody);
            wrap.append(table);
            card.append(wrap);
          }
        }

        card.append(rawToggle(rev));
        return card;
      }
    }
  }
}
