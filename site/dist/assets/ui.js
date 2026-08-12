// ui.js — the small shared primitives every island reached for and re-wrote.
//
// Before this existed there were four hand-rolled chip builders, an `el()` in
// every island and an `errorBlock()` in five of them. They drifted: the chips
// on Events grew an aria-pressed the ones on Map did not have, and two of the
// error blocks stopped showing the request URL.
//
// No DOM access at import time — node can import and test the pure parts.

/**
 * Create an element. The one function every island defined for itself.
 * @param {string} tag
 * @param {string=} className
 * @param {string=} text  set via textContent — upstream text is untrusted
 * @returns {HTMLElement}
 */
export function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/**
 * Resolve a page's elements by id, and FAIL LOUDLY AND BY NAME if one is gone.
 *
 * Islands wire themselves to markup through id strings, so renaming an id in a
 * `.astro` file used to surface as `Cannot read properties of null (reading
 * 'addEventListener')` at some line in the middle of `init()` — with the page
 * half-built, no clue which id was at fault, and the error thrown far from the
 * edit that caused it.
 *
 * This turns that into "events.js: markup is missing #ev-edit" before anything
 * has been wired. It cannot prevent the mismatch, but it names it, and it fails
 * before the page is left in a partial state.
 *
 * @param {string} who      island name, for the message
 * @param {Object<string,string>} map  key → element id
 * @returns {Object<string,HTMLElement>}
 */
export function requireEls(who, map) {
  const out = {};
  const missing = [];
  for (const [key, id] of Object.entries(map)) {
    const node = document.getElementById(id);
    if (!node) missing.push(`#${id} (${key})`);
    out[key] = node;
  }
  if (missing.length) {
    throw new Error(
      `${who}: markup is missing ${missing.join(', ')}. ` +
        'The island and its .astro page have drifted — rebuild with `make site`, ' +
        'and if that does not fix it the id was renamed on one side only.'
    );
  }
  return out;
}

/**
 * The shared fail-loud error block: what failed, and the exact request that
 * failed, so a reader can replay it.
 * @param {Error|{status?:number,url?:string,body?:any,message?:string}} err
 * @param {string=} lead  override the first line
 * @returns {HTMLElement}
 */
export function errorBlock(err, lead) {
  const div = el('div', 'error-block');
  const status = err && err.status;
  div.append(
    el('div', '', lead || (status !== undefined
      ? `Request failed (HTTP ${status || 'network error'}):`
      : 'Request failed:'))
  );
  div.append(
    el('div', 'error-url', err && err.url ? `GET ${err.url}` : String((err && err.message) || err))
  );
  if (err && err.body && typeof err.body === 'object' && err.body.message) {
    div.append(el('div', 'muted', err.body.message));
  }
  return div;
}

/* --- Copying -----------------------------------------------------------
 *
 * Two shapes, one behaviour. `copyOnClick` makes a VALUE copyable in place (an
 * event id in a record row); `copyButton` is a standalone control beside
 * something (a request line, a .geojson URL).
 *
 * Both signal the result with an icon that swaps to a tick, and neither
 * disturbs what is on screen. The id form used to replace the id's text with
 * "copied id" for 1.2s — which hid the very thing you had just copied, and
 * because "copied id" is far shorter than a seventy-character id, the line
 * collapsed and everything after it jumped. The feedback was good; destroying
 * the content to deliver it was not.
 *
 * The icons are inline SVG, not glyphs: ✓ and ⧉ are not in the vendored Plex
 * faces, so they would fall back to whatever the OS has and render at a
 * different weight and size on every machine. */

const ICON_COPY =
  '<svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" ' +
  'stroke-width="1.5" aria-hidden="true"><rect x="5.75" y="5.75" width="8.5" height="9.5"/>' +
  '<path d="M11 3.25H1.75v9.5"/></svg>';
const ICON_TICK =
  '<svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" ' +
  'stroke-width="2" stroke-linecap="square" aria-hidden="true"><path d="M2.5 8.5l3.5 3.5L13.5 4"/></svg>';

/**
 * Write to the clipboard and report whether it worked. Never throws.
 * @param {string} value
 * @returns {Promise<boolean>}
 */
function writeClipboard(value) {
  if (!navigator.clipboard) return Promise.resolve(false);
  return navigator.clipboard.writeText(value).then(() => true, () => false);
}

