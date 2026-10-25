import { launcherReady } from './harness-offer.js';
import { request } from './api.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { confirmDialog } from './dialogs.js';
import { iconButton, setGlyph } from './icon-button.js';
import { RoleDefaults, ROLE_DEFAULTS_CHANGED } from './role-defaults.js';
import { positionOf, canAddDefault } from './role-defaults-logic.js';
import { roleToggle, rolePanel } from './harness-views.js';

const node = (tag, className, value = '') => {
  const element = document.createElement(tag);
  element.className = className;
  element.textContent = value;
  return element;
};
const starGlyph = () => {
  const star = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  star.setAttribute('class', 'harness-model-star'); star.setAttribute('viewBox', '0 0 24 24'); star.setAttribute('aria-hidden', 'true');
  const shape = document.createElementNS('http://www.w3.org/2000/svg', 'use'); shape.setAttribute('href', '/icons/star.svg#star');
  star.append(shape); return star;
};

// The server refuses a list longer than this, so a longer detected list is cut to it rather than failing the save.
const MODELS_MAX = 64;

const lines = value => value.split('\n').map(row => row.trim()).filter(Boolean);
const pairs = (value, separator) => {
  const result = [];
  for (const row of lines(value)) {
    const at = row.indexOf(separator);
    if (at <= 0 || at === row.length - separator.length) throw new Error(`Use one ${separator} pair per line`);
    result.push([row.slice(0, at).trim(), row.slice(at + separator.length).trim()]);
  }
  return result;
};

const extraField = (form, label, value = '') => {
  const wrapper = node('label', 'machine-harness-path-label', label);
  const input = document.createElement('textarea'); input.value = value; input.spellcheck = false;
  wrapper.append(input); form.append(wrapper); return input;
};

const FAMILIES = [['claude', 'Claude Code'], ['codex', 'Codex'], ['pi', 'Pi'], ['opencode', 'OpenCode']];
const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$/;
const familyName = family => FAMILIES.find(([key]) => key === family)?.[1] || family;
const button = (label, className = 'btn', type = 'button') => { const item = node('button', className, label); item.type = type; return item; };
const detectionFailure = (problem, containerized) => {
  const detail = problem.message || "Couldn't detect the models.";
  const noModels = detail.match(/(?:pi|opencode|codex) reports no models — is it logged in\? Run \/login in it/);
  const folderMissing = /(?:not mounted|not visible|does not exist or is not visible)/i.test(detail);
  const summary = containerized && problem.code === 'detect_failed' && problem.detectDir && folderMissing
    ? `Detection blocked — please mount ${problem.detectDir} to your harness Docker, or pick a mounted folder below.`
    : noModels ? noModels[0] : containerized ? detail : problem.code === 'detect_failed' ? 'Model detection failed. Check the harness or try another folder.' : detail;
  return { summary, detail: detail === summary ? '' : detail };
};
const showDetectionDetails = (details, detail) => {
  details.hidden = !detail;
  details.querySelector('p').textContent = detail;
  details.open = false;
};
const detectionDetails = () => {
  const details = node('details', 'harness-detect-details'); details.hidden = true;
  details.append(node('summary', '', 'Details'), node('p', ''));
  return details;
};

// Settings → Harnesses: a machine's harnesses (the coding tools installed on it: Claude Code, Codex, Pi, OpenCode) and, under each, the
// models it can start agents with. Nothing is there until the owner adds it, by hand or by ticking what auto-detect found. Only the owner
// reads or edits it (launchers run code on the host).
//
// With `embedded` it is the same component placed in another page (the first-run onboarding) instead of the Settings dialog: no tab, shown
// at once inside `host`, and its element ids carry a prefix so they never repeat those of the Settings copy.
export class HarnessSettings {
  constructor(dialog, { embedded = false, host = null } = {}) {
    const prefix = this.prefix = embedded ? 'onboarding-' : '';
    this.embedded = embedded;
    // Called whenever the list this component shows has a harness that can start an agent (enabled, with a model): the app then forgets that the
    // person skipped setting one up.
    this.onReady = null;
    this.tab = button('Harnesses', 'row-btn'); this.tab.id = 'settings-harnesses-tab';
    if (!embedded) dialog.querySelector('nav').append(this.tab);
    this.section = node('section', 'settings-content'); this.section.id = `${prefix}settings-harnesses`; this.section.hidden = !embedded;
    const head = node('div', 'harness-head'); head.append(node('h3', '', 'Harnesses'));
    this.roleDefaults = embedded ? null : new RoleDefaults();
    this.roleViews = new Set();
    this.otherTried = ''; // the unknown launcher ids the other machines were last read for, so the same ones are not asked for twice
    this.otherHarnesses = new Map(); // launcher id -> its machine's name, label and models, for role defaults on other machines
    this.drafts = new Map(); // harness id -> { models, dirty }: unsaved model edits, kept while the page redraws
    this.selectedHarness = '';
    this.section.append(head, node('p', 'settings-intro', 'A harness is a coding tool installed on a machine, with the models it can start agents with. Nothing is listed until you add it. Pick a machine, add its harnesses (or auto-detect them), then give each harness its models: exactly one is the default, which an agent falls back to when its own model is removed.'));
    this.select = document.createElement('select'); this.select.id = `${prefix}harness-machine`;
    this.select.onchange = () => { this.drafts.clear(); this.otherTried = ''; this.loadHarnesses(); };
    this.section.append(settingsGroup([settingsRow({ label: 'Machine', desc: 'The supervisor whose harnesses and recent changes are shown below.', control: this.select })], 'Harness machine'));
    this.summary = node('p', 'settings-intro'); this.summary.setAttribute('role', 'status');
    this.error = node('p', 'error'); this.error.setAttribute('role', 'alert');
    this.errorDetails = detectionDetails();
    this.toolbar = node('div', 'machine-actions');
    this.addButton = button('Add harness', 'btn btn-primary'); this.addButton.id = `${prefix}harness-add`;
    this.detectButton = button('Auto-detect harnesses'); this.detectButton.id = `${prefix}harness-detect`;
    this.toolbar.append(this.addButton, this.detectButton);
    this.panel = node('div'); this.panel.id = `${prefix}harness-panel`;
    this.list = node('div', 'harness-list'); this.list.id = `${prefix}harnesses-list`;
    this.rolesHost = node('div', 'harness-roles-host'); this.rolesHost.id = `${prefix}harness-roles-host`;
    this.roleError = node('p', 'error'); this.roleError.setAttribute('role', 'alert');
    this.activityHeading = node('h4', 'machine-activity-heading', 'Activity');
    this.activity = node('div'); this.activity.id = `${prefix}machine-activity`;
    this.section.append(this.summary, this.error, this.errorDetails, this.toolbar, this.panel, this.list, this.rolesHost, this.roleError, this.activityHeading, this.activity);
    (embedded ? host : dialog).append(this.section);
    this.sequence = 0;
    this.openLaunchers = new Set();
    // Why the last model detection of a harness failed (by harness id), so its models panel shows the reason and the folder retry.
    this.modelFailures = new Map();
    this.wanted = ''; // a machine to open on, set by whoever sent the person here
    this.addButton.onclick = () => this.showPanel(this.addForm());
    this.detectButton.onclick = () => this.runDetect();
    // Role defaults saved from Settings → Roles show here without reopening the page.
    document.addEventListener(ROLE_DEFAULTS_CHANGED, event => {
      if (!this.roleDefaults || event.detail?.source === this.roleDefaults || this.section.hidden) return;
      this.loadRoles().then(() => { this.refreshRoleViews(); return this.loadOtherHarnesses(this.sequence); });
    });
  }

