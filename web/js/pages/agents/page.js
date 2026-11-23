import { html, effect } from '../../ui.js';
import { rights } from '../../api.js';
import { mountPreactPage } from '../mount-page.js';
import { AgentsStore } from './store.js';
import { AgentsList, HumansList } from './agents-list.js';
import { SpawnForm } from './spawn-dialog.js';
import { SpawnStore } from './spawn-store.js';
import { SwitchForm } from './switch-form.js';
import { SwitchStore } from './switch-store.js';
import { LogStore } from './log-store.js';
import { LogDialog } from './log-dialog.js';
import { agentLabel } from '../../sender-identity.js';
import { captureError } from '../../telemetry.js';

// How long after a read of the room's agents the next one may start (events that come in the meantime ask for one more, together).
const RESYNC_MS = 500;

// A touch screen wider than a phone and up to 900px (an iPad upright): one side panel at a time.
const TABLET = matchMedia('(min-width: 701px) and (max-width: 900px) and (pointer: coarse)');

// The adapter keeps the panel's public face for app.js, the health popover and the dialogs: the cards are the store's, drawn by the lists
// below, and everything outside them (the panel's title, its close button, the Spawn button) is written from here. The Spawn dialog
// and the Switch dialog are pages of their own over a SpawnStore and a SwitchStore.
export class AgentsPanel {
  constructor(source, onRecovery, onChange, onPrivate, onPrivateHuman) {
    this.source = source; this.onRecovery = onRecovery; this.onChange = onChange; this.onPrivate = onPrivate; this.onPrivateHuman = onPrivateHuman;
    this.panel = document.querySelector('#agents-panel'); this.list = document.querySelector('#agents-list'); this.humans = document.querySelector('#agents-humans'); this.error = document.querySelector('#agents-error');
    this.cardListeners = new Set();
    this.store = new AgentsStore({ source, onError: message => { this.error.textContent = message; }, onChange: agents => this.onChange?.(agents), onCards: () => this.notifyCards(), onSync: () => this.syncSpawnControl() });
    this.api = { store: this.store, source, onPrivate: agent => this.onPrivate?.(agent), onRecovery: agent => this.onRecovery(agent), onPrivateHuman: human => this.onPrivateHuman?.(human),
      openSwitch: id => this.switch.open(id), switchAction: (id, action) => this.switchAction(id, action), canReadOutput: () => this.canReadOutput(), openLogs: (agent, opener) => this.openLogs(agent, opener), canControl: () => !!this.room && ['owner', 'operator'].includes(this.projectRole?.(this.room.project_id)) };
    this.spawnDialog = document.querySelector('#spawn-dialog');
    this.spawn = new SpawnStore(this);
    this.switchDialog = document.querySelector('#switch-dialog');
    this.switch = new SwitchStore(this);
    this.switchPage = mountPreactPage(this.switchDialog, () => html`<${SwitchForm} store=${this.switch} host=${this} />`);
    this.spawnPage = mountPreactPage(this.spawnDialog, () => html`<${SpawnForm} store=${this.spawn} host=${this} />`);
    this.spawnDialog.addEventListener('close', () => this.spawn.closed());
    // The agent log popup: one dialog for the panel, its contents Preact's, opened from the Show agent's logs button of a profile.
    this.logDialog = document.querySelector('#agent-log-dialog');
    this.logs = new LogStore({ source, roomID: () => this.room?.id, agent: id => this.cards.get(id) });
    this.logPage = mountPreactPage(this.logDialog, () => html`<${LogDialog} store=${this.logs} host=${this} />`);
    this.logDialog.addEventListener('close', () => this.logsClosed());
    document.querySelector('#spawn-open').onclick = () => this.openSpawn();
    document.querySelector('#spawn-open').disabled = !source.available;
    document.querySelector('#agents-toggle').onclick = () => {
      if (!matchMedia('(max-width: 700px)').matches) return this.setDesktopOpen(document.querySelector('.chat').classList.contains('agents-collapsed'));
      // On a phone the Agents tab (and the top-bar button) open the members list. A profile left open from an earlier visit would be what it showed instead,
      // and the person asked for the members: it opens only by a tap on a member (or a link that names one).
      const open = !this.panel.classList.contains('mobile-open');
      if (open) this.store.closeProfile();
      this.setMobileOpen(open);
    };
    document.querySelector('#agents-close').onclick = () => matchMedia('(max-width: 700px)').matches ? this.setMobileOpen(false) : this.setDesktopOpen(false);
    document.querySelector('#edge-agents').onclick = () => this.setDesktopOpen(false);
    document.querySelector('#edge-agents-expand').onclick = () => this.setDesktopOpen(true);
    document.querySelector('#agents-scrim').onclick = () => this.setMobileOpen(false);
    this.profileClose = document.querySelector('#agent-profile-close'); this.profileClose.onclick = () => this.closeProfile();
    this.panelTitle = this.panel.querySelector('header h2');
    this.stopPanel = effect(() => {
      const open = !!this.store.profileId.value;
      this.panel.classList.toggle('profile-open', open); this.panelTitle.textContent = open ? 'Agent profile' : 'Members'; this.profileClose.hidden = !open;
    });
    this.onKey = event => { if (event.key === 'Escape' && this.store.profileId.peek() && !document.querySelector('dialog[open]')) { event.preventDefault(); this.closeProfile(); } };
    document.addEventListener('keydown', this.onKey);
    this.listPage = mountPreactPage(this.list, () => html`<${AgentsList} store=${this.store} api=${this.api} />`);
    this.humansPage = mountPreactPage(this.humans, () => html`<${HumansList} store=${this.store} api=${this.api} />`);
    this.unsubscribe = this.source.subscribe(event => this.sourceChanged(event));
    this.switchPoll = setInterval(() => { void this.pollSwitches(); }, 4000);
    // A touch tablet has room for one side panel next to the chat: the members panel starts closed, and opening one panel closes the
    // other. Those changes are not remembered; the desktop preference is.
    if (TABLET.matches) this.setDesktopOpen(false, false);
    else if (localStorage.getItem('aimebu_right_collapsed') === 'true' && !matchMedia('(max-width: 700px)').matches) this.setDesktopOpen(false);
    document.addEventListener('side-panel-open', event => { if (event.detail === 'rooms' && TABLET.matches) this.setDesktopOpen(false, false); });
  }
  get room() { return this.store.room; }
  get cards() { return this.store.agents.peek(); }
  get refreshGeneration() { return this.store.generation; }
  get generation() { return this.store.generation; }
  get spawnMachine() { return this.spawn.machine; }
  get switchMachine() { return this.switch.machine; }
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
    // The members drawer closes when the person moves to another room, not when the same room is selected again (a second selection of it
    // is still finishing, or a refresh): that would take the drawer away from someone who has just opened it.
    this.setRoomFacts(room);
    if (this.drawerRoomID !== room.id) { this.setMobileOpen(false); this.drawerRoomID = room.id; }
    await this.refresh();
    void this.pollSwitches();
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
    this.closeLogs();
    this.store.reset();
    this.drawerRoomID = null;
    this.setMobileOpen(false); this.spawn.invalidate(); this.switch.invalidate(); document.querySelector('#spawn-open').disabled = true;
  }
  setRoomFacts(room) {
    if (this.store.cardsRoomID !== room.id) { this.switch.invalidate(); this.spawn.invalidate(); this.closeLogs(); }
    this.store.setRoomFacts(room);
    this.spawn.touchRoom();
    this.syncSpawnControl();
  }
  syncSpawnControl() {
    if (!this.room) return;
    const spawn = document.querySelector('#spawn-open'), viewer = this.projectRole?.(this.room.project_id) === 'viewer';
    spawn.disabled = !this.source.available || this.room.kind === 'dm' || !!this.room.lifecycle && this.room.lifecycle !== 'open' || viewer;
    this.spawnTitle ||= spawn.title; spawn.title = viewer ? 'Viewers cannot spawn agents' : this.spawnTitle;
  }
  // What the Spawn form asks of the panel.
  showSpawnDialog() { this.spawnDialog.showModal(); }
  setError(message) { this.error.textContent = message; }
  focusName() { this.spawnDialog.querySelector('[name=name]')?.focus(); }
  focusConfirm() { this.spawnDialog.querySelector('#spawn-resume-confirm')?.focus(); }
  canManageHarnesses() { return rights.owner; } // what a machine runs is the owner's alone (admins read it)
  // Settings opens on the machine the dialog was showing.
  openSettings(machineID) {
    this.spawnDialog.close(); this.switchDialog.close();
    document.dispatchEvent(new CustomEvent('aimebu:open-settings', { detail: { section: 'harnesses', machineID } }));
  }
  refresh() { return this.store.refresh(); }
  async pollSwitches() {
    if (this.switchPollBusy || !this.room) return;
    if (this.projectRole?.(this.room.project_id) === 'viewer') return;
    this.switchPollBusy = true;
    const generation = this.generation;
    try {
      for (const agent of this.cards.values()) {
        if (generation !== this.generation) return;
        if (!agent.switch || agent.switch.state === 'started' || agent.switch.state === 'failed') continue;
        try { await this.source.switchProgress(agent.id); }
        catch (error) { captureError(error, 'agents.switch-progress'); /* A transient progress read leaves the card's last known state visible. */ }
      }
    } finally { this.switchPollBusy = false; }
  }
  async switchAction(id, action) {
    try { await this.source.switchAction(id, action); await this.refresh(); }
    catch (error) { this.error.textContent = error.message; }
  }
  // The event stream carries every agent event of every room and project (a turn queued, started, finished, a ping), and a refresh reads the room's agents and
  // members. So an event only counts when it is about this room or one of its agents, and the reads it asks for are coalesced: one runs at once, and any number
  // of events during it or within the next RESYNC_MS ask for exactly one more. (Each event used to start its own pair of reads.)
  sourceChanged(event) {
    if (event?.room_id && this.room && event.room_id !== this.room.id && !(event.agent_id && this.cards.has(event.agent_id))) return undefined;
    if (this.refreshBusy) { this.refreshAgain = true; return undefined; }
    this.refreshBusy = true; this.refreshAgain = false;
    return this.refresh().finally(() => {
      this.refreshTimer = setTimeout(() => { this.refreshTimer = 0; this.refreshBusy = false; if (this.refreshAgain) { this.refreshAgain = false; void this.sourceChanged(); } }, RESYNC_MS);
    });
  }
  control(id, action) { return this.store.control(id, action); }
  openSpawn(options) { return this.spawn.open(options); }
  openSwitch(id) { return this.switch.open(id); }
  openProfile(id) { if (this.store.openProfile(id)) { this.panel.scrollTop = 0; this.list.scrollTop = 0; if (this.list.parentElement) this.list.parentElement.scrollTop = 0; } }
  closeProfile() {
    const id = this.store.closeProfile();
    // The card's open button is back once the list has drawn the closed card (an open card has none: the panel's back arrow closes the profile).
    // The focus goes back only if it is still in the panel (or nowhere): a frame later the person may already be typing elsewhere, and must not be pulled away.
    if (id) requestAnimationFrame(() => { const at = document.activeElement; if (at && at !== document.body && !this.panel.contains(at)) return; this.list.querySelector(`[data-agent-id="${CSS.escape(id)}"] .agent-expand`)?.focus(); });
  }
  // What an agent's harness printed to stderr is for a project's owner or an admin: it can hold whatever its tools printed.
  canReadOutput() { return !!this.room && (rights.admin || this.projectRole?.(this.room.project_id) === 'owner'); }
  // The log popup opens over the panel with the focus on the dialog itself (not the lines field, which would raise a phone's keyboard); closing it,
  // by its button, Escape or a room change, gives the focus back to the button that opened it.
  openLogs(agent, opener) {
    if (!this.canReadOutput() || !agent) return;
    this.logOpener = opener || null; this.logAgentID = agent.id;
    this.logs.open(agent.id);
    if (!this.logDialog.open) this.logDialog.showModal();
    this.logDialog.tabIndex = -1; this.logDialog.focus({ preventScroll: true });
  }
  closeLogs() { if (this.logDialog?.open) this.logDialog.close(); else this.logs?.close(); }
  logsClosed() {
    this.logs.close();
    const id = this.logAgentID, opener = this.logOpener; this.logOpener = null; this.logAgentID = '';
    const target = opener?.isConnected ? opener : id ? this.list.querySelector(`[data-agent-id="${CSS.escape(id)}"] .agent-logs`) : null;
    target?.focus();
  }
  agentName(id) { const agent = this.cards.get(id); return agent ? agentLabel(agent) || agent.slug || id : id; }
  agentSlug(id) { return this.cards.get(id)?.slug || ''; }

  destroy() {
    clearTimeout(this.refreshTimer); this.refreshAgain = false;
    this.stopPanel(); document.removeEventListener('keydown', this.onKey); this.unsubscribe?.(); clearInterval(this.switchPoll); this.store.destroy(); this.spawn.destroy(); this.switch.destroy(); this.listPage.destroy(); this.humansPage.destroy(); this.spawnPage.destroy(); this.switchPage.destroy();
    this.logOpener = null; if (this.logDialog.open) this.logDialog.close(); this.logs.destroy(); this.logPage.destroy();
  }
}
