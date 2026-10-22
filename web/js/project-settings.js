import { UsageSummary } from './usage-summary.js';
import { request, rights } from './api.js';
import { ScopeControl } from './scope-control.js';
import { UsageLimit } from './usage-limit.js';
import { SpawnDefaultsEditor } from './spawn-defaults-editor.js';

// The project's own settings (name and folder, its agents, daily limits, the home room's lead-only rule, recovery and worktrees). One implementation, two places: the
// Project section of the home room's settings and Settings → Project. Both dialogs are modal, so the one form body moves into whichever
// opened last and goes back to the other when that one closes.
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
    this.projectID = '';
    this.mounts = [];
    this.details.onsubmit = event => { event.preventDefault(); this.saveDetails(); };
    this.autoResume.onchange = () => this.saveProject();
    this.worktrees.onchange = () => this.saveWorktrees();
    this.homeLeadOnly.onchange = () => this.saveToggle(this.homeLeadOnly, 'home_lead_only', this.homeLeadOnly.checked);
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
    form.querySelector('#project-details-save').disabled = !canName && !canFolder;
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
