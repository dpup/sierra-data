// api.js — the single network surface of data.sierragridteam.org.
//
// Every data fetch on this site is a same-origin browser GET of a /v1/* path
// through this module. Each request is recorded in `requests` and announced
// via a "grid:api-request" CustomEvent so the shared footer can render the
// live "requests behind this page" log. No other module performs network I/O.
//
// Pure URL/curl helpers have no DOM or network access at import time, so node
// can import and test this module directly.

/** Typed error for non-2xx responses. */
export class ApiError extends Error {
  /**
   * @param {number} status HTTP status code (0 for network-level failures)
   * @param {string} url    Full request URL
   * @param {*} body        Parsed error body if JSON, else raw text (may be null)
   */
  constructor(status, url, body) {
    const detail =
      body && typeof body === 'object' && body.message
        ? `: ${body.message}`
        : '';
    super(`GET ${url} failed (${status || 'network error'})${detail}`);
    this.name = 'ApiError';
    this.status = status;
    this.url = url;
    this.body = body;
  }
}

/**
 * Request log: one entry per get() call, in order.
 * Entries: { url, method, status, ok, startedAt, durationMs, error }
 * The footer renders this; pages may read it too.
 */
export const requests = [];

const EVENT_NAME = 'grid:api-request';

function announce(entry) {
  // In the browser, tell the footer a request happened. In node (tests),
  // there is no document; the log array alone suffices.
  if (typeof document !== 'undefined') {
    document.dispatchEvent(new CustomEvent(EVENT_NAME, { detail: entry }));
  }
}

/** Event name the footer listens for. */
export const API_REQUEST_EVENT = EVENT_NAME;

/**
 * Build the URL for an API path + query params.
 * Empty/null/undefined param values are skipped so URLs stay canonical and
 * shareable. Array values become repeated params (?layer=a&layer=b).
 *
 * @param {string} path   e.g. "/v1/events" (leading /v1 included by caller)
 * @param {Object=} params
 * @returns {string} path with query string, e.g. "/v1/events?place=calaveras"
 */
export function apiURL(path, params) {
  const search = new URLSearchParams();
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      if (value === undefined || value === null || value === '') continue;
      if (Array.isArray(value)) {
        for (const v of value) {
          if (v === undefined || v === null || v === '') continue;
          search.append(key, String(v));
        }
      } else {
        search.append(key, String(value));
      }
    }
  }
  const qs = search.toString();
  return qs ? `${path}?${qs}` : path;
}

/**
 * Render a copyable curl line for a request URL. Relative URLs are shown
 * against the canonical public origin so the line works from any shell.
 * @param {string} url
 * @returns {string}
 */
export function curlFor(url) {
  const absolute = /^https?:\/\//.test(url)
    ? url
    : `https://data.sierragridteam.org${url}`;
  return `curl -s '${absolute}'`;
}

/**
 * GET an API path, returning parsed JSON.
 * Records the request (success or failure) and dispatches API_REQUEST_EVENT.
 * Throws ApiError on non-2xx or network failure — callers render the error
 * inline (never a blank page).
 *
 * @param {string} path    e.g. "/v1/sources"
 * @param {Object=} params query params; empty values skipped
 * @returns {Promise<*>} parsed JSON body
 */
export async function get(path, params) {
  const url = apiURL(path, params);
  const entry = {
    url,
    method: 'GET',
    status: null,
    ok: false,
    startedAt: new Date().toISOString(),
    durationMs: null,
    error: null,
  };
  requests.push(entry);
  const t0 = Date.now();

  let res;
  try {
    res = await fetch(url, { headers: { Accept: 'application/json' } });
  } catch (err) {
    entry.status = 0;
    entry.durationMs = Date.now() - t0;
    entry.error = String(err && err.message ? err.message : err);
    announce(entry);
    throw new ApiError(0, url, null);
  }

  entry.status = res.status;
  entry.ok = res.ok;
  entry.durationMs = Date.now() - t0;

  if (!res.ok) {
    // Errors are google.rpc.Status protojson ({"code":5,"message":"..."})
    // when the API produced them; fall back to raw text otherwise.
    let body = null;
    try {
      const text = await res.text();
      try {
        body = JSON.parse(text);
      } catch {
        body = text || null;
      }
    } catch {
      body = null;
    }
    entry.error =
      body && typeof body === 'object' && body.message
        ? body.message
        : `HTTP ${res.status}`;
    announce(entry);
    throw new ApiError(res.status, url, body);
  }

  announce(entry);
  return res.json();
}
