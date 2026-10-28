import { signal } from '../../ui.js';

// v1's phone navigation: a bottom bar with Rooms, Chat and Agents. It drives the drawers the phone layout already has (the rooms drawer, the members
// drawer, the Inbox view) through their own buttons, which stay in the page but are not shown on a phone, so there is one way to open each and no
// second state to keep in step.
const PHONE = '(max-width: 700px)';
export const TABS = [['rooms', 'Rooms'], ['chat', 'Chat'], ['agents', 'Agents']];

export class MobileTabsStore {
  constructor({ app, agentsPanel }) {
    this.app = app; this.agentsPanel = agentsPanel;
    this.current = signal('');
    this.sync();
    this.observer = new MutationObserver(() => this.sync());
    this.observer.observe(app, { attributes: true, attributeFilter: ['class'] });
    this.observer.observe(agentsPanel, { attributes: true, attributeFilter: ['class'] });
  }

  view() { return this.app.classList.contains('rooms-open') ? 'rooms' : this.agentsPanel.classList.contains('mobile-open') ? 'agents' : 'chat'; }
  sync() { this.current.value = this.app.classList.contains('inbox-open') ? '' : this.view(); }

  show(name) {
    if (!matchMedia(PHONE).matches) return;
    const click = selector => document.querySelector(selector).click();
    if (!document.querySelector('#inbox-view').hidden) click('#inbox-toggle');
    const now = this.view();
    if (name === 'rooms' && now !== 'rooms') { if (now === 'agents') click('#agents-close'); click('#rooms-toggle'); }
    else if (name === 'chat') { if (now === 'rooms') click('#back'); if (now === 'agents') click('#agents-close'); }
    else if (name === 'agents' && now !== 'agents') { if (now === 'rooms') click('#back'); click('#agents-toggle'); }
  }

  destroy() { this.observer.disconnect(); }
}
