// api.js — the single network surface of data.sierragridteam.org.
//
// Every data fetch on this site is a same-origin browser GET of a /api/v1/* path
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
   * @param {string=} reason Transport-level detail (e.g. the timeout message)
   */
  constructor(status, url, body, reason) {
    const detail =
      body && typeof body === 'object' && body.message
        ? `: ${body.message}`
        : reason
          ? `: ${reason}`
          : '';
    super(`GET ${url} failed (${status || 'network error'})${detail}`);
    this.name = 'ApiError';
    this.status = status;
    this.url = url;
    this.body = body;
    this.reason = reason || null;
    this.timedOut = Boolean(reason && reason.includes('request abandoned'));
  }
}

/**
 * Request log: one entry per get() call, in order.
 * Entries: { url, method, status, ok, startedAt, durationMs, error, timedOut }
 * The footer renders this; pages may read it too.
 */
export const requests = [];

/**
 * Deadline for every request, in ms.
 *
 * A hung endpoint is worse than a failed one: without a deadline a screen sits
 * on "fetching…" forever, which is neither a value nor an admission that the
 * state is unknown — and the fail-loud contract says an unknown must be stated.
 * Six seconds is well past the slowest healthy response (the summary and the
 * 60-event list both land in ~3s) and well short of a user giving up.
 *
 * /api/v1/history is the endpoint that made this necessary: it was measured at
 * 6-40s before the observed_at index landed (store migration v4), and a
 * 6s abort was what turned that into a visible, replayable failure rather than
 * a dead screen. Callers that legitimately need longer pass `timeoutMs`.
 */
export const DEFAULT_TIMEOUT_MS = 6000;

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
 * @param {string} path   e.g. "/api/v1/events" (leading /api/v1 included by caller)
 * @param {Object=} params
 * @returns {string} path with query string, e.g. "/api/v1/events?place=ebbetts-pass"
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
 * Every request runs under a deadline (DEFAULT_TIMEOUT_MS); an abort is logged
 * as a timeout and thrown as an ApiError so it takes the same loud-failure path
 * as any other error. Never let a hang resolve to silence.
 *
 * @param {string} path    e.g. "/api/v1/sources"
 * @param {Object=} params query params; empty values skipped
 * @param {{timeoutMs?: number}=} opts
 * @returns {Promise<*>} parsed JSON body
 */
export async function get(path, params, opts) {
  const url = apiURL(path, params);
  const timeoutMs = (opts && opts.timeoutMs) || DEFAULT_TIMEOUT_MS;
  const entry = {
    url,
    method: 'GET',
    status: null,
    ok: false,
    startedAt: new Date().toISOString(),
    durationMs: null,
    error: null,
    timedOut: false,
  };
  requests.push(entry);
  const t0 = Date.now();

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  let res;
  try {
    res = await fetch(url, {
      headers: { Accept: 'application/json' },
      signal: controller.signal,
    });
  } catch (err) {
    entry.status = 0;
    entry.durationMs = Date.now() - t0;
    // An abort is our own deadline firing, not a network fault — say which, so
    // the drawer distinguishes "the server never answered" from "DNS failed".
    if (err && err.name === 'AbortError') {
      entry.timedOut = true;
      entry.error = `no response within ${timeoutMs} ms — request abandoned`;
    } else {
      entry.error = String(err && err.message ? err.message : err);
    }
    clearTimeout(timer);
    announce(entry);
    throw new ApiError(0, url, null, entry.error);
  }

  entry.status = res.status;
  entry.ok = res.ok;
  entry.durationMs = Date.now() - t0;

  // NOTE: the timer is deliberately still armed here. fetch() resolves on
  // RESPONSE HEADERS; the body is a separate stream that can stall
  // indefinitely. Clearing the timeout at this point would leave a hung body
  // hanging the screen forever — which is exactly what the deadline exists to
  // prevent. Every path below clears it once the body has actually been read.
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
    } finally {
      clearTimeout(timer);
    }
    entry.error =
      body && typeof body === 'object' && body.message
        ? body.message
        : `HTTP ${res.status}`;
    announce(entry);
    throw new ApiError(res.status, url, body);
  }

  try {
    const data = await res.json();
    clearTimeout(timer);
    announce(entry);
    return data;
  } catch (err) {
    // 200 with a body that is not JSON (a proxy error page, a truncated
    // response, a hung stream that aborted mid-body). The request did NOT
    // succeed, so it must not be logged green — and it must throw the error
    // type every caller's catch block already handles.
    clearTimeout(timer);
    entry.ok = false;
    entry.durationMs = Date.now() - t0;
    const aborted = err && err.name === 'AbortError';
    if (aborted) entry.timedOut = true;
    entry.error = aborted
      ? `response body did not arrive within ${timeoutMs} ms — request abandoned`
      : `response was not valid JSON: ${(err && err.message) || err}`;
    announce(entry);
    throw new ApiError(res.status, url, null, entry.error);
  }
}
