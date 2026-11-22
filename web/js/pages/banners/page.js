import { html, render } from '../../ui.js';
import { copyText } from '../../markdown.js';
import { NotificationBannerModel, UpdateBannerModel, UpdateBarModel } from './model.js';
import { NotificationBannerBody, UpdateBannerBody, UpdateBarBody } from './views.js';

export { BANNER_DISMISSED_PREFERENCE, bannerWanted, reloadIsSafe, updateBannerWanted } from './model.js';

// The adapters app.js uses for the three bars. Each makes its bar's element itself (so `.bar` is there as soon as it is constructed, for the
// next bar to be placed after it), keeps its hidden state, and draws its contents with a synchronous render() on every change of the
// controller (model.js), so what a call changed is on the page when it returns.

function makeBar(doc, { id, className, role, label }) {
  const bar = doc.createElement('div'); bar.className = className;
  if (id) bar.id = id;
  bar.setAttribute('role', role);
  if (label) bar.setAttribute('aria-label', label);
  return bar;
}

// Asks for a reload once the server is a different build. The bar is made on the first show() or stale() and put first in the body.
export class UpdateBar {
  constructor({ doc = document, reload = () => location.reload(), hasDraft = () => false } = {}) {
    this.doc = doc; this.bar = null;
    this.model = new UpdateBarModel({ doc, reload, hasDraft, onChange: () => this.draw() });
  }
  draw() {
    if (!this.bar) { this.bar = makeBar(this.doc, { id: 'update-bar', className: 'update-bar', role: 'alert' }); this.doc.body.prepend(this.bar); }
    render(html`<${UpdateBarBody} model=${this.model} />`, this.bar);
  }
  maybeReload() { this.model.maybeReload(); }
  show() { this.model.show(); }
  stale() { return this.model.stale(); }
}

// Reports the server's own update check to the admins; placed after `anchor`.
export class UpdateBanner {
  constructor({ getStatus, ack, anchor = document.querySelector('.chat > header') }) {
    this.model = new UpdateBannerModel({ getStatus, ack, copy: copyText, setTimeout: (fn, ms) => setTimeout(fn, ms), clearTimeout: id => clearTimeout(id), onChange: () => this.draw() });
    this.bar = makeBar(document, { className: 'update-banner', role: 'region', label: 'aimebu update' });
    if (anchor) anchor.after(this.bar);
    this.draw();
  }
  draw() {
    this.bar.hidden = this.model.hidden;
    if (this.bar.title !== this.model.title) this.bar.title = this.model.title; // the tooltip says why an answer was not saved
    render(html`<${UpdateBannerBody} model=${this.model} />`, this.bar);
  }
  setAccount(rights, ackRecord) { return this.model.setAccount(rights, ackRecord); }
  refresh() { return this.model.refresh(); }
  sync() { return this.model.sync(); }
  answer(kind) { return this.model.answer(kind); }
}

// Offers browser notifications the first time something needs the person; placed after `anchor`.
export class NotificationBanner {
  constructor({ notifications, saveDismissed, anchor = document.querySelector('.chat > header'), permission = () => ('Notification' in window ? Notification.permission : 'denied') }) {
    this.model = new NotificationBannerModel({ notifications, saveDismissed, permission, isHidden: () => document.hidden, onChange: () => this.draw() });
    this.bar = makeBar(document, { className: 'notification-banner', role: 'region', label: 'Browser notifications' });
    anchor.after(this.bar);
    this.draw();
  }
  draw() { this.bar.hidden = this.model.hidden; render(html`<${NotificationBannerBody} model=${this.model} doc=${document} permissions=${navigator.permissions} />`, this.bar); }
  get dismissed() { return this.model.dismissed; }
  sync() { this.model.sync(); }
  reset(dismissed) { this.model.reset(dismissed); }
  attention() { this.model.attention(); }
  show() { this.model.show(); }
  ask() { return this.model.ask(); }
  close(forGood) { this.model.close(forGood); }
}
