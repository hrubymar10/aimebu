import { batch, signal } from '../../ui.js';
import { request, rights } from '../../api.js';
import { confirmDialog, formDialog } from '../../dialogs.js';
import { createAgentSource } from '../../agent-source.js';
import { ClaudeSessionImport } from '../claude-import/page.js';
import { ScopeStore } from '../../components/scope-control.js';
import { UsageSummaryStore } from '../../components/usage-summary.js';
import { UsageLimitStore } from './usage-limit.js';
import { TeamFlowStore } from './team-flow-store.js';
import { detailsChanges, detailsView, lifecycleView, stopFailedNote } from './model.js';

const FLAGS = { autoResume: 'auto_resume_after_clean_diagnostic', worktrees: 'worktree_mode', homeLeadOnly: 'home_lead_only', workerRecoveryToLead: 'worker_recovery_to_lead' };
const flagValue = (name, settings) => (name === 'worktrees' ? settings.worktree_mode !== 'off' : name === 'homeLeadOnly' || name === 'workerRecoveryToLead' ? settings[FLAGS[name]] !== false : !!settings[FLAGS[name]]);
const flagBody = (name, checked) => (name === 'worktrees' ? { worktree_mode: checked ? 'auto' : 'off' } : { [FLAGS[name]]: checked });

export class ProjectStore {
  constructor(source, projectContext) {
    this.source = source;
    this.projectContext = projectContext;
    this.projectID = signal('');
    this.revision = signal(0);
    this.error = signal('');
    this.view = signal(detailsView(null, false));
    this.project = signal(null);
    this.name = signal('');
    this.folder = signal('');
    this.loaded = { name: '', folder: '' };
    this.detailsStatus = signal('');
    this.detailsBusy = signal(false);
    this.flags = { autoResume: signal({ checked: false, disabled: true }), worktrees: signal({ checked: false, disabled: true }), homeLeadOnly: signal({ checked: false, disabled: true }), workerRecoveryToLead: signal({ checked: false, disabled: true }) };
    this.workerRecoveryMinutes = signal({ value: 30, disabled: true });
    this.reviewerAgents = signal({ value: 'inherit', global: true, enabled: true, disabled: true });
    this.lifecycleBusy = signal('');
    this.lifecycleTurn = 0;
    this.lifecycleNote = signal('');
    this.scope = new ScopeStore('project');
    this.limit = new UsageLimitStore();
    this.usage = new UsageSummaryStore(id => `/api/projects/${id}/usage`);
    this.flow = new TeamFlowStore({ onApplied: (id, result) => this.onFlowApplied?.(id, result), canPreview: () => ['owner', 'operator'].includes(this.project.peek()?.role),
      retainSelection: () => !!document.querySelector('#team-flow-modal')?.open });
    this.alive = true;
    this.generation = 0;
    this.onLifecycle = null;
    this.onRemoved = null;
    this.onRenamed = null;
    this.onFlowApplied = null;
  }

  // After a dialog: the page is still here, and still shows the project the dialog was about.
  shows(id) { return this.alive && this.projectID.peek() === id; }

  currentProject() { return this.projectContext().projects.find(item => item.id === this.projectID.peek()); }

  // Takes the project from the shell's list again: its role, its name and folder, and whether it is archived.
  refreshDetails(id) {
    const project = this.projectContext().projects.find(item => item.id === id), view = detailsView(project, rights.admin);
    batch(() => {
      this.project.value = project || null; this.view.value = view; this.name.value = project?.name || ''; this.folder.value = project?.folder || '';
      this.detailsStatus.value = view.status; this.lifecycleNote.value = lifecycleView(project, view.canName).note;
    });
    this.loaded = { name: this.name.peek(), folder: this.folder.peek() };
  }

