// Import-free: scripts/test-knowledge-model.cjs runs this module in a vm.

export const SCOPE_TITLES = { all: 'All visible memory', project: 'Project facts', user_profile: 'User profiles', agent_shared_notes: 'Shared agent notes' };
export const SCOPE_LABELS = { project: 'Project facts', user_profile: 'User profile', agent_shared_notes: 'Shared agent notes', task: 'Task notes' };
export const GLOBAL_SCOPES = ['user_profile', 'agent_shared_notes'];

export const byteLength = text => new TextEncoder().encode(text).length;

export function scopePath(scope, { projectID, roomID }) {
  if (scope === 'task') return `/api/rooms/${encodeURIComponent(roomID)}/task-notes`;
  if (GLOBAL_SCOPES.includes(scope)) return `/api/global-memory/${scope}`;
  return `/api/projects/${encodeURIComponent(projectID)}/memory`;
}

export const recordScope = (item, mode) => (item.scope === 'project' || item.scope === 'task' || GLOBAL_SCOPES.includes(item.scope) ? item.scope : mode);

export const editable = (scope, part) => !!part.can_edit && (scope !== 'project' || !!part.enabled);

export const projectLabelText = mode => (mode === 'all' ? 'Project facts from' : 'Project');

// Bytes used against each scope's cap: a project, one profile, or the shared notes, mirroring the server's per-scope limits.
export function usageRows(records, mode, projectName) {
  const rows = new Map();
  for (const item of records) {
    const scope = recordScope(item, mode), key = scope === 'user_profile' ? item.scope_id : '';
    const id = `${scope}\u0000${key}`;
    const row = rows.get(id) || { scope, key: scope === 'user_profile' ? (item.scope_label || item.scope_id) : scope === 'project' ? projectName : '', used: 0, count: 0, cap: item._cap };
    row.used += byteLength(item.body);
    row.count += 1;
    rows.set(id, row);
  }
  return [...rows.values()];
}

export const usageMeta = row => `${SCOPE_LABELS[row.scope] || row.scope}${row.key ? ` · ${row.key}` : ''} · ${row.used} / ${row.cap} bytes · ${row.count} ${row.count === 1 ? 'record' : 'records'}`;
export const usageShare = row => `${row.cap ? Math.min(100, Math.round(row.used / row.cap * 100)) : 0}%`;

export function statusLine(data, mode, projectName) {
  const records = data.records || [], all = mode === 'all', project = mode === 'project';
  const used = records.reduce((sum, item) => sum + byteLength(item.body), 0);
  const profiles = new Set(mode === 'user_profile' ? records.map(item => item.scope_id) : []);
  const counts = { project: 0, user_profile: 0, agent_shared_notes: 0 };
  if (all) for (const item of records) counts[recordScope(item, mode)] = (counts[recordScope(item, mode)] || 0) + 1;
  const head = all ? `${records.length} records: ${counts.project} project facts of ${projectName}, ${counts.user_profile} user profiles, ${counts.agent_shared_notes} shared notes. Other projects' facts are in the Project facts view.`
    : mode === 'user_profile' ? `${records.length} records · ${profiles.size} profiles · ${data.cap_bytes} bytes per profile` : `${used} / ${data.cap_bytes} bytes · ${records.length} records`;
  return head + ((project || all) && !data.enabled ? ` · Memory is off for ${projectName}: agents do not get its project facts, and saved records stay available.` : '');
}

export const emptyText = mode => (mode === 'all' ? 'No visible memory saved.' : mode === 'project' ? 'No project facts saved.' : mode === 'task' ? 'No notes saved for this task.' : 'No global memory saved.');

export function recordMeta(item, { mode, profileUsage, cap, when }) {
  const scope = recordScope(item, mode), all = mode === 'all';
  return `${all ? SCOPE_LABELS[scope] + (scope === 'user_profile' ? ` ${item.scope_label || item.scope_id}` : '') + ' · ' : ''}${mode === 'user_profile' ? (item.scope_label || item.scope_id) + ` · ${profileUsage.get(item.scope_id)} / ${cap} bytes · ` : ''}${GLOBAL_SCOPES.includes(scope) && !item.active ? 'Inactive · ' : ''}Revision ${item.rev} · ${item.author_kind} ${item.author_id} · ${when}`;
}

const EVENTS = ['knowledge_created', 'knowledge_updated', 'knowledge_deleted', 'project_memory_setting_changed', 'global_knowledge_created', 'global_knowledge_updated', 'global_knowledge_deleted'];
export function eventTouches(event, { mode, projectID, roomID }) {
  if (!EVENTS.includes(event.kind)) return false;
  const global = event.kind.startsWith('global_knowledge_');
  return ((mode === 'project' || mode === 'all') && event.project_id === projectID) || (mode === 'all' && global) || (mode === 'task' && event.room_id === roomID) || (GLOBAL_SCOPES.includes(mode) && global);
}
