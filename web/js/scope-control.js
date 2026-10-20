import { request } from './api.js';
import { uuid } from './uuid.js';
import { setGlyph } from './icon-button.js';
import { confirmDialog } from './dialogs.js';

const labels = { active: 'Active', paused: 'Paused', stopped: 'Stopped' };
// Each control is an icon with its name as tooltip and accessible name (never text); Resume is a play glyph.
const glyphs = { pause: ['pause', 'Pause'], stop: ['stop', 'Stop'], resume: ['play', 'Resume'] };

export class ScopeControl {
  constructor(root, kind) {
    this.root = root;
    this.kind = kind;
    this.status = root.querySelector('.scope-control-status');
    this.results = root.querySelector('.scope-control-results');
    this.error = root.querySelector('.scope-control-error');
    this.buttons = [...root.querySelectorAll('[data-scope-action]')];
    for (const button of this.buttons) { button.onclick = () => this.act(button.dataset.scopeAction); button.classList.add('btn-glyph'); setGlyph(button, ...glyphs[button.dataset.scopeAction]); }
    this.render();
  }
  path(id) { return `/api/${this.kind}s/${encodeURIComponent(id)}/control`; }
  reset() { this.generation = (this.generation || 0) + 1; this.id = null; this.state = null; this.retryResume = false; this.busy = false; this.error.textContent = ''; this.results.replaceChildren(); this.render(); }
  async load(id) {
    const generation = this.generation;
    this.id = id;
    this.state = null;
    this.retryResume = false;
    this.error.textContent = '';
    this.results.replaceChildren();
    this.render();
    if (!id) return;
    try {
      const state = await request(this.path(id));
      if (generation !== this.generation || this.id !== id) return;
      this.state = state;
      this.render();
    } catch (error) { if (generation === this.generation && this.id === id) this.error.textContent = error.message; }
  }
  render() {
    const state = this.state;
    this.root.dataset.desired = state?.desired || '';
    this.status.textContent = state ? `State: ${labels[state.effective_state] || state.effective_state}` : this.id ? 'Loading control state…' : '';
    for (const button of this.buttons) {
      const action = button.dataset.scopeAction;
      if (action === 'resume') setGlyph(button, 'play', state?.desired === 'active' ? 'Retry resume' : 'Resume');
      button.disabled = this.busy || !state ||
        (action === 'pause' && state.desired !== 'active') ||
        (action === 'stop' && state.desired === 'stopped') ||
        (action === 'resume' && state.desired === 'active' && !this.retryResume);
    }
  }
  async act(action) {
    const id = this.id, state = this.state;
    const generation = this.generation;
    if (!id || !state || this.busy) return;
    if (action === 'stop') {
      this.busy = true;
      const sure = await confirmDialog(`Stop agents in this ${this.kind}? Their current work will be stopped. You can resume this ${this.kind} later.`, { title: `Stop agents in this ${this.kind}`, confirm: 'Stop agents', danger: true });
      this.busy = false;
      if (!sure || generation !== this.generation) return;
    }
    this.busy = true;
    this.error.textContent = '';
    this.results.replaceChildren();
    this.render();
    try {
      const next = await request(this.path(id), {
        method: 'POST',
        headers: { 'Idempotency-Key': uuid() },
        body: { action, rev: state.rev },
      });
      if (generation !== this.generation || this.id !== id) return;
      this.state = next;
      const failures = (next.results || []).filter(result => result.error).map(result => result.error);
      this.retryResume = action === 'resume' && failures.length > 0;
      this.results.textContent = failures.length ? `Agent controls needing attention: ${failures.join('; ')}.${action === 'resume' ? ' Retry Resume after the agent exits.' : ''}` : '';
    } catch (error) {
      if (generation !== this.generation || this.id !== id) return;
      this.error.textContent = error.code === 'stale_rev' ? 'Control state changed. Refreshing…' : error.message;
      if (error.code === 'stale_rev') await this.load(id);
    } finally { if (generation === this.generation) { this.busy = false; this.render(); } }
  }
}
