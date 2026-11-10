import { savePreference } from './preferences.js';
// Formatting for the message composer: a rich-text editor (ProseMirror, loaded on first use) with a Slack-like toolbar in its own
// row above the composer. The textarea stays the source of truth and the message goes out as the Markdown in it. The
// "Aa" button turns formatting off for a plain textarea (no toolbar, no editor); the choice is kept in this browser and shared by
// the message and thread composers.
import { loadProseMirror, ComposerEditor, EditorBridge, safeHref } from './composer-editor.js';
import { ComposerInput } from './composer-input.js';
import { setGlyph } from './icon-button.js';

// The layout: the toolbar is a row across the full width over the input, with attach and emoji as its first items (left of the format
// buttons); the bottom row is the input and send. A small screen (up to 900px, where the chat column shares the window with the side panels) has no room for attach and emoji in the toolbar row, and a phone held
// sideways has no toolbar at all.
const compact = window.matchMedia('(max-width: 900px), (max-height: 520px)');
// A phone held sideways hides the toolbar; "Aa" then moves into the field's corner so it can still be reached.
const short = window.matchMedia('(max-height: 520px)');
// A phone (or a touch screen up to 900px wide, held upright): the composer is one row with attach, emoji and "Aa" in it, and the format row is opened by "Aa"
// (a view state of each composer, not stored). Held sideways it has no room for the row, and "Aa" keeps its old job in the field's corner.
const phoneQuery = window.matchMedia('(max-width: 520px), (max-width: 900px) and (pointer: coarse)');
const phoneLayout = () => phoneQuery.matches && !short.matches;

const PREF = 'aimebu2.composer.formatting';
const readPref = () => { try { return localStorage.getItem(PREF) !== 'off'; } catch { return true; } };
const writePref = on => { try { localStorage.setItem(PREF, on ? 'on' : 'off'); } catch { /* Private browsing may deny storage. */ } savePreference('composer_formatting', on ? 'on' : 'off'); };
const composers = new Set();

const mac = /Mac|iPhone|iPad/i.test(navigator.platform || navigator.userAgent || '');
const MOD = mac ? '⌘' : 'Ctrl+';
const SHIFT = mac ? '⇧' : 'Shift+';
const ICON = body => `<svg aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${body}</svg>`;
const ICONS = {
  link: ICON('<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>'),
  ol: ICON('<path d="M10 6h11M10 12h11M10 18h11"/><path d="M3 5l1.5-1v4M3 13.5c0-1 1.5-1 1.5 0S3 15 3 16h2M3 18.5h2l-1 1.2c.6 0 1.2.3 1.2.9 0 1-1.6 1-2.2.4" stroke-width="1.4"/>'),
  ul: ICON('<path d="M9 6h12M9 12h12M9 18h12"/><circle cx="4" cy="6" r="1" fill="currentColor"/><circle cx="4" cy="12" r="1" fill="currentColor"/><circle cx="4" cy="18" r="1" fill="currentColor"/>'),
  quote: ICON('<path d="M5 6v12"/><path d="M10 8h10M10 12h10M10 16h6"/>'),
  code: ICON('<path d="M8 8l-4 4 4 4M16 8l4 4-4 4"/>'),
  codeblock: ICON('<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9.5 9.5L7 12l2.5 2.5M14.5 9.5L17 12l-2.5 2.5"/>'),
};

// The toolbar's commands, in order. `keys` is only the label of the shortcut; the editor's keymap owns the shortcuts themselves.
const COMMANDS = [
  { id: 'bold', label: 'Bold', glyph: 'B', keys: 'Mod+B' },
  { id: 'italic', label: 'Italic', glyph: 'I', keys: 'Mod+I' },
  { id: 'underline', label: 'Underline', glyph: 'U', keys: 'Mod+U' },
  { id: 'strike', label: 'Strikethrough', glyph: 'S', keys: 'Mod+Shift+X' },
  { id: 'link', label: 'Link', icon: 'link', keys: 'Mod+K' },
  { id: 'ol', label: 'Numbered list', icon: 'ol', keys: 'Mod+Shift+7' },
  { id: 'ul', label: 'Bulleted list', icon: 'ul', keys: 'Mod+Shift+8' },
  { id: 'quote', label: 'Quote', icon: 'quote', keys: 'Mod+Shift+9' },
  { id: 'code', label: 'Code', icon: 'code', keys: 'Mod+Shift+C' },
  { id: 'codeblock', label: 'Code block', icon: 'codeblock', keys: 'Mod+Alt+Shift+C' },
];

