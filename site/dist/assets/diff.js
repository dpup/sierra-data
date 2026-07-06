// diff.js — pure field-level diff of protojson objects (plain JSON only).
//
// Used by the event detail page to compute the revision timeline's
// field-level diff between consecutive revisions, entirely client-side
// (site spec §2 /events/{id}; the API has no diff support by design).
//
// Zero DOM access — node can import and test this module directly.

/** True for a plain JSON object (not null, not an array). */
function isPlainObject(v) {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** "a" + "b" -> "a.b"; "" + "b" -> "b". Array indices append as "[i]". */
function joinPath(path, key) {
  return path ? `${path}.${key}` : String(key);
}

/**
 * Deep-diff two plain-JSON values (objects / arrays / primitives).
 *
 * Semantics:
 * - Objects recurse key-by-key; a key present on only one side yields a
 *   single `added`/`removed` entry whose value is the whole subtree.
 * - Arrays are compared by index; a length change yields `added`/`removed`
 *   entries for the trailing indices.
 * - Primitives (and type mismatches, e.g. object -> string) yield one
 *   `changed` entry with the full before/after values.
 * - Strings are opaque: the base64 `geometry.geojson` bytes field diffs as
 *   a single `changed` entry, never decoded or recursed into.
 *
 * Entry order is stable: `a`'s key order first, then keys only in `b`.
 *
 * @param {*} a the "before" value (older revision's protojson)
 * @param {*} b the "after" value (newer revision's protojson)
 * @returns {Array<{path: string, before: *, after: *, kind: 'added'|'removed'|'changed'}>}
 *   Empty array when the values are deeply equal. `before` is undefined for
 *   `added` entries; `after` is undefined for `removed` entries. Paths are
 *   dotted with bracketed indices, e.g. "geometry.bbox.min_lat", "place_ids[2]".
 */
export function diffObjects(a, b) {
  const out = [];
  walk(a, b, '', out);
  return out;
}

function walk(a, b, path, out) {
  if (a === b) return;

  if (isPlainObject(a) && isPlainObject(b)) {
    const keys = Object.keys(a);
    for (const k of Object.keys(b)) {
      if (!Object.prototype.hasOwnProperty.call(a, k)) keys.push(k);
    }
    for (const k of keys) {
      const p = joinPath(path, k);
      const inA = Object.prototype.hasOwnProperty.call(a, k);
      const inB = Object.prototype.hasOwnProperty.call(b, k);
      if (!inA) out.push({ path: p, before: undefined, after: b[k], kind: 'added' });
      else if (!inB) out.push({ path: p, before: a[k], after: undefined, kind: 'removed' });
      else walk(a[k], b[k], p, out);
    }
    return;
  }

  if (Array.isArray(a) && Array.isArray(b)) {
    const len = Math.max(a.length, b.length);
    for (let i = 0; i < len; i++) {
      const p = `${path}[${i}]`;
      if (i >= a.length) out.push({ path: p, before: undefined, after: b[i], kind: 'added' });
      else if (i >= b.length) out.push({ path: p, before: a[i], after: undefined, kind: 'removed' });
      else walk(a[i], b[i], p, out);
    }
    return;
  }

  // Primitives, nulls, or a type mismatch (object vs array vs scalar):
  // one `changed` entry carrying both sides whole.
  out.push({ path, before: a, after: b, kind: 'changed' });
}
