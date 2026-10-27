import { MachineSelection } from './machine-selection.js';
import { healthLabel } from './health.js';
import { request, currentUser, rights } from './api.js';
import { harnessIcon, agentLabel } from './sender-identity.js';
import { setGlyph } from './icon-button.js';
import { relativeTime, stateAge } from './time.js';
import { ClaudeSessionPicker } from './claude-sessions.js';
import { chooseModel, toolKey, sessionEntryState, modelChangeWarning } from './spawn-model.js';
import { harnessName } from './harness-version.js';

// Compact icon buttons for a member row (the label is the title and the accessible name; no text of their own).
const ICONS = {
  dm: 'M3 4h14a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H9l-4 3v-3H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z',
  pause: 'M6 4h3v12H6zM11 4h3v12h-3z',
  stop: 'M5 5h10v10H5z',
};
function iconButton(className, label, icon) {
  const button = document.createElement('button'); button.type = 'button'; button.className = `icon-btn ${className}`; button.title = label; button.setAttribute('aria-label', label);
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); svg.setAttribute('viewBox', '0 0 20 20'); svg.setAttribute('width', '13'); svg.setAttribute('height', '13'); svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', ICONS[icon]); path.setAttribute('fill', 'currentColor'); svg.append(path); button.append(svg);
  return button;
}

// The Spawn dialog's "Start in" value that asks for a room of its own, for a continued Claude Code session.
const NEW_ROOM = '__new__';

// A touch screen wider than a phone and up to 900px (an iPad upright): one side panel at a time.
const TABLET = matchMedia('(min-width: 701px) and (max-width: 900px) and (pointer: coarse)');

