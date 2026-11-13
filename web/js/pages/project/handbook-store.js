import { batch, signal } from '../../ui.js';
import { handbookSource } from '../../handbook-source.js';

// Each request belongs to the selected project. A response from an earlier
// project or revision cannot replace the document currently on screen.
export class HandbookStore {
  constructor(source = handbookSource) {
    this.source = source;
    this.projectID = '';
    this.document = signal(null);
    this.documentLoading = signal(false);
    this.documentError = signal('');
    this.historyOpen = signal(false);
    this.revisions = signal([]);
    this.nextBeforeRev = signal(null);
    this.historyLoading = signal(false);
    this.historyError = signal('');
    this.snapshot = signal(null);
    this.snapshotRev = signal(null);
    this.snapshotLoading = signal(false);
    this.snapshotError = signal('');
    this.generation = 0;
    this.historyRequest = 0;
    this.snapshotRequest = 0;
    this.alive = true;
  }

  reset() {
    this.generation++; this.historyRequest++; this.snapshotRequest++;
    this.projectID = '';
    batch(() => {
      this.document.value = null; this.documentLoading.value = false; this.documentError.value = '';
      this.historyOpen.value = false; this.revisions.value = []; this.nextBeforeRev.value = null;
      this.historyLoading.value = false; this.historyError.value = '';
      this.snapshot.value = null; this.snapshotRev.value = null; this.snapshotLoading.value = false; this.snapshotError.value = '';
    });
  }

  async load(projectID) {
    if (!this.alive) return;
    this.reset();
    if (!projectID) return;
    this.projectID = projectID;
    const generation = this.generation;
    this.documentLoading.value = true;
    try {
      const document = await this.source.get(projectID);
      if (this.alive && generation === this.generation) batch(() => { this.document.value = document; this.documentLoading.value = false; });
    } catch (error) {
      if (this.alive && generation === this.generation) batch(() => { this.documentError.value = error.message; this.documentLoading.value = false; });
    }
  }

  async showHistory() {
    if (!this.projectID || !this.alive) return;
    if (this.historyOpen.peek()) { this.historyOpen.value = false; return; }
    this.historyOpen.value = true;
    if (!this.revisions.peek().length && !this.historyLoading.peek()) await this.loadHistory(false);
  }

  async loadHistory(more = false) {
    const projectID = this.projectID, generation = this.generation;
    if (!this.alive || !projectID || this.historyLoading.peek() || more && this.nextBeforeRev.peek() == null) return;
    const request = ++this.historyRequest;
    const beforeRev = more ? this.nextBeforeRev.peek() : null;
    batch(() => { this.historyLoading.value = true; this.historyError.value = ''; });
    try {
      const result = await this.source.history(projectID, beforeRev);
      if (!this.alive || generation !== this.generation || request !== this.historyRequest) return;
      batch(() => {
        this.revisions.value = more ? [...this.revisions.peek(), ...(result.revisions || [])] : result.revisions || [];
        this.nextBeforeRev.value = result.next_before_rev ?? null;
        this.historyLoading.value = false;
      });
    } catch (error) {
      if (this.alive && generation === this.generation && request === this.historyRequest) batch(() => { this.historyError.value = error.message; this.historyLoading.value = false; });
    }
  }

  async showRevision(rev) {
    const projectID = this.projectID, generation = this.generation, request = ++this.snapshotRequest;
    if (!this.alive || !projectID) return;
    batch(() => { this.snapshotRev.value = rev; this.snapshot.value = null; this.snapshotLoading.value = true; this.snapshotError.value = ''; });
    try {
      const snapshot = await this.source.revision(projectID, rev);
      if (this.alive && generation === this.generation && request === this.snapshotRequest) batch(() => { this.snapshot.value = snapshot; this.snapshotLoading.value = false; });
    } catch (error) {
      if (this.alive && generation === this.generation && request === this.snapshotRequest) batch(() => { this.snapshotError.value = error.message; this.snapshotLoading.value = false; });
    }
  }

  showCurrent() {
    this.snapshotRequest++;
    batch(() => { this.snapshotRev.value = null; this.snapshot.value = null; this.snapshotLoading.value = false; this.snapshotError.value = ''; });
  }

  destroy() { this.alive = false; this.reset(); }
}
