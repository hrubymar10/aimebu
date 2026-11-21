import { request } from './api.js';

// The v1 import wizard is a lazily fetched chunk (a large module that only an install with a v1 backup ever shows). This is what the app holds in its
// place: it asks the server whether an import is pending (the one request the wizard's own startup check makes) and loads the wizard only when it is,
// so an instance with nothing to import never fetches it.
export class LazyImportWizard {
  constructor(currentSession) {
    this.currentSession = currentSession;
    this.wizard = null;
    this.loading = null;
    this.closeListeners = [];
  }
  // Whether the wizard's dialog is on screen (it cannot be before it is loaded).
  get open() { return !!this.wizard?.dialog.open; }
  close() { if (this.open) this.wizard.dialog.close(); }
  onClose(listener) { this.closeListeners.push(listener); this.wizard?.dialog.addEventListener('close', listener); }
  load() {
    this.loading ||= import('./import-v1.js').then(module => {
      this.wizard = new module.ImportV1Wizard(this.currentSession);
      for (const listener of this.closeListeners) this.wizard.dialog.addEventListener('close', listener);
      return this.wizard;
    }, error => { this.loading = null; throw error; }); // a startup check, not the person's: the caller (app.js) records the failure quietly
    return this.loading;
  }
  async maybeOpen(signal, isCurrent = () => true) {
    if (this.wizard) return this.wizard.maybeOpen(signal, isCurrent);
    // One status request in all: what it answers is acted on here, and the wizard is told, not asked again (a second request could answer differently,
    // and a pending backup would go unshown).
    let status, changed = null;
    try { status = await request('/api/import-v1/status', { signal }); }
    catch (error) {
      if (error.status === 403) return false;
      if (error.status !== 409 || error.code !== 'backup_changed') throw error; // a backup changed after it was made is shown by the wizard
      changed = error;
    }
    if (signal?.aborted || !isCurrent()) return false;
    if (!changed && status.state !== 'ready' && status.state !== 'resume') return false;
    const wizard = await this.load();
    if (signal?.aborted || !isCurrent()) return false;
    return changed ? wizard.showBackupChanged(changed.message, signal, isCurrent) : wizard.open(signal, isCurrent);
  }
}