export class AgentsPanel {
  constructor(source, onRecovery, onChange, onPrivate, onPrivateHuman) {
    this.source = source; this.onRecovery = onRecovery; this.onChange = onChange; this.onPrivate = onPrivate; this.onPrivateHuman = onPrivateHuman;
    this.panel = document.querySelector('#agents-panel'); this.list = document.querySelector('#agents-list'); this.humans = document.querySelector('#agents-humans'); this.humanNodes = new Map(); this.error = document.querySelector('#agents-error'); this.nodes = new Map(); this.cardListeners = new Set();
    this.dialog = document.querySelector('#spawn-dialog'); this.form = document.querySelector('#spawn-form');
    const worktreeLabel = document.createElement('label'); worktreeLabel.className = 'spawn-worktree-option';
    worktreeLabel.innerHTML = '<input type="checkbox" name="worktree"><span>Use a Git worktree for this task room</span><small class="spawn-worktree-note" hidden></small>';
    this.form.querySelector('#spawn-agent-fields').before(worktreeLabel);
    const baseLabel = document.createElement('label'); baseLabel.className = 'spawn-worktree-base';
    baseLabel.innerHTML = 'Base branch or ref (optional)<input name="base_ref" placeholder="Current HEAD">';
    worktreeLabel.after(baseLabel);
    this.form.elements.worktree.onchange = () => { baseLabel.hidden = !this.form.elements.worktree.checked; };
    baseLabel.hidden = true;
    this.switchDialog = document.querySelector('#switch-dialog'); this.switchForm = document.querySelector('#switch-form'); this.cards = new Map();
    this.spawnMachine = new MachineSelection('spawn', () => { this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1; this.syncWorktreeOption(); return this.loadSpawnCatalog(); }, () => this.updateSpawnSubmit());
    this.switchMachine = new MachineSelection('switch', () => this.loadSwitchCatalog(), () => this.renderSwitchModels(false));
    document.querySelector('#agents-toggle').onclick = () => matchMedia('(max-width: 700px)').matches
      ? this.setMobileOpen(!this.panel.classList.contains('mobile-open'))
      : this.setDesktopOpen(document.querySelector('.chat').classList.contains('agents-collapsed'));
    document.querySelector('#agents-close').onclick = () => matchMedia('(max-width: 700px)').matches ? this.setMobileOpen(false) : this.setDesktopOpen(false);
    document.querySelector('#edge-agents').onclick = () => this.setDesktopOpen(false);
    document.querySelector('#edge-agents-expand').onclick = () => this.setDesktopOpen(true);
    document.querySelector('#agents-scrim').onclick = () => this.setMobileOpen(false);
    document.querySelector('#spawn-open').onclick = () => this.openSpawn();
    // Continuing a Claude Code session the machine already has: pick it, then the Spawn dialog opens for it.
    this.sessionPicker = new ClaudeSessionPicker(source);
    // A labelled button, not a bare glyph that reads as "refresh": it opens the list of Claude Code sessions this machine has.
    this.sessionsButton = setGlyph(document.querySelector('#spawn-sessions'), 'reopen', 'Continue a Claude Code session');
    this.sessionsButton.append(document.createTextNode(' Continue a session…'));
    this.sessionsButton.onclick = () => this.openSessionPicker();
    this.profileClose = document.querySelector('#agent-profile-close'); this.profileClose.onclick = () => this.closeProfile();
    this.panelTitle = this.panel.querySelector('header h2');
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && this.profileID && !document.querySelector('dialog[open]')) { event.preventDefault(); this.closeProfile(); } });
    document.querySelector('#spawn-cancel').onclick = () => this.dialog.close();
    this.dialog.addEventListener('close', () => { this.pendingResume = null; this.renderStartRooms(); this.renderResume(); });
    this.form.elements.tool.onchange = () => {
      this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1;
      // The person's tool is remembered on its own, so a catalog reload keeps it even when they never touched the model.
      const picked = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
      this.pickedToolKey = toolKey(picked);
      this.renderSpawnModels();
    };
    // What the person picked is kept while the list is rebuilt (the catalog arriving late, the same tool picked again): see renderSpawnModels.
    this.form.elements.model.onchange = () => {
      this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1;
      const tool = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
      this.pickedModel = { toolKey: toolKey(tool), model: this.form.elements.model.value };
      if (this.pendingResume) this.renderResume(true); // the warning about continuing a session on another model follows the choice
    };
    this.form.elements.role.onchange = () => this.prefillRoleDefault();
    this.nameInput = this.form.elements.name; this.nameError = this.form.querySelector('#spawn-name-error');
    this.nameDice = setGlyph(this.form.querySelector('#spawn-name-dice'), 'dice', 'Another name');
    this.nameDice.onclick = () => this.fillSpawnName();
    this.nameInput.oninput = () => { this.nameRequestID = (this.nameRequestID || 0) + 1; this.nameError.textContent = ''; };
    this.form.elements.start_room.onchange = () => { this.syncWorktreeOption(); this.updateSpawnSubmit(); };
    this.form.onsubmit = event => { event.preventDefault(); this.spawn(); };
    document.querySelector('#spawn-resume-mode').onchange = () => this.renderResume(true);
    document.querySelector('#switch-cancel').onclick = () => this.switchDialog.close();
    this.switchForm.elements.tool.onchange = () => this.renderSwitchModels();
    this.switchForm.elements.model.onchange = () => this.renderSwitchModels(false);
    this.switchForm.onsubmit = event => { event.preventDefault(); this.switchAgent(); };
    this.source.subscribe(() => this.refresh());
    document.querySelector('#spawn-open').disabled = !source.available;
    setInterval(() => this.updateLiveChips(), 1000);
    // A touch tablet has room for one side panel next to the chat: the members panel starts closed, and opening one panel closes the
    // other. Those changes are not remembered; the desktop preference is.
    if (TABLET.matches) this.setDesktopOpen(false, false);
    else if (localStorage.getItem('aimebu_right_collapsed') === 'true' && !matchMedia('(max-width: 700px)').matches) this.setDesktopOpen(false);
    document.addEventListener('side-panel-open', event => { if (event.detail === 'rooms' && TABLET.matches) this.setDesktopOpen(false, false); });
  }
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
  reset() { this.refreshGeneration = (this.refreshGeneration || 0) + 1; this.room = null; this.cardsRoomID = null; this.profileID = null; this.panel.classList.remove('profile-open'); this.panelTitle.textContent = 'Room members'; this.profileClose.hidden = true; this.cards.clear(); this.nodes.clear(); this.list.replaceChildren(); this.humans.replaceChildren(); this.humanNodes.clear(); this.setMobileOpen(false); this.dialog.close(); this.switchDialog.close(); document.querySelector('#spawn-open').disabled = true; }
  setRoomFacts(room) {
    // Room facts change before the chat and agent list finish loading. Use the
    // room represented by the visible cards to retire old controls at once.
    if (this.cardsRoomID !== room.id) {
      this.refreshGeneration = (this.refreshGeneration || 0) + 1;
      this.cardsRoomID = room.id;
      this.switchDialog.close(); this.switchAgentID = null;
      this.profileID = null; this.panel.classList.remove('profile-open'); this.panelTitle.textContent = 'Room members'; this.profileClose.hidden = true;
      this.cards.clear(); this.nodes.clear(); this.list.replaceChildren(); this.humans.replaceChildren(); this.humanNodes.clear();
      this.notifyCards();
      this.onChange?.([]);
    }
    this.room = room;
    this.syncSpawnControl();
    this.syncWorktreeOption();
  }
  syncSpawnControl() {
    if (!this.room) return;
    const spawn = document.querySelector('#spawn-open'), viewer = this.projectRole?.(this.room.project_id) === 'viewer';
    spawn.disabled = !this.source.available || this.room.kind === 'dm' || !!this.room.lifecycle && this.room.lifecycle !== 'open' || viewer;
    this.spawnTitle ||= spawn.title; spawn.title = viewer ? 'Viewers cannot spawn agents' : this.spawnTitle;
  }
  // The worktree choice exists only in a task room, and is offered only where the server would make one: the machine's row carries the
  // server's own check for the host (with git's reason when it refuses), and is silent for other machines, where the box stays available.
  syncWorktreeOption() {
    const option = this.form.querySelector('.spawn-worktree-option'), box = this.form.elements.worktree;
    const check = this.spawnMachine?.machine?.project_worktree, refused = check?.ok === false;
    option.hidden = this.spawnRoom()?.kind !== 'task' || !!this.pendingResume?.resumable;
    box.disabled = refused;
    if (refused) { box.checked = false; this.form.querySelector('.spawn-worktree-base').hidden = true; }
    const note = option.querySelector('.spawn-worktree-note');
    note.hidden = !refused; note.textContent = refused ? `No worktree here: ${check.reason || 'the project folder cannot have one.'}` : '';
  }
  async refresh() {
    if (!this.room) return;
    const generation = this.refreshGeneration;
    const roomID = this.room.id;
    const agents = await this.source.list(roomID);
    if (generation !== this.refreshGeneration || this.room?.id !== roomID) return;
    this.cards = new Map(agents.map(agent => [agent.id, agent]));
    this.notifyCards();
    this.syncSpawnControl();
    const active = new Set(agents.map(agent => agent.id));
    for (const [id, node] of this.nodes) if (!active.has(id)) { node.remove(); this.nodes.delete(id); }
    for (const agent of agents) { let node = this.nodes.get(agent.id); if (!node) { node = this.card(agent); this.nodes.set(agent.id, node); this.list.append(node); } this.update(node, agent); }
    if (this.profileID && !active.has(this.profileID)) this.closeProfile();
    else if (this.profileID) { this.renderProfile(this.nodes.get(this.profileID), this.cards.get(this.profileID)); if (Date.now() - (this.profileLoadedAt || 0) > 30000) this.loadProfile(this.profileID); }
    this.onChange?.(agents);
    this.refreshHumans(roomID, generation);
    let empty = this.list.querySelector('.agents-empty');
    if (!agents.length && !empty) { empty = document.createElement('p'); empty.className = 'agents-empty'; empty.textContent = 'No agents in this room'; this.list.append(empty); }
    else if (agents.length) empty?.remove();
  }
  // The people in the room come from the members list (the agents already come from the agent source).
  async refreshHumans(roomID, generation) {
    let members;
    try { members = await request(`/api/rooms/${encodeURIComponent(roomID)}/members`); } catch { return; }
    if (generation !== this.refreshGeneration || this.room?.id !== roomID) return;
    const people = members?.humans || [];
    const present = new Set(people.map(human => human.id));
    for (const [id, node] of this.humanNodes) if (!present.has(id)) { node.remove(); this.humanNodes.delete(id); }
    for (const human of people) {
      let node = this.humanNodes.get(human.id);
      if (!node) {
        node = document.createElement('article'); node.className = 'agent-card human-card'; node.dataset.humanId = human.id;
        const title = document.createElement('div'); title.className = 'agent-title';
        const dot = document.createElement('span'); dot.className = 'presence-dot'; dot.setAttribute('aria-hidden', 'true');
        const glyph = document.createElement('img'); glyph.className = 'agent-harness-icon'; glyph.src = '/icons/human.svg'; glyph.alt = ''; glyph.title = 'human'; glyph.setAttribute('aria-hidden', 'true');
        const name = document.createElement('strong'); name.className = 'agent-name human-name';
        const role = document.createElement('small'); role.className = 'human-role';
        const quick = document.createElement('div'); quick.className = 'agent-quick';
        title.append(dot, glyph, name, role, quick); node.append(title);
        this.humanNodes.set(human.id, node); this.humans.append(node);
      }
      node.querySelector('.human-name').textContent = human.login;
      node.querySelector('.human-role').textContent = human.role || '';
      const quick = node.querySelector('.agent-quick');
      const mine = human.id === currentUser;
      node.classList.toggle('is-you', mine);
      if (mine) node.querySelector('.human-name').title = 'You';
      if (!mine && !quick.children.length && this.room?.kind !== 'dm') {
        const dm = iconButton('human-dm', `Message ${human.login} privately`, 'dm');
        dm.onclick = () => this.onPrivateHuman?.(human); quick.append(dm);
      }
    }
  }
  // The Agent profile: clicking a member opens one agent's full detail in place of the members list (as in v1);
  // the close button, Escape or the agent leaving the room brings the list back. The card itself becomes the
  // profile, so the same elements are updated in both views.
  toggleProfile(id) { if (this.profileID === id) this.closeProfile(); else this.openProfile(id); }
  openProfile(id) {
    const node = this.nodes.get(id), agent = this.cards.get(id);
    if (!node || !agent) return;
    for (const other of this.nodes.values()) other.classList.toggle('expanded', other === node);
    this.profileID = id; this.profileData = null; this.profileLoadedAt = 0; this.output = null;
    this.panel.classList.add('profile-open');
    this.panelTitle.textContent = 'Agent profile'; this.profileClose.hidden = false;
    for (const [other, item] of this.nodes) { const button = item.querySelector('.agent-expand'); button.setAttribute('aria-expanded', String(other === id)); setGlyph(button, other === id ? 'close' : 'chevron-right', other === id ? 'Close agent profile' : 'Open agent profile'); }
    this.renderProfile(node, agent);
    this.loadProfile(id);
    this.panel.scrollTop = 0; this.list.scrollTop = 0;
  }
  closeProfile() {
    if (!this.profileID) return;
    const id = this.profileID; this.profileID = null; this.profileData = null; this.output = null;
    for (const item of this.nodes.values()) { item.classList.remove('expanded'); const button = item.querySelector('.agent-expand'); button.setAttribute('aria-expanded', 'false'); setGlyph(button, 'chevron-right', 'Open agent profile'); }
    this.panel.classList.remove('profile-open');
    this.panelTitle.textContent = 'Room members'; this.profileClose.hidden = true;
    this.nodes.get(id)?.querySelector('.agent-expand')?.focus();
  }
  async loadProfile(id) {
    const roomID = this.room?.id, generation = this.refreshGeneration;
    if (!roomID) return;
    let profile = null;
    try { profile = await request(`/api/rooms/${encodeURIComponent(roomID)}/agents/${encodeURIComponent(id)}/profile`); } catch { profile = null; }
    if (this.profileID !== id || generation !== this.refreshGeneration || this.room?.id !== roomID) return;
    this.profileData = profile; this.profileLoadedAt = Date.now();
    const node = this.nodes.get(id), agent = this.cards.get(id);
    if (node && agent) this.renderProfile(node, agent);
  }
  // One label/value row; ids, paths and commands are monospace, and a command can be copied.
  fact(label, value, { mono = false, copy = false } = {}) {
    if (value === undefined || value === null || value === '') return null;
    const row = document.createElement('div'); row.className = 'profile-fact';
    const key = document.createElement('span'); key.className = 'profile-fact-label'; key.textContent = label;
    const text = document.createElement(mono ? 'code' : 'strong'); text.className = 'profile-fact-value'; text.textContent = String(value);
    row.append(key, text);
    if (copy) {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'icon-btn profile-copy'; const copyLabel = `Copy ${label.toLowerCase()}`; setGlyph(button, 'copy', copyLabel);
      button.onclick = async () => {
        try { await navigator.clipboard.writeText(String(value)); setGlyph(button, 'check', 'Copied'); } catch { setGlyph(button, 'copy', 'Copy failed: press Ctrl or ⌘ C'); const range = document.createRange(); range.selectNodeContents(text); const sel = getSelection(); sel.removeAllRanges(); sel.addRange(range); }
        setTimeout(() => setGlyph(button, 'copy', copyLabel), 1500);
      };
      row.append(button);
    }
    return row;
  }
  renderProfile(node, agent) {
    const box = node.querySelector('.agent-profile-facts');
    if (this.profileID !== agent.id) { box.replaceChildren(); return; }
    const profile = this.profileData || {};
    const status = agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Paused' : agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : 'Online';
    const seen = agent.last_activity_at || agent.machine_last_seen;
    const variant = agent.variant && agent.variant !== agent.tool ? `${harnessName(agent.tool)}/${agent.variant}` : harnessName(agent.tool);
    const today = profile.turns_today || profile.cost_today ? `${profile.turns_today || 0} turn${profile.turns_today === 1 ? '' : 's'}${profile.cost_today ? ` · $${profile.cost_today.toFixed(profile.cost_today < 0.01 ? 4 : 2)}${profile.cost_today_estimated ? ' estimated' : ''}` : ''}` : '';
    const rows = [
      this.fact('Status', status), this.fact('Presence', agent.activity || (agent.observed === 'in_turn' ? 'Working on a turn' : agent.observed === 'lost' ? 'Unconfirmed; recovery needed' : 'Idle')),
      this.fact('Last seen', seen ? relativeTime(seen) : ''), this.fact('Runtime', `${agent.model || 'unknown'} · ${variant || 'unknown'}`),
      this.fact('Role', `${agent.role_emoji ? agent.role_emoji + ' ' : ''}${agent.role_key || agent.role || ''}`.trim()),
      this.fact('Session', profile.native_session_id, { mono: true }), this.fact('Resume', profile.resume_command, { mono: true, copy: true }),
      this.fact('CWD', profile.cwd, { mono: true }), this.fact('Branch', profile.branch, { mono: true }),
      this.fact('Machine', profile.machine_name), this.fact('Launcher', profile.launcher_name),
      this.fact('Project', profile.project_name), this.fact('Repo', profile.repo_url, { mono: true }),
      this.fact('Spawned', profile.spawned_by ? `${profile.spawned_by}${profile.spawned_at ? ` · ${relativeTime(profile.spawned_at)}` : ''}` : ''),
      profile.resumed_from ? this.fact('Continues', `the v1 session of ${profile.resumed_from}`) : null,
      this.fact('Last 24 h', today), this.fact('Agent ID', agent.id, { mono: true }),
    ].filter(Boolean);
    const list = document.createElement('div'); list.className = 'profile-facts-list'; list.append(...rows);
    // The Output section is kept across redraws (they run with every refresh of the member list): replacing it would close it under a
    // person's click and drop where they were reading.
    const output = box.querySelector('.profile-output');
    box.replaceChildren(list);
    if (this.canReadOutput()) box.append(output && this.output?.agentID === agent.id ? output : this.outputSection(agent));
  }
  // What an agent's harness printed to stderr is for a project's owner or an admin: it can hold whatever its tools printed.
  canReadOutput() { return !!this.room && (rights.admin || this.projectRole?.(this.room.project_id) === 'owner'); }
  // The Output section of the profile: collapsed until asked for, read from the machine that ran the agent when it is opened or
  // refreshed. It belongs to one agent and is built once; what it shows follows this.output (see updateOutput).
  outputSection(agent) {
    this.output = { agentID: agent.id, open: false, loading: false, text: '', status: '', loaded: false };
    const state = this.output;
    const section = document.createElement('details'); section.className = 'profile-output';
    const summary = document.createElement('summary'); summary.textContent = 'Output';
    const bar = document.createElement('div'); bar.className = 'profile-output-bar';
    const refresh = document.createElement('button'); refresh.type = 'button'; refresh.className = 'icon-btn profile-output-refresh'; setGlyph(refresh, 'refresh', 'Refresh output');
    const download = document.createElement('button'); download.type = 'button'; download.className = 'icon-btn profile-output-download'; setGlyph(download, 'arrow-down', 'Download output'); download.disabled = true;
    bar.append(refresh, download);
    const note = document.createElement('p'); note.className = 'profile-output-note';
    note.textContent = 'The end of what this agent\'s harness printed to stderr, kept on its machine for 14 days. It can contain secrets from tool output.';
    const text = document.createElement('pre'); text.className = 'profile-output-text'; text.tabIndex = 0; text.setAttribute('role', 'region'); text.setAttribute('aria-label', 'Agent output'); text.hidden = true;
    const status = document.createElement('p'); status.className = 'profile-output-status'; status.setAttribute('role', 'status');
    section.append(summary, bar, note, text, status);
    const slug = agent.slug || agent.id;
    section.addEventListener('toggle', () => { state.open = section.open; if (section.open && !state.loaded && !state.loading) this.loadOutput(agent.id); });
    refresh.onclick = () => this.loadOutput(agent.id);
    download.onclick = () => {
      const url = URL.createObjectURL(new Blob([state.text], { type: 'text/plain;charset=utf-8' }));
      const link = document.createElement('a'); link.href = url; link.download = `${slug}-output.txt`; document.body.append(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    };
    return section;
  }
  async loadOutput(agentID) {
    const state = this.output, roomID = this.room?.id;
    if (!state || state.agentID !== agentID || !roomID) return;
    state.loading = true; this.updateOutput();
    try {
      const view = await this.source.output(roomID, agentID, 200);
      if (state !== this.output) return;
      state.text = view.text || ''; state.loaded = true;
      state.status = !view.text ? 'No output has been captured for this agent yet.' : view.truncated ? 'Earlier output is not shown.' : '';
    } catch (error) {
      if (state !== this.output) return;
      state.text = ''; state.loaded = error.code === 'no_output';
      state.status = error.code === 'no_output' ? 'No output has been captured for this agent yet.' : (error.message || 'The output could not be read.');
    } finally { if (state === this.output) { state.loading = false; this.updateOutput(); } }
  }
  // Brings the section on screen up to date with its state, in place.
  updateOutput() {
    const state = this.output, node = state && this.nodes.get(state.agentID), section = node?.querySelector('.profile-output');
    if (!section || this.profileID !== state.agentID) return;
    const text = section.querySelector('.profile-output-text');
    text.textContent = state.text; text.hidden = !state.text;
    section.querySelector('.profile-output-status').textContent = state.loading ? 'Loading…' : state.status;
    section.querySelector('.profile-output-refresh').disabled = state.loading;
    section.querySelector('.profile-output-download').disabled = !state.text;
  }
  card(agent) {
    const card = document.createElement('article'); card.className = 'agent-card'; card.dataset.agentId = agent.id;
    const title = document.createElement('div'); title.className = 'agent-title';
    const dot = document.createElement('span'); dot.className = 'presence-dot agent-dot'; dot.setAttribute('aria-hidden', 'true');
    const emoji = document.createElement('span'); emoji.className = 'agent-emoji'; emoji.setAttribute('aria-hidden', 'true');
    const identity = document.createElement('span'); identity.className = 'agent-identity';
    const icon = document.createElement('img'); icon.className = 'agent-harness-icon'; icon.alt = '';
    const name = document.createElement('strong'); name.className = 'agent-name'; identity.append(name);
    const role = document.createElement('span'); role.className = 'agent-role';
    const expand = document.createElement('button'); expand.type = 'button'; expand.className = 'icon-btn agent-expand'; setGlyph(expand, 'chevron-right', 'Open agent profile'); expand.setAttribute('aria-expanded', 'false');
    expand.onclick = () => this.toggleProfile(agent.id);
    const quick = document.createElement('div'); quick.className = 'agent-quick';
    // v1's state reading: the state and how long it has lasted, "idle 0:44". The label and the age are separate so the age can tick.
    const state = document.createElement('div'); state.className = 'agent-state';
    const stateLabel = document.createElement('span'); stateLabel.className = 'agent-state-label'; const stateAge = document.createElement('span'); stateAge.className = 'agent-state-age'; const statePill = document.createElement('span'); statePill.className = 'agent-state-pill'; statePill.append(stateLabel, document.createTextNode(' '), stateAge); state.append(statePill);
    title.append(dot, icon, emoji, identity, state, role, quick); card.append(title);
    // A click anywhere on the row that is not one of its buttons opens the agent's profile; the Output section (a disclosure and text to select) is not part of that.
    card.onclick = event => { if (!event.target.closest('button, a, input, select, textarea, .profile-output')) expand.click(); };
    const meta = document.createElement('small'); meta.className = 'agent-meta'; card.append(meta);
    const roleUpdate = document.createElement('small'); roleUpdate.className = 'agent-role-update'; card.append(roleUpdate);
    const health = document.createElement('div'); health.className = 'agent-health'; card.append(health);
    const activity = document.createElement('p'); activity.className = 'agent-activity'; card.append(activity);
    const switchStatus = document.createElement('p'); switchStatus.className = 'agent-switch-status'; switchStatus.hidden = true; card.append(switchStatus);
    const boundaryGrants = document.createElement('small'); boundaryGrants.className = 'agent-boundary-grants'; boundaryGrants.hidden = true; card.append(boundaryGrants);
    const context = document.createElement('div'); context.className = 'agent-context';
    const contextLabel = document.createElement('span'); contextLabel.textContent = 'Memory';
    const contextTrack = document.createElement('span'); contextTrack.className = 'agent-context-track';
    const contextFill = document.createElement('span'); contextFill.className = 'agent-context-fill'; contextTrack.append(contextFill);
    const contextValue = document.createElement('span'); contextValue.className = 'agent-context-value';
    context.append(contextLabel, contextTrack, contextValue); card.append(context);
    const live = document.createElement('span'); live.className = 'agent-live-chip'; live.hidden = true; card.append(live);
    const recovery = document.createElement('button'); recovery.type = 'button'; recovery.className = 'icon-btn agent-recovery'; setGlyph(recovery, 'health', 'Recovery needed'); recovery.onclick = () => this.onRecovery(agent); card.append(recovery);
    const facts = document.createElement('div'); facts.className = 'agent-profile-facts'; card.append(facts);
    const actions = document.createElement('div'); actions.className = 'agent-actions';
    const privateButton = iconButton('agent-private', `Message ${agent.name || agent.slug || 'agent'} privately`, 'dm'); privateButton.onclick = () => this.onPrivate?.(agent); quick.prepend(privateButton);
    // The row's hover icons and the profile's text buttons do the same things; the profile shows the labelled ones.
    for (const [label, action, icon] of [['Pause', 'pause', 'pause'], ['Stop', 'stop', 'stop']]) {
      const button = iconButton(`agent-${action}`, label, icon); if (action === 'stop') button.classList.add('icon-btn-danger');
      button.onclick = () => this.control(agent.id, action); quick.append(button);
    }
    const iconAction = (name, label, glyph, className, handler) => { const button = document.createElement('button'); button.type = 'button'; button.className = `icon-btn ${className} agent-act agent-${name}`.replace(/\s+/g, ' '); setGlyph(button, glyph, label); button.onclick = handler; return button; };
    const hints = document.createElement('div'); hints.className = 'agent-action-hints';
    const list = [
      ['message', 'Message', 'message', '', () => this.onPrivate?.(agent)],
      ['act-pause', 'Pause', 'pause', '', () => this.control(agent.id, 'pause')],
      ['act-stop', 'Stop', 'stop', 'icon-btn-danger', () => this.control(agent.id, 'stop')],
      ['switch', 'Switch…', 'switch', '', () => this.openSwitch(agent.id)],
      ['reactivate', 'Reactivate', 'play', '', () => this.control(agent.id, 'reactivate')],
    ];
    for (const [name, label, glyph, className, handler] of list) {
      const button = iconAction(name, label, glyph, className, handler); actions.append(button);
      const hint = document.createElement('small'); hint.className = 'agent-action-hint'; hint.id = `agent-hint-${agent.id}-${name}`; hint.hidden = true; hints.append(hint);
    }
    quick.append(expand);
    card.append(actions, hints); return card;
  }
  update(node, agent) {
    { const name = node.querySelector('.agent-name'); name.textContent = agentLabel(agent) || agent.id; name.title = name.textContent; }
    node.querySelector('.agent-emoji').textContent = agent.role_emoji || '';
    { const dm = node.querySelector('.agent-private'); dm.setAttribute('aria-label', `Message ${agent.name || agent.slug || 'agent'} privately`); dm.title = dm.getAttribute('aria-label'); }
    node.querySelector('.agent-role').textContent = `${agent.role_emoji || ''} ${agent.role || 'Agent'}${agent.role_deleted ? ' (deleted)' : ''}`.trim();
    node.querySelector('.agent-role').title = agent.role_updated ? 'Role updated; respawn to apply the project definition.' : '';
    node.querySelector('.agent-role-update').textContent = agent.role_updated ? 'Role updated · respawn to apply' : '';
    node.querySelector('.agent-meta').textContent = `${harnessName(agent.tool) || 'unknown'} · ${agent.model || 'unknown'}`;
    const icon = node.querySelector('.agent-harness-icon');
    const { src, label } = harnessIcon(agent.tool || '');
    icon.src = src;
    icon.title = `${agent.model || 'unknown'} · ${label}`;
    const interrupted = agent.observed === 'interrupting' || (agent.desired === 'stopped' && agent.observed !== 'stopped');
    const control = interrupted ? 'Interrupt requested' : agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Not accepting' : 'Active';
    const observed = agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : agent.observed === 'interrupting' ? 'Interrupting' : 'Idle';
    const state = node.querySelector('.agent-state');
    const stateName = agent.boundary_pending ? 'awaiting approval' : interrupted ? 'stopping' : agent.observed === 'in_turn' ? 'working' : agent.observed === 'lost' ? 'lost' : agent.desired === 'stopped' ? 'stopped' : agent.desired === 'not_accepting' ? 'paused' : 'idle';
    state.querySelector('.agent-state-label').textContent = stateName;
    // How long it has been so: a working agent counts from the start of its turn, any other state from the last thing it did.
    state.dataset.since = (agent.observed === 'in_turn' ? agent.activity_at || agent.last_activity_at : agent.last_activity_at) || '';
    this.updateStateAge(state);
    state.dataset.state = agent.boundary_pending ? 'paused'
      : interrupted || agent.observed === 'in_turn' ? 'working'
      : agent.observed === 'lost' ? 'error'
      : agent.desired === 'stopped' ? 'stopped'
      : agent.desired === 'not_accepting' ? 'paused' : 'idle';
    state.title = agent.boundary_pending ? 'Waiting for project owner approval in Inbox.'
      : agent.desired === 'stopped' ? 'Stopped; no new work is accepted.'
      : agent.desired === 'not_accepting' ? 'Paused; no new work is accepted.'
      : agent.observed === 'in_turn' ? 'Working on a turn.'
      : agent.observed === 'lost' ? 'The previous process is unconfirmed; recovery is needed.'
      : 'Idle; waiting for new work.';
    if (agent.recovery_block && agent.last_exit_evidence) state.title += `\n${agent.last_exit_evidence}`;
    node.querySelector('.agent-dot').dataset.state = state.dataset.state; node.querySelector('.agent-dot').title = state.title;
    const health = node.querySelector('.agent-health');
    health.replaceChildren(...(agent.health_flags || []).map(flag => {
      const pill = document.createElement('span'); pill.className = `health-flag health-${flag.code}`;
      pill.textContent = healthLabel(flag); pill.title = healthLabel(flag); return pill;
    }));
    // A turn that began because another agent replied to this one says so, so an exchange between agents is visible as it happens.
    const woken = agent.woken_by === 'reply' ? 'Woken by reply · ' : '';
    node.querySelector('.agent-activity').textContent = woken + (agent.activity || (observed === 'Working' ? 'Working…' : 'No recent activity'));
    node.dataset.wokenBy = agent.woken_by || '';
    const switchStatus = node.querySelector('.agent-switch-status');
    const handoff = agent.switch;
    const phase = { draining: 'Finishing current turn', stopping: 'Stopping old session', starting: 'Starting new session', failed: 'Switch failed' }[handoff?.state];
    switchStatus.hidden = !phase && !agent.boundary_pending;
    if (agent.boundary_pending) {
      switchStatus.textContent = 'Folder or machine approval needed · Open Inbox';
      switchStatus.onclick = () => document.querySelector('#inbox-toggle')?.click();
    } else if (phase) {
      switchStatus.textContent = `${phase}: ${harnessName(handoff.tool)} · ${handoff.model}${handoff.state === 'failed' && handoff.error ? ` — ${handoff.error}` : ''}`;
      switchStatus.onclick = null;
    } else switchStatus.onclick = null;
    const grants = node.querySelector('.agent-boundary-grants');
    grants.hidden = !(agent.boundary_grants || []).length;
    grants.textContent = (agent.boundary_grants || []).map(grant => `Approved: ${grant.machine_id} · ${grant.path}`).join(' · ');
    this.updateContext(node, agent.latest_usage);
    const last = agent.last_activity_at || agent.activity_at;
    const seen = agent.machine_last_seen;
    node.querySelector('.agent-activity').title = [last && `Last agent event: ${new Date(last).toLocaleString()}`, seen && `Supervisor seen: ${new Date(seen).toLocaleString()}`].filter(Boolean).join(' · ') || 'No activity time recorded';
    node.dataset.liveAt = agent.observed === 'in_turn' ? (agent.activity_at || agent.last_activity_at || '') : '';
    node.dataset.liveLabel = /^tool\b/i.test(agent.activity || '') ? 'tool' : 'working';
    this.updateLiveChip(node);
    const recovery = node.querySelector('.agent-recovery'); recovery.hidden = !agent.recovery_block; recovery.onclick = () => this.onRecovery(agent);
    // Every action says what it does, and a disabled one says why, both as a tooltip and as a line under the buttons.
    const down = !this.source.available ? 'Agent controls are unavailable' : '';
    const quotaFlag = (agent.health_flags || []).some(flag => flag.code === 'out_of_quota');
    const reasons = {
      message: ['Send this agent a private message', ''],
      'act-pause': ['Pause: finish the current turn, then accept no new work', down || (agent.desired === 'not_accepting' ? 'it is already paused' : agent.desired === 'stopped' ? 'it is stopped; reactivate it first' : '')],
      'act-stop': ['Stop: interrupt the current turn and accept no new work', down || (agent.desired === 'stopped' ? 'it is already stopped' : '')],
      reactivate: ['Reactivate: accept new work again', down || (agent.boundary_pending ? 'it is waiting for folder or machine approval' : agent.role_deleted ? 'its role was deleted' : agent.desired === 'active' && !quotaFlag ? 'it is already active' : '')],
      switch: ['Switch: change this agent\'s tool, model or machine', down || (this.room?.lifecycle && this.room.lifecycle !== 'open' ? 'reopen the room first' : agent.recovery_block ? 'resolve the recovery item first' : agent.desired === 'stopped' ? 'reactivate it first' : agent.boundary_pending ? 'it is waiting for folder or machine approval' : handoff && handoff.state !== 'failed' ? 'a switch is already in progress' : '')],
    };
    const labels = { message: 'Message', 'act-pause': 'Pause', 'act-stop': 'Stop', reactivate: 'Reactivate', switch: 'Switch' };
    for (const [name, [what, why]] of Object.entries(reasons)) {
      const button = node.querySelector(`.agent-${name}`), hint = node.querySelector(`#agent-hint-${CSS.escape(agent.id)}-${name}`);
      button.disabled = !!why;
      button.setAttribute('aria-label', labels[name]);
      button.title = why ? `${labels[name]} is unavailable: ${why}` : what;
      hint.hidden = !why; hint.textContent = why ? `${labels[name]} is unavailable: ${why}.` : '';
      if (why) button.setAttribute('aria-describedby', hint.id); else button.removeAttribute('aria-describedby');
    }
    node.querySelector('.agent-stop').disabled = !!reasons['act-stop'][1];
    node.querySelector('.agent-pause').disabled = !!reasons['act-pause'][1];
  }
  updateLiveChips() { for (const node of this.nodes.values()) { this.updateLiveChip(node); this.updateStateAge(node.querySelector('.agent-state')); } }
  // The time in a state is M:SS under an hour, then 3h, then 4d (stateAge); nothing when it is unknown.
  updateStateAge(state) {
    const since = Date.parse(state.dataset.since || ''), age = state.querySelector('.agent-state-age');
    if (!Number.isFinite(since)) { age.textContent = ''; return; }
    age.textContent = stateAge((Date.now() - since) / 1000);
  }
  updateContext(node, usage) {
    const context = node.querySelector('.agent-context');
    const track = context.querySelector('.agent-context-track');
    const value = context.querySelector('.agent-context-value');
    const used = usage?.context_used, window = usage?.context_window;
    const usedKnown = Number.isFinite(used) && used >= 0;
    const windowKnown = Number.isFinite(window) && window > 0;
    const known = usedKnown && windowKnown;
    track.hidden = !known;
    track.querySelector('.agent-context-fill').style.width = known ? `${Math.min(100, used / window * 100)}%` : '0%';
    value.textContent = known ? `${Math.round(Math.min(100, used / window * 100))}%` : '—';
    const number = amount => Number.isFinite(amount) && amount >= 0 ? new Intl.NumberFormat().format(amount) : '—';
    const cost = Number.isFinite(usage?.cost_so_far) && usage.cost_so_far >= 0 ? `$${usage.cost_so_far.toFixed(usage.cost_so_far > 0 && usage.cost_so_far < 0.01 ? 4 : 2)}` : '—';
    context.title = `Context ${usedKnown ? number(used) : '—'} / ${windowKnown ? number(window) : '—'} tokens · Input ${number(usage?.tokens_in)} · Output ${number(usage?.tokens_out)} · Cost so far ${cost}${usage?.cost_estimated ? ' (includes estimates)' : ''}`;
    context.setAttribute('aria-label', `Memory: ${context.title}`);
  }
  updateLiveChip(node) {
    const chip = node.querySelector('.agent-live-chip');
    const at = Date.parse(node.dataset.liveAt || '');
    chip.hidden = !Number.isFinite(at);
    if (chip.hidden) return;
    const seconds = Math.max(0, Math.floor((Date.now() - at) / 1000));
    chip.textContent = `${node.dataset.liveLabel} ${stateAge(seconds)}`;
  }
  async control(id, action) { this.error.textContent = ''; try { await this.source.control(id, action); await this.refresh(); } catch (error) { this.error.textContent = error.message; } }
  async openSwitch(id) {
    const agent = this.cards.get(id); if (!agent) return;
    this.switchAgentID = id;
    this.switchRoomID = this.room.id;
    this.switchDialog.showModal();
    document.querySelector('#switch-agent-name').textContent = `${agentLabel(agent) || id} · ${harnessName(agent.tool)} · ${agent.model}`;
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    const reason = document.querySelector('#switch-tool-reason');
    tools.replaceChildren(); models.replaceChildren();
    reason.textContent = 'Loading available tools…'; document.querySelector('#switch-version').textContent = '';
    document.querySelector('#switch-error').textContent = '';
    this.switchForm.querySelector('button[type=submit]').disabled = true;
    try {
      await this.loadOwner(); // before the catalog, whose hints depend on it
      await this.switchMachine.load(this.room.project_id, agent.machine_id);
    } catch (error) {
      if (this.switchDialog.open) reason.textContent = `Machines unavailable: ${error.message}`;
    }
  }
  async loadSwitchCatalog() {
    const id = this.switchAgentID, machineID = this.switchMachine.id;
    const requestID = (this.switchCatalogRequestID || 0) + 1; this.switchCatalogRequestID = requestID;
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    tools.replaceChildren(); models.replaceChildren(); this.switchForm.querySelector('button[type=submit]').disabled = true;
    document.querySelector('#switch-tool-reason').textContent = 'Loading available tools…';
    if (!machineID) return;
    try {
      const result = await this.source.catalog(machineID);
      if (!this.switchDialog.open || this.switchAgentID !== id || requestID !== this.switchCatalogRequestID) return;
      this.switchCatalog = Array.isArray(result?.tools) ? result.tools : [];
      this.switchNeedsUpdate = !!result?.supervisor_update_needed;
      this.switchCatalog.forEach((tool, index) => {
        const option = document.createElement('option'); option.value = String(index);
        option.textContent = `${tool.label || tool.tool}${tool.available ? '' : ` — ${tool.unavailable_reason || 'unavailable'}`}`;
        option.disabled = !tool.available; tools.append(option);
      });
      const agent = this.cards.get(id);
      const current = this.switchCatalog.findIndex(tool => tool.available && tool.tool === agent?.tool && tool.variant === agent?.variant);
      tools.selectedIndex = current >= 0 ? current : this.switchCatalog.findIndex(tool => tool.available);
      this.renderSwitchModels();
    } catch (error) {
      if (this.switchDialog.open && requestID === this.switchCatalogRequestID) document.querySelector('#switch-tool-reason').textContent = `Tool catalog unavailable: ${error.message}`;
    }
  }
  renderSwitchModels(resetModels = true) {
    const agent = this.cards.get(this.switchAgentID);
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    const tool = tools.selectedIndex < 0 ? null : this.switchCatalog?.[Number(tools.value)];
    if (resetModels) {
      models.replaceChildren();
      for (const model of tool?.models || []) {
        const option = document.createElement('option'); option.value = model.id; option.textContent = model.label || model.id; if (model.note) option.title = model.note; models.append(option);
      }
      const preferred = (tool?.models || []).find(model => model.default);
      if (preferred) models.value = preferred.id;
      if (tool?.tool === agent?.tool && tool?.variant === agent?.variant && [...models.options].some(option => option.value === agent.model)) models.value = agent.model;
    }
    document.querySelector('#switch-version').textContent = tool?.cli_version ? `CLI version ${tool.cli_version}` : '';
    const unchanged = tool?.tool === agent?.tool && tool?.variant === agent?.variant && models.value === agent?.model && (!agent?.machine_id || this.switchMachine.id === agent.machine_id);
    this.setHint(document.querySelector('#switch-tool-reason'), this.switchNeedsUpdate && !tool?.available ? 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.'
      : !tool ? 'No harness is available on a connected machine. Add one in Settings → Harnesses.'
      : !models.options.length ? 'This harness has no models yet. Add one under Settings → Harnesses.'
        : unchanged ? 'Choose a different model or tool.'
          : tool.toolbox === false ? `No agent toolbox: ${tool.toolbox_reason || 'unavailable for this launcher'}.` : '');
    this.switchForm.querySelector('button[type=submit]').disabled = !tool?.available || !models.options.length || unchanged || !this.switchMachine.folderReady;
  }
  async switchAgent() {
    const id = this.switchAgentID;
    const tools = this.switchForm.elements.tool;
    const selected = tools.selectedIndex < 0 ? null : this.switchCatalog?.[Number(tools.value)];
    const model = this.switchForm.elements.model.value;
    if (!id || !selected?.available || !model) return;
    const submit = this.switchForm.querySelector('button[type=submit]');
    const cancel = document.querySelector('#switch-cancel');
    submit.disabled = true; cancel.disabled = true;
    document.querySelector('#switch-error').textContent = '';
    try {
      await this.source.switch(id, { room_id: this.switchRoomID, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, machine_id: this.switchMachine.id });
      await this.refresh();
      this.switchDialog.close();
    } catch (error) { document.querySelector('#switch-error').textContent = error.message; this.renderSwitchModels(false); }
    finally { cancel.disabled = false; }
  }
  // The worktree choice, as a request carries it: only a task room can have a worktree, so from any other room nothing is sent, however the
  // box was left (it is hidden there and could not be unticked).
  worktreeChoice() {
    if (this.spawnRoom()?.kind !== 'task' || !this.form.elements.worktree.checked) return { worktree: false, base_ref: '' };
    return { worktree: true, base_ref: this.form.elements.base_ref.value };
  }
  // The room the dialog starts the agent in: the open room, or for a start from v1 history the one picked in "Start in".
  spawnRoom() {
    if (!this.pendingResume) return this.room;
    // A new room for a continued session is made by the server; the request still names the room the dialog was opened from.
    if (this.pendingResume.kind === 'claude_session' && this.form.elements.start_room.value === NEW_ROOM) return this.room;
    return this.startRooms?.get(this.form.elements.start_room.value) || null;
  }
  // The session picker: closes the Spawn dialog (the machine and project are taken from it) and opens the picker; a pick reopens the Spawn dialog.
  openSessionPicker() {
    const projectID = this.room?.project_id, machine = this.spawnMachine.machine;
    if (!projectID) return;
    const machineID = this.spawnMachine.id, machineName = machine?.name || '';
    // The sessions are those of the launcher that will continue them (its own Claude home); another tool's launcher means the default one.
    const chosen = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
    const launcherID = chosen?.tool === 'claude' ? chosen.launcher_id || '' : '';
    this.dialog.close();
    void this.sessionPicker.open({ projectID, machineID, machineName, launcherID, onPick: session => {
      void this.openSpawn({ resume: { kind: 'claude_session', session_id: session.id, tool: 'claude', resumable: true, model: session.model || '', cwd: session.cwd, title: session.title || '',
        modified_at: session.modified_at || '', outside_folder: !!session.outside_folder, active: !!session.active, held_by: session.agent?.slug || '', machine_id: session.machine_id || machineID, launcher_id: session.launcher_id || '' } });
    } });
  }
  // The rooms a v1 agent can be started in: the project's open task rooms. Its history may be only in the home room (which keeps its own
  // lead-only rule) or in a room since archived, so the start is not tied to the room the profile was opened in.
  renderStartRooms() {
    const label = document.querySelector('#spawn-start-in'), select = this.form.elements.start_room;
    label.hidden = !this.pendingResume;
    select.replaceChildren(); this.startRooms = new Map();
    if (!this.pendingResume || !this.room) return;
    const rooms = (this.projectRooms?.(this.room.project_id) || []).filter(room => room.kind === 'task' && room.lifecycle === 'open');
    // A continued Claude Code session gets a room of its own unless the person picks an existing one.
    if (this.pendingResume.kind === 'claude_session') {
      const option = document.createElement('option'); option.value = NEW_ROOM; option.textContent = 'A new room for this session'; select.append(option);
      select.value = NEW_ROOM;
    }
    for (const room of rooms) {
      this.startRooms.set(room.id, room);
      const option = document.createElement('option'); option.value = room.id; option.textContent = room.title; select.append(option);
    }
    // general is the project's shared task room; otherwise the open room if it is one, else the first.
    const preferred = rooms.filter(room => room.title === 'general').sort((a, b) => a.depth - b.depth)[0] || rooms.find(room => room.id === this.room.id) || rooms[0];
    if (preferred && this.pendingResume.kind !== 'claude_session') select.value = preferred.id;
  }
  // A refused or cancelled spawn leaves the form as it was; each opening of the dialog starts without a worktree.
  resetWorktreeChoice() {
    this.form.elements.worktree.checked = false; this.form.elements.base_ref.value = '';
    this.form.querySelector('.spawn-worktree-base').hidden = true;
  }
  // openSpawn({ resume }) opens the dialog to start an agent from imported v1 history: the name is the old one, the tool and model are
  // what v1 ran, and the v1 session is continued when the harness can. Without it, it is the ordinary spawn dialog.
  async openSpawn({ resume = null } = {}) {
    this.resetWorktreeChoice();
    // Nothing of the last opening is left to act on: the role list and the lead-only rule are read again
    // below, and until they are, Start stays off. Otherwise a role picked in the last opening (the lead, in a lead-only home room) could
    // still be selected while the tool catalog arrives first and enables Start, and the agent would be started as that role.
    this.rolesReady = false;
    this.pickedModel = null; this.pickedToolKey = ''; this.resumeToolNote = '';
    this.form.elements.role.replaceChildren();
    this.pendingResume = resume;
    this.renderStartRooms();
    this.renderResume();
    this.dialog.showModal();
    // A v1 agent keeps its own name, so the dialog has no name field then; otherwise it starts with a free one.
    const continuing = resume?.kind === 'claude_session';
    this.form.querySelector('.spawn-name').hidden = !!resume && !continuing;
    this.sessionsButton.hidden = !!resume || this.room?.kind === 'dm';
    this.nameRequestID = (this.nameRequestID || 0) + 1;
    this.nameInput.value = ''; this.nameError.textContent = '';
    if (!resume || continuing) this.fillSpawnName();
    this.renderSpawnModels();
    const toolSelect = this.form.elements.tool;
    const modelSelect = this.form.elements.model;
    const reason = document.querySelector('#spawn-tool-reason');
    const version = document.querySelector('#spawn-version');
    const submit = this.form.querySelector('button[type=submit]');
    toolSelect.replaceChildren(); modelSelect.replaceChildren();
    reason.textContent = 'Loading available tools…'; version.textContent = ''; submit.disabled = true;
    // Each opening has a number. The answers below (the owner, the roles, the lead-only rule) arrive later, possibly after the dialog was
    // closed and opened again for another room or another v1 agent; an answer meant for an earlier opening must not fill this one.
    const opening = this.spawnOpening = (this.spawnOpening || 0) + 1;
    const current = () => opening === this.spawnOpening && this.dialog.open;
    try {
      const room = this.room, roomID = room.id;
      await this.loadOwner(); // before the catalog, whose hints depend on it
      if (!current()) return;
      // A start from v1 history goes to a task room, so the home room's lead-only rule does not apply to it.
      const home = room.kind === 'home' && !resume, hint = document.querySelector('#spawn-home-hint');
      hint.hidden = true;
      const [allRoles, leadOnly] = await Promise.all([this.source.roles(), home ? Promise.resolve(this.source.homeLeadOnly?.(room.project_id)).catch(() => true) : false, this.spawnMachine.load(room.project_id, resume?.machine_id || '')]);
      if (!current() || this.room?.id !== roomID) return;
      // With Home room: lead only (the default) the home room offers the lead roles alone; the server refuses the others.
      const roles = leadOnly ? allRoles.filter(role => role.cap_is_lead) : allRoles;
      hint.hidden = !leadOnly;
      this.form.elements.role.replaceChildren(...roles.map(role => {
        const node = document.createElement('option'); node.value = role.role_key; node.textContent = `${role.emoji || ''} ${role.label}`.trim(); return node;
      }));
      if (roles.some(role => role.role_key === 'worker')) this.form.elements.role.value = 'worker';
      if (resume?.role_key && roles.some(role => role.role_key === resume.role_key)) this.form.elements.role.value = resume.role_key;
      this.rolesReady = true;
      this.updateSpawnSubmit();
      if (!resume) await this.prefillRoleDefault();
    } catch (error) {
      if (!current()) return;
      this.catalog = [];
      reason.textContent = `Machines or tools unavailable: ${error.message}`;
      submit.disabled = true;
    }
  }
  // Puts a free name from the server in the Name field (the dialog's first look, and the dice button). Failing to get one leaves the
  // field as it was: empty still spawns, under a name the server picks.
  async fillSpawnName() {
    const roomID = this.room?.id; if (!roomID) return;
    const requestID = this.nameRequestID = (this.nameRequestID || 0) + 1;
    const previous = this.nameInput.value;
    this.namePendingID = requestID;
    this.nameDice.disabled = true;
    try {
      const name = await this.source.suggestName(roomID, previous);
      if (this.dialog.open && this.room?.id === roomID && requestID === this.nameRequestID && this.nameInput.value === previous && name) {
        this.nameInput.value = name; this.nameError.textContent = '';
      }
    } catch (error) {
      if (this.dialog.open && requestID === this.nameRequestID) this.nameError.textContent = `Could not get a name: ${error.message}`;
    } finally { if (requestID === this.namePendingID) this.nameDice.disabled = false; }
  }
  async prefillRoleDefault() {
    if (this.pendingResume || !this.room?.project_id || !this.form.elements.role.value) return;
    const token = this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1;
    const roleKey = this.form.elements.role.value;
    try {
      const choice = await request(`/api/projects/${encodeURIComponent(this.room.project_id)}/role-spawn-defaults/${encodeURIComponent(roleKey)}/next`);
      if (token !== this.defaultPreviewToken || !this.dialog.open || this.form.elements.role.value !== roleKey || !choice.available) return;
      if (choice.machine_id && this.spawnMachine.id !== choice.machine_id) {
        this.spawnMachine.select.value = choice.machine_id;
        this.spawnMachine.renderFolder();
        await this.loadSpawnCatalog();
      }
      if (token !== this.defaultPreviewToken || !this.dialog.open) return;
      const index = this.catalog?.findIndex(item => item.launcher_id === choice.launcher_id && item.available);
      if (index >= 0) {
        this.form.elements.tool.value = String(index);
        this.renderSpawnModels();
        if ([...this.form.elements.model.options].some(option => option.value === choice.model)) this.form.elements.model.value = choice.model;
      }
    } catch { /* The ordinary first-available choice remains usable. */ }
  }
  async loadSpawnCatalog() {
    const machineID = this.spawnMachine.id;
    const requestID = (this.catalogRequestID || 0) + 1; this.catalogRequestID = requestID;
    const toolSelect = this.form.elements.tool, modelSelect = this.form.elements.model;
    // A reload (the machine changed, a live update) must not undo what the person picked: the tool they had is picked again when it is still
    // offered and available.
    const previousTool = this.pickedToolKey || '';
    toolSelect.replaceChildren(); modelSelect.replaceChildren();
    this.form.querySelector('button[type=submit]').disabled = true;
    document.querySelector('#spawn-tool-reason').textContent = 'Loading available tools…';
    if (!machineID) return;
    try {
      const result = await this.source.catalog(machineID);
      if (!this.dialog.open || requestID !== this.catalogRequestID) return;
      this.catalog = Array.isArray(result?.tools) ? result.tools : [];
      this.spawnNeedsUpdate = !!result?.supervisor_update_needed;
      this.catalog.forEach((tool, index) => {
        const option = document.createElement('option'); option.value = String(index);
        const detail = tool.unavailable_reason || 'unavailable';
        const help = detail.includes('Settings → Harnesses') ? detail : `${detail}; check Settings → Harnesses`;
        option.textContent = `${tool.label || tool.tool}${tool.available ? '' : ` — ${help}`}`;
        option.disabled = !tool.available; toolSelect.append(option);
      });
      const kept = previousTool ? this.catalog.findIndex(tool => tool.available && toolKey(tool) === previousTool) : -1;
      toolSelect.selectedIndex = kept >= 0 ? kept : this.catalog.findIndex(tool => tool.available);
      if (kept < 0) this.applyResumeChoice(toolSelect);
      this.renderSpawnModels();
    } catch (error) {
      if (this.dialog.open && requestID === this.catalogRequestID) document.querySelector('#spawn-tool-reason').textContent = `Tool catalog unavailable: ${error.message}`;
    }
  }
  // The v1 tool is picked when this machine offers it. A session that can be continued needs that tool natively, or in a Docker launcher that
  // shares the host's Claude home (the catalog says so: shares_claude_home), since any other container has a home of its own and cannot see
  // it. Only those entries stay selectable and the first of them is picked, whatever order the catalog lists them in. A fresh start only
  // preselects the v1 tool; the others stay available.
  applyResumeChoice(toolSelect) {
    const resume = this.pendingResume;
    this.resumeToolNote = '';
    if (!resume?.tool) return;
    const fits = tool => tool.tool === resume.tool && tool.available && !(resume.resumable && tool.containerized && !tool.shares_claude_home);
    // A continued session was listed from one launcher's Claude home: that launcher is picked when it is still offered, else the first fit.
    const preferred = resume.launcher_id ? this.catalog.findIndex(tool => fits(tool) && tool.launcher_id === resume.launcher_id) : -1;
    const index = preferred >= 0 ? preferred : this.catalog.findIndex(fits);
    if (index >= 0) toolSelect.selectedIndex = index;
    if (resume.resumable) for (const option of toolSelect.options) option.disabled = option.disabled || !fits(this.catalog[Number(option.value)]);
    // Nothing fits: no entry is selected, so the dialog cannot submit a start that the server would refuse.
    if (index < 0 && resume.resumable) toolSelect.selectedIndex = -1;
    // The words go into the banner's notes, which are drawn again whenever the model or the tool changes (the render after the catalog loaded shows it), so they are kept, not written once.
    if (index < 0 && resume.resumable) { this.resumeToolNote = `${resume.tool} is not available natively on the chosen machine, so the session cannot be continued here.`; }
  }
  // The banner of the start-from-v1 dialog: what is being started, where it will work, and what to confirm.
  // keep: a change of the continue/copy choice re-renders the banner and keeps what was ticked; opening the dialog starts from "continue".
  renderResume(keep = false) {
    const resume = this.pendingResume;
    const box = document.querySelector('#spawn-resume'), text = document.querySelector('#spawn-resume-text'), notes = document.querySelector('#spawn-resume-notes');
    const confirm = document.querySelector('#spawn-resume-confirm'), confirmLabel = document.querySelector('#spawn-resume-confirm-label');
    box.hidden = !resume;
    document.querySelector('#spawn-resume-card').hidden = true;
    this.syncWorktreeOption();
    const continuing = resume?.kind === 'claude_session';
    // A v1 agent whose Claude Code session can be continued is offered the same choice: continue the original, or work on a copy.
    const legacySession = !!resume && !continuing && !!resume.resumable;
    const choosing = continuing || legacySession;
    const modeBox = document.querySelector('#spawn-resume-mode');
    // A session an agent already continues can only be copied.
    const held = continuing && !!resume.held_by;
    if (choosing && !keep) this.form.elements.resume_mode.value = held ? 'fork' : 'continue';
    modeBox.hidden = !choosing;
    modeBox.querySelector('input[value=continue]').disabled = held;
    const forking = choosing && this.form.elements.resume_mode.value === 'fork';
    this.form.querySelector('h2').textContent = continuing ? (forking ? 'Work on a copy of a Claude Code session' : 'Continue a Claude Code session') : resume ? (forking ? `Start ${resume.slug} on a copy of its v1 session` : `Start ${resume.slug} as a v2 agent`) : 'Spawn';
    // Closing the session matters only when it is continued; a copy leaves the original as it is.
    if (!keep) confirm.checked = false;
    confirmLabel.hidden = continuing ? !resume.active || forking : !resume?.v1_active || forking; notes.textContent = '';
    // The one box means "nothing else has this session open"; its words are for what the person is continuing.
    confirmLabel.replaceChildren(confirm, document.createTextNode(continuing ? ' I closed this session in my terminal' : ' v1 is not running this session any more'));
    if (!resume) return;
    if (continuing) {
      // The session is a neutral card (its short id, its folder, when it was last used), not a box titled with whatever was typed first.
      const card = document.querySelector('#spawn-resume-card');
      card.hidden = false;
      card.replaceChildren(...[['Session', resume.session_id.slice(0, 8), true], ['Folder', resume.cwd, true], ['Last active', resume.modified_at ? relativeTime(resume.modified_at) : 'unknown', false]].flatMap(([name, value, mono]) => {
        const term = document.createElement('dt'); term.textContent = name;
        const detail = document.createElement('dd'); if (mono) detail.className = 'mono'; detail.textContent = value;
        return [term, detail];
      }));
      text.textContent = forking
        ? 'This works on a copy of the session. The agent starts from everything said so far, and what it does goes to the copy: the original stays exactly as it is.'
        : 'This continues the session. It carries on from where it stopped, in the same transcript.';
      const lines = [];
      if (!this.spawnRoom()) lines.push('This project has no open task room to start it in.');
      if (resume.outside_folder) lines.push(`It works in ${resume.cwd}, outside the project folder, so the owner must approve it in the Inbox before it starts.`);
      if (held) lines.push(`${resume.held_by} already continues this session, so it can only be copied.`);
      if (resume.active && !forking) lines.push('A Claude process has this session open. Close it first: two processes on one session corrupt it.');
      lines.push(this.resumeToolNote, this.modelWarning(resume));
      notes.textContent = lines.filter(Boolean).join(' ');
      return;
    }
    text.textContent = resume.resumable
      ? (forking
        ? `This works on a copy of the v1 session of ${resume.legacy_id}, in ${resume.cwd}. The agent starts from everything said so far, and what it does goes to the copy: the original stays exactly as it is.`
        : `This continues the v1 session of ${resume.legacy_id}, working in ${resume.cwd}.`)
      : `${resume.reason || 'This agent starts fresh with the same name.'}`;
    const lines = [];
    if (!this.spawnRoom()) lines.push('This project has no open task room to start it in.');
    if (resume.resumable && resume.outside_folder) lines.push(`It works in ${resume.cwd}, outside the project folder, so the owner must approve it in the Inbox before it starts.`);
    if (resume.v1_active && !forking) lines.push('v1 saw this agent a moment ago and may still be using the session. Stop it in v1 first: two processes on one session corrupt it.');
    if (resume.resumable) lines.push(this.resumeToolNote, this.modelWarning(resume));
    notes.textContent = lines.filter(Boolean).join(' ');
  }
  // The model the session ran on against the one picked for the agent.
  modelWarning(resume) {
    const option = this.form.elements.model.selectedOptions[0];
    return modelChangeWarning(resume.model, option ? { value: option.value, label: option.textContent } : null);
  }
  updateSpawnSubmit() {
    const tool = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
    // A resumed session runs in the v1 folder, which the server checks itself, so the machine's project folder is not needed for it.
    this.form.querySelector('button[type=submit]').disabled = !this.rolesReady || (!!this.pendingResume && !this.spawnRoom()) || (!this.pendingResume?.resumable && !this.spawnMachine.folderReady) || (!tool?.available || !this.form.elements.model.options.length);
  }
  // A hint that names Settings → Harnesses makes that name a button that opens it, so an empty machine says where to go.
  // Only an owner or admin can add or change a harness, so anyone else gets plain words (the link would only end in a refusal).
  setHint(node, text) {
    const marker = 'Settings → Harnesses', at = text.indexOf(marker);
    node.replaceChildren();
    if (at < 0) { node.textContent = text; return; }
    if (!this.canManageHarnesses) {
      node.textContent = text.replace(/Add a harness in Settings → Harnesses/, 'Ask an admin to add a harness').replace(/Add one (?:in|under) Settings → Harnesses/, 'Ask an admin to add one')
        .replace(/; [Cc]heck Settings → Harnesses/, '; ask an admin to check the harnesses').replace(/[Cc]heck Settings → Harnesses/, 'Ask an admin to check the harnesses').replace(/enable it in Settings → Harnesses/, 'ask an admin to enable it');
      return;
    }
    const link = document.createElement('button'); link.type = 'button'; link.className = 'inline-link'; link.textContent = marker;
    // Settings opens on the machine the dialog is showing.
    const machineID = this.switchDialog.open ? this.switchMachine.id : this.spawnMachine.id;
    link.onclick = () => { this.dialog.close(); this.switchDialog.close(); document.dispatchEvent(new CustomEvent('aimebu:open-settings', { detail: { section: 'harnesses', machineID } })); };
    node.append(text.slice(0, at), link, text.slice(at + marker.length));
  }
  // Whether this person is the owner or an admin (who are trusted alike), read when a dialog opens; it decides what the hints can offer.
  async loadOwner() {
    this.canManageHarnesses = rights.admin;
  }
  renderSpawnModels() {
    const tool = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
    const models = this.form.elements.model;
    const reason = document.querySelector('#spawn-tool-reason');
    const version = document.querySelector('#spawn-version');
    models.replaceChildren();
    for (const model of tool?.models || []) {
      const option = document.createElement('option');
      option.value = typeof model === 'string' ? model : model.id;
      option.textContent = typeof model === 'string' ? model : model.label || model.id;
      if (model.note) option.title = model.note;
      models.append(option);
    }
    // The person's own pick for this tool, else the model the start names (a v1 agent's, a session's), else the harness's default.
    const options = [...models.options].map(option => ({ value: option.value, label: option.textContent }));
    const preferred = (tool?.models || []).find(model => model.default);
    const chosen = chooseModel(options, { picked: this.pickedModel, toolKey: toolKey(tool), named: this.pendingResume?.model, preferred: preferred?.id });
    if (chosen) models.value = chosen;
    version.textContent = tool?.cli_version ? `CLI version ${tool.cli_version}` : '';
    this.setHint(reason, this.spawnNeedsUpdate && !tool?.available ? 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.'
      : !tool ? (this.catalog?.length ? 'No tool is available on a connected machine. Check Settings → Harnesses.' : 'No harness yet. Add a harness in Settings → Harnesses.')
      : !tool.available ? (tool.unavailable_reason?.includes('Settings → Harnesses')
        ? tool.unavailable_reason : `${tool.unavailable_reason || 'This tool is unavailable'}; check Settings → Harnesses`)
        : models.options.length ? (tool.toolbox === false ? `No agent toolbox: ${tool.toolbox_reason || 'unavailable for this launcher'}.` : '') : 'This harness has no models yet. Add one under Settings → Harnesses.');
    this.renderSessionsEntry();
    if (this.pendingResume) this.renderResume(true); // the model warning depends on the model now selected
    this.updateSpawnSubmit();
  }
  // The entry to the session list: with a launcher that cannot continue a session selected (a container with a Claude home of its own), the
  // dialog says so before anything is picked and offers the launcher that can.
  renderSessionsEntry() {
    const selected = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)];
    const state = sessionEntryState(selected, this.catalog || []);
    this.sessionsButton.disabled = !state.enabled;
    const note = document.querySelector('#spawn-sessions-note'), swap = document.querySelector('#spawn-sessions-switch');
    note.hidden = this.sessionsButton.hidden || !state.note;
    document.querySelector('#spawn-sessions-note-text').textContent = state.note;
    swap.hidden = state.switchTo < 0;
    if (state.switchTo >= 0) {
      const target = this.catalog[state.switchTo];
      swap.textContent = `Use ${target.label || 'the native launcher'}`;
      swap.onclick = () => { this.form.elements.tool.value = String(state.switchTo); this.pickedToolKey = toolKey(target); this.pickedModel = null; this.renderSpawnModels(); };
    }
  }
  async spawn() {
    const target = this.spawnRoom();
    if (!this.room || !target) return;
    this.error.textContent = '';
    const role = this.form.elements.role.value, selected = this.form.elements.tool.selectedIndex < 0 ? null : this.catalog?.[Number(this.form.elements.tool.value)], model = this.form.elements.model.value;
    if (!role || !selected?.available || !model) return;
    const resume = this.pendingResume, continuing = resume?.kind === 'claude_session';
    const name = resume && !continuing ? '' : this.nameInput.value.trim();
    if (name && !/^[a-z][a-z0-9_-]{1,19}[a-z0-9]$/.test(name)) { this.nameError.textContent = 'A name is 3 to 21 characters: lowercase letters, digits, - and _, starting with a letter and ending with a letter or digit.'; this.nameInput.focus(); return; }
    if (resume?.resumable && selected.tool !== resume.tool) { this.error.textContent = `To continue this session the tool must be ${resume.tool}.`; return; }
    const forkMode = continuing && this.form.elements.resume_mode.value === 'fork';
    if (continuing && !forkMode && resume.active && !document.querySelector('#spawn-resume-confirm').checked) { document.querySelector('#spawn-resume-notes').textContent = 'Confirm that you closed this session.'; document.querySelector('#spawn-resume-confirm').focus(); return; }
    const legacyFork = !continuing && !!resume?.resumable && this.form.elements.resume_mode.value === 'fork';
    if (!continuing && !legacyFork && resume?.v1_active && !document.querySelector('#spawn-resume-confirm').checked) { document.querySelector('#spawn-resume-notes').textContent = 'Confirm that v1 is not running this session any more.'; document.querySelector('#spawn-resume-confirm').focus(); return; }
    try { const card = await this.source.spawn(target.id, { role, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, provider: selected.provider, machine_id: this.spawnMachine.id, ...(name ? { name } : {}), ...(resume?.resumable ? {} : this.worktreeChoice()), ...(resume && !continuing ? { resume_legacy: resume.legacy_id, ...(resume.resumable ? { resume_mode: legacyFork ? 'fork' : 'continue' } : {}), confirm_v1_idle: !legacyFork && document.querySelector('#spawn-resume-confirm').checked } : {}), ...(continuing ? { resume_session: resume.session_id, resume_mode: forkMode ? 'fork' : 'continue', confirm_session_closed: !forkMode && document.querySelector('#spawn-resume-confirm').checked, new_room: this.form.elements.start_room.value === NEW_ROOM } : {}) });
      this.dialog.close(); this.form.reset();
      // A session continued in a room of its own: that room is where the person goes.
      if (continuing && card?.room_id && card.room_id !== this.room.id && card.room_id !== target.id) await this.showRoomByID?.(this.room.project_id, card.room_id);
      else if (target.id !== this.room.id) await this.showRoom?.(target); else await this.refresh(); }
    catch (error) {
      // A refused name is answered under its field and the dialog stays open, with everything else as it was, so another name can be tried.
      if (error.code === 'name_in_use' || error.code === 'invalid_name') { this.nameError.textContent = error.message; this.nameInput.focus(); return; }
      this.error.textContent = error.message; this.dialog.close();
    }
  }
}
