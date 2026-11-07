import { html } from '../../ui.js';
import { relativeTime } from '../../time.js';
import { IconButton } from '../../components/icon-button.js';
import { GLOBAL_SCOPES, awaitsApproval, byteLength, emptyText, projectLabelText, recordMeta, recordScope, statusLine, usageMeta, usageRows, usageShare } from './model.js';

function Record({ store, item, mode, profileUsage, cap }) {
  const scope = recordScope(item, mode);
  return html`<article class="knowledge-record"><small title=${item.updated_at ? new Date(item.updated_at).toLocaleString() : ''}>${recordMeta(item, { mode, profileUsage, cap, when: relativeTime(item.updated_at) })}</small>
    <textarea rows="3" maxlength="2000" aria-label=${`Record ${item.id}`} readOnly=${!item._edit} defaultValue=${item.body}></textarea>
    ${item._edit && html`<div class="knowledge-actions"><button type="button" class="btn btn-primary" onClick=${event => store.save(item, event.currentTarget.closest('.knowledge-record').querySelector('textarea').value)}>Save</button>
      <${IconButton} name="trash" label="Delete" danger onClick=${() => store.remove(item)} />
      ${GLOBAL_SCOPES.includes(scope) && html`<${IconButton} name=${item.active ? 'stop' : 'play'} label=${item.active ? 'Deactivate' : awaitsApproval(item, scope) ? 'Approve for agents' : 'Activate for agents'} onClick=${() => store.activate(item, !item.active)} />`}</div>`}</article>`;
}

export function KnowledgePage({ store, onClose }) {
  const mode = store.mode.value, data = store.data.value, all = mode === 'all', project = mode === 'project', task = mode === 'task';
  const projectName = store.projects.value.find(item => item.id === store.projectID.value)?.name || 'this project';
  const writable = !!data && !all && data.can_edit && (!project || data.enabled);
  const records = data?.records || [], clearable = records.filter(item => item._edit);
  const profileUsage = new Map();
  if (mode === 'user_profile') for (const item of records) profileUsage.set(item.scope_id, (profileUsage.get(item.scope_id) || 0) + byteLength(item.body));
  const usage = data ? usageRows(records, mode, store.projects.value.find(item => item.id === store.projectID.value)?.name || '') : [];
  return html`<header><h2 id="knowledge-title">${store.title.value}</h2><${IconButton} id="knowledge-close" name="close" label="Close memory" onClick=${onClose} /></header>
    <label id="knowledge-scope-label" hidden=${task}>View<select id="knowledge-scope" value=${task ? 'all' : mode} onChange=${event => store.selectScope(event.currentTarget.value)}><option value="all">All visible</option><option value="project">Project facts</option><option value="user_profile">User profiles</option><option value="agent_shared_notes">Shared agent notes</option></select></label>
    <label id="knowledge-project-label" hidden=${!project && !all}>${projectLabelText(mode)}<select id="knowledge-project" value=${store.projectID.value} onChange=${event => store.selectProject(event.currentTarget.value)}>${store.projects.value.map(item => html`<option value=${item.id}>${item.name}</option>`)}</select></label>
    <label id="knowledge-enabled-label" hidden=${!project}><input id="knowledge-enabled" type="checkbox" checked=${!!data?.enabled} disabled=${!data?.can_edit || store.saving.value} onChange=${event => store.changeEnabled(event.currentTarget.checked)} /> Enable project memory</label>
    <p id="knowledge-status" class="settings-intro">${data && !store.blocked.value ? statusLine(data, mode, projectName) : store.status.value}</p>
    <div class="knowledge-usage" aria-label="Memory usage by scope" hidden=${!records.length}>${usage.map(row => html`<div class="knowledge-usage-row"><div class="knowledge-usage-meta">${usageMeta(row)}</div>
      <div class="knowledge-usage-bar" role="progressbar" aria-valuemin="0" aria-valuemax=${String(row.cap)} aria-valuenow=${String(row.used)}><span style=${`width:${usageShare(row)}`}></span></div></div>`)}</div>
    <div class="knowledge-actions" hidden=${!clearable.length}><button type="button" class="btn btn-danger" id="knowledge-clean" disabled=${!clearable.length || store.cleaning.value} onClick=${() => store.clean()}>${all ? 'Clear all' : 'Clear filtered'}</button></div>
    <div id="knowledge-records" class="knowledge-records">${data && !records.length && html`<p class="settings-intro">${emptyText(mode)}</p>`}${records.map(item => html`<${Record} key=${`${store.seq.value}:${item.id}`} store=${store} item=${item} mode=${mode} profileUsage=${profileUsage} cap=${data.cap_bytes} />`)}</div>
    <form id="knowledge-add" hidden=${store.blocked.value || (data ? !writable : false)} onSubmit=${async event => { event.preventDefault(); const form = event.currentTarget; if (await store.add(form.elements.body.value)) form.elements.body.value = ''; }}>
      <label id="knowledge-scope-key-label" hidden=${mode !== 'user_profile'}>User ID<input id="knowledge-scope-key" name="scope_key" placeholder="Current user by default" value=${store.scopeKey.value} onInput=${event => { store.scopeKey.value = event.currentTarget.value; }} /></label>
      <label>New record<textarea name="body" rows="3" maxlength="2000" required></textarea></label><button type="submit" class="btn btn-primary">Add</button></form>
    <p id="knowledge-error" class="error" role="alert">${store.error.value}</p>`;
}
