import { html, useEffect, useState } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';

// Role prompts per project: the extension a project adds to each role's instructions, under the role's own body, plus the final
// prompt the server composes for an agent of that role here. Only the project's owner and operators read and change it.

const titleOf = role => role.label || role.role_key.replace(/[_-]+/g, ' ').replace(/\b\w/g, letter => letter.toUpperCase());

function PromptCard({ role, draft, onChange, onSave, busy }) {
  const id = `project-role-extension-${role.role_key}`;
  const dirty = (draft.body || '') !== (role.project_extension?.body || '');
  return html`<section class="settings-card project-role-prompt"><h4>${titleOf(role)}</h4>
    ${role.flow_override && html`<p class="settings-desc">This project's team flow replaces the role's instructions with its own text; the extensions still add to it.</p>`}
    <div class="settings-row settings-row-stack"><div class="settings-row-info"><label class="settings-label" for=${id}>Extension</label><span class="settings-desc" id=${`${id}-desc`}>Added after the role's instructions, without replacing them. Empty inherits the global extension only.${role.global_body_extension ? ' The instance owner also set a global extension.' : ''}</span></div>
      <textarea class="settings-text-input settings-textarea" id=${id} aria-describedby=${`${id}-desc`} rows="4" value=${draft.body} onInput=${event => onChange({ ...draft, body: event.currentTarget.value })}></textarea></div>
    <div class="settings-row settings-row-stack"><label class="settings-label" for=${`${id}-final`}>Final prompt</label><span class="settings-desc" id=${`${id}-final-desc`}>What an agent with this role in this project is told, as last saved.</span>
      <details><summary aria-describedby=${`${id}-final-desc`}>Show final prompt</summary><textarea class="settings-text-input settings-textarea" id=${`${id}-final`} aria-describedby=${`${id}-final-desc`} rows="10" readOnly value=${role.delivered_body}></textarea></details></div>
    <div class="settings-row"><div class="settings-row-info"></div><div class="settings-control"><button type="button" class="btn btn-primary" id=${`${id}-save`} disabled=${busy || !dirty} onClick=${onSave}>Save extension</button></div></div>
  </section>`;
}

export function RolePrompts({ store }) {
  const projectID = store.projectID.value, alive = useAlive();
  const canEdit = !!store.project.value && ['owner', 'operator'].includes(store.project.value.role);
  const [rows, setRows] = useState(null), [drafts, setDrafts] = useState({});
  const [status, setStatus] = useState(''), [busy, setBusy] = useState(false);
  useEffect(() => {
    setRows(null); setDrafts({}); setStatus(''); setBusy(false);
    if (!projectID || !canEdit) return undefined;
    let current = true;
    const live = () => current && alive();
    request(`/api/projects/${encodeURIComponent(projectID)}/role-prompts`).then(roles => {
      if (live()) {
        setRows(roles);
        setDrafts(Object.fromEntries(roles.map(role => [role.role_key, { body: role.project_extension?.body || '' }])));
      }
    }, failure => { if (live()) setRows(failure.message); });
    return () => { current = false; };
  }, [projectID, canEdit, store.revision.value]);
  const save = async role => {
    const draft = drafts[role.role_key] || { body: '' };
    setStatus(''); setBusy(true);
    try {
      await request(`/api/projects/${encodeURIComponent(projectID)}/role-prompts/${encodeURIComponent(role.role_key)}`,
        { method: 'PUT', body: { body: draft.body, rev: role.project_extension?.rev || 0 } });
      // The delivered text changed with the extension, so the list is read again for its fresh preview.
      const fresh = await request(`/api/projects/${encodeURIComponent(projectID)}/role-prompts`);
      if (alive()) { setRows(fresh); setDrafts(Object.fromEntries(fresh.map(item => [item.role_key, { body: item.project_extension?.body || '' }]))); setStatus('Saved.'); }
    } catch (failure) { if (alive()) setStatus(failure.message); }
    finally { if (alive()) setBusy(false); }
  };
  return html`<section class="settings-subsection" id="project-role-prompts"><h3>Role prompt extensions</h3><p class="settings-intro">Add text after a role's instructions without replacing them; the built-in defaults keep updating underneath.</p>
    ${canEdit && typeof rows === 'string' && html`<p class="error" role="alert">${rows}</p>`}
    <div class="project-role-prompts-list">${canEdit && Array.isArray(rows) && rows.map(role => html`<${PromptCard} key=${`${projectID}:${role.role_key}`} role=${role} draft=${drafts[role.role_key] || { body: '' }}
      onChange=${draft => { setStatus(''); setDrafts(previous => ({ ...previous, [role.role_key]: draft })); }} onSave=${() => save(role)} busy=${busy} />`)}</div>
    <p class="settings-desc" role="status">${status}</p></section>`;
}
