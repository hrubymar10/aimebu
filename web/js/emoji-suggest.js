import { Picker } from './picker.js';
import { emojiLoaded, insideCode, loadEmoji, matchedAlias, savedTone, searchEmoji, withTone } from './emoji-data.js';
// `:shortcode:` in the composer. Typing ":" and two or more shortcode characters opens the shared picker list with
// matching emoji; a complete `:shrug:` is converted in place. Neither fires inside code, in a URL ("https://") or in a time
// ("10:30"): the colon has to open a word, so it must follow whitespace, "(" or the start of the draft.
const OPEN = /(^|[\s(]):([a-z0-9_+-]{2,})$/i;
const DONE = /(^|[\s(]):([a-z0-9_+-]+):$/i;
const LIMIT = 8;

export class EmojiAutocomplete {
  constructor(draft) {
    this.draft = draft; this.items = []; this.busy = false; this.seq = 0;
    this.list = new Picker(draft, 'emoji-commands', index => this.pick(index));
    this.popup = this.list.popup;
    draft.addEventListener('input', event => this.input(event));
    draft.addEventListener('click', () => this.refresh());
    draft.addEventListener('focus', () => { loadEmoji().catch(() => {}); });
    draft.addEventListener('keyup', event => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) this.refresh(); });
    draft.addEventListener('keydown', event => this.list.keydown(event));
    draft.addEventListener('blur', () => setTimeout(() => { if (!draft.hasFocus?.()) this.close(); }, 120));
  }
  reset() { this.seq++; this.close(); }
  before() { return this.draft.value.slice(0, this.draft.selectionStart); }
  input(event) {
    if (this.busy) return;
    if (!event.isComposing && !String(event.inputType || '').startsWith('delete') && this.convert()) return;
    this.refresh();
  }
  // Replaces a just-typed `:name:` with its glyph. Returns true when it did. On a cold cache it starts the load and tries again
  // when it arrives, but only if the draft and the caret are still where they were.
  convert() {
    const db = emojiLoaded(), before = this.before(), match = DONE.exec(before);
    if (!match) return false;
    if (!db) {
      const value = this.draft.value, caret = this.draft.selectionStart;
      loadEmoji().then(() => { if (this.draft.value === value && this.draft.selectionStart === caret && (this.draft.hasFocus ? this.draft.hasFocus() : document.activeElement === this.draft)) this.convert(); }, () => {});
      return false;
    }
    const entry = db.byAlias.get(match[2].toLowerCase());
    if (!entry || insideCode(before, before.length - match[2].length - 2)) return false;
    this.replace(before.length - match[2].length - 2, before.length, withTone(entry, savedTone()));
    this.close();
    return true;
  }
  async refresh() {
    const seq = ++this.seq, before = this.before(), match = OPEN.exec(before);
    if (!match || insideCode(before, before.length - match[2].length - 1)) { this.close(); return; }
    let db = emojiLoaded();
    if (!db) { try { db = await loadEmoji(); } catch { this.close(); return; } }
    // The draft moved on while the data loaded.
    if (seq !== this.seq) return;
    const query = match[2];
    this.items = searchEmoji(db, query, LIMIT);
    if (!this.items.length) { this.close(); return; }
    const tone = savedTone();
    this.list.show(this.items.map(entry => ({
      key: `:${matchedAlias(entry, query)}:`, preview: entry.name,
      render: node => {
        const glyph = document.createElement('i'); glyph.className = 'emoji-glyph'; glyph.setAttribute('aria-hidden', 'true'); glyph.textContent = withTone(entry, tone);
        const alias = matchedAlias(entry, query), at = alias.toLowerCase().indexOf(query.toLowerCase());
        const label = document.createElement('i'); label.className = 'emoji-code';
        if (at < 0) label.textContent = `:${alias}:`;
        else { const mark = document.createElement('mark'); mark.textContent = alias.slice(at, at + query.length); label.append(`:${alias.slice(0, at)}`, mark, `${alias.slice(at + query.length)}:`); }
        node.append(glyph, label);
      },
    })));
  }
  pick(index) {
    const entry = this.items[index], before = this.before(), match = OPEN.exec(before);
    if (!entry || !match) return;
    const after = this.draft.value.slice(this.draft.selectionStart);
    const space = !after || (!/\s/.test(after[0]) && !/[,.!?;:)\]}>]/.test(after[0])) ? ' ' : '';
    this.close();
    this.replace(before.length - match[2].length - 1, before.length, withTone(entry, savedTone()) + space);
    this.draft.focus();
  }
  // Goes through execCommand so the browser's undo stack keeps the edit; the assignment is the fallback.
  replace(start, end, text) {
    const draft = this.draft;
    this.busy = true;
    try {
      draft.setSelectionRange(start, end);
      if (!document.execCommand?.('insertText', false, text)) {
        draft.setRangeText(text, start, end, 'end');
        draft.dispatchEvent(new Event('input', { bubbles: true }));
      }
    } finally { this.busy = false; }
  }
  // The draft's aria-* are shared with the other pickers, so a list that is not open must not touch them.
  close() { this.seq++; if (this.list.open) this.list.close(); this.items = []; }
}
