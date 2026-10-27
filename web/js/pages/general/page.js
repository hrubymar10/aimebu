import { html, signal } from '../../ui.js';
import { savePreference } from '../../preferences.js';
import { mountPreactPage } from '../mount-page.js';
import { Appearance } from './appearance.js';
import { knownTheme, themeAttribute, themeKey, previewKey } from './model.js';

// Settings -> General holds sections of its own modules too (retention, system events, backup append to it), so Preact owns only the
// Appearance block of it. The theme and the room-preview switch are the account's, kept in the browser per person and on the server.
export class AppearanceSettings {
  constructor(container = document.querySelector('#settings-appearance')) {
    this.container = container;
    this.userID = '';
    this.theme = signal(document.documentElement.dataset.theme || 'dark');
    this.preview = signal(false);
    this.page = mountPreactPage(container, () => html`<${Appearance} appearance=${this} />`);
  }

  apply(theme) {
    const value = themeAttribute(theme);
    if (value === null) delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = value;
  }

  // Reads what this person chose in this browser and applies the theme.
  setUser(userID) {
    this.userID = userID;
    let theme = 'dark', preview = false;
    try {
      theme = localStorage.getItem(themeKey(userID)) || theme;
      preview = localStorage.getItem(previewKey(userID)) === '1';
      localStorage.setItem('aimebu2.ui.last-user', userID);
    } catch { /* storage is off: the defaults stand */ }
    theme = knownTheme(theme);
    this.theme.value = theme;
    this.preview.value = preview;
    this.apply(theme);
  }

  chooseTheme(theme) {
    this.theme.value = theme;
    this.apply(theme);
    try { if (this.userID) localStorage.setItem(themeKey(this.userID), theme); } catch { /* the server copy is what counts */ }
    savePreference('theme', theme);
  }

  choosePreview(checked) {
    this.preview.value = checked;
    const value = checked ? '1' : '0';
    try { if (this.userID) localStorage.setItem(previewKey(this.userID), value); } catch { /* the server copy is what counts */ }
    savePreference('sidebar_preview', value);
    document.dispatchEvent(new CustomEvent('aimebu:sidebar-preview'));
  }

  destroy() { this.page.destroy(); }
}
