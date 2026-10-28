import { MachineSelection } from './machine-selection.js';
import { request, rights } from './api.js';
import { agentLabel } from './sender-identity.js';
import { setGlyph } from './icon-button.js';
import { relativeTime } from './time.js';
import { ClaudeSessionPicker } from './claude-sessions.js';
import { chooseModel, toolKey, sessionEntryState, modelChangeWarning } from './spawn-model.js';
import { harnessName } from './harness-version.js';

// The Spawn and Switch dialogs, moved out of AgentsPanel unchanged while the panel itself was replaced; they read the panel through `host`.
// The "Start in" value that asks for a room of its own, for a continued Claude Code session.
const NEW_ROOM = '__new__';

export class SpawnDialogs {
  constructor(host) {
    this.host = host;
    this.dialog = document.querySelector('#spawn-dialog'); this.form = document.querySelector('#spawn-form');
    const worktreeLabel = document.createElement('label'); worktreeLabel.className = 'spawn-worktree-option';
    worktreeLabel.innerHTML = '<input type="checkbox" name="worktree"><span>Use a Git worktree for this task room</span><small class="spawn-worktree-note" hidden></small>';
    this.form.querySelector('#spawn-agent-fields').before(worktreeLabel);
    const baseLabel = document.createElement('label'); baseLabel.className = 'spawn-worktree-base';
    baseLabel.innerHTML = 'Base branch or ref (optional)<input name="base_ref" placeholder="Current HEAD">';
    worktreeLabel.after(baseLabel);
    this.form.elements.worktree.onchange = () => { baseLabel.hidden = !this.form.elements.worktree.checked; };
    baseLabel.hidden = true;
    this.switchDialog = document.querySelector('#switch-dialog'); this.switchForm = document.querySelector('#switch-form');
    this.spawnMachine = new MachineSelection('spawn', () => { this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1; this.syncWorktreeOption(); return this.loadSpawnCatalog(); }, () => this.updateSpawnSubmit());
    this.switchMachine = new MachineSelection('switch', () => this.loadSwitchCatalog(), () => this.renderSwitchModels(false));
    document.querySelector('#spawn-open').onclick = () => this.openSpawn();
    // Continuing a Claude Code session the machine already has: pick it, then the Spawn dialog opens for it.
    this.sessionPicker = new ClaudeSessionPicker(host.source);
    // A labelled button, not a bare glyph that reads as "refresh": it opens the list of Claude Code sessions this machine has.
    this.sessionsButton = setGlyph(document.querySelector('#spawn-sessions'), 'reopen', 'Continue a Claude Code session');
    this.sessionsButton.append(document.createTextNode(' Continue a session…'));
    this.sessionsButton.onclick = () => this.openSessionPicker();
    document.querySelector('#spawn-cancel').onclick = () => this.dialog.close();
    // The browser queues the event after close(): one that arrives once the dialog was opened again belongs to the opening before.
    this.dialog.addEventListener('close', () => { if (this.dialog.open) return; this.pendingResume = null; this.renderStartRooms(); this.renderResume(); });
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
    document.querySelector('#spawn-open').disabled = !host.source.available;
  }
  get room() { return this.host.room; }
  get cards() { return this.host.cards; }
  get error() { return this.host.error; }
  get source() { return this.host.source; }
  get projectRole() { return this.host.projectRole; }
  get projectRooms() { return this.host.projectRooms; }
  get showRoom() { return this.host.showRoom; }
  get showRoomByID() { return this.host.showRoomByID; }
  get refreshGeneration() { return this.host.refreshGeneration; }
  refresh() { return this.host.refresh(); }
  roomChanged() { this.switchDialog.close(); this.switchAgentID = null; }
  closeAll() { this.dialog.close(); this.switchDialog.close(); }
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
  async openSwitch(id) {
    const agent = this.cards.get(id); if (!agent) return;
    this.switchAgentID = id;
    this.switchRoomID = this.room.id;
    this.switchOpening = (this.switchOpening || 0) + 1;
    document.querySelector('#switch-cancel').disabled = false;
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
    const opening = this.switchOpening, generation = this.refreshGeneration, stale = () => opening !== this.switchOpening || generation !== this.refreshGeneration;
    const submit = this.switchForm.querySelector('button[type=submit]');
    const cancel = document.querySelector('#switch-cancel');
    submit.disabled = true; cancel.disabled = true;
    document.querySelector('#switch-error').textContent = '';
    try {
      await this.source.switch(id, { room_id: this.switchRoomID, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, machine_id: this.switchMachine.id });
      if (stale()) return;
      await this.refresh();
      this.switchDialog.close();
    } catch (error) { if (stale()) return; document.querySelector('#switch-error').textContent = error.message; this.renderSwitchModels(false); }
    finally { if (!stale()) cancel.disabled = false; }
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
    this.catalogRequestID = (this.catalogRequestID || 0) + 1; // a catalog asked for by an earlier opening is no answer to this one
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
        this.spawnMachine.endSave(); // a folder save of the machine it leaves is over, as when the person picks another machine
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
      card.replaceChildren(...[['Session', resume.session_id.slice(0, 8), true], ['Folder', resume.cwd, true], ['Last active', resume.modified_at ? relativeTime(resume.modified_at) : 'unknown', false], ...(resume.model ? [['Ran on', resume.model, true]] : [])].flatMap(([name, value, mono]) => {
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
      node.textContent = text.replace(/Add a harness in Settings → Harnesses/, 'Ask an admin to add a harness').replace(/Add one (?:in|under) Settings → Harnesses/, 'Ask an admin to add one').replace(/add one (?:in|under) Settings → Harnesses/, 'ask an admin to add one')
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
    this.setHint(document.querySelector('#spawn-sessions-note-text'), state.note); // the Settings link for an admin, "ask an admin" for anyone else
    swap.hidden = state.switchTo < 0;
    if (state.switchTo >= 0) {
      const target = this.catalog[state.switchTo];
      swap.textContent = `Use ${target.label || 'the native launcher'}`;
      swap.onclick = () => {
        this.defaultPreviewToken = (this.defaultPreviewToken || 0) + 1; // a role-default prefill still in flight must not flip the tool back
        this.form.elements.tool.value = String(state.switchTo); this.pickedToolKey = toolKey(target); this.pickedModel = null; this.renderSpawnModels();
      };
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
    // The dialog can be cancelled, or closed and opened again for another room, or the panel reset, while the call is out; its outcome is then not this view's.
    const opening = this.spawnOpening, generation = this.refreshGeneration, stale = () => !this.dialog.open || opening !== this.spawnOpening || generation !== this.refreshGeneration;
    try { const card = await this.source.spawn(target.id, { role, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, provider: selected.provider, machine_id: this.spawnMachine.id, ...(name ? { name } : {}), ...(resume?.resumable ? {} : this.worktreeChoice()), ...(resume && !continuing ? { resume_legacy: resume.legacy_id, ...(resume.resumable ? { resume_mode: legacyFork ? 'fork' : 'continue' } : {}), confirm_v1_idle: !legacyFork && document.querySelector('#spawn-resume-confirm').checked } : {}), ...(continuing ? { resume_session: resume.session_id, resume_mode: forkMode ? 'fork' : 'continue', confirm_session_closed: !forkMode && document.querySelector('#spawn-resume-confirm').checked, new_room: this.form.elements.start_room.value === NEW_ROOM } : {}) });
      if (stale()) return;
      this.dialog.close(); this.form.reset();
      // A session continued in a room of its own: that room is where the person goes.
      if (continuing && card?.room_id && card.room_id !== this.room.id && card.room_id !== target.id) await this.showRoomByID?.(this.room.project_id, card.room_id);
      else if (target.id !== this.room.id) await this.showRoom?.(target); else await this.refresh(); }
    catch (error) {
      if (stale()) return;
      // A refused name is answered under its field and the dialog stays open, with everything else as it was, so another name can be tried.
      if (error.code === 'name_in_use' || error.code === 'invalid_name') { this.nameError.textContent = error.message; this.nameInput.focus(); return; }
      this.error.textContent = error.message; this.dialog.close();
    }
  }
}
