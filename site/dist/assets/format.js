// format.js — pure formatting helpers shared by every page.
//
// No DOM access at import time: functions that build elements only touch
// `document` when called, so node can import this module and test the pure
// helpers (esc, timeAgo, timeAbs, decodeGeometry, layerLabel, fmtNum).
//
// Severity ramp and status colors are canonical (v2-implementation-plan §2.4):
// color is never the only signal — chips and dots always carry a text label.

/** Escape a string for safe insertion into HTML. All upstream-derived text
 * (headlines, descriptions, error strings) is untrusted and must pass through
 * esc() or be assigned via textContent. */
export function esc(s) {
  return String(s ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

/** Canonical severity ramp (label → color). Order: worst first. These are the
 * dark dev-console ramp values (brighter than print) — kept in sync with the
 * --sev-* tokens in app.css and the map marker paint. */
export const SEVERITY_COLORS = {
  EXTREME: '#e5544e',
  SEVERE: '#e2673f',
  MODERATE: '#d0a24a',
  MINOR: '#6ba15a',
  INFO: '#5aa6e8',
};

/** Source health colors (label → color). In sync with --st-* in app.css. */
export const STATUS_COLORS = {
  OK: '#3ec27f',
  STALE: '#d0a24a',
  UNAVAILABLE: '#e5544e',
};

/**
 * Relative time: "4m ago", "2h ago", "3d ago"; "in 5m" for future stamps.
 * @param {string} iso RFC 3339 timestamp
 * @param {Date=} now  injectable clock for tests
 * @returns {string} "—" for missing/unparseable input
 */
export function timeAgo(iso, now) {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '—';
  const ref = now instanceof Date ? now.getTime() : Date.now();
  let delta = Math.round((ref - t) / 1000); // seconds; positive = past
  const future = delta < 0;
  delta = Math.abs(delta);

  let text;
  if (delta < 60) text = `${delta}s`;
  else if (delta < 3600) text = `${Math.floor(delta / 60)}m`;
  else if (delta < 86400) text = `${Math.floor(delta / 3600)}h`;
  else text = `${Math.floor(delta / 86400)}d`;

  return future ? `in ${text}` : `${text} ago`;
}

/**
 * Absolute time, UTC, compact: "2026-07-05 05:42Z".
 * @param {string} iso RFC 3339 timestamp
 * @returns {string} "—" for missing/unparseable input
 */
export function timeAbs(iso) {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '—';
  const d = new Date(t);
  const p = (n) => String(n).padStart(2, '0');
  return (
    `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ` +
    `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}Z`
  );
}

/**
 * Element showing relative + absolute time together (site rule: timestamps
 * are always both). Relative is primary, absolute alongside and in title.
 * @param {string} iso
 * @returns {HTMLElement}
 */
export function timeCell(iso) {
  const span = document.createElement('span');
  span.className = 'time-cell';
  const rel = document.createElement('span');
  rel.className = 'time-rel';
  rel.textContent = timeAgo(iso);
  const abs = document.createElement('span');
  abs.className = 'time-abs';
  abs.textContent = timeAbs(iso);
  span.title = iso || '';
  span.append(rel, ' ', abs);
  return span;
}

/**
 * Severity chip: <span class="chip sev-SEVERE">SEVERE</span>.
 * Unknown severities render as a neutral chip with the raw label escaped by
 * textContent — never colored as if recognized.
 * @param {string} severity "EXTREME"|"SEVERE"|"MODERATE"|"MINOR"|"INFO"
 * @returns {HTMLElement}
 */
export function sevChip(severity) {
  const label = String(severity ?? '').toUpperCase() || 'UNKNOWN';
  const span = document.createElement('span');
  span.className =
    'chip ' + (SEVERITY_COLORS[label] ? `sev-${label}` : 'sev-unknown');
  span.textContent = label;
  return span;
}

/**
 * Source status indicator: colored dot + text label.
 * OK green, STALE amber, UNAVAILABLE red; anything else neutral.
 * @param {string} status
 * @returns {HTMLElement}
 */
export function sourceDot(status) {
  const label = String(status ?? '').toUpperCase() || 'UNKNOWN';
  const wrap = document.createElement('span');
  wrap.className =
    'dot-status ' + (STATUS_COLORS[label] ? `st-${label}` : 'st-unknown');
  const dot = document.createElement('span');
  dot.className = 'dot';
  dot.setAttribute('aria-hidden', 'true');
  const text = document.createElement('span');
  text.textContent = label;
  wrap.append(dot, text);
  return wrap;
}

/**
 * Decode Event.geometry.geojson: a proto `bytes` field, so protojson carries
 * it as base64. Returns the parsed GeoJSON geometry object, or null if the
 * input is missing or does not decode to JSON.
 * @param {string} b64
 * @returns {Object|null}
 */
export function decodeGeometry(b64) {
  if (!b64 || typeof b64 !== 'string') return null;
  try {
    // Protojson may use URL-safe base64; normalize.
    const std = b64.replaceAll('-', '+').replaceAll('_', '/');
    let bytes;
    if (typeof atob === 'function') {
      const bin = atob(std);
      bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    } else {
      // node fallback for tests
      bytes = Uint8Array.from(Buffer.from(std, 'base64'));
    }
    const text = new TextDecoder('utf-8').decode(bytes);
    const obj = JSON.parse(text);
    return obj && typeof obj === 'object' ? obj : null;
  } catch {
    return null;
  }
}

/**
 * Human label for a Layer enum name or geojson layer slug:
 * "WILDFIRE" / "wildfire" -> "Wildfire"; "WEATHER_ALERT" / "weather_alert"
 * -> "Weather Alert".
 * @param {string} value
 * @returns {string}
 */
export function layerLabel(value) {
  if (!value) return '—';
  return String(value)
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
    .join(' ');
}

/**
 * Format a number with thousands separators; "—" for null/undefined/NaN.
 * @param {number|string|null|undefined} n
 * @param {number=} digits max fraction digits (default 0)
 * @returns {string}
 */
export function fmtNum(n, digits = 0) {
  if (n === null || n === undefined || n === '') return '—';
  const num = Number(n);
  if (Number.isNaN(num)) return '—';
  return num.toLocaleString('en-US', { maximumFractionDigits: digits });
}
