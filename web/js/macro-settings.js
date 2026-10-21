import { request, rights } from './api.js';
import { iconButton } from './icon-button.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { copyText } from './markdown.js';
import { parseMacroImport, describeMerge, mergeMacros, filterMacroKeys } from './macro-import.js';

function node(tag, text = '') { const item = document.createElement(tag); item.textContent = text; return item; }

export class MacroSettings {
  constructor(dialog) {
    this.tab = node('button', 'Macros'); this.tab.id = 'settings-macros-tab'; this.tab.type = 'button'; this.tab.className = 'row-btn';
    dialog.querySelector('nav').append(this.tab);
    this.section = node('section'); this.section.id = 'settings-macros'; this.section.className = 'settings-content'; this.section.hidden = true;
    this.section.append(node('h3', 'Macros'), (() => { const intro = node('p', 'Type <KEY> anywhere in a draft, then select a suggestion to insert its editable text.'); intro.className = 'settings-intro'; return intro; })());
    this.filter = '';
    this.toolbar = node('div'); this.toolbar.className = 'macro-toolbar';
    this.search = node('input'); this.search.type = 'search'; this.search.placeholder = 'Search macros'; this.search.setAttribute('aria-label', 'Search macros');
    this.search.oninput = () => { this.filter = this.search.value; this.render(); };
    this.copyAll = node('button', 'Copy all'); this.copyAll.type = 'button'; this.copyAll.id = 'macros-copy'; this.copyAll.className = 'btn'; this.copyAll.title = 'Copy every macro as JSON';
    this.copyAll.onclick = () => this.copyJSON();
    this.importOpen = node('button', 'Import…'); this.importOpen.type = 'button'; this.importOpen.id = 'macros-import-open'; this.importOpen.className = 'btn'; this.importOpen.title = 'Paste macro JSON to add or update macros'; this.importOpen.hidden = true;
    this.importOpen.onclick = () => this.toggleImport(true);
    const searchRow = settingsRow({ label: 'Search', desc: 'Show only the macros whose key or text contains this.', control: this.search, id: 'macros-search' });
    const toolbarActions = node('div'); toolbarActions.className = 'dialog-actions'; toolbarActions.append(this.copyAll, this.importOpen);
    this.toolbar.append(settingsGroup([searchRow], 'Find macros'), toolbarActions);
    this.importPanel = node('div'); this.importPanel.id = 'macros-import'; this.importPanel.className = 'macro-import'; this.importPanel.hidden = true;
    this.importText = node('textarea'); this.importText.rows = 6; this.importText.placeholder = '{"greeting": "Hello"}'; this.importText.setAttribute('aria-label', 'Macro JSON to import'); this.importText.spellcheck = false;
    this.importText.oninput = () => { this.pending = null; this.importApply.disabled = true; this.importSummary.textContent = ''; };
    this.importSummary = node('p'); this.importSummary.id = 'macros-import-summary'; this.importSummary.className = 'macro-import-summary'; this.importSummary.setAttribute('role', 'status');
    this.importPreview = node('button', 'Preview'); this.importPreview.type = 'button'; this.importPreview.id = 'macros-import-preview'; this.importPreview.className = 'btn';
    this.importPreview.onclick = () => this.previewImport();
    this.importApply = node('button', 'Apply import'); this.importApply.type = 'button'; this.importApply.id = 'macros-import-apply'; this.importApply.className = 'btn btn-primary'; this.importApply.disabled = true;
    this.importApply.onclick = () => this.applyImport();
    this.importCancel = node('button', 'Cancel'); this.importCancel.type = 'button'; this.importCancel.className = 'btn';
    this.importCancel.onclick = () => this.toggleImport(false);
    const importActions = node('div'); importActions.className = 'dialog-actions'; importActions.append(this.importPreview, this.importApply, this.importCancel);
    const importRow = settingsRow({ label: 'Macro JSON', desc: 'An object of macro keys and bodies, such as the output of Copy all. A matching key is overwritten.', control: this.importText, id: 'macros-import-text', className: 'settings-row-stack' });
    this.importPanel.append(settingsGroup([importRow], 'Import macros'), this.importSummary, importActions);
    this.pending = null;
    this.form = node('form'); this.form.id = 'macro-editor';
    this.key = node('input'); this.key.name = 'key'; this.key.required = true; this.key.placeholder = 'greeting';
    this.body = node('textarea'); this.body.name = 'body'; this.body.rows = 3;
    const fields = settingsGroup([
      settingsRow({ label: 'Key', desc: 'Typed between angle brackets, as <KEY>, to insert the macro. Lowercase letters, digits, _ or -, starting with a letter.', control: this.key, id: 'macro-key' }),
      settingsRow({ label: 'Body', desc: 'Text inserted into the draft when you pick the macro. It may contain other <KEY> references, which expand up to 8 levels. Up to 16 KiB.', control: this.body, id: 'macro-body', className: 'settings-row-stack' }),
    ], 'Macro');
    this.saveButton = node('button', 'Save macro'); this.saveButton.type = 'submit'; this.saveButton.className = 'btn btn-primary';
    this.cancelButton = node('button', 'Cancel'); this.cancelButton.type = 'button'; this.cancelButton.className = 'btn'; this.cancelButton.hidden = true;
    this.cancelButton.onclick = () => this.reset();
    const actions = node('div'); actions.className = 'dialog-actions'; actions.append(this.saveButton, this.cancelButton);
    this.form.append(fields, actions);
    this.error = node('p'); this.error.className = 'error'; this.error.setAttribute('role', 'alert');
    this.list = node('div'); this.list.id = 'macros-list';
    this.section.append(this.toolbar, this.importPanel, this.form, this.error, this.list); dialog.append(this.section);
    this.form.onsubmit = event => { event.preventDefault(); this.save(); };
    this.macros = {}; this.owner = false; this.editing = null;
    this.generation = 0;
  }

