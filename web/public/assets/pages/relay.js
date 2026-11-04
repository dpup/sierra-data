// pages/relay.js — /relay: SIERRA's own MeshCore gateways, and whether each can
// be used right now.
//
// Reads GET /mesh/relay/api/status — the relay's one public endpoint, served by
// the mesh relay (a separate process from the Grid; snake_case, its own spec in
// docs/mesh-relay-server.md). It deliberately carries no pubkeys, notes, node
// names or lease holders: a gateway sits at a volunteer's house.
//
// Fail-loud: the relay is its own failure domain. A failed fetch is "gateway
// status UNKNOWN", never "every gateway offline"; with a last-good board it is
// shown as stale with its age.
//
// Pure helpers have no DOM access at import time so node can import and test
// this module.

import { get, ApiError } from '../api.js';
import { timeCell, timeAgo, timeAbs } from '../format.js';
import { requireEls } from '../ui.js';

export const STATUS_PATH = '/mesh/relay/api/status';

/**
 * A gateway's one-word state, most-usable first, and the status-chip tone it
 * borrows (the same --st-* ramp the Sources board uses).
 *
 *   READY       bridge online, node connected and verified
 *   NODE DOWN   bridge online, but its radio is not connected
 *   UNVERIFIED  the node reports a pubkey the relay has not accepted
 *   OFFLINE     no bridge attached
 */
export const STATES = {
  READY: { label: 'READY', tone: 'OK', rank: 3 },
  NODE_DOWN: { label: 'NODE DOWN', tone: 'STALE', rank: 1 },
  UNVERIFIED: { label: 'UNVERIFIED', tone: 'STALE', rank: 2 },
  OFFLINE: { label: 'OFFLINE', tone: 'UNAVAILABLE', rank: 0 },
};

/**
 * Classify a public gateway record.
 * @param {{online?:boolean, ready?:boolean, node_state?:string}} g
 * @returns {keyof STATES}
 */
export function gatewayState(g) {
  if (!g || !g.online) return 'OFFLINE';
  if (g.ready) return 'READY';
  if (g.node_state === 'mismatch') return 'UNVERIFIED';
  return 'NODE_DOWN';
}

/** Least-usable first, then by name — the rows that need attention lead. */
export function sortGateways(gateways) {
  return [...gateways].sort((a, b) => {
    const ra = STATES[gatewayState(a)].rank;
    const rb = STATES[gatewayState(b)].rank;
    if (ra !== rb) return ra - rb;
    return String(a.name ?? '').localeCompare(String(b.name ?? ''));
  });
}

/** Counts by state, plus how many are in use. */
export function summarize(gateways) {
  const out = { total: gateways.length, READY: 0, NODE_DOWN: 0, UNVERIFIED: 0, OFFLINE: 0, inUse: 0 };
  for (const g of gateways) {
    out[gatewayState(g)] += 1;
    if (g.in_use) out.inUse += 1;
  }
  return out;
}

/** "v0.3.1 · serial", or "" when the bridge has never said. */
export function bridgeLabel(g) {
  const parts = [];
  if (g.agent_version) parts.push(/^\d/.test(g.agent_version) ? `v${g.agent_version}` : g.agent_version);
  if (g.link_kind) parts.push(g.link_kind);
  return parts.join(' · ');
}

/* ------------------------------------------------------------------ */
/* Page wiring (browser only)                                          */
/* ------------------------------------------------------------------ */

const REFRESH_MS = 30_000;

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

function stateChip(key) {
  const s = STATES[key];
  const chip = el('span', `status-chip st-${s.tone}`, s.label);
  return chip;
}

const COLUMNS = ['Gateway', 'State', 'Node', 'Lease', 'Bridge', 'Last seen'];

