import { html, signal } from '../../ui.js';
import { savePreference } from '../../preferences.js';
import { retention } from '../../retention.js';
import { chatPreferences } from '../../chat-preferences.js';
import { mountPreactPage } from '../mount-page.js';
import { GeneralPage } from './general-page.js';
import { knownTheme, themeAttribute, themeKey, previewKey } from './model.js';

// Settings -> General: the theme and room-preview choices, the chat switches, the message-expiry window and the settings backup. The shell
// sets who is signed in and what they may do from outside the page, so this object keeps those as signals the components read. The theme and
// the preview switch are the account's: kept in the browser per person and on the server.
export class GeneralSettings {
  constructor(section = document.querySelector('#settings-general')) {
    this.section = section;
    this.userID = '';
    this.theme = signal(document.documentElement.dataset.theme || 'dark');
    this.preview = signal(false);
    this.chat = signal({ systemEvents: true, debugInspector: false });
    this.rights = signal({ owner: false, admin: false });
    this.page = mountPreactPage(section, () => html`<${GeneralPage} store=${this} />`);
  }

  apply(theme) {
    const value = themeAttribute(theme);
    if (value === null) delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = value;
  }

  // What the chat switches hold now, for the person this browser is signed in as.
  readChat() { this.chat.value = { systemEvents: chatPreferences.showSystemEvents, debugInspector: chatPreferences.debugInspector }; }

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
    this.readChat();
  }

  setRights({ owner, admin }) { this.rights.value = { owner: !!owner, admin: !!admin }; }

  // Each time General is shown: the window may have been changed elsewhere, and so may the chat switches.
  open() {
    this.readChat();
    retention.load();
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

  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
