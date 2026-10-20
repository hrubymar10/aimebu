import { request, currentUser } from './api.js';
import { relativeTime } from './time.js';
import { confirmDialog } from './dialogs.js';


const SCOPE_TITLES = { all: 'All visible memory', project: 'Project facts', user_profile: 'User profiles', agent_shared_notes: 'Shared agent notes' };
const SCOPE_LABELS = { project: 'Project facts', user_profile: 'User profile', agent_shared_notes: 'Shared agent notes', task: 'Task notes' };
const GLOBAL_SCOPES = ['user_profile', 'agent_shared_notes'];
const byteLength = text => new TextEncoder().encode(text).length;

export class KnowledgePanel {
  constructor(projectContext, roomContext) {
    this.projectContext = projectContext;
    this.roomContext = roomContext;
    this.dialog = document.querySelector('#knowledge-dialog');
    this.title = document.querySelector('#knowledge-title');
    this.projectLabel = document.querySelector('#knowledge-project-label');
    this.projectSelect = document.querySelector('#knowledge-project');
    this.scopeSelect = document.querySelector('#knowledge-scope');
    this.scopeLabel = document.querySelector('#knowledge-scope-label');
    this.scopeKeyLabel = document.querySelector('#knowledge-scope-key-label');
    this.scopeKeyInput = document.querySelector('#knowledge-scope-key');
    this.enabledLabel = document.querySelector('#knowledge-enabled-label');
    this.enabledInput = document.querySelector('#knowledge-enabled');
    this.status = document.querySelector('#knowledge-status');
    this.records = document.querySelector('#knowledge-records');
	this.usage = document.createElement('div');
	this.usage.className = 'knowledge-usage';
	this.usage.setAttribute('aria-label', 'Memory usage by scope');
	this.status.after(this.usage);
	this.clean = document.createElement('div');
	this.clean.className = 'knowledge-actions';
	this.cleanButton = document.createElement('button');
	this.cleanButton.type = 'button'; this.cleanButton.className = 'btn btn-danger'; this.cleanButton.id = 'knowledge-clean'; this.cleanButton.textContent = 'Clear';
	this.cleanButton.onclick = () => this.cleanRecords();
	this.clean.append(this.cleanButton);
	this.records.before(this.clean);
    this.addForm = document.querySelector('#knowledge-add');
    this.error = document.querySelector('#knowledge-error');
    this.taskButton = document.querySelector('#task-notes-toggle');
    document.querySelector('#knowledge-toggle').onclick = () => this.openProject();
    this.taskButton.onclick = () => this.openTask();
    document.querySelector('#knowledge-close').onclick = () => this.dialog.close();
    this.dialog.addEventListener('close', () => { this.shownPath = null; });
    this.projectSelect.onchange = () => this.load();
    this.scopeSelect.onchange = () => {
      this.mode = this.scopeSelect.value;
      this.enabledLabel.hidden = this.mode !== 'project';
      this.projectLabel.hidden = this.mode !== 'project' && this.mode !== 'all';
      this.scopeKeyLabel.hidden = this.mode !== 'user_profile';
      this.title.textContent = SCOPE_TITLES[this.mode];
      this.labelProject();
      this.load();
    };
    this.enabledInput.onchange = () => this.changeEnabled();
    this.addForm.onsubmit = event => this.add(event);
  }

  setRoom(room) {
    this.taskButton.hidden = room?.kind !== 'task';
  }

  // In the All view the project picker only chooses whose project facts are listed next to the global records.
  labelProject() {
    const text = this.mode === 'all' ? 'Project facts from' : 'Project';
    const first = this.projectLabel.firstChild;
    if (first?.nodeType === Node.TEXT_NODE) first.textContent = text; else this.projectLabel.prepend(text);
  }

  // The viewer opens on "All visible": project facts, user profiles and shared notes together.
  async openProject() {
    this.mode = 'all';
    this.scopeSelect.value = 'all';
    this.scopeLabel.hidden = false;
    this.scopeKeyLabel.hidden = true;
    this.room = null;
    this.title.textContent = SCOPE_TITLES.all;
    this.projectLabel.hidden = false;
    this.labelProject();
    this.enabledLabel.hidden = true;
    const { projects, selectedProjectID } = this.projectContext();
    this.projectSelect.replaceChildren(...projects.map(project => {
      const option = document.createElement('option');
      option.value = project.id;
      option.textContent = project.name;
      return option;
    }));
    if (projects.some(project => project.id === selectedProjectID)) this.projectSelect.value = selectedProjectID;
    this.dialog.showModal();
    await this.load();
  }