  // Both reads finish before anything is applied, and only the newest call (or one not
  // overtaken by a save/remove, which also bump the generation) may render, so a slow
  // earlier open can never put older data back over a newer view.
  async open() {
    const generation = ++this.generation;
    this.error.textContent = '';
    let macros = {}, error = '', owner = rights.admin;
    try { macros = await request('/api/macros'); }
    catch (failure) { error = failure.message; }
    if (generation !== this.generation) return;
    this.macros = macros; this.owner = owner; this.error.textContent = error;
    this.form.hidden = !this.owner; this.importOpen.hidden = !this.owner; if (!this.owner) this.toggleImport(false);
    this.render();
  }

  render() {
    this.list.replaceChildren();
    const keys = filterMacroKeys(this.macros, this.filter);
    this.copyAll.disabled = !Object.keys(this.macros).length;
    if (!Object.keys(this.macros).length) this.list.append(node('p', 'No macros yet.'));
    else if (!keys.length) this.list.append(node('p', 'No macros match the search.'));
    for (const key of keys) {
      const row = node('div'); row.className = 'snippet-row';
      const text = node('div'); text.className = 'snippet-text';
      text.append(node('strong', `<${key.toUpperCase()}>`), node('p', this.macros[key])); row.append(text);
      if (this.owner) {
        const edit = node('button', 'Edit'); edit.type = 'button'; edit.className = 'btn'; edit.onclick = () => { this.editing = key; this.key.value = key; this.body.value = this.macros[key]; this.cancelButton.hidden = false; this.key.focus(); };
        const remove = iconButton('trash', `Delete <${key.toUpperCase()}>`, { danger: true }); remove.onclick = () => this.remove(key);
        const actions = node('div'); actions.className = 'snippet-actions-row'; actions.append(edit, remove); row.append(actions);
      }
      this.list.append(row);
    }
  }

