import { rights, refreshRights } from './api.js';
import { confirmDialog } from './dialogs.js';
import { GeneralSettings } from './pages/general/page.js';
import { NotificationSettings } from './pages/notifications/page.js';
import { UsagesSettings } from './pages/usages/page.js';
import { createProjectSettingsSource } from './project-settings-source.js';
import { RoleSettings } from './pages/roles/page.js';
import { ProjectSettings } from './pages/project/page.js';
import { MachinesSettings } from './pages/machines/page.js';
import { HarnessSettings } from './pages/harnesses/page.js';
import { MacroSettings } from './pages/macros/page.js';
import { UsersSettings } from './pages/users/page.js';
import { SettingsShell } from './pages/shell/page.js';
import { PromptSettings } from './pages/prompts/page.js';
import { TelemetrySettings } from './pages/telemetry/page.js';
import { HttpQuotaSource } from './quota-source.js';
import { HttpLimitGuardSource } from './limit-guard-source.js';

export class SettingsPanel {
  constructor(source, projectContext, notifications) {
    this.source = source;
    this.projectSource = createProjectSettingsSource();
    // The project's settings form lives in the home room's settings (Room settings → Project); this panel only owns the object.
    this.project = new ProjectSettings(this.projectSource, projectContext);
    this.projectContext = projectContext;
    this.roleSettings = new RoleSettings();
    this.machinesSettings = new MachinesSettings(document.querySelector('#settings-machines'), document.querySelector('#machine-pair-dialog'));
    this.notifications = notifications;
    this.dialog = document.querySelector('#settings-dialog');
    this.usersSettings = new UsersSettings(this.dialog, projectContext);
    this.promptSettings = new PromptSettings(this.dialog, projectContext);
    this.telemetrySettings = new TelemetrySettings(this.dialog);
    this.usagesSection = document.querySelector('#settings-usages');
    this.usages = new UsagesSettings({ source, quotaSource: new HttpQuotaSource(), guardSource: new HttpLimitGuardSource(), dialog: this.dialog, section: this.usagesSection });
    this.generalSection = document.querySelector('#settings-general');
    this.general = new GeneralSettings(this.generalSection);
    this.notificationsSection = document.querySelector('#settings-notifications');
    this.rolesSection = document.querySelector('#settings-roles');
    this.machinesSection = document.querySelector('#settings-machines');
    this.macroSettings = new MacroSettings(this.dialog);
    this.harnessSettings = new HarnessSettings(this.dialog);
    this.notificationSettings = new NotificationSettings(this.notificationsSection, notifications);
    this.shell = new SettingsShell(this.dialog, {
      general: this.generalSection, usages: this.usagesSection, notifications: this.notificationsSection, roles: this.rolesSection, users: this.usersSettings.section,
      prompts: this.promptSettings.section, macros: this.macroSettings.section, telemetry: this.telemetrySettings.section, machines: this.machinesSection, harnesses: this.harnessSettings.section,
    }, { users: this.usersSettings.store.tab, onSelect: section => this.selectSection(section), onClose: () => this.requestClose() });
    // Each section's page is drawn when the section is selected and taken out of the page when the dialog closes (a page holding unsaved edits keeps
    // them): the sections are about a thousand nodes together, which stay in the page for nothing otherwise.
    this.pages = { general: this.general.page, usages: this.usages.page, notifications: this.notificationSettings.page, roles: this.roleSettings.page, users: this.usersSettings.page,
      prompts: this.promptSettings.page, macros: this.macroSettings.page, telemetry: this.telemetrySettings.page, machines: this.machinesSettings.page, harnesses: this.harnessSettings.page };
    this.dialog.addEventListener('close', () => this.release());
    this.dialog.addEventListener('cancel', event => { event.preventDefault(); void this.requestClose(); });
    this.dialog.addEventListener('click', event => { if (event.target === this.dialog) void this.requestClose(); });
    this.release();
    document.querySelector('#settings-toggle').onclick = () => this.open();
    // v1's global shortcuts: Cmd/Ctrl+, opens Settings, and Cmd/Ctrl+W closes it while it is open.
    document.addEventListener('keydown', event => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey || event.isComposing) return;
      if (event.key === ',' && this.userID) { event.preventDefault(); this.open(); }
      else if (event.key.toLowerCase() === 'w' && this.dialog.open) { event.preventDefault(); void this.requestClose(); }
    });
  }
  // What the account may do instance-wide. The owner and the admins (who are trusted like the owner) edit the roles, prompts and macros and see the machines and
  // their harnesses (what a machine runs, and pairing, approving and revoking machines, stay the owner's). For anyone else those tabs are gone instead of opening onto a 403.
  setRights({ owner, admin }) {
    this.general.setRights({ owner, admin });
    const before = this.shell.store.section.value;
    this.shell.store.setRights({ owner, admin });
    if (this.shell.store.section.value !== before) this.selectSection(this.shell.store.section.value); // the section's tab went: General opens in its place
    // An admin reads the provider usages (the limits, with no account names); the account switcher and every change are the owner's alone.
    this.usages.setOwner(owner);
    document.querySelector('#usages-toggle').hidden = !(owner || admin);
  }
  setUser(userID) {
    this.userID = userID;
    this.shell.store.setUser();
    document.querySelector('#usages-toggle').hidden = false;
    this.general.setUser(userID);
  }
  // Settings opens on General; the usages column's own Settings button asks for the Usages section.
  async open(section = '', options = {}) {
    const wanted = section; // a phone shows the list of sections unless a section was asked for
    section = section || 'general';
    if (!this.dialog.open) this.dialog.showModal();
    if (options.machineID) this.harnessSettings.wanted = options.machineID; // Harnesses opens on this machine
    this.selectSection(section); // the dialog shows at once; what the account may do is read again right after
    if (!wanted) this.shell.store.showList();
    this.usages.setError(''); // before the wait: whatever the person does in the meantime may write here, and must not be wiped
    try { await refreshRights(); this.setRights(rights); this.onRights?.(rights); } catch { /* keep what sign-in said */ }
    this.usersSettings.prepare().catch(() => this.usersSettings.settle(true));
    // The default daily limits do not depend on the switcher or the usage snapshots, so they load on their own.
    void this.usages.syncDefault();
    if (!rights.owner && !rights.admin) { if (!this.usagesSection.hidden) this.selectSection('general'); return; } // nothing of the usages is theirs to load
    try { await this.usages.load(); }
    catch (error) {
      if (error.status === 403) {
        this.shell.store.hideUsages();
        document.querySelector('#usages-toggle').hidden = true;
        // Fall back to General only while the reader is still looking at the Usages section this open() showed;
        // a tab they picked while the request was in flight must not be taken away.
        if (!this.usagesSection.hidden) this.selectSection('general');
      } else this.usages.setError(error.message || 'Could not load usage snapshots');
    }
  }
  usagesChanged() { return this.usages.changed(); }
  async requestClose() {
    if (!this.dialog.open || this.closePending) return;
    if (Object.values(this.pages).some(page => page.hasUnsavedChanges())) {
      this.closePending = true;
      try {
        const discard = await confirmDialog('Discard unsaved changes?', { title: 'Discard unsaved changes?', confirm: 'Discard', cancel: 'Keep editing', danger: true });
        if (!discard || !this.dialog.open) return;
        this.discarding = true;
      } finally {
        this.closePending = false;
      }
    }
    this.dialog.close();
  }
  // The session ended: every page is cleared, edits not yet saved included (release() keeps those for the person who made them; the next person must not
  // find them).
  resetSession() {
    this.usages.setOwner(false); // and with it everything the usages page held
    this.usages.forget();
    for (const [section, page] of Object.entries(this.pages)) {
      if (section === 'harnesses') this.harnessSettings.clear(); else page.clear();
    }
  }
  release() {
    for (const [section, page] of Object.entries(this.pages)) {
      if (!this.discarding && page.hasUnsavedChanges()) continue;
      if (section === 'harnesses') this.harnessSettings.clear(); else page.clear();
    }
    this.discarding = false;
  }
  selectSection(section) {
    this.shell.store.select(section);
    section = this.shell.store.section.value; // a section the account has no tab for is General
    this.pages[section]?.show();
    if (section === 'general') this.general.open();
    if (section === 'users') this.usersSettings.open();
    if (section === 'notifications') this.notificationSettings.open();
    if (section === 'roles') this.roleSettings.open();
    if (section === 'machines') this.machinesSettings.open();
    if (section === 'harnesses') this.harnessSettings.open();
    if (section === 'macros') this.macroSettings.open();
    if (section === 'prompts') this.promptSettings.open();
    if (section === 'telemetry') this.telemetrySettings.open();
  }
}
