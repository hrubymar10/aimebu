import { html } from '../../ui.js';
import { Appearance } from './appearance.js';
import { ChatPreferences } from './chat-preferences.js';
import { MessageRetention } from './message-retention.js';
import { ErrorReports } from './error-reports.js';
import { SettingsBackup } from './settings-backup.js';

// The sections of Settings -> General, in the order they have always had.
export function GeneralPage({ store, onDirty }) {
  return html`
    <div class="settings-subsection" id="settings-appearance"><${Appearance} appearance=${store} /></div>
    <div class="settings-subsection" id="chat-preferences"><${ChatPreferences} store=${store} /></div>
    <div class="settings-subsection" id="message-retention"><${MessageRetention} onDirty=${onDirty} /></div>
    <div class="settings-subsection" id="error-reports-section"><${ErrorReports} /></div>
    <${SettingsBackup} store=${store} />
  `;
}
