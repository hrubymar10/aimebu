import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { SettingsHeader, SettingsNav } from './settings-shell.js';
import { ShellStore } from './store.js';

// `sections` maps a name to the element its page fills; only its visibility follows the selection.
export class SettingsShell {
  constructor(dialog, sections, { users, onSelect, onClose }) {
    this.store = new ShellStore({ users });
    this.header = mountPreactPage(dialog.querySelector('header'), () => html`<${SettingsHeader} store=${this.store} onClose=${onClose} />`);
    this.nav = mountPreactPage(dialog.querySelector('nav'), () => html`<${SettingsNav} store=${this.store} onSelect=${onSelect} />`);
    this.stop = effect(() => {
      const current = this.store.section.value;
      dialog.dataset.section = current; // the Harnesses page is wide: its layouts need the room (see theme-v1.css)
      for (const [key, element] of Object.entries(sections)) { element.hidden = key !== current; element.classList.toggle('active', key === current); }
    });
  }

  destroy() { this.stop(); this.header.destroy(); this.nav.destroy(); }
}
