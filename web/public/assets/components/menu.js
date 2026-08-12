// <grid-menu> — a trigger and a paper dropdown, as a custom element.
//
// Replaces the native <select>, which cannot be styled to the broadsheet and
// renders as OS chrome in the middle of a paper page. Every place picker on the
// site is one of these — Events, Map, History and Roads — plus History's
// page_size. Its styles live in app.css (a shared element's CSS belongs with
// the design system, not in the page it was born on).
//
// Replacing a <select> means re-writing by hand everything the native control
// gave away for free, which is why this is a component and not two copies:
// Enter/Space/ArrowDown to open, arrows to move, Enter to choose, Escape to
// close and return focus to the trigger, click-away to dismiss, and
// aria-expanded / role=listbox / aria-selected kept in step with the visuals.
//
// LIGHT DOM, deliberately — see the note in chip-row.js. The panel inherits the
// page's tokens, and `position: absolute` needs the page's own stacking
// context, not one sealed inside a shadow root.
//
// Usage:
//   <grid-menu id="ev-place" trigger-label="[change ▾]" aria-label="Scope"></grid-menu>
//   menu.options = [{ value: '', label: 'All places' },
//                   { group: 'AREA' },                    // a heading
//                   { value: 'ebbetts-pass', label: 'Ebbetts Pass' }];
//   menu.value = 'ebbetts-pass';
//   menu.addEventListener('change', (e) => …)  // e.detail.value

// Islands that import this module must stay importable in node (the codebase
// keeps its pure helpers node-testable), and `class X extends HTMLElement` is
// evaluated at module load — which throws outside a browser. Extending a stub
// off-DOM keeps the import side-effect-free there; customElements.define below
// is likewise guarded.
const Base = typeof HTMLElement === 'undefined' ? class {} : HTMLElement;

export class GridMenu extends Base {
  constructor() {
    super();
    this._options = [];
    this._value = '';
    this._focused = -1;
    this._onDocClick = (e) => {
      if (!this.open) return;
      if (!this.contains(e.target)) this.close();
    };
  }

  connectedCallback() {
    this.classList.add('menu-wrap');

    this._trigger = document.createElement('button');
    this._trigger.type = 'button';
    this._trigger.className = 'menu-trigger';
    this._trigger.textContent = this.getAttribute('trigger-label') || 'choose';
    this._trigger.setAttribute('aria-haspopup', 'listbox');
    this._trigger.setAttribute('aria-expanded', 'false');

    this._panel = document.createElement('div');
    this._panel.className = 'menu-panel';
    this._panel.setAttribute('role', 'listbox');
    this._panel.setAttribute('tabindex', '-1');
    this._panel.hidden = true;
    const label = this.getAttribute('aria-label');
    if (label) this._panel.setAttribute('aria-label', label);

    this._trigger.addEventListener('click', (e) => {
      e.stopPropagation();
      this.open ? this.close() : this.show();
    });
    this._trigger.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        this.show();
      }
    });
    this._panel.addEventListener('keydown', (e) => this._onKey(e));
    this._panel.addEventListener('click', (e) => {
      const item = e.target.closest('.menu-item');
      if (!item) return;
      this._pick(item.dataset.value);
    });
    document.addEventListener('click', this._onDocClick);

    this.append(this._trigger, this._panel);
    this._render();
  }

  disconnectedCallback() {
    document.removeEventListener('click', this._onDocClick);
  }

  get open() {
    return this._panel && !this._panel.hidden;
  }

  get options() {
    return this._options;
  }
  set options(list) {
    this._options = Array.isArray(list) ? list : [];
    this._render();
  }

  get value() {
    return this._value;
  }
  set value(v) {
    this._value = v == null ? '' : String(v);
    this._render();
  }

  /** Set the trigger's text (callers show the current selection there). */
  set triggerLabel(text) {
    if (this._trigger) this._trigger.textContent = text;
  }

  show() {
    if (!this._panel) return;
    this._panel.hidden = false;
    this._trigger.setAttribute('aria-expanded', 'true');
    const items = this._items();
    const sel = items.findIndex((i) => i.getAttribute('aria-selected') === 'true');
    this._focus(sel === -1 ? 0 : sel);
  }

  close() {
    if (!this._panel) return;
    this._panel.hidden = true;
    this._trigger.setAttribute('aria-expanded', 'false');
    this._focused = -1;
  }

  _items() {
    return [...this._panel.querySelectorAll('.menu-item')];
  }

  _focus(i) {
    const items = this._items();
    if (!items.length) return;
    this._focused = Math.max(0, Math.min(items.length - 1, i));
    for (const [n, it] of items.entries()) it.classList.toggle('focused', n === this._focused);
    items[this._focused].scrollIntoView({ block: 'nearest' });
  }

  _onKey(e) {
    if (e.key === 'Escape') {
      e.preventDefault();
      this.close();
      this._trigger.focus();
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      this._focus(this._focused + 1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      this._focus(this._focused - 1);
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      const item = this._items()[this._focused];
      if (item) this._pick(item.dataset.value);
    }
  }

  _pick(value) {
    this._value = value;
    this._render();
    this.close();
    this._trigger.focus();
    this.dispatchEvent(new CustomEvent('change', {
      detail: { value },
      bubbles: true,
    }));
  }

  _render() {
    if (!this._panel) return;
    this._panel.textContent = '';
    for (const opt of this._options) {
      if (opt.group !== undefined) {
        const g = document.createElement('div');
        g.className = 'menu-group';
        g.textContent = opt.group;
        this._panel.append(g);
        continue;
      }
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'menu-item';
      b.dataset.value = opt.value;
      b.textContent = opt.label;
      b.setAttribute('role', 'option');
      b.setAttribute('aria-selected', opt.value === this._value ? 'true' : 'false');
      this._panel.append(b);
    }
  }
}

if (typeof customElements !== 'undefined' && !customElements.get('grid-menu')) {
  customElements.define('grid-menu', GridMenu);
}
