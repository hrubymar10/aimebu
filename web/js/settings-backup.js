import { request, currentUser, rights } from './api.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { collectPrefs, applyPrefs, describeSection, SECTION_LABELS } from './settings-backup-format.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// Settings → General → Settings backup (the owner and the admins export, only the owner imports): download the instance's settings as one JSON file, or read such a
// file back in two steps: Preview says what it would change and saves nothing, Apply saves the sections that were ticked.
// What the exported file holds: the owner's carries the quota switcher and limit guard setup too, an admin's does not (the server leaves
// them out, and only the owner can apply a file).
const EXPORT_BASE = 'macros, global roles, the model catalog, message expiry';
const EXPORT_OWNER_DESC = `Download ${EXPORT_BASE}, the quota switcher and limit guard setup and this browser's interface preferences. Credentials and tokens are never included.`;
const EXPORT_ADMIN_DESC = `Download ${EXPORT_BASE} and this browser's interface preferences. Credentials and tokens are never included.`;

export class SettingsBackup {
  constructor() {
    const general = document.querySelector('#settings-general');
    if (!general || general.querySelector('#settings-backup')) return;
    this.section = node('div', 'settings-section'); this.section.id = 'settings-backup'; this.section.hidden = true;
    this.section.append(node('h3', '', 'Settings backup'));
    this.exportButton = node('button', 'btn', 'Export'); this.exportButton.type = 'button'; this.exportButton.id = 'settings-backup-export';
    this.exportButton.onclick = () => this.exportFile();
    this.file = node('input'); this.file.type = 'file'; this.file.id = 'settings-backup-file'; this.file.accept = 'application/json,.json';
    this.file.onchange = () => this.choose();
    this.status = node('p', 'settings-intro'); this.status.id = 'settings-backup-status'; this.status.setAttribute('role', 'status');
    this.error = node('p', 'error'); this.error.id = 'settings-backup-error'; this.error.setAttribute('role', 'alert');
    this.plan = node('div', 'backup-plan'); this.plan.id = 'settings-backup-plan'; this.plan.hidden = true;
    this.section.append(
      settingsGroup([
        (this.exportRow = settingsRow({ label: 'Export settings', desc: EXPORT_OWNER_DESC, control: this.exportButton, id: 'settings-backup-export' })),
        (this.importRow = settingsRow({ label: 'Import settings', desc: 'Choose a file from Export. You see what it would change before anything is saved; matching macros and roles are overwritten, nothing is deleted.', control: this.file, id: 'settings-backup-file' })),
      ], 'Settings backup'),
      this.plan, this.status, this.error);
    general.append(this.section);
    // Whether to show the section depends on who is signed in, so ask each time Settings opens or General is chosen.
    document.querySelector('#settings-general-tab')?.addEventListener('click', () => this.refresh());
    const dialog = document.querySelector('#settings-dialog');
    if (dialog) new MutationObserver(() => { if (dialog.open) this.refresh(); }).observe(dialog, { attributes: true, attributeFilter: ['open'] });
    this.pending = null;
  }

  async refresh() {
    const admin = rights.admin, owner = rights.owner;
    // Export is for the owner and the admins; replacing the instance's settings is the owner's alone.
    this.section.hidden = !admin;
    this.importRow.hidden = !owner;
    // The owner's file carries the quota switcher and limit guard setup; an admin's does not (the server leaves them out).
    this.exportRow.querySelector('.settings-desc').textContent = owner ? EXPORT_OWNER_DESC : EXPORT_ADMIN_DESC;
  }

