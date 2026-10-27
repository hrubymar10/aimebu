import { html, batch, signal } from '../ui.js';
import { request } from '../api.js';
import { uuid } from '../uuid.js';
import { confirmDialog } from '../dialogs.js';
import { scopeStatus, scopeButtons, scopeFailures } from '../pages/project/model.js';

export class ScopeStore {
  constructor(kind) {
    this.kind = kind;
    this.id = signal(null);
    this.state = signal(null);
    this.busy = signal(false);
    this.retryResume = signal(false);
    this.results = signal('');
    this.error = signal('');
    this.alive = true;
    this.generation = 0;
    this.ticket = 0;
  }

  path(id) { return `/api/${this.kind}s/${encodeURIComponent(id)}/control`; }

  reset() {
    this.generation++; this.ticket++;
    batch(() => { this.id.value = null; this.state.value = null; this.retryResume.value = false; this.busy.value = false; this.error.value = ''; this.results.value = ''; });
  }

  async load(id) {
    if (!this.alive) return;
    const generation = this.generation, ticket = ++this.ticket;
    batch(() => { this.id.value = id; this.state.value = null; this.retryResume.value = false; this.error.value = ''; this.results.value = ''; });
    if (!id) return;
    try {
      const state = await request(this.path(id));
      if (!this.alive || generation !== this.generation || ticket !== this.ticket) return;
      this.state.value = state;
    } catch (failure) { if (this.alive && generation === this.generation && ticket === this.ticket) this.error.value = failure.message; }
  }

  async act(action) {
    const id = this.id.peek(), state = this.state.peek(), generation = this.generation;
    if (!this.alive || !id || !state || this.busy.peek()) return;
    if (action === 'stop') {
      this.busy.value = true;
      const sure = await confirmDialog(`Stop agents in this ${this.kind}? Their current work will be stopped. You can resume this ${this.kind} later.`, { title: `Stop agents in this ${this.kind}`, confirm: 'Stop agents', danger: true });
      if (this.alive) this.busy.value = false;
      if (!sure || !this.alive || generation !== this.generation) return;
    }
    batch(() => { this.busy.value = true; this.error.value = ''; this.results.value = ''; });
    try {
      const next = await request(this.path(id), { method: 'POST', headers: { 'Idempotency-Key': uuid() }, body: { action, rev: state.rev } });
      if (!this.alive || generation !== this.generation || this.id.peek() !== id) return;
      const failures = scopeFailures(next, action);
      this.ticket++;
      batch(() => { this.state.value = next; this.retryResume.value = failures.retryResume; this.results.value = failures.text; });
    } catch (failure) {
      if (!this.alive || generation !== this.generation || this.id.peek() !== id) return;
      this.error.value = failure.code === 'stale_rev' ? 'Control state changed. Refreshing…' : failure.message;
      if (failure.code === 'stale_rev') await this.load(id);
    } finally { if (this.alive && generation === this.generation) this.busy.value = false; }
  }

  destroy() { this.alive = false; this.generation++; this.ticket++; }
}

const ACTIONS = [['pause', 'Pause', 'pause', 'btn'], ['stop', 'Stop', 'stop', 'btn btn-danger'], ['resume', 'Resume', 'play', 'btn']];

export function ScopeControl({ store }) {
  const state = store.state.value, buttons = scopeButtons(state, { busy: store.busy.value, retryResume: store.retryResume.value });
  return html`<p class="scope-control-status" role="status">${scopeStatus(state, store.id.value)}</p>
    <div class="scope-control-actions">${ACTIONS.map(([action, label, glyph, classes]) => {
      const name = action === 'resume' ? buttons.resume.label : label;
      return html`<button type="button" data-scope-action=${action} class=${`${classes} btn-glyph`} title=${name} aria-label=${name} disabled=${buttons[action].disabled} onClick=${() => store.act(action)}><span class=${`icon-mask icon-mask-${glyph}`} aria-hidden="true"></span></button>`;
    })}</div>
    <p class="scope-control-results" role="status">${store.results.value}</p>
    <p class="scope-control-error error" role="alert">${store.error.value}</p>`;
}
