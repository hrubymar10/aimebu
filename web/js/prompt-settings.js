import { request } from './api.js';
import { settingsRow, settingsGroup } from './settings-row.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// The parts of an agent's prompt that this screen cannot edit, in the order they sit among the editable sentences.
const LOCKED = { before: 'Opening line (set by the agent adapter)', afterIdentity: 'Role pointer (adapter)', afterRoleBlock: 'Attachment tool (adapter)', afterRetry: 'Response contract and schema (fixed)', afterInbox: null, afterMentions: 'Acknowledgement rule (adapter)', after: 'Closing line (adapter)' };

// Settings → Prompts: the shared sentences of an agent's turn prompt, each with its shipped text, an optional global override and an
// optional project override. The response contract and the adapter-specific parts stay locked. Everyone can read; the instance owner
// edits the global scope, a project's owners and operators edit the project.
export class PromptSettings {
  constructor(dialog, projectContext) {
    this.projectContext = projectContext;
    this.tab = node('button', 'row-btn', 'Prompts'); this.tab.id = 'settings-prompts-tab'; this.tab.type = 'button';
    dialog.querySelector('nav').append(this.tab);
    this.section = node('section', 'settings-content'); this.section.id = 'settings-prompts'; this.section.hidden = true;
    this.section.append(node('h3', '', 'Prompts'), node('p', 'settings-intro', 'The wording an agent gets around its role instructions. A project override wins over a global one, which wins over the shipped text. The response format stays fixed, so a wording change cannot stop answers from being read.'));
    this.scope = node('select'); this.scope.id = 'prompt-scope';
    this.scope.onchange = () => this.open();
    this.inline = node('select'); this.inline.id = 'prompt-inline-plan';
    for (const [value, label] of [['optional', 'Optional'], ['always', 'Always']]) { const option = node('option', '', label); option.value = value; this.inline.append(option); }
    this.inline.onchange = () => this.saveInline();
    this.section.append(settingsGroup([
      settingsRow({ label: 'Scope', desc: 'Whose prompts you are looking at. Global applies to every project that has no override of its own.', control: this.scope, id: 'prompt-scope' }),
      settingsRow({ label: 'Inline plans', desc: 'Whether the lead is told to always attach the full plan as a collapsed "Full plan" block under a visual plan, or leave it to the lead. Applies to every project.', control: this.inline, id: 'prompt-inline-plan' }),
    ], 'Prompt scope and inline plans'));
    this.status = node('p', 'settings-intro'); this.status.setAttribute('role', 'status');
    this.error = node('p', 'error'); this.error.setAttribute('role', 'alert');
    this.list = node('div'); this.list.id = 'prompt-list';
    this.preview = node('pre', 'prompt-preview'); this.preview.id = 'prompt-preview';
    this.section.append(this.status, this.error, this.list, node('h3', '', 'Preview'), node('p', 'settings-intro', 'The prompt as an agent would get it in this scope, with your unsaved edits. Bracketed parts are the locked ones.'), this.preview);
    dialog.append(this.section);
    this.view = null; this.fields = new Map(); this.generation = 0; this.busy = false;
  }

  async open() {
    const generation = ++this.generation;
    this.error.textContent = ''; this.status.textContent = '';
    const { projects = [], selectedProjectID = '' } = this.projectContext?.() || {};
    const keep = this.scope.value;
    this.scope.replaceChildren(...[['', 'Global (all projects)'], ...projects.map(project => [project.id, `Project: ${project.name}`])].map(([value, label]) => { const option = node('option', '', label); option.value = value; return option; }));
    this.scope.value = [...this.scope.options].some(option => option.value === keep) ? keep : '';
    let view;
    try { view = await request(`/api/prompts?project_id=${encodeURIComponent(this.scope.value)}`); }
    catch (failure) { if (generation === this.generation) this.error.textContent = failure.message || 'Could not load prompts'; return; }
    if (generation !== this.generation) return;
    this.view = view;
    this.inline.value = view.inline_plan_appendix; this.lock(this.inline, !view.can_edit_global);
    this.render();
  }

  layer(slot) { return this.scope.value ? slot.project : slot.global; }

