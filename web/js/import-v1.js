// The v1 import wizard: the lazy chunk's entry (import-v1-lazy.js loads it only when an import is pending). The wizard itself is a Preact
// page under pages/import-v1/ (model.js, store.js, components.js), reached only from here, so it stays out of the main bundle.
//
// ImportV1Wizard is the adapter the app and test-ui hold. It owns the <dialog> element (its open state, data-phase, the cancel guard) and
// Preact owns the dialog's children. Its fields are the store's (setters write the store), its DOM fields (content, status, error,
// actions, skipZone, progress…) are the rendered elements, and render()/refresh() draw at once, so a caller reads the DOM straight after.
import { html } from './ui.js';
import { mountPreactPage } from './pages/mount-page.js';
import { ImportStore } from './pages/import-v1/store.js';
import { Wizard } from './pages/import-v1/components.js';
import { dmTitle } from './pages/import-v1/model.js';

const FIELDS = ['phase', 'preview', 'mapping', 'steps', 'stepID', 'jobID', 'report', 'digest', 'newProject', 'staleNote', 'restored', 'destinationMacros', 'changedMessage'];
const ELEMENTS = ['heading', 'progress', 'progressLabel', 'progressBar', 'status', 'error', 'content', 'totals', 'actions', 'skipZone'];

export class ImportV1Wizard {
  constructor(currentSession = () => () => true) {
    this.currentSession = currentSession;
    this.dialog = document.createElement('dialog'); this.dialog.className = 'import-v1-dialog';
    this.dialog.addEventListener('cancel', event => event.preventDefault());
    this.dialog.addEventListener('close', () => this.store.stopPolling());
    document.body.append(this.dialog);
    this.refs = Object.fromEntries(ELEMENTS.map(name => [name, { current: null }]));
    this.store = new ImportStore({
      draw: () => this.draw(),
      isOpen: () => this.dialog.open,
      showModal: () => { if (!this.dialog.open) this.dialog.showModal(); },
      close: () => this.dialog.close(),
      scrollToTop: () => { if (this.content) this.content.scrollTop = 0; },
      focusProblem: problem => this.focusProblem(problem),
    }, () => this.currentSession());
    this.page = mountPreactPage(this.dialog, () => html`<${Wizard} store=${this.store} actions=${this} refs=${this.refs} />`);
    this.draw();
  }

  // Synchronous: Preact renders the tree before this returns.
  draw() { this.dialog.dataset.phase = this.store.phase; this.page?.update({}); }
  render() { this.store.render(); }
  // The DOM is patched in place, so the scroll position of a long list and the focused control survive a redraw.
  refresh() { this.store.refresh(); }

  get stepIndex() { return this.store.stepIndex; }

  maybeOpen(signal, isCurrent) { return this.store.maybeOpen(signal, isCurrent); }
  showBackupChanged(message, signal, isCurrent) { return this.store.showBackupChanged(message, signal, isCurrent); }
  open(signal, isCurrent) { return this.store.open(signal, isCurrent); }
  restoreDraft() { return this.store.restoreDraft(); }
  save() { this.store.save(); }
  clearDraft() { this.store.clearDraft(); }
  startOver() { return this.store.startOver(); }
  go(id) { return this.store.go(id); }
  next() { return this.store.next(); }
  check() { return this.store.check(); }
  commit() { return this.store.commit(); }
  skip() { return this.store.skip(); }
  poll() { return this.store.poll(); }
  confirmSkip() { this.store.confirmSkip(); }
  removeProject(alias) { this.store.removeProject(alias); }
  problem_projects() { return this.store.problems('projects'); }
  problem_people() { return this.store.problems('people'); }
  problem_dms() { return this.store.problems('dms'); }
  dmTitle(room) { return dmTitle(room, this.store.mapping); }

  // Shows the field a problem is about: it unfolds what hides it, scrolls it into view and focuses it.
  focusProblem(problem) {
    const input = this.content?.querySelector(problem.selector);
    if (!input) return;
    input.closest('details')?.setAttribute('open', '');
    input.scrollIntoView({ block: 'center' }); input.focus();
  }
}

for (const name of FIELDS) {
  Object.defineProperty(ImportV1Wizard.prototype, name, {
    get() { return this.store[name]; },
    set(value) { if (name === 'phase' && value === 'import' && this.store.phase !== 'import') this.store.resetImport(); this.store[name] = value; },
  });
}
for (const name of ELEMENTS) Object.defineProperty(ImportV1Wizard.prototype, name, { get() { return this.refs[name].current; } });
