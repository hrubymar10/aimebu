import { Picker } from './picker.js';
import { request } from './api.js';

// Mirrors v1 expansion at selection time. The server always receives the
// resulting editable draft, never a request to expand message text.
export const MAX_MACRO_EXPANSION = 16 * 1024;

export function expandMacros(body, macros) {
  const escape = '\x02';
  let value = body.replace(/\\</g, escape);
  if (value.length > MAX_MACRO_EXPANSION) return body.replaceAll(/\\</g, '<');
  for (let depth = 0; depth < 8; depth++) {
    let next = '', cursor = 0;
    const references = /<([a-z][a-z0-9_-]*)>/gi;
    for (let match; (match = references.exec(value)) !== null;) {
      const key = match[1].toLowerCase();
      const replacement = Object.hasOwn(macros, key) ? macros[key].replace(/\\</g, escape) : match[0];
      const literal = value.slice(cursor, match.index);
      if (next.length + literal.length + replacement.length > MAX_MACRO_EXPANSION) return value.replaceAll(escape, '<');
      next += literal + replacement;
      cursor = references.lastIndex;
    }
    if (next.length + value.length - cursor > MAX_MACRO_EXPANSION) return value.replaceAll(escape, '<');
    next += value.slice(cursor);
    if (next === value) break;
    value = next;
  }
  return value.replaceAll(escape, '<');
}

export class MacroAutocomplete {
  constructor(draft) {
    this.draft = draft;
    this.macros = {};
    this.items = [];
    this.seq = 0;
    this.sessionGeneration = 0;
    this.list = new Picker(draft, 'macro-commands', index => this.pick(index));
    this.popup = this.list.popup;
    draft.addEventListener('input', () => this.refresh());
    draft.addEventListener('click', () => this.refresh());
    draft.addEventListener('keyup', event => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) this.refresh(); });
    draft.addEventListener('focus', () => this.refresh());
    draft.addEventListener('keydown', event => this.keydown(event));
    draft.addEventListener('blur', () => setTimeout(() => { if (!draft.hasFocus?.()) this.close(); }, 120));
  }

  reset() { this.seq++; this.sessionGeneration++; this.wasActive = false; this.loading = null; this.macros = {}; this.items = []; this.close(); }

  match() {
    const before = this.draft.value.slice(0, this.draft.selectionStart);
    const match = /(^|[^\\])<([a-z][a-z0-9_-]*)?$/i.exec(before);
    if (!match) return null;
    const start = before.length - (match[2] || '').length - 1;
    return { start, prefix: (match[2] || '').toLowerCase() };
  }

  async refresh() {
    const seq = ++this.seq;
    let match = this.match();
    if (!match) { this.wasActive = false; this.close(); return; }
    if (!this.wasActive) {
      this.wasActive = true;
      const generation = this.sessionGeneration;
      this.loading = request('/api/macros').then(macros => { if (generation === this.sessionGeneration) this.macros = macros || {}; }).catch(() => { if (generation === this.sessionGeneration) this.macros = {}; });
    }
    await this.loading;
    if (seq !== this.seq) return;
    match = this.match();
    if (!match) { this.wasActive = false; this.close(); return; }
    this.items = Object.keys(this.macros).filter(key => key.startsWith(match.prefix)).sort();
    if (!this.items.length) { this.close(); return; }
    // v1 shows the first 40 characters of the body.
    this.list.show(this.items.map(key => ({ key: `<${key.toUpperCase()}>`, preview: this.macros[key].length > 40 ? `${this.macros[key].slice(0, 40)}...` : this.macros[key] })));
  }

  keydown(event) { this.list.keydown(event); }

  pick(index) {
    const match = this.match(), key = this.items[index];
    if (!match || !key) return;
    const position = this.draft.selectionStart;
    const after = this.draft.value.slice(position);
    const space = !after || (!/\s/.test(after[0]) && !/[,.!?;:)\]}>]/.test(after[0])) ? ' ' : '';
    const inserted = expandMacros(`<${key}>`, this.macros) + space;
    this.draft.value = this.draft.value.slice(0, match.start) + inserted + after;
    this.draft.setSelectionRange(match.start + inserted.length, match.start + inserted.length);
    this.close(); this.wasActive = false;
    this.draft.dispatchEvent(new Event('input', { bubbles: true }));
    this.draft.focus();
  }

  close() { this.seq++; this.list.close(); this.items = []; }
}
