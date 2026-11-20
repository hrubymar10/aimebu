import { element } from './usage-format.js';
import { focusPopover, returnFocus } from './popover-focus.js';
import { html, render, effect } from './ui.js';
import { UsagesStore } from './pages/usages/store.js';
import { UsagesView, usagesGroup, toggleUsagesGroup } from './pages/usages/usages-view.js';

// A glanceable overview of every configured provider and profile, so usage can be checked
// without opening Settings. Like v1's panel it is view-only apart from Refresh and switching the active profile: management stays in Settings → Usages.
// It draws the Usages page's own view (read-only) from a store of the panel's own.

// What a failed action says goes above the view, as on the Usages page.
function PanelView({ store }) {
  return html`<p class="error usages-popover-error" role="alert">${store.error.value}</p><${UsagesView} store=${store} readOnly />`;
}

export class UsagesPopover {
  // canChange says whether this person may change usages (the owner): an admin reads them, and the refresh, which asks the providers again, is the owner's.
  constructor(source, openSettings, canChange = () => true) {
    this.source = source; this.openSettings = openSettings; this.canChange = canChange;
    this.toggle = document.querySelector('#usages-toggle');
    this.popover = document.querySelector('#usages-popover');
    // The panel is a column of the chat, not a floating popover: it sits where the members column is.
    this.chat = document.querySelector('.chat'); this.chat.append(this.popover);
    this.body = this.popover.querySelector('.usages-popover-body'); this.body.classList.add('usages-popover-alt');
    this.ticket = 0;
    // A switcher that cannot be read (no switcher, a 503) counts as none, so the figures still show.
    const altSource = Object.create(source); altSource.switcher = () => Promise.resolve(source.switcher?.()).catch(() => null);
    this.store = new UsagesStore({ source: altSource, section: this.body, isOpen: () => !this.popover.hidden, isShown: () => !this.popover.hidden });
    this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.toggle.onclick = event => { event.stopPropagation(); if (this.popover.hidden) this.open(); else this.close(); };
    this.refreshButton = this.popover.querySelector('.usages-popover-refresh');
    this.refreshButton.onclick = () => this.refresh();
    // Cooldown and failure hints from Refresh live outside the body, which a reload replaces.
    this.status = element('p', 'usages-popover-note usages-popover-status'); this.status.setAttribute('role', 'status'); this.status.hidden = true;
    this.popover.querySelector('header').after(this.status);
    // The header's group button drives the view's one shared group-by choice.
    this.groupButton = this.popover.querySelector('.usages-popover-group');
    effect(() => this.syncGroupButton(usagesGroup.value));
    this.groupButton.onclick = () => toggleUsagesGroup();
    this.popover.querySelector('.usages-popover-close').onclick = () => { this.close(); this.toggle.focus(); };
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.popover.hidden) { this.close(); this.toggle.focus(); } });
  }
  // Nothing of what was drawn stays after a sign-out: the tiles, the cached usage, switcher state and credential tokens.
  reset() { this.store.forget(); render(null, this.body); this.ticket++; this.status.hidden = true; }
  open() { this.syncRefresh(); this.status.hidden = true; this.popover.hidden = false; this.chat.classList.add('usages-open'); this.toggle.setAttribute('aria-expanded', 'true'); focusPopover(this.popover); this.load(); }
  close() { this.ticket++; returnFocus(this.popover, this.toggle); this.popover.hidden = true; this.chat.classList.remove('usages-open'); this.toggle.setAttribute('aria-expanded', 'false'); }
  // Ask the server to fetch fresh snapshots (rate limited to one forced refresh per cooldown
  // window, shared with Settings), then re-read the overview the usual way.
  syncRefresh() { const allowed = !!this.canChange(); this.refreshButton.disabled = !allowed; this.refreshButton.title = allowed ? 'Refresh usages' : 'Only the owner can refresh usages'; }
  async refresh() {
    if (this.refreshButton.disabled) return;
    this.refreshButton.disabled = true; this.status.hidden = true;
    try {
      await this.source.refresh();
    } catch (error) {
      this.status.textContent = error.status === 429 && error.retryAfter ? `Refreshed moments ago; try again in ${error.retryAfter}s` : error.message || 'Refresh failed';
      this.status.hidden = false;
    }
    try { await this.load(); }
    finally { this.syncRefresh(); }
  }
  // Live push from the event stream: re-read in place while the popover is open. A closed
  // popover loads fresh on its next open, so there is nothing to keep current.
  // The store holds a push while an input has focus, instead of redrawing it.
  changed() {
    if (this.popover.hidden) return;
    this.store.owner.value = !!this.canChange(); this.store.changed();
  }
  async load() {
    const ticket = ++this.ticket;
    this.store.owner.value = !!this.canChange();
    try {
      await this.store.load();
      if (ticket !== this.ticket) return;
      this.store.setError(''); this.body.querySelector('.usages-popover-note')?.remove();
      render(html`<${PanelView} store=${this.store} />`, this.body);
    } catch (error) {
      if (ticket === this.ticket) { render(null, this.body); this.body.replaceChildren(element('p', 'usages-popover-note', error.status === 403 ? 'Usage details are limited to the instance owner.' : error.message || 'Could not load usage snapshots')); }
    }
  }
  syncGroupButton(group) {
    const activeFirst = group === 'active', label = activeFirst ? 'Group usages by provider' : 'Show active profiles first';
    this.groupButton.classList.toggle('active', activeFirst); this.groupButton.setAttribute('aria-pressed', String(activeFirst)); this.groupButton.setAttribute('aria-label', label); this.groupButton.title = label;
  }
}
