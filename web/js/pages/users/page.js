import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { UsersPage } from './users-page.js';
import { UsersStore } from './store.js';

// The tab sits in the dialog's nav, outside the Preact tree; it holds its place invisibly until the access check answers, so the tabs below never move.
export class UsersSettings {
  constructor(dialog, projectContext) {
    this.store = new UsersStore(projectContext);
    this.tab = document.createElement('button');
    this.tab.type = 'button';
    this.tab.id = 'settings-users-tab';
    this.tab.className = 'row-btn settings-tab-pending';
    this.tab.textContent = 'Users';
    this.tab.setAttribute('aria-hidden', 'true');
    this.tab.tabIndex = -1;
    dialog.querySelector('nav').append(this.tab);
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

  settle(hide = this.tab.hidden) {
    this.tab.hidden = hide;
    this.tab.classList.remove('settings-tab-pending'); this.tab.removeAttribute('aria-hidden'); this.tab.removeAttribute('tabindex');
  }

  async open() { const access = await this.prepare(); if (access && !access.hidden) await this.store.load(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return false; }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
