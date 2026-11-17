import { html, useEffect, useState } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { loadSpawnOptions } from '../../spawn-options.js';
import { SpawnDefaults } from '../../components/spawn-defaults.js';

const draftOf = role => ({ source: role.inherited ? 'inherit' : 'custom', items: (role.default_agents || []).map(item => ({ launcher_id: item.launcher_id, model: item.model })), mode: role.pick_mode || 'in_turn' });
const titleOf = role => role.role_label || ({ 'sec-reviewer': 'Security reviewer', 'test-reviewer': 'Test reviewer', 'ux-reviewer': 'UX reviewer' }[role.role_key]) || role.role_key.replace(/[_-]+/g, ' ').replace(/\b\w/g, letter => letter.toUpperCase());
const changed = (role, draft) => JSON.stringify(draftOf(role)) !== JSON.stringify(draft);

function RoleCard({ role, draft, options, onChange }) {
  const id = `project-role-default-${role.role_key}`;
  return html`<section class="settings-card project-role-default"><h4>${titleOf(role)}</h4>
    <div class="settings-row"><div class="settings-row-info"><label class="settings-label" for=${id}>Default source</label><span class="settings-desc" id=${`${id}-desc`}>Use global choices or set project choices.</span></div>
      <select class="settings-select" id=${id} aria-label=${`${titleOf(role)} default source`} aria-describedby=${`${id}-desc`} value=${draft.source} onChange=${event => onChange({ ...draft, source: event.currentTarget.value })}>
        <option value="inherit">${`Inherit global (${role.global_agents.length} ${role.global_agents.length === 1 ? 'agent' : 'agents'})`}</option><option value="custom">Custom</option></select></div>
    <div hidden=${draft.source === 'inherit'}><${SpawnDefaults} items=${draft.items} mode=${draft.mode} options=${options} onChange=${next => onChange({ ...draft, ...next })} /></div>
  </section>`;
}

export function RoleDefaults({ store }) {
  const projectID = store.projectID.value, alive = useAlive();
  const [rows, setRows] = useState(null), [drafts, setDrafts] = useState({});
  const [options, setOptions] = useState({ list: [], error: '' });
  const [status, setStatus] = useState(''), [busy, setBusy] = useState(false);
  useEffect(() => {
    setRows(null); setDrafts({}); setOptions({ list: [], error: '' }); setStatus(''); setBusy(false);
    if (!projectID) return undefined;
    let current = true;
    const live = () => current && alive();
    request(`/api/projects/${encodeURIComponent(projectID)}/role-spawn-defaults`).then(roles => {
      if (live()) { setRows(roles); setDrafts(Object.fromEntries(roles.map(role => [role.role_key, draftOf(role)]))); }
    }, failure => { if (live()) setRows(failure.message); });
    loadSpawnOptions(projectID).then(list => { if (live()) setOptions({ list, error: '' }); }, failure => { if (live()) setOptions({ list: [], error: failure.message }); });
    return () => { current = false; };
  }, [projectID, store.revision.value]);
  const dirty = Array.isArray(rows) ? rows.filter(role => drafts[role.role_key] && changed(role, drafts[role.role_key])) : [];
  const save = async () => {
    const revision = store.revision.peek();
    const currentProject = () => alive() && store.projectID.peek() === projectID && store.revision.peek() === revision;
    setStatus(''); setBusy(true);
    const saved = new Map();
    try {
      for (const role of dirty) {
        const draft = drafts[role.role_key];
        await request(`/api/projects/${encodeURIComponent(projectID)}/role-spawn-defaults/${encodeURIComponent(role.role_key)}`,
          { method: 'PUT', body: { inherit: draft.source === 'inherit', default_agents: draft.items.map(item => ({ ...item })), pick_mode: draft.mode } });
        saved.set(role.role_key, draft);
      }
      if (currentProject()) setStatus('Saved.');
    } catch (failure) { if (currentProject()) setStatus(failure.message); }
    finally {
      if (currentProject()) {
        setRows(previous => Array.isArray(previous) ? previous.map(role => saved.has(role.role_key) ? { ...role, inherited: saved.get(role.role_key).source === 'inherit', default_agents: saved.get(role.role_key).items, pick_mode: saved.get(role.role_key).mode } : role) : previous);
        setBusy(false);
      }
    }
  };
  return html`<section class="settings-subsection" id="project-role-defaults"><h3>Default agents per role</h3><p class="settings-intro">Inherit the global list or set an ordered list for this project.</p>
    <div class="project-role-defaults-list">${typeof rows === 'string' ? rows : (rows || []).map(role => html`<${RoleCard} key=${`${projectID}:${role.role_key}`} role=${role} draft=${drafts[role.role_key] || draftOf(role)} options=${options.list} onChange=${draft => { setStatus(''); setDrafts(previous => ({ ...previous, [role.role_key]: draft })); }} />`)}</div>
    ${options.error && html`<p class="error" role="alert">${options.error}</p>`}
    <button type="button" class="btn btn-primary" id="project-role-defaults-save" disabled=${busy || !dirty.length} onClick=${save}>Save default agents</button>
    <p class="settings-desc" role="status">${status}</p></section>`;
}
