// One list box for the composer's pickers ("@" mentions, "<" macros, "/" commands). It owns the popup, the single
// active row (hover moves the keyboard selection, so two rows are never highlighted), aria-activedescendant on the
// draft, scrolling the active row into view, and the keys. The callers decide which items to offer and what a pick does.
let counter = 0;

export class Picker {
  constructor(draft, className, onPick) {
    this.draft = draft; this.onPick = onPick; this.items = []; this.active = 0;
    this.id = `picker-${++counter}`;
    this.popup = document.createElement('div');
    this.popup.className = `quick-commands ${className}`; this.popup.id = this.id; this.popup.setAttribute('role', 'listbox'); this.popup.hidden = true;
    // The popup is positioned against the form (not the draft's wrapper), so the composer's toolbar and layer do not move it.
    (draft.closest('form') || draft.parentNode).append(this.popup);
  }
  get open() { return !this.popup.hidden; }
  // items: [{ key, preview, className?, render? }]; the first row is active. `render(node)` fills the key element when plain text is not enough.
  show(items) {
    this.items = items; this.active = 0;
    this.popup.replaceChildren(...items.map((item, index) => {
      const option = document.createElement('div'); option.className = `quick-command${item.className ? ` ${item.className}` : ''}`; option.setAttribute('role', 'option'); option.id = `${this.id}-${index}`;
      const key = document.createElement('strong'); if (item.render) item.render(key); else key.textContent = item.key;
      const preview = document.createElement('span'); preview.textContent = item.preview || '';
      option.append(key, preview);
      option.onmousedown = event => { event.preventDefault(); this.onPick(index); };
      option.onmousemove = () => this.setActive(index);
      return option;
    }));
    this.popup.hidden = false;
    this.draft.setAttribute('aria-controls', this.id); this.draft.setAttribute('aria-expanded', 'true');
    this.highlight();
  }
  setActive(index) { if (index !== this.active) { this.active = index; this.highlight(); } }
  highlight() {
    [...this.popup.children].forEach((option, index) => option.setAttribute('aria-selected', String(index === this.active)));
    this.draft.setAttribute('aria-activedescendant', `${this.id}-${this.active}`);
    this.popup.children[this.active]?.scrollIntoView({ block: 'nearest' });
  }
  close() {
    this.popup.hidden = true; this.popup.replaceChildren(); this.items = []; this.active = 0;
    // Several lists share the one draft, and every list closes itself on every keystroke it does not match. Only the list that the draft
    // currently names may clear that naming, or the list that is open would be reported as closed.
    const named = this.draft.getAttribute('aria-controls');
    if (!named || named === this.id) {
      this.draft.removeAttribute('aria-activedescendant'); this.draft.setAttribute('aria-expanded', 'false');
    }
  }
  // Returns true when the key was for the list.
  keydown(event) {
    if (!this.open || event.isComposing) return false;
    if (event.key === 'ArrowDown') { event.preventDefault(); this.setActive((this.active + 1) % this.items.length); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); this.setActive((this.active - 1 + this.items.length) % this.items.length); }
    else if (event.key === 'Enter' || event.key === 'Tab') { event.preventDefault(); this.onPick(this.active); }
    else if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); this.close(); }
    else return false;
    return true;
  }
}
