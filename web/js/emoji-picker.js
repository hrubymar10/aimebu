import { CATEGORIES, SKIN_TONES, loadEmoji, matchedAlias, saveTone, savedTone, searchEmoji, withTone } from './emoji-data.js';
// The Slack-style emoji picker: category tabs, a search box that is focused on open, a grid, a footer that previews the
// hovered emoji and a skin-tone selector. One instance, made on the first open and kept for the next ones, anchored to the control
// that opened it (a bottom sheet on a phone). `onPick(glyph, entry)` runs after the picker has closed.
//
// The grid is virtual: the database is about 1,900 emoji, so drawing them all took about 3,800 DOM nodes, half the size of a whole
// chat page, on every open. Every row has a fixed height, a row is a heading or a line of cells, and only the rows in view (plus a few
// either side) exist. Tabs and search work on the list of entries, never on the DOM, and between opens the picker has no cells at all.
const PHONE = '(max-width: 700px)';
const SEARCH_LIMIT = 200;
const OVERSCAN = 3;
let picker = null;
let current = null;

function el(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text != null) node.textContent = text; return node; }

// The entries of each category, once per database.
const grouped = new WeakMap();
function groupsOf(db) {
  if (!grouped.has(db)) grouped.set(db, CATEGORIES.map(category => ({ key: category.key, label: category.label, entries: db.entries.filter(entry => entry.group === category.key) })));
  return grouped.get(db);
}

export function emojiPickerOpen() { return !!current; }
export function closeEmojiPicker(restoreFocus = true) { current?.close(restoreFocus); }
// How many cell buttons are in the page right now (the tests hold this to a bound).
export function emojiPickerCellCount() { return picker?.live.size ? [...picker.live.values()].reduce((sum, row) => sum + row.childElementCount, 0) : 0; }

export function openEmojiPicker({ anchor, onPick }) {
  const wasAnchor = current?.anchor === anchor;
  closeEmojiPicker(false);
  if (wasAnchor) return;
  picker ||= new EmojiPicker();
  picker.open(anchor, onPick);
}

class EmojiPicker {
  constructor() {
    this.entries = []; this.rows = []; this.blocks = []; this.live = new Map(); this.db = null; this.focused = 0; this.query = ''; this.cols = 1; this.frame = 0;
    this.scrim = el('div', 'emoji-picker-scrim'); this.scrim.onclick = () => this.close();
    const root = this.root = el('div', 'emoji-picker'); root.setAttribute('role', 'dialog'); root.setAttribute('aria-label', 'Emoji picker');
    this.search = el('input', 'emoji-search'); this.search.type = 'search'; this.search.placeholder = 'Search emoji'; this.search.setAttribute('aria-label', 'Search emoji'); this.search.autocomplete = 'off'; this.search.spellcheck = false;
    this.search.oninput = () => this.layout(true);
    this.tabs = el('div', 'emoji-tabs'); this.tabs.setAttribute('role', 'group'); this.tabs.setAttribute('aria-label', 'Emoji categories');
    this.tabButtons = new Map();
    for (const category of CATEGORIES) {
      const tab = el('button', 'row-btn emoji-tab', category.icon); tab.type = 'button'; tab.title = category.label; tab.setAttribute('aria-label', category.label);
      tab.onclick = () => this.jump(category.key); this.tabButtons.set(category.key, tab); this.tabs.append(tab);
    }
    this.grid = el('div', 'emoji-grid'); this.grid.setAttribute('role', 'grid'); this.grid.setAttribute('aria-label', 'Emoji');
    this.canvas = el('div', 'emoji-canvas'); this.grid.append(this.canvas);
    this.grid.onscroll = () => this.scheduled();
    // The cells are made and removed as the grid scrolls, so their events are delegated: mouseenter does not bubble, hence the capture phase.
    this.grid.addEventListener('click', event => { const cell = event.target.closest?.('.emoji-cell'); if (cell) this.pick(this.entries[Number(cell.dataset.i)]); });
    this.grid.addEventListener('mouseenter', event => { const cell = event.target.closest?.('.emoji-cell'); if (cell) this.previewEntry(this.entries[Number(cell.dataset.i)]); }, true);
    this.grid.addEventListener('focusin', event => { const cell = event.target.closest?.('.emoji-cell'); if (cell) this.previewEntry(this.entries[Number(cell.dataset.i)]); });
    this.status = el('div', 'sr-only'); this.status.setAttribute('role', 'status');
    this.preview = el('div', 'emoji-preview'); this.previewGlyph = el('span', 'emoji-preview-glyph'); this.previewGlyph.setAttribute('aria-hidden', 'true');
    this.previewName = el('strong', 'emoji-preview-name'); this.previewCode = el('span', 'emoji-preview-code');
    const text = el('span', 'emoji-preview-text'); text.append(this.previewName, this.previewCode); this.preview.append(this.previewGlyph, text);
    this.toneRow = el('div', 'emoji-tones'); this.toneRow.setAttribute('role', 'group'); this.toneRow.setAttribute('aria-label', 'Skin tone');
    this.toneButtons = SKIN_TONES.map(tone => {
      const button = el('button', 'row-btn emoji-tone', tone.swatch); button.type = 'button'; button.title = `${tone.label} skin tone`; button.setAttribute('aria-label', `${tone.label} skin tone`);
      button.onclick = () => this.setTone(tone.key); this.toneRow.append(button); return button;
    });
    const footer = el('div', 'emoji-footer'); footer.append(this.preview, this.toneRow);
    root.append(this.search, this.tabs, this.grid, this.status, footer);
    root.addEventListener('keydown', event => this.keydown(event));
  }

