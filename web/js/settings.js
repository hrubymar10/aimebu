import { request, rights, refreshRights } from './api.js';
import { savePreference } from './preferences.js';
import { NotificationSounds } from './notification-sounds.js';
import { element, creditsText, usageWindowRow } from './usage-format.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { iconButton } from './icon-button.js';
import { createProjectSettingsSource } from './project-settings-source.js';
import { RoleSettings } from './role-settings.js';
import { ProjectSettings } from './project-settings.js';
import { MachinesSettings } from './machines.js';
import { HarnessSettings } from './harness-settings.js';
import { MacroSettings } from './macro-settings.js';
import { UsersSettings } from './users.js';
import { PromptSettings } from './prompt-settings.js';
import { QuotaSettings } from './quota-settings.js';
import { HttpQuotaSource } from './quota-source.js';
import { LimitGuardSettings } from './limit-guard-settings.js';
import { UsageLimitDefault } from './usage-limit-default.js';
import { HttpLimitGuardSource } from './limit-guard-source.js';
import { confirmDialog, promptDialog } from './dialogs.js';

export class SettingsPanel {
  constructor(source, projectContext, notifications) {
    this.source = source;
    this.projectSource = createProjectSettingsSource();
    // One project-settings form, shared with the Project section of the home room's settings.
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
    this.list = document.querySelector('#usages-list');
    this.error = document.querySelector('#usages-error');
    this.providers = new Map();
    this.profiles = new Map();
    this.switcherState = null;
    this.switcherControls = element('div', 'usage-switcher-controls');
    this.list.before(this.switcherControls);
    this.quotaSettings = new QuotaSettings(new HttpQuotaSource());
    this.switcherControls.after(this.quotaSettings.node);
    this.limitGuardSettings = new LimitGuardSettings(new HttpLimitGuardSource());
    this.quotaSettings.node.after(this.limitGuardSettings.node);
    this.usageLimitDefault = new UsageLimitDefault();
    this.limitGuardSettings.node.after(this.usageLimitDefault.node);
    this.usagesTab = document.querySelector('#settings-usages-tab');
    this.generalTab = document.querySelector('#settings-general-tab');
    this.generalSection = document.querySelector('#settings-general');
    this.themeSelect = document.querySelector('#theme-select');
    this.themeSelect.value = document.documentElement.dataset.theme || 'dark';
    this.projectTab = document.querySelector('#settings-project-tab');
    this.notificationsTab = document.querySelector('#settings-notifications-tab');
    this.rolesTab = document.querySelector('#settings-roles-tab');
    this.machinesTab = document.querySelector('#settings-machines-tab');
    this.usagesSection = document.querySelector('#settings-usages');
    this.projectSection = document.querySelector('#settings-project');
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
    this.agentsTitle.parentElement.append(this.generalTab, this.usagesTab, this.projectTab, this.notificationsTab, this.rolesTab, this.usersSettings.tab,
      this.promptSettings.tab, this.macroSettings.tab, this.agentsTitle, this.machinesTab, this.harnessSettings.tab);
    this.muteToggle = document.querySelector('#notification-muted');
    this.permissionButton = document.querySelector('#notification-permission');
    this.permissionStatus = document.querySelector('#notification-status');
    this.sounds = new NotificationSounds(this.notificationsSection, notifications);
    this.projectSelect = document.querySelector('#project-settings-select');
    this.project.onRenamed = (id, name) => { const option = [...this.projectSelect.options].find(item => item.value === id); if (option) option.textContent = name; };
    this.usagesTab.onclick = () => this.selectSection('usages');
    this.generalTab.onclick = () => this.selectSection('general');
    this.themeSelect.onchange = () => {
      const theme = this.themeSelect.value;
      if (theme === 'dark') delete document.documentElement.dataset.theme;
      else document.documentElement.dataset.theme = theme;
      try {
        if (this.userID) localStorage.setItem(`aimebu2.ui.theme.${this.userID}`, theme);
      } catch {}
      savePreference('theme', theme);
    };
    this.projectTab.onclick = () => this.selectSection('project');
    this.notificationsTab.onclick = () => this.selectSection('notifications');
    this.rolesTab.onclick = () => this.selectSection('roles');
    this.machinesTab.onclick = () => this.selectSection('machines');
    this.muteToggle.onchange = () => { this.notifications.muted = this.muteToggle.checked; };
    this.permissionButton.onclick = async () => {
      try { await this.notifications.requestPermission(); }
      catch (error) { this.permissionStatus.textContent = error.message; return; }
      this.renderNotifications();
    };
    this.projectSelect.onchange = () => this.project.mount(this.projectSection, this.projectSelect.value);
    // The shared form goes back to the home room's settings if those are still open underneath.
    this.dialog.addEventListener('close', () => this.project.release(this.projectSection));
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
    let theme = 'dark';
    try {
      theme = localStorage.getItem(`aimebu2.ui.theme.${userID}`) || theme;
      localStorage.setItem('aimebu2.ui.last-user', userID);
    } catch {}
    if (![...this.themeSelect.options].some(option => option.value === theme)) theme = 'dark';
    this.themeSelect.value = theme;
    if (theme === 'dark') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
  }
  // Settings opens on General; the usages column's own Settings button asks for the Usages section.
  async open(section = 'general', options = {}) {
    if (!this.dialog.open) this.dialog.showModal();
    if (options.machineID) this.harnessSettings.wanted = options.machineID; // Harnesses opens on this machine
    this.selectSection(section); // the dialog shows at once; what the account may do is read again right after
    this.error.textContent = ''; // before the wait: whatever the person does in the meantime may write here, and must not be wiped
    try { await refreshRights(); this.setRights(rights); this.onRights?.(rights); } catch { /* keep what sign-in said */ }
    this.usersSettings.prepare().catch(() => this.usersSettings.settle(true));
    // The default daily limits do not depend on the switcher or the usage snapshots, so they load on their own.
    void this.usageLimitDefault.sync();
    if (!rights.owner) { if (!this.usagesSection.hidden) this.selectSection('general'); return; } // nothing of the usages is theirs to load
    try { await this.loadUsages(); }
    catch (error) {
      if (error.status === 403) {
        this.usagesTab.hidden = true;
        document.querySelector('#usages-toggle').hidden = true;
        // Fall back to General only while the reader is still looking at the Usages section this open() showed;
        // a tab they picked while the request was in flight must not be taken away.
        if (!this.usagesSection.hidden) this.selectSection('general');
      } else this.error.textContent = error.message || 'Could not load usage snapshots';
    }
  }
  async loadUsages() {
    const [usage, state] = await Promise.all([this.source.list(), this.source.switcher()]);
    this.switcherState = state;
    this.tokenStatesPending = Object.values(usage?.settings?.token_auto_refresh_state || {}).some(item => item?.status === 'pending');
    this.render(usage);
    await this.quotaSettings.sync({ owner: !!state?.owner, enabled: !!state?.enabled });
    await this.limitGuardSettings.sync({ owner: !!state?.owner });
  }
  // Live push from the event stream: reload the open usages panel in place.
  // A push that cannot be applied right away (a load is running, or a field in
  // the panel has focus and a re-render would drop what is being typed) is
  // remembered and applied once the load finishes or focus leaves the panel.
  async usagesChanged() {
    if (!this.dialog.open || this.usagesSection.hidden) return;
    if (this.usagesLoading) { this.usagesDirty = true; return; }
    const focused = document.activeElement;
    if (focused && this.usagesSection.contains(focused) && focused.matches('input, select, textarea')) {
      this.usagesDirty = true;
      if (!this.usagesFocusWatch) {
        this.usagesFocusWatch = () => {
          if (!this.usagesDirty) return;
          // focusout fires before the next element takes focus; look after it settles.
          setTimeout(() => { if (this.usagesDirty) this.usagesChanged(); }, 0);
        };
        this.usagesSection.addEventListener('focusout', this.usagesFocusWatch);
      }
      return;
    }
    this.usagesDirty = false;
    this.usagesLoading = true;
    try { await this.loadUsages(); }
    catch {}
    finally {
      this.usagesLoading = false;
      if (this.usagesDirty) this.usagesChanged();
    }
  }
  async mutateSwitcher(action) {
    this.error.textContent = '';
    try { await action(); await this.loadUsages(); }
    catch (error) { this.error.textContent = error.message || 'Account switch failed'; }
  }
  async mutateUsage(action) {
    this.error.textContent = '';
    try { await action(); await this.loadUsages(); }
    catch (error) { this.error.textContent = error.message || 'Usage settings could not be saved'; }
  }
  async startCopilot(host) {
    this.error.textContent = '';
    try {
      this.copilotFlow = await this.source.copilotStart(host);
      await this.loadUsages();
      this.pollCopilot();
    } catch (error) { this.error.textContent = error.message || 'GitHub sign-in could not start'; }
  }
  async pollCopilot() {
    const flow = this.copilotFlow;
    if (!flow || Date.now() >= new Date(flow.expires_at).getTime()) return;
    try {
      const result = await this.source.copilotPoll(flow.flow_id);
      if (this.copilotFlow !== flow) return;
      flow.status = result.status;
      if (result.status === 'success') { this.copilotFlow = null; await this.loadUsages(); return; }
      if (result.status === 'denied' || result.status === 'expired') { await this.loadUsages(); return; }
      this.renderCopilotFlow();
      setTimeout(() => this.pollCopilot(), Math.max(1, result.retry_after || result.interval || flow.interval || 5) * 1000);
    } catch (error) { this.error.textContent = error.message || 'GitHub sign-in polling failed'; }
  }
  renderCopilotFlow() {
    const target = this.providers.get('github-copilot')?.querySelector('.usage-copilot-flow');
    if (!target) return;
    target.replaceChildren();
    const flow = this.copilotFlow;
    if (!flow) return;
    const link = element('a', '', 'Open GitHub verification');
    link.href = flow.verification_uri; link.target = '_blank'; link.rel = 'noopener noreferrer';
    target.append(element('span', '', `Code: ${flow.user_code} · ${flow.status || 'Waiting for GitHub'}`), link);
  }
  selectSection(section) {
    queueMicrotask(() => { const selected = this.dialog.querySelector('nav .settings-selected'); if (selected) this.dialog.querySelector('#settings-title').textContent = selected.textContent.trim(); });
	this.usersSettings.section.hidden = section !== 'users';
	this.usersSettings.tab.classList.toggle('settings-selected', section === 'users');
    this.promptSettings.tab.classList.toggle('settings-selected', section === 'prompts');
    const project = section === 'project';
    const notifications = section === 'notifications';
    this.generalSection.hidden = section !== 'general';
    this.generalTab.classList.toggle('settings-selected', section === 'general');
    this.usagesSection.hidden = section !== 'usages'; this.projectSection.hidden = !project; this.notificationsSection.hidden = !notifications;
    this.rolesSection.hidden = section !== 'roles';
    this.machinesSection.hidden = section !== 'machines';
	this.macroSettings.section.hidden = section !== 'macros';
    this.harnessSettings.section.hidden = section !== 'harnesses';
    this.promptSettings.section.hidden = section !== 'prompts';
    this.harnessSettings.tab.classList.toggle('settings-selected', section === 'harnesses');
    this.usagesTab.classList.toggle('settings-selected', section === 'usages');
    this.projectTab.classList.toggle('settings-selected', project);
    this.notificationsTab.classList.toggle('settings-selected', notifications);
    this.rolesTab.classList.toggle('settings-selected', section === 'roles');
    this.machinesTab.classList.toggle('settings-selected', section === 'machines');
    this.macroSettings.tab.classList.toggle('settings-selected', section === 'macros');
    for (const [tab, selected] of [[this.generalTab, section === 'general'], [this.usagesTab, section === 'usages'], [this.projectTab, project], [this.notificationsTab, notifications], [this.rolesTab, section === 'roles'], [this.machinesTab, section === 'machines'], [this.macroSettings.tab, section === 'macros'], [this.harnessSettings.tab, section === 'harnesses'], [this.usersSettings.tab, section === 'users'], [this.promptSettings.tab, section === 'prompts']]) {
      if (selected) tab.setAttribute('aria-current', 'page'); else tab.removeAttribute('aria-current');
    }
    if (project) this.showProjects();
    if (section === 'users') this.usersSettings.open();
    if (notifications) { this.renderNotifications(); this.sounds.open(); }
    if (section === 'roles') this.roleSettings.open();
    if (section === 'machines') this.machinesSettings.load();
    if (section === 'harnesses') this.harnessSettings.open();
    if (section === 'macros') this.macroSettings.open();
    if (section === 'prompts') this.promptSettings.open();
  }
  renderNotifications() {
    this.muteToggle.checked = this.notifications.muted;
    this.permissionStatus.textContent = this.notifications.status;
    this.permissionButton.disabled = !this.notifications.available || Notification.permission !== 'default';
    this.permissionButton.textContent = this.notifications.available && Notification.permission === 'granted' ? 'Enabled' : 'Enable browser notifications';
  }
  async showProjects() {
    let { projects, selectedProjectID } = this.projectContext();
    const ticket = (this.projectLoadSeq || 0) + 1; this.projectLoadSeq = ticket;
    this.project.autoResume.disabled = true; this.project.worktrees.disabled = true;
    this.project.error.textContent = '';
    if (!projects.length) {
      try { projects = await this.projectSource.projects(); }
      catch (error) { if (ticket === this.projectLoadSeq) this.project.error.textContent = error.message; return; }
    }
    if (ticket !== this.projectLoadSeq || this.projectSection.hidden) return;
    this.projectSelect.replaceChildren(...projects.map(project => {
      const option = document.createElement('option'); option.value = project.id; option.textContent = project.name; return option;
    }));
    if (projects.some(project => project.id === selectedProjectID)) this.projectSelect.value = selectedProjectID;
    this.project.mount(this.projectSection, this.projectSelect.value);
  }
  providerCard() {
    const card = element('article', 'usage-provider');
    const header = element('header', 'usage-provider-header');
    header.append(element('h4', 'usage-provider-name'), element('span', 'usage-provider-status'));
    card.append(header, element('p', 'usage-affected'), element('div', 'usage-provider-actions'), element('div', 'usage-profiles'));
    return card;
  }
  profileCard() {
    const card = element('section', 'usage-profile');
    const header = element('div', 'usage-profile-header');
    header.append(element('strong', 'usage-profile-name'), element('span', 'usage-profile-plan'), element('span', 'usage-profile-active'));
    card.append(header, element('p', 'usage-profile-email'), element('div', 'usage-windows'), element('p', 'usage-profile-credits'), element('p', 'usage-profile-status'));
    const action = element('button', 'btn usage-switch', 'Switch');
    action.type = 'button';
    const remove = iconButton('trash', 'Delete profile', { danger: true, className: 'usage-remove' });
    const rename = iconButton('edit', 'Rename profile', { className: 'usage-rename' });
    card.append(action, rename, remove);
    return card;
  }
  renderUsageControls(settings, owner) {
    if (!owner) return;
    const controls = element('div', 'usage-config-controls');
    const interval = element('input'); interval.type = 'number'; interval.min = '15'; interval.value = String(settings?.refresh_interval_sec || 120);
    const display = element('select');
    for (const [value, label] of [['left', 'Percent left'], ['used', 'Percent used']]) {
      const option = element('option', '', label); option.value = value; display.append(option);
    }
    display.value = settings?.percent_display || 'left';
    const warning = element('input'); warning.type = 'number'; warning.min = '1'; warning.max = '99'; warning.value = String(settings?.alert_warning_remaining ?? 20);
    const critical = element('input'); critical.type = 'number'; critical.min = '1'; critical.max = '99'; critical.value = String(settings?.alert_critical_remaining ?? 10);
    const save = element('button', 'btn btn-primary', 'Save usage settings'); save.type = 'button';
    save.onclick = () => this.mutateUsage(() => this.source.settings({ refresh_interval_sec: Number(interval.value), percent_display: display.value,
      alert_warning_remaining: Number(warning.value), alert_critical_remaining: Number(critical.value) }));
    const refresh = element('button', 'btn', 'Refresh now'); refresh.type = 'button';
    refresh.onclick = () => this.mutateUsage(async () => {
      try { await this.source.refresh(); }
      catch (error) {
        if (error.status === 429 && error.retryAfter) error.message = `Refreshed moments ago; try again in ${error.retryAfter}s`;
        throw error;
      }
    });
    const actions = element('div', 'settings-actions'); actions.append(save, refresh);
    controls.append(settingsGroup([
      settingsRow({ label: 'Refresh interval (seconds)', desc: 'How often the server re-reads provider limits in the background. Minimum 15 seconds.', control: interval, id: 'usage-refresh-interval' }),
      settingsRow({ label: 'Percentages', desc: 'Show usage bars as the share still left or the share already used, here and in the usages popover.', control: display, id: 'usage-percent-display' }),
      settingsRow({ label: 'Usage warning (% remaining)', desc: 'Send one global inbox alert per provider window and reset when this much quota remains.', control: warning, id: 'usage-alert-warning' }),
      settingsRow({ label: 'Usage critical (% remaining)', desc: 'Send a stronger alert at this lower remaining percentage. It must be below the warning threshold.', control: critical, id: 'usage-alert-critical' }),
    ], 'Usage refresh'), actions, this.tokenAutoRefreshGroup(settings));
    this.switcherControls.append(controls);
  }
  // Credentials of the token-copy model that need a human (a rejected refresh token, one about to
  // expire), one sentence each. Plain text only: the server never sends CLI output here.
  renderCredentialAlerts(alerts, owner) {
    if (!owner || !Array.isArray(alerts) || !alerts.length) return;
    const box = element('div', 'usage-credential-alerts');
    box.setAttribute('role', 'alert');
    for (const alert of alerts) box.append(element('p', `usage-credential-alert usage-credential-alert-${String(alert.kind || '').replace(/[^a-z_]/g, '')}`, String(alert.message || '')));
    this.switcherControls.append(box);
  }
  // "Auto-refresh tokens": the server becomes the only refresher of the Claude and Codex logins
  // (the token-copy model). The setting migrates or un-migrates each tool on the server; the
  // per-tool line shows how that went (migrated, off, pending, n/a, or waiting: <reason>).
  tokenAutoRefreshGroup(settings) {
    const on = !!settings?.token_auto_refresh;
    const available = !!settings?.token_auto_refresh_available;
    const toggle = element('input'); toggle.type = 'checkbox'; toggle.checked = on;
    toggle.disabled = !available && !on; // OFF must stay possible if the executor disappears
    toggle.onchange = () => this.saveTokenAutoRefresh(toggle.checked);
    let desc = 'aimebu becomes the only thing that refreshes your Claude and Codex logins. While on, aimebu must stay running whenever you use Claude or Codex, also outside aimebu: if it is stopped for more than about an hour around a token\'s expiry, sessions get auth errors until it runs again. Turn this off before stopping aimebu for long or uninstalling it.';
    if (!available) desc += ' Unavailable: harness-docker-ctrl is not installed.';
    const row = settingsRow({ label: 'Auto-refresh tokens', desc, control: toggle, id: 'usage-token-auto-refresh', className: toggle.disabled ? 'usages-provider-row-disabled' : '' });
    const info = row.querySelector('.settings-row-info');
    for (const tool of ['claude', 'codex']) {
      const state = settings?.token_auto_refresh_state?.[tool] || { status: 'off' };
      const status = ['migrated', 'waiting', 'pending', 'n/a'].includes(state.status) ? state.status : 'off';
      const line = element('span', 'settings-desc usage-token-state', `${tool[0].toUpperCase()}${tool.slice(1)}: ${status}${status === 'waiting' && state.reason ? `: ${state.reason}` : ''}`);
      line.dataset.tool = tool;
      info.append(line);
    }
    return settingsGroup([row], 'Account maintenance');
  }
  async saveTokenAutoRefresh(value) {
    this.error.textContent = '';
    try {
      await this.source.settings({ token_auto_refresh: value });
      await this.loadUsages();
      // The server answers pending and converts in the background: re-read until it settles.
      for (let attempt = 0; attempt < 120 && this.tokenStatesPending; attempt++) {
        await new Promise(resolve => setTimeout(resolve, 500));
        if (!this.dialog.open) return;
        await this.loadUsages();
      }
    } catch (error) {
      this.error.textContent = error.message || `Could not ${value ? 'enable' : 'disable'} token auto-refresh`;
      await this.loadUsages().catch(() => {});
    }
  }
  renderProviderSetup(card, provider, order, providers, owner) {
    if (!owner) return;
    const key = provider.provider_name;
    const actions = card.querySelector('.usage-provider-actions');
    const configured = provider.token_configured || provider.cookie_configured || provider.api_key_configured;
    const toggle = element('button', 'btn', provider.enabled ? 'Disable' : 'Enable'); toggle.type = 'button';
    toggle.disabled = !provider.enabled && !configured && !['codex', 'claude-code'].includes(key);
    toggle.onclick = () => this.mutateUsage(() => this.source.provider(key, !provider.enabled));
    actions.append(toggle);
    for (const [label, delta, glyph] of [['up', -1, 'arrow-up'], ['down', 1, 'arrow-down']]) {
      const button = iconButton(glyph, `Move ${provider.label || key} ${label}`); button.disabled = order + delta < 0 || order + delta >= providers.length;
      button.onclick = () => { const keys = providers.map(item => item.provider_name); [keys[order], keys[order + delta]] = [keys[order + delta], keys[order]]; this.mutateUsage(() => this.source.settings({ provider_order: keys })); };
      actions.append(button);
    }
    if (key === 'github-copilot') {
      const host = element('input'); host.type = 'url'; host.placeholder = 'GitHub Enterprise host (optional)'; host.value = provider.enterprise_host || '';
      const signIn = element('button', 'btn btn-primary', 'Sign in with GitHub'); signIn.type = 'button'; signIn.onclick = () => this.startCopilot(host.value);
      actions.append(host, signIn);
      if (provider.token_configured) { const signOut = element('button', 'btn', 'Sign out'); signOut.type = 'button'; signOut.onclick = () => this.mutateUsage(() => this.source.copilotLogout()); actions.append(signOut); }
      actions.append(element('div', 'usage-copilot-flow'));
      this.renderCopilotFlow();
    }
    if (key === 'mistral') {
      const cookie = element('textarea'); cookie.placeholder = 'Paste Mistral Cookie request header'; cookie.rows = 2; cookie.setAttribute('aria-label', 'Mistral Cookie header');
      const save = element('button', 'btn btn-primary', provider.cookie_configured ? 'Update cookie' : 'Save cookie'); save.type = 'button'; save.onclick = () => this.mutateUsage(() => this.source.mistral(cookie.value));
      actions.append(cookie, save);
      if (provider.cookie_configured) { const clear = element('button', 'btn btn-danger', 'Clear cookie'); clear.type = 'button'; clear.onclick = () => this.mutateUsage(() => this.source.mistral('')); actions.append(clear); }
    }
    if (key === 'ollama-cloud') {
      const mode = element('select'); mode.setAttribute('aria-label', 'Ollama Cloud authentication mode');
      for (const [value, label] of [['auto', 'Auto'], ['cookie', 'Cookie'], ['api_key', 'API key']]) { const option = element('option', '', label); option.value = value; mode.append(option); }
      mode.value = provider.auth_mode || 'auto';
      const cookie = element('textarea'); cookie.placeholder = 'Paste Ollama Cookie request header'; cookie.rows = 2; cookie.setAttribute('aria-label', 'Ollama Cloud Cookie header');
      const keyInput = element('input'); keyInput.type = 'password'; keyInput.placeholder = 'Ollama API key'; keyInput.setAttribute('aria-label', 'Ollama Cloud API key');
      const save = element('button', 'btn btn-primary', 'Save credentials'); save.type = 'button';
      save.onclick = () => { const body = { auth_mode: mode.value }; if (cookie.value.trim()) body.cookie = cookie.value; if (keyInput.value.trim()) body.api_key = keyInput.value; this.mutateUsage(() => this.source.ollama(body)); };
      actions.append(mode, cookie, keyInput, save);
      if (configured) { const clear = element('button', 'btn btn-danger', 'Clear credentials'); clear.type = 'button'; clear.onclick = () => this.mutateUsage(() => this.source.ollama({ auth_mode: 'auto', cookie: '', api_key: '' })); actions.append(clear); }
    }
  }
  render(response) {
    const state = this.switcherState || { enabled: false, profiles: [], eligibility: [], affected_agents: {} };
    this.switcherControls.replaceChildren();
    if (state.owner) {
      const toggle = element('button', state.enabled ? 'btn' : 'btn btn-primary', state.enabled ? 'Disable account switcher' : 'Enable account switcher');
      toggle.type = 'button';
      toggle.onclick = () => this.mutateSwitcher(() => this.source.enable(!state.enabled));
      this.switcherControls.append(toggle);
    }
    this.renderCredentialAlerts(response?.credential_alerts, state.owner);
    this.renderUsageControls(response?.settings, state.owner);
    const providers = [...(response?.providers || [])].sort((a, b) => (a.order_number || 0) - (b.order_number || 0));
    const active = new Set();
    for (const provider of providers) {
      const key = provider.provider_name;
      active.add(key);
      let card = this.providers.get(key);
      if (!card) { card = this.providerCard(); this.providers.set(key, card); }
      card.dataset.provider = key;
      card.querySelector('.usage-provider-name').textContent = provider.label || key;
      card.querySelector('.usage-provider-status').textContent = !provider.available ? 'Unavailable' : provider.enabled ? 'Enabled' : 'Not configured';
      const tool = key === 'claude-code' ? 'claude' : key;
      const eligible = (state.eligibility || []).find(item => item.tool === tool);
      const switchable = tool === 'claude' || tool === 'codex';
      const hasActiveProfile = (state.profiles || []).some(item => item.tool === tool && item.active);
      const affected = (state.affected_agents || {})[tool] || [];
      const impact = card.querySelector('.usage-affected');
      impact.textContent = affected.length ? `Switch affects: ${affected.map(agent => `${agent.id}${agent.running ? ' (running)' : ''}`).join(', ')}` : 'No agents use this tool';
      impact.hidden = !switchable || !state.enabled;
      const providerActions = card.querySelector('.usage-provider-actions');
      providerActions.replaceChildren();
      if (switchable && state.enabled && state.owner) {
        for (const [label, method] of [['Add profile', 'add'], ['Import current login', 'import']]) {
          const button = element('button', 'btn', label); button.type = 'button';
          button.onclick = async () => {
            const name = await promptDialog('Profile name', { title: label, label: 'Profile name', required: true, confirm: label === 'Add profile' ? 'Add' : 'Import' });
            if (name?.trim()) this.mutateSwitcher(() => this.source[method](tool, name.trim()));
          };
          providerActions.append(button);
        }
      }
      this.renderProviderSetup(card, provider, providers.findIndex(item => item.provider_name === key), providers, state.owner);
      const list = card.querySelector('.usage-profiles');
      const profileKeys = new Set();
      for (const profile of provider.profiles || []) {
        const id = `${key}/${profile.profile_name || 'local'}`;
        profileKeys.add(id);
        let node = this.profiles.get(id);
        if (!node) { node = this.profileCard(); this.profiles.set(id, node); }
        this.updateProfile(node, profile, response.settings?.percent_display === 'used');
        const configured = (state.profiles || []).some(item => item.tool === tool && item.name === profile.profile_name);
        const switchButton = node.querySelector('.usage-switch');
        switchButton.hidden = !switchable || !state.enabled || !state.owner || !configured || profile.active;
        switchButton.disabled = !hasActiveProfile || (!eligible?.eligible && !eligible?.absent);
        switchButton.title = !hasActiveProfile ? 'Import the current login first' : switchButton.disabled ? (eligible?.reason || 'Tool is not eligible') : '';
        switchButton.onclick = async () => {
          const impactText = affected.length ? `Affected agents: ${affected.map(agent => agent.id).join(', ')}.` : 'No affected agents.';
          if (await confirmDialog(`${impactText}${profile.has_credentials ? '' : ' This profile needs a new login.'} Switch to ${profile.profile_name}?`, { title: 'Switch account', confirm: 'Switch' })) {
            this.mutateSwitcher(() => this.source.use(tool, profile.profile_name));
          }
        };
        const renameButton = node.querySelector('.usage-rename');
        renameButton.title = `Rename ${profile.profile_name}`; renameButton.setAttribute('aria-label', renameButton.title);
        renameButton.hidden = !switchable || !state.enabled || !state.owner || !configured;
        renameButton.onclick = async () => {
          const next = (await promptDialog(`Rename ${profile.profile_name} to`, { title: 'Rename profile', label: 'New name', value: profile.profile_name, required: true, confirm: 'Rename' }))?.trim();
          if (next && next !== profile.profile_name) this.mutateSwitcher(() => this.source.rename(tool, profile.profile_name, next));
        };
        const removeButton = node.querySelector('.usage-remove');
        removeButton.title = `Delete ${profile.profile_name}`; removeButton.setAttribute('aria-label', removeButton.title);
        removeButton.hidden = !switchable || !state.enabled || !state.owner || !configured;
        removeButton.onclick = async () => {
          if (await confirmDialog(`Delete ${profile.profile_name}?${profile.active ? ' This clears the live login.' : ''}`, { title: 'Delete profile', confirm: 'Delete', danger: true })) {
            this.mutateSwitcher(() => this.source.remove(tool, profile.profile_name));
          }
        };
        list.append(node);
      }
      for (const [id, node] of this.profiles) if (id.startsWith(`${key}/`) && !profileKeys.has(id)) { node.remove(); this.profiles.delete(id); }
      this.list.append(card);
    }
    for (const [key, card] of this.providers) if (!active.has(key)) { card.remove(); this.providers.delete(key); }
    if (!providers.length) {
      if (!this.empty) this.empty = element('p', 'usage-empty', 'No usage providers are available yet.');
      this.list.append(this.empty);
    } else this.empty?.remove();
  }
  updateProfile(node, profile, usedDisplay) {
    node.querySelector('.usage-profile-name').textContent = profile.profile_name || 'local';
    const plan = node.querySelector('.usage-profile-plan'); plan.textContent = profile.plan || ''; plan.hidden = !plan.textContent;
    const active = node.querySelector('.usage-profile-active'); active.textContent = profile.active ? 'Active' : ''; active.hidden = !profile.active;
    const email = node.querySelector('.usage-profile-email'); email.textContent = profile.email || ''; email.hidden = !email.textContent;
    const windows = node.querySelector('.usage-windows');
    windows.replaceChildren(...(profile.windows || []).map(window => this.windowRow(window, usedDisplay)));
    const credits = node.querySelector('.usage-profile-credits'); credits.textContent = profile.credits ? creditsText(profile.credits) : ''; credits.hidden = !credits.textContent;
    const status = node.querySelector('.usage-profile-status');
    const refreshed = profile.last_refresh_at ? `Updated ${new Date(profile.last_refresh_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : '';
    const detail = profile.status === 'token_expired' ? 'Stale – token expired' : profile.stale ? 'Stale snapshot' : profile.status === 'ok' ? '' : (profile.status || 'No snapshot').replaceAll('_', ' ');
    status.textContent = detail + refreshed;
    status.hidden = !status.textContent;
  }
  windowRow(window, usedDisplay) { return usageWindowRow(window, usedDisplay); }
}
