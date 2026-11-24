import { batch, computed, signal } from '../../ui.js';
import { request, rights } from '../../api.js';
import { ClaudeSessionPicker } from '../claude-sessions/page.js';
import { chooseModel, hintParts, historyNote, modelChangeWarning, NEW_ROOM, REOPEN_PREFIX, resumeBanner, resumeToolChoice, sessionEntryState, sortModelOptions, spawnReason, spawnRequest, startRoomChoices, toolKey, worktreeOption } from '../../spawn-model.js';
import { relativeTime } from '../../time.js';
import { MachineStore } from './machine-store.js';
import { captureError } from '../../telemetry.js';

// The Spawn dialog: one opening at a time. Each opening, catalog read, name read and spawn carries a number and says nothing once a newer
// one started or the dialog is gone, so an answer meant for an earlier opening fills nothing.
export class SpawnStore {
  constructor(host) {
    this.host = host;
    this.machine = new MachineStore({ onChange: () => { this.defaultPreviewToken++; this.syncWorktree(); return this.loadCatalog(); }, onFolderEdit: () => {}, mayAdmit: () => rights.admin || this.host.projectRole?.(this.host.room?.project_id) === 'owner' });
    this.resume = signal(null);
    this.mode = signal('continue');
    this.confirmed = signal(false);
    this.notesOverride = signal('');
    this.catalog = signal([]);
    this.toolIndex = signal(-1);
    this.toolsDisabled = signal(null);
    this.toolNote = signal('');
    this.reasonText = signal('');
    this.models = signal([]);
    this.model = signal('');
    this.roles = signal([]);
    this.role = signal('');
    this.leadHint = signal(false);
    this.flowHint = signal('');
    this.rolesReady = signal(false);
    this.name = signal('');
    this.nameError = signal('');
    this.nameBusy = signal(false);
    this.startRoom = signal('');
    this.startOptions = signal([]);
    this.historyNote = signal('');
    this.worktree = signal(false);
    this.baseRef = signal('');
    this.roomTick = signal(0);
    this.needsUpdate = false; this.startRooms = new Map();
    this.opening = 0; this.pickToken = 0; this.catalogRequestID = 0; this.nameRequestID = 0; this.namePendingID = 0; this.defaultPreviewToken = 0; this.pickedToolKey = ''; this.pickedModel = null;
    this.alive = true;
    this.picker = new ClaudeSessionPicker(host.source);
    this.tool = computed(() => this.toolIndex.value < 0 ? null : this.catalog.value[this.toolIndex.value] || null);
    this.target = computed(() => { this.roomTick.value; return this.spawnRoom(); });
    this.worktreeView = computed(() => worktreeOption({ roomKind: this.target.value?.kind, resumable: this.resume.value?.resumable, check: this.machine.machine?.project_worktree }));
    this.sessions = computed(() => sessionEntryState(this.tool.value, this.catalog.value));
    this.version = computed(() => this.tool.value?.cli_version ? `CLI version ${this.tool.value.cli_version}` : '');
    this.banner = computed(() => {
      const resume = this.resume.value, option = this.models.value.find(model => model.value === this.model.value);
      return resumeBanner({ resume, mode: this.mode.value, hasRoom: !!this.target.value, toolNote: this.toolNote.value, warning: resume ? modelChangeWarning(resume.model, option) : '', relative: relativeTime, history: this.historyNote.value });
    });
    this.submitDisabled = computed(() => !this.rolesReady.value || !this.role.value || (!!this.resume.value && !this.target.value) || (!this.resume.value?.resumable && !this.machine.ready) || !this.tool.value?.available || !this.models.value.length);
  }

  get dialog() { return this.host.spawnDialog; }
  get room() { return this.host.room; }