const shortcutLabel = keys => keys.replace('Mod+', MOD).replace('Shift+', SHIFT).replace('Alt+', mac ? '⌥' : 'Alt+');

export class ComposerFormatting {
  // options.onFiles receives image files pasted into the editor (the textarea's own paste handler is not reached there).
  constructor(textarea, options = {}) {
    this.textarea = textarea;
    this.onFiles = options.onFiles;
    this.rich = readPref();
    this.formatOpen = false; // phone layout: the format row is shown under the row of buttons
    this.editor = null;
    // What the pickers (mentions, macros, commands, emoji) read the draft through: this textarea, or the editor while it is on.
    this.input = new ComposerInput(textarea);
    // The rest of the app treats this textarea as the draft: it clears it after a send, sets it when the room changes, and a picker
    // can write into it. Those all go through `textarea.value = ...`, which would leave the editor showing the old text. So the
    // `value` property is wrapped on this one element: a write still lands in the textarea (through the prototype's own setter,
    // `setNative`), and also reloads the editor unless the write came from the editor itself (`syncing`).
    const native = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value');
    this.setNative = value => native.set.call(textarea, value);
    Object.defineProperty(textarea, 'value', { configurable: true, get() { return native.get.call(this); }, set: value => { native.set.call(textarea, value); if (this.editor && !this.syncing) this.editor.setText(value); } });
    const focus = textarea.focus.bind(textarea);
    textarea.focus = options => (this.editor ? this.editor.focus() : focus(options));
    this.field = document.createElement('div'); this.field.className = 'composer-field';
    this.toolbar = document.createElement('div'); this.toolbar.className = 'composer-toolbar'; this.toolbar.setAttribute('role', 'toolbar'); this.toolbar.setAttribute('aria-label', 'Formatting');
    this.box = document.createElement('div'); this.box.className = 'composer-box';
    textarea.before(this.field);
    this.box.append(textarea);
    // The link address, asked for in a small row under the toolbar (inside the frame) rather than in a browser prompt.
    this.linkForm = document.createElement('div'); this.linkForm.className = 'composer-link'; this.linkForm.hidden = true; this.linkForm.setAttribute('role', 'group'); this.linkForm.setAttribute('aria-label', 'Link');
    this.linkInput = document.createElement('input'); this.linkInput.type = 'text'; this.linkInput.className = 'composer-link-input'; this.linkInput.placeholder = 'https://'; this.linkInput.setAttribute('aria-label', 'Link address'); this.linkInput.autocomplete = 'off'; this.linkInput.spellcheck = false;
    this.linkError = document.createElement('span'); this.linkError.className = 'composer-link-error error'; this.linkError.setAttribute('role', 'alert');
    const apply = document.createElement('button'); apply.type = 'button'; apply.addEventListener('click', () => this.applyLink());
    apply.className = 'btn-icon composer-link-apply'; setGlyph(apply, 'link', 'Add link');
    const cancel = document.createElement('button'); cancel.type = 'button'; cancel.className = 'btn-icon composer-link-cancel'; setGlyph(cancel, 'close', 'Cancel link'); cancel.addEventListener('click', () => this.closeLink());
    this.linkForm.append(this.linkInput, apply, cancel, this.linkError);
    // Enter applies and Escape cancels. This row sits inside the composer's own form, so Enter must not submit the message, and
    // neither key reaches the page's own Enter and Escape handling.
    this.linkInput.addEventListener('keydown', event => {
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); this.closeLink(); }
      else if (event.key === 'Enter') { event.preventDefault(); event.stopPropagation(); this.applyLink(); }
      else this.linkError.textContent = '';
    });
    this.field.append(this.box);
    // Where the toolbar and the link row go depends on the layout (see place()). The attach and emoji buttons of the message
    // composer are the form's own children, which the layouts move and put back.
    this.form = textarea.form || null;
    this.slots = this.form ? [...this.form.querySelectorAll(':scope > .attachment-picker-btn, :scope > #emoji-button')] : [];
    // The buttons scroll inside their own strip when the field is narrow, so they never run under the "Aa" at the toolbar's end.
    this.tools = document.createElement('div'); this.tools.className = 'composer-tools'; this.toolbar.append(this.tools);
    this.buttons = new Map();
    for (const command of COMMANDS) {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'btn-icon composer-format'; button.dataset.format = command.id;
      button.setAttribute('aria-label', command.label); button.title = `${command.label} (${shortcutLabel(command.keys)})`; button.setAttribute('aria-pressed', 'false');
      if (command.icon) button.innerHTML = ICONS[command.icon]; else { button.textContent = command.glyph; button.classList.add(`composer-glyph-${command.id}`); }
      button.tabIndex = -1;
      // mousedown would move focus (and the selection's visibility) out of the editor before the click.
      button.addEventListener('mousedown', event => event.preventDefault());
      button.addEventListener('click', () => this.run(command));
      this.buttons.set(command.id, button); this.tools.append(button);
    }
    this.buttons.values().next().value.tabIndex = 0;
    this.toolbar.addEventListener('keydown', event => this.toolbarKeys(event));
    // Slack's "Aa": formatting on or off.
    this.toggle = document.createElement('button'); this.toggle.type = 'button'; this.toggle.className = 'btn-icon composer-aa'; this.toggle.innerHTML = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 14h-5"/><path d="M16 16v-3.5a2.5 2.5 0 0 1 5 0V16"/><path d="M4.5 13h6"/><path d="m3 16 4.5-9 4.5 9"/></svg>';
    this.toggle.addEventListener('mousedown', event => event.preventDefault());
    this.toggle.addEventListener('click', () => {
      if (!phoneLayout()) { ComposerFormatting.setRich(!this.rich); return; }
      // On a phone "Aa" shows or hides the format row; with formatting off it turns formatting on (and shows the row).
      if (this.rich) { this.formatOpen = !this.formatOpen; this.apply(); } else { this.formatOpen = true; ComposerFormatting.setRich(true); }
    });
    this.field.append(this.toggle);
    composers.add(this);
    this.apply();
  }
  // The choice is one setting for every composer on the page, and for other tabs through the storage event.
  static setRich(on) { writePref(on); for (const composer of composers) { composer.rich = on; composer.apply(); } }
  // The toolbar and the link row go in the composer's form (a full row above everything in it; the form wraps). Attach and emoji move
  // into the toolbar, as its first items, while the toolbar shows and are put back in the bottom row otherwise (a small screen).
  place() {
    (this.form || this.field).prepend(this.toolbar, this.linkForm);
    // "Aa" is the toolbar's last item while the toolbar shows, and sits in the field's corner when formatting is off or the toolbar is hidden.
    // On a phone it is in the row of buttons instead (after attach and emoji, before the input), whatever the state of the toolbar.
    const phone = phoneLayout() && !!this.form;
    const home = phone ? this.form : this.rich && !short.matches ? this.toolbar : this.field;
    if (this.toggle.parentNode !== home) { const focused = document.activeElement === this.toggle; home.append(this.toggle); if (focused) this.toggle.focus(); }
    if (!this.slots.length) { if (phone) this.form.insertBefore(this.toggle, this.field); return; }
    // Moving a node in the DOM drops its focus (to <body>): a key press or a closing picker that has just put focus on attach or emoji would lose it
    // to a layout change (a rotated phone, a resized window) that moves the buttons between the toolbar and the bottom row.
    const held = this.slots.find(slot => slot === document.activeElement);
    if (this.rich && !compact.matches) this.toolbar.prepend(...this.slots);
    else { for (const slot of this.slots) this.form.insertBefore(slot, this.field); }
    if (phone) { const focused = document.activeElement === this.toggle; this.form.insertBefore(this.toggle, this.field); if (focused) this.toggle.focus(); }
    if (held && document.activeElement !== held) held.focus({ preventScroll: true });
  }
  apply() {
    this.place();
    this.field.classList.toggle('composer-rich', this.rich);
    const phone = phoneLayout() && !!this.form, shown = this.rich && (!phone || this.formatOpen);
    this.toolbar.hidden = !shown;
    if (!shown) this.closeLink(false);
    if (phone) {
      // "Aa" opens and closes the format row; its label says what a press does.
      this.toggle.setAttribute('aria-pressed', String(shown));
      this.toggle.setAttribute('aria-label', shown ? 'Hide formatting' : 'Show formatting');
      this.toggle.title = shown ? 'Hide formatting' : 'Show formatting';
    } else {
      this.toggle.setAttribute('aria-pressed', String(this.rich));
      this.toggle.setAttribute('aria-label', this.rich ? 'Formatting is on' : 'Formatting is off');
      this.toggle.title = this.rich ? 'Hide formatting (plain text)' : 'Show formatting';
    }
    if (this.rich) this.startEditor(); else this.stopEditor();
    this.refreshState();
  }
  // For a history walk (chat.js): the draft as it is now, to put back exactly (the editor's document, so bold, lists and links survive), and a message's Markdown
  // loaded as formatting. Without the editor (plain text) both are the textarea's text.
  saveDraft() { return { text: this.textarea.value, doc: this.editor ? this.editor.getDoc() : null }; }
  restoreDraft(saved) {
    if (this.editor && saved.doc) { this.setNative(saved.text); this.editor.setDoc(saved.doc); } else this.textarea.value = saved.text;
  }
  loadMarkdown(text) {
    if (this.editor) { this.setNative(text); this.editor.setMarkdown(text); } else this.textarea.value = text;
  }
  async startEditor() {
    if (this.editor || this.starting) return;
    this.starting = true;
    try {
      const PM = await loadProseMirror();
      if (!this.rich || this.editor) return;
      this.host = document.createElement('div'); this.host.className = 'composer-editor';
      this.box.append(this.host);
      this.editor = new ComposerEditor(PM, this.host, {
        placeholder: this.textarea.placeholder,
        onChange: markdown => { this.edited = true; this.syncing = true; this.setNative(markdown); this.syncing = false; },
        onState: () => this.refreshState(),
        onSend: () => this.textarea.form?.requestSubmit(),
        onLink: () => this.link(),
      });
      this.edited = false;
      this.host.addEventListener('paste', event => {
        const files = [...(event.clipboardData?.files || [])];
        if (files.length && this.onFiles) { event.preventDefault(); event.stopPropagation(); this.onFiles(files); }
      }, true);
      this.editor.setText(this.textarea.value);
      this.input.attach(new EditorBridge(this.editor));
      this.field.classList.add('composer-editing');
      if (document.activeElement === this.textarea) this.editor.focus();
      this.refreshState();
    } finally { this.starting = false; }
  }
  stopEditor() {
    if (!this.editor) return;
    const markdown = this.editor.markdown(), hadFocus = this.host.contains(document.activeElement);
    this.input.detach();
    this.editor.destroy(); this.host.remove(); this.editor = null; this.host = null;
    this.field.classList.remove('composer-editing');
    // Leaving the editor writes nothing back unless something was edited in it: the draft stays exactly as it was.
    if (this.edited) this.setNative(markdown);
    if (hadFocus) this.textarea.focus();
  }
  // Each toolbar button shows whether its mark or block is on at the selection.
  refreshState() {
    for (const [id, button] of this.buttons) button.setAttribute('aria-pressed', String(!!this.editor && this.editor.isActive(id)));
  }
  // The link button, or Mod+K: on a link it takes the link off, otherwise it opens the address row.
  link() {
    if (!this.editor) return;
    if (this.editor.isActive('link')) { this.editor.removeLink(); return; }
    this.linkForm.hidden = false; this.linkError.textContent = '';
    this.linkInput.value = 'https://';
    this.linkInput.focus(); this.linkInput.select();
  }
  applyLink() {
    const href = this.linkInput.value.trim();
    if (!safeHref(href)) { this.linkError.textContent = 'Use an http, https or mailto address.'; this.linkInput.focus(); return; }
    this.closeLink();
    this.editor?.link(href);
  }
  closeLink(refocus = true) {
    if (this.linkForm.hidden) return;
    this.linkForm.hidden = true; this.linkError.textContent = '';
    if (refocus) this.editor?.focus();
  }
  run(command) {
    if (!this.editor) return;
    if (command.id === 'link') this.link(); else this.editor.run(command.id);
  }
  toolbarKeys(event) {
    const visible = [...this.buttons.values()];
    const index = visible.indexOf(document.activeElement);
    if (index < 0) return;
    const move = { ArrowRight: 1, ArrowLeft: -1 }[event.key];
    const target = event.key === 'Home' ? 0 : event.key === 'End' ? visible.length - 1 : move ? (index + move + visible.length) % visible.length : -1;
    if (target < 0) return;
    event.preventDefault();
    for (const button of this.buttons.values()) button.tabIndex = -1;
    visible[target].tabIndex = 0; visible[target].focus();
  }
}

const replaceAll = () => { for (const composer of composers) composer.apply(); };
compact.addEventListener('change', replaceAll);
short.addEventListener('change', replaceAll);
phoneQuery.addEventListener('change', replaceAll);

window.addEventListener('storage', event => {
  if (event.key !== PREF) return;
  const on = readPref();
  for (const composer of composers) { composer.rich = on; composer.apply(); }
});