  // The global roles with their default agents.
  async loadRoles() {
    if (!this.roleDefaults) return;
    try { await this.roleDefaults.load(); } catch (error) { this.roleDefaults.roles = []; this.roleError.textContent = error.message || 'Could not load the roles'; }
  }
  // A role default names its harness and model by ids. On the machine shown they are read from the harnesses already loaded; defaults on
  // other machines need those machines' harnesses once, and only when some role has such a default.
  async loadOtherHarnesses(sequence) {
    const known = id => this.rows?.some(row => row.id === id) || this.otherHarnesses.has(id);
    const missing = [...new Set(this.roles().flatMap(role => (role.default_agents || []).map(item => item.launcher_id)))].filter(id => !known(id)).sort();
    // Other machines are read when some role default names a harness not seen yet, and not again for the same unknown ones: a redraw
    // repeats nothing, a harness that appears later (or a machine change) is looked for once more.
    const key = missing.join(',');
    if (!missing.length || key === this.otherTried) return;
    this.otherTried = key;
    const others = (this.machines || []).filter(machine => machine.id !== this.select.value);
    await Promise.all(others.map(async machine => {
      try {
        const response = await request(`/api/machines/${encodeURIComponent(machine.id)}/launchers`);
        for (const row of response?.launchers || []) this.otherHarnesses.set(row.config.id, { machine: machine.name || machine.id, label: row.config.label || row.config.name, models: row.models || [] });
      } catch { /* those entries are then named by their ids */ }
    }));
    if (sequence === this.sequence) this.renderRolePanel();
  }
  labelOfDefault(item) {
    const here = this.rows?.find(row => row.id === item.launcher_id);
    const machine = this.machines?.find(entry => entry.id === this.select.value);
    const other = this.otherHarnesses.get(item.launcher_id);
    const harness = here || other, owner = here ? (machine?.name || machine?.id) : other?.machine;
    if (!harness) return `${item.launcher_id} · ${item.model}`;
    const model = (harness.models || []).find(entry => entry.slug === item.model);
    return [owner, harness.label || harness.name, model?.label || item.model].filter(Boolean).join(' · ');
  }
  roles() { return this.roleDefaults?.roles || []; }
  refreshRoleViews() { for (const update of this.roleViews) update(); this.renderRolePanel(); }
  renderRolePanel() {
    if (!this.roleDefaults || !this.rows) { this.rolesHost.replaceChildren(); return; }
    this.rolesHost.replaceChildren(rolePanel({ roles: this.roles(), labelOf: item => this.labelOfDefault(item), store: this.roleDefaults,
      report: text => { this.roleError.textContent = text; }, refresh: () => this.refreshRoleViews() }));
  }

  // Forgets what was shown (the embedded copy, when its page is left), so no element of it is left in the page.
  clear() {
    this.sequence++;
    this.list.replaceChildren(); this.rolesHost.replaceChildren(); this.activity.replaceChildren(); this.panel.replaceChildren(); this.summary.textContent = ''; this.error.textContent = ''; this.roleError.textContent = '';
    this.roleViews.clear(); this.syncMaster = null; this.drafts.clear();
  }

  showPanel(content) { this.panel.replaceChildren(...(content ? [content] : [])); content?.querySelector('input,select,button')?.focus(); }

  async open() {
    const sequence = ++this.sequence;
    this.error.textContent = '';
    showDetectionDetails(this.errorDetails, '');
    this.summary.textContent = 'Loading machines…';
    this.list.replaceChildren();
    this.rolesHost.replaceChildren();
    this.roleError.textContent = '';
    this.activity.replaceChildren();
    this.showPanel(null);
    this.drafts.clear(); this.otherHarnesses.clear(); this.otherTried = '';
    try {
      const [machines] = await Promise.all([request('/api/machines'), this.loadRoles()]);
      if (sequence !== this.sequence) return;
      const rows = Array.isArray(machines) ? machines : machines?.machines || [];
      const selected = this.select.value;
      this.select.replaceChildren(...rows.map(machine => {
        const option = document.createElement('option');
        option.value = machine.id;
        option.textContent = machine.name || machine.id;
        return option;
      }));
      if (rows.some(machine => machine.id === selected)) this.select.value = selected;
      if (this.wanted && rows.some(machine => machine.id === this.wanted)) this.select.value = this.wanted;
      this.wanted = '';
      this.machines = rows;
      this.toolbar.hidden = !rows.length;
      if (!rows.length) {
        this.summary.textContent = 'No machines are registered.';
        return;
      }
      await this.loadHarnesses();
    } catch (error) {
      if (sequence === this.sequence) {
        this.summary.textContent = '';
        this.error.textContent = error.message || 'Could not load machines';
      }
    }
  }

