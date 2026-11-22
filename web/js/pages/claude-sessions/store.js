import { batch, signal } from '../../ui.js';
import { listingStatus } from '../../claude-sessions.js';

// The picker's state: what is listed, the line above the list and the error. Every read carries a number and says nothing once a newer one
// started, the dialog was closed or the store is gone.
export class SessionPickerStore {
  constructor(source, dialog) {
    this.source = source;
    this.dialog = dialog;
    this.machineName = signal('');
    this.sessions = signal([]);
    this.status = signal('');
    this.error = signal('');
    this.alive = true;
    this.generation = 0;
    this.launcherID = '';
  }

  fresh(generation) { return this.alive && generation === this.generation && this.dialog.open; }

  // open({ projectID, machineID, machineName, launcherID, onPick }): lists the project's sessions on that machine; onPick(session) is called with
  // the chosen one (a row of the server's list, plus machine_id and launcher_id). `clearInput` empties the id box, which the browser keeps.
  async open({ projectID, machineID, machineName, launcherID = '', onPick }, clearInput = () => {}) {
    if (!this.alive) return;
    this.projectID = projectID; this.machineID = machineID; this.launcherID = launcherID; this.onPick = onPick;
    clearInput();
    batch(() => { this.machineName.value = machineName || ''; this.error.value = ''; this.sessions.value = []; this.status.value = 'Looking for sessions…'; });
    this.dialog.showModal();
    const generation = ++this.generation;
    try {
      const result = await this.source.claudeSessions(projectID, { machineID, launcherID });
      if (!this.fresh(generation)) return;
      this.launcherID = result.launcher_id || '';
      this.show(result.sessions || [], 'There are no Claude Code sessions for this project\'s folder on this machine.', result);
    } catch (failure) {
      if (!this.fresh(generation)) return;
      batch(() => { this.status.value = ''; this.error.value = failure.message || 'Could not list the sessions.'; });
    }
  }

  // A session pasted by its id.
  async byID(text) {
    const id = String(text || '').trim();
    if (!id || !this.alive) return;
    const generation = ++this.generation;
    batch(() => { this.error.value = ''; this.status.value = 'Looking for that session…'; });
    try {
      const result = await this.source.claudeSessions(this.projectID, { machineID: this.machineID, launcherID: this.launcherID, sessionID: id });
      if (!this.fresh(generation)) return;
      this.launcherID = result.launcher_id || this.launcherID;
      this.show(result.sessions || [], 'No session with that id.');
    } catch (failure) {
      if (!this.fresh(generation)) return;
      batch(() => { this.sessions.value = []; this.status.value = ''; this.error.value = failure.message || 'Could not find that session.'; });
    }
  }

  // `result` carries what a folder listing could not show: transcripts it could not read, and a listing cut at its limits.
  show(sessions, empty, result = {}) {
    batch(() => { this.error.value = ''; this.status.value = listingStatus(sessions.length, empty, result); this.sessions.value = sessions; });
  }

  pick(session) {
    this.dialog.close();
    this.onPick?.({ ...session, machine_id: this.machineID, launcher_id: this.launcherID });
  }

  destroy() { this.alive = false; this.generation++; this.dialog.close(); }
}
