import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { chatPreferences, applyChatPreferences } from '../../chat-preferences.js';

export function ChatPreferences({ store }) {
  const chat = store.chat.value;
  const toggle = (id, label, desc, checked, set) => html`
    <${SettingsRow} label=${label} desc=${desc} id=${id} className="settings-row-toggle">
      <input type="checkbox" checked=${checked} onChange=${event => { set(event.currentTarget.checked); applyChatPreferences(); store.readChat(); }} />
    <//>`;
  return html`
    <h3>Chat</h3>
    <${SettingsGroup} label="Chat">
      ${toggle('pref-system-events', 'Show system events', 'Adds a bell to the top bar with a feed of joins, spawns, recoveries, quota limits, and machine changes.', chat.systemEvents, value => { chatPreferences.showSystemEvents = value; })}
      ${toggle('pref-debug-inspector', 'Message debug inspector', 'Adds a debug button to each message that shows its raw record, who it addressed, and what happened to each agent it woke.', chat.debugInspector, value => { chatPreferences.debugInspector = value; })}
    <//>
  `;
}