  async openTask() {
    const room = this.roomContext();
    if (!room || room.kind !== 'task') return;
    this.mode = 'task';
    this.scopeLabel.hidden = true;
    this.scopeKeyLabel.hidden = true;
    this.room = room;
    this.title.textContent = `Notes · ${room.title}`;
    this.projectLabel.hidden = true;
    this.enabledLabel.hidden = true;
    this.dialog.showModal();
    await this.load();
  }

  path(scope = this.mode) {
    if (scope === 'task') return `/api/rooms/${encodeURIComponent(this.room.id)}/task-notes`;
    if (GLOBAL_SCOPES.includes(scope)) return `/api/global-memory/${scope}`;
    return `/api/projects/${encodeURIComponent(this.projectSelect.value)}/memory`;
  }

  recordScope(item) { return item.scope === 'project' || item.scope === 'task' || GLOBAL_SCOPES.includes(item.scope) ? item.scope : this.mode; }

  // One identity for what is on screen, so a same-view refresh keeps the records.
  viewKey() { return this.mode === 'all' ? `all:${this.projectSelect.value}` : this.path(); }

  async load() {
    const ticket = (this.ticket || 0) + 1;
    this.ticket = ticket;
    this.error.textContent = '';
    const needsProject = this.mode === 'project' || this.mode === 'all';
    const path = needsProject && !this.projectSelect.value ? null : this.viewKey();
    // Refreshing what is already on screen (mutation echo, SSE event) keeps the
    // rendered records: blanking them shrinks the centered modal, moving the
    // controls under a pointer that already targeted them.
    if (path === null || path !== this.shownPath) {
      this.shownPath = null;
      this.records.replaceChildren();
      this.status.textContent = 'Loading…';
    }
    if (!this.mode || needsProject && !this.projectSelect.value) {
      this.status.textContent = 'Choose a project.';
      this.addForm.hidden = true;
      return;
    }
    try {
      const data = await this.fetchData();
      if (ticket !== this.ticket || !this.dialog.open) return;
      this.data = data;
      this.shownPath = path;
      this.render();
    } catch (error) {
      if (ticket === this.ticket) {
        this.status.textContent = '';
        this.addForm.hidden = true;
        this.error.textContent = error.message || 'Could not load memory';
      }
    }
  }

  async fetchData() {
    if (this.mode !== 'all') {
      const data = await request(this.path());
      for (const item of data.records || []) { item._edit = !!data.can_edit && (this.mode !== 'project' || !!data.enabled); item._cap = data.cap_bytes; }
      return data;
    }
    const [project, ...globals] = await Promise.all(['project', ...GLOBAL_SCOPES].map(scope => request(this.path(scope))));
    const parts = [['project', project], ...GLOBAL_SCOPES.map((scope, index) => [scope, globals[index]])];
    const records = [];
    for (const [scope, part] of parts) {
      for (const item of part.records || []) {
        records.push({ ...item, scope, _cap: part.cap_bytes, _edit: !!part.can_edit && (scope !== 'project' || !!part.enabled) });
      }
    }
    return { records, enabled: project.enabled, can_edit: project.can_edit || globals.some(part => part.can_edit), cap_bytes: project.cap_bytes };
  }

  // Bytes used against each scope's cap: a project, one profile, or the shared
  // notes, mirroring the server's per-scope limits.
  usageRows(records) {
    const rows = new Map();
    for (const item of records) {
      const scope = this.recordScope(item);
      const key = scope === 'user_profile' ? item.scope_id : '';
      const id = `${scope}\u0000${key}`;
      const row = rows.get(id) || { scope, key: scope === 'user_profile' ? (item.scope_label || item.scope_id) : scope === 'project' ? (this.projectSelect.selectedOptions[0]?.textContent || '') : '', used: 0, count: 0, cap: item._cap };
      row.used += byteLength(item.body);
      row.count += 1;
      rows.set(id, row);
    }
    return [...rows.values()];
  }

