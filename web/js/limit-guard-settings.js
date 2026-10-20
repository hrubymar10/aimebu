import { element } from './usage-format.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { iconButton } from './icon-button.js';
import { GUARD_TOOLS, guardSentence, guardProblem } from './limit-guard-format.js';

// Settings -> Usages -> Limits before 100%: per tool, the soft threshold (no new turns, and the automatic switcher
// leaves the profile for a usable one) and the hard one (a notice that it is at its limit), with per-profile overrides.
// Everything the server says is rendered with textContent only.
const clone = value => JSON.parse(JSON.stringify(value));
const canon = value => Array.isArray(value) ? value.map(canon) : value && typeof value === 'object' ? Object.fromEntries(Object.keys(value).sort().map(key => [key, canon(value[key])])) : value;
const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));

function numberInput(id, value, label, oninput) {
  const input = element('input'); input.type = 'number'; input.id = id; input.setAttribute('aria-label', label);
  input.min = '1'; input.max = '100'; input.step = '1'; input.value = Number.isFinite(value) ? String(value) : '';
  input.oninput = () => oninput(input.value === '' ? NaN : Number(input.value));
  return input;
}
function button(text, label, onclick, { disabled = false, primary = false } = {}) {
  const item = element('button', primary ? 'btn btn-primary' : 'btn', text); item.type = 'button'; item.setAttribute('aria-label', label); item.disabled = disabled; item.onclick = onclick;
  return item;
}

export class LimitGuardSettings {
  constructor(source) {
    this.source = source;
    this.node = element('section', 'limit-guard-settings'); this.node.hidden = true;
    this.node.setAttribute('aria-label', 'Limits before 100%');
    this.data = null; this.drafts = {}; this.errors = {}; this.notes = {}; this.busy = new Set(); this.generation = 0;
    this.node.addEventListener('focusout', () => setTimeout(() => { if (this.stale && !this.editing()) this.render(); }, 0));
  }
  editing() { return this.node.contains(document.activeElement) && document.activeElement.matches('input, select'); }

  // Called by the Usages panel after every load: only the owner sees this, and only where the server runs the guard.
  async sync({ owner }) {
    if (!owner) { this.hide(); return; }
    await this.load();
  }
  hide() { this.generation++; this.node.hidden = true; this.node.replaceChildren(); this.data = null; this.drafts = {}; }
  dirty(tool) { return !!this.data?.tools?.[tool] && !!this.drafts[tool] && !same(this.drafts[tool], this.data.tools[tool].config); }
  savable(tool) { return this.dirty(tool) || this.data?.tools?.[tool]?.saved === false; }

  async load() {
    const generation = ++this.generation;
    let data;
    try { data = await this.source.load(); }
    catch (error) { if (generation === this.generation && [403, 404, 503].includes(error.status)) this.hide(); return; }
    if (generation !== this.generation) return;
    const edited = new Set(GUARD_TOOLS.filter(([tool]) => this.dirty(tool)).map(([tool]) => tool));
    const configChanged = !!this.data && GUARD_TOOLS.some(([tool]) => !same(this.data.tools?.[tool]?.config, data.tools?.[tool]?.config));
    const unchanged = this.data && same(this.data, data) && !this.node.hidden;
    this.data = data;
    if (unchanged) return;
    for (const [tool] of GUARD_TOOLS) if (data.tools?.[tool] && !edited.has(tool)) this.drafts[tool] = clone(data.tools[tool].config);
    if (!this.node.hidden && this.editing()) { if (configChanged) this.stale = true; this.patchLive(); return; }
    this.render();
  }

  render() {
    this.stale = false;
    const focus = this.node.contains(document.activeElement) ? document.activeElement.id : '';
    this.node.replaceChildren(element('h4', 'quota-title', 'Limits before 100%'),
      element('p', 'settings-desc', 'Act before an account runs out. At the soft threshold the server starts no new turns on that tool (a running turn finishes) and the automatic switcher moves to another profile that is still usable. If there is none, new turns stay held until usage falls; at the hard threshold you are told it has reached its limit. The higher of the session and weekly usage counts.'),
      ...GUARD_TOOLS.filter(([tool]) => this.data?.tools?.[tool]).map(([tool, label]) => this.tool(tool, label)));
    this.node.hidden = false;
    if (focus) this.node.querySelector(`#${CSS.escape(focus)}`)?.focus();
  }
  patchLive() {
    for (const [tool, label] of GUARD_TOOLS) this.node.querySelector(`.guard-tool[data-tool="${tool}"] .guard-live`)?.replaceWith(this.live(tool, label));
  }
  live(tool, label) {
    const line = element('p', 'settings-desc guard-live', guardSentence(label, this.data.tools[tool]));
    line.dataset.state = this.data.tools[tool].result?.state || '';
    return line;
  }
  touch(tool) {
    const box = this.node.querySelector(`.guard-tool[data-tool="${tool}"]`);
    if (!box) return;
    box.querySelector('.guard-save').disabled = !this.savable(tool) || this.busy.has(tool);
    box.querySelector('.guard-discard').disabled = !this.dirty(tool);
    box.querySelector('.guard-unsaved').hidden = !this.dirty(tool);
  }
  change(tool) { this.errors[tool] = ''; this.notes[tool] = ''; this.render(); }

