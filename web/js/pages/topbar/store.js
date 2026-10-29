import { batch, signal } from '../../ui.js';
import { nextIndex, visibleIndexes } from './model.js';

// The overflow menu: whether it is open, the items (each mirroring its real button's hidden state and badge), the health mark and which
// item takes focus. The host feeds it what the real buttons show and does the clicks that leave this page.
export class TopbarStore {
  constructor({ items, click = () => {} }) {
    this.items = signal(items.map(item => ({ label: item.label, hidden: false, badge: item.badge ? { text: '', hidden: true } : null })));
    this.open = signal(false);
    this.issues = signal(false);
    this.focus = signal({ n: 0, index: -1 });
    this.click = click; this.flush = () => {};
  }

  set(change) { batch(change); this.flush(); }

  focusOn(index) { this.focus.value = { n: this.focus.value.n + 1, index }; }

  setOpen(open) {
    this.set(() => { this.open.value = open; if (open) this.focusOn(visibleIndexes(this.items.value)[0] ?? -1); });
  }

  setHidden(index, hidden) { this.items.value = this.items.value.map((item, at) => (at === index ? { ...item, hidden } : item)); }

  setBadge(index, text, hidden) { this.items.value = this.items.value.map((item, at) => (at === index ? { ...item, badge: { text, hidden } } : item)); }

  setIssues(issues) { this.issues.value = issues; }

  move(key, from) {
    const visible = visibleIndexes(this.items.value), next = nextIndex(visible.length, visible.indexOf(from), key);
    if (next >= 0) this.set(() => this.focusOn(visible[next]));
  }

  activate(index) { this.setOpen(false); this.click(index); }
}
