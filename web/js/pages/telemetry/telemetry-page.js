import { html, useState, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { request, rights } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { useIdentity, IdentityRows } from './identity.js';
import { DSN_PLACEHOLDER, RESET_DSN_LABEL, dsnProblem } from '../../telemetry-consent.js';

// 0.5 -> "50%", 1 -> "100%"; a share under a tenth of a percent still shows as such rather than as 0.
function sampleRate(rate) {
  const percent = Number(rate) * 100;
  return `${Number.isInteger(percent) ? percent : Math.round(percent * 10) / 10}%`;
}

// Settings -> Telemetry. One card: the master switch first (the owner's and the admins'; it applies at once), then everything that depends on it: the
// Sentry DSN with its reset and Save, and this person's identity in reports. With the switch off all of that is disabled and muted. Under the card one
// status line, then the volume facts. Someone who is not the owner or an admin gets the identity rows only.
export function TelemetryPage({ onDirty = () => {} }) {
  const alive = useAlive();
  const [view, setView] = useState(null);
  const [enabled, setEnabled] = useState(false);
  const [dsn, setDsn] = useState('');
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState({ status: '', error: '' });
  const [readOnly, setReadOnly] = useState(false);
  const report = (status = '', error = '') => setMessage({ status, error });
  const identity = useIdentity(alive, report);
  // Read when Settings first opens, not when the page loads: this page is mounted before sign-in, and a signed-out request would be a 401.
  // Someone who is not the owner or an admin is not asked at all (it would be a 403).
  useLayoutEffect(() => {
    const dialog = document.querySelector('#settings-dialog');
    let started = false, observer = null;
    const load = () => {
      if (started || !alive()) return;
      started = true; observer?.disconnect();
      if (!rights.owner && !rights.admin) { setReadOnly(true); return; }
      request('/api/telemetry/settings').then(next => { if (!alive()) return; setView(next); setEnabled(!!next.enabled); setDsn(next.dsn || ''); })
        .catch(failure => { if (alive() && failure.status === 403) setReadOnly(true); else if (alive()) report('', failure.message || 'Could not load'); });
    };
    if (dialog?.open) load();
    else if (dialog) { observer = new MutationObserver(() => { if (dialog.open) load(); }); observer.observe(dialog, { attributes: true, attributeFilter: ['open'] }); }
    return () => observer?.disconnect();
  }, []);
  const outcome = next => (next.env_locked ? 'Saved, but AIMEBU2_SENTRY_DSN is set on the server and wins over this.' : next.active ? 'Saved. Telemetry is on; reload the page to include this page.' : 'Saved. Telemetry is off.');
  // The switch applies at once; the DSN has its own Save.
  const toggle = async checked => {
    report(); setSaving(true); setEnabled(checked);
    try {
      const next = await request('/api/telemetry/settings', { method: 'PUT', body: { enabled: checked, prompted: true } });
      if (!alive()) return;
      setView(next); setEnabled(!!next.enabled); setDsn(next.dsn || ''); // text typed but not saved is put back to what is stored
      if (checked && !next.dsn) report('', 'Enter a Sentry DSN and press Save to start sending.'); else report(outcome(next));
    } catch (failure) { if (alive()) { setEnabled(!checked); report('', failure.message || 'Could not save'); } } finally { if (alive()) setSaving(false); }
  };
  const save = async () => {
    const text = dsn.trim();
    const problem = !text ? 'Enter a DSN.' : dsnProblem(text);
    report(); if (problem) { report('', problem); return; }
    setSaving(true);
    try {
      const next = await request('/api/telemetry/settings', { method: 'PUT', body: { dsn: text, prompted: true } });
      if (alive()) { setView(next); setDsn(next.dsn || ''); report(outcome(next)); }
    } catch (failure) { if (alive()) report('', failure.message || 'Could not save'); } finally { if (alive()) setSaving(false); }
  };
  // Closing Settings keeps this page, with what was typed, until it is saved or the text is put back.
  useDirty(onDirty, 'telemetry', !!view && !readOnly && enabled && dsn.trim() !== (view.dsn || ''));
  const defaultDsn = view?.default_dsn || '';
  const known = !!view && !readOnly;
  const off = known && !enabled; // everything below the master switch is greyed out
  const muted = off ? ' telemetry-muted' : '';
  const fieldOff = !known || saving || !enabled;
  return html`
    <${SettingsGroup} label="Telemetry">
      ${!readOnly && html`
        <div class="settings-row">
          <div class="settings-row-info"><label class="settings-label" for="telemetry-enabled">Send telemetry</label><span class="settings-desc" id="telemetry-enabled-desc">Errors and performance traces to Sentry; never messages, prompts, tokens or file contents.</span></div>
          <div class="settings-control"><input id="telemetry-enabled" aria-describedby="telemetry-enabled-desc" type="checkbox" checked=${enabled} disabled=${!known || saving} onChange=${event => toggle(event.currentTarget.checked)} /></div>
        </div>
        <div class=${`settings-row${muted}`}>
          <div class="settings-row-info"><label class="settings-label" for="telemetry-dsn">Sentry DSN</label><span class="settings-desc" id="telemetry-dsn-desc">Where reports go. Filled with the default unless you chose another.</span></div>
          <div class="settings-control"><div class="settings-inline-control">
            ${defaultDsn && html`<${IconButton} id="telemetry-dsn-reset" name="reopen" label=${RESET_DSN_LABEL} disabled=${fieldOff || dsn.trim() === defaultDsn} onClick=${() => setDsn(defaultDsn)} />`}
            <input class="settings-text-input" id="telemetry-dsn" aria-describedby="telemetry-dsn-desc" type="text" autocomplete="off" spellcheck="false" placeholder=${DSN_PLACEHOLDER}
              value=${dsn} disabled=${fieldOff} onInput=${event => setDsn(event.currentTarget.value)} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); save(); } }} />
            <button type="button" id="telemetry-save" class="btn btn-primary" disabled=${fieldOff} onClick=${save}>Save</button>
          </div></div>
        </div>`}
      <${IdentityRows} state=${identity} off=${off} muted=${muted} />
    <//>
    <p class="settings-intro" id="telemetry-status" role="status">${readOnly && !message.status && !message.error ? 'Only the owner or an admin changes what is sent and where.' : message.status}</p>
    <p id="telemetry-error" class="error" role="alert">${message.error}</p>
    ${known && html`
      <dl class=${`settings-facts${muted}`} id="telemetry-facts" aria-label="Volume">
        <dt>Trace sample rate</dt><dd id="telemetry-sample-rate">${sampleRate(view.traces_sample_rate)} of server requests and of page loads. AIMEBU2_SENTRY_TRACES_SAMPLE_RATE changes it.</dd>
        <dt>Database spans</dt><dd id="telemetry-db-spans">At most ${view.max_db_spans} per request; the rest are counted on the request, not traced.</dd>
        <dt>Server file loads</dt><dd id="telemetry-static-files">Not traced: the scripts, styles and icons the server hands out are not requests of their own; the API and the pages are.</dd>
        <dt>Browser file loads</dt><dd id="telemetry-resource-spans">Not traced: no span per script, stylesheet or image, and a page load keeps only its request and response phases.</dd>
      </dl>`}
  `;
}
