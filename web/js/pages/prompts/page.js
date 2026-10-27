import { mountPreactPage } from '../mount-page.js';
import { PromptsPage } from './prompts-page.js';

// The settings shell's handle on the page: a tab and a section, with Preact rendering only inside the section.
export class PromptSettings {
  constructor(dialog, projectContext) {
    this.tab = document.createElement('button');
    this.tab.type = 'button';
    this.tab.className = 'row-btn';
    this.tab.textContent = 'Prompts';
    this.tab.id = 'settings-prompts-tab';
    dialog.querySelector('nav').append(this.tab);
    this.section = document.createElement('section');
    this.section.className = 'settings-content';
    this.section.id = 'settings-prompts';
    this.section.hidden = true;
    dialog.append(this.section);
    this.page = mountPreactPage(this.section, PromptsPage);
    this.page.update({ projectContext });
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
