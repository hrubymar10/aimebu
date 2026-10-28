import { html, effect } from '../../ui.js';
import { rights } from '../../api.js';
import { mountPreactPage } from '../mount-page.js';
import { SpawnDialogs } from '../../spawn-dialogs.js';
import { AgentsStore } from './store.js';
import { AgentsList, HumansList } from './agents-list.js';

// A touch screen wider than a phone and up to 900px (an iPad upright): one side panel at a time.
const TABLET = matchMedia('(min-width: 701px) and (max-width: 900px) and (pointer: coarse)');

// The adapter keeps the panel's public face for app.js, the health popover and the dialogs: the cards are the store's, drawn by the lists
// below, and everything outside them (the panel's title, its close button, the Spawn button) is written from here. The Spawn and Switch
// dialogs are still the hand-written SpawnDialogs, which read the panel through the getters below.
export class AgentsPanel {
  constructor(source, onRecovery, onChange, onPrivate, onPrivateHuman) {
    this.source = source; this.onRecovery = onRecovery; this.onChange = onChange; this.onPrivate = onPrivate; this.onPrivateHuman = onPrivateHuman;
    this.panel = document.querySelector('#agents-panel'); this.list = document.querySelector('#agents-list'); this.humans = document.querySelector('#agents-humans'); this.error = document.querySelector('#agents-error');
    this.cardListeners = new Set();
    this.store = new AgentsStore({ source, onError: message => { this.error.textContent = message; }, onChange: agents => this.onChange?.(agents), onCards: () => this.notifyCards(), onSync: () => this.dialogs.syncSpawnControl() });
    this.api = { store: this.store, source, onPrivate: agent => this.onPrivate?.(agent), onRecovery: agent => this.onRecovery(agent), onPrivateHuman: human => this.onPrivateHuman?.(human),
      openSwitch: id => this.dialogs.openSwitch(id), canReadOutput: () => this.canReadOutput() };
    this.dialogs = new SpawnDialogs(this);
    document.querySelector('#agents-toggle').onclick = () => matchMedia('(max-width: 700px)').matches
      ? this.setMobileOpen(!this.panel.classList.contains('mobile-open'))
      : this.setDesktopOpen(document.querySelector('.chat').classList.contains('agents-collapsed'));
    document.querySelector('#agents-close').onclick = () => matchMedia('(max-width: 700px)').matches ? this.setMobileOpen(false) : this.setDesktopOpen(false);
    document.querySelector('#edge-agents').onclick = () => this.setDesktopOpen(false);
    document.querySelector('#edge-agents-expand').onclick = () => this.setDesktopOpen(true);
    document.querySelector('#agents-scrim').onclick = () => this.setMobileOpen(false);
    this.profileClose = document.querySelector('#agent-profile-close'); this.profileClose.onclick = () => this.closeProfile();
    this.panelTitle = this.panel.querySelector('header h2');
    this.stopPanel = effect(() => {
      const open = !!this.store.profileId.value;
      this.panel.classList.toggle('profile-open', open); this.panelTitle.textContent = open ? 'Agent profile' : 'Room members'; this.profileClose.hidden = !open;
    });
    this.onKey = event => { if (event.key === 'Escape' && this.store.profileId.peek() && !document.querySelector('dialog[open]')) { event.preventDefault(); this.closeProfile(); } };
    document.addEventListener('keydown', this.onKey);
    this.listPage = mountPreactPage(this.list, () => html`<${AgentsList} store=${this.store} api=${this.api} />`);
    this.humansPage = mountPreactPage(this.humans, () => html`<${HumansList} store=${this.store} api=${this.api} />`);
    this.unsubscribe = this.source.subscribe(() => this.refresh());
    // A touch tablet has room for one side panel next to the chat: the members panel starts closed, and opening one panel closes the
    // other. Those changes are not remembered; the desktop preference is.
    if (TABLET.matches) this.setDesktopOpen(false, false);
    else if (localStorage.getItem('aimebu_right_collapsed') === 'true' && !matchMedia('(max-width: 700px)').matches) this.setDesktopOpen(false);
    document.addEventListener('side-panel-open', event => { if (event.detail === 'rooms' && TABLET.matches) this.setDesktopOpen(false, false); });
  }
  get room() { return this.store.room; }
  get cards() { return this.store.agents.peek(); }
  get refreshGeneration() { return this.store.generation; }
  get spawnMachine() { return this.dialogs.spawnMachine; }
  get switchMachine() { return this.dialogs.switchMachine; }
  setMobileOpen(open) {
    this.panel.classList.toggle('mobile-open', open);
    document.querySelector('.chat').classList.toggle('agents-open', open);
    document.querySelector('#agents-toggle').setAttribute('aria-expanded', String(open));
  }
  setDesktopOpen(open, remember = !TABLET.matches) {
    document.querySelector('.chat').classList.toggle('agents-collapsed', !open);
    document.querySelector('#agents-toggle').setAttribute('aria-expanded', String(open));
    if (remember) localStorage.setItem('aimebu_right_collapsed', String(!open));
    if (open && TABLET.matches) document.dispatchEvent(new CustomEvent('side-panel-open', { detail: 'agents' }));
  }
  async setRoom(room) {
    this.setRoomFacts(room); this.setMobileOpen(false);
    await this.refresh();
  }
  waitForCard(id, roomID, timeout = 2000) {
    if (this.room?.id === roomID && this.cards.has(id)) return Promise.resolve(true);
    return new Promise(resolve => {
      let timer;
      const finish = found => { clearTimeout(timer); this.cardListeners.delete(check); resolve(found); };
      const check = () => { if (this.room?.id === roomID && this.cards.has(id)) finish(true); };
      this.cardListeners.add(check);
      timer = setTimeout(() => finish(false), timeout);
      check();
    });
  }
  notifyCards() { for (const listener of this.cardListeners) listener(); }
  reset() {
    this.store.reset();
    this.setMobileOpen(false); this.dialogs.closeAll(); document.querySelector('#spawn-open').disabled = true;
  }
  setRoomFacts(room) {
    if (this.store.cardsRoomID !== room.id) this.dialogs.roomChanged();
    this.store.setRoomFacts(room);
    this.dialogs.syncSpawnControl();
    this.dialogs.syncWorktreeOption();
  }
  refresh() { return this.store.refresh(); }
  control(id, action) { return this.store.control(id, action); }
  openSpawn(options) { return this.dialogs.openSpawn(options); }
  openSwitch(id) { return this.dialogs.openSwitch(id); }
  openProfile(id) { if (this.store.openProfile(id)) { this.panel.scrollTop = 0; this.list.scrollTop = 0; } }
  closeProfile() {
    const id = this.store.closeProfile();
    if (id) this.list.querySelector(`[data-agent-id="${CSS.escape(id)}"] .agent-expand`)?.focus();
  }
  // What an agent's harness printed to stderr is for a project's owner or an admin: it can hold whatever its tools printed.
  canReadOutput() { return !!this.room && (rights.admin || this.projectRole?.(this.room.project_id) === 'owner'); }

  destroy() {
    this.stopPanel(); document.removeEventListener('keydown', this.onKey); this.unsubscribe?.(); this.store.destroy(); this.listPage.destroy(); this.humansPage.destroy();
  }
}
