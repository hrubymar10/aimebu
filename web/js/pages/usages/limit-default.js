import { html, signal, batch, useState, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';

// Settings -> Usages -> Daily limits: the default every project inherits unless it sets its own in Project settings. Blank is no limit. Only
// the owner changes it, so only the owner sees it here (the project settings show the inherited value to everyone).
export class LimitDefaultStore {
  constructor({ send = request } = {}) {
    this.send = send;
    this.state = signal(null);
    this.saved = signal('');
    this.generation = 0;
    this.alive = true;
  }

  // Called when Settings opens. The server says whether this person may change the default; anyone else gets no card.
  async sync() {
    if (!this.alive) return;
    const generation = ++this.generation;
    let state;
    try { state = await this.send('/api/usage-limit-default'); } catch { if (this.alive && generation === this.generation) this.state.value = null; return; }
    if (!this.alive || generation !== this.generation) return;
    batch(() => { this.saved.value = ''; this.state.value = state.can_edit ? state : null; });
  }

  async save(body) {
    const state = await this.send('/api/usage-limit-default', { method: 'PUT', body });
    if (this.alive) batch(() => { this.state.value = state; this.saved.value = 'Saved.'; });
    return state;
  }

  destroy() { this.alive = false; this.generation++; }
}

const limit = text => (text.trim() === '' ? null : Number(text));

function Form({ store, state }) {
  const cost = useRef(null), turns = useRef(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const writing = useRef(false);
  const save = async () => {
    setError('');
    const costValue = limit(cost.current.value), turnValue = limit(turns.current.value);
    if (costValue !== null && (!Number.isFinite(costValue) || costValue <= 0) || turnValue !== null && (!Number.isSafeInteger(turnValue) || turnValue <= 0)) {
      setError('Enter positive limits, or leave a field blank for no limit.');
      return;
    }
    if (writing.current) return;
    writing.current = true; setBusy(true);
    try { await store.save({ daily_cost_cap: costValue, daily_turn_cap: turnValue }); } catch (problem) { if (store.alive) setError(problem.message); } finally { writing.current = false; setBusy(false); }
  };
  return html`
    <h4 class="quota-title">Daily limits</h4>
    <p class="settings-desc">The most a project may spend in a day, all its rooms together, before new agent turns stop and you are asked to continue. Every project uses these unless it sets its own in Project settings.</p>
    <${SettingsGroup} label="Default daily limits">
      <${SettingsRow} label="Cost cap ($)" desc="Reported cost per project per day. Blank means no limit." id="default-cost-cap">
        <input type="number" min="0" step="any" placeholder="No limit" ref=${cost} value=${state.daily_cost_cap ?? ''} />
      <//>
      <${SettingsRow} label="Turn cap" desc="Turns per project per day. Blank means no limit." id="default-turn-cap">
        <input type="number" min="1" step="1" placeholder="No limit" ref=${turns} value=${state.daily_turn_cap ?? ''} />
      <//>
    <//>
    <div class="settings-actions"><button type="button" class="btn btn-primary" disabled=${busy} onClick=${save}>Save default limits</button><span class="settings-saved" role="status">${store.saved.value}</span></div>
    <p class="error" role="alert">${error}</p>
  `;
}

export function LimitDefault({ store }) {
  const state = store.state.value;
  return html`<section class="usage-limit-default" aria-label="Daily limits for all projects" hidden=${!state}>${state && html`<${Form} key=${JSON.stringify(state)} store=${store} state=${state} />`}</section>`;
}
