import { batch, signal } from '../../ui.js';
import { request, rights } from '../../api.js';

// The machine a dialog starts or moves an agent on, and the project's folder on it. Every load and every pick of another machine takes a
// ticket for the saves, so the answer of a save that was left says nothing and does not hold the button.
export class MachineStore {
  // mayAdmit says whether the signed-in person may admit a machine for this project (its owner, or an admin); the server decides again.
  constructor({ onChange, onFolderEdit, mayAdmit = () => rights.admin }) {
    this.onChange = onChange; this.onFolderEdit = onFolderEdit; this.mayAdmit = mayAdmit;
    this.machines = signal([]);
    this.id = signal('');
    this.disabled = signal(false); // as the static select was, until a load starts
    this.folder = signal('');
    this.status = signal('');
    this.saving = signal(false);
    this.folders = new Map();
    this.version = signal(0);
    this.projectID = ''; this.loadID = 0; this.saveID = 0;
  }

  // Read through the signals, so what is computed from them follows a pick, an edit or a saved folder.
  get machine() { return this.machines.value.find(machine => machine.id === this.id.value); }
  get ready() { this.version.value; const folder = this.folders.get(this.id.value); return !!this.machine?.is_host || (!!folder && this.folder.value.trim() === folder); }
  // A paired machine with this project's folder mapped that the owner has not admitted yet, for someone who may admit it.
  get canAdmit() {
    this.version.value;
    const machine = this.machine;
    return !!machine && !machine.is_host && machine.approved && !machine.revoked_at && !machine.project_allowed && !!this.folders.get(machine.id) && !!this.mayAdmit();
  }
  get folderShown() { const machine = this.machine; return !!machine && !machine.is_host; }

  // The dialog that was loading is gone: no read of the load goes on, and a save in flight neither speaks nor holds the button.
  cancel() { this.loadID++; this.endSave(); }

  // A save of the last opening, or of the machine that was on show, is over for the view that replaces it: it neither speaks nor holds the button.
  endSave() { this.saveID++; this.saving.value = false; }

  async load(projectID, preferredID = '') {
    this.projectID = projectID;
    // The dialog can be closed and opened again while this read is out; only the newest load may fill the list.
    const load = this.loadID = this.loadID + 1;
    batch(() => { this.endSave(); this.machines.value = []; this.id.value = ''; this.disabled.value = true; this.status.value = 'Loading machines…'; });
    const folders = await request(`/api/projects/${encodeURIComponent(projectID)}/machine-folders`);
    if (load !== this.loadID) return;
    this.folders = new Map((folders.folders || []).map(item => [item.machine_id, item.folder]));
    this.version.value++;
    const machines = (await request(`/api/machines?project_id=${encodeURIComponent(projectID)}`)).machines || [];
    if (load !== this.loadID) return;
    const usable = machine => machine.approved && !machine.revoked_at;
    const chosen = machines.find(machine => machine.id === preferredID && usable(machine)) || machines.find(machine => machine.is_host && usable(machine)) || machines.find(usable);
    batch(() => { this.machines.value = machines; this.id.value = chosen?.id || ''; this.disabled.value = !chosen; this.renderFolder(); });
    await this.onChange();
  }

  renderFolder() {
    const machine = this.machine, shown = this.folderShown, folder = machine ? this.folders.get(machine.id) || '' : '';
    batch(() => {
      this.folder.value = folder;
      this.status.value = shown && !folder ? 'Map this project folder before spawning or switching here.' : shown && !machine.project_allowed ? 'The project owner will approve the first agent on this machine in Inbox.' : '';
    });
  }

  // Admits the machine on show for the project without starting an agent to raise the approval, then reads the machines again so the note and the
  // machine's label follow. Only the newest save of this opening speaks, as for the folder.
  async admit() {
    if (!this.canAdmit) return;
    const save = this.saveID = this.saveID + 1, machineID = this.id.peek(), projectID = this.projectID, current = () => save === this.saveID && machineID === this.id.peek();
    this.saving.value = true;
    try {
      await request(`/api/projects/${encodeURIComponent(projectID)}/machine-folders/${encodeURIComponent(machineID)}/admit`, { method: 'POST', body: {} });
      if (!current()) return;
      await this.load(projectID, machineID);
      // The load starts a new view of the machines (it ends this save and writes the folder note for the machine as the server now reports it), so
      // what happened is said on top of that, while the same machine of the same project is still on show.
      if (this.projectID === projectID && this.id.peek() === machineID) this.status.value = 'Machine admitted for this project.';
    } catch (error) { if (current()) this.status.value = error.message; }
    finally { if (save === this.saveID) this.saving.value = false; }
  }

  // Another machine on show ends the save of the one it leaves.
  select(id) { batch(() => { this.endSave(); this.id.value = id; this.renderFolder(); }); }

  pick(id) { this.select(id); return this.onChange(); }

  edit(text) { batch(() => { this.folder.value = text; this.status.value = 'Save this folder to use it.'; }); this.onFolderEdit?.(); }

  async saveFolder() {
    if (!this.machine || this.machine.is_host) return;
    const folder = this.folder.peek().trim();
    if (!folder.startsWith('/') || folder.includes('/../') || folder.endsWith('/..') || folder.includes('/./') || folder.endsWith('/.')) { this.status.value = 'Enter a clean absolute folder path.'; return; }
    // Only the newest save of this opening for the machine on show speaks: an older one, or one of a closed opening or another machine, says nothing.
    const save = this.saveID = this.saveID + 1, machineID = this.id.peek(), current = () => save === this.saveID && machineID === this.id.peek();
    this.saving.value = true;
    try {
      const saved = await request(`/api/projects/${encodeURIComponent(this.projectID)}/machine-folders/${encodeURIComponent(machineID)}`, { method: 'PUT', body: { folder } });
      if (save === this.saveID) { this.folders.set(saved.machine_id, saved.folder); this.version.value++; }
      if (!current()) return;
      this.status.value = 'Folder saved.';
      await this.onChange();
    } catch (error) { if (current()) this.status.value = error.message; }
    finally { if (save === this.saveID) this.saving.value = false; }
  }
}
