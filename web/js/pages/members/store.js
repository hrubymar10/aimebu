import { signal } from '../../ui.js';
import { request } from '../../api.js';

// The member list of the room on show. Only the newest load may draw: a slow answer for a room that was closed or replaced, an answer after
// reset() and a failure of an earlier load all do nothing.
export class MembersStore {
  constructor() {
    this.members = signal(null);
    this.status = signal('');
    this.ticket = 0;
    this.alive = true;
  }

  reset() { this.ticket++; this.status.value = ''; this.members.value = null; }

  async load(roomID) {
    if (!this.alive) return;
    const ticket = ++this.ticket;
    this.status.value = 'Loading members…'; this.members.value = null;
    let members;
    try { members = await request(`/api/rooms/${roomID}/members`); }
    catch (error) { if (this.alive && ticket === this.ticket) this.status.value = error.message || 'Could not load the member list'; return; }
    if (!this.alive || ticket !== this.ticket) return;
    this.status.value = ''; this.members.value = members;
  }

  // A list that is already in hand, drawn as a finished load.
  show(members) { if (!this.alive) return; this.ticket++; this.status.value = ''; this.members.value = members; }

  destroy() { this.alive = false; this.ticket++; }
}
