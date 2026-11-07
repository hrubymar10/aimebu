import { html, useState, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { IconButton } from '../../components/icon-button.js';
import { useAlive } from '../../hooks/use-alive.js';
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { setTelemetryUser } from '../../telemetry.js';

export const ROLL_LABEL = 'Generate a new fingerprint';
export const ROLL_CONFIRM = 'Rolling a new fingerprint turns off sending your user name and gives you a new anonymous id.';

// Settings -> Telemetry, for everyone: how this person's reports name them. A random fingerprint by default (stored per person, not derived from the
// account); with the switch on, their login name instead, never both. Changes apply at once, here and to the page's own reports.
export function Identity() {
  const alive = useAlive();
  const [identity, setIdentity] = useState(null);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  // Read when Settings first opens, like the rest of this page: it is mounted before sign-in, and a signed-out request would be a 401.
  useLayoutEffect(() => {
    const dialog = document.querySelector('#settings-dialog');
    let started = false, observer = null;
    const load = () => {
      if (started || !alive()) return;
      started = true; observer?.disconnect();
      request('/api/me/telemetry').then(next => { if (alive()) setIdentity(next); }).catch(failure => { if (alive()) setError(failure.message || 'Could not load'); });
    };
    if (dialog?.open) load();
    else if (dialog) { observer = new MutationObserver(() => { if (dialog.open) load(); }); observer.observe(dialog, { attributes: true, attributeFilter: ['open'] }); }
    return () => observer?.disconnect();
  }, []);
  const change = async (method, url, body, message) => {
    setError(''); setStatus(''); setBusy(true);
    try {
      const next = await request(url, { method, body });
      if (!alive()) return;
      setIdentity(next); setTelemetryUser(next.report_id); setStatus(message(next));
    } catch (failure) { if (alive()) setError(failure.message || 'Could not save'); } finally { if (alive()) setBusy(false); }
  };
  const roll = async () => {
    if (identity.send_name && !(await confirmDialog(ROLL_CONFIRM, { title: ROLL_LABEL, confirm: 'Roll' }))) return; // cancelled: nothing changes
    await change('POST', '/api/me/telemetry/fingerprint', {}, () => 'You have a new fingerprint.');
  };
  const copy = async () => {
    try { await navigator.clipboard.writeText(identity.fingerprint); setStatus('Copied.'); setError(''); } catch { setError('Could not copy; select the fingerprint and copy it.'); }
  };
  const off = !identity || busy;
  return html`
    <h3>You in reports</h3>
    <p class="settings-intro">Reports name you by a random fingerprint, so Sentry can count affected people and you can tell your own reports from someone else's. Only while telemetry is on.</p>
    <${SettingsGroup} label="Your identity in reports">
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="telemetry-fingerprint">Fingerprint</label><span class="settings-desc" id="telemetry-fingerprint-desc">Random, kept for you only, and not derived from your account. Roll it to get a new one.</span></div>
        <div class="settings-control"><div class="settings-inline-control">
          <input class="settings-text-input" id="telemetry-fingerprint" aria-describedby="telemetry-fingerprint-desc" type="text" readonly spellcheck="false" value=${identity?.fingerprint || ''} />
          <${IconButton} id="telemetry-fingerprint-copy" name="copy" label="Copy fingerprint" disabled=${!identity} onClick=${copy} />
          <${IconButton} id="telemetry-fingerprint-roll" name="dice" label=${ROLL_LABEL} disabled=${off} onClick=${roll} />
        </div></div>
      </div>
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="telemetry-send-name">Send my user name instead of a fingerprint</label><span class="settings-desc" id="telemetry-send-name-desc">Off by default. Your reports then carry your login name (nothing else) in place of the fingerprint, never both.</span></div>
        <div class="settings-control"><input id="telemetry-send-name" aria-describedby="telemetry-send-name-desc" type="checkbox" checked=${!!identity?.send_name} disabled=${off}
          onChange=${event => change('PUT', '/api/me/telemetry', { send_name: event.currentTarget.checked }, next => (next.send_name ? 'Your reports now carry your user name.' : 'Your reports now carry your fingerprint.'))} /></div>
      </div>
    <//>
    <p class="settings-intro" id="telemetry-identity-status" role="status">${status}</p>
    <p id="telemetry-identity-error" class="error" role="alert">${error}</p>
  `;
}
