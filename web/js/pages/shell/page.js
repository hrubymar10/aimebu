import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { SettingsHeader, SettingsNav, CategoryHead } from './settings-shell.js';
import { ShellStore } from './store.js';
import { categoryPath } from './registry.js';

// `sections` maps a page name to the element its page fills; only its visibility follows the selection. The category head is made here, between the top bar
// and the pages.
export class SettingsShell {
  constructor(dialog, sections, { users, onSelect, onClose, onResult = result => onSelect(categoryPath(result.category)), onBack = () => this.store.showList() }) {
    this.store = new ShellStore({ users });
    const body = dialog.querySelector('.settings-body');
    this.head = body.querySelector('.settings-cat-host') || Object.assign(document.createElement('div'), { className: 'settings-cat-host' });
    if (!this.head.parentNode) body.insertBefore(this.head, dialog.querySelector('header').nextSibling);
    this.header = mountPreactPage(dialog.querySelector('header'), () => html`<${SettingsHeader} store=${this.store} onClose=${onClose} onBack=${onBack} />`);
    this.nav = mountPreactPage(dialog.querySelector('nav'), () => html`<${SettingsNav} store=${this.store} onNavigate=${onSelect} onClose=${onClose} onResult=${onResult} />`);
    this.catHead = mountPreactPage(this.head, () => html`<${CategoryHead} store=${this.store} onNavigate=${onSelect} />`);
    this.stop = effect(() => {
      const current = this.store.section.value;
      dialog.dataset.section = current;
      dialog.dataset.category = this.store.category.value;
      dialog.dataset.view = this.store.view.value; // a phone shows the list of categories or one category (see v2-only.css)
      for (const [key, element] of Object.entries(sections)) {
        element.hidden = key !== current; element.classList.toggle('active', key === current);
        if (key === 'general') element.dataset.cat = this.store.category.value; // the General page's blocks are shared by several categories
      }
    });
  }

  destroy() { this.stop(); this.header.destroy(); this.nav.destroy(); this.catHead.destroy(); this.head.remove(); }
}
