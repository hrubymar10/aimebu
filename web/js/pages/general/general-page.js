import { html } from '../../ui.js';
import { Appearance } from './appearance.js';
import { ChatPreferences } from './chat-preferences.js';
import { MessageRetention } from './message-retention.js';
import { AgentIsolation } from './agent-isolation.js';
import { PromptCache } from './prompt-cache.js';
import { SettingsBackup } from './settings-backup.js';

// The sections of Settings -> General, in the order they have always had.
export function GeneralPage({ store, onDirty }) {
  return html`
    <div class="settings-subsection" id="settings-appearance"><${Appearance} appearance=${store} /></div>
    <div class="settings-subsection" id="chat-preferences"><${ChatPreferences} store=${store} /></div>
    <div class="settings-subsection" id="message-retention"><${MessageRetention} onDirty=${onDirty} /></div>
    <div class="settings-subsection" id="agent-isolation"><${AgentIsolation} /></div>
    <div class="settings-subsection" id="prompt-cache"><${PromptCache} /></div>
    <${SettingsBackup} store=${store} />
  `;
}