  open(anchor, onPick) {
    current = this; this.anchor = anchor; this.onPick = onPick; this.tone = savedTone(); this.search.value = ''; this.query = ''; this.hovered = null;
    this.outside = event => { if (!this.root.contains(event.target) && !anchor.contains(event.target) && event.target !== this.scrim) this.close(false); };
    document.addEventListener('pointerdown', this.outside, true);
    this.reposition = () => { this.position(); if (this.db && this.rows.length && this.sizeKey() !== this.laidOut) this.layout(false); };
    window.addEventListener('resize', this.reposition);
    document.body.append(this.scrim, this.root);
    anchor.setAttribute('aria-expanded', 'true');
    this.updateTones(); this.position();
    this.showMessage('Loading emoji…');
    this.search.focus();
    loadEmoji().then(db => { if (current !== this) return; this.db = db; this.layout(true); }, () => { if (current === this) this.showMessage('Emoji could not be loaded.'); });
  }

  showMessage(text) { this.clearRows(); this.canvas.style.height = ''; this.canvas.append(el('p', 'emoji-empty', text)); this.entries = []; this.rows = []; this.blocks = []; this.status.textContent = text; this.previewEntry(null); this.syncTabs(); }

  // Where the picker sits: above the button when there is room (the composer is at the bottom), else below; its left edge on the button's, so it
  // opens to the right of it, moved left only as far as the viewport needs.
  position() {
    const root = this.root;
    if (matchMedia(PHONE).matches) { root.style.top = root.style.left = ''; return; }
    const anchor = this.anchor.getBoundingClientRect(), width = root.offsetWidth || 340, height = root.offsetHeight || 380, margin = 8;
    const below = anchor.bottom + 6, above = anchor.top - 6 - height;
    const top = above >= margin ? above : Math.min(below, Math.max(margin, innerHeight - height - margin));
    root.style.top = `${Math.max(margin, top)}px`;
    root.style.left = `${Math.min(Math.max(margin, anchor.left), Math.max(margin, innerWidth - width - margin))}px`;
  }
  setTone(tone) {
    this.tone = tone; saveTone(tone); this.updateTones();
    if (!this.rows.length) return;
    this.clearRows(); this.render();
    this.previewEntry(this.hovered);
  }
  updateTones() { this.toneButtons.forEach((button, index) => button.setAttribute('aria-pressed', String(SKIN_TONES[index].key === this.tone))); }

  // Sizes come from the style sheet (they differ on a phone) and are read from the cell's own custom properties.
  metrics() {
    const style = getComputedStyle(this.root);
    return { cell: parseFloat(style.getPropertyValue('--emoji-cell')) || 34, heading: parseFloat(style.getPropertyValue('--emoji-heading')) || 28 };
  }
  // Columns and the two row heights the rows were laid out with: a change in any of them (a resize, a phone breakpoint) needs a new layout.
  sizeKey() { const { cell, heading } = this.metrics(); return `${this.columnsFor()}/${cell}/${heading}`; }
  columnsFor() { return Math.max(1, Math.floor((this.canvas.clientWidth || 328) / this.metrics().cell)); }

