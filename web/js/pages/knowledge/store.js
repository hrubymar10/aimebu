import { batch, signal } from '../../ui.js';
import { request, currentUser } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { GLOBAL_SCOPES, SCOPE_TITLES, editable, eventTouches, recordScope, scopePath } from './model.js';

export class KnowledgeStore {
  constructor({ projectContext, roomContext, dialog }) {
    this.projectContext = projectContext;
    this.roomContext = roomContext;
    this.dialog = dialog;
    this.mode = signal('');
    this.projects = signal([]);
    this.projectID = signal('');
    this.title = signal('Memory');
    this.data = signal(null);
    this.status = signal('');
    this.error = signal('');
    this.scopeKey = signal('');
    this.saving = signal(false);
    this.cleaning = signal(false);
    this.seq = signal(0);
    this.blocked = signal(false);
    this.room = null;
    this.shownPath = null;
    this.ticket = 0;
    this.view = 0;
    this.enabledTurn = 0;
    this.cleanTurn = 0;
    this.alive = true;
  }

  get projectName() { return this.projects.value.find(project => project.id === this.projectID.value)?.name || ''; }

  path(scope = this.mode.peek()) { return scopePath(scope, { projectID: this.projectID.peek(), roomID: this.room?.id }); }

  viewKey() { return this.mode.peek() === 'all' ? `all:${this.projectID.peek()}` : this.path(); }

  async openProject(scope = 'all', projectID = '') {
    if (!this.alive) return;
    this.leave();
    let { projects, selectedProjectID } = this.projectContext();
    if (projectID) selectedProjectID = projectID;
    batch(() => {
      this.mode.value = scope; this.room = null; this.title.value = 'Memory';
      this.projects.value = projects.map(project => ({ id: project.id, name: project.name }));
      this.projectID.value = projects.some(project => project.id === selectedProjectID) ? selectedProjectID : (projects[0]?.id || '');
    });
    this.dialog.showModal();
    await this.load();
  }

  openProjectFacts(projectID) { return this.openProject('project', projectID); }

  async openTask() {
    const room = this.roomContext();
    if (!this.alive || !room || room.kind !== 'task') return;
    this.leave();
    this.room = room;
    batch(() => { this.mode.value = 'task'; this.title.value = `Notes · ${room.title}`; });
    this.dialog.showModal();
    await this.load();
  }

  selectScope(scope) {
    this.leave();
    batch(() => { this.mode.value = scope; this.title.value = 'Memory'; });
    return this.load();
  }

  selectProject(id) { this.leave(); this.projectID.value = id; return this.load(); }

  // A new view: what was begun on the old one no longer owns the memory switch.
  leave() { this.view++; this.enabledTurn++; this.cleanTurn++; batch(() => { this.saving.value = false; this.cleaning.value = false; }); }

  async load() {
    if (!this.alive) return;
    const ticket = ++this.ticket, mode = this.mode.peek();
    this.error.value = '';
    const needsProject = mode === 'project' || mode === 'all';
    const path = needsProject && !this.projectID.peek() ? null : this.viewKey();
    // Refreshing what is already on screen (a mutation's echo, an event) keeps the records: blanking them would shrink the centered modal
    // and move the controls under a pointer that already targeted them.
    if (path === null || path !== this.shownPath) { this.shownPath = null; batch(() => { this.data.value = null; this.status.value = 'Loading…'; }); }
    if (!mode || (needsProject && !this.projectID.peek())) { batch(() => { this.status.value = 'Choose a project.'; this.data.value = null; this.blocked.value = true; }); return; }
    try {
      const data = await this.fetchData(mode);
      if (!this.alive || ticket !== this.ticket || !this.dialog.open) return;
      this.shownPath = path;
      batch(() => { this.data.value = data; this.blocked.value = false; this.seq.value++; });
    } catch (failure) {
      if (this.alive && ticket === this.ticket) batch(() => { this.status.value = ''; this.blocked.value = true; this.error.value = failure.message || 'Could not load memory'; });
    }
  }

  async fetchData(mode) {
    if (mode !== 'all') {
      const data = await request(this.path());
      for (const item of data.records || []) { item._edit = editable(mode, data); item._cap = data.cap_bytes; }
      return data;
    }
    const [project, ...globals] = await Promise.all(['project', ...GLOBAL_SCOPES].map(scope => request(this.path(scope))));
    const records = [];
    for (const [scope, part] of [['project', project], ...GLOBAL_SCOPES.map((scope, index) => [scope, globals[index]])]) {
      for (const item of part.records || []) records.push({ ...item, scope, _cap: part.cap_bytes, _edit: editable(scope, part) });
    }
    return { records, enabled: project.enabled, can_edit: project.can_edit || globals.some(part => part.can_edit), cap_bytes: project.cap_bytes };
  }