  renderUsage(records) {
    this.usage.replaceChildren(...this.usageRows(records).map(row => {
      const wrap = document.createElement('div');
      wrap.className = 'knowledge-usage-row';
      const meta = document.createElement('div');
      meta.className = 'knowledge-usage-meta';
      meta.textContent = `${SCOPE_LABELS[row.scope] || row.scope}${row.key ? ` · ${row.key}` : ''} · ${row.used} / ${row.cap} bytes · ${row.count} ${row.count === 1 ? 'record' : 'records'}`;
      const bar = document.createElement('div');
      bar.className = 'knowledge-usage-bar';
      bar.setAttribute('role', 'progressbar');
      bar.setAttribute('aria-valuemin', '0');
      bar.setAttribute('aria-valuemax', String(row.cap));
      bar.setAttribute('aria-valuenow', String(row.used));
      const fill = document.createElement('span');
      fill.style.width = `${row.cap ? Math.min(100, Math.round(row.used / row.cap * 100)) : 0}%`;
      bar.append(fill);
      wrap.append(meta, bar);
      return wrap;
    }));
    this.usage.hidden = !records.length;
  }

  render() {
    const data = this.data;
    const project = this.mode === 'project';
    const all = this.mode === 'all';
    const writable = !all && data.can_edit && (!project || data.enabled);
	const visible = data.records || [];
    this.enabledInput.checked = !!data.enabled;
    this.enabledInput.disabled = !data.can_edit;
    const used = (data.records || []).reduce((sum, item) => sum + new TextEncoder().encode(item.body).length, 0);
    const profileUsage = new Map();
    if (this.mode === 'user_profile') for (const item of data.records) profileUsage.set(item.scope_id, (profileUsage.get(item.scope_id) || 0) + new TextEncoder().encode(item.body).length);
    const projectName = this.projectSelect.selectedOptions[0]?.textContent || 'this project';
    const counts = { project: 0, user_profile: 0, agent_shared_notes: 0 };
    if (all) for (const item of data.records) counts[this.recordScope(item)] = (counts[this.recordScope(item)] || 0) + 1;
    this.status.textContent = (all ? `${data.records.length} records: ${counts.project} project facts of ${projectName}, ${counts.user_profile} user profiles, ${counts.agent_shared_notes} shared notes. Other projects' facts are in the Project facts view.` : this.mode === 'user_profile' ? `${data.records.length} records · ${profileUsage.size} profiles · ${data.cap_bytes} bytes per profile` : `${used} / ${data.cap_bytes} bytes · ${data.records.length} records`) +
      ((project || all) && !data.enabled ? ` · Memory is off for ${projectName}: agents do not get its project facts, and saved records stay available.` : '');
    this.addForm.hidden = !writable;
    this.renderUsage(data.records || []);
    const clearable = (data.records || []).filter(item => item._edit);
    this.clean.hidden = !clearable.length;
    this.cleanButton.textContent = all ? 'Clear all' : 'Clear filtered';
    this.cleanButton.disabled = !clearable.length;
    this.records.replaceChildren();
    if (!visible.length) {
      const empty = document.createElement('p');
      empty.className = 'settings-intro';
	  empty.textContent = all ? 'No visible memory saved.' : project ? 'No project facts saved.' : this.mode === 'task' ? 'No notes saved for this task.' : 'No global memory saved.';
      this.records.append(empty);
    }
    for (const item of visible) {
      const card = document.createElement('article');
      card.className = 'knowledge-record';
      const meta = document.createElement('small');
	  const scope = this.recordScope(item);
	  meta.textContent = `${all ? SCOPE_LABELS[scope] + (scope === 'user_profile' ? ` ${item.scope_label || item.scope_id}` : '') + ' · ' : ''}${this.mode === 'user_profile' ? (item.scope_label || item.scope_id) + ` · ${profileUsage.get(item.scope_id)} / ${data.cap_bytes} bytes · ` : ''}${GLOBAL_SCOPES.includes(scope) && !item.active ? 'Inactive · ' : ''}Revision ${item.rev} · ${item.author_kind} ${item.author_id} · ${relativeTime(item.updated_at)}`; meta.title = item.updated_at ? new Date(item.updated_at).toLocaleString() : '';
      const body = document.createElement('textarea');
      body.value = item.body;
      body.rows = 3;
      body.maxLength = 2000;
      body.setAttribute('aria-label', `Record ${item.id}`);
	  body.readOnly = !item._edit;
      card.append(meta, body);
      if (item._edit) {
        const actions = document.createElement('div');
        actions.className = 'knowledge-actions';
        const save = document.createElement('button');
        save.type = 'button'; save.className = 'btn btn-primary'; save.textContent = 'Save';
        save.onclick = () => this.changeRecord(item, body.value, false);
        const remove = document.createElement('button');
        remove.type = 'button'; remove.className = 'btn btn-danger'; remove.textContent = 'Delete';
        remove.onclick = async () => {
          if (await confirmDialog('Delete this record?', { title: 'Delete record', confirm: 'Delete', danger: true })) this.changeRecord(item, '', true);
        };
        actions.append(save, remove);
        if (GLOBAL_SCOPES.includes(scope)) {
          const activation = document.createElement('button');
          activation.type = 'button';
          activation.className = 'btn';
          activation.textContent = item.active ? 'Deactivate' : 'Activate for agents';
          activation.onclick = () => this.changeActivation(item, !item.active);
          actions.append(activation);
        }
        card.append(actions);
      }
      this.records.append(card);
    }
  }

