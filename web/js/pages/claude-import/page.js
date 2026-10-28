import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { ImportDialog } from './import-dialog.js';
import { ImportStore } from './store.js';

export class ClaudeSessionImport {
  constructor(source) {
    const dialog = document.querySelector('#cc-import-dialog');
    this.store = new ImportStore(source, dialog);
    this.page = mountPreactPage(dialog, () => html`<${ImportDialog} store=${this.store} />`);
  }

  open(target) { return this.store.open(target); }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
