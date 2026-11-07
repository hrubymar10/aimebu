import { html, useState, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { request, rights } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { Identity } from './identity.js';
import { CONSENT_TEXT, DSN_PLACEHOLDER, RESET_DSN_LABEL, dsnProblem } from '../../telemetry-consent.js';

// 0.5 -> "50%", 1 -> "100%"; a share under a tenth of a percent still shows as such rather than as 0.
function sampleRate(rate) {
  const percent = Number(rate) * 100;
  return `${Number.isInteger(percent) ? percent : Math.round(percent * 10) / 10}%`;
}

// Settings -> Telemetry: the same two fields as the first-run popup. A save applies to the server at once; this page starts
// or stops its own reporting after a reload.
export function TelemetryPage({ onDirty = () => {} }) {
  return html`<h3>Telemetry</h3><${Identity} /><${Controls} onDirty=${onDirty} />`;
}

function Controls({ onDirty }) {
  const alive = useAlive();
  const [view, setView] = useState(null);
  const [enabled, setEnabled] = useState(false);
  const [dsn, setDsn] = useState('');
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  const [readOnly, setReadOnly] = useState(false);
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
        .catch(failure => { if (alive() && failure.status === 403) setReadOnly(true); else if (alive()) setError(failure.message || 'Could not load'); });
    };
    if (dialog?.open) load();
    else if (dialog) { observer = new MutationObserver(() => { if (dialog.open) load(); }); observer.observe(dialog, { attributes: true, attributeFilter: ['open'] }); }
    return () => observer?.disconnect();
  }, []);
  const save = async () => {
    setError(''); setStatus('');
    const text = dsn.trim(); // off: only the switch is saved, the field is greyed out and may hold anything
    const problem = !enabled ? '' : !text ? 'Enter a DSN, or turn telemetry off.' : dsnProblem(text);
    if (problem) { setError(problem); return; }
    setSaving(true);
    try {
      const next = await request('/api/telemetry/settings', { method: 'PUT', body: enabled ? { enabled, dsn: text, prompted: true } : { enabled, prompted: true } });
      if (alive()) { setView(next); setDsn(next.dsn || ''); setStatus(next.env_locked ? 'Saved, but AIMEBU2_SENTRY_DSN is set on the server and wins over this.' : next.active ? 'Saved. Telemetry is on; reload the page to include this page.' : 'Saved. Telemetry is off.'); }
    } catch (failure) { if (alive()) setError(failure.message || 'Could not save'); } finally { if (alive()) setSaving(false); }
  };
  const locked = readOnly || !view;
  // Closing Settings keeps this page, with what was typed, until it is saved or the choice is put back.
  useDirty(onDirty, 'telemetry', !!view && !readOnly && (enabled !== !!view.enabled || (enabled && dsn.trim() !== (view.dsn || ''))));
  const defaultDsn = view?.default_dsn || '';
  // Off disables the field and its reset button; it does not hide them.
  const fieldOff = locked || saving || !enabled;
  if (readOnly) return html`<p class="settings-intro" role="status">Only the owner or an admin can change what is sent and where.</p>`;
  return html`
    <h3>What is sent</h3>
    <p class="settings-intro">${CONSENT_TEXT}</p>
    <${SettingsGroup} label="Telemetry">
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="telemetry-enabled">Send telemetry</label><span class="settings-desc" id="telemetry-enabled-desc">Errors and performance traces; never messages, prompts, tokens or file contents.</span></div>
        <div class="settings-control"><input id="telemetry-enabled" aria-describedby="telemetry-enabled-desc" type="checkbox" checked=${enabled} disabled=${locked || saving} onChange=${event => setEnabled(event.currentTarget.checked)} /></div>
      </div>
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="telemetry-dsn">Sentry DSN</label><span class="settings-desc" id="telemetry-dsn-desc">Where reports go. Filled with the default unless you chose another.</span></div>
        <div class="settings-control"><div class="settings-inline-control">
          ${defaultDsn && html`<${IconButton} id="telemetry-dsn-reset" name="reopen" label=${RESET_DSN_LABEL} disabled=${fieldOff || dsn.trim() === defaultDsn} onClick=${() => setDsn(defaultDsn)} />`}
          <input class="settings-text-input" id="telemetry-dsn" aria-describedby="telemetry-dsn-desc" type="text" autocomplete="off" spellcheck="false" placeholder=${DSN_PLACEHOLDER}
            value=${dsn} disabled=${fieldOff} onInput=${event => setDsn(event.currentTarget.value)} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); save(); } }} />
          <button type="button" id="telemetry-save" class="btn btn-primary" disabled=${locked || saving} onClick=${save}>Save</button>
        </div></div>
      </div>
    <//>
    <p class="settings-intro" id="telemetry-status" role="status">${status}</p>
    <p id="telemetry-error" class="error" role="alert">${error}</p>
    ${view && html`
      <h3>Volume</h3>
      <p class="settings-intro">What keeps the number of traces small. These are facts, not settings: the server's environment decides them.</p>
      <dl class="settings-facts" id="telemetry-facts">
        <dt>Trace sample rate</dt><dd id="telemetry-sample-rate">${sampleRate(view.traces_sample_rate)} of server requests and of page loads. AIMEBU2_SENTRY_TRACES_SAMPLE_RATE changes it.</dd>
        <dt>Database spans</dt><dd id="telemetry-db-spans">At most ${view.max_db_spans} per request; the rest are counted on the request, not traced.</dd>
        <dt>Server file loads</dt><dd id="telemetry-static-files">Not traced: the scripts, styles and icons the server hands out are not requests of their own; the API and the pages are.</dd>
        <dt>Browser file loads</dt><dd id="telemetry-resource-spans">Not traced: no span per script, stylesheet or image, and a page load keeps only its request and response phases.</dd>
      </dl>`}
  `;
}