  async changeEnabled() {
    const enabled = this.enabledInput.checked;
    this.enabledInput.disabled = true;
    this.error.textContent = '';
    try {
      await request(`/api/projects/${encodeURIComponent(this.projectSelect.value)}/memory-enabled`, {
        method: 'PUT', body: { enabled }
      });
      await this.load();
    } catch (error) {
      this.enabledInput.checked = !enabled;
      this.enabledInput.disabled = false;
      this.error.textContent = error.message || 'Could not change memory setting';
    }
  }

  async add(event) {
    event.preventDefault();
    this.error.textContent = '';
    const input = this.addForm.elements.body;
    try {
      const body = { body: input.value };
      if (this.mode === 'user_profile') body.scope_key = this.scopeKeyInput.value.trim() || currentUser;
      if (this.mode === 'agent_shared_notes') body.scope_key = 'global';
      await request(this.path(), { method: 'POST', body });
      input.value = '';
      await this.load();
    } catch (error) { this.error.textContent = error.message || 'Could not add record'; }
  }

  async changeRecord(item, body, remove) {
    this.error.textContent = '';
    const path = `${this.path(this.recordScope(item))}/${encodeURIComponent(item.id)}`;
    try {
      if (remove) await request(`${path}?rev=${item.rev}&scope_key=${encodeURIComponent(item.scope_id)}`, { method: 'DELETE' });
      else await request(path, { method: 'PUT', body: { body, rev: item.rev, scope_key: item.scope_id } });
      await this.load();
    } catch (error) { this.error.textContent = error.message || 'Could not save record'; }
  }

  async changeActivation(item, active) {
    this.error.textContent = '';
    try {
      await request(`${this.path(this.recordScope(item))}/${encodeURIComponent(item.id)}/activation`, {
        method: 'PUT', body: { scope_key: item.scope_id, rev: item.rev, active }
      });
      await this.load();
    } catch (error) { this.error.textContent = error.message || 'Could not change activation'; }
  }

  // Deletes every editable record in the current view, one request each so the
  // server's per-record revision check still applies.
  async cleanRecords() {
    const targets = (this.data.records || []).filter(item => item._edit);
    if (!targets.length) return;
    const what = this.mode === 'all' ? 'all visible memory records' : 'the records in this view';
    if (!await confirmDialog(`Delete ${targets.length} of ${what}? This cannot be undone.`, { title: 'Delete records', confirm: 'Delete', danger: true })) return;
    this.error.textContent = '';
    this.cleanButton.disabled = true;
    let removed = 0;
    try {
      for (const item of targets) {
        const path = `${this.path(this.recordScope(item))}/${encodeURIComponent(item.id)}`;
        await request(`${path}?rev=${item.rev}&scope_key=${encodeURIComponent(item.scope_id)}`, { method: 'DELETE' });
        removed += 1;
      }
    } catch (error) {
      this.error.textContent = `Deleted ${removed} of ${targets.length}: ${error.message || 'could not delete record'}`;
    }
    await this.load();
  }

  onEvent(event) {
    if (!this.dialog.open || !['knowledge_created', 'knowledge_updated', 'knowledge_deleted', 'project_memory_setting_changed', 'global_knowledge_created', 'global_knowledge_updated', 'global_knowledge_deleted'].includes(event.kind)) return;
    if ((this.mode === 'project' || this.mode === 'all') && event.project_id === this.projectSelect.value || this.mode === 'all' && event.kind.startsWith('global_knowledge_') || this.mode === 'task' && event.room_id === this.room?.id || ['user_profile', 'agent_shared_notes'].includes(this.mode) && event.kind.startsWith('global_knowledge_')) this.load();
  }
}