  async load(id) {
    if (!this.alive) return;
    const generation = ++this.generation;
    batch(() => {
      this.projectID.value = id || ''; this.revision.value++; this.error.value = ''; this.lifecycleBusy.value = ''; this.lifecycleTurn++;
      for (const flag of Object.values(this.flags)) flag.value = { checked: false, disabled: true };
      this.workerRecoveryMinutes.value = { value: 30, disabled: true };
      this.reviewerAgents.value = { value: 'inherit', global: true, enabled: true, disabled: true };
    });
    this.refreshDetails(id);
    this.scope.load(id); this.limit.load(id); this.usage.load(id); this.flow.load(id);
    if (!id) { this.error.value = 'Choose a project first.'; return; }
    try {
      const settings = await this.source.get(id);
      if (!this.alive || generation !== this.generation) return;
      for (const name of Object.keys(this.flags)) this.flags[name].value = { checked: flagValue(name, settings), disabled: false };
      this.workerRecoveryMinutes.value = { value: settings.worker_recovery_escalate_minutes || 30, disabled: false };
      this.reviewerAgents.value = { value: settings.reviewer_agents || 'inherit', global: settings.reviewer_agents_global_enabled !== false, enabled: settings.reviewer_agents_enabled !== false, disabled: false };
    } catch (failure) { if (this.alive && generation === this.generation) this.error.value = failure.message; }
  }

  // The values are read from the form when it is submitted, whatever wrote them.
  async saveDetails(drafts) {
    const id = this.projectID.peek(), generation = this.generation;
    if (!this.alive) return;
    const body = detailsChanges(drafts, this.loaded, this.view.peek());
    this.detailsStatus.value = '';
    if (!Object.keys(body).length) { this.detailsStatus.value = 'Nothing changed.'; return; }
    this.detailsBusy.value = true;
    try {
      const project = await this.source.edit(id, body);
      if (!this.alive || generation !== this.generation) return;
      this.loaded = { name: project.name, folder: project.folder };
      batch(() => { this.name.value = project.name; this.folder.value = project.folder; this.detailsStatus.value = 'Saved.'; });
      this.onRenamed?.(id, project.name);
    } catch (failure) { if (this.alive && generation === this.generation) this.detailsStatus.value = failure.message; }
    finally { if (this.alive && generation === this.generation) this.detailsBusy.value = false; }
  }

  // A switch saves the moment it is pressed, goes back when the server refuses, and is off while it is saving.
  async saveFlag(name, checked) {
    const id = this.projectID.peek(), generation = this.generation;
    if (!this.alive) return;
    batch(() => { this.error.value = ''; this.flags[name].value = { checked, disabled: true }; });
    try {
      await this.source.update(id, flagBody(name, checked));
      if (this.alive && generation === this.generation) this.flags[name].value = { checked, disabled: false };
    } catch (failure) {
      if (this.alive && generation === this.generation) batch(() => { this.flags[name].value = { checked: !checked, disabled: false }; this.error.value = failure.message; });
    }
  }

