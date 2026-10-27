import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { UsagesStore } from './store.js';
import { SwitcherControls } from './switcher-controls.js';
import { UsagesList } from './usages-list.js';

// Settings -> Usages: provider cards with their profiles, and the account switcher's controls. The section also holds the automatic-switching
// blocks, which other modules build into it between the two, so Preact owns two hosts of the section (`switcher` above those blocks and
// `list` below them) and the section's error line is kept by the store.
export class UsagesSettings {
  constructor({ source, dialog, section, switcher, list, error, afterLoad }) {
    this.store = new UsagesStore({ source, section, isOpen: () => dialog.open, isShown: () => dialog.open && !section.hidden, afterLoad });
    this.stopError = effect(() => { error.textContent = this.store.error.value; });
    this.pages = [mountPreactPage(switcher, () => html`<${SwitcherControls} store=${this.store} />`), mountPreactPage(list, () => html`<${UsagesList} store=${this.store} />`)];
  }

  load() { return this.store.load(); }
  changed() { return this.store.changed(); }
  setError(text) { this.store.setError(text); }

  destroy() {
    this.store.destroy();
    this.stopError();
    for (const page of this.pages) page.destroy();
  }
}