  // Lays the entries out as rows: a heading per block, then lines of `cols` cells. `reset` goes back to the top and the first cell.
  layout(reset, again = false) {
    const db = this.db; if (!db) return;
    this.query = this.search.value.trim();
    let blocks;
    if (this.query) {
      const results = searchEmoji(db, this.query, SEARCH_LIMIT);
      if (!results.length) { this.showMessage(`No emoji found for “${this.query}”`); return; }
      blocks = [{ key: '', label: 'Search results', entries: results }];
      this.status.textContent = `${results.length} emoji found`;
    } else { blocks = groupsOf(db); this.status.textContent = ''; }
    const { cell, heading } = this.metrics(), cols = this.cols = this.columnsFor(); this.laidOut = this.sizeKey(); this.root.style.setProperty('--cols', String(cols));
    this.grid.setAttribute('aria-colcount', String(cols));
    const rows = [], entries = [], tops = []; let top = 0;
    for (const block of blocks) {
      tops.push({ key: block.key, top });
      rows.push({ kind: 'heading', label: block.label, top, height: heading }); top += heading;
      for (let at = 0; at < block.entries.length; at += cols) {
        const line = block.entries.slice(at, at + cols);
        rows.push({ kind: 'cells', first: entries.length, count: line.length, top, height: cell }); top += cell;
        for (const entry of line) entries.push(entry);
      }
    }
    this.rows = rows; this.entries = entries; this.blocks = tops;
    this.canvas.replaceChildren(); this.live.clear(); this.canvas.style.height = `${top}px`; this.grid.setAttribute('aria-rowcount', String(rows.length));
    // The rows made the grid scroll, and a classic scrollbar takes its width from the canvas: a column counted without it would be cut in half under it. Lay out once more with it.
    if (!again && this.sizeKey() !== this.laidOut) { this.layout(reset, true); return; }
    if (reset) { this.grid.scrollTop = 0; this.focused = 0; this.previewEntry(null); } else this.focused = Math.min(this.focused, Math.max(0, entries.length - 1));
    this.render(); this.syncTabs();
  }

  clearRows() { this.canvas.replaceChildren(); this.live.clear(); }
  scheduled() { if (!this.frame) this.frame = requestAnimationFrame(() => { this.frame = 0; if (current === this) { this.render(); this.syncTabs(); } }); }

  // The index of the row at a scroll offset (rows are sorted by top).
  rowAt(offset) {
    let low = 0, high = this.rows.length - 1;
    while (low < high) { const mid = (low + high + 1) >> 1; if (this.rows[mid].top <= offset) low = mid; else high = mid - 1; }
    return low;
  }
  render() {
    if (!this.rows.length) return;
    const from = Math.max(0, this.rowAt(this.grid.scrollTop) - OVERSCAN), to = Math.min(this.rows.length - 1, this.rowAt(this.grid.scrollTop + this.grid.clientHeight) + OVERSCAN);
    // The row holding the focus stays however far the grid is scrolled (a wheel or a scrollbar can move it away), or the focus would fall to <body>.
    for (const [index, node] of this.live) if ((index < from || index > to) && !node.contains(document.activeElement)) { node.remove(); this.live.delete(index); }
    for (let index = from; index <= to; index++) if (!this.live.has(index)) { const node = this.rowNode(this.rows[index], index); this.live.set(index, node); this.canvas.append(node); }
  }
  rowNode(row, index) {
    if (row.kind === 'heading') {
      const heading = el('div', 'emoji-heading'); heading.setAttribute('role', 'row'); heading.setAttribute('aria-rowindex', String(index + 1)); heading.style.top = `${row.top}px`;
      const name = el('span', '', row.label); name.setAttribute('role', 'rowheader'); name.setAttribute('aria-colspan', String(this.cols)); heading.append(name); return heading;
    }
    const line = el('div', 'emoji-row'); line.setAttribute('role', 'row'); line.setAttribute('aria-rowindex', String(index + 1)); line.style.top = `${row.top}px`;
    for (let at = 0; at < row.count; at++) line.append(this.cell(row.first + at));
    return line;
  }
  cell(index) {
    const entry = this.entries[index];
    const button = el('button', 'row-btn emoji-cell'); button.type = 'button'; button.setAttribute('role', 'gridcell'); button.tabIndex = index === this.focused ? 0 : -1; button.title = entry.name; button.setAttribute('aria-label', entry.name); button.dataset.i = String(index);
    const glyph = el('span', '', withTone(entry, this.tone)); glyph.setAttribute('aria-hidden', 'true'); button.append(glyph);
    return button;
  }
  cellNode(index) { return this.canvas.querySelector(`.emoji-cell[data-i="${index}"]`); }

