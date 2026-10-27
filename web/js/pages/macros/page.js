import { mountPreactPage } from '../mount-page.js';
import { MacrosPage } from './macros-page.js';

// The settings shell's handle on the page: a tab and a section, with Preact rendering only inside the section.
export class MacroSettings {
  constructor(dialog) {
    this.tab = document.createElement('button');
    this.tab.type = 'button';
    this.tab.className = 'row-btn';
    this.tab.textContent = 'Macros';
    this.tab.id = 'settings-macros-tab';
    dialog.querySelector('nav').append(this.tab);
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
