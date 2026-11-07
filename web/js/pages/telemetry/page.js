import { mountPreactPage } from '../mount-page.js';
import { TelemetryPage } from './telemetry-page.js';

// Settings -> Telemetry: the owner's and the admins' choice of whether errors and performance traces are sent, and where. Drawn when the section
// is selected and taken out of the page when Settings closes (unless it holds an unsaved change), like the other sections.
export class TelemetrySettings {
  constructor(dialog) {
    this.section = document.createElement('section');
    this.section.className = 'settings-section';
    this.section.id = 'settings-telemetry';
    this.section.hidden = true;
    (dialog.querySelector('.settings-body') || dialog).append(this.section);
    this.page = mountPreactPage(this.section, TelemetryPage);
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
