import { mountPreactPage } from '../mount-page.js';
import { PromptsPage } from './prompts-page.js';

export class PromptSettings {
  constructor(dialog, projectContext) {
    this.section = document.createElement('section');
    this.section.className = 'settings-section';
    this.section.id = 'settings-prompts';
    this.section.hidden = true;
    (dialog.querySelector('.settings-body') || dialog).append(this.section);
    this.page = mountPreactPage(this.section, PromptsPage);
    this.page.update({ projectContext });
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
