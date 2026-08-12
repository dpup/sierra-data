// <grid-chip-row> — a row of toggle chips, as a custom element.
//
// This replaced four near-identical hand-rolled implementations (the Events
// facets, the Map layer group, the History layer group, the Places kind
// filter). They had already drifted apart in the ways duplicated UI always
// does: two set `aria-pressed` and two did not, one forgot `type="button"`
// inside a form, and the single-select variants each re-derived "clear the
// others" slightly differently.
//
// LIGHT DOM, DELIBERATELY. No shadow root: the whole design system is custom
// properties inherited through the tree (`--sev-*` re-pointed by `.on-ink`,
// the measure and rhythm tokens, `.chip-toggle` itself). A shadow boundary
// would cut every one of those off, and the fix would be to copy tokens into
// the component — which is the drift this element exists to end.
//
// Usage — markup carries the shape, JS carries the data:
//
//   <grid-chip-row id="ev-layers" mode="multi" exclusive="" aria-label="layer">
//
//   row.options = [{ value: 'wildfire', label: 'wildfire' }, …];
//   row.value   = ['wildfire'];              // string[] multi, string single
//   row.addEventListener('change', (e) => …) // e.detail.value
//
// `exclusive` names a value that clears the rest and lights when nothing else
// is selected — the `all` chip, so a multi-select row never reads as though
// nothing is chosen.

// Islands that import this module must stay importable in node (the codebase
// keeps its pure helpers node-testable), and `class X extends HTMLElement` is
// evaluated at module load — which throws outside a browser. Extending a stub
// off-DOM keeps the import side-effect-free there; customElements.define below
// is likewise guarded.
const Base = typeof HTMLElement === 'undefined' ? class {} : HTMLElement;

export class GridChipRow extends Base {
  static observedAttributes = ['mode', 'exclusive'];

  constructor() {
    super();
    /** @type {Array<{value: string, label: string}>} */
    this._options = [];
    /** @type {string[]} */
    this._value = [];
  }

  connectedCallback() {
    if (!this.hasAttribute('role')) this.setAttribute('role', 'group');
    this._render();
  }

  attributeChangedCallback() {
    if (this.isConnected) this._render();
  }

  /** 'multi' (default) or 'single'. */
  get mode() {
    return this.getAttribute('mode') === 'single' ? 'single' : 'multi';
  }

  /** The value that clears the others, or null. Only meaningful when multi. */
  get exclusive() {
    return this.hasAttribute('exclusive') ? this.getAttribute('exclusive') || '' : null;
  }

  get options() {
    return this._options;
  }
  set options(list) {
    this._options = Array.isArray(list) ? list : [];
    this._render();
  }

  /** string[] in multi mode, string in single mode. */
  get value() {
    return this.mode === 'single' ? this._value[0] ?? '' : [...this._value];
  }
  set value(v) {
    this._value = this.mode === 'single'
      ? [v == null ? '' : String(v)]
      : (Array.isArray(v) ? v.map(String) : []);
    this._paint();
  }

  _render() {
    this.textContent = '';
    for (const opt of this._options) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'chip-toggle';
      b.dataset.value = opt.value;
      b.textContent = opt.label;
      b.addEventListener('click', () => this._toggle(opt.value));
      this.append(b);
    }
    this._paint();
  }

  _toggle(value) {
    const ex = this.exclusive;
    if (this.mode === 'single') {
      this._value = [value];
    } else if (ex !== null && value === ex) {
      this._value = [];
    } else {
      this._value = this._value.includes(value)
        ? this._value.filter((v) => v !== value)
        : [...this._value, value];
    }
    this._paint();
    this.dispatchEvent(new CustomEvent('change', {
      detail: { value: this.value },
      bubbles: true,
    }));
  }

  _paint() {
    const ex = this.exclusive;
    for (const b of this.querySelectorAll('.chip-toggle')) {
      const v = b.dataset.value;
      // The exclusive chip lights when nothing else is selected, so the row
      // always shows exactly one lit chip rather than reading as "nothing set".
      const on = ex !== null && v === ex ? this._value.length === 0 : this._value.includes(v);
      b.classList.toggle('on', on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    }
  }
}

if (typeof customElements !== 'undefined' && !customElements.get('grid-chip-row')) {
  customElements.define('grid-chip-row', GridChipRow);
}
