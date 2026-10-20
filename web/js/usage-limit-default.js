import { request } from './api.js';
import { element } from './usage-format.js';
import { settingsRow, settingsGroup } from './settings-row.js';

// Settings -> Usages -> Daily limits: the default every project inherits unless it sets its own in Project settings. Blank is no limit.
// Only the owner changes it, so only the owner sees it here (the project settings show the inherited value to everyone).
export class UsageLimitDefault {
  constructor({ send = request } = {}) {
    this.send = send;
    this.node = element('section', 'usage-limit-default'); this.node.hidden = true;
    this.node.setAttribute('aria-label', 'Daily limits for all projects');
    this.generation = 0;
  }
  // Called when Settings opens. The server says whether this person may change the default; anyone else gets no card.
  async sync() {
    const generation = ++this.generation;
    let state;
    try { state = await this.send('/api/usage-limit-default'); }
    catch { if (generation === this.generation) { this.node.hidden = true; this.node.replaceChildren(); } return; }
    if (generation !== this.generation) return;
    if (!state.can_edit) { this.node.hidden = true; this.node.replaceChildren(); return; }
    this.render(state);
  }
  render(state) {
    const cost = element('input', ''); cost.type = 'number'; cost.id = 'default-cost-cap'; cost.min = '0'; cost.step = 'any'; cost.placeholder = 'No limit'; cost.value = state.daily_cost_cap ?? '';
    const turns = element('input', ''); turns.type = 'number'; turns.id = 'default-turn-cap'; turns.min = '1'; turns.step = '1'; turns.placeholder = 'No limit'; turns.value = state.daily_turn_cap ?? '';
    const error = element('p', 'error', ''); error.setAttribute('role', 'alert');
    const save = element('button', 'btn btn-primary', 'Save default limits'); save.type = 'button';
    save.onclick = async () => {
      error.textContent = '';
      const costValue = cost.value.trim() === '' ? null : Number(cost.value), turnValue = turns.value.trim() === '' ? null : Number(turns.value);
      if (costValue !== null && (!Number.isFinite(costValue) || costValue <= 0) || turnValue !== null && (!Number.isSafeInteger(turnValue) || turnValue <= 0)) {
        error.textContent = 'Enter positive limits, or leave a field blank for no limit.'; return;
      }
      save.disabled = true;
      try { this.render(await this.send('/api/usage-limit-default', { method: 'PUT', body: { daily_cost_cap: costValue, daily_turn_cap: turnValue } })); this.node.querySelector('.settings-saved').textContent = 'Saved.'; }
      catch (problem) { error.textContent = problem.message; save.disabled = false; }
    };
    const saved = element('span', 'settings-saved', ''); saved.setAttribute('role', 'status');
    const actions = element('div', 'settings-actions'); actions.append(save, saved);
    this.node.replaceChildren(
      element('h4', 'quota-title', 'Daily limits'),
      element('p', 'settings-desc', 'The most a project may spend in a day, all its rooms together, before new agent turns stop and you are asked to continue. Every project uses these unless it sets its own in Project settings.'),
      settingsGroup([
        settingsRow({ label: 'Cost cap ($)', desc: 'Reported cost per project per day. Blank means no limit.', control: cost }),
        settingsRow({ label: 'Turn cap', desc: 'Turns per project per day. Blank means no limit.', control: turns }),
      ], 'Default daily limits'),
      actions, error);
    this.node.hidden = false;
  }
}