  tool(tool, label) {
    const draft = this.drafts[tool], stored = this.data.profiles?.[tool] || [];
    const box = element('div', 'guard-tool'); box.dataset.tool = tool;
    box.append(element('h5', 'guard-tool-title', label), this.live(tool, label));
    const enabled = element('input'); enabled.type = 'checkbox'; enabled.checked = !!draft.enabled; enabled.id = `guard-${tool}-enabled`;
    enabled.onchange = () => { draft.enabled = enabled.checked; this.touch(tool); };
    const rows = [
      settingsRow({ label: `Hold ${label} before it runs out`, desc: `Use the thresholds below for ${label}.`, control: enabled, id: `guard-${tool}-enabled`, className: 'settings-row-toggle' }),
      settingsRow({ label: 'Soft threshold (%)', desc: 'No new turns from here, and the automatic switcher moves to another usable profile. The running turn finishes.', control: numberInput(`guard-${tool}-soft`, draft.soft, `${label} soft threshold in percent`, value => { draft.soft = value; this.touch(tool); }), id: `guard-${tool}-soft` }),
      settingsRow({ label: 'Hard threshold (%)', desc: 'You are told the profile has reached its limit; new turns stay held until usage falls.', control: numberInput(`guard-${tool}-hard`, draft.hard, `${label} hard threshold in percent`, value => { draft.hard = value; this.touch(tool); }), id: `guard-${tool}-hard` }),
    ];
    box.append(settingsGroup(rows, `${label} limit guard`));
    box.append(this.overrides(tool, label, draft, stored));

    const dirty = this.dirty(tool);
    const save = button('Save', `Save ${label} limits`, () => this.save(tool), { primary: true, disabled: !this.savable(tool) || this.busy.has(tool) }); save.classList.add('guard-save'); save.id = `guard-${tool}-save`;
    const discard = button('Discard changes', `Discard ${label} limit changes`, () => { this.drafts[tool] = clone(this.data.tools[tool].config); this.change(tool); }, { disabled: !dirty }); discard.classList.add('guard-discard');
    const unsaved = element('span', 'settings-desc guard-unsaved', 'Unsaved changes'); unsaved.hidden = !dirty;
    const actions = element('div', 'settings-actions'); actions.append(save, discard, unsaved);
    const problem = element('p', 'error guard-error', this.errors[tool] || ''); problem.setAttribute('role', 'alert');
    const note = element('p', 'settings-desc guard-note', this.notes[tool] || ''); note.setAttribute('role', 'status');
    box.append(actions, problem, note);
    return box;
  }

  // Per-profile overrides: one row per profile that has its own thresholds, and a picker for the rest.
  overrides(tool, label, draft, stored) {
    draft.profiles ||= {};
    const rows = Object.keys(draft.profiles).sort().map((name, index) => {
      const limit = draft.profiles[name];
      const controls = element('div', 'quota-entry-controls');
      const field = (text, input) => { const wrap = element('span', 'quota-entry-field'); const caption = element('span', 'quota-entry-caption', text); caption.setAttribute('aria-hidden', 'true'); wrap.append(caption, input); return wrap; };
      controls.append(field('Soft %', numberInput(`guard-${tool}-o${index}-soft`, limit.soft, `${name} soft threshold in percent`, value => { limit.soft = value; this.touch(tool); })),
        field('Hard %', numberInput(`guard-${tool}-o${index}-hard`, limit.hard, `${name} hard threshold in percent`, value => { limit.hard = value; this.touch(tool); })),
        (() => { const remove = iconButton('close', `Remove the own thresholds of ${name}`, { danger: true }); remove.onclick = () => { delete draft.profiles[name]; this.change(tool); }; return remove; })());
      const missing = !stored.includes(name);
      return settingsRow({ label: `${name}${missing ? ' (not found)' : ''}`, desc: missing ? 'This profile no longer exists. Remove the override or restore the profile.' : `Thresholds for ${name} only.`, control: controls, id: `guard-${tool}-o${index}-row`, className: 'settings-row-stack' });
    });
    const available = stored.filter(name => !draft.profiles[name]);
    const select = element('select'); select.id = `guard-${tool}-add`;
    const placeholder = element('option', '', available.length ? 'Choose a profile' : 'No other profiles'); placeholder.value = ''; select.append(placeholder);
    for (const name of available) { const option = element('option', '', name); option.value = name; select.append(option); }
    select.disabled = !available.length;
    const add = button('Add', `Give the chosen ${label} profile its own thresholds`, () => { if (select.value) { draft.profiles[select.value] = { soft: draft.soft, hard: draft.hard }; this.change(tool); } }, { disabled: true });
    select.onchange = () => { add.disabled = !select.value; };
    const control = element('div', 'quota-add-controls'); control.append(select, add);
    rows.push(settingsRow({ label: 'Own thresholds for a profile', desc: 'For a profile that should be held earlier or later than the rest, such as a smaller plan.', control, id: `guard-${tool}-add` }));
    return settingsGroup(rows, `${label} per-profile thresholds`);
  }

  async save(tool) {
    const problem = guardProblem(this.drafts[tool]);
    if (problem) { this.errors[tool] = problem; this.notes[tool] = ''; this.render(); return; }
    if (this.busy.has(tool)) return;
    this.busy.add(tool); this.errors[tool] = ''; this.notes[tool] = ''; this.render();
    try {
      const result = await this.source.save(tool, this.drafts[tool]);
      this.data.tools[tool] = result; this.drafts[tool] = clone(result.config); this.notes[tool] = 'Saved.';
    } catch (error) { this.errors[tool] = error.message || 'The change was not accepted'; }
    finally { this.busy.delete(tool); this.render(); }
  }
}
