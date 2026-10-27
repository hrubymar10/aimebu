import { html, useState, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { request, currentUser } from '../../api.js';
import { collectPrefs, applyPrefs, describeSection, SECTION_LABELS } from '../../settings-backup-format.js';

// What the exported file holds: the owner's carries the quota switcher and limit guard setup too, an admin's does not (the server leaves
// them out, and only the owner can apply a file).
const EXPORT_BASE = 'macros, global roles, the model catalog, message expiry';
const EXPORT_OWNER_DESC = `Download ${EXPORT_BASE}, the quota switcher and limit guard setup and this browser's interface preferences. Credentials and tokens are never included.`;
const EXPORT_ADMIN_DESC = `Download ${EXPORT_BASE} and this browser's interface preferences. Credentials and tokens are never included.`;
const RETIRED = { spawn_models: 'Model lists are per harness now (Settings → Harnesses), so the model catalog in this file is not imported.' };

function Plan({ preview, ticked, busy, onTick, onApply, onCancel }) {
  const actionable = preview.sections.filter(section => section.new + section.overwrite > 0).length;
  const unknown = (preview.ignored || []).filter(name => !RETIRED[name]);
  return html`
    <div class="backup-sections">
      ${preview.sections.map(section => html`<label class="backup-section" key=${section.section}>
        <input type="checkbox" value=${section.section} checked=${ticked.has(section.section)} disabled=${!(section.new + section.overwrite)} onChange=${() => onTick(section.section)} />
        <span>${describeSection(section)}</span>
      </label>${(section.invalid || []).map(issue => html`<p class="backup-issue" key=${`${section.section}:${issue.key}`}>${`${SECTION_LABELS[section.section] || section.section}: ${issue.key} skipped (${issue.reason})`}</p>`)}`)}
    </div>
    ${(preview.ignored || []).filter(name => RETIRED[name]).map(name => html`<p class="settings-intro" key=${name}>${RETIRED[name]}</p>`)}
    ${unknown.length > 0 && html`<p class="settings-intro">${`Not understood by this version and ignored: ${unknown.join(', ')}.`}</p>`}
    ${!preview.sections.length && html`<p class="settings-intro">The file has nothing this version can import.</p>`}
    <div class="dialog-actions">
      <button type="button" id="settings-backup-apply" class="btn btn-primary" disabled=${busy || !actionable} onClick=${onApply}>Apply import</button>
      <button type="button" class="btn" onClick=${onCancel}>Cancel</button>
    </div>
  `;
}

// Export is for the owner and the admins; replacing the instance's settings is the owner's alone. Importing is two steps: Preview says what the
// file would change and saves nothing, Apply saves the sections that were ticked.
export function SettingsBackup({ store }) {
  const alive = useAlive();
  const { owner, admin } = store.rights.value;
  const file = useRef(null);
  const reading = useRef(0);
  const [pending, setPending] = useState(null);
  const [ticked, setTicked] = useState(() => new Set());
  const [busy, setBusy] = useState(false);
  const applying = useRef(false);
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  const clear = () => { setPending(null); if (file.current) file.current.value = ''; };
  const exportFile = async () => {
    setError(''); setStatus('');
    try {
      const backup = await request('/api/settings-backup');
      const prefs = collectPrefs(localStorage, currentUser);
      if (Object.keys(prefs).length) backup.ui_prefs = prefs;
      const url = URL.createObjectURL(new Blob([JSON.stringify(backup, null, 2)], { type: 'application/json' }));
      const link = document.createElement('a'); link.href = url; link.download = 'aimebu-settings-backup.json'; document.body.append(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      if (alive()) setStatus('Settings exported.');
    } catch (failure) { if (alive()) setError(failure.message); }
  };
  const choose = async () => {
    const ticket = ++reading.current;
    setError(''); setStatus(''); setPending(null);
    const chosen = file.current.files[0];
    if (!chosen) return;
    let backup;
    try { backup = JSON.parse(await chosen.text()); } catch { if (alive() && ticket === reading.current) setError('That file is not valid JSON.'); return; }
    if (!backup || typeof backup !== 'object' || Array.isArray(backup)) { if (alive() && ticket === reading.current) setError('That file is not a settings backup.'); return; }
    try {
      const preview = await request('/api/settings-backup/preview', { method: 'POST', body: { backup } });
      if (!alive() || ticket !== reading.current) return;
      setTicked(new Set(preview.sections.filter(section => section.new + section.overwrite > 0).map(section => section.section)));
      setPending({ backup, preview });
    } catch (failure) { if (alive() && ticket === reading.current) setError(failure.message); }
  };
  const apply = async () => {
    if (!pending || applying.current) return;
    const sections = pending.preview.sections.map(section => section.section).filter(name => ticked.has(name));
    if (!sections.length) { setError('Tick at least one section.'); return; }
    setError(''); applying.current = true; setBusy(true);
    const { backup, preview } = pending;
    try {
      // The browser's own preferences are applied here; the server is only asked when a server section is ticked (an empty list from
      // here would otherwise be read as a choice of nothing, and with nothing ticked there is nothing to ask).
      const serverSections = sections.filter(name => name !== 'ui_prefs');
      const result = serverSections.length ? await request('/api/settings-backup/apply', { method: 'POST', body: { backup, digest: preview.digest, sections: serverSections } }) : { sections: [], complete: true };
      let reload = false;
      if (sections.includes('ui_prefs') && backup.ui_prefs) { applyPrefs(localStorage, currentUser, backup.ui_prefs); reload = true; }
      const applied = result.sections.map(section => `${SECTION_LABELS[section.section] || section.section}: ${section.applied}`);
      if (reload) applied.push(`${SECTION_LABELS.ui_prefs}: applied after a reload`);
      document.dispatchEvent(new CustomEvent('aimebu:settings-imported'));
      if (!alive()) return;
      clear();
      setStatus(`${result.complete ? 'Imported' : 'Imported with problems'}. ${applied.join('; ')}.`);
      if (result.failed?.length) setError(`Not saved: ${result.failed.map(item => `${item.key} (${item.error})`).join('; ')}.`);
    } catch (failure) {
      if (!alive()) return;
      setError(failure.code === 'preview_stale' ? 'The settings changed since the preview. Choose the file again to see the new plan.' : failure.message);
      if (failure.code === 'preview_stale') clear();
    } finally { applying.current = false; if (alive()) setBusy(false); }
  };
  const tick = name => setTicked(current => { const next = new Set(current); if (next.has(name)) next.delete(name); else next.add(name); return next; });
  return html`<div class="settings-section" id="settings-backup" hidden=${!admin}>
    <h3>Settings backup</h3>
    <${SettingsGroup} label="Settings backup">
      <${SettingsRow} label="Export settings" desc=${owner ? EXPORT_OWNER_DESC : EXPORT_ADMIN_DESC} id="settings-backup-export">
        <button type="button" class="btn" onClick=${exportFile}>Export</button>
      <//>
      <div class="settings-row" hidden=${!owner}>
        <div class="settings-row-info"><label class="settings-label" for="settings-backup-file">Import settings</label><span class="settings-desc" id="settings-backup-file-desc">Choose a file from Export. You see what it would change before anything is saved; matching macros and roles are overwritten, nothing is deleted.</span></div>
        <div class="settings-control"><input type="file" id="settings-backup-file" aria-describedby="settings-backup-file-desc" accept="application/json,.json" ref=${file} onChange=${choose} /></div>
      </div>
    <//>
    <div class="backup-plan" id="settings-backup-plan" hidden=${!pending}>${pending && html`<${Plan} preview=${pending.preview} ticked=${ticked} busy=${busy} onTick=${tick} onApply=${apply} onCancel=${clear} />`}</div>
    <p class="settings-intro" id="settings-backup-status" role="status">${status}</p>
    <p class="error" id="settings-backup-error" role="alert">${error}</p>
  </div>`;
}
