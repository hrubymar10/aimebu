import { CATEGORIES, SKIN_TONES, loadEmoji, matchedAlias, saveTone, savedTone, searchEmoji, withTone } from './emoji-data.js';
// The Slack-style emoji picker: category tabs, a search box that is focused on open, a grid, a footer that previews the
// hovered emoji and a skin-tone selector. One instance at a time, anchored to the control that opened it (a bottom sheet on a
// phone). `onPick(glyph, entry)` runs after the picker has closed.
const PHONE = '(max-width: 700px)';
const SEARCH_LIMIT = 200;
let current = null;

function el(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text != null) node.textContent = text; return node; }

export function emojiPickerOpen() { return !!current; }
export function closeEmojiPicker(restoreFocus = true) { current?.close(restoreFocus); }

export function openEmojiPicker({ anchor, onPick }) {
  const wasAnchor = current?.anchor === anchor;
  closeEmojiPicker(false);
  if (wasAnchor) return;
  current = new EmojiPicker(anchor, onPick);
}

class EmojiPicker {
  constructor(anchor, onPick) {
    this.anchor = anchor; this.onPick = onPick; this.tone = savedTone(); this.cells = []; this.sections = []; this.db = null;
    this.scrim = el('div', 'emoji-picker-scrim'); this.scrim.onclick = () => this.close();
    const root = this.root = el('div', 'emoji-picker'); root.setAttribute('role', 'dialog'); root.setAttribute('aria-label', 'Emoji picker');
    this.search = el('input', 'emoji-search'); this.search.type = 'search'; this.search.placeholder = 'Search emoji'; this.search.setAttribute('aria-label', 'Search emoji'); this.search.autocomplete = 'off'; this.search.spellcheck = false;
    this.search.oninput = () => this.render();
    this.tabs = el('div', 'emoji-tabs'); this.tabs.setAttribute('role', 'group'); this.tabs.setAttribute('aria-label', 'Emoji categories');
    this.tabButtons = new Map();
    for (const category of CATEGORIES) {
      const tab = el('button', 'row-btn emoji-tab', category.icon); tab.type = 'button'; tab.title = category.label; tab.setAttribute('aria-label', category.label);
      tab.onclick = () => this.jump(category.key); this.tabButtons.set(category.key, tab); this.tabs.append(tab);
    }
    this.grid = el('div', 'emoji-grid'); this.grid.setAttribute('role', 'group'); this.grid.setAttribute('aria-label', 'Emoji'); this.grid.onscroll = () => this.syncTabs();
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
    this.outside = event => { if (!root.contains(event.target) && !anchor.contains(event.target) && event.target !== this.scrim) this.close(false); };
    document.addEventListener('pointerdown', this.outside, true);
    this.reposition = () => this.position();
    window.addEventListener('resize', this.reposition);
    document.body.append(this.scrim, root);
    anchor.setAttribute('aria-expanded', 'true');
    this.updateTones(); this.position();
    this.showMessage('Loading emoji…');
    this.search.focus();
    loadEmoji().then(db => { if (current !== this) return; this.db = db; this.render(); }, () => { if (current === this) this.showMessage('Emoji could not be loaded.'); });
  }
  showMessage(text) { this.grid.replaceChildren(el('p', 'emoji-empty', text)); this.cells = []; this.sections = []; this.status.textContent = text; }
  position() {
    const root = this.root;
    if (matchMedia(PHONE).matches) { root.style.top = root.style.left = ''; return; }
    const anchor = this.anchor.getBoundingClientRect(), width = root.offsetWidth || 340, height = root.offsetHeight || 380, margin = 8;
    const below = anchor.bottom + 6, above = anchor.top - 6 - height;
    const top = below + height + margin <= innerHeight || above < margin ? Math.min(below, Math.max(margin, innerHeight - height - margin)) : above;
    root.style.top = `${Math.max(margin, top)}px`;
    root.style.left = `${Math.min(Math.max(margin, anchor.right - width), Math.max(margin, innerWidth - width - margin))}px`;
  }
  setTone(tone) {
    this.tone = tone; saveTone(tone); this.updateTones();
    for (const cell of this.cells) cell.firstChild.textContent = withTone(cell.entry, this.tone);
    this.previewEntry(this.hovered);
  }
  updateTones() { this.toneButtons.forEach((button, index) => button.setAttribute('aria-pressed', String(SKIN_TONES[index].key === this.tone))); }
  cell(entry) {
    const button = el('button', 'row-btn emoji-cell'); button.type = 'button'; button.tabIndex = -1; button.title = entry.name; button.setAttribute('aria-label', entry.name);
    const glyph = el('span', '', withTone(entry, this.tone)); glyph.setAttribute('aria-hidden', 'true'); button.append(glyph);
    button.entry = entry;
    button.onclick = () => this.pick(entry);
    button.onmouseenter = button.onfocus = () => this.previewEntry(entry);
    return button;
  }
  previewEntry(entry) {
    this.hovered = entry;
    if (!entry) { this.previewGlyph.textContent = ''; this.previewName.textContent = 'Pick an emoji'; this.previewCode.textContent = ''; return; }
    this.previewGlyph.textContent = withTone(entry, this.tone); this.previewName.textContent = entry.name;
    const alias = this.query ? matchedAlias(entry, this.query) : entry.aliases[0]; this.previewCode.textContent = `:${alias}:`;
  }
  render() {
    const db = this.db; if (!db) return;
    this.query = this.search.value.trim();
    this.cells = []; this.sections = [];
    const frame = document.createDocumentFragment();
    if (this.query) {
      const results = searchEmoji(db, this.query, SEARCH_LIMIT);
      if (!results.length) { this.showMessage(`No emoji found for “${this.query}”`); this.syncTabs(); this.previewEntry(null); return; }
      const section = el('section', 'emoji-section'); section.append(el('h3', 'emoji-heading', 'Search results'));
      const list = el('div', 'emoji-cells'); for (const entry of results) { const cell = this.cell(entry); this.cells.push(cell); list.append(cell); }
      section.append(list); frame.append(section);
      this.status.textContent = `${results.length} emoji found`;
    } else {
      for (const category of CATEGORIES) {
        const section = el('section', 'emoji-section'); section.dataset.category = category.key; section.append(el('h3', 'emoji-heading', category.label));
        const list = el('div', 'emoji-cells');
        for (const entry of db.entries) if (entry.group === category.key) { const cell = this.cell(entry); this.cells.push(cell); list.append(cell); }
        section.append(list); frame.append(section); this.sections.push({ key: category.key, section });
      }
      this.status.textContent = '';
    }
    this.grid.replaceChildren(frame); this.grid.scrollTop = 0;
    this.focusCell(this.cells[0], false); this.previewEntry(null); this.syncTabs();
  }
  syncTabs() {
    let active = this.query ? '' : this.sections[0]?.key;
    if (!this.query) for (const { key, section } of this.sections) if (section.offsetTop - this.grid.offsetTop <= this.grid.scrollTop + 8) active = key;
    for (const [key, tab] of this.tabButtons) { if (key === active) tab.setAttribute('aria-current', 'true'); else tab.removeAttribute('aria-current'); }
  }
  jump(key) {
    if (this.query) { this.search.value = ''; this.render(); }
    const target = this.sections.find(item => item.key === key); if (!target) return;
    this.grid.scrollTop = target.section.offsetTop - this.grid.offsetTop;
    this.focusCell(target.section.querySelector('.emoji-cell'), false); this.syncTabs();
  }
  focusCell(cell, focus = true) {
    for (const other of this.cells) other.tabIndex = -1;
    if (!cell) return;
    cell.tabIndex = 0;
    if (focus) { cell.focus(); cell.scrollIntoView({ block: 'nearest' }); }
  }
  // Moves along a row (left/right), or to the cell nearest in the row above or below.
  neighbour(cell, key) {
    const index = this.cells.indexOf(cell);
    if (key === 'ArrowLeft') return this.cells[index - 1];
    if (key === 'ArrowRight') return this.cells[index + 1];
    if (key === 'Home') return this.cells[0];
    if (key === 'End') return this.cells.at(-1);
    const down = key === 'ArrowDown' || key === 'PageDown', here = cell.getBoundingClientRect();
    const rows = this.cells.map(other => [other, other.getBoundingClientRect()]).filter(([, rect]) => down ? rect.top > here.top + 2 : rect.top < here.top - 2);
    if (!rows.length) return null;
    const edge = down ? Math.min(...rows.map(([, rect]) => rect.top)) : Math.max(...rows.map(([, rect]) => rect.top));
    return rows.filter(([, rect]) => Math.abs(rect.top - edge) < 2).sort((a, b) => Math.abs(a[1].left - here.left) - Math.abs(b[1].left - here.left))[0][0];
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
      if (event.key === 'ArrowDown' && this.cells.length) { event.preventDefault(); this.focusCell(this.cells[0]); }
      else if (event.key === 'Enter' && this.query && this.cells.length) { event.preventDefault(); this.pick(this.cells[0].entry); }
      return;
    }
    if (target.classList?.contains('emoji-cell') && ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(event.key)) {
      event.preventDefault();
      const next = this.neighbour(target, event.key);
      if (next) this.focusCell(next); else if (event.key === 'ArrowUp') this.search.focus();
    }
  }
  pick(entry) {
    const glyph = withTone(entry, this.tone), onPick = this.onPick;
    this.close(false);
    onPick?.(glyph, entry);
  }
  close(restoreFocus = true) {
    if (current !== this) return;
    current = null;
    document.removeEventListener('pointerdown', this.outside, true); window.removeEventListener('resize', this.reposition);
    this.scrim.remove(); this.root.remove(); this.anchor.setAttribute('aria-expanded', 'false');
    if (restoreFocus && this.anchor.isConnected) this.anchor.focus();
  }
}