  // The room the dialog starts the agent in: the open room, or for a start from v1 history the one picked in "Start in".
  spawnRoom() {
    const resume = this.resume.value;
    if (!resume) return this.room;
    // A new room for a continued session is made by the server; the request still names the room the dialog was opened from.
    if (resume.kind === 'claude_session' && this.startRoom.value === NEW_ROOM) return this.room;
    return this.startRooms.get(this.startRoom.value) || null;
  }

  // The worktree box cannot stay ticked where the machine refuses it.
  syncWorktree() { if (this.worktreeView.value.refused) this.worktree.value = false; }

  touchRoom() { this.roomTick.value++; }

  // The room changed or the panel was reset: the opening is over, the dialog closes, and what it was waiting for starts nothing.
  invalidate() {
    this.dialog.close();
    this.opening++; this.catalogRequestID++; this.nameRequestID++; this.defaultPreviewToken++; this.pickToken++; this.machine.cancel();
  }

  // After a reopen the choice is the plain, open room: the option that reopened it is gone, and the room is the one the request goes to.
  useReopenedRoom(room) {
    const opened = { ...room, lifecycle: 'open', lifecycle_rev: (room.lifecycle_rev || 0) + 1 };
    this.startRooms.delete(REOPEN_PREFIX + room.id);
    this.startRooms.set(room.id, opened);
    batch(() => {
      this.startOptions.value = this.startOptions.value.map(option => option.value === REOPEN_PREFIX + room.id ? { value: room.id, label: option.label.replace(/^Reopen /, '').replace(/ and start there$/, '') } : option);
      this.startRoom.value = room.id;
      this.historyNote.value = '';
    });
    this.roomTick.value++;
  }

  renderStartRooms({ keep = false } = {}) {
    const resume = this.resume.peek(), room = this.room;
    this.startRooms = new Map();
    if (!resume || !room) { batch(() => { this.startOptions.value = []; this.startRoom.value = ''; this.historyNote.value = ''; }); return; }
    const rooms = this.host.projectRooms?.(room.project_id) || [];
    const lead = !!this.allRoles?.find(role => role.role_key === this.role.peek())?.cap_is_lead;
    const choices = startRoomChoices(rooms, resume, room.id, this.host.projectName?.(room.project_id) || '', lead);
    // A pick that is still on offer after the role changed stays picked.
    const kept = this.startRoom.peek();
    if (keep && kept && choices.options.some(option => option.value === kept)) choices.value = kept;
    for (const item of rooms) if ((item.kind === 'task' || lead && item.kind === 'home') && item.lifecycle === 'open') this.startRooms.set(item.id, item);
    // The archived room that holds the agent's history, offered as "Reopen … and start there": it is reopened when the start is sent.
    for (const option of choices.options) if (option.value.startsWith(REOPEN_PREFIX)) { const archived = rooms.find(item => item.id === option.value.slice(REOPEN_PREFIX.length)); if (archived) this.startRooms.set(option.value, archived); }
    batch(() => { this.startOptions.value = choices.options; this.startRoom.value = choices.value; this.historyNote.value = historyNote(resume, rooms, this.host.projectName?.(room.project_id) || ''); });
  }

