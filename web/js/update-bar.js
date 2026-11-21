import { iconButton } from './icon-button.js';

// A page that was loaded from one build keeps running after the server is replaced by another (a deploy): its scripts no longer match what
// the server serves. The event stream tells the page when the server it reconnected to is a different build; this bar asks for a reload, and
// the page reloads by itself once nobody is looking at it and nothing typed would be lost.

// Whether a reload would lose nothing: no dialog is open, the chat holds no draft or pending file (`hasDraft`, since the formatted composer's
// textarea is out of sight), and no other text field on show holds a draft (a field out of sight, in a closed dialog or a
// section not drawn, keeps what the server gave it and is no draft).
export function reloadIsSafe(doc = document, hasDraft = () => false) {
  if (doc.querySelector('dialog[open]') || hasDraft()) return false;
  return ![...doc.querySelectorAll('textarea, input[type=text], input:not([type])')].some(field => !field.disabled && String(field.value || '').trim() !== '' && field.type !== 'password' && field.getClientRects().length > 0);
}

export class UpdateBar {
  constructor({ doc = document, reload = () => location.reload(), hasDraft = () => false } = {}) {
    this.doc = doc; this.reload = reload; this.hasDraft = hasDraft; this.bar = null;
    // Looking away is the moment a reload costs nothing.
    doc.addEventListener('visibilitychange', () => { if (this.bar && doc.hidden) this.maybeReload(); });
  }
  maybeReload() { if (this.doc.hidden && reloadIsSafe(this.doc, this.hasDraft)) this.reload(); }
  show() {
    if (this.bar) return;
    const bar = this.doc.createElement('div'); bar.className = 'update-bar'; bar.id = 'update-bar'; bar.setAttribute('role', 'alert');
    const text = this.doc.createElement('span'); text.textContent = 'aimebu was updated. Reload to use the new version.';
    const button = this.doc.createElement('button'); button.type = 'button'; button.className = 'btn btn-primary'; button.textContent = 'Reload'; button.onclick = () => this.reload();
    bar.append(text, button); this.doc.body.prepend(bar); this.bar = bar;
    this.maybeReload();
  }
  // A part of the page could not be fetched because the server is a newer build (the old build's chunks are gone): reload at once when nothing typed would be
  // lost, otherwise keep a bar up (with a reload icon button) until the person reloads.
  stale() {
    if (reloadIsSafe(this.doc, this.hasDraft)) { this.reload(); return 'reloaded'; }
    if (!this.bar) {
      const bar = this.doc.createElement('div'); bar.className = 'update-bar'; bar.id = 'update-bar'; bar.setAttribute('role', 'alert');
      const text = this.doc.createElement('span'); text.textContent = 'A new version is ready, reload to continue.';
      const button = iconButton('refresh', 'Reload', { className: 'update-bar-reload' }); button.onclick = () => this.reload();
      bar.append(text, button); this.doc.body.prepend(bar); this.bar = bar;
    }
    return 'bar';
  }
}
