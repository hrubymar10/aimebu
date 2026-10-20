import { retention } from './retention.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { validExpirySeconds, EXPIRY_MIN_SECONDS, EXPIRY_MAX_SECONDS, EXPIRY_DEFAULT_SECONDS } from './expiry.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// Settings → General → Messages: the expiry window. Everyone sees it; only the owner
// can change it.
export class RetentionSettings {
  constructor() {
    const general = document.querySelector('#settings-general');
    if (!general || general.querySelector('#message-retention')) return;
    const section = node('div', 'settings-section'); section.id = 'message-retention';
    section.append(node('h3', '', 'Messages'));
    this.input = node('input'); this.input.type = 'number'; this.input.id = 'message-expiry-seconds'; this.input.min = '0'; this.input.max = String(EXPIRY_MAX_SECONDS); this.input.step = '1';
    this.save = node('button', 'btn btn-primary', 'Save'); this.save.type = 'button'; this.save.id = 'message-expiry-save';
    const control = node('div', 'settings-inline-control'); control.append(this.input, this.save);
    this.status = node('p', 'settings-intro'); this.status.setAttribute('role', 'status');
    this.error = node('p', 'error'); this.error.id = 'message-expiry-error'; this.error.setAttribute('role', 'alert');
    section.append(settingsGroup([settingsRow({ label: 'Message expiry (seconds)', desc: `Messages older than this are hidden from what AI agents are given. They are never deleted and stay visible here, marked with an hourglass. 0 turns expiry off; otherwise ${EXPIRY_MIN_SECONDS} to ${EXPIRY_MAX_SECONDS} seconds. Default ${EXPIRY_DEFAULT_SECONDS} (6 hours).`, control, id: 'message-expiry-seconds' })], 'Messages'), this.status, this.error);
    general.append(section);
    this.dirty = false;
    this.input.oninput = () => { this.dirty = true; };
    this.save.onclick = () => this.submit();
    this.input.onkeydown = event => { if (event.key === 'Enter') { event.preventDefault(); this.submit(); } };
    retention.onChange(() => this.render());
    document.querySelector('#settings-general-tab')?.addEventListener('click', () => { retention.load(); });
    this.render();
  }
  render() {
    if (!this.input) return;
    if (!this.dirty) this.input.value = String(retention.seconds);
    this.input.disabled = !retention.canEdit; this.save.disabled = !retention.canEdit;
    if (!retention.canEdit) this.status.textContent = 'Only the owner or an admin can change this.';
    else if (this.status.textContent === 'Only the owner or an admin can change this.') this.status.textContent = '';
  }
  async submit() {
    this.error.textContent = '';
    const value = this.input.value.trim() === '' ? NaN : Number(this.input.value);
    if (!validExpirySeconds(value)) { this.error.textContent = `Enter 0, or a whole number of seconds from ${EXPIRY_MIN_SECONDS} to ${EXPIRY_MAX_SECONDS}.`; return; }
    this.save.disabled = true;
    try { this.dirty = false; await retention.save(value); this.status.textContent = value === 0 ? 'Saved. Messages no longer expire.' : 'Saved.'; }
    catch (error) { this.dirty = true; this.error.textContent = error.message || 'Could not save'; }
    finally { this.render(); }
  }
}