  // The template supplies room access by kind. An empty rule list (Custom)
  // leaves the role catalog as it is; the server still checks the final spawn.
  filterRoles() {
    const target = this.spawnRoom();
    const roomKind = this.startRoom.peek() === NEW_ROOM ? 'task' : target?.team_flow_room_key || (target?.kind === 'task' && target?.purpose === 'research' ? 'research' : target?.kind);
    const rules = this.flowTemplate?.join_rules;
    // A research room follows the task rule in a flow with no research rule of its own.
    const rule = rules?.find(item => item.room_kind === roomKind) || (roomKind === 'research' ? rules?.find(item => item.room_kind === 'task') : undefined);
    const eligible = rule ? this.allRoles.filter(role => rule.allowed_roles.includes(role.role_key)) : this.allRoles;
    const byKey = new Map(this.allRoles.map(role => [role.role_key, role]));
    const reviewerRole = role => { const seen = new Set(); while (role && !seen.has(role.role_key)) { if (role.role_key === 'reviewer') return true; seen.add(role.role_key); role = byKey.get(role.extends); } return false; };
    const roles = eligible.filter(role => (!this.homeLeadOnly || role.cap_is_lead) && (this.reviewerAgentsEnabled || !reviewerRole(role)));
    const current = this.role.peek();
    batch(() => {
      this.roles.value = roles.map(role => ({ value: role.role_key, label: `${role.emoji || ''} ${role.label}`.trim() }));
      this.role.value = roles.some(role => role.role_key === current) ? current : roles.some(role => role.role_key === 'worker') ? 'worker' : roles[0]?.role_key || '';
      this.flowHint.value = rule ? `This team flow allows ${roles.map(role => role.label).join(', ') || 'no roles'} in this room.` : '';
      this.rolesReady.value = true;
    });
  }

  // openSpawn({ resume }) opens the dialog to start an agent from imported v1 history: the name is the old one, the tool and model are
  // what v1 ran, and the v1 session is continued when the harness can. Without it, it is the ordinary spawn dialog.
  async open({ resume = null } = {}) {
    if (!this.alive) return;
    const continuing = resume?.kind === 'claude_session';
    // A catalog or a name asked for by an earlier opening is no answer to this one.
    this.catalogRequestID++; this.nameRequestID++;
    this.allRoles = null; this.flowTemplate = null; this.homeLeadOnly = false; this.reviewerAgentsEnabled = true;
    batch(() => {
      this.worktree.value = false; this.baseRef.value = '';
      // Nothing of the last opening is left to act on: the roles and the lead-only rule are read again below, and until they are, Start
      // stays off. Otherwise a role picked in the last opening could still be selected when the tool catalog arrives and enables Start.
      this.rolesReady.value = false; this.pickedModel = null; this.pickedToolKey = ''; this.toolNote.value = '';
      this.roles.value = []; this.role.value = ''; this.resume.value = resume; this.name.value = ''; this.nameError.value = ''; this.notesOverride.value = '';
      this.mode.value = resume?.kind === 'claude_session' && resume.held_by ? 'fork' : 'continue'; this.confirmed.value = false;
      this.catalog.value = []; this.toolIndex.value = -1; this.toolsDisabled.value = null; this.models.value = []; this.model.value = ''; this.reasonText.value = 'Loading available tools…'; this.leadHint.value = false; this.flowHint.value = '';
    });
    this.renderStartRooms();
    this.roomTick.value++;
    this.host.showSpawnDialog();
    if (!resume || continuing) this.fillName();
    // Each opening has a number. The answers below arrive later, possibly after the dialog was closed and opened again for another room or
    // another v1 agent; an answer meant for an earlier opening must not fill this one.
    const opening = this.opening = this.opening + 1;
    const current = () => this.alive && opening === this.opening && this.dialog.open;
    try {
      const room = this.room, roomID = room.id;
      // A start from v1 history goes to a task room, so the home room's lead-only rule does not apply to it.
      const home = room.kind === 'home' && !resume;
      const [allRoles, leadOnly, flow, reviewers] = await Promise.all([this.host.source.roles(), home ? Promise.resolve(this.host.source.homeLeadOnly?.(room.project_id)).catch(() => true) : false, this.host.source.teamFlow?.(room.project_id), Promise.resolve(this.host.source.reviewerAgentsEnabled?.(room.project_id)).catch(() => true), this.machine.load(room.project_id, resume?.machine_id || '')]);
      if (!current() || this.room?.id !== roomID) return;
      // With Home room: lead only (the default) the home room offers the lead roles alone; the server refuses the others.
      this.allRoles = allRoles;
      this.flowTemplate = flow?.template;
      this.homeLeadOnly = !!leadOnly;
      this.reviewerAgentsEnabled = reviewers !== false;
      this.leadHint.value = !!leadOnly;
      this.role.value = resume?.role_key || '';
      this.filterRoles();
      if (resume) this.renderStartRooms();
      if (!resume) await this.prefillRoleDefault();
    } catch (error) {
      if (!current()) return;
      batch(() => { this.catalog.value = []; this.reasonText.value = `Machines or tools unavailable: ${error.message}`; });
    }
  }

