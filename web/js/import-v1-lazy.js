import { request } from './api.js';
import { reportChunkFailure } from './lazy.js';

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
    }, error => { this.loading = null; reportChunkFailure(error, 'import-v1'); throw error; });
    return this.loading;
  }
  async maybeOpen(signal, isCurrent = () => true) {
    if (!this.wizard) {
      let status;
      try { status = await request('/api/import-v1/status', { signal }); }
      catch (error) {
        if (error.status === 403) return false;
        if (error.status !== 409 || error.code !== 'backup_changed') throw error; // a backup changed after it was made is shown by the wizard
      }
      if (signal?.aborted || !isCurrent()) return false;
      if (status && status.state !== 'ready' && status.state !== 'resume') return false;
    }
    return (await this.load()).maybeOpen(signal, isCurrent);
  }
}