  async loadHarnesses() {
    const machineID = this.select.value;
    const sequence = ++this.sequence;
    this.error.textContent = '';
    showDetectionDetails(this.errorDetails, '');
    this.list.replaceChildren();
    this.activity.replaceChildren();
    if (!machineID) return;
    const machine = this.machines?.find(item => item.id === machineID);
    this.summary.textContent = machine?.approved === false ? 'Approval pending' :
      machine?.last_seen ? `Last seen ${new Date(machine.last_seen).toLocaleString()}` : 'Never connected';
    try {
      const response = await request(`/api/machines/${encodeURIComponent(machineID)}/launchers`);
      if (sequence !== this.sequence || this.select.value !== machineID) return;
      this.rows = (response?.launchers || []).map(row => ({ ...row.config, variant: row.config.name,
        launcher_path: row.resolved_command, version: row.version, available: row.available,
        unavailable_reason: row.unavailable_reason, active_agents: row.active_agents || 0, models: row.models || [], models_rev: row.models_rev || '' }));
      this.renderRows(machineID);
      this.noteReady();
      if (!this.rows.length) this.list.append(node('p', 'settings-intro', 'No harnesses on this machine yet. Add one, or auto-detect what is installed.'));
      this.renderActivity(response?.activity || []);
    } catch (error) {
      if (sequence === this.sequence) this.error.textContent = error.message || 'Could not load harnesses';
    }
  }

  // Lays the machine's harnesses out as a list beside the detail of the chosen one.
  renderRows(machineID) {
    this.roleViews.clear(); this.syncMaster = null;
    const cards = this.rows.map(harness => this.harnessCard(machineID, harness));
    this.list.replaceChildren(...(cards.length ? [this.splitView(cards)] : []));
    this.renderRolePanel();
    this.loadOtherHarnesses(this.sequence);
  }

  // A vertical tab list of the harnesses, and the card of the chosen one beside it.
  splitView(cards) {
    const wrap = node('div', 'harness-split'), master = node('div', 'harness-master'), detail = node('div', 'harness-detail');
    master.setAttribute('role', 'tablist'); master.setAttribute('aria-orientation', 'vertical'); master.setAttribute('aria-label', 'Harnesses');
    if (!this.rows.some(row => row.id === this.selectedHarness)) this.selectedHarness = this.rows[0].id;
    const tabs = this.rows.map((harness, index) => {
      const tab = node('button', 'row-btn harness-master-item'); tab.type = 'button'; tab.id = `${this.prefix}harness-tab-${harness.id}`;
      tab.setAttribute('role', 'tab'); tab.setAttribute('aria-controls', cards[index].id); tab.dataset.harnessId = harness.id;
      cards[index].setAttribute('role', 'tabpanel'); cards[index].setAttribute('aria-labelledby', tab.id);
      return tab;
    });
    const paint = () => tabs.forEach((tab, index) => {
      const harness = this.rows[index], on = harness.id === this.selectedHarness;
      const status = !harness.enabled ? 'Disabled' : harness.available ? 'Available' : 'Unavailable';
      tab.replaceChildren(node('span', `harness-master-dot${status === 'Available' ? ' is-available' : ''}`), node('span', 'harness-master-name', harness.label || `${familyName(harness.tool)} · ${harness.variant}`),
        node('span', 'harness-master-count', `${(harness.models || []).length}`));
      tab.title = `${harness.label || harness.name}: ${status}, ${(harness.models || []).length} models`;
      tab.setAttribute('aria-selected', String(on)); tab.tabIndex = on ? 0 : -1; cards[index].hidden = !on;
    });
    tabs.forEach((tab, index) => {
      tab.onclick = () => { this.selectedHarness = this.rows[index].id; paint(); };
      tab.onkeydown = event => {
        const to = { ArrowDown: (index + 1) % tabs.length, ArrowUp: (index + tabs.length - 1) % tabs.length, Home: 0, End: tabs.length - 1 }[event.key];
        if (to === undefined) return;
        event.preventDefault(); tabs[to].focus(); tabs[to].click();
      };
    });
    this.syncMaster = paint; paint();
    master.append(...tabs); detail.append(...cards); wrap.append(master, detail);
    return wrap;
  }

  noteReady() { if (this.rows?.some(row => launcherReady(row.enabled, row.models))) this.onReady?.(); }

  // Re-reads the machine's launchers and the activity list, and refreshes one card's summary from them without touching its form.
  async refreshCard(machineID, harness, summary, setStatus) {
    const response = await request(`/api/machines/${encodeURIComponent(machineID)}/launchers`);
    if (this.select.value !== machineID) return;
    const row = (response?.launchers || []).find(item => item.config.id === harness.id);
    if (row) {
      Object.assign(harness, row.config, { variant: row.config.name, launcher_path: row.resolved_command, version: row.version, available: row.available, unavailable_reason: row.unavailable_reason, active_agents: row.active_agents || 0 });
      setStatus(!harness.enabled ? 'Disabled' : harness.available ? 'Available' : 'Unavailable');
      summary.version.textContent = `${harness.version ? `Version ${harness.version}` : 'Version unknown'} · ${harness.active_agents || 0} active agents`;
      summary.path.textContent = `Command: ${harness.launcher_path || harness.command}`;
      let reason = summary.info.querySelector('.machine-harness-reason');
      if (harness.unavailable_reason) { if (!reason) { reason = node('span', 'settings-desc machine-harness-reason'); summary.version.after(reason); } reason.textContent = harness.unavailable_reason; } else reason?.remove();
    }
    this.renderActivity(response?.activity || []);
    this.syncMaster?.();
    this.noteReady(); // the Enabled switch lands here: turning a harness on can make one ready
  }

