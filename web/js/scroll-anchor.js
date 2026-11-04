// Keeps the message list stable while content changes height after insertion
// (lazy images, Mermaid diagrams, attachments, panel/composer resizes).
// A list scrolled to the bottom stays pinned to it; a list scrolled up keeps
// the first visible message at the same offset. Ported from v1's scroll anchor;
// v2 renders messages incrementally, so every `.message` is observed on its own
// instead of rebuilding the list.
const BOTTOM_THRESHOLD = 50;
// A scroll event within this long of the reader's last input (or of the scroll event that continued it: a wheel's or a finger's momentum)
// is the reader's.
const READER_WINDOW = 300;
// An input that has not produced a scroll event yet is still the reader's for this long: the page can be too busy to deliver the event within
// READER_WINDOW (a long task, Mermaid on a slow device), and a reader's scroll must not become a "layout clamp" for being handled late.
const PENDING_WINDOW = 1500;
const SCROLL_KEYS = new Set(['PageUp', 'PageDown', ' ', 'ArrowUp', 'ArrowDown', 'Home', 'End']);
// How long after Cmd/Ctrl+F, Cmd/Ctrl+G or F3 a move of the list is taken as the reader's (finding text scrolls to each match).
const FIND_WINDOW = 30000;
// A pinned list that moved far up with the same height as when it was recorded, and whose content has been still for this long, was moved by something
// outside the page's view (assistive technology, an extension): that is the reader's. A layout clamp comes with content changing, and changes the height.
const QUIET_WINDOW = 1000;

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
    this.settledTop = 0; this.settledHeight = 0;
    // When the list's content last changed (rows added or removed, a row resized): see QUIET_WINDOW.
    this.activity = 0;
    // When the reader last acted on the list (wheel, touch, a key, the scrollbar). A pinned list that moves with no reader input has been moved by
    // its layout: content shrank under it and the browser clamped scrollTop (Mermaid and code highlighting swap rows for shorter placeholders for a
    // moment), and the late scroll event reports that clamp after the content grew back. That is not the reader leaving the bottom.
    this.lastInput = -Infinity;
    // The keys that scroll a list are the reader's whether focus is in the list or nowhere (on the body, where the page scrolls with nothing focused).
    // Tab moves focus and the browser scrolls the focused row into view. Find in page (Cmd/Ctrl+F or G, F3, then Enter in the browser's own bar) moves the list with no event the page can see, so for a while after the shortcut
    // every move is the reader's too.
    this.findUntil = 0;
    this.holding = false; this.pendingUntil = 0;
    const input = () => { this.lastInput = performance.now(); this.pendingUntil = this.lastInput + PENDING_WINDOW; };
    for (const type of ['wheel', 'touchstart', 'touchmove', 'touchend', 'focusin']) list.addEventListener(type, input, { passive: true, capture: true });
    // A press on the scrollbar thumb is input for as long as it is held, however long the pause before the drag starts; a move with a button down
    // is the fallback for a press the page did not see.
    list.addEventListener('pointerdown', () => { this.holding = true; input(); }, { passive: true, capture: true });
    list.addEventListener('mousedown', () => { this.holding = true; input(); }, { passive: true, capture: true });
    list.addEventListener('pointermove', event => { if (event.buttons) input(); }, { passive: true, capture: true });
    const release = () => { if (this.holding) { this.holding = false; input(); } };
    for (const type of ['pointerup', 'pointercancel', 'mouseup', 'blur']) window.addEventListener(type, release, { passive: true, capture: true });
    document.addEventListener('keydown', event => {
      if ((event.metaKey || event.ctrlKey) && /^[fg]$/i.test(event.key) || event.key === 'F3') { this.findUntil = performance.now() + FIND_WINDOW; return; }
      const loose = event.target === document.body || event.target === document.documentElement || list.contains(event.target);
      if (loose && (SCROLL_KEYS.has(event.key) || event.key === 'Tab')) input();
    }, { passive: true, capture: true });
    // Last height of every message that has been accounted for. Late growth is found by comparing against
    // it (reconcile) instead of by re-reading the anchor's offset, which an explicit update() between
    // the growth and the resize callback would have re-recorded from the already-grown layout.
    this.heights = new WeakMap();
    this.observer = typeof ResizeObserver === 'function' ? new ResizeObserver(() => this.restore()) : null;
    list.addEventListener('scroll', () => this.onScroll());
    if (this.observer) this.observer.observe(list);
    window.addEventListener('resize', () => this.restore());
    new MutationObserver(records => {
      this.activity = performance.now();
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
  reading() { const now = performance.now(); return this.holding || now - this.lastInput < READER_WINDOW || now < this.findUntil || now < this.pendingUntil; }
  // The scroll event the reader's input caused (however late it is handled) is consumed here; the ones that follow it (momentum) are the reader's while
  // they keep coming within READER_WINDOW of each other.
  claimReader() {
    if (!this.reading()) return false;
    this.pendingUntil = 0; this.lastInput = performance.now();
    return true;
  }
  onScroll() {
    const echo = Math.abs(this.list.scrollTop - this.settledTop) < 1;
    const reading = echo ? this.reading() : this.claimReader();
    trace(this.list, 'scroll', () => ({ echo, settled: this.settledTop, pinned: this.pinned, reading }));
    if (echo) return;
    // Pinned, and nobody scrolled: the layout moved the list. Follow the bottom again instead of reading the clamp as the reader leaving it. Unless the
    // move is the kind no layout makes: far up, at the height the list had when it was recorded, with the content still (QUIET_WINDOW).
    const list = this.list, unexplained = performance.now() - this.activity > QUIET_WINDOW && list.scrollHeight === this.settledHeight && this.settledTop - list.scrollTop > BOTTOM_THRESHOLD;
    if (this.pinned && !reading && !unexplained) { this.restore(); return; }
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
    this.settledTop = this.list.scrollTop; this.settledHeight = this.list.scrollHeight;
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
    this.activity = performance.now();
    // The list is not where it was last recorded, and nothing of ours moved it: a reader scrolled (a key, the wheel) and the scroll event
    // has not been delivered yet, so a pinned list must not be dragged back to the bottom over that. Record where the reader is; the
    // list stays pinned only if that is still near the bottom (a list that shrank is clamped there). A move with no reader input is the
    // layout's (see onScroll) and falls through to the re-pin below.
    if (this.pinned && Math.abs(list.scrollTop - this.settledTop) >= 1 && this.claimReader()) {
      trace(list, 'restore-moved', () => ({ settled: this.settledTop }));
      this.update(true);
      if (!this.pinned) return;
    }
    this.reconcile();
    trace(list, 'restore', () => ({ pinned: this.pinned, settled: this.settledTop, anchor: this.anchorEl?.isConnected ? [...list.querySelectorAll(this.itemSelector)].indexOf(this.anchorEl) : null, offset: Math.round(this.visibleOffset) }));
    if (this.pinned) list.scrollTop = list.scrollHeight;
    else if (this.anchorEl?.isConnected) list.scrollTop += this.anchorEl.getBoundingClientRect().top - list.getBoundingClientRect().top - this.visibleOffset;
    else list.scrollTop = list.scrollHeight - list.clientHeight - this.distanceFromBottom;
    this.settledTop = list.scrollTop; this.settledHeight = list.scrollHeight;
    this.distanceFromBottom = list.scrollHeight - list.scrollTop - list.clientHeight;
    trace(list, 'restored', () => ({ settled: this.settledTop }));
  }
}
