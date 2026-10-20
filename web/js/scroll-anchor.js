// Keeps the message list stable while content changes height after insertion
// (lazy images, Mermaid diagrams, attachments, panel/composer resizes).
// A list scrolled to the bottom stays pinned to it; a list scrolled up keeps
// the first visible message at the same offset. Ported from v1's scroll anchor;
// v2 renders messages incrementally, so every `.message` is observed on its own
// instead of rebuilding the list.
const BOTTOM_THRESHOLD = 50;

// Decision log for diagnosing a flaky UI test: when a page sets `window.__anchorLog = []` every decision below is
// appended to it (position, state, what was found and applied). `info` is a thunk, so nothing is measured or built
// unless a log is installed: production pays one global lookup per event and no layout reads.
const trace = (list, ev, info) => {
  const log = globalThis.__anchorLog;
  if (!Array.isArray(log)) return;
  if (log.length >= 400) log.shift();
  log.push({ t: Math.round(performance.now()), ev, st: Math.round(list.scrollTop * 10) / 10, sh: list.scrollHeight, ch: list.clientHeight, ...info() });
};

export class ScrollAnchor {
  constructor(list, itemSelector = '.message') {
    this.list = list; this.itemSelector = itemSelector;
    this.pinned = true; this.anchorEl = null; this.visibleOffset = 0; this.distanceFromBottom = 0;
    // Where the list was when pinned/anchor state was last recorded. A scroll event that reports this
    // same position is only the echo of a write we made (restore() or chat.js), not a reader scrolling.
    this.settledTop = 0;
    // Last height of every message that has been accounted for. Late growth is found by comparing against
    // it (reconcile) instead of by re-reading the anchor's offset, which an explicit update() between
    // the growth and the resize callback would have re-recorded from the already-grown layout.
    this.heights = new WeakMap();
    this.observer = typeof ResizeObserver === 'function' ? new ResizeObserver(() => this.restore()) : null;
    list.addEventListener('scroll', () => this.onScroll());
    if (this.observer) this.observer.observe(list);
    window.addEventListener('resize', () => this.restore());
    new MutationObserver(records => {
      trace(list, 'mutation', () => ({ added: records.reduce((n, r) => n + r.addedNodes.length, 0), removed: records.reduce((n, r) => n + r.removedNodes.length, 0), settled: this.settledTop, pinned: this.pinned }));
      for (const record of records) {
        for (const node of record.addedNodes) this.watch(node);
        for (const node of record.removedNodes) this.unwatch(node);
      }
      this.baseline();
    }).observe(list, { childList: true });
    this.watchAll(); this.baseline();
  }
  watch(node) { if (this.observer && node.nodeType === 1 && node.matches(this.itemSelector)) this.observer.observe(node); }
  unwatch(node) { if (this.observer && node.nodeType === 1) this.observer.unobserve(node); }
  watchAll() { for (const node of this.list.children) this.watch(node); }
  // Give every message its starting height as soon as it is inserted (this runs in a microtask, before any
  // frame). Without it a message's first height would only be recorded by the first resize callback, a frame
  // later, and an image that finished loading before that frame would look like a new arrival and its growth
  // above the reader would not be compensated. A busy machine delays that first frame by hundreds of ms.
  baseline() {
    for (const item of this.list.children) if (item.nodeType === 1 && item.matches(this.itemSelector) && !this.heights.has(item)) this.heights.set(item, item.offsetHeight);
  }
  // Forget the old room: pinned again, observers re-attached to the new list contents.
  reset() {
    trace(this.list, 'reset', () => ({}));
    this.pinned = true; this.anchorEl = null; this.visibleOffset = 0; this.distanceFromBottom = 0; this.heights = new WeakMap();
    if (this.observer) { this.observer.disconnect(); this.observer.observe(this.list); this.watchAll(); this.baseline(); }
  }
  // Scroll events are delivered a frame after the write that caused them. Late content (an image or a
  // diagram) can grow the list in between, and measuring then would read that growth as the reader
  // scrolling away from the bottom, un-pinning a list that should have stayed pinned.
  onScroll() {
    const echo = Math.abs(this.list.scrollTop - this.settledTop) < 1;
    trace(this.list, 'scroll', () => ({ echo, settled: this.settledTop, pinned: this.pinned }));
    if (echo) return;
    this.update(true);
  }
  // Compare every message's height with the last one accounted for. For a scrolled-up list with a
  // recorded anchor, scroll by the growth of the messages above that anchor, so what the reader looks
  // at stays put. Returns whether any height changed. Messages seen for the first time (new arrivals, or
  // older pages the chat compensates itself) have no previous height and cause no scrolling.
  reconcile() {
    const list = this.list, anchor = this.pinned || !this.anchorEl?.isConnected ? null : this.anchorEl;
    let delta = 0, changed = false, above = anchor !== null, index = -1;
    const grown = globalThis.__anchorLog ? [] : null;
    const items = list.querySelectorAll(this.itemSelector);
    for (const item of items) {
      index++;
      if (item === anchor) above = false;
      const height = item.offsetHeight, known = this.heights.get(item);
      if (known !== undefined && height !== known) { changed = true; grown?.push([index, known, height, above]); if (above) delta += height - known; }
      this.heights.set(item, height);
    }
    if (grown) trace(list, 'reconcile', () => ({ settled: this.settledTop, pinned: this.pinned, anchor: anchor ? [...items].indexOf(anchor) : null, delta, grown, items: items.length }));
    if (delta) list.scrollTop += delta;
    return changed;
  }
  nearBottom() { return this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < BOTTOM_THRESHOLD; }
  // Record where the reader is. Anything that grew since the last record is reconciled first, so an
  // update() that lands between late content growing and the resize callback cannot absorb the growth.
  // A pinned list whose content merely grew stays pinned, unless the update reports the reader scrolling or
  // something already moved the list away from where it was last recorded.
  update(fromReader = false) {
    const wasPinned = this.pinned, grew = this.reconcile();
    // Only when nothing moved the list since the last record: a programmatic jump (scrollIntoView, then update())
    // is a deliberate move away from the bottom and must not be undone.
    if (wasPinned && grew && !fromReader && Math.abs(this.list.scrollTop - this.settledTop) < 1) this.list.scrollTop = this.list.scrollHeight;
    this.settledTop = this.list.scrollTop;
    this.distanceFromBottom = this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight;
    this.pinned = this.distanceFromBottom < BOTTOM_THRESHOLD;
    this.anchorEl = null; this.visibleOffset = 0;
    if (this.pinned) { trace(this.list, 'update', () => ({ fromReader, wasPinned, grew, pinned: true })); return; }
    const top = this.list.getBoundingClientRect().top;
    for (const item of this.list.querySelectorAll(this.itemSelector)) {
      const rect = item.getBoundingClientRect();
      // Strictly below the top edge: a message that only touches it shows nothing, so its growth is above the reader.
      if (rect.bottom > top + 0.5) {
        this.anchorEl = item; this.visibleOffset = rect.top - top;
        trace(this.list, 'update', () => ({ fromReader, wasPinned, grew, pinned: false, anchor: [...this.list.querySelectorAll(this.itemSelector)].indexOf(item), offset: Math.round(this.visibleOffset) }));
        return;
      }
    }
    trace(this.list, 'update', () => ({ fromReader, wasPinned, grew, pinned: false, anchor: null }));
  }
  restore() {
    const list = this.list;
    this.reconcile();
    trace(list, 'restore', () => ({ pinned: this.pinned, settled: this.settledTop, anchor: this.anchorEl?.isConnected ? [...list.querySelectorAll(this.itemSelector)].indexOf(this.anchorEl) : null, offset: Math.round(this.visibleOffset) }));
    if (this.pinned) list.scrollTop = list.scrollHeight;
    else if (this.anchorEl?.isConnected) list.scrollTop += this.anchorEl.getBoundingClientRect().top - list.getBoundingClientRect().top - this.visibleOffset;
    else list.scrollTop = list.scrollHeight - list.clientHeight - this.distanceFromBottom;
    this.settledTop = list.scrollTop;
    this.distanceFromBottom = list.scrollHeight - list.scrollTop - list.clientHeight;
    trace(list, 'restored', () => ({ settled: this.settledTop }));
  }
}
