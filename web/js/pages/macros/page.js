import { mountPreactPage } from '../mount-page.js';
import { MacrosPage } from './macros-page.js';

export class MacroSettings {
  constructor(dialog) {
    this.section = document.createElement('section');
    this.section.className = 'settings-content';
    this.section.id = 'settings-macros';
    this.section.hidden = true;
    dialog.append(this.section);
    this.page = mountPreactPage(this.section, MacrosPage);
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