  async exportFile() {
    this.error.textContent = ''; this.status.textContent = '';
    try {
      const backup = await request('/api/settings-backup');
      const prefs = collectPrefs(localStorage, currentUser);
      if (Object.keys(prefs).length) backup.ui_prefs = prefs;
      const url = URL.createObjectURL(new Blob([JSON.stringify(backup, null, 2)], { type: 'application/json' }));
      const link = node('a'); link.href = url; link.download = 'aimebu-settings-backup.json'; document.body.append(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      this.status.textContent = 'Settings exported.';
    } catch (error) { this.error.textContent = error.message; }
  }

  async choose() {
    this.error.textContent = ''; this.status.textContent = ''; this.plan.hidden = true; this.pending = null;
    const file = this.file.files[0]; if (!file) return;
    let backup;
    try { backup = JSON.parse(await file.text()); } catch { this.error.textContent = 'That file is not valid JSON.'; return; }
    if (!backup || typeof backup !== 'object' || Array.isArray(backup)) { this.error.textContent = 'That file is not a settings backup.'; return; }
    try {
      const preview = await request('/api/settings-backup/preview', { method: 'POST', body: { backup } });
      this.pending = { backup, preview }; this.renderPlan();
    } catch (error) { this.error.textContent = error.message; }
  }

  renderPlan() {
    const { preview } = this.pending;
    this.plan.replaceChildren(); this.plan.hidden = false;
    const list = node('div', 'backup-sections');
    let actionable = 0;
    for (const section of preview.sections) {
      const row = node('label', 'backup-section');
      const box = node('input'); box.type = 'checkbox'; box.value = section.section; box.checked = section.new + section.overwrite > 0; box.disabled = !(section.new + section.overwrite);
      if (box.checked) actionable++;
      const text = node('span', '', describeSection(section)); row.append(box, text);
      list.append(row);
      for (const issue of section.invalid || []) list.append(node('p', 'backup-issue', `${SECTION_LABELS[section.section] || section.section}: ${issue.key} skipped (${issue.reason})`));
    }
    this.plan.append(list);
    const retired = { spawn_models: 'Model lists are per harness now (Settings → Harnesses), so the model catalog in this file is not imported.' };
    for (const name of (preview.ignored || []).filter(name => retired[name])) this.plan.append(node('p', 'settings-intro', retired[name]));
    const unknown = (preview.ignored || []).filter(name => !retired[name]);
    if (unknown.length) this.plan.append(node('p', 'settings-intro', `Not understood by this version and ignored: ${unknown.join(', ')}.`));
    if (!preview.sections.length) this.plan.append(node('p', 'settings-intro', 'The file has nothing this version can import.'));
    const actions = node('div', 'dialog-actions');
    this.apply = node('button', 'btn btn-primary', 'Apply import'); this.apply.type = 'button'; this.apply.id = 'settings-backup-apply'; this.apply.disabled = !actionable;
    this.apply.onclick = () => this.applyChosen();
    const cancel = node('button', 'btn', 'Cancel'); cancel.type = 'button'; cancel.onclick = () => this.clear();
    actions.append(this.apply, cancel); this.plan.append(actions);
  }

  clear() { this.pending = null; this.plan.hidden = true; this.plan.replaceChildren(); this.file.value = ''; }

  async applyChosen() {
    if (!this.pending) return;
    const sections = [...this.plan.querySelectorAll('.backup-section input:checked')].map(box => box.value);
    if (!sections.length) { this.error.textContent = 'Tick at least one section.'; return; }
    this.error.textContent = ''; this.apply.disabled = true;
    const { backup, preview } = this.pending;
    try {
      // The browser's own preferences are applied here; the server is only asked when a server section is ticked (an empty list from
      // here would otherwise be read as a choice of nothing, and with nothing ticked there is nothing to ask).
      const serverSections = sections.filter(name => name !== 'ui_prefs');
      const result = serverSections.length ? await request('/api/settings-backup/apply', { method: 'POST', body: { backup, digest: preview.digest, sections: serverSections } }) : { sections: [], complete: true };
      let reload = false;
      if (sections.includes('ui_prefs') && backup.ui_prefs) { applyPrefs(localStorage, currentUser, backup.ui_prefs); reload = true; }
      const applied = result.sections.map(section => `${SECTION_LABELS[section.section] || section.section}: ${section.applied}`);
      if (reload) applied.push(`${SECTION_LABELS.ui_prefs}: applied after a reload`);
      this.clear();
      this.status.textContent = `${result.complete ? 'Imported' : 'Imported with problems'}. ${applied.join('; ')}.`;
      if (result.failed?.length) this.error.textContent = `Not saved: ${result.failed.map(item => `${item.key} (${item.error})`).join('; ')}.`;
      document.dispatchEvent(new CustomEvent('aimebu:settings-imported'));
    } catch (error) {
      this.error.textContent = error.code === 'preview_stale' ? 'The settings changed since the preview. Choose the file again to see the new plan.' : error.message;
      if (error.code === 'preview_stale') this.clear(); else this.apply.disabled = false;
    }
  }
}
