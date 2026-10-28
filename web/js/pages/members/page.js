import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { MembersList } from './members-list.js';
import { MembersStore } from './store.js';

// The Members section of the room dialog: who is in the room (people with their project role, agents with their role emoji and label) and who
// holds each singleton role. The status line stays in index.html and is written from here; Preact fills the list.
export class RoomMembers {
  constructor(root) {
    this.root = root;
    this.status = root.querySelector('.room-members-status');
    this.body = root.querySelector('.room-members-body');
    this.store = new MembersStore();
    this.stopStatus = effect(() => { this.status.textContent = this.store.status.value; });
    this.page = mountPreactPage(this.body, () => html`<${MembersList} store=${this.store} />`);
  }
  reset() { this.store.reset(); }
  load(roomID) { return this.store.load(roomID); }
  render(members) { this.store.show(members); }
  destroy() { this.stopStatus(); this.store.destroy(); this.page.destroy(); }
}
