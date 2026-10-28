import { batch, signal } from '../../ui.js';
import { request, currentUser, rights } from '../../api.js';
import { heldTools } from '../../limit-guard-format.js';

// The limit guard is read on its own slow rhythm, not on every rebuild of the agent list: usage moves without any agent event.
// It is the owner's: once the server says this person is not the owner, it is not asked again for that sign-in.
export class HealthStore {
  constructor({ sidebar, agents, limitGuard }) {
    this.sidebar = sidebar;
    this.agents = agents;
    this.limitGuard = limitGuard;
    this.rows = signal(null);
    this.guard = signal(null);
    this.guardUser = null;
    this.guardGone = false;
    this.ticket = 0;
    this.limitTicket = 0;
    this.alive = true;
  }

  badge() { return (this.rows.value?.length || 0) + heldTools(this.guard.value); }

  async refresh() {
    if (!this.alive) return;
    const ticket = ++this.ticket;
    let rows;
    try {
      if (new URLSearchParams(location.search).has('mock_agents')) {
        rows = [...this.agents.cards.values()].map(card => ({ id: card.id, name: card.name, room_ids: [card.room_id], flags: card.health_flags || [] }));
      } else rows = (await request('/api/health')).agents || [];
    } catch { return; }
    if (!this.alive || ticket !== this.ticket || document.querySelector('#app-view').hidden) return;
    const affected = rows.filter(row => row.flags?.length);
    // The first rebuild of a sign-in reads the limit guard once; later ones draw what the timer last read.
    if (this.guardUser !== currentUser) { this.guard.value = null; this.guardGone = false; this.guardUser = currentUser; this.limitTicket++; await this.refreshLimits(); if (!this.alive || ticket !== this.ticket) return; }
    this.sidebar.setHealth(affected);
    this.rows.value = affected;
  }

  async refreshLimits() {
    if (!this.alive || this.guardGone || document.querySelector('#app-view').hidden) return;
    const ticket = ++this.limitTicket;
    if (!rights.owner) { batch(() => { this.guard.value = null; this.guardGone = true; }); } // no request that can only answer 403
    else {
      let guard = null, gone = false;
      try { guard = await this.limitGuard.load(); }
      catch (failure) { gone = failure.status === 403 || failure.status === 404; } // not the owner, or not served: do not ask again
      if (!this.alive || ticket !== this.limitTicket) return;
      if (gone) this.guardGone = true;
      this.guard.value = guard;
    }
  }

  destroy() { this.alive = false; this.ticket++; this.limitTicket++; }
}
