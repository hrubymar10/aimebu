import { html, useState, useLayoutEffect } from '../../ui.js';
import { SettingsGroup } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { request, rights } from '../../api.js';
import { CONSENT_TEXT, DSN_PLACEHOLDER, dsnProblem } from '../../telemetry-consent.js';

// Settings -> General -> Error reports: the same two fields as the first-run popup. A save applies to the server at once; this page starts
// or stops its own reporting after a reload.
export function ErrorReports() {
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
    const text = dsn.trim();
    const problem = enabled && !text ? 'Enter a DSN, or turn error reports off.' : dsnProblem(text);
    if (problem) { setError(problem); return; }
    setSaving(true);
    try {
      const next = await request('/api/telemetry/settings', { method: 'PUT', body: { enabled, dsn: text, prompted: true } });
      if (alive()) { setView(next); setDsn(next.dsn || ''); setStatus(next.env_locked ? 'Saved, but AIMEBU2_SENTRY_DSN is set on the server and wins over this.' : next.active ? 'Saved. Error reports are on; reload the page to include this page.' : 'Saved. Error reports are off.'); }
    } catch (failure) { if (alive()) setError(failure.message || 'Could not save'); } finally { if (alive()) setSaving(false); }
  };
  const locked = readOnly || !view;
  return html`
    <h3>Error reports</h3>
    <p class="settings-intro">${CONSENT_TEXT}</p>
    <${SettingsGroup} label="Error reports">
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="error-reports-enabled">Send error reports</label><span class="settings-desc" id="error-reports-enabled-desc">Errors and performance traces; never messages, prompts, tokens or file contents.</span></div>
        <div class="settings-control"><input id="error-reports-enabled" aria-describedby="error-reports-enabled-desc" type="checkbox" checked=${enabled} disabled=${locked || saving} onChange=${event => setEnabled(event.currentTarget.checked)} /></div>
      </div>
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="error-reports-dsn">Sentry DSN</label><span class="settings-desc" id="error-reports-dsn-desc">Where reports go. Clear it to use the default.</span></div>
        <div class="settings-control"><div class="settings-inline-control">
          <input class="settings-text-input" id="error-reports-dsn" aria-describedby="error-reports-dsn-desc" type="text" autocomplete="off" spellcheck="false" placeholder=${DSN_PLACEHOLDER}
            value=${dsn} disabled=${locked || saving} onInput=${event => setDsn(event.currentTarget.value)} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); save(); } }} />
          <button type="button" id="error-reports-save" class="btn btn-primary" disabled=${locked || saving} onClick=${save}>Save</button>
        </div></div>
      </div>
    <//>
    <p class="settings-intro" role="status">${readOnly ? 'Only the owner or an admin can change this.' : status}</p>
    <p id="error-reports-error" class="error" role="alert">${error}</p>
  `;
}
