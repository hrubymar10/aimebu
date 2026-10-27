import { mountPreactPage } from '../mount-page.js';
import { RolesPage } from './roles-page.js';

// The settings shell's handle on the page: the section is the shell's own (index.html), Preact renders only inside it.
export class RoleSettings {
  constructor(section = document.querySelector('#settings-roles')) {
    this.section = section;
    this.page = mountPreactPage(section, RolesPage);
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
