import { rights, refreshRights } from './api.js';
import { GeneralSettings } from './pages/general/page.js';
import { NotificationSettings } from './pages/notifications/page.js';
import { UsagesSettings } from './pages/usages/page.js';
import { createProjectSettingsSource } from './project-settings-source.js';
import { RoleSettings } from './pages/roles/page.js';
import { ProjectSettings } from './project-settings.js';
import { MachinesSettings } from './machines.js';
import { HarnessSettings } from './pages/harnesses/page.js';
import { MacroSettings } from './pages/macros/page.js';
import { UsersSettings } from './users.js';
import { PromptSettings } from './pages/prompts/page.js';
import { QuotaSettings } from './quota-settings.js';
import { HttpQuotaSource } from './quota-source.js';
import { LimitGuardSettings } from './limit-guard-settings.js';
import { UsageLimitDefault } from './usage-limit-default.js';
import { HttpLimitGuardSource } from './limit-guard-source.js';

export class SettingsPanel {
  constructor(source, projectContext, notifications) {
    this.source = source;
    this.projectSource = createProjectSettingsSource();
    // The project's settings form lives in the home room's settings (Room settings → Project); this panel only owns the object.
    this.project = new ProjectSettings(this.projectSource, projectContext);
    this.projectContext = projectContext;
    this.roleSettings = new RoleSettings();
    this.machinesSettings = new MachinesSettings();
    this.notifications = notifications;
    this.dialog = document.querySelector('#settings-dialog');
    this.usersSettings = new UsersSettings(this.dialog, projectContext);
    this.usersSettings.tab.onclick = () => this.selectSection('users');
    this.promptSettings = new PromptSettings(this.dialog, projectContext);
    this.promptSettings.tab.onclick = () => this.selectSection('prompts');
    // The automatic-switching blocks below are other modules', built between the two hosts of the usages page.
    this.usages = new UsagesSettings({ source, dialog: this.dialog, section: document.querySelector('#settings-usages'), switcher: document.querySelector('#usages-switcher'),
      list: document.querySelector('#usages-list'), error: document.querySelector('#usages-error'),
      afterLoad: async state => { await this.quotaSettings.sync({ owner: !!state?.owner, enabled: !!state?.enabled }); await this.limitGuardSettings.sync({ owner: !!state?.owner }); } });
    this.quotaSettings = new QuotaSettings(new HttpQuotaSource());
    document.querySelector('#usages-switcher').after(this.quotaSettings.node);
    this.limitGuardSettings = new LimitGuardSettings(new HttpLimitGuardSource());
    this.quotaSettings.node.after(this.limitGuardSettings.node);
    this.usageLimitDefault = new UsageLimitDefault();
    this.limitGuardSettings.node.after(this.usageLimitDefault.node);
    this.usagesTab = document.querySelector('#settings-usages-tab');
    this.generalTab = document.querySelector('#settings-general-tab');
    this.generalSection = document.querySelector('#settings-general');
    this.general = new GeneralSettings(this.generalSection);
    this.notificationsTab = document.querySelector('#settings-notifications-tab');
    this.rolesTab = document.querySelector('#settings-roles-tab');
    this.machinesTab = document.querySelector('#settings-machines-tab');
    this.usagesSection = document.querySelector('#settings-usages');
    this.notificationsSection = document.querySelector('#settings-notifications');
    this.rolesSection = document.querySelector('#settings-roles');
    this.machinesSection = document.querySelector('#settings-machines');
    this.macroSettings = new MacroSettings(this.dialog);
    this.macroSettings.tab.onclick = () => this.selectSection('macros');
    this.harnessSettings = new HarnessSettings(this.dialog);
    this.harnessSettings.tab.onclick = () => this.selectSection('harnesses');
    // Two nav groups: the account and instance settings under SETTINGS, then what runs the agents (Machines, Harnesses) under AGENTS.
    // The tabs the other modules add are appended at the end of the nav, so the order is set here once they all exist.
    this.agentsTitle = this.dialog.querySelector('#settings-agents-title');
    this.agentsTitle.parentElement.append(this.generalTab, this.usagesTab, this.notificationsTab, this.rolesTab, this.usersSettings.tab,
      this.promptSettings.tab, this.macroSettings.tab, this.agentsTitle, this.machinesTab, this.harnessSettings.tab);
    this.notificationSettings = new NotificationSettings(this.notificationsSection, notifications);
    this.usagesTab.onclick = () => this.selectSection('usages');
    this.generalTab.onclick = () => this.selectSection('general');
    this.notificationsTab.onclick = () => this.selectSection('notifications');
    this.rolesTab.onclick = () => this.selectSection('roles');
    this.machinesTab.onclick = () => this.selectSection('machines');
    document.querySelector('#settings-toggle').onclick = () => this.open();
    document.querySelector('#settings-close').onclick = () => this.dialog.close();
    // v1's global shortcuts: Cmd/Ctrl+, opens Settings, and Cmd/Ctrl+W closes it while it is open.
    document.addEventListener('keydown', event => {
      if (!(event.metaKey || event.ctrlKey) || event.altKey || event.shiftKey || event.isComposing) return;
      if (event.key === ',' && this.userID) { event.preventDefault(); this.open(); }
      else if (event.key.toLowerCase() === 'w' && this.dialog.open) { event.preventDefault(); this.dialog.close(); }
    });
  }
  // What the account may do instance-wide. The owner and the admins (who are trusted like the owner) edit the roles, prompts and macros and see the machines and
  // edit their harnesses (pairing, approving and revoking machines stay the owner's). For anyone else those tabs are gone instead of opening onto a 403.
  setRights({ owner, admin }) {
    const settings = owner || admin;
    this.general.setRights({ owner, admin });
    this.rolesTab.hidden = !settings; this.promptSettings.tab.hidden = !settings; this.macroSettings.tab.hidden = !settings; 
    this.machinesTab.hidden = !settings; this.harnessSettings.tab.hidden = !settings;
    this.agentsTitle.hidden = this.machinesTab.hidden && this.harnessSettings.tab.hidden; // no AGENTS caption over an empty group
    // Provider usages and the account switcher are the owner's alone.
    this.usagesTab.hidden = !owner; document.querySelector('#usages-toggle').hidden = !owner;
  }
  setUser(userID) {
    this.userID = userID;
    this.usagesTab.hidden = false;
    document.querySelector('#usages-toggle').hidden = false;
    this.general.setUser(userID);
  }
  // Settings opens on General; the usages column's own Settings button asks for the Usages section.
  async open(section = 'general', options = {}) {
    if (!this.dialog.open) this.dialog.showModal();
    if (options.machineID) this.harnessSettings.wanted = options.machineID; // Harnesses opens on this machine
    this.selectSection(section); // the dialog shows at once; what the account may do is read again right after
    this.usages.setError(''); // before the wait: whatever the person does in the meantime may write here, and must not be wiped
    try { await refreshRights(); this.setRights(rights); this.onRights?.(rights); } catch { /* keep what sign-in said */ }
    this.usersSettings.prepare().catch(() => this.usersSettings.settle(true));
    // The default daily limits do not depend on the switcher or the usage snapshots, so they load on their own.
    void this.usageLimitDefault.sync();
    if (!rights.owner) { if (!this.usagesSection.hidden) this.selectSection('general'); return; } // nothing of the usages is theirs to load
    try { await this.usages.load(); }
    catch (error) {
      if (error.status === 403) {
        this.usagesTab.hidden = true;
        document.querySelector('#usages-toggle').hidden = true;
        // Fall back to General only while the reader is still looking at the Usages section this open() showed;
        // a tab they picked while the request was in flight must not be taken away.
        if (!this.usagesSection.hidden) this.selectSection('general');
      } else this.usages.setError(error.message || 'Could not load usage snapshots');
    }
  }
  usagesChanged() { return this.usages.changed(); }
  selectSection(section) {
    this.dialog.dataset.section = section; // the Harnesses page is wide: its layouts need the room (see theme-v1.css)
    queueMicrotask(() => { const selected = this.dialog.querySelector('nav .settings-selected'); if (selected) this.dialog.querySelector('#settings-title').textContent = selected.textContent.trim(); });
	this.usersSettings.section.hidden = section !== 'users';
	this.usersSettings.tab.classList.toggle('settings-selected', section === 'users');
    this.promptSettings.tab.classList.toggle('settings-selected', section === 'prompts');
    const notifications = section === 'notifications';
    this.generalSection.hidden = section !== 'general';
    this.generalTab.classList.toggle('settings-selected', section === 'general');
    this.usagesSection.hidden = section !== 'usages'; this.notificationsSection.hidden = !notifications;
    this.rolesSection.hidden = section !== 'roles';
    this.machinesSection.hidden = section !== 'machines';
	this.macroSettings.section.hidden = section !== 'macros';
    this.harnessSettings.section.hidden = section !== 'harnesses';
    this.promptSettings.section.hidden = section !== 'prompts';
    this.harnessSettings.tab.classList.toggle('settings-selected', section === 'harnesses');
    this.usagesTab.classList.toggle('settings-selected', section === 'usages');
    this.notificationsTab.classList.toggle('settings-selected', notifications);
    this.rolesTab.classList.toggle('settings-selected', section === 'roles');
    this.machinesTab.classList.toggle('settings-selected', section === 'machines');
    this.macroSettings.tab.classList.toggle('settings-selected', section === 'macros');
    for (const [tab, selected] of [[this.generalTab, section === 'general'], [this.usagesTab, section === 'usages'], [this.notificationsTab, notifications], [this.rolesTab, section === 'roles'], [this.machinesTab, section === 'machines'], [this.macroSettings.tab, section === 'macros'], [this.harnessSettings.tab, section === 'harnesses'], [this.usersSettings.tab, section === 'users'], [this.promptSettings.tab, section === 'prompts']]) {
      if (selected) tab.setAttribute('aria-current', 'page'); else tab.removeAttribute('aria-current');
    }
    if (section === 'general') this.general.open();
    if (section === 'users') this.usersSettings.open();
    if (notifications) this.notificationSettings.open();
    if (section === 'roles') this.roleSettings.open();
    if (section === 'machines') this.machinesSettings.load();
    if (section === 'harnesses') this.harnessSettings.open();
    if (section === 'macros') this.macroSettings.open();
    if (section === 'prompts') this.promptSettings.open();
  }
}
