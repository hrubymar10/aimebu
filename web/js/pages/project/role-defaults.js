import { html, useEffect, useState } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { loadSpawnOptions } from '../../spawn-options.js';
import { SpawnDefaults } from '../../components/spawn-defaults.js';

function RoleCard({ projectID, role, options, optionsError }) {
  const alive = useAlive();
  const [source, setSource] = useState(role.inherited ? 'inherit' : 'custom');
  const [defaults, setDefaults] = useState({ items: (role.default_agents || []).map(item => ({ launcher_id: item.launcher_id, model: item.model })), mode: role.pick_mode || 'in_turn' });
  const [status, setStatus] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { if (optionsError) setStatus(optionsError); }, [optionsError]);
  const id = `project-role-default-${role.role_key}`;
  const save = async () => {
    setStatus(''); setBusy(true);
    try {
      const saved = await request(`/api/projects/${encodeURIComponent(projectID)}/role-spawn-defaults/${encodeURIComponent(role.role_key)}`,
        { method: 'PUT', body: { inherit: source === 'inherit', default_agents: defaults.items.map(item => ({ ...item })), pick_mode: defaults.mode } });
      if (alive()) { setStatus('Saved.'); setSource(saved.inherited ? 'inherit' : 'custom'); }
    } catch (failure) { if (alive()) setStatus(failure.message); }
    finally { if (alive()) setBusy(false); }
  };
  return html`<section class="settings-card project-role-default"><h4>${role.role_key}</h4>
    <div class="settings-group"><div class="settings-row"><div class="settings-row-info"><label class="settings-label" for=${id}>Default source</label><span class="settings-desc" id=${`${id}-desc`}>Use global choices or set project choices.</span></div>
      <select class="settings-select" id=${id} aria-label=${`${role.role_key} default source`} aria-describedby=${`${id}-desc`} value=${source} onChange=${event => setSource(event.currentTarget.value)}>
        <option value="inherit">${`Inherit global (${role.global_agents.length} agents)`}</option><option value="custom">Custom</option></select></div></div>
    <div hidden=${source === 'inherit'}><${SpawnDefaults} items=${defaults.items} mode=${defaults.mode} options=${options} onChange=${next => setDefaults(next)} /></div>
    <button type="button" class="btn btn-primary" disabled=${busy} onClick=${save}>Save</button>
    <p class="settings-desc" role="status">${status}</p></section>`;
}

export function RoleDefaults({ store }) {
  const projectID = store.projectID.value, alive = useAlive();
  const [rows, setRows] = useState(null);
  const [options, setOptions] = useState({ list: [], error: '' });
  useEffect(() => {
    setRows(null); setOptions({ list: [], error: '' });
    if (!projectID) return undefined;
    let current = true;
    const live = () => current && alive();
    request(`/api/projects/${encodeURIComponent(projectID)}/role-spawn-defaults`).then(roles => { if (live()) setRows(roles); }, failure => { if (live()) setRows(failure.message); });
    loadSpawnOptions(projectID).then(list => { if (live()) setOptions({ list, error: '' }); }, failure => { if (live()) setOptions({ list: [], error: failure.message }); });
    return () => { current = false; };
  }, [projectID, store.revision.value]);
  return html`<section class="settings-subsection"><h3>Default agents per role</h3><p class="settings-intro">Inherit the global list or set an ordered list for this project.</p>
    <div>${typeof rows === 'string' ? rows : (rows || []).map(role => html`<${RoleCard} key=${`${projectID}:${role.role_key}`} projectID=${projectID} role=${role} options=${options.list} optionsError=${options.error} />`)}</div></section>`;
}
