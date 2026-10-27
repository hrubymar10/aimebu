import { html, batch, signal } from '../../ui.js';
import { request } from '../../api.js';
import { inheritText, limitDraft, limitRequest, limitSummary } from './model.js';

export class UsageLimitStore {
  constructor() {
    this.id = signal(null);
    this.state = signal(null);
    this.draft = signal({ costMode: 'inherit', cost: '', turnMode: 'inherit', turn: '' });
    this.busy = signal(false);
    this.error = signal('');
    this.alive = true;
    this.generation = 0;
    this.ticket = 0;
  }

  path(id) { return `/api/projects/${encodeURIComponent(id)}/usage-limit`; }

  reset() { this.generation++; this.ticket++; batch(() => { this.id.value = null; this.state.value = null; this.busy.value = false; this.error.value = ''; }); }

  accept(state) { batch(() => { this.state.value = state; this.draft.value = limitDraft(state); }); }

  async load(id) {
    if (!this.alive) return;
    const generation = this.generation, ticket = ++this.ticket;
    batch(() => { this.id.value = id; this.state.value = null; this.error.value = ''; });
    if (!id) return;
    try {
      const state = await request(this.path(id));
      if (!this.alive || generation !== this.generation || ticket !== this.ticket) return;
      this.accept(state);
    } catch (failure) { if (this.alive && generation === this.generation && ticket === this.ticket) this.error.value = failure.message; }
  }

  async save() {
    const id = this.id.peek(), state = this.state.peek(), generation = this.generation;
    if (!this.alive || !id || !state || this.busy.peek()) return;
    const { problem, body } = limitRequest(this.draft.peek(), state.rev);
    if (problem) { this.error.value = problem; return; }
    batch(() => { this.busy.value = true; this.error.value = ''; });
    try {
      const next = await request(this.path(id), { method: 'PUT', body });
      if (!this.alive || generation !== this.generation || this.id.peek() !== id) return;
      this.accept(next);
    } catch (failure) {
      if (!this.alive || generation !== this.generation || this.id.peek() !== id) return;
      if (failure.code === 'stale_rev') {
        await this.load(id);
        if (!this.alive || generation !== this.generation) return;
        this.error.value = 'Limits changed elsewhere. Review the refreshed values before saving.';
      } else this.error.value = failure.message;
    } finally {
      // A save that did not take puts the form back to the stored values.
      if (this.alive && generation === this.generation) batch(() => { this.busy.value = false; if (this.state.peek()) this.draft.value = limitDraft(this.state.peek()); });
    }
  }

  destroy() { this.alive = false; this.generation++; this.ticket++; }
}

function Mode({ id, label, unit, valueName, ariaLabel, desc, inherited, mode, value, min, step, disabled, onMode, onValue }) {
  return html`<div class="settings-row"><div class="settings-row-info"><label class="settings-label" for=${`${id}-mode`}>${label}</label><span class="settings-desc" id=${`${id}-mode-desc`}>${desc}</span></div>
    <div class="settings-control usage-limit-control">
      <select id=${`${id}-mode`} name=${unit === 'cost' ? 'cost_mode' : 'turn_mode'} aria-describedby=${`${id}-mode-desc`} value=${mode} disabled=${disabled} onChange=${event => onMode(event.currentTarget.value)}>
        <option value="inherit">${inherited}</option><option value="custom">Custom</option><option value="none">No limit</option></select>
      <input id=${`${id}-cap`} name=${valueName} type="number" min=${min} step=${step} aria-label=${ariaLabel} hidden=${mode !== 'custom'} disabled=${disabled} value=${value} onInput=${event => onValue(event.currentTarget.value)} /></div></div>`;
}

export function UsageLimit({ store }) {
  const state = store.state.value, draft = store.draft.value, disabled = store.busy.value || !state;
  const set = patch => { store.draft.value = { ...draft, ...patch }; };
  return html`<h3 id="project-usage-limit-title">Daily limits</h3>
    <p class="usage-limit-summary settings-intro" role="status">${limitSummary(state, store.id.value)}</p>
    <form onSubmit=${event => { event.preventDefault(); store.save(); }}>
      <div class="settings-group" role="group" aria-label="Daily limits">
        <${Mode} id="project-cost" unit="cost" label="Cost cap ($)" valueName="daily_cost_cap" ariaLabel="Cost cap ($) value" min="0" step="any" disabled=${disabled}
          desc="Stop dispatching new agent turns in this project once the reported cost of all its rooms together for the day reaches this amount. Inherit uses the default from Settings, Usages."
          inherited=${state ? inheritText(state.inherited_cost_cap, 'cost') : 'Inherit'} mode=${draft.costMode} value=${draft.cost} onMode=${costMode => set({ costMode })} onValue=${cost => set({ cost })} />
        <${Mode} id="project-turn" unit="turn" label="Turn cap" valueName="daily_turn_cap" ariaLabel="Turn cap value" min="1" step="1" disabled=${disabled}
          desc="Stop dispatching new agent turns in this project after this many turns, all its rooms together, in a day. Inherit uses the default from Settings, Usages."
          inherited=${state ? inheritText(state.inherited_turn_cap, 'turn') : 'Inherit'} mode=${draft.turnMode} value=${draft.turn} onMode=${turnMode => set({ turnMode })} onValue=${turn => set({ turn })} />
      </div>
      <div class="settings-actions"><button type="submit" class="btn btn-primary" disabled=${disabled}>Save limits</button></div>
    </form>
    <p class="usage-limit-error error" role="alert">${store.error.value}</p>`;
}