  // A harness is one summary row: its label and status, a one-line summary, the Enabled switch and Edit. Everything that rarely
  // changes (executable, arguments, environment, container and path settings) sits behind Edit; its models are listed below it.
  harnessCard(machineID, harness) {
    const card = node('article', `machine-harness settings-group`); card.id = `${this.prefix}harness-card-${harness.id}`; card.dataset.harnessId = harness.id;
    const harnessLabel = harness.label || `${familyName(harness.tool)}${harness.containerized ? ' (Docker)' : ''}`;
    card.dataset.tool = harness.tool;
    card.dataset.variant = harness.variant;
    const status = !harness.enabled ? 'Disabled' : harness.available ? 'Available' : 'Unavailable';
    const row = node('div', 'settings-row machine-harness-summary');
    const info = node('div', 'settings-row-info');
    const title = node('div', 'machine-harness-title');
    const statusNode = node('span', `machine-harness-status ${status === 'Available' ? 'is-available' : ''}`, status);
    title.append(node('strong', 'settings-label', harness.label || `${familyName(harness.tool)} · ${harness.variant}`), statusNode);
    const version = harness.version ? `Version ${harness.version}` : 'Version unknown';
    info.append(title, node('span', 'settings-desc machine-harness-meta', `${version} · ${familyName(harness.tool)} · ${harness.active_agents || 0} active agents`));
    if (harness.unavailable_reason) info.append(node('span', 'settings-desc machine-harness-reason', harness.unavailable_reason));
    info.append(node('span', 'settings-desc machine-harness-path', `Command: ${harness.launcher_path || harness.command}`));
    const control = node('div', 'settings-control machine-harness-controls');
    const enabledLabel = node('label', 'machine-harness-enabled');
    const enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.name = 'enabled'; enabled.checked = !!harness.enabled;
    enabled.setAttribute('aria-label', `Enable ${harness.label || harness.tool}`);
    enabledLabel.append(enabled, document.createTextNode('Enabled'));
    const edit = iconButton('edit', `Edit ${harnessLabel}`, { className: 'machine-harness-edit' }); edit.setAttribute('aria-expanded', 'false');
    control.append(enabledLabel, edit);
    row.append(info, control);
    const summary = { title, version: info.querySelector('.machine-harness-meta'), path: info.querySelector('.machine-harness-path'), info };

    const form = node('form', 'machine-harness-form');
    form.id = `${this.prefix}launcher-form-${harness.id}`; form.hidden = true; edit.setAttribute('aria-controls', form.id);
    const labelField = node('label', 'machine-harness-path-label', 'Label');
    const labelInput = document.createElement('input'); labelInput.type = 'text'; labelInput.name = 'label'; labelInput.value = harness.label || ''; labelInput.maxLength = 80; labelInput.autocomplete = 'off';
    labelField.append(labelInput); form.append(labelField);
    const pathLabel = node('label', 'machine-harness-path-label', 'Executable name or absolute path');
    const path = document.createElement('input'); path.type = 'text'; path.name = 'command';
    path.value = harness.command || '';
    path.placeholder = harness.launcher_path || 'claude';
    path.autocomplete = 'off'; path.spellcheck = false;
    pathLabel.append(path); form.append(pathLabel);
    form.append(node('p', 'machine-harness-help', 'The supervisor finds executable names on this machine. Arguments run without a shell.'));
    const args = extraField(form, 'Extra arguments, one per line', (harness.args || []).join('\n'));
    const refs = extraField(form, 'Environment references, TARGET=SOURCE per line', Object.entries(harness.env_refs || {}).map(([target, source]) => `${target}=${source}`).join('\n'));
    const containerLabel = node('label', 'machine-harness-enabled');
    const container = document.createElement('input'); container.type = 'checkbox'; container.checked = !!harness.containerized;
    containerLabel.append(container, document.createTextNode('Containerized')); form.append(containerLabel);
    const urlLabel = node('label', 'machine-harness-path-label', 'Agent-reachable server URL (optional)');
    const serverURL = document.createElement('input'); serverURL.type = 'text'; serverURL.value = harness.agent_server_url || '';
    urlLabel.append(serverURL); form.append(urlLabel);
    const mappings = extraField(form, 'Path mappings, host → visible per line', (harness.path_maps || []).map(map => `${map.host} → ${map.visible}`).join('\n'));
    const roots = extraField(form, 'Visible host roots, one per line', (harness.visible_roots || []).join('\n'));
    const toolboxLabel = node('label', 'machine-harness-enabled');
    const toolbox = document.createElement('input'); toolbox.type = 'checkbox'; toolbox.checked = !!harness.supports_toolbox;
    toolboxLabel.append(toolbox, document.createTextNode('Supports agent toolbox')); form.append(toolboxLabel);
    const actions = node('div', 'machine-harness-actions');
    const save = node('button', 'btn btn-primary', 'Save'); save.type = 'submit';
    const error = node('p', 'error'); error.setAttribute('role', 'alert');
    actions.append(save);
    {
      const remove = iconButton('trash', `Remove ${harnessLabel}`, { danger: true, className: 'machine-harness-remove' });
      remove.onclick = async () => {
        if (!await confirmDialog(`Remove ${harness.label || harness.name}? Its models go with it.`, { title: 'Remove harness', confirm: 'Remove', danger: true })) return;
        try {
          await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harness.id)}`, { method: 'DELETE' });
          this.drafts.delete(harness.id);
          await this.loadHarnesses();
        } catch (problem) { error.textContent = problem.message || 'Could not remove launcher'; }
      };
      actions.append(remove);
    }
    form.append(actions, error);
    const setOpen = open => { form.hidden = !open; edit.setAttribute('aria-expanded', String(open)); setGlyph(edit, open ? 'close' : 'edit', `${open ? 'Close settings for' : 'Edit'} ${harnessLabel}`); if (open) this.openLaunchers.add(harness.id); else this.openLaunchers.delete(harness.id); };
    edit.onclick = () => setOpen(form.hidden);
    if (this.openLaunchers.has(harness.id)) setOpen(true);
    form.onsubmit = async event => {
      event.preventDefault();
      error.textContent = '';
      save.disabled = true; enabled.disabled = true;
      try {
        const { variant, launcher_path, version, available, unavailable_reason, active_agents, ...config } = harness;
        const { models, models_rev, ...definition } = config;
        await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harness.id)}`, {
          method: 'PUT', body: { ...definition, label: labelInput.value.trim(), enabled: enabled.checked, command: path.value.trim(),
            args: lines(args.value), env_refs: Object.fromEntries(pairs(refs.value, '=')),
            containerized: container.checked, agent_server_url: serverURL.value.trim(),
            path_maps: pairs(mappings.value, '→').map(([host, visible]) => ({ host, visible })),
            visible_roots: lines(roots.value), supports_toolbox: toolbox.checked },
        });
        await this.loadHarnesses();
      } catch (problem) {
        error.textContent = problem.message || 'Could not save harness settings';
        if (form.hidden) setOpen(true);
        save.disabled = false; enabled.disabled = false;
      }
    };
    // The Enabled switch saves on its own, like every other settings switch, and only itself: it sends the persisted
    // definition with the new Enabled value, so edits still open in the form stay unsaved until Save. The card is
    // updated in place for the same reason; nothing the person typed is thrown away.
    enabled.onchange = async () => {
      error.textContent = ''; enabled.disabled = true;
      try {
        const { variant, launcher_path, version, available, unavailable_reason, active_agents, models, models_rev, ...config } = harness;
        await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harness.id)}`, { method: 'PUT', body: { ...config, enabled: enabled.checked } });
        await this.refreshCard(machineID, harness, summary, status => { statusNode.textContent = status; statusNode.classList.toggle('is-available', status === 'Available'); });
      } catch (problem) {
        enabled.checked = !enabled.checked; error.textContent = problem.message || 'Could not save harness settings'; if (form.hidden) setOpen(true);
      } finally { enabled.disabled = false; }
    };
    card.append(row, form, this.modelsPanel(machineID, harness));
    return card;
  }

  // Add a harness by hand: the family, an executable name or absolute path, and a label (the family's name by default).
  addForm() {
    const machineID = this.select.value;
    const form = node('form', 'machine-harness-form harness-add-form'); form.id = `${this.prefix}harness-add-form`;
    const family = document.createElement('select'); family.name = 'family';
    family.replaceChildren(...FAMILIES.map(([value, name]) => { const option = document.createElement('option'); option.value = value; option.textContent = name; return option; }));
    const field = (text, input) => { const label = node('label', 'machine-harness-path-label', text); label.append(input); form.append(label); return input; };
    field('Family', family);
    const label = field('Label (optional)', Object.assign(document.createElement('input'), { type: 'text', name: 'label', maxLength: 80, autocomplete: 'off' }));
    const command = field('Executable name or absolute path', Object.assign(document.createElement('input'), { type: 'text', name: 'command', autocomplete: 'off', spellcheck: false, required: true }));
    const dockerLabel = node('label', 'machine-harness-enabled'); const docker = document.createElement('input'); docker.type = 'checkbox'; docker.name = 'containerized';
    dockerLabel.append(docker, document.createTextNode('A Docker wrapper')); form.append(dockerLabel);
    const actions = node('div', 'machine-harness-actions'); const save = button('Add harness', 'btn btn-primary', 'submit'); const cancel = button('Cancel');
    const error = node('p', 'error'); error.setAttribute('role', 'alert'); actions.append(cancel, save); form.append(actions, error);
    cancel.onclick = () => this.showPanel(null);
    form.onsubmit = async event => {
      event.preventDefault(); error.textContent = ''; save.disabled = true;
      try {
        await this.createHarness(machineID, { tool: family.value, label: label.value.trim(), command: command.value.trim(), containerized: docker.checked });
        this.showPanel(null); await this.loadHarnesses();
      } catch (problem) { error.textContent = problem.message || 'Could not add the harness'; save.disabled = false; }
    };
    return form;
  }

  createHarness(machineID, { tool, label, command, containerized }) {
    return request(`/api/machines/${encodeURIComponent(machineID)}/launchers`, { method: 'POST', body: {
      tool, label, command, args: [], env_refs: {}, enabled: true, containerized, path_maps: [], visible_roots: [], supports_toolbox: !(tool === 'pi' && containerized),
    } });
  }

  // Auto-detect asks the machine's supervisor what is installed. It only reports: nothing is added until it is ticked and confirmed.
  async runDetect() {
    const machineID = this.select.value;
    this.error.textContent = ''; showDetectionDetails(this.errorDetails, ''); this.detectButton.disabled = true; this.detectButton.textContent = 'Detecting…';
    try {
      const found = await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/detect`, { method: 'POST', body: {} });
      this.showPanel(this.detectPanel(machineID, found.candidates || []));
    } catch (problem) { this.error.textContent = problem.message || "Couldn't detect harnesses"; }
    finally { this.detectButton.disabled = false; this.detectButton.textContent = 'Auto-detect harnesses'; }
  }

  // Detect a new harness's models and save them (at most MODELS_MAX), the first as the default. Resolves to whether it ended up with models,
  // how many were left out to fit the limit, and why detection failed when it did.
  async fillModels(machineID, harnessID, containerized = false) {
    if (!harnessID) return { saved: false, cut: 0 };
    const base = `/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harnessID)}/models`;
    try {
      let found;
      try { found = await request(`${base}/detect`, { method: 'POST', body: {} }); } catch (problem) {
        const failure = detectionFailure(problem, containerized);
        (this.modelFailures ||= new Map()).set(harnessID, failure); return { saved: false, cut: 0, reason: failure.summary, detail: failure.detail };
      }
      const all = (found.candidates || []).filter(candidate => !candidate.already_listed);
      const models = all.slice(0, MODELS_MAX).map((candidate, index) => ({ slug: candidate.slug, label: candidate.label || candidate.slug, note: candidate.note || '', default: index === 0 }));
      if (!models.length) return { saved: false, cut: 0 };
      await request(base, { method: 'PUT', body: { models } });
      return { saved: true, cut: all.length - models.length, kept: models.length };
    } catch (problem) { return { saved: false, cut: 0, reason: problem.message || '' }; }
  }

  detectPanel(machineID, candidates) {
    const panel = node('form', 'machine-harness-form harness-detect-panel'); panel.id = `${this.prefix}harness-detect-panel`;
    panel.append(node('strong', '', candidates.length ? 'Found on this machine — tick the ones to add' : 'Nothing found on this machine'));
    if (!candidates.length) panel.append(node('p', 'machine-harness-help', 'No Claude Code, Codex, Pi or OpenCode executable was found. Add one by hand with its path.'));
    const boxes = [];
    for (const candidate of candidates) {
      const row = node('label', 'machine-harness-enabled harness-detect-row'); row.dataset.command = candidate.command;
      const box = document.createElement('input'); box.type = 'checkbox'; box.checked = false; box.disabled = !!candidate.already_added;
      box.setAttribute('aria-label', `Add ${candidate.label} at ${candidate.command}`);
      const text = node('span', '', `${candidate.label} · ${candidate.command}${candidate.version ? ` · ${candidate.version}` : ''}${candidate.already_added ? ' · already added' : ''}`);
      row.append(box, text); panel.append(row); boxes.push([box, candidate]);
    }
    const actions = node('div', 'machine-harness-actions'); const add = button('Add selected', 'btn btn-primary', 'submit'); const close = button('Close');
    const error = node('p', 'error'); error.setAttribute('role', 'alert');
    if (candidates.length) actions.append(add); actions.append(close); panel.append(actions, error);
    close.onclick = () => this.showPanel(null);
    panel.onsubmit = async event => {
      event.preventDefault(); error.textContent = ''; showDetectionDetails(this.errorDetails, '');
      const chosen = boxes.filter(([box]) => box.checked).map(([, candidate]) => candidate);
      if (!chosen.length) { error.textContent = 'Tick at least one harness to add.'; return; }
      add.disabled = true;
      try {
        const created = [];
        for (const candidate of chosen) created.push(await this.createHarness(machineID, { tool: candidate.family, label: candidate.label, command: candidate.command, containerized: candidate.containerized }));
        // A harness with no models cannot start an agent, so ask each new one what it offers and keep that list. It is best effort: a
        // harness that cannot answer is left empty with its reason shown, and its models panel shows that reason with a folder to
        // retry in, besides Auto-detect and the by-hand form.
        const filled = await Promise.all(created.map(item => this.fillModels(machineID, item?.config?.id || item?.id, !!item?.config?.containerized)));
        const empty = filled.filter(done => !done.saved).length;
        const cut = filled.find(done => done.cut > 0);
        this.showPanel(null); await this.loadHarnesses();
        const notes = [];
        const reason = filled.find(done => !done.saved && done.reason)?.reason;
        const detail = filled.find(done => !done.saved && done.detail)?.detail;
        if (empty) notes.push(`${empty === 1 ? 'A harness has' : `${empty} harnesses have`} no models yet.${reason ? ` ${reason}` : ''} Retry Auto-detect in its models, in a folder you choose if the harness needs one, or add a model by hand.`);
        if (cut) notes.push(`A harness offered more models than a list can hold, so only the first ${cut.kept} were added.`);
        this.error.textContent = notes.join(' ');
        showDetectionDetails(this.errorDetails, detail);
      } catch (problem) { error.textContent = problem.message || 'Could not add the harnesses'; add.disabled = false; await this.loadHarnesses(); }
    };
    return panel;
  }

  // The models of one harness: an editable list (label, note, which one is the default), added to by hand or from auto-detect, saved as a
  // whole. Slugs are exact model ids, never aliases.
  modelsPanel(machineID, harness) {
    const panel = node('div', 'harness-models'); panel.id = `${this.prefix}harness-models-${harness.id}`;
    // Unsaved edits survive a redraw (saving the harness's settings): a draft with edits lives on the page, keyed by harness,
    // together with the revision of the list it was started from. A harness nobody edited keeps nothing, so every reload shows its stored list.
    const entry = this.drafts.get(harness.id) || { models: (harness.models || []).map(model => ({ ...model })), dirty: false, rev: harness.models_rev || '' };
    const draft = entry.models;
    const rows = node('div', 'harness-model-rows');
    const error = node('p', 'error'); error.setAttribute('role', 'alert');
    const status = node('p', 'settings-desc'); status.setAttribute('role', 'status');
    const save = button('Save models', 'btn btn-primary'); save.disabled = !entry.dirty;
    const mark = () => { entry.dirty = true; this.drafts.set(harness.id, entry); save.disabled = false; status.textContent = ''; };
    // The role defaults of one model, as labelled switches. They save on their own (a role's defaults are the role's, not the harness's),
    // so a model that is not saved yet cannot be ticked.
    const panelViews = new Set();
    this.roleViews.add(() => panelViews.forEach(update => update()));
    const roleControls = model => this.roles().map(role => {
      const live = () => this.roles().find(item => item.id === role.id) || role;
      const control = roleToggle(role, model.label || model.slug, async on => {
        error.textContent = '';
        try { await this.roleDefaults.setModel(live(), harness.id, model.slug, on); } catch (problem) { error.textContent = problem.message || 'Could not save the role'; }
        this.refreshRoleViews();
      });
      const update = () => {
        const current = live(), at = positionOf(current, harness.id, model.slug), stored = (harness.models || []).some(item => item.slug === model.slug);
        control.update({ on: at >= 0, position: at + 1, total: (current.default_agents || []).length,
          disabled: !stored || (at < 0 && !canAddDefault(current)), reason: stored ? (at < 0 && !canAddDefault(current) ? `${role.label} has the most defaults it can hold` : '') : 'Save the models first' });
      };
      panelViews.add(update); update();
      return control.element;
    });
    const draw = () => {
      rows.replaceChildren(); panelViews.clear();
      if (!draft.length) rows.append(node('p', 'settings-intro', 'No models yet: this harness cannot start an agent until one is added.'));
      draft.forEach((model, index) => {
        const def = document.createElement('input'); def.type = 'radio'; def.name = `default-${harness.id}`; def.checked = !!model.default;
        def.setAttribute('aria-label', model.default ? `${model.label || model.slug} is the default model` : `Make ${model.label || model.slug} the default`);
        def.onchange = () => {
          draft.forEach(item => { item.default = false; }); model.default = true; mark(); draw();
          [...rows.querySelectorAll('.harness-model-row')].find(item => item.dataset.slug === model.slug)?.querySelector('input[type=radio]')?.focus();
        };
        const defLabel = node('label', `harness-model-default${model.default ? ' is-default' : ''}`);
        defLabel.title = model.default ? 'Default model' : 'Make default';
        defLabel.append(def, starGlyph(), node('span', 'harness-model-default-text', model.default ? 'Default' : ''));
        const slug = node('code', 'harness-model-slug', model.slug);
        const label = document.createElement('input'); label.type = 'text'; label.value = model.label; label.maxLength = 80; label.name = 'label'; label.setAttribute('aria-label', `Label of ${model.slug}`);
        const note = document.createElement('input'); note.type = 'text'; note.value = model.note || ''; note.maxLength = 280; note.name = 'note'; note.placeholder = 'Note (optional)'; note.setAttribute('aria-label', `Note of ${model.slug}`);
        note.title = note.value;
        note.oninput = () => { model.note = note.value; note.title = note.value; mark(); };
        const remove = iconButton('trash', `Remove ${model.slug}`, { danger: true, className: 'harness-model-remove' });
        remove.onclick = () => { draft.splice(index, 1); if (model.default && draft.length) draft[0].default = true; mark(); draw(); };
        // A model row: the controls on top, the roles below as labelled switches.
        const row = node('div', 'harness-model-row harness-detail-row');
        label.oninput = () => { model.label = label.value; mark(); };
        const top = node('div', 'harness-detail-top'); top.append(defLabel, slug, remove);
        const fields = node('div', 'harness-detail-fields'); fields.append(label, note);
        const switches = node('div', 'harness-detail-roles'); switches.setAttribute('role', 'group'); switches.setAttribute('aria-label', `Default for roles: ${model.label || model.slug}`); switches.append(...roleControls(model));
        row.append(top, fields, switches);
        row.dataset.slug = model.slug;
        rows.append(row);
      });
    };
    const addModel = (slug, label = '', note = '') => {
      slug = slug.trim();
      if (!MODEL_ID.test(slug)) throw new Error(`"${slug}" is not a valid model id.`);
      if (harness.tool === 'opencode' && !slug.includes('/')) throw new Error('An OpenCode model id is provider/model.');
      if (draft.some(model => model.slug === slug)) return false;
      draft.push({ slug, label: label.trim() || slug, note: note.trim(), default: draft.length === 0 });
      return true;
    };
    draw();
    // By hand.
    const form = node('form', 'harness-model-add');
    const slugInput = Object.assign(document.createElement('input'), { type: 'text', name: 'slug', required: true, autocomplete: 'off', spellcheck: false, placeholder: harness.tool === 'opencode' ? 'provider/model' : 'model id' });
    slugInput.setAttribute('aria-label', 'Model id');
    const labelInput = Object.assign(document.createElement('input'), { type: 'text', name: 'new-label', maxLength: 80, placeholder: 'Label (optional)' }); labelInput.setAttribute('aria-label', 'New model label');
    const noteInput = Object.assign(document.createElement('input'), { type: 'text', name: 'new-note', maxLength: 280, placeholder: 'Note (optional)' }); noteInput.setAttribute('aria-label', 'New model note');
    const add = button('Add model', 'btn', 'submit');
    form.append(slugInput, labelInput, noteInput, add);
    form.onsubmit = event => {
      event.preventDefault(); error.textContent = '';
      try { addModel(slugInput.value, labelInput.value, noteInput.value); } catch (problem) { error.textContent = problem.message; return; }
      slugInput.value = labelInput.value = noteInput.value = ''; mark(); draw(); slugInput.focus();
    };
    // From the harness itself (or, for Claude, the SDK's published list): candidates to tick, nothing saved until Save models. The command
    // runs in the machine's aimebu config folder; when it fails there, the person may name another folder on that machine to retry in.
    // The folder is sent with that one request and never saved.
    const detect = iconButton('refresh', 'Auto-detect models', { className: 'harness-model-detect' }); const detectBox = node('div', 'harness-detect-models');
    const retryRow = node('div', 'harness-detect-retry'); retryRow.hidden = true;
    const details = detectionDetails();
    const dirLabel = node('label', 'harness-detect-dir', 'Run the command in this folder');
    if (harness.containerized) dirLabel.append(node('span', 'harness-detect-dir-hint', 'or pick a folder that is mounted in the container'));
    const dirInput = Object.assign(document.createElement('input'), { type: 'text', name: 'detect-dir', autocomplete: 'off', spellcheck: false, placeholder: 'Absolute path on that machine' });
    dirLabel.append(dirInput);
    const retry = iconButton('refresh', 'Retry auto-detect in this folder', { className: 'harness-detect-retry-button' });
    retryRow.append(dirLabel, retry);
    const failed = problem => {
      const failure = problem?.summary ? problem : detectionFailure(problem, !!harness.containerized);
      error.textContent = failure.summary;
      showDetectionDetails(details, failure.detail);
      if (harness.tool !== 'claude') retryRow.hidden = false;
    };
    const runDetect = async dir => {
      error.textContent = ''; showDetectionDetails(details, ''); detect.disabled = retry.disabled = true; detect.title = 'Detecting models…'; detect.setAttribute('aria-label', detect.title);
      try {
        const found = await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harness.id)}/models/detect`, { method: 'POST', body: dir ? { dir } : {} });
        this.modelFailures?.delete(harness.id); retryRow.hidden = true;
        detectBox.replaceChildren(this.modelCandidates(found, models => {
          let added = 0;
          for (const model of models) {
            if (draft.length >= MODELS_MAX) break;
            try { if (addModel(model.slug, model.label, model.note)) added++; } catch { /* a candidate the list would refuse is skipped */ }
          }
          if (added) { mark(); draw(); }
          status.textContent = added ? `${added} added to the list${added < models.length && draft.length >= MODELS_MAX ? ` (limit ${MODELS_MAX})` : ''}. Save models to keep them.` : draft.length >= MODELS_MAX ? `The list is full (limit ${MODELS_MAX}).` : 'Nothing new to add.';
          detectBox.replaceChildren();
        }, () => detectBox.replaceChildren()));
      } catch (problem) { failed(problem); }
      finally { detect.disabled = retry.disabled = false; detect.title = 'Auto-detect models'; detect.setAttribute('aria-label', detect.title); }
    };
    detect.onclick = () => runDetect('');
    retry.onclick = () => {
      const dir = dirInput.value.trim();
      if (!dir.startsWith('/')) { error.textContent = 'Enter an absolute folder path on that machine.'; dirInput.focus(); return; }
      runDetect(dir);
    };
    dirInput.onkeydown = event => { if (event.key === 'Enter') { event.preventDefault(); retry.click(); } };
    if (this.modelFailures?.has(harness.id)) failed(this.modelFailures.get(harness.id));
    save.onclick = async () => {
      error.textContent = ''; status.textContent = '';
      if (draft.length && draft.filter(model => model.default).length !== 1) { error.textContent = 'Choose exactly one default model.'; return; }
      save.disabled = true;
      try {
        await request(`/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harness.id)}/models`, { method: 'PUT', body: { models: draft, expected_rev: entry.rev } });
        this.drafts.delete(harness.id);
        await this.loadHarnesses();
      } catch (problem) {
        if (problem.status === 409) {
          // The list changed since this draft was started (another tab, another person): show what is stored now, and say so.
          this.drafts.delete(harness.id);
          await this.loadHarnesses();
          this.error.textContent = `Someone changed the models of ${harness.label || harness.name} in the meantime. The list was reloaded; make your changes again.`;
          return;
        }
        error.textContent = problem.message || 'Could not save the models'; save.disabled = false;
      }
    };
    const actions = node('div', 'machine-harness-actions'); actions.append(detect, save);
    panel.append(node('h4', '', `Models of ${harness.label || harness.name}`), rows, form, actions, error, retryRow, details, detectBox, status);
    return panel;
  }

  // The candidates auto-detect found for one harness, as ticks. Models already listed are shown but not tickable.
  modelCandidates(found, onAdd, onClose) {
    const box = node('form', 'machine-harness-form harness-model-candidates');
    const candidates = found.candidates || [];
    const header = node('div', 'harness-candidate-header');
    const title = node('strong', '', `${candidates.length} found${found.source === 'snapshot' ? ' (built-in list: the published source could not be reached)' : ''} — tick the ones to add`);
    const selectControls = node('div', 'harness-candidate-controls');
    const selectAll = iconButton('select-all', 'Select all available models', { className: 'harness-candidate-select-all' });
    const selectedCount = node('span', 'harness-candidate-count');
    selectControls.append(selectedCount, selectAll); header.append(title, selectControls); box.append(header);
    const list = node('div', 'harness-candidate-list');
    const boxes = [];
    for (const candidate of candidates) {
      const row = node('label', 'machine-harness-enabled harness-detect-row'); row.dataset.slug = candidate.slug;
      const check = document.createElement('input'); check.type = 'checkbox'; check.disabled = !!candidate.already_listed;
      check.setAttribute('aria-label', `Add ${candidate.slug}`);
      row.append(check, node('span', '', `${candidate.label} · ${candidate.slug}${candidate.note ? ` · ${candidate.note}` : ''}${candidate.already_listed ? ' · already listed' : ''}`));
      list.append(row); boxes.push([check, candidate]);
    }
    const available = boxes.filter(([check]) => !check.disabled);
    const updateSelection = () => {
      const selected = available.filter(([check]) => check.checked).length;
      selectedCount.textContent = `${selected}/${available.length} selected`;
      selectAll.setAttribute('aria-pressed', selected === available.length && available.length ? 'true' : selected ? 'mixed' : 'false');
      selectAll.title = selected === available.length && available.length ? 'Clear selection' : 'Select all available models';
      selectAll.setAttribute('aria-label', selectAll.title);
      selectAll.disabled = available.length === 0;
    };
    selectAll.onclick = () => { const checkAll = available.some(([check]) => !check.checked); for (const [check] of available) check.checked = checkAll; updateSelection(); };
    for (const [check] of available) check.onchange = updateSelection;
    updateSelection();
    const actions = node('div', 'machine-harness-actions'); const add = button('Add selected', 'btn btn-primary', 'submit'); const close = button('Close');
    actions.append(add, close); box.append(list, actions);
    close.onclick = onClose;
    box.onsubmit = event => { event.preventDefault(); onAdd(boxes.filter(([check]) => check.checked).map(([, candidate]) => candidate)); };
    return box;
  }

  renderActivity(activity) {
    this.activity.replaceChildren();
    if (!activity.length) {
      this.activity.append(node('p', 'settings-intro', 'No harness changes recorded yet.'));
      return;
    }
    for (const change of activity) {
      const item = node('article', 'machine-activity-item');
      const when = change.created_at ? new Date(change.created_at).toLocaleString() : '';
      item.append(node('strong', '', `Harness ${change.action || 'updated'}`));
      item.append(node('span', '', `${when}${change.actor ? ` · ${change.actor}` : ''}`));
      this.activity.append(item);
    }
  }
}
