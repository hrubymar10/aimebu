import { html, useState } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { GLOBAL_SCOPES, VIEW_LABELS, VIEW_OPTIONS, awaitsApproval, emptyText, offNotice, recordKey, recordMeta, recordScope, usageCount, usageRows, usageShare } from './model.js';

// One record, as v1 draws it: a card that leads with the scope's badge and the key, then the grey line (version, author, date), the text, and Save, Delete.
// A switched-off global record also has its activation button (v2 only), a text button like the others.
function Record({ store, item, mode, names }) {
  const scope = recordScope(item, mode), [edited, setEdited] = useState(false);
  const when = item.updated_at ? new Date(item.updated_at).toLocaleDateString() : 'never';
  return html`<div class="memory-row knowledge-record" data-id=${item.id}>
    <div class="memory-row-header"><span class="memory-scope-badge">${VIEW_LABELS[scope] || scope}</span><code>${recordKey(item, mode, names)}</code><span class="memory-meta" title=${item.updated_at ? new Date(item.updated_at).toLocaleString() : ''}>${recordMeta(item, { mode, when })}</span></div>
    <textarea class="memory-textarea" rows="3" maxlength="2000" aria-label=${`Record ${item.id}`} readOnly=${!item._edit} defaultValue=${item.body} onInput=${event => setEdited(event.currentTarget.value !== item.body)}></textarea>
    ${item._edit && html`<div class="memory-row-actions"><button type="button" class="btn btn-sm knowledge-save" disabled=${!edited} onClick=${event => store.save(item, event.currentTarget.closest('.knowledge-record').querySelector('textarea').value)}>Save</button>
      <button type="button" class="btn btn-sm btn-danger knowledge-delete" onClick=${() => store.remove(item)}>Delete</button>
      ${GLOBAL_SCOPES.includes(scope) && html`<button type="button" class="btn btn-sm knowledge-activation" onClick=${() => store.activate(item, !item.active)}>${item.active ? 'Deactivate' : awaitsApproval(item, scope) ? 'Approve for agents' : 'Activate for agents'}</button>`}</div>`}</div>`;
}

// The memory viewer in v1's layout: the pane of v1's Settings, a filter row (what to show, which project, Refresh), the clear buttons at the top right, one usage bar per scope,
// the cards, and the add form under them. What v2 added sits in v1's form: the project select takes the place of v1's key box, the switch for a project's memory is a
// row under the filter, and a project whose memory is off gets v1's notice.
export function KnowledgePage({ store, onClose }) {
  const mode = store.mode.value, data = store.data.value, all = mode === 'all', project = mode === 'project', task = mode === 'task', profiles = mode === 'user_profile';
  const projectName = store.projects.value.find(item => item.id === store.projectID.value)?.name || 'this project';
  const writable = !!data && !all && data.can_edit && (!project || data.enabled);
  const records = data?.records || [], clearable = records.filter(item => item._edit);
  const usage = data ? usageRows(records, mode, projectName) : [];
  const names = { projectName, taskTitle: store.room?.title || '' };
  const state = store.blocked.value ? 'blocked' : data ? 'ready' : 'loading';
  const message = data && !store.blocked.value ? '' : store.status.value;
  return html`<div class="settings-pane memory-viewer-pane" data-state=${state}><div class="settings-body">
    <header class="settings-body-header"><span class="settings-section-title" id="knowledge-title">${store.title.value}</span><${IconButton} id="knowledge-close" className="settings-close-btn" name="close" label="Close memory" onClick=${onClose} /></header>
    <div class="settings-section active">
      <div class="memory-disabled-banner" id="knowledge-off" hidden=${!(data && (project || all) && !data.enabled)}>${offNotice(projectName)}</div>
      <div class="memory-toolbar">
        ${task ? html`<span></span>` : html`<select id="knowledge-scope" class="settings-select" aria-label="Show" value=${mode} onChange=${event => store.selectScope(event.currentTarget.value)}>${VIEW_OPTIONS.map(([value, label]) => html`<option value=${value}>${label}</option>`)}</select>`}
        ${project || all ? html`<select id="knowledge-project" class="settings-select memory-scope-key-input" aria-label=${all ? 'Project facts from' : 'Project'} title=${all ? 'The project whose facts are shown' : 'The project'} value=${store.projectID.value} onChange=${event => store.selectProject(event.currentTarget.value)}>${store.projects.value.map(item => html`<option value=${item.id}>${item.name}</option>`)}</select>` : html`<span></span>`}
        <button type="button" class="btn btn-sm" id="knowledge-refresh" onClick=${() => store.load()}>Refresh</button>
      </div>
      <label id="knowledge-enabled-label" class="memory-enabled-row" hidden=${!project}><input id="knowledge-enabled" type="checkbox" checked=${!!data?.enabled} disabled=${!data?.can_edit || store.saving.value} onChange=${event => store.changeEnabled(event.currentTarget.checked)} /> Enable memory for ${projectName}</label>
      <div class="memory-clean-row knowledge-clear" hidden=${!clearable.length}><button type="button" class="btn btn-sm btn-danger" id="knowledge-clean" disabled=${!clearable.length || store.cleaning.value} onClick=${() => store.clean()}>${all ? 'Clear all' : 'Clear filtered'}</button></div>
      <div class="memory-usage-list knowledge-usage" aria-label="Memory usage by scope" hidden=${!records.length}>${usage.map(row => html`<div class="memory-usage-row knowledge-usage-row"><div class="memory-usage-meta"><span>${VIEW_LABELS[row.scope] || row.scope}</span><code>${row.key}</code><span>${usageCount(row)}</span></div>
        <div class="memory-usage-bar" role="progressbar" aria-label=${`${VIEW_LABELS[row.scope] || row.scope} memory used`} aria-valuemin="0" aria-valuemax=${String(row.cap)} aria-valuenow=${String(row.used)}><span style=${`width:${usageShare(row)}`}></span></div></div>`)}</div>
      <div id="knowledge-records" class="memory-list knowledge-records">${message ? html`<div id="knowledge-status" class="empty-state">${message}</div>` : !records.length ? html`<div class="empty-state">${emptyText(mode)}</div>` : ''}${records.map(item => html`<${Record} key=${`${store.seq.value}:${item.id}`} store=${store} item=${item} mode=${mode} names=${names} />`)}</div>
      <form id="knowledge-add" class="memory-add-form" hidden=${store.blocked.value || (data ? !writable : false)} onSubmit=${async event => { event.preventDefault(); const form = event.currentTarget; if (await store.add(form.elements.body.value)) form.elements.body.value = ''; }}>
        <div class="memory-add-row" hidden=${!profiles}><input id="knowledge-scope-key" name="scope_key" class="settings-text-input memory-scope-key-input" type="text" aria-label="User ID" placeholder="User ID (the current user by default)" autocomplete="off" spellcheck="false" value=${store.scopeKey.value} onInput=${event => { store.scopeKey.value = event.currentTarget.value; }} /></div>
        <textarea class="memory-textarea" name="body" rows="3" maxlength="2000" required placeholder="Add a durable memory record..." aria-label="New record"></textarea>
        <button type="submit" class="btn btn-sm btn-primary">Add memory</button></form>
      <p id="knowledge-error" class="error" role="alert">${store.error.value}</p>
    </div></div></div>`;
}