  render() {
    const view = this.view, projectScope = !!this.scope.value;
    this.list.replaceChildren(); this.fields = new Map();
    this.list.dataset.scope = this.scope.value; // the scope this list was drawn for, so a caller can tell a fresh list from the previous one
    for (const slot of view.slots) {
      const layer = this.layer(slot);
      const card = node('div', 'prompt-slot'); card.dataset.key = slot.key;
      const head = node('div', 'prompt-slot-head'); head.append(node('strong', '', slot.title));
      const where = node('span', 'prompt-source', slot.source === 'default' ? 'Shipped text' : slot.source === 'global' ? 'Global override' : 'Project override');
      if (layer) where.textContent += projectScope ? ' (this project)' : ' (saved here)';
      head.append(where);
      const text = node('textarea'); text.rows = 4; text.maxLength = view.max; text.value = slot.effective; this.lock(text, !view.can_edit); text.id = `prompt-${slot.key}`;
      text.setAttribute('aria-label', slot.title);
      const count = node('span', 'prompt-count');
      const update = () => { count.textContent = `${[...text.value].length} / ${view.max}`; this.drawPreview(); };
      text.oninput = update;
      const save = node('button', 'btn btn-primary prompt-save', 'Save'); save.type = 'button'; this.lock(save, !view.can_edit);
      save.onclick = () => this.save(slot, text.value);
      const reset = node('button', 'btn prompt-reset', projectScope ? 'Reset to global/shipped' : 'Reset to shipped'); reset.type = 'button'; this.lock(reset, !view.can_edit || !layer);
      reset.onclick = () => this.reset(slot, layer);
      const shipped = node('details', 'prompt-shipped'); shipped.append(node('summary', '', 'Shipped text'), node('p', '', slot.default));
      const actions = node('div', 'dialog-actions'); actions.append(save, reset, count);
      card.append(head, node('p', 'settings-intro', slot.desc), text, actions, shipped);
      text.dataset.rendered = text.value;
      this.fields.set(slot.key, text); this.list.append(card); update();
    }
    if (!view.can_edit) this.status.textContent = projectScope ? 'Only a project owner or operator can change this project\'s prompts.' : 'Only the owner or an admin can change the global prompts.';
    this.drawPreview();
  }

  drawPreview() {
    if (!this.view) return;
    const text = key => (this.fields.get(key)?.value ?? '').trim() || this.view.slots.find(slot => slot.key === key)?.default || '';
    const part = label => `[${label}]`;
    this.preview.textContent = [part(LOCKED.before), text('identity'), part(LOCKED.afterIdentity), part(LOCKED.afterRoleBlock), text('retry_notice'), part(LOCKED.afterRetry), text('inbox_ask'), text('mentions'), part(LOCKED.afterMentions), text('human_reply'), text('tail'), part(LOCKED.after)].join('\n\n');
  }

  // What the owner has actually edited, by slot: a box that still holds the text it was drawn with is not a draft, so a reload never puts
  // stale text back over a newer value another editor saved (the fresh revision would let a Save overwrite it without a conflict).
  drafts() {
    return new Map([...this.fields].filter(([, field]) => field.value !== field.dataset.rendered).map(([key, field]) => [key, field.value]));
  }

  restore(drafts, except = '') {
    for (const [key, value] of drafts) {
      const field = this.fields.get(key);
      if (field && key !== except && field.value !== value) { field.value = value; field.dispatchEvent(new Event('input')); }
    }
  }

  // A control's own enabled state (what the viewer may do) is kept apart from the busy lock, so unlocking puts each back as it was.
  lock(control, base) { control.dataset.baseDisabled = String(base); control.disabled = base || this.busy; }

  // While a save or reset is in flight nothing can be edited and the scope cannot be switched, so the answer always belongs to the page
  // it was asked from; no stale generation, no text typed into a box that is about to be redrawn.
  setBusy(on) {
    this.busy = on;
    this.section.toggleAttribute('aria-busy', on);
    this.scope.disabled = on;
    for (const control of [this.inline, ...this.list.querySelectorAll('textarea, button')]) control.disabled = on || control.dataset.baseDisabled === 'true';
  }

  // Run one change. A failure other than a conflict leaves the page and every draft as they are. Otherwise the list is reloaded (fresh
  // revisions) and the real edits typed before are put back, a conflict's own slot included, with a warning that its saved text changed.
  async change(slot, run, done, failedText) {
    this.error.textContent = ''; this.status.textContent = '';
    this.setBusy(true);
    try {
      let conflict = false;
      try { await run(); } catch (failure) {
        if (failure.status !== 409) { this.error.textContent = failure.message || failedText; return; }
        conflict = true;
      }
      const drafts = this.drafts();
      await this.open();
      this.restore(drafts, conflict ? '' : slot.key);
      if (conflict) this.error.textContent = 'This prompt changed elsewhere. The list is reloaded; your text is still in the box, so check it and save again.';
      else this.status.textContent = done;
    } finally { this.setBusy(false); }
  }

  save(slot, value) {
    const layer = this.layer(slot);
    return this.change(slot, () => request(`/api/prompts/${encodeURIComponent(slot.key)}`, { method: 'PUT', body: { project_id: this.scope.value, value, rev: layer ? layer.rev : 0 } }), `Saved “${slot.title}”.`, 'Could not save');
  }

  reset(slot, layer) {
    return this.change(slot, () => request(`/api/prompts/${encodeURIComponent(slot.key)}?project_id=${encodeURIComponent(this.scope.value)}&rev=${layer.rev}`, { method: 'DELETE' }),
      `“${slot.title}” is back to the ${this.scope.value ? 'next level up' : 'shipped text'}.`, 'Could not reset');
  }

  async saveInline() {
    this.error.textContent = ''; this.status.textContent = '';
    try { await request('/api/inline-plans', { method: 'PUT', body: { value: this.inline.value } }); this.status.textContent = `Saved. Inline plans: ${this.inline.value}.`; }
    catch (failure) { this.error.textContent = failure.message || 'Could not save'; this.inline.value = this.view.inline_plan_appendix; }
  }
}
