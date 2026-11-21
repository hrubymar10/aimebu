import { rights, refreshRights } from './api.js';
import { confirmDialog } from './dialogs.js';
import { GeneralSettings } from './pages/general/page.js';
import { NotificationSettings } from './pages/notifications/page.js';
import { UsagesSettings } from './pages/usages/page.js';
import { RoleSettings } from './pages/roles/page.js';
import { MachinesSettings } from './pages/machines/page.js';
import { HarnessSettings } from './pages/harnesses/page.js';
import { MacroSettings } from './pages/macros/page.js';
import { UsersSettings } from './pages/users/page.js';
import { SettingsShell } from './pages/shell/page.js';
import { SettingsAddress } from './pages/shell/address.js';
import { categoryById, categoryPath, parsePath } from './pages/shell/registry.js';
import { reveal } from './pages/shell/highlight.js';
import { PromptSettings } from './pages/prompts/page.js';
import { TelemetrySettings } from './pages/telemetry/page.js';
import { HttpQuotaSource } from './quota-source.js';
import { HttpLimitGuardSource } from './limit-guard-source.js';
import { captureError } from './telemetry.js';

// The Settings dialog and its pages. It is loaded the first time Settings is opened (see settings.js, which owns what the rest of the app reaches
// for before that: the project's settings object, the usages source and the shortcuts), so the pages are a lazily fetched chunk, not part of the page's first load.
export class SettingsDialog {
  constructor(owner) {
    const { source, projectContext, notifications } = owner;
    this.owner = owner;
    this.source = source;
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
    }, { users: this.usersSettings.store.tab, onSelect: path => this.navigate(path), onClose: () => this.requestClose(), onBack: () => this.back(), onResult: result => this.openResult(result) });
    // The address: each level has its own (#settings/<section>/<category>[/<sub>]), so Back, Forward and a copied link work (see shell/address.js).
    this.address = new SettingsAddress({
      dialog: this.dialog, parse: parsePath, pathOf: target => this.pathOf(target), resolve: path => this.shell.store.resolve(path), show: target => this.show(target),
      open: (path, options) => this.open(path, options), listPath: () => this.pathOf({ section: this.shell.store.route.value.section, category: '', sub: '' }),
      requestClose: () => this.requestClose(), currentRoom: () => this.owner.currentRoom?.() || '', ready: () => !!this.userID,
    });
    // Each section's page is drawn when the section is selected and taken out of the page when the dialog closes (a page holding unsaved edits keeps
    // them): the sections are about a thousand nodes together, which stay in the page for nothing otherwise.
    this.pages = { general: this.general.page, usages: this.usages.page, notifications: this.notificationSettings.page, roles: this.roleSettings.page, users: this.usersSettings.page,
      prompts: this.promptSettings.page, macros: this.macroSettings.page, telemetry: this.telemetrySettings.page, machines: this.machinesSettings.page, harnesses: this.harnessSettings.page };
    // Closing hands the address back at once (requestClose calls afterClose itself): the close event is a task later, and a Settings opened in between must not be
    // taken for the one that closed. The event stays for a close that did not come through requestClose.
    this.dialog.addEventListener('close', () => { if (!this.dialog.open) this.afterClose(); });
    this.dialog.addEventListener('cancel', event => { event.preventDefault(); void this.requestClose(); });
    this.dialog.addEventListener('click', event => { if (event.target === this.dialog) void this.requestClose(); });
    this.release();
    // Cmd/Ctrl+W closes Settings while it is open (v1's shortcut; Cmd/Ctrl+, to open it is the shim's, as the dialog may not be loaded yet).
    document.addEventListener('keydown', event => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey || event.isComposing) return;
      if (event.key.toLowerCase() === 'w' && this.dialog.open) { event.preventDefault(); void this.requestClose(); }
    });
  }
  // What the account may do instance-wide. The owner and the admins (who are trusted like the owner) edit the roles, prompts and macros and see the machines and
  // their harnesses (what a machine runs, and pairing, approving and revoking machines, stay the owner's). For anyone else those tabs are gone instead of opening onto a 403.
  setRights({ owner, admin }) {
    this.general.setRights({ owner, admin });
    const before = this.shell.store.category.value;
    this.shell.store.setRights({ owner, admin });
    if (this.shell.store.category.value !== before) this.show(this.shell.store.target(this.shell.store.category.value)); // the category's entry went: Appearance opens in its place
    // An admin reads the provider usages (the limits, with no account names); the account switcher and every change are the owner's alone.
    this.usages.setOwner(owner);
  }
  setUser(userID) {
    this.userID = userID;
    this.shell.store.setUser();
    if (parsePath(location.hash) && !this.dialog.open) queueMicrotask(() => { if (this.userID === userID) void this.address.follow(); }); // opened by a link
    this.general.setUser(userID);
  }
  // Settings opens on General; the usages column's own Settings button asks for the Usages section.
  async open(section = '', options = {}) {
    if (options.machineID) this.harnessSettings.wanted = options.machineID; // Harnesses opens on this machine
    if (this.address.unwinding && !this.dialog.open) await this.address.unwinding; // a close's history walk is still going: opening now would race it
    const target = this.shell.store.resolve(section);
    if (!this.dialog.open) { this.dialog.showModal(); this.closed = false; if (!options.fromAddress) this.address.enter(this.pathOf(target)); }
    else this.navigate(this.pathOf(target));
    this.show(target); // the dialog shows at once; what the account may do is read again right after
    this.usages.setError(''); // before the wait: whatever the person does in the meantime may write here, and must not be wiped
    try { await refreshRights(); this.setRights(rights); this.owner.onRights?.(rights); } catch (error) { captureError(error, 'settings.rights'); /* keep what sign-in said */ }
    this.usersSettings.prepare().catch(() => this.usersSettings.settle(true));
    // The default daily limits do not depend on the switcher or the usage snapshots, so they load on their own.
    void this.usages.syncDefault();
    if (!rights.owner && !rights.admin) { if (!this.usagesSection.hidden) this.show(this.shell.store.target('appearance')); return; } // nothing of the usages is theirs to load
    try { await this.usages.load(); }
    catch (error) {
      if (error.status === 403) {
        this.shell.store.hideUsages();
        document.querySelector('#usages-toggle').hidden = true;
        // Fall back to General only while the reader is still looking at the Usages section this open() showed;
        // a tab they picked while the request was in flight must not be taken away.
        if (!this.usagesSection.hidden) this.show(this.shell.store.target('appearance'));
      } else this.usages.setError(error.message || 'Could not load usage snapshots');
    }
  }
  usagesChanged() { return this.usages.changed(); }
  async requestClose() {
    if (!this.dialog.open || this.closePending) return false;
    if (Object.values(this.pages).some(page => page.hasUnsavedChanges())) {
      this.closePending = true;
      try {
        const discard = await confirmDialog('Discard unsaved changes?', { title: 'Discard unsaved changes?', confirm: 'Discard', cancel: 'Keep editing', danger: true });
        if (!discard || !this.dialog.open) return false;
        this.discarding = true;
      } finally {
        this.closePending = false;
      }
    }
    this.dialog.close();
    this.afterClose();
    return true;
  }
  afterClose() {
    if (this.closed) return;
    this.closed = true;
    this.release(); this.address.leave();
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
  // What a level looks like in the address.
  pathOf(target) { return target.category ? categoryPath(categoryById(target.category), target.sub) : target.section ? `settings/${target.section}` : 'settings'; }
  // Shows a level: the shell follows, and the category's page is drawn and told it was opened. A sub-category scrolls to its place.
  show(target) {
    target = this.shell.store.apply(target);
    const category = categoryById(this.shell.store.category.value), section = category.page;
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
    const sub = target.sub && (category.subs || []).find(item => item.id === target.sub);
    if (sub) void reveal(this.dialog, { anchor: sub.anchor });
  }
  // A search result: its category opens (a new entry, as a click in the navigation) and the setting is brought into view and marked; one that never draws
  // (a page that failed to load) marks the category's head instead.
  async openResult(result) {
    this.shell.store.query.value = '';
    const path = categoryPath(result.category);
    if (path === this.address.path) this.show(this.shell.store.resolve(path)); else this.navigate(path);
    const row = result.setting ? await reveal(this.dialog, result.setting) : null;
    if (!row) await reveal(this.dialog, { anchor: '', selector: '.settings-cat-head' });
  }
  // An old call with a page name.
  selectSection(section) { this.show(this.shell.store.resolve(section)); }
  // A click in the navigation: a new entry in the history, and the level shown.
  navigate(path) { this.address.navigate(path); }
  back() { this.address.back(); }
  followAddress(event) { return this.address.follow(event); }
}
