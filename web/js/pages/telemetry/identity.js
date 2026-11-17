import { html, useState, useLayoutEffect } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { setTelemetryUser } from '../../telemetry.js';
import { SEND_NAME_LABEL, SEND_NAME_DESC } from '../../telemetry-consent.js';

export const ROLL_LABEL = 'Generate a new anonymous ID (the current one stops being used)';
export const ROLL_LABEL_NAME = 'Use a random fingerprint instead';
export const ROLL_CONFIRM = 'Rolling a new fingerprint turns off sending your user name and gives you a new anonymous id.';

// How this person's reports name them: a random fingerprint by default (stored per person, not derived from the account); with the switch on, their
// login name instead, never both. Changes apply at once, here and to the page's own reports. `report(status, error)` is the page's one status line.
export function useIdentity(alive, report) {
  const [identity, setIdentity] = useState(null);
  const [busy, setBusy] = useState(false);
  // Read when Settings first opens, like the rest of this page: it is mounted before sign-in, and a signed-out request would be a 401.
  useLayoutEffect(() => {
    const dialog = document.querySelector('#settings-dialog');
    let started = false, observer = null;
    const load = () => {
      if (started || !alive()) return;
      started = true; observer?.disconnect();
      request('/api/me/telemetry').then(next => { if (alive()) setIdentity(next); }).catch(failure => { if (alive()) report('', failure.message || 'Could not load'); });
    };
    if (dialog?.open) load();
    else if (dialog) { observer = new MutationObserver(() => { if (dialog.open) load(); }); observer.observe(dialog, { attributes: true, attributeFilter: ['open'] }); }
    return () => observer?.disconnect();
  }, []);
  const change = async (method, url, body, message) => {
    report(); setBusy(true);
    try {
      const next = await request(url, { method, body });
      if (!alive()) return;
      setIdentity(next); report(message(next));
      // The running SDK is told after the page has painted the answer, never inside the click's own turn.
      setTimeout(() => setTelemetryUser(next.report_id), 0);
    } catch (failure) { if (alive()) report('', failure.message || 'Could not save'); } finally { if (alive()) setBusy(false); }
  };
  // The field shows what the reports carry now: the user name while that switch is on, otherwise the fingerprint (again after a roll or the switch off).
  const shown = identity ? (identity.send_name ? identity.login : identity.fingerprint) : '';
  const roll = async () => {
    if (identity.send_name && !(await confirmDialog(ROLL_CONFIRM, { title: ROLL_LABEL_NAME, confirm: 'Use a random fingerprint' }))) return; // cancelled: nothing changes
    await change('POST', '/api/me/telemetry/fingerprint', {}, () => 'You have a new fingerprint.');
  };
  const copy = async () => {
    try { await navigator.clipboard.writeText(shown); report('Copied.'); } catch { report('', `Could not copy; select the ${identity.send_name ? 'user name' : 'fingerprint'} and copy it.`); }
  };
  const sendName = checked => change('PUT', '/api/me/telemetry', { send_name: checked }, next => (next.send_name ? 'Your reports now carry your user name.' : 'Your reports now carry your fingerprint.'));
  return { identity, busy, shown, roll, copy, sendName };
}

// The two rows of the card: the field with its copy and dice buttons, and the switch. `off` greys them out (telemetry is off); `muted` is their class.
export function IdentityRows({ state, off, muted }) {
  const { identity, busy, shown, roll, copy, sendName } = state;
  const disabled = !identity || busy || off;
  return html`
    <div class=${`settings-row${muted}`}>
      <div class="settings-row-info"><label class="settings-label" for="telemetry-fingerprint">${identity?.send_name ? 'User name' : 'Fingerprint'}</label><span class="settings-desc" id="telemetry-fingerprint-desc">${identity?.send_name ? 'Your login name, which your reports carry while the switch below is on. Rolling turns it off and gives you a new random fingerprint.' : 'Random, kept for you only, and not derived from your account. Roll it to get a new one.'}</span></div>
      <div class="settings-control"><div class="settings-inline-control">
        <input class="settings-text-input" id="telemetry-fingerprint" aria-describedby="telemetry-fingerprint-desc" type="text" readonly disabled=${off} spellcheck="false" value=${shown} />
        ${!identity?.send_name && html`<${IconButton} id="telemetry-fingerprint-copy" name="copy" label="Copy fingerprint" disabled=${!identity || off} onClick=${copy} />`}
        <${IconButton} id="telemetry-fingerprint-roll" name="dice" label=${identity?.send_name ? ROLL_LABEL_NAME : ROLL_LABEL} disabled=${disabled} onClick=${roll} />
      </div></div>
    </div>
    <div class=${`settings-row settings-row-toggle${muted}`}>
      <div class="settings-row-info"><label class="settings-label" for="telemetry-send-name">${SEND_NAME_LABEL}</label><span class="settings-desc" id="telemetry-send-name-desc">${SEND_NAME_DESC}</span></div>
      <div class="settings-control"><input id="telemetry-send-name" aria-describedby="telemetry-send-name-desc" type="checkbox" checked=${!!identity?.send_name} disabled=${disabled}
        onChange=${event => sendName(event.currentTarget.checked)} /></div>
    </div>
  `;
}
