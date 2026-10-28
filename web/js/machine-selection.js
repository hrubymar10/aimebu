import { request } from './api.js';

// Shared machine and per-project folder choice for Spawn and Switch.
export class MachineSelection {
  constructor(prefix, onChange, onFolderEdit) {
    this.prefix = prefix;
    this.onChange = onChange;
    this.onFolderEdit = onFolderEdit;
    this.select = document.querySelector(`#${prefix}-machine`);
    this.wrapper = document.querySelector(`#${prefix}-machine-folder`);
    this.input = this.wrapper.querySelector('input[name=machine_folder]');
    this.status = document.querySelector(`#${prefix}-folder-status`);
    this.select.onchange = () => { this.renderFolder(); this.onChange(); };
    this.input.oninput = () => { this.status.textContent = 'Save this folder to use it.'; this.onFolderEdit?.(); };
    document.querySelector(`#${prefix}-save-folder`).onclick = () => this.saveFolder();
  }
  get id() { return this.select.value; }
  get machine() { return this.machines?.find(machine => machine.id === this.id); }
  get folderReady() { return !!this.machine?.is_host || !!this.folders?.get(this.id) && this.input.value.trim() === this.folders.get(this.id); }
  async load(projectID, preferredID = '') {
    this.projectID = projectID;
    // The dialog can be closed and opened again while this read is out; only the newest load may fill the list.
    const load = this.loadID = (this.loadID || 0) + 1;
    this.select.replaceChildren(); this.select.disabled = true;
    this.status.textContent = 'Loading machines…';
    const folders = await request(`/api/projects/${encodeURIComponent(projectID)}/machine-folders`);
    if (load !== this.loadID) return;
    this.folders = new Map((folders.folders || []).map(item => [item.machine_id, item.folder]));
    const machines = (await request(`/api/machines?project_id=${encodeURIComponent(projectID)}`)).machines || [];
    if (load !== this.loadID) return;
    this.machines = machines;
    for (const machine of machines) {
      const option = document.createElement('option'); option.value = machine.id;
      option.textContent = `${machine.name}${machine.is_host ? ' · Host' : ''}${machine.revoked_at ? ' · Revoked' : !machine.approved ? ' · Pending approval' : !machine.connected ? ' · Offline' : !machine.project_allowed ? ' · Project approval needed' : ''}`;
      option.disabled = !!machine.revoked_at || !machine.approved;
      this.select.append(option);
    }
    const preferred = machines.find(machine => machine.id === preferredID && machine.approved && !machine.revoked_at);
    const host = machines.find(machine => machine.is_host && machine.approved && !machine.revoked_at);
    this.select.value = (preferred || host || machines.find(machine => machine.approved && !machine.revoked_at))?.id || '';
    this.select.disabled = !this.select.value;
    this.renderFolder();
    await this.onChange();
  }
  renderFolder() {
    const machine = this.machine;
    this.wrapper.hidden = !machine || !!machine.is_host;
    this.status.textContent = '';
    this.input.value = machine ? this.folders?.get(machine.id) || '' : '';
    if (!this.wrapper.hidden && !this.input.value) this.status.textContent = 'Map this project folder before spawning or switching here.';
    else if (!this.wrapper.hidden && !machine.project_allowed) this.status.textContent = 'The project owner will approve the first agent on this machine in Inbox.';
  }
  async saveFolder() {
    if (!this.machine || this.machine.is_host) return;
    const folder = this.input.value.trim();
    if (!folder.startsWith('/') || folder.includes('/../') || folder.endsWith('/..') || folder.includes('/./') || folder.endsWith('/.')) {
      this.status.textContent = 'Enter a clean absolute folder path.'; return;
    }
    const button = document.querySelector(`#${this.prefix}-save-folder`); button.disabled = true;
    try {
      const saved = await request(`/api/projects/${encodeURIComponent(this.projectID)}/machine-folders/${encodeURIComponent(this.id)}`, { method: 'PUT', body: { folder } });
      this.folders.set(saved.machine_id, saved.folder);
      this.status.textContent = 'Folder saved.';
      await this.onChange();
    } catch (error) { this.status.textContent = error.message; }
    finally { button.disabled = false; }
  }
}