  // A write that is about what was on screen when it began: its refusal is not shown once the viewer has moved on. The list is read again after it worked.
  async write(send, failed) {
    if (!this.alive) return false;
    const view = this.view;
    this.error.value = '';
    try { await send(); } catch (failure) { if (this.alive && view === this.view) this.error.value = failure.message || failed; return false; }
    if (!this.alive || view !== this.view || !this.dialog.open) return false;
    await this.load();
    return true;
  }

  async changeEnabled(enabled) {
    if (!this.alive) return;
    const view = this.view, project = this.projectID.peek(), turn = ++this.enabledTurn;
    this.saving.value = true; this.error.value = '';
    try {
      await request(`/api/projects/${encodeURIComponent(project)}/memory-enabled`, { method: 'PUT', body: { enabled } });
      if (!this.alive || turn !== this.enabledTurn || view !== this.view || !this.dialog.open) return;
      this.saving.value = false;
      await this.load();
    } catch (failure) {
      if (!this.alive || turn !== this.enabledTurn) return;
      this.saving.value = false;
      if (view === this.view) { this.seq.value++; this.error.value = failure.message || 'Could not change memory setting'; }
    }
  }

  add(body) {
    const mode = this.mode.peek();
    const payload = { body };
    if (mode === 'user_profile') payload.scope_key = this.scopeKey.peek().trim() || currentUser;
    if (mode === 'agent_shared_notes') payload.scope_key = 'global';
    const path = this.path();
    return this.write(() => request(path, { method: 'POST', body: payload }), 'Could not add record');
  }

  save(item, body) {
    const path = `${this.path(recordScope(item, this.mode.peek()))}/${encodeURIComponent(item.id)}`;
    return this.write(() => request(path, { method: 'PUT', body: { body, rev: item.rev, scope_key: item.scope_id } }), 'Could not save record');
  }

  async remove(item) {
    if (!this.alive) return;
    const view = this.view, path = `${this.path(recordScope(item, this.mode.peek()))}/${encodeURIComponent(item.id)}`;
    if (!await confirmDialog('Delete this record?', { title: 'Delete record', confirm: 'Delete', danger: true })) return;
    if (!this.alive || view !== this.view) return;
    await this.write(() => request(`${path}?rev=${item.rev}&scope_key=${encodeURIComponent(item.scope_id)}`, { method: 'DELETE' }), 'Could not save record');
  }

  activate(item, active) {
    const path = `${this.path(recordScope(item, this.mode.peek()))}/${encodeURIComponent(item.id)}/activation`;
    return this.write(() => request(path, { method: 'PUT', body: { scope_key: item.scope_id, rev: item.rev, active } }), 'Could not change activation');
  }

  // Deletes every editable record in the view, one request each so the server's per-record revision check still applies.
  async clean() {
    if (!this.alive) return;
    const view = this.view, mode = this.mode.peek();
    const targets = (this.data.peek()?.records || []).filter(item => item._edit);
    if (!targets.length) return;
    const what = mode === 'all' ? 'all visible memory records' : 'the records in this view';
    if (!await confirmDialog(`Delete ${targets.length} of ${what}? This cannot be undone.`, { title: 'Delete records', confirm: 'Delete', danger: true })) return;
    if (!this.alive || view !== this.view) return;
    const turn = ++this.cleanTurn;
    batch(() => { this.error.value = ''; this.cleaning.value = true; });
    let removed = 0, message = '';
    try {
      for (const item of targets) {
        if (!this.alive || view !== this.view) break;
        const path = `${this.path(recordScope(item, mode))}/${encodeURIComponent(item.id)}`;
        await request(`${path}?rev=${item.rev}&scope_key=${encodeURIComponent(item.scope_id)}`, { method: 'DELETE' });
        removed += 1;
      }
    } catch (failure) { message = `Deleted ${removed} of ${targets.length}: ${failure.message || 'could not delete record'}`; }
    if (!this.alive) return;
    if (turn === this.cleanTurn) this.cleaning.value = false;
    if (view === this.view && this.dialog.open) await this.load();
    // The read clears the error, so the account of a partial clear is set after it.
    if (message && this.alive && view === this.view) this.error.value = message;
  }

  onEvent(event) {
    if (!this.alive || !this.dialog.open) return;
    if (eventTouches(event, { mode: this.mode.peek(), projectID: this.projectID.peek(), roomID: this.room?.id })) this.load();
  }

  closed() { this.shownPath = null; }

  destroy() { this.alive = false; this.ticket++; this.view++; }
}