/**
 * Turn an element into a click-to-copy control for a long value.
 *
 * Event ids run to seventy characters —
 * `meshcore:3811ef9726fbc987f4cdd0065b971a51db1fe578e9570f2c6c888c89c6ef10af` —
 * and a reader wants one of two things from them: to see WHICH record this is
 * (the namespace and a few characters are enough) or to have the whole thing in
 * the clipboard to paste into a URL. Neither is served by wrapping seventy
 * characters across three lines. So the display truncates with CSS and the
 * click hands over the full value.
 *
 * Inside a record row the id sits within the row's link, so the click is
 * stopped here — clicking the id copies it and does NOT open the record, which
 * is the whole point of clicking the id specifically.
 *
 * @param {HTMLElement} node   the element showing the (possibly clipped) value
 * @param {string} value       the full value to copy
 * @param {string=} what       noun for the tooltip, e.g. 'id'
 */
export function copyOnClick(node, value, what = 'id') {
  if (!node || !value) return node;
  node.classList.add('copyable');
  node.title = `${value}\n\nclick to copy this ${what}`;
  node.setAttribute('role', 'button');
  node.tabIndex = 0;

  // Wrap the value so the mark is a SIBLING of it, not part of it.
  //
  // Appending the mark straight onto the node put it after the value's full
  // text — and on a clipped, nowrap line (a record row's id) the icon landed
  // 150px past the visible end, pushing the document to 555px at a 390px
  // viewport. A phone scrolled sideways because of a 13px icon. As a flex
  // sibling the value truncates and the mark stays on screen, which is also
  // what you want: the affordance should not disappear with the text.
  const valueEl = el('span', 'copy-value');
  while (node.firstChild) valueEl.appendChild(node.firstChild);

  const mark = el('span', 'copy-mark');
  mark.innerHTML = ICON_COPY;
  node.append(valueEl, mark);

  // Announced instead of the visual swap: an aria-hidden icon says nothing.
  const said = el('span', 'sr-only');
  said.setAttribute('aria-live', 'polite');
  node.append(said);
  // sr-only is position:absolute, so it adds no width — but it must not be
  // inside the clipped value either, or the announcement gets clipped too.

  let resetTimer = null;
  const done = (ok) => {
    mark.innerHTML = ok ? ICON_TICK : ICON_COPY;
    mark.classList.toggle('ok', ok);
    mark.classList.toggle('failed', !ok);
    said.textContent = ok ? `${what} copied` : `could not copy the ${what}`;
    clearTimeout(resetTimer);
    resetTimer = setTimeout(() => {
      mark.innerHTML = ICON_COPY;
      mark.classList.remove('ok', 'failed');
      said.textContent = '';
    }, 1400);
  };

  const copy = (e) => {
    // The id lives inside the row's <a>; copying must not also navigate.
    e.preventDefault();
    e.stopPropagation();
    writeClipboard(value).then(done);
  };

  node.addEventListener('click', copy);
  node.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') copy(e);
  });
  return node;
}

/**
 * Give an existing `.copy-btn` the shared behaviour: an icon that becomes a
 * tick, and a label that never moves.
 *
 * Six islands had written this button — Map, Roads, Docs, the front page, the
 * request drawer and the event detail pane — and every one of them swapped its
 * own label to "copied" for a beat. "copy curl" and "copied" are different
 * widths, so the button resized and nudged whatever sat beside it, on a click
 * whose entire job was to change nothing on screen.
 *
 * @param {HTMLButtonElement} btn
 * @param {string|function(): string} value  the clipboard payload; pass a
 *   function when it depends on live state (the Events URL tracks the filters)
 * @returns {HTMLButtonElement}
 */
export function wireCopyButton(btn, value) {
  if (!btn) return btn;
  btn.type = 'button';

  const mark = el('span', 'copy-mark');
  mark.innerHTML = ICON_COPY;
  btn.prepend(mark);

  let resetTimer = null;
  btn.addEventListener('click', () => {
    writeClipboard(typeof value === 'function' ? value() : value).then((ok) => {
      mark.innerHTML = ok ? ICON_TICK : ICON_COPY;
      mark.classList.toggle('ok', ok);
      mark.classList.toggle('failed', !ok);
      clearTimeout(resetTimer);
      resetTimer = setTimeout(() => {
        mark.innerHTML = ICON_COPY;
        mark.classList.remove('ok', 'failed');
      }, 1400);
    });
  });
  return btn;
}

/**
 * A standalone copy control, built and wired.
 * @param {string|function(): string} value
 * @param {string=} label   the button's text, e.g. 'copy curl'
 * @returns {HTMLButtonElement}
 */
export function copyButton(value, label = 'copy') {
  const btn = el('button', 'copy-btn');
  btn.append(el('span', '', label));
  return wireCopyButton(btn, value);
}
