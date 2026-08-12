// docs.js — the live parts of the API reference.
//
// Three things, all built from assets/spec.js so the reference and the rest of
// the site document the API from one source:
//
//   1. Endpoint reference — collapsible entries with parameter tables and RUN
//      buttons that fetch the example URL for real and print the response.
//   2. The Event envelope beside a live record — each field's spec sentence next
//      to the actual value from a sampled event, with a cycler to page through
//      the records that were fetched.
//   3. The "on this page" rail, generated from the section headings already in
//      the document.
//
// Why RUN exists: an example URL in a static reference is a claim. Fetching it
// makes it a demonstration — and, when it has rotted, a visible failure rather
// than a quiet lie.

import { get, ApiError, curlFor } from '../api.js';
import { ENDPOINTS, FIELD_DOCS } from '../spec.js';
import { fmtNum, timeAgo } from '../format.js';
import { copyButton } from '../ui.js';

const el = (t, c, x) => {
  const n = document.createElement(t);
  if (c) n.className = c;
  if (x !== undefined) n.textContent = x;
  return n;
};

/* ------------------------------------------------- 1. endpoint reference */

/**
 * Fetch one example URL and render the real response into `pane`.
 * A failure renders as a failure — status, message, timing — never as an empty
 * body that could be mistaken for "the endpoint returned nothing".
 */
async function runExample(url, pane) {
  pane.textContent = '';
  const body = el('pre', null, 'running…');
  const foot = el('div', 'resp-foot');
  const head = el('div', 'resp-head');
  head.append(el('span', 'method', 'GET'), el('span', null, url));
  head.append(copyButton(curlFor(url), 'copy curl'));
  pane.append(head, body, foot);

  const t0 = Date.now();
  try {
    const data = await get(url);
    const text = JSON.stringify(data, null, 2);
    body.textContent = text;
    foot.textContent = '';
    foot.append(
      el('span', 'ok', '200 OK'),
      el('span', null, `${Date.now() - t0} ms`),
      el('span', null, `${fmtNum(new Blob([text]).size)} bytes`),
      el('span', null, 'application/json')
    );
  } catch (err) {
    const timedOut = err instanceof ApiError && err.timedOut;
    body.textContent = timedOut
      ? 'no response within 6000 ms — request abandoned'
      : String((err && err.message) || err);
    foot.textContent = '';
    foot.append(
      el('span', 'bad', timedOut ? 'timeout' : String((err && err.status) || 'error')),
      el('span', null, `${Date.now() - t0} ms`)
    );
  }
}

function endpointEntry(ep, index) {
  const wrap = el('div', 'ep');
  const btn = el('button', 'ep-head');
  btn.type = 'button';
  btn.setAttribute('aria-expanded', 'false');

  const method = el('span', 'ep-method', 'GET');
  const path = el('span', 'ep-path', ep.path);
  const blurb = el('span', 'ep-blurb', ep.blurb);
  const mark = el('span', 'ep-mark', '+');
  btn.append(method, path, blurb, mark);

  const bodyEl = el('div', 'ep-body');
  bodyEl.hidden = true;

  if (ep.detail) bodyEl.append(el('p', 'ep-detail', ep.detail));

  if (ep.params.length) {
    const wrapT = el('div', 'table-wrap');
    const table = el('table', 'data-table ep-params');
    const thead = el('thead');
    const hr = el('tr');
    for (const h of ['Param', 'Type', 'Semantics', 'Default']) hr.append(el('th', null, h));
    thead.append(hr);
    const tbody = el('tbody');
    for (const [name, type, semantics, dflt] of ep.params) {
      const tr = el('tr');
      tr.append(el('td', 'ep-param-name', name));
      tr.append(el('td', 'mono', type));
      const sem = el('td', 'wrap');
      sem.textContent = semantics;
      tr.append(sem);
      tr.append(el('td', 'mono', dflt));
      ['Param', 'Type', 'Semantics', 'Default'].forEach((lbl, i) => {
        if (tr.children[i]) tr.children[i].dataset.label = lbl;
      });
      tbody.append(tr);
    }
    table.append(thead, tbody);
    wrapT.append(table);
    bodyEl.append(wrapT);
  }

  if (ep.examples.length) {
    const exWrap = el('div', 'ep-examples');
    exWrap.append(el('div', 'ep-ex-label', 'Example requests — press RUN to fetch live'));
    const pane = el('div', 'resp-pane ep-pane');
    pane.hidden = true;
    for (const url of ep.examples) {
      const line = el('div', 'ep-ex');
      const run = el('button', 'ep-run', 'RUN');
      run.type = 'button';
      run.addEventListener('click', () => {
        pane.hidden = false;
        runExample(url, pane);
      });
      const u = el('code', 'ep-ex-url', url);
      line.append(run, u);
      exWrap.append(line);
    }
    exWrap.append(pane);
    bodyEl.append(exWrap);
  }

  const toggle = () => {
    const open = bodyEl.hidden;
    bodyEl.hidden = !open;
    mark.textContent = open ? '—' : '+';
    btn.setAttribute('aria-expanded', String(open));
    wrap.classList.toggle('open', open);
  };
  btn.addEventListener('click', toggle);

  // One endpoint open by default, so the shape of an entry is visible without
  // a click and the reader knows the rest expand too.
  if (index === 0) toggle();

  wrap.append(btn, bodyEl);
  return wrap;
}