function renderRow(g) {
  const key = gatewayState(g);
  const tr = el('tr', key === 'READY' ? '' : `row-${STATES[key].tone}`);

  const name = el('td');
  name.append(el('span', 'cell-name mono', g.name || '?'));
  tr.append(name);

  const state = el('td');
  state.append(stateChip(key));
  tr.append(state);

  // The node is only knowable through a live bridge; offline means unknown,
  // not disconnected.
  const node = el('td');
  if (!g.online) node.append(el('span', 'absent absent-na', 'unknown while offline'));
  else node.append(el('span', 'mono', g.node_state || 'unknown'));
  tr.append(node);

  const lease = el('td');
  // Only the holder's role is public (agent | operator | admin), never who.
  if (g.in_use) lease.append(el('span', 'mono', g.in_use_role ? `in use · ${g.in_use_role}` : 'in use'));
  else if (key === 'READY') lease.append(el('span', 'mono muted', 'free'));
  else lease.append(el('span', 'muted', '—'));
  tr.append(lease);

  const bridge = el('td');
  const bl = bridgeLabel(g);
  bridge.append(bl ? el('span', 'mono', bl) : el('span', 'absent absent-empty', 'never connected'));
  tr.append(bridge);

  const seen = el('td');
  if (g.online && g.connected_since) {
    seen.append(el('span', 'mono', 'now'));
    const sub = el('div', 'cell-sub', `connected ${timeAgo(g.connected_since)}`);
    sub.title = g.connected_since;
    seen.append(sub);
  } else if (g.last_seen) {
    seen.append(timeCell(g.last_seen));
  } else {
    seen.append(el('span', 'absent absent-empty', 'never'));
  }
  tr.append(seen);

  tr.querySelectorAll(':scope > td').forEach((td, i) => { td.dataset.label = COLUMNS[i]; });
  return tr;
}

function errorBlock(err, hadData) {
  const div = el('div', hadData ? 'notice' : 'error-block');
  div.setAttribute('role', 'alert');
  div.append(el('div', '', hadData
    ? 'The relay stopped answering. The board below is the last good read, and may be out of date.'
    : 'Gateway status is UNKNOWN: the relay did not answer. This does not mean the gateways are offline.'));
  div.append(el('div', 'error-url', err instanceof ApiError
    ? `GET ${err.url} — ${err.status ? `HTTP ${err.status}` : err.reason || 'network error'}`
    : String(err && err.message ? err.message : err)));
  return div;
}

/** One-time page init. */
export function initRelayPage() {
  const els = requireEls('relay.js', {
    summary: 'relay-summary',
    errors: 'relay-errors',
    board: 'relay-board',
    refresh: 'relay-refresh',
  });
  const state = { gateways: null, generatedAt: null, error: null };

  function render() {
    els.summary.textContent = '';
    els.errors.textContent = '';
    els.board.textContent = '';
    if (state.error) els.errors.append(errorBlock(state.error, Boolean(state.gateways)));
    if (!state.gateways) {
      if (!state.error) els.board.append(el('div', 'loading', `Loading ${STATUS_PATH}…`));
      return;
    }
    const c = summarize(state.gateways);
    els.summary.append(el('span', 'muted', `${c.total} gateway${c.total === 1 ? '' : 's'}`));
    for (const [key, n] of [['READY', c.READY], ['NODE_DOWN', c.NODE_DOWN], ['UNVERIFIED', c.UNVERIFIED], ['OFFLINE', c.OFFLINE]]) {
      if (n === 0 && key !== 'READY') continue;
      els.summary.append(el('span', 'pill', `${n} ${STATES[key].label.toLowerCase()}`));
    }
    els.summary.append(el('span', 'pill', `${c.inUse} in use`));

    if (state.gateways.length === 0) {
      els.board.append(el('div', 'notice', 'No gateways are registered with the relay yet.'));
      return;
    }
    const wrap = el('div', 'table-wrap');
    const table = el('table', 'data-table capped-head');
    const thead = el('thead');
    const hr = el('tr');
    for (const col of COLUMNS) hr.append(el('th', '', col));
    thead.append(hr);
    const tbody = el('tbody');
    for (const g of sortGateways(state.gateways)) tbody.append(renderRow(g));
    table.append(thead, tbody);
    wrap.append(table);
    els.board.append(wrap);
  }

  function renderRefresh() {
    els.refresh.textContent = '';
    if (state.generatedAt) {
      els.refresh.append(
        el('span', '', `Read ${timeAgo(state.generatedAt)} (${timeAbs(state.generatedAt)}) from `),
        el('code', 'inline', STATUS_PATH),
        el('span', '', ` — refreshes every ${REFRESH_MS / 1000}s while this tab is visible.`)
      );
    }
  }

  async function load() {
    try {
      const body = await get(STATUS_PATH);
      state.gateways = Array.isArray(body && body.gateways) ? body.gateways : [];
      state.generatedAt = (body && body.generated_at) || new Date().toISOString();
      state.error = null;
    } catch (err) {
      state.error = err; // keep last-good gateways, if any
    }
    render();
    renderRefresh();
  }

  load();
  setInterval(() => { if (document.visibilityState === 'visible') load(); }, REFRESH_MS);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') load(); });
}
