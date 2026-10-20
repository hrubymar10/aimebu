import { request } from './api.js';

const money = value => `$${Number(value).toFixed(2)}`;

// Settings -> Project -> Daily limits. The limits apply to the whole project, all its rooms together. For cost and for turns the
// project either inherits the default (Settings -> Usages), sets its own value, or has no limit.
export class UsageLimit {
  constructor(root) {
    this.root = root;
    this.form = root.querySelector('form');
    this.fields = {
      cost: { mode: this.form.elements.cost_mode, value: this.form.elements.daily_cost_cap },
      turn: { mode: this.form.elements.turn_mode, value: this.form.elements.daily_turn_cap },
    };
    this.summary = root.querySelector('.usage-limit-summary');
    this.error = root.querySelector('.usage-limit-error');
    this.form.onsubmit = event => { event.preventDefault(); this.save(); };
    for (const field of Object.values(this.fields)) field.mode.onchange = () => this.showValues();
    this.render();
  }
  path(id) { return `/api/projects/${encodeURIComponent(id)}/usage-limit`; }
  controls() { return [...Object.values(this.fields).flatMap(field => [field.mode, field.value]), this.form.querySelector('button')]; }
  reset() {
    this.generation = (this.generation || 0) + 1; this.loadSeq = (this.loadSeq || 0) + 1; this.id = null; this.state = null; this.busy = false;
    this.error.textContent = ''; this.render();
  }
  async load(id) {
    const generation = this.generation;
    this.id = id;
    const ticket = (this.loadSeq || 0) + 1;
    this.loadSeq = ticket;
    this.state = null;
    this.error.textContent = '';
    this.render();
    if (!id) return;
    try {
      const state = await request(this.path(id));
      if (generation !== this.generation || ticket !== this.loadSeq) return;
      this.state = state;
      this.render();
    } catch (error) { if (generation === this.generation && ticket === this.loadSeq) this.error.textContent = error.message; }
  }
  // A value box shows only for "Custom".
  showValues() { for (const field of Object.values(this.fields)) field.value.hidden = field.mode.value !== 'custom'; }
  render() {
    const state = this.state;
    for (const control of this.controls()) control.disabled = this.busy || !state;
    if (!state) { this.summary.textContent = this.id ? 'Loading today’s usage…' : ''; for (const field of Object.values(this.fields)) field.value.hidden = true; return; }
    const { cost, turn } = this.fields;
    cost.mode.querySelector('option[value=inherit]').textContent = `Inherit (default: ${state.inherited_cost_cap == null ? 'no limit' : money(state.inherited_cost_cap)})`;
    turn.mode.querySelector('option[value=inherit]').textContent = `Inherit (default: ${state.inherited_turn_cap == null ? 'no limit' : `${state.inherited_turn_cap} turns`})`;
    cost.mode.value = state.cost_mode; turn.mode.value = state.turn_mode;
    cost.value.value = state.cost_mode === 'custom' ? state.daily_cost_cap ?? '' : '';
    turn.value.value = state.turn_mode === 'custom' ? state.daily_turn_cap ?? '' : '';
    this.showValues();
    const day = new Date(state.day_start).toLocaleDateString();
    const costLimit = state.daily_cost_cap == null ? 'no cap' : `${money(state.daily_cost_cap)} cap`;
    const turnLimit = state.daily_turn_cap == null ? 'no cap' : `${state.daily_turn_cap} cap`;
    const coverage = state.cost_coverage === 'known' ? 'reported' : state.cost_coverage === 'partial' ? 'partly reported' : 'unreported';
    this.summary.textContent = `${day} (${state.tz}), all rooms: ${state.daily_turns} turns / ${turnLimit}; ${money(state.daily_cost)} ${coverage} cost / ${costLimit}. Dispatch: ${state.effective_state}.`;
  }
  async save() {
    const id = this.id, state = this.state;
    const generation = this.generation;
    if (!id || !state || this.busy) return;
    const { cost, turn } = this.fields;
    const costCap = cost.mode.value === 'custom' ? Number(cost.value.value) : null;
    const turnCap = turn.mode.value === 'custom' ? Number(turn.value.value) : null;
    if (cost.mode.value === 'custom' && (cost.value.value.trim() === '' || !Number.isFinite(costCap) || costCap <= 0) ||
        turn.mode.value === 'custom' && (turn.value.value.trim() === '' || !Number.isSafeInteger(turnCap) || turnCap <= 0)) {
      this.error.textContent = 'Enter a positive value for each custom limit, or choose Inherit or No limit.';
      return;
    }
    this.busy = true;
    this.error.textContent = '';
    for (const control of this.controls()) control.disabled = true;
    try {
      const next = await request(this.path(id), { method: 'PUT', body: { cost_mode: cost.mode.value, daily_cost_cap: costCap, turn_mode: turn.mode.value, daily_turn_cap: turnCap, rev: state.rev } });
      if (generation !== this.generation || this.id !== id) return;
      this.state = next;
      this.render();
    } catch (error) {
      if (generation !== this.generation || this.id !== id) return;
      if (error.code === 'stale_rev') {
        await this.load(id);
        if (generation !== this.generation) return;
        this.error.textContent = 'Limits changed elsewhere. Review the refreshed values before saving.';
      } else this.error.textContent = error.message;
    } finally { if (generation === this.generation) { this.busy = false; this.render(); } }
  }
}
