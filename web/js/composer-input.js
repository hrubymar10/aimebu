// What the composer's pickers (@ mentions, < macros, / commands, : emoji) talk to instead of the raw textarea. With formatting
// off it is the textarea. With formatting on it is the editor, presented through the same small surface (value,
// selectionStart/End, setSelectionRange, setRangeText, focus, events, aria attributes), so the pickers need not know which
// one is in use. Listeners are remembered and moved when the editor starts or stops.
export class ComposerInput {
  constructor(textarea) { this.textarea = textarea; this.bridge = null; this.listeners = []; }
  get target() { return this.bridge ? this.bridge.dom : this.textarea; }
  flags(type, options) {
    // The editor's own Enter, Arrow and Escape handling runs in the bubble phase; a picker must see the key first.
    if (this.bridge && type === 'keydown') return { ...(typeof options === 'object' ? options : {}), capture: true };
    return options;
  }
  addEventListener(type, handler, options) {
    const entry = { type, handler, options };
    this.listeners.push(entry);
    this.target.addEventListener(type, handler, this.flags(type, options));
  }
  rebind(bridge) {
    for (const { type, handler, options } of this.listeners) this.target.removeEventListener(type, handler, this.flags(type, options));
    this.bridge = bridge;
    for (const { type, handler, options } of this.listeners) this.target.addEventListener(type, handler, this.flags(type, options));
  }
  attach(bridge) { this.rebind(bridge); }
  detach() { this.rebind(null); }
  get value() { return this.bridge ? this.bridge.value : this.textarea.value; }
  set value(text) { if (this.bridge) this.bridge.value = text; else this.textarea.value = text; }
  get selectionStart() { return this.bridge ? this.bridge.selectionStart : this.textarea.selectionStart; }
  get selectionEnd() { return this.bridge ? this.bridge.selectionEnd : this.textarea.selectionEnd; }
  setSelectionRange(start, end) { if (this.bridge) this.bridge.setSelectionRange(start, end); else this.textarea.setSelectionRange(start, end); }
  setRangeText(text, start, end, mode) { if (this.bridge) this.bridge.replaceRange(start, end, text); else this.textarea.setRangeText(text, start, end, mode); }
  focus(options) { this.textarea.focus(options); }
  hasFocus() { return this.bridge ? this.bridge.dom.contains(document.activeElement) : document.activeElement === this.textarea; }
  dispatchEvent(event) { return this.target.dispatchEvent(event); }
  setAttribute(name, value) { this.target.setAttribute(name, value); }
  removeAttribute(name) { this.target.removeAttribute(name); }
  getAttribute(name) { return this.target.getAttribute(name); }
  closest(selector) { return this.textarea.closest(selector); }
  get parentNode() { return this.textarea.parentNode; }
  get form() { return this.textarea.form; }
}