  // Puts a free name from the server in the Name field (the dialog's first look, and the dice button). Failing to get one leaves the
  // field as it was: empty still spawns, under a name the server picks.
  async fillName() {
    const roomID = this.room?.id; if (!roomID) return;
    const requestID = this.nameRequestID = this.nameRequestID + 1;
    const previous = this.name.peek();
    this.namePendingID = requestID;
    this.nameBusy.value = true;
    try {
      const name = await this.host.source.suggestName(roomID, previous);
      if (this.alive && this.dialog.open && this.room?.id === roomID && requestID === this.nameRequestID && this.name.peek() === previous && name) batch(() => { this.name.value = name; this.nameError.value = ''; });
    } catch (error) {
      if (this.alive && this.dialog.open && requestID === this.nameRequestID) this.nameError.value = `Could not get a name: ${error.message}`;
    } finally { if (this.alive && requestID === this.namePendingID) this.nameBusy.value = false; }
  }

  typeName(value) { this.nameRequestID++; batch(() => { this.name.value = value; this.nameError.value = ''; }); }

  async prefillRoleDefault() {
    const room = this.room, role = this.role.peek();
    if (this.resume.peek() || !room?.project_id || !role) return;
    const token = this.defaultPreviewToken = this.defaultPreviewToken + 1;
    try {
      const choice = await request(`/api/projects/${encodeURIComponent(room.project_id)}/role-spawn-defaults/${encodeURIComponent(role)}/next`);
      if (!this.alive || token !== this.defaultPreviewToken || !this.dialog.open || this.role.peek() !== role || !choice.available) return;
      if (choice.machine_id && this.machine.id.peek() !== choice.machine_id) {
        this.machine.select(choice.machine_id);
        await this.loadCatalog();
      }
      if (!this.alive || token !== this.defaultPreviewToken || !this.dialog.open) return;
      const index = this.catalog.peek().findIndex(item => item.launcher_id === choice.launcher_id && item.available);
      if (index >= 0) {
        this.toolIndex.value = index;
        this.rebuildModels();
        if (this.models.peek().some(option => option.value === choice.model)) this.model.value = choice.model;
      }
    } catch (error) { captureError(error, 'spawn.choice'); /* The ordinary first-available choice remains usable. */ }
  }

  async loadCatalog() {
    const machineID = this.machine.id.peek();
    const requestID = this.catalogRequestID = this.catalogRequestID + 1;
    // A reload (the machine changed, a live update) must not undo what the person picked: the tool they had is picked again when it is still
    // offered and available.
    const previousTool = this.pickedToolKey || '';
    batch(() => { this.catalog.value = []; this.toolIndex.value = -1; this.models.value = []; this.model.value = ''; this.reasonText.value = 'Loading available tools…'; });
    if (!machineID) return;
    try {
      const result = await this.host.source.catalog(machineID);
      if (!this.alive || !this.dialog.open || requestID !== this.catalogRequestID) return;
      const catalog = Array.isArray(result?.tools) ? result.tools : [];
      this.needsUpdate = !!result?.supervisor_update_needed;
      const kept = previousTool ? catalog.findIndex(tool => tool.available && toolKey(tool) === previousTool) : -1;
      const choice = kept < 0 ? resumeToolChoice(catalog, this.resume.peek()) : { index: null, disabled: null, note: '' };
      batch(() => {
        this.catalog.value = catalog;
        this.toolIndex.value = kept >= 0 ? kept : choice.index !== null ? choice.index : catalog.findIndex(tool => tool.available);
        this.toolsDisabled.value = kept < 0 ? choice.disabled : null;
        this.toolNote.value = kept < 0 ? choice.note : this.toolNote.peek();
        this.rebuildModels();
      });
      this.syncWorktree();
    } catch (error) {
      if (this.alive && this.dialog.open && requestID === this.catalogRequestID) this.reasonText.value = `Tool catalog unavailable: ${error.message}`;
    }
  }

