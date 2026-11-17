import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { retention } from '../../retention.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { validExpirySeconds, EXPIRY_MIN_SECONDS, EXPIRY_MAX_SECONDS } from '../../expiry.js';

const READ_ONLY = 'Only the owner or an admin can change this.';
// The window is stored in seconds, as it always was; the field shows it in the biggest unit that holds it exactly (21600 is "6 hours"), and a person picks the unit.
const UNITS = [['seconds', 1], ['minutes', 60], ['hours', 3600], ['days', 86400]];
const multiplier = unit => UNITS.find(([name]) => name === unit)?.[1] || 1;
const shownAs = seconds => { const fit = seconds > 0 ? [...UNITS].reverse().find(([, size]) => seconds % size === 0) : null; return fit ? { amount: String(seconds / fit[1]), unit: fit[0] } : { amount: '0', unit: 'hours' }; };
const secondsOf = ({ amount, unit }) => (String(amount).trim() === '' ? NaN : Number(amount) * multiplier(unit));
const DESC = `Messages older than this are hidden from what AI agents are given. They are never deleted and stay visible here, marked with an hourglass. 0 turns expiry off; otherwise from 1 minute to 30 days. Default 6 hours.`;

// The window is the chat's too: `retention` is the shared store, and what is typed is a draft kept apart from it until Save.
export function MessageRetention({ onDirty = () => {} }) {
  const alive = useAlive();
  const [, changed] = useState(0);
  const [draft, setDraft] = useState(null);
  const [saving, setSaving] = useState(false);
  const writing = useRef(false);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  useLayoutEffect(() => retention.onChange(() => changed(count => count + 1)), []);
  const canEdit = retention.canEdit;
  const current = draft ?? shownAs(retention.seconds); // what the fields show: the draft, or the saved window
  // A window typed and not saved keeps Settings' General page in the page when Settings closes.
  useDirty(onDirty, 'message-retention', draft !== null && secondsOf(draft) !== retention.seconds);
  const submit = async () => {
    setError('');
    const value = secondsOf(current);
    if (!validExpirySeconds(value)) { setError(`Enter 0, or an amount from 1 minute (${EXPIRY_MIN_SECONDS} seconds) to 30 days (${EXPIRY_MAX_SECONDS} seconds).`); return; }
    if (writing.current) return;
    writing.current = true;
    setSaving(true);
    try {
      await retention.save(value);
      if (alive()) { setDraft(null); setStatus(value === 0 ? 'Saved. Messages no longer expire.' : 'Saved.'); }
    } catch (failure) {
      if (alive()) setError(failure.message || 'Could not save');
    } finally { writing.current = false; if (alive()) setSaving(false); }
  };
  return html`
    <h3>Messages</h3>
    <${SettingsGroup} label="Messages">
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="message-expiry-amount">Message expiry</label><span class="settings-desc" id="message-expiry-desc">${DESC}</span></div>
        <div class="settings-control"><div class="settings-inline-control">
          <input class="settings-text-input" id="message-expiry-amount" aria-describedby="message-expiry-desc" type="number" min="0" step="1"
            value=${current.amount} disabled=${!canEdit} onInput=${event => { const amount = event.currentTarget.value; setDraft(prior => ({ ...(prior ?? shownAs(retention.seconds)), amount })); }}
            onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); submit(); } }} />
          <select class="settings-select" id="message-expiry-unit" aria-label="Message expiry unit" value=${current.unit} disabled=${!canEdit} onChange=${event => { const unit = event.currentTarget.value; setDraft(prior => ({ ...(prior ?? shownAs(retention.seconds)), unit })); }}>${UNITS.map(([name]) => html`<option value=${name}>${name}</option>`)}</select>
          <button type="button" id="message-expiry-save" class="btn btn-primary" disabled=${!canEdit || saving} onClick=${submit}>Save</button>
        </div></div>
      </div>
    <//>
    <p class="settings-intro" role="status">${canEdit ? status : READ_ONLY}</p>
    <p id="message-expiry-error" class="error" role="alert">${error}</p>
  `;
}
