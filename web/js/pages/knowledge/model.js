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

// The names v1's viewer gives each scope, for its badges and usage bars.
export const VIEW_LABELS = { project: 'Project facts', user_profile: 'User profile', agent_shared_notes: 'Shared notes', task: 'Task notes' };
export const VIEW_OPTIONS = [['all', 'All visible memory'], ['project', 'Project facts'], ['user_profile', 'User profile'], ['agent_shared_notes', 'Shared notes']];
export const usageCount = row => `${row.used}/${row.cap} · ${row.count} ${row.count === 1 ? 'record' : 'records'}`;
export const usageShare = row => `${row.cap ? Math.min(100, Math.round(row.used / row.cap * 100)) : 0}%`;

// The notice v1 shows when memory is off, for the project in view.
export const offNotice = projectName => `Memory is off for ${projectName}: agents do not get its project facts, and saved records stay available.`;

export const emptyText = mode => (mode === 'all' ? 'No visible memory saved.' : mode === 'project' ? 'No project facts saved.' : mode === 'task' ? 'No notes saved for this task.' : 'No global memory saved.');

// A shared note an agent wrote and nobody approved yet: not the same as one the owner switched off, and the owner needs to be able to tell.
export const awaitsApproval = (item, scope) => scope === 'agent_shared_notes' && !item.active && item.author_kind === 'agent';

// The name of who wrote a record: the server sends it (a person's login, an agent's slug); an imported v1 author's id is already a name; an id with no name is not shown.
export function authorLabel(item) {
  if (item.author_name) return item.author_name;
  const id = String(item.author_id || '');
  return id && !/^(usr|agt)_/.test(id) ? id : (item.author_kind === 'agent' ? 'an agent' : 'someone');
}

// What a card's row shows after its scope badge and key: v1's "v1 · owner", with the date, and a word first when the record is switched off.
export function recordMeta(item, { mode, when }) {
  const scope = recordScope(item, mode);
  const state = GLOBAL_SCOPES.includes(scope) && !item.active ? (awaitsApproval(item, scope) ? 'Waiting for approval · ' : 'Inactive · ') : '';
  return `${state}v${item.rev} · ${authorLabel(item)} · ${when}`;
}

// The key beside a card's badge: the project, the person a profile is about, or "global" for the shared notes.
export function recordKey(item, mode, { projectName, taskTitle }) {
  const scope = recordScope(item, mode);
  if (scope === 'user_profile') return item.scope_label || item.scope_id;
  if (scope === 'agent_shared_notes') return 'global';
  return scope === 'task' ? taskTitle : projectName;
}

const EVENTS = ['knowledge_created', 'knowledge_updated', 'knowledge_deleted', 'project_memory_setting_changed', 'global_knowledge_created', 'global_knowledge_updated', 'global_knowledge_deleted'];
export function eventTouches(event, { mode, projectID, roomID }) {
  if (!EVENTS.includes(event.kind)) return false;
  const global = event.kind.startsWith('global_knowledge_');
  return ((mode === 'project' || mode === 'all') && event.project_id === projectID) || (mode === 'all' && global) || (mode === 'task' && event.room_id === roomID) || (GLOBAL_SCOPES.includes(mode) && global);
}
