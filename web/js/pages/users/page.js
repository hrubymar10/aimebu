import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { UsersPage } from './users-page.js';
import { UsersStore } from './store.js';

export class UsersSettings {
  constructor(dialog, projectContext) {
    this.store = new UsersStore(projectContext);
    this.section = document.createElement('section');
    this.section.id = 'settings-users';
    this.section.className = 'settings-content';
    this.section.hidden = true;
    dialog.append(this.section);
    this.page = mountPreactPage(this.section, () => html`<${UsersPage} store=${this.store} />`);
  }

  async prepare() {
    const access = await this.store.prepare();
    if (access) this.settle(access.hidden);
    return access;
  }

  settle(hide) { this.store.tab.value = hide ? 'hidden' : 'shown'; }

  async open() { const access = await this.prepare(); if (access && !access.hidden) await this.store.load(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return false; }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
