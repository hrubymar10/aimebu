import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { InboxPage } from './inbox-page.js';
import { InboxStore } from './store.js';
import { countView } from './model.js';

// The adapter is the one writer of everything outside the view: the toolbar's count and toggle label, the tab title, and the view's own
// hidden with the chat and the room list it replaces. It keeps the unsubscribe the source hands back and calls it on destroy.
export class Inbox {
  constructor(source, projects, rooms, onJump, notifications, onItems) {
    this.source = source;
    this.view = document.querySelector('#inbox-view');
    this.badge = document.querySelector('#inbox-count');
    this.store = new InboxStore({ source, projects, rooms, notifications, onItems });
    this.page = mountPreactPage(this.view, () => html`<${InboxPage} store=${this.store} onJump=${onJump} />`);
    this.stopCount = effect(() => {
      const items = this.store.items.value, view = countView(items ? items.length : 0);
      this.badge.textContent = view.text; this.badge.hidden = view.hidden; this.labelToggle(items ? items.length : 0);
      if (items) document.title = view.title;
    });
    this.unsubscribe = source.subscribe(() => this.refresh());
  }

  get openUsages() { return this.store.openUsages.value; }
  set openUsages(value) { this.store.openUsages.value = value; }
  get items() { return this.store.items.value; }
  set items(value) { this.store.items.value = value; }

  refresh(options) { return this.store.refresh(options); }
  answer(item, value) { return this.store.answer(item, value); }

  // The count is a corner pill, so the accessible name and the tooltip carry it for everyone else.
  labelToggle(count) {
    const toggle = document.querySelector('#inbox-toggle'); if (!toggle) return;
    const name = countView(count).name; toggle.title = name; toggle.setAttribute('aria-label', name);
  }

  reset() { this.store.reset(); this.hide(); }

  show() {
    // On desktop the room list stays beside the Inbox (the view replaces only the chat column), so any room is one click away;
    // on a phone the list is a drawer and stays closed.
    this.view.hidden = false; document.querySelector('.chat').hidden = true; document.querySelector('.sidebar').hidden = matchMedia('(max-width: 700px)').matches; document.querySelector('#app-view').classList.add('inbox-open');
  }

  hide() { this.view.hidden = true; document.querySelector('.chat').hidden = false; document.querySelector('.sidebar').hidden = false; document.querySelector('#app-view').classList.remove('inbox-open'); }
  setRoom(room) { this.activeRoom = room?.id; }
  setError(message) { this.store.setError(message); }

  destroy() { this.unsubscribe?.(); this.stopCount(); this.store.destroy(); this.page.destroy(); }
}