  // The model list follows the tool; the person's own pick for this tool, else the model the start names, else the harness's default stays.
  rebuildModels() {
    const tool = this.tool.peek();
    const models = sortModelOptions((tool?.models || []).map(model => ({ value: typeof model === 'string' ? model : model.id, label: typeof model === 'string' ? model : model.label || model.id, note: model.note || '', default: !!model.default })));
    const preferred = (tool?.models || []).find(model => model.default);
    const chosen = chooseModel(models, { picked: this.pickedModel, toolKey: toolKey(tool), named: this.resume.peek()?.model, preferred: preferred?.id });
    batch(() => {
      this.models.value = models; if (chosen) this.model.value = chosen; else if (!models.length) this.model.value = '';
      this.reasonText.value = spawnReason({ needsUpdate: this.needsUpdate, tool, catalogCount: this.catalog.peek().length, modelCount: models.length });
      this.notesOverride.value = '';
    });
  }

  pickTool(value) {
    this.defaultPreviewToken++;
    const index = value === '' ? -1 : Number(value);
    this.toolIndex.value = index;
    // The person's tool is remembered on its own, so a catalog reload keeps it even when they never touched the model.
    this.pickedToolKey = toolKey(this.tool.peek());
    this.rebuildModels();
  }

  // What the person picked is kept while the list is rebuilt (the catalog arriving late, the same tool picked again).
  pickModel(value) {
    this.defaultPreviewToken++;
    this.pickedModel = { toolKey: toolKey(this.tool.peek()), model: value };
    batch(() => { this.model.value = value; this.notesOverride.value = ''; });
  }

  pickRole(value) {
    this.role.value = value;
    // Only a lead may start in the home room, so the choice of rooms follows the role.
    if (this.resume.peek()) { this.renderStartRooms({ keep: true }); this.roomTick.value++; this.syncWorktree(); }
    return this.prefillRoleDefault();
  }

  pickStartRoom(value) { this.startRoom.value = value; this.roomTick.value++; this.syncWorktree(); if (this.allRoles) this.filterRoles(); }

  pickMode(value) { batch(() => { this.mode.value = value; this.notesOverride.value = ''; }); }

  pickConfirmed(value) { this.confirmed.value = value; }

  // The same tool, the one that can continue a session, picked from the note under the session button.
  useTool(index) {
    this.defaultPreviewToken++; // a role-default prefill still in flight must not flip the tool back
    this.toolIndex.value = index; this.pickedToolKey = toolKey(this.catalog.peek()[index]); this.pickedModel = null; this.rebuildModels();
  }

  // The session picker: closes the Spawn dialog (the machine and project are taken from it) and opens the picker; a pick reopens the Spawn dialog.
  openSessionPicker() {
    const projectID = this.room?.project_id, machine = this.machine.machine;
    if (!projectID) return;
    const machineID = this.machine.id.peek(), machineName = machine?.name || '';
    // The sessions are those of the launcher that will continue them (its own Claude home); another tool's launcher means the default one.
    const chosen = this.tool.peek();
    const launcherID = chosen?.tool === 'claude' ? chosen.launcher_id || '' : '';
    this.dialog.close();
    const pick = this.pickToken = this.pickToken + 1;
    void this.picker.open({ projectID, machineID, machineName, launcherID, onPick: session => {
      if (!this.alive || pick !== this.pickToken) return;
      void this.open({ resume: { kind: 'claude_session', session_id: session.id, tool: 'claude', resumable: true, model: session.model || '', cwd: session.cwd, title: session.title || '',
        modified_at: session.modified_at || '', outside_folder: !!session.outside_folder, active: !!session.active, held_by: session.agent?.slug || '', machine_id: session.machine_id || machineID, launcher_id: session.launcher_id || '' } });
    } });
  }

