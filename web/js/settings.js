import { ProjectSettings } from './pages/project/page.js';
import { createProjectSettingsSource } from './project-settings-source.js';
import { parsePath } from './pages/shell/registry.js';
import { captureError } from './telemetry.js';
import { knownTheme, themeAttribute, themeKey } from './pages/general/model.js';

// The signed-in person's theme is applied at sign-in, not when General is first opened (that page is in the lazy dialog and applies it again).
function applyStoredTheme(userID) {
  let theme = 'dark';
  try { theme = localStorage.getItem(themeKey(userID)) || theme; localStorage.setItem('aimebu2.ui.last-user', userID); } catch { /* storage is off: the default stands */ }
  const value = themeAttribute(knownTheme(theme));
  if (value === null) delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = value;
}

// Settings as the rest of the app sees it. What the app reaches for at once stays here (the project's settings object, which the home room's settings
// show, the usages source, the toggles and Cmd/Ctrl+,), and the dialog with its pages (SettingsDialog, about half of the page's code) is a chunk
// loaded the first time Settings is opened or its address is followed. What the app set before the chunk arrived (the person, their rights, the callbacks)
// is handed to the dialog when it is made.
export class SettingsPanel {
  constructor(source, projectContext, notifications) {
    this.source = source;
    this.projectSource = createProjectSettingsSource();
    // The project's settings form lives in the home room's settings (Room settings → Project); this panel only owns the object.
    this.project = new ProjectSettings(this.projectSource, projectContext);
    this.projectContext = projectContext;
    this.notifications = notifications;
    this.dialogModule = null;
    this.dialogReady = null;
    let onReady = null;
    // The harness settings page is part of the dialog: the app's callback waits here until the dialog exists.
    this.harnessSettings = {
      get onReady() { return onReady; },
      set onReady(callback) { onReady = callback; if (this.target) this.target.onReady = callback; },
      target: null,
    };
    document.querySelector('#settings-toggle').onclick = () => void this.open();
    // v1's global shortcut: Cmd/Ctrl+, opens Settings.
    document.addEventListener('keydown', event => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey || event.isComposing) return;
      if (event.key === ',' && this.userID) { event.preventDefault(); void this.open(); }
    });
  }
  // The dialog, made once (a failed fetch of the chunk is tried again by the next call).
  load() {
    this.dialogReady ||= import('./settings-dialog.js').then(module => {
      const dialog = new module.SettingsDialog(this);
      this.dialogModule = dialog;
      this.harnessSettings.target = dialog.harnessSettings;
      if (this.harnessSettings.onReady) dialog.harnessSettings.onReady = this.harnessSettings.onReady;
      if (this.rights) dialog.setRights(this.rights);
      if (this.userID) dialog.setUser(this.userID);
      return dialog;
    }, error => { this.dialogReady = null; throw error; });
    return this.dialogReady;
  }
  setRights(rights) {
    this.rights = { owner: rights.owner, admin: rights.admin };
    document.querySelector('#usages-toggle').hidden = !(rights.owner || rights.admin);
    this.dialogModule?.setRights(this.rights);
  }
  setUser(userID) {
    this.userID = userID;
    applyStoredTheme(userID);
    document.querySelector('#usages-toggle').hidden = false;
    if (this.dialogModule) this.dialogModule.setUser(userID);
    else if (parsePath(location.hash)) void this.load().catch(error => captureError(error, 'settings.load')); // opened by a link
  }
  async open(section = '', options = {}) {
    let dialog;
    try { dialog = await this.load(); } catch (error) { captureError(error, 'settings.load'); return; }
    return dialog.open(section, options);
  }
  usagesChanged() { return this.dialogModule?.usagesChanged(); }
  resetSession() { this.rights = null; this.dialogModule?.resetSession(); }
  followAddress(event) { return this.dialogModule?.followAddress(event); }
}