function renderEndpoints() {
  const host = document.getElementById('ep-list');
  if (!host) return;
  host.textContent = '';
  ENDPOINTS.forEach((ep, i) => host.append(endpointEntry(ep, i)));
}

/* ------------------------------------------ 2. envelope beside live data */

/** Render a value compactly enough to sit in a table cell. */
function fmtValue(v) {
  if (v === undefined || v === null) return '—';
  if (typeof v === 'string') return v || '""';
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  if (Array.isArray(v)) return v.length ? JSON.stringify(v, null, 1) : '[]';
  if (typeof v === 'object') {
    // geometry.geojson is base64 bytes — show its size, not 40KB of payload.
    if (v.geojson) {
      const clone = { ...v, geojson: `«${fmtNum(String(v.geojson).length)} base64 chars»` };
      return JSON.stringify(clone, null, 1);
    }
    return JSON.stringify(v, null, 1);
  }
  return String(v);
}

async function renderEnvelope() {
  const host = document.getElementById('env-live');
  if (!host) return;

  let events = [];
  try {
    const data = await get('/api/v1/events', { page_size: 25 });
    events = Array.isArray(data.events) ? data.events : [];
  } catch (err) {
    host.textContent = '';
    const block = el('div', 'error-block');
    block.append(
      el('strong', null, 'Could not fetch a sample record. '),
      el('span', 'error-url', err instanceof ApiError ? `GET ${err.url}` : ''),
      el('div', null,
        'The field table below is still accurate — it is static reference — but the ' +
        'live column is unavailable, which is itself worth knowing.')
    );
    host.append(block);
    return;
  }

  if (!events.length) {
    host.textContent = '';
    host.append(
      el('p', 'notice',
        'The event query succeeded and returned no records, so there is nothing to ' +
        'sample right now. This is a confirmed empty result, not a failure.')
    );
    return;
  }

  let idx = 0;

  const bar = el('div', 'env-bar');
  const barId = el('span', 'env-bar-id');
  const next = el('button', 'copy-btn', 'next event →');
  next.type = 'button';
  bar.append(barId, next);

  const rows = el('div', 'env-rows');
  host.textContent = '';
  host.append(bar, rows);

  function paint() {
    const ev = events[idx];
    barId.textContent = `${ev.id || '(no id)'} · ${String(ev.layer || '').toLowerCase()} · sampled ${timeAgo(ev.observedAt)} · record ${idx + 1}/${events.length}`;
    rows.textContent = '';
    for (const [field, [type, doc]] of Object.entries(FIELD_DOCS)) {
      // Detail blocks are a oneof: only render the one this record carries.
      if (type === 'detail' && !(field in ev)) continue;
      const row = el('div', 'env-row');

      const left = el('div');
      const name = el('div', 'env-name');
      name.append(el('b', 'env-field', field), ' ', el('span', 'env-type', type));
      left.append(name, el('div', 'env-doc', doc));

      const right = el('div', 'env-val');
      right.textContent = fmtValue(ev[field]);

      row.append(left, right);
      rows.append(row);
    }
  }

  next.addEventListener('click', () => {
    idx = (idx + 1) % events.length;
    paint();
  });
  paint();
}

/* ------------------------------------------------------- 3. on-this-page */

function renderRail() {
  const host = document.getElementById('docs-rail');
  if (!host) return;
  // NOT `section[id] > h2`: §6's heading sits inside a .contract-callout, so a
  // direct-child selector silently drops it from the rail. Take the first h2
  // anywhere inside each section, and anchor to the SECTION's id (which is the
  // scroll target), not the heading's parent.
  const sections = document.querySelectorAll('main section[id]');
  if (sections.length < 3) return;
  const list = el('ol', 'rail-list');
  for (const sec of sections) {
    const h = sec.querySelector('h2');
    if (!h) continue;
    const li = el('li');
    const a = el('a', null, h.textContent.trim());
    a.href = `#${sec.id}`;
    li.append(a);
    list.append(li);
  }
  if (!list.childElementCount) return;
  host.append(el('div', 'rail-label', 'On this page'), list);
}

renderEndpoints();
renderEnvelope();
renderRail();