  async saveWorkerRecoveryMinutes(value) {
    const id = this.projectID.peek(), generation = this.generation, previous = this.workerRecoveryMinutes.peek();
    const minutes = Number(value);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > 1440) { this.error.value = 'Escalation interval must be 1 to 1440 minutes.'; return; }
    this.workerRecoveryMinutes.value = { value: minutes, disabled: true };
    try {
      const settings = await this.source.update(id, { worker_recovery_escalate_minutes: minutes });
      if (this.alive && generation === this.generation) this.workerRecoveryMinutes.value = { value: settings.worker_recovery_escalate_minutes, disabled: false };
    } catch (failure) {
      if (this.alive && generation === this.generation) { this.workerRecoveryMinutes.value = previous; this.error.value = failure.message; }
    }
  }

  async saveReviewerAgents(value) {
    const id = this.projectID.peek(), generation = this.generation, previous = this.reviewerAgents.peek();
    this.reviewerAgents.value = { ...previous, value, disabled: true };
    try {
      const settings = await this.source.update(id, { reviewer_agents: value });
      if (this.alive && generation === this.generation) this.reviewerAgents.value = { value: settings.reviewer_agents ?? value, global: settings.reviewer_agents_global_enabled ?? previous.global, enabled: settings.reviewer_agents_enabled ?? (value === 'on' || (value === 'inherit' && previous.global)), disabled: false };
    } catch (failure) {
      if (this.alive && generation === this.generation) { this.reviewerAgents.value = previous; this.error.value = failure.message; }
    }
  }

  async lifecycleCall(key, id, path, options) {
    if (!this.alive || this.projectID.peek() !== id) return null;
    const turn = ++this.lifecycleTurn;
    batch(() => { this.error.value = ''; this.lifecycleBusy.value = key; });
    const current = () => this.alive && this.projectID.peek() === id && turn === this.lifecycleTurn;
    try {
      const data = await request(`/api/projects/${encodeURIComponent(id)}${path}`, options);
      return current() ? { data } : null;
    } catch (failure) { if (current()) this.error.value = failure.message; return null; }
    finally { if (this.alive && turn === this.lifecycleTurn) this.lifecycleBusy.value = ''; }
  }

  async archive() {
    const project = this.currentProject(); if (!project) return;
    if (!await confirmDialog(`Archive “${project.name}”? Every agent in it will be stopped, its schedules switched off and its rooms made read-only. You can reopen it later.`, { title: 'Archive project', confirm: 'Archive project', danger: true }) || !this.shows(project.id)) return;
    const done = await this.lifecycleCall('archive', project.id, '/archive', { method: 'POST' });
    if (!done || !this.alive) return;
    await this.onLifecycle?.(project.id);
    // An agent whose execution is unknown cannot be stopped until its recovery is decided in the Inbox; say so beside the result.
    const failed = done.data?.stop_failed || [];
    if (failed.length && this.alive && this.projectID.peek() === project.id) this.lifecycleNote.value = stopFailedNote(failed);
  }

  async reopen() {
    const project = this.currentProject(); if (!project) return;
    if (await this.lifecycleCall('reopen', project.id, '/reopen', { method: 'POST' }) && this.alive) await this.onLifecycle?.(project.id);
  }

  async remove() {
    const project = this.currentProject(); if (!project) return;
    const answer = await formDialog({ title: 'Delete project', danger: true, confirm: 'Delete project',
      message: `This stops the project's agents and permanently deletes “${project.name}”: every room, message, reaction, attachment, agent, usage record, memory, schedule, task note, Inbox item and member. It cannot be undone. Files in the project folder are not touched.`,
      fields: [{ name: 'name', label: `Type “${project.name}” to confirm`, required: true, requiredMessage: 'Type the project name to confirm.', validate: value => value === project.name ? '' : 'That is not the project name.' }] });
    if (!answer || !this.shows(project.id)) return;
    const done = await this.lifecycleCall('delete', project.id, '', { method: 'DELETE', body: { name: answer.name } });
    if (done && this.alive) await this.onRemoved?.(project.id);
    else if (!done && !project.archived_at && this.shows(project.id)) {
      // A pending stop may leave the project archived even though deletion returned 409.
      try { await this.onLifecycle?.(project.id); } catch { /* keep the deletion error visible */ }
    }
  }

  // Importing the Claude Code sessions a machine already has: one agent and one room per session, each on a copy of its session.
  openSessionImport() {
    if (!this.alive) return;
    const project = this.currentProject(), id = this.projectID.peek();
    const home = (this.projectContext().rooms || []).find(room => room.project_id === id && room.kind === 'home');
    if (!project || !home) { this.error.value = 'This project has no home room to import into.'; return; }
    this.importer ||= new ClaudeSessionImport(createAgentSource());
    void this.importer.open({ projectID: id, homeRoomID: home.id });
  }

  lifecycle() { const view = this.view.value; return lifecycleView(this.project.value, view.canName); }

  destroy() { this.alive = false; this.generation++; this.scope.destroy(); this.limit.destroy(); this.usage.destroy(); this.flow.destroy(); this.importer?.destroy(); }
}
