import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { NotificationsPage } from './notifications-page.js';

// The settings shell's handle on the page: the section is the shell's own (index.html), Preact renders only inside it.
export class NotificationSettings {
  constructor(section, notifications) {
    this.section = section;
    this.page = mountPreactPage(section, props => html`<${NotificationsPage} ...${props} notifications=${notifications} />`);
  }

  open() { this.page.open(); }
  clear() { this.page.clear(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
