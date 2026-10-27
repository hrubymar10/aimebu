import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { MachinesPage } from './machines-page.js';
import { PairDialog } from './pair-dialog.js';
import { MachinesStore } from './store.js';

export class MachinesSettings {
  constructor(section, dialog) {
    this.store = new MachinesStore();
    this.page = mountPreactPage(section, () => html`<${MachinesPage} store=${this.store} />`);
    this.pairing = mountPreactPage(dialog, () => html`<${PairDialog} store=${this.store} dialog=${dialog} />`);
  }

  open() { return this.store.load(); }
  clear() {}
  hasUnsavedChanges() { return false; }
  destroy() { this.store.destroy(); this.page.destroy(); this.pairing.destroy(); }
}
