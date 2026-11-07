import { batch, signal } from '../../ui.js';
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { agentLabel } from '../../sender-identity.js';
import { outputFailure, outputStatus, reconcile } from './model.js';

const PROFILE_MAX_AGE_MS = 30000;

// The agents of the room on show, the people next to them and the open profile. Every read carries the generation of the room it was
// asked in; a room change, a reset or destroy() ends it, so an answer for a view that is gone draws nothing.
export class AgentsStore {
  constructor({ source, onError, onChange, onCards, onSync }) {
    this.source = source; this.onError = onError; this.onChange = onChange; this.onCards = onCards; this.onSync = onSync;
    this.agents = signal(new Map());
    this.humans = signal([]);
    this.loaded = signal(false);
    this.profileId = signal(null);
    this.profileData = signal(null);
    this.output = signal(null);
    this.now = signal(Date.now());
    this.roomInfo = signal({ kind: '', lifecycle: 'open' });
    this.available = signal(source.available !== false);
    this.room = null; this.cardsRoomID = null; this.generation = 0; this.profileLoadedAt = 0; this.outputToken = 0; this.listTicket = 0; this.humansTicket = 0; this.profileTicket = 0;
    this.alive = true;
    this.tick = setInterval(() => { this.now.value = Date.now(); }, 1000);
  }

  clear() {
    batch(() => { this.agents.value = new Map(); this.humans.value = []; this.loaded.value = false; this.profileId.value = null; this.profileData.value = null; this.output.value = null; });
    this.outputToken++;
  }

  // Room facts change before the chat and the agent list finish loading. A new room retires the old cards at once.
  setRoomFacts(room) {
    const changed = this.cardsRoomID !== room.id;
    if (changed) {
      this.generation++; this.cardsRoomID = room.id;
      this.clear();
      this.onCards();
      this.onChange?.([]);
    }
    this.room = room;
    const info = { kind: room.kind || '', lifecycle: room.lifecycle || 'open' }, held = this.roomInfo.peek();
    if (info.kind !== held.kind || info.lifecycle !== held.lifecycle) this.roomInfo.value = info;
    return changed;
  }

  reset() { this.generation++; this.room = null; this.cardsRoomID = null; this.clear(); }

  async refresh() {
    if (!this.alive || !this.room) return;
    const generation = this.generation, roomID = this.room.id, ticket = ++this.listTicket;
    const list = await this.source.list(roomID);
    // Two reads for the same room can finish in either order: only the newest may draw.
    if (!this.alive || generation !== this.generation || this.room?.id !== roomID || ticket !== this.listTicket) return;
    batch(() => { this.agents.value = reconcile(this.agents.peek(), list); this.loaded.value = true; this.available.value = this.source.available !== false; });
    this.onCards();
    this.onSync();
    const id = this.profileId.peek();
    if (id && !this.agents.peek().has(id)) this.closeProfile();
    else if (id && Date.now() - this.profileLoadedAt > PROFILE_MAX_AGE_MS) this.loadProfile(id);
    this.onChange?.(list);
    this.refreshHumans(roomID, generation);
  }

  // The people in the room come from the members list (the agents already come from the agent source).
  async refreshHumans(roomID, generation) {
    const ticket = ++this.humansTicket;
    let members;
    try { members = await request(`/api/rooms/${encodeURIComponent(roomID)}/members`); } catch { return; }
    if (!this.alive || generation !== this.generation || this.room?.id !== roomID || ticket !== this.humansTicket) return;
    this.humans.value = members?.humans || [];
  }

  toggleProfile(id) { if (this.profileId.peek() === id) this.closeProfile(); else this.openProfile(id); }

  openProfile(id) {
    if (!this.alive || !this.agents.peek().has(id)) return false;
    this.outputToken++; this.profileLoadedAt = 0;
    batch(() => { this.profileId.value = id; this.profileData.value = null; this.output.value = { agentID: id, open: false, loading: false, text: '', status: '', loaded: false }; });
    this.loadProfile(id);
    return true;
  }

  closeProfile() {
    const id = this.profileId.peek();
    if (!id) return null;
    this.outputToken++;
    batch(() => { this.profileId.value = null; this.profileData.value = null; this.output.value = null; });
    return id;
  }

  async loadProfile(id) {
    const roomID = this.room?.id, generation = this.generation;
    if (!roomID) return;
    // Opening, closing and every reload take a ticket, so an answer of an earlier one cannot fill a profile that was reopened since.
    const ticket = ++this.profileTicket;
    let profile = null;
    try { profile = await request(`/api/rooms/${encodeURIComponent(roomID)}/agents/${encodeURIComponent(id)}/profile`); } catch { profile = null; }
    if (!this.alive || this.profileId.peek() !== id || generation !== this.generation || this.room?.id !== roomID || ticket !== this.profileTicket) return;
    batch(() => { this.profileData.value = profile; });
    this.profileLoadedAt = Date.now();
  }

  patchOutput(token, patch) { if (token === this.outputToken && this.output.peek()) this.output.value = { ...this.output.peek(), ...patch }; }

  setOutputOpen(open) {
    const state = this.output.peek();
    if (!state) return;
    this.patchOutput(this.outputToken, { open });
    if (open && !state.loaded && !state.loading) this.loadOutput(state.agentID);
  }

  async loadOutput(agentID) {
    const state = this.output.peek(), roomID = this.room?.id, token = this.outputToken;
    if (!this.alive || !state || state.agentID !== agentID || !roomID) return;
    this.patchOutput(token, { loading: true });
    try {
      const view = await this.source.output(roomID, agentID, 200);
      this.patchOutput(token, { text: view.text || '', loaded: true, status: outputStatus(view) });
    } catch (error) {
      this.patchOutput(token, { text: '', loaded: error.code === 'no_output', status: outputFailure(error) });
    } finally { this.patchOutput(token, { loading: false }); }
  }

  // A call belongs to the room it was made in: its refusal, and the read after it, are dropped once the panel moved on.
  async control(id, action) {
    const generation = this.generation;
    this.onError('');
    try {
      await this.source.control(id, action);
      if (this.alive && generation === this.generation) await this.refresh();
    } catch (error) { if (this.alive && generation === this.generation) this.onError(error.message); }
  }

  // Removing an agent is for good (its messages stay, it cannot be started again), so it asks first, naming the agent as the rest of the page does.
  async remove(agent) {
    const generation = this.generation;
    this.onError('');
    const sure = await confirmDialog(`Remove ${agentLabel(agent) || agent.id}? Its messages stay; it can't be started again.`, { title: 'Remove agent', confirm: 'Remove', danger: true });
    if (!sure || !this.alive || generation !== this.generation) return;
    try {
      await this.source.remove(agent.id);
      if (this.alive && generation === this.generation) await this.refresh();
    } catch (error) { if (this.alive && generation === this.generation) this.onError(error.message); }
  }

  destroy() { this.alive = false; this.generation++; this.outputToken++; this.profileTicket++; clearInterval(this.tick); }
}
