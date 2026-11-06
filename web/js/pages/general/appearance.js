import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { THEMES } from './model.js';

export function Appearance({ appearance }) {
  return html`
    <h3>Appearance</h3>
    <${SettingsGroup} label="Appearance">
      <${SettingsRow} label="Theme" desc="Colour scheme for the web UI. Saved to your account, so it follows you to other devices." id="theme-select">
        <select class="settings-select" value=${appearance.theme.value} onChange=${event => appearance.chooseTheme(event.currentTarget.value)}>
          ${THEMES.map(([value, label]) => html`<option value=${value} key=${value}>${label}</option>`)}
        </select>
      <//>
      <${SettingsRow} label="Show the last message under every room" desc="A one-line preview of the newest message under each room in the room list. Off, only the room that is open shows it. Saved to your account." id="sidebar-preview" className="settings-row-toggle">
        <input type="checkbox" checked=${appearance.preview.value} onChange=${event => appearance.choosePreview(event.currentTarget.checked)} />
      <//>
    <//>
  `;
}