  async copyJSON() {
    this.error.textContent = '';
    try { await copyText(JSON.stringify(this.macros, null, 2)); this.copyAll.textContent = 'Copied'; }
    catch { this.copyAll.textContent = 'Copy failed'; }
    setTimeout(() => { this.copyAll.textContent = 'Copy all'; }, 2000);
  }

  toggleImport(open) {
    this.importPanel.hidden = !open; this.importOpen.hidden = open || !this.owner;
    this.pending = null; this.importApply.disabled = true; this.importSummary.textContent = ''; this.importSummary.classList.remove('error');
    if (open) this.importText.focus(); else this.importText.value = '';
  }

  // Read the pasted JSON and say what applying it would do; nothing is saved until Apply.
  previewImport() {
    this.pending = null; this.importApply.disabled = true; this.importSummary.classList.remove('error');
    const text = this.importText.value.trim();
    if (!text) { this.importSummary.textContent = 'Paste macro JSON first.'; this.importSummary.classList.add('error'); return; }
    try {
      const parsed = parseMacroImport(text);
      const merge = describeMerge(this.macros, parsed.macros);
      const skipped = parsed.invalid.length ? ` Skipped ${parsed.invalid.length}: ${parsed.invalid.slice(0, 5).map(row => `${row.key} (${row.reason})`).join('; ')}${parsed.invalid.length > 5 ? '; …' : ''}.` : '';
      if (merge.tooMany) { this.importSummary.textContent = `Importing would leave ${merge.total} macros; the limit is 256.${skipped}`; this.importSummary.classList.add('error'); return; }
      if (!merge.added && !merge.updated) { this.importSummary.textContent = `Nothing to change: ${merge.unchanged ? 'every macro in the import already exists unchanged' : 'no valid macros in the import'}.${skipped}`; return; }
      this.pending = parsed.macros; this.importApply.disabled = false;
      this.importSummary.textContent = `Will add ${merge.added} new and overwrite ${merge.updated} existing (matched by key); ${merge.unchanged} already match.${skipped}`;
    } catch (error) { this.importSummary.textContent = error.message; this.importSummary.classList.add('error'); }
  }

  async applyImport() {
    if (!this.pending) return;
    const next = mergeMacros(this.macros, this.pending);
    this.error.textContent = ''; this.generation++;
    try { this.macros = await request('/api/macros', { method: 'PUT', body: next }); this.toggleImport(false); this.render(); }
    catch (error) { this.importSummary.textContent = error.message; this.importSummary.classList.add('error'); }
  }

  reset() { this.editing = null; this.form.reset(); this.cancelButton.hidden = true; }

  async save() {
    const key = this.key.value.trim().toLowerCase(), body = this.body.value;
    this.error.textContent = '';
    if (!/^[a-z][a-z0-9_-]*$/.test(key)) { this.error.textContent = 'Keys must start with a letter and contain lowercase letters, digits, _ or -.'; return; }
    if (new TextEncoder().encode(body).length > 16 * 1024) { this.error.textContent = 'Macro body exceeds 16 KiB.'; return; }
    if (Object.hasOwn(this.macros, key) && key !== this.editing) { this.error.textContent = 'A macro with that key already exists.'; return; }
    const next = { ...this.macros };
    if (this.editing && this.editing !== key) delete next[this.editing];
    next[key] = body;
    this.generation++;
    try { this.macros = await request('/api/macros', { method: 'PUT', body: next }); this.reset(); this.render(); }
    catch (error) { this.error.textContent = error.message; }
  }

  async remove(key) {
    const next = { ...this.macros }; delete next[key]; this.error.textContent = '';
    this.generation++;
    try { this.macros = await request('/api/macros', { method: 'PUT', body: next }); if (this.editing === key) this.reset(); this.render(); }
    catch (error) { this.error.textContent = error.message; }
  }
}