  previewEntry(entry) {
    this.hovered = entry;
    this.previewName.classList.toggle('emoji-preview-idle', !entry); // the invitation to pick is a hint, not a name
    if (!entry) { this.previewGlyph.textContent = ''; this.previewName.textContent = 'Pick an emoji'; this.previewCode.textContent = ''; return; }
    this.previewGlyph.textContent = withTone(entry, this.tone); this.previewName.textContent = entry.name;
    const alias = this.query ? matchedAlias(entry, this.query) : entry.aliases[0]; this.previewCode.textContent = `:${alias}:`;
  }
  syncTabs() {
    let active = this.query || !this.blocks.length ? '' : this.blocks[0].key;
    if (!this.query) for (const block of this.blocks) if (block.top <= this.grid.scrollTop + 8) active = block.key;
    for (const [key, tab] of this.tabButtons) { if (key === active) tab.setAttribute('aria-current', 'true'); else tab.removeAttribute('aria-current'); }
  }
  jump(key) {
    if (this.query) { this.search.value = ''; this.layout(true); }
    const block = this.blocks.find(item => item.key === key); if (!block) return;
    this.grid.scrollTop = block.top;
    const row = this.rows[this.rowAt(block.top) + 1];
    this.render(); this.focusCell(row?.kind === 'cells' ? row.first : this.focused, false); this.syncTabs();
  }
  // Makes cell `index` the one that takes the Tab stop, scrolls it into view and (with `focus`) focuses it.
  focusCell(index, focus = true) {
    if (!this.entries.length) return;
    index = Math.min(Math.max(0, index), this.entries.length - 1);
    const previous = this.cellNode(this.focused); if (previous) previous.tabIndex = -1;
    this.focused = index;
    const row = this.rows.find(item => item.kind === 'cells' && index >= item.first && index < item.first + item.count);
    if (row && focus) {
      if (row.top < this.grid.scrollTop) this.grid.scrollTop = row.top;
      else if (row.top + row.height > this.grid.scrollTop + this.grid.clientHeight) this.grid.scrollTop = row.top + row.height - this.grid.clientHeight;
    }
    this.render();
    const node = this.cellNode(index); if (!node) return;
    node.tabIndex = 0;
    if (focus) node.focus({ preventScroll: true });
  }
  // Moves along a row (left/right), or to the cell nearest in the line above or below, over the headings.
  neighbour(index, key) {
    if (key === 'ArrowLeft') return index - 1;
    if (key === 'ArrowRight') return index + 1;
    if (key === 'Home') return 0;
    if (key === 'End') return this.entries.length - 1;
    const at = this.rows.findIndex(row => row.kind === 'cells' && index >= row.first && index < row.first + row.count); if (at < 0) return null;
    const column = index - this.rows[at].first, step = key === 'ArrowDown' || key === 'PageDown' ? 1 : -1, lines = key.startsWith('Page') ? Math.max(1, Math.floor(this.grid.clientHeight / this.metrics().cell) - 1) : 1;
    let target = at, moved = 0;
    for (let next = at + step; next >= 0 && next < this.rows.length && moved < lines; next += step) if (this.rows[next].kind === 'cells') { target = next; moved++; }
    if (target === at) return null;
    const row = this.rows[target]; return row.first + Math.min(column, row.count - 1);
  }
  keydown(event) {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); this.close(); return; }
    if (event.key === 'Tab') {
      const stops = [...this.root.querySelectorAll('button, input')].filter(node => !node.disabled && node.tabIndex >= 0 && node.getClientRects().length);
      const first = stops[0], last = stops.at(-1);
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); } else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
      return;
    }
    const target = event.target;
    if (target === this.search) {
      if (event.key === 'ArrowDown' && this.entries.length) { event.preventDefault(); this.focusCell(this.focused); }
      else if (event.key === 'Enter' && this.query && this.entries.length) { event.preventDefault(); this.pick(this.entries[0]); }
      return;
    }
    if (target.classList?.contains('emoji-cell') && ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      const next = this.neighbour(Number(target.dataset.i), event.key);
      if (next !== null && next >= 0 && next < this.entries.length) this.focusCell(next); else if (event.key === 'ArrowUp') this.search.focus();
    }
  }
  pick(entry) {
    if (!entry) return;
    const glyph = withTone(entry, this.tone), onPick = this.onPick;
    this.close(true);
    onPick?.(glyph, entry);
  }
  close(restoreFocus = true) {
    if (current !== this) return;
    current = null;
    document.removeEventListener('pointerdown', this.outside, true); window.removeEventListener('resize', this.reposition);
    if (this.frame) { cancelAnimationFrame(this.frame); this.frame = 0; }
    // Nothing of the grid is kept between opens: the picker is a few dozen nodes then, and out of the page.
    this.clearRows(); this.entries = []; this.rows = []; this.blocks = [];
    this.scrim.remove(); this.root.remove(); this.anchor.setAttribute('aria-expanded', 'false');
    if (restoreFocus && this.anchor.isConnected) this.anchor.focus();
  }
}
