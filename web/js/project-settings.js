import { UsageSummary } from './usage-summary.js';
import { request, rights } from './api.js';
import { ScopeControl } from './scope-control.js';
import { UsageLimit } from './usage-limit.js';
import { SpawnDefaultsEditor } from './spawn-defaults-editor.js';
import { iconButton } from './icon-button.js';
import { confirmDialog, formDialog } from './dialogs.js';
import { createAgentSource } from './agent-source.js';
import { ClaudeSessionImport } from './claude-session-import.js';

// The project's own settings (name and folder, its agents, daily limits, the home room's lead-only rule, recovery and worktrees). The form body lives in the Project
// section of the home room's settings (there is no Settings → Project page); mount/release keep it consistent if more than one place ever shows it.
export class ProjectSettings {
  constructor(source, projectContext) {
    this.source = source;
    this.projectContext = projectContext;
    this.body = document.querySelector('#project-settings-body');
    this.scopeControl = new ScopeControl(document.querySelector('#project-scope-control'), 'project');
    this.usageLimit = new UsageLimit(document.querySelector('#project-usage-limit'));
    this.usageSummary = new UsageSummary(document.querySelector('#project-usage'), id => `/api/projects/${id}/usage`);
    this.autoResume = document.querySelector('#project-auto-resume');
    this.worktrees = document.querySelector('#project-worktrees');
    this.homeLeadOnly = document.querySelector('#project-home-lead-only');
    this.error = document.querySelector('#project-settings-error');
    this.details = document.querySelector('#project-details');
    this.roleDefaultsSection = document.createElement('section'); this.roleDefaultsSection.className = 'settings-section';
    this.roleDefaultsSection.innerHTML = '<h3>Default agents per role</h3><p class="settings-intro">Inherit the global list or set an ordered list for this project.</p>';
    this.roleDefaultsRows = document.createElement('div'); this.roleDefaultsSection.append(this.roleDefaultsRows);
    this.body.querySelector('#project-usage-limit').after(this.roleDefaultsSection);
    this.buildSessionImport();
    this.buildLifecycle();
    this.projectID = '';
    this.mounts = [];
    this.details.onsubmit = event => { event.preventDefault(); this.saveDetails(); };
    this.autoResume.onchange = () => this.saveProject();
    this.worktrees.onchange = () => this.saveWorktrees();
    this.homeLeadOnly.onchange = () => this.saveToggle(this.homeLeadOnly, 'home_lead_only', this.homeLeadOnly.checked);
  }
  // Importing the Claude Code sessions a machine already has: one agent and one room per session, each on a copy of its session.
  buildSessionImport() {
    const section = this.sessionImport = document.createElement('div'); section.className = 'settings-section'; section.id = 'project-session-import';
    const heading = document.createElement('h3'); heading.textContent = 'Claude Code sessions';
    const group = document.createElement('div'); group.className = 'settings-group'; group.setAttribute('role', 'group'); group.setAttribute('aria-label', 'Claude Code sessions');
    const holder = document.createElement('div'); holder.className = 'settings-row';
    const info = document.createElement('div'); info.className = 'settings-row-info';
    const name = document.createElement('label'); name.className = 'settings-label'; name.htmlFor = 'project-session-import-open'; name.textContent = 'Import sessions';
    const desc = document.createElement('span'); desc.className = 'settings-desc'; desc.id = 'project-session-import-desc';
    desc.textContent = 'Turn the Claude Code sessions a machine has for this project\'s folder into agents: one room and one agent per session, each working on a copy and idle until it gets a message. The project\'s agent and spawn-rate limits apply.';
    info.append(name, desc);
    const control = document.createElement('div'); control.className = 'settings-control';
    this.sessionImportButton = iconButton('play', 'Import Claude Code sessions'); this.sessionImportButton.id = 'project-session-import-open';
    this.sessionImportButton.setAttribute('aria-describedby', desc.id);
    this.sessionImportButton.onclick = () => this.openSessionImport();
    control.append(this.sessionImportButton); holder.append(info, control); group.append(holder); section.append(heading, group);
    this.body.querySelector('#project-settings-error').before(section);
  }
  openSessionImport() {
    const project = this.currentProject();
    const home = (this.projectContext().rooms || []).find(room => room.project_id === this.projectID && room.kind === 'home');
    if (!project || !home) { this.error.textContent = 'This project has no home room to import into.'; return; }
    this.importer ||= new ClaudeSessionImport(createAgentSource());
    void this.importer.open({ projectID: this.projectID, homeRoomID: home.id });
  }
  // Archive, reopen and delete: the project's owner or an admin. An archived project is read-only and its agents are stopped; only an
  // archived one can be deleted, after its name is typed. Each row shows only what applies to the project's state.
  buildLifecycle() {
    const section = this.lifecycle = document.createElement('div'); section.className = 'settings-section'; section.id = 'project-lifecycle';
    const group = document.createElement('div'); group.className = 'settings-group'; group.setAttribute('role', 'group'); group.setAttribute('aria-label', 'Archive and delete');
    const heading = document.createElement('h3'); heading.textContent = 'Archive';
    const row = (id, title, description, label, glyph, danger, action) => {
      const holder = document.createElement('div'); holder.className = 'settings-row'; holder.id = `${id}-row`;
      const info = document.createElement('div'); info.className = 'settings-row-info';
      const name = document.createElement('label'); name.className = 'settings-label'; name.htmlFor = id; name.textContent = title;
      const desc = document.createElement('span'); desc.className = 'settings-desc'; desc.id = `${id}-desc`; desc.textContent = description;
      info.append(name, desc);
      const control = document.createElement('div'); control.className = 'settings-control';
      const button = iconButton(glyph, label, { danger }); button.id = id; button.setAttribute('aria-describedby', desc.id); button.onclick = action;
      control.append(button); holder.append(info, control);
      return { holder, button };
    };
    this.archiveRow = row('project-archive', 'Archive project', 'Make the project and all its rooms read-only, stop its agents and switch its schedules off. It moves to Archived projects in the room list, and can be reopened.', 'Archive project', 'archive', false, () => this.archive());
    this.reopenRow = row('project-reopen', 'Reopen project', 'Let the project accept changes again. Its agents stay stopped and its schedules stay off until you start them.', 'Reopen project', 'reopen', false, () => this.reopen());
    this.deleteRow = row('project-delete', 'Delete project', 'Delete this archived project for good, with every room, message, attachment, agent, memory and setting in it. Files in the project folder and worktrees on disk are not touched.', 'Delete project', 'trash', true, () => this.remove());
    this.lifecycleNote = document.createElement('p'); this.lifecycleNote.className = 'settings-desc'; this.lifecycleNote.setAttribute('role', 'status');
    group.append(this.archiveRow.holder, this.reopenRow.holder, this.deleteRow.holder);
    section.append(heading, group, this.lifecycleNote);
    this.body.querySelector('#project-settings-error').before(section);
  }
  // Shows the rows that apply to this project's state, for the people who may use them.
  showLifecycle(project, allowed) {
    const archived = !!project?.archived_at;
    this.archiveRow.holder.hidden = !project || archived;
    this.reopenRow.holder.hidden = !project || !archived;
    this.deleteRow.holder.hidden = !project || !archived;
    for (const { button } of [this.archiveRow, this.reopenRow, this.deleteRow]) { button.disabled = !allowed; }
    this.lifecycle.hidden = !project;
    // Starting agents is for the project's owners and operators; an archived project is read-only.
    this.sessionImport.hidden = !project || archived;
    this.sessionImportButton.disabled = !project || archived;
    this.lifecycleNote.textContent = !project ? '' : !allowed ? 'Only the project owner or an admin can archive, reopen or delete a project.' : archived ? `Archived ${new Date(project.archived_at).toLocaleString()}. It is read-only.` : '';
  }
  async lifecycleCall(button, path, options) {
    const id = this.projectID; this.error.textContent = ''; button.disabled = true;
    try { return { data: await request(`/api/projects/${encodeURIComponent(id)}${path}`, options) }; }
    catch (error) { if (this.projectID === id) this.error.textContent = error.message; return null; }
    finally { button.disabled = false; }
  }
  currentProject() { return this.projectContext().projects.find(item => item.id === this.projectID); }
  async archive() {
    const project = this.currentProject(); if (!project) return;
    if (!await confirmDialog(`Archive “${project.name}”? Every agent in it will be stopped, its schedules switched off and its rooms made read-only. You can reopen it later.`, { title: 'Archive project', confirm: 'Archive project', danger: true })) return;
    const done = await this.lifecycleCall(this.archiveRow.button, '/archive', { method: 'POST' });
    if (!done) return;
    await this.onLifecycle?.(project.id);
    // An agent whose execution is unknown cannot be stopped until its recovery is decided in the Inbox; say so beside the result.
    const failed = done.data?.stop_failed || [];
    if (failed.length && this.projectID === project.id) this.lifecycleNote.textContent = `Archived. ${failed.length} agent${failed.length === 1 ? '' : 's'} could not be stopped: ${failed.map(item => item.error).join('; ')}. Choose Abandon on its recovery item in the Inbox: that stops it. The project cannot be deleted before every agent has stopped.`;
  }
  async reopen() {
    const project = this.currentProject(); if (!project) return;
    if (await this.lifecycleCall(this.reopenRow.button, '/reopen', { method: 'POST' })) await this.onLifecycle?.(project.id);
  }
  async remove() {
    const project = this.currentProject(); if (!project) return;
    const answer = await formDialog({ title: 'Delete project', danger: true, confirm: 'Delete project',
      message: `This permanently deletes “${project.name}”: every room, message, reaction, attachment, agent, usage record, memory, schedule, task note, Inbox item and member. It cannot be undone. Files in the project folder and worktrees on disk are not touched.`,
      fields: [{ name: 'name', label: `Type “${project.name}” to confirm`, required: true, requiredMessage: 'Type the project name to confirm.', validate: value => value === project.name ? '' : 'That is not the project name.' }] });
    if (!answer) return;
    const done = await this.lifecycleCall(this.deleteRow.button, '', { method: 'DELETE', body: { name: answer.name } });
    if (done) await this.onRemoved?.(project.id);
  }
  // Show the settings of a project inside parent. A later mount wins; release hands the body back to the mount before it.
  mount(parent, projectID) {
    this.mounts = this.mounts.filter(item => item.parent !== parent);
    this.mounts.push({ parent, projectID });
    if (this.body.parentElement !== parent) parent.append(this.body);
    this.load(projectID);
  }
  release(parent) {
    const top = this.mounts.at(-1);
    this.mounts = this.mounts.filter(item => item.parent !== parent);
    const previous = this.mounts.at(-1);
    if (top?.parent === parent && previous) { previous.parent.append(this.body); this.load(previous.projectID); }
  }
  isMountedIn(parent) { return this.body.parentElement === parent; }
  // The project's name and folder. Its owner or an admin renames it; moving the folder is the admins' alone (the server decides; the form
  // says so up front). Whether this account is an admin is read from the rights, which opening Settings has just read again.
  async isAdmin() { return rights.admin; }
  async showDetails(id) {
    const admin = await this.isAdmin();
    if (this.projectID !== id) return;
    const project = this.projectContext().projects.find(item => item.id === id);
    const canName = !!project && (project.role === 'owner' || admin), canFolder = !!project && admin;
    const form = this.details;
    form.elements.name.value = project?.name || ''; form.elements.folder.value = project?.folder || '';
    this.detailsLoaded = { name: form.elements.name.value, folder: form.elements.folder.value };
    form.elements.name.disabled = !canName; form.elements.folder.disabled = !canFolder;
    form.querySelector('#project-details-save').disabled = !canName && !canFolder || !!project?.archived_at;
    this.showLifecycle(project, canName);
    form.querySelector('#project-details-status').textContent = !project || canFolder ? '' : canName ? 'Only an admin can change the folder.' : 'Only the project owner can change these.';
  }
  async saveDetails() {
    const id = this.projectID, form = this.details, status = form.querySelector('#project-details-status'), save = form.querySelector('#project-details-save');
    // Only what changed goes to the server: a project from before folders were checked can still be renamed.
    const body = {};
    if (!form.elements.name.disabled && form.elements.name.value !== this.detailsLoaded?.name) body.name = form.elements.name.value;
    if (!form.elements.folder.disabled && form.elements.folder.value !== this.detailsLoaded?.folder) body.folder = form.elements.folder.value;
    status.textContent = '';
    if (!Object.keys(body).length) { status.textContent = 'Nothing changed.'; return; }
    save.disabled = true;
    try {
      const project = await this.source.edit(id, body);
      if (this.projectID !== id) return;
      form.elements.name.value = project.name; form.elements.folder.value = project.folder;
      this.detailsLoaded = { name: project.name, folder: project.folder };
      this.onRenamed?.(id, project.name);
      status.textContent = 'Saved.';
    } catch (error) { if (this.projectID === id) status.textContent = error.message; }
    finally { if (this.projectID === id) save.disabled = false; }
  }
  async load(id) {
    this.projectID = id || '';
    this.showDetails(id);
    this.scopeControl.load(id);
    this.usageLimit.load(id);
    this.loadRoleDefaults(id);
    this.usageSummary.load(id);
    this.error.textContent = '';
    this.autoResume.checked = false;
    this.autoResume.disabled = true; this.worktrees.checked = false; this.worktrees.disabled = true;
    this.homeLeadOnly.checked = false; this.homeLeadOnly.disabled = true;
    if (!id) { this.error.textContent = 'Choose a project first.'; return; }
    try {
      const settings = await this.source.get(id);
      if (this.projectID !== id) return;
      this.autoResume.checked = !!settings.auto_resume_after_clean_diagnostic;
      this.autoResume.disabled = false;
      this.worktrees.checked = settings.worktree_mode !== 'off'; this.worktrees.disabled = false;
      this.homeLeadOnly.checked = settings.home_lead_only !== false; this.homeLeadOnly.disabled = false;
    } catch (error) { if (this.projectID === id) this.error.textContent = error.message; }
  }
  async loadRoleDefaults(id) {
    this.roleDefaultsRows.replaceChildren();
    if (!id) return;
    try {
      const roles = await request(`/api/projects/${encodeURIComponent(id)}/role-spawn-defaults`);
      if (this.projectID !== id) return;
      for (const role of roles) {
        const card = document.createElement('section'); card.className = 'settings-card project-role-default';
        const title = document.createElement('h4'); title.textContent = role.role_key;
        const source = document.createElement('select'); source.setAttribute('aria-label', `${role.role_key} default source`);
        const sourceRow = document.createElement('div'); sourceRow.className = 'settings-row';
        const sourceGroup = document.createElement('div'); sourceGroup.className = 'settings-group';
        const sourceInfo = document.createElement('div'); sourceInfo.className = 'settings-row-info';
        const sourceLabel = document.createElement('label'); sourceLabel.className = 'settings-label';
        source.id = `project-role-default-${role.role_key}`; sourceLabel.htmlFor = source.id; sourceLabel.textContent = 'Default source';
        const sourceDesc = document.createElement('span'); sourceDesc.className = 'settings-desc';
        sourceDesc.id = `${source.id}-desc`; sourceDesc.textContent = 'Use global choices or set project choices.';
        source.setAttribute('aria-describedby', sourceDesc.id);
        sourceInfo.append(sourceLabel, sourceDesc); sourceRow.append(sourceInfo, source); sourceGroup.append(sourceRow);
        for (const [value, label] of [['inherit', `Inherit global (${role.global_agents.length} agents)`], ['custom', 'Custom']]) {
          const option = document.createElement('option'); option.value = value; option.textContent = label; source.append(option);
        }
        source.value = role.inherited ? 'inherit' : 'custom';
        const editorHost = document.createElement('div'); const editor = new SpawnDefaultsEditor(editorHost);
        editor.set(role.default_agents, role.pick_mode); editor.loadOptions(id).catch(error => { status.textContent = error.message; });
        const sync = () => { editorHost.hidden = source.value === 'inherit'; };
        source.onchange = sync; sync();
        const save = document.createElement('button'); save.type = 'button'; save.className = 'btn btn-primary'; save.textContent = 'Save';
        const status = document.createElement('p'); status.className = 'settings-desc'; status.setAttribute('role', 'status');
        save.onclick = async () => {
          status.textContent = ''; save.disabled = true;
          try {
            const saved = await request(`/api/projects/${encodeURIComponent(id)}/role-spawn-defaults/${encodeURIComponent(role.role_key)}`,
              { method: 'PUT', body: { inherit: source.value === 'inherit', ...editor.get() } });
            if (this.projectID === id) { status.textContent = 'Saved.'; source.value = saved.inherited ? 'inherit' : 'custom'; sync(); }
          } catch (error) { status.textContent = error.message; }
          finally { save.disabled = false; }
        };
        card.append(title, sourceGroup, editorHost, save, status); this.roleDefaultsRows.append(card);
      }
    } catch (error) { if (this.projectID === id) this.roleDefaultsRows.textContent = error.message; }
  }
  async saveProject() {
    const id = this.projectID, enabled = this.autoResume.checked;
    this.error.textContent = '';
    this.autoResume.disabled = true;
    try {
      await this.source.update(id, { auto_resume_after_clean_diagnostic: enabled });
      if (this.projectID === id) this.autoResume.disabled = false;
    } catch (error) {
      if (this.projectID === id) { this.autoResume.checked = !enabled; this.autoResume.disabled = false; this.error.textContent = error.message; }
    }
  }
  async saveToggle(box, field, value) {
    const id = this.projectID;
    this.error.textContent = '';
    box.disabled = true;
    try {
      await this.source.update(id, { [field]: value });
      if (this.projectID === id) box.disabled = false;
    } catch (error) {
      if (this.projectID === id) { box.checked = !value; box.disabled = false; this.error.textContent = error.message; }
    }
  }
  async saveWorktrees() {
    const id = this.projectID, enabled = this.worktrees.checked;
    this.error.textContent = '';
    this.worktrees.disabled = true;
    try {
      await this.source.update(id, { worktree_mode: enabled ? 'auto' : 'off' });
      if (this.projectID === id) this.worktrees.disabled = false;
    } catch (error) {
      if (this.projectID === id) { this.worktrees.checked = !enabled; this.worktrees.disabled = false; this.error.textContent = error.message; }
    }
  }
}
