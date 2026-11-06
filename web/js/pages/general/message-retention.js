import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { retention } from '../../retention.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { validExpirySeconds, EXPIRY_MIN_SECONDS, EXPIRY_MAX_SECONDS, EXPIRY_DEFAULT_SECONDS } from '../../expiry.js';

const READ_ONLY = 'Only the owner or an admin can change this.';
const DESC = `Messages older than this are hidden from what AI agents are given. They are never deleted and stay visible here, marked with an hourglass. 0 turns expiry off; otherwise ${EXPIRY_MIN_SECONDS} to ${EXPIRY_MAX_SECONDS} seconds. Default ${EXPIRY_DEFAULT_SECONDS} (6 hours).`;

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
  // A window typed and not saved keeps Settings' General page in the page when Settings closes.
  useDirty(onDirty, 'message-retention', draft !== null && draft.trim() !== String(retention.seconds));
  const submit = async () => {
    setError('');
    const text = (draft ?? String(retention.seconds)).trim();
    const value = text === '' ? NaN : Number(text);
    if (!validExpirySeconds(value)) { setError(`Enter 0, or a whole number of seconds from ${EXPIRY_MIN_SECONDS} to ${EXPIRY_MAX_SECONDS}.`); return; }
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
        <div class="settings-row-info"><label class="settings-label" for="message-expiry-seconds">Message expiry (seconds)</label><span class="settings-desc" id="message-expiry-seconds-desc">${DESC}</span></div>
        <div class="settings-control"><div class="settings-inline-control">
          <input class="settings-text-input" id="message-expiry-seconds" aria-describedby="message-expiry-seconds-desc" type="number" min="0" max=${String(EXPIRY_MAX_SECONDS)} step="1"
            value=${draft ?? String(retention.seconds)} disabled=${!canEdit} onInput=${event => setDraft(event.currentTarget.value)}
            onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); submit(); } }} />
          <button type="button" id="message-expiry-save" class="btn btn-primary" disabled=${!canEdit || saving} onClick=${submit}>Save</button>
        </div></div>
      </div>
    <//>
    <p class="settings-intro" role="status">${canEdit ? status : READ_ONLY}</p>
    <p id="message-expiry-error" class="error" role="alert">${error}</p>
  `;
}
