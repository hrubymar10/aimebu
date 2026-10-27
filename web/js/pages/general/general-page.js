import { html } from '../../ui.js';
import { Appearance } from './appearance.js';
import { ChatPreferences } from './chat-preferences.js';
import { MessageRetention } from './message-retention.js';
import { SettingsBackup } from './settings-backup.js';

// The sections of Settings -> General, in the order they have always had.
export function GeneralPage({ store }) {
  return html`
    <div class="settings-section" id="settings-appearance"><${Appearance} appearance=${store} /></div>
    <div class="settings-section" id="chat-preferences"><${ChatPreferences} store=${store} /></div>
    <div class="settings-section" id="message-retention"><${MessageRetention} /></div>
    <${SettingsBackup} store=${store} />
  `;
}
