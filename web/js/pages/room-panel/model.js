export const kindIcon = room => (room.kind === 'home' ? '⌂' : room.agent_chat ? '⇄' : room.kind === 'dm' ? '@' : '#');

export function headerStatus(room, archivedProject) {
  return archivedProject ? 'Project archived' : room.lifecycle === 'closing' ? 'Closing…' : room.lifecycle === 'closed' ? 'Archived' : '';
}

// Where a room sits: its project, then the rooms it is nested under, outermost first. Each crumb names where it leads (a project leads to its home room).
// A project's home room is the project itself, and a chat outside any project has no path. `lookup` reads the loaded lists: room(id), project(id),
// home(projectID).
export function roomPath(room, lookup) {
  if (!room || room.kind === 'home' || !room.project_id) return [];
  const project = lookup.project(room.project_id);
  if (!project) return [];
  const trail = [], seen = new Set([room.id]);
  for (let parent = room.parent_id && lookup.room(room.parent_id); parent && parent.kind !== 'home' && !seen.has(parent.id); parent = parent.parent_id && lookup.room(parent.parent_id)) {
    seen.add(parent.id); trail.unshift({ label: parent.title, roomID: parent.id });
  }
  return [{ label: project.name, roomID: lookup.home(room.project_id)?.id || '', project: true }, ...trail];
}

export const membersLabel = (count, status) => `${count} ${count === 1 ? 'member' : 'members'}${status ? ` · ${status}` : ''}`;

// The server decides; this only keeps a field out of sight that it would refuse. A task room's title is for owners and operators, the home room's is
// the project's name (its owner or an admin), and a private chat has none of its own.
export function canRename(room, role, admin) {
  if (room.kind === 'dm') return false;
  return room.kind === 'home' ? role === 'owner' || !!admin : !!role && role !== 'viewer';
}

// Kind and Room ID are for debugging: they show only while the message debug inspector is on. Whoever may rename edits the title in its own form.
export function facts(room, renamable) {
  const kind = { home: 'Home room', task: 'Task room', dm: 'Private chat' }[room.kind] || room.kind;
  const status = { open: 'Open', closing: 'Closing', closed: 'Archived' }[room.lifecycle || 'open'] || room.lifecycle;
  return [...(renamable ? [] : [['Name', room.title]]), ['Kind', kind, true], ['Status', status], ['Members', String(room.member_count || 0)], ['Room ID', room.id, true, true]]
    .map(([label, value, debug, copy]) => ({ label, value, debug: !!debug, copy: !!copy }));
}

export function lifecycleControl(room) {
  const closed = room.lifecycle === 'closed';
  return { hidden: room.kind === 'home', disabled: room.lifecycle === 'closing', glyph: closed ? 'reopen' : 'archive', label: closed ? 'Reopen room' : room.lifecycle === 'closing' ? 'Closing…' : 'Archive room' };
}

// Deleting is for task rooms only: never a home room or a private chat.
export function removeControl(room, role) {
  return { hidden: room.kind === 'home' || room.kind === 'dm' || role !== 'owner', disabled: room.lifecycle === 'closing', title: 'Delete this room and its messages for good' };
}

export const canLeave = room => !!room.agent_chat;

export function worktreeView(item, room) {
  const state = item.state;
  return {
    status: item.enabled === false ? 'Using the project folder.' : `${state}: ${item.path || item.relative_path}${item.reason ? ` · ${item.reason}` : ''}${item.dirty_base_count ? ` · The base has ${item.dirty_base_count} uncommitted changes, which are not in the worktree.` : ''}`,
    createHidden: room.lifecycle !== 'open' || (item.enabled !== false && state !== 'failed'),
    createLabel: state === 'failed' ? 'Retry worktree' : 'Create worktree',
    removeHidden: item.enabled === false,
    removeLabel: (item.path || room.lifecycle === 'closed') ? 'Remove worktree' : 'Cancel worktree',
  };
}

export const memoryValue = room => (room.memory_enabled === false ? 'off' : 'inherit');

export function notesView(room) {
  const home = room.kind === 'home';
  return {
    hidden: room.kind !== 'task' && !home,
    heading: home ? 'Project memory' : 'Task notes',
    text: home ? 'The home room has no task notes of its own: its agents use the project’s memory (project facts).' : 'Notes the agents keep for this task. They stay with this room.',
    button: home ? 'Open project memory' : 'Open task notes',
  };
}

export const closeBlockedLabel = reason => (reason === 'agent_stop_invalid_transition' ? 'an agent could not be stopped' : (reason || 'unknown reason').replaceAll('_', ' '));