  // The form is read as it stands when it is sent: a field set without an input event still counts.
  async submit(fields, { workOnCopy = false } = {}) {
    const target = this.target.peek();
    if (!this.room || !target) return;
    this.host.setError('');
    this.name.value = fields.name; this.baseRef.value = fields.baseRef;
    const resume = this.resume.peek(), mode = fields.mode;
    const request_ = spawnRequest({ role: this.role.peek(), tool: this.tool.peek(), model: this.model.peek(), typed: fields.name, machineID: this.machine.id.peek(), resume, mode, confirmed: fields.confirmed, worktree: this.worktree.peek(), baseRef: fields.baseRef, target, newRoom: this.startRoom.peek() === NEW_ROOM , workOnCopy });
    if (request_.skip) return;
    if (request_.nameError) { this.nameError.value = request_.nameError; this.host.focusName(); return; }
    if (request_.error) { this.host.setError(request_.error); return; }
    if (request_.confirm) { this.notesOverride.value = request_.confirm; this.host.focusConfirm(); return; }
    // The dialog can be closed and opened again for another room, or the panel reset, while the call is out; its outcome is then not this view's.
    const opening = this.opening, generation = this.host.generation, stale = () => !this.alive || !this.dialog.open || opening !== this.opening || generation !== this.host.generation;
    const continuing = resume?.kind === 'claude_session';
    try {
      // The archived room that holds the agent's v1 history is put back first (the same call as the room menu's Reopen), then the agent starts in it.
      if (this.startRoom.peek().startsWith(REOPEN_PREFIX)) {
        await request(`/api/rooms/${target.id}/reopen`, { method: 'POST', body: { lifecycle_rev: target.lifecycle_rev || 0 } });
        if (stale()) return;
        // The room is open now: a retry after a failed start goes to the room itself, not through another reopen
        // with the revision of the archived one, which the server would refuse.
        this.useReopenedRoom(target);
      }
      const card = await this.host.source.spawn(target.id, request_.params);
      if (stale()) return;
      this.dialog.close(); this.resetForm();
      // A session continued in a room of its own: that room is where the person goes.
      if (continuing && card?.room_id && card.room_id !== this.room.id && card.room_id !== target.id) await this.host.showRoomByID?.(this.room.project_id, card.room_id);
      else if (target.id !== this.room.id) await this.host.showRoom?.(target); else await this.host.refresh();
    } catch (error) {
      if (stale()) return;
      // A refused name is answered under its field and the dialog stays open, with everything else as it was, so another name can be tried.
      if (error.code === 'name_in_use' || error.code === 'invalid_name') { this.nameError.value = error.message; this.host.focusName(); return; }
      if (error.code === 'team_flow_rule') { this.host.setError(error.message); return; }
      this.host.setError(error.message); this.dialog.close();
    }
  }

  resetForm() { batch(() => { this.name.value = ''; this.worktree.value = false; this.baseRef.value = ''; this.mode.value = 'continue'; this.confirmed.value = false; }); }

  // Closing the dialog drops what a start from v1 history had set up.
  // The browser queues the close event after close(): one that arrives once the dialog was opened again belongs to the opening before.
  closed() {
    if (this.dialog.open) return;
    batch(() => { this.resume.value = null; this.mode.value = 'continue'; this.confirmed.value = false; this.notesOverride.value = ''; });
    this.renderStartRooms();
  }

  hint(text) { return hintParts(text, this.host.canManageHarnesses()); }

  destroy() { this.alive = false; this.picker.destroy(); this.invalidate(); }
}
