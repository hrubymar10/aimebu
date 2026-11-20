import { createRegistry } from './registry.js';

// The settings of a room and, in a project's home room, of the project: one dialog, one registry (the shell, the addresses and the search read it as they read
// the global Settings'). The names live here once: a rename is a one-line change. A category id is unique across both sections, so an address needs only the
// category: #room/<room id>/settings/<category>[/<sub>] and #project/<project id>/settings/<category>[/<sub>].
//
// blocks: the elements of the dialog the category shows (CSS selectors; the dialog hides the rest). subs: groups inside a category, each a jump to an anchor.
// A setting's `when` names a fact (see roomFacts in room-panel/model.js) that must hold for the viewer to see it; a category with no setting left is not listed.

export const SECTIONS = [
  { id: 'project', title: 'Project', description: 'The project this home room belongs to' },
  { id: 'room', title: 'Room', description: 'This room, for you and for the room' },
];

export const CATEGORIES = [
  { id: 'details', section: 'project', title: 'General', icon: 'folder', blocks: ['#project-details'], description: 'The folder the project\'s agents work in (its name is the room\'s title)' },
  { id: 'project-agents', section: 'project', title: 'Agents', icon: 'bot', blocks: ['#project-scope-control', '#project-role-defaults', '#project-reviewers', '#project-home-room'],
    description: 'Who may run in the project, the default agents per role and the home room',
    subs: [{ id: 'control', title: 'Control', anchor: 'project-scope-control' }, { id: 'role-defaults', title: 'Role defaults', anchor: 'project-role-defaults' },
      { id: 'reviewers', title: 'Reviewers', anchor: 'project-reviewer-agents' }, { id: 'home-room', title: 'Home room', anchor: 'project-home-lead-only' }] },
  { id: 'team-flow', section: 'project', title: 'Team flow', icon: 'flow', blocks: ['#project-team-flow'], description: 'The team of agents the project starts with' },
  { id: 'recovery', section: 'project', title: 'Recovery & worktrees', icon: 'refresh', blocks: ['#project-recovery'], description: 'Where recoveries go, restarting agents and separate checkouts per task' },
  { id: 'limits', section: 'project', title: 'Limits & usage', icon: 'chart', blocks: ['#project-usage-limit', '#project-usage'], description: 'Daily cost and turn caps, and what the project used',
    subs: [{ id: 'daily-limits', title: 'Daily limits', anchor: 'project-usage-limit' }, { id: 'usage', title: 'Usage', anchor: 'project-usage' }] },
  { id: 'sessions', section: 'project', title: 'Claude Code sessions', icon: 'import', blocks: ['#project-session-import'], description: 'Bring earlier Claude Code sessions into the project' },
  { id: 'danger', section: 'project', title: 'Danger zone', icon: 'warning', blocks: ['#project-lifecycle'], description: 'Archive, reopen or delete the project' },
  { id: 'general', section: 'room', title: 'General', icon: 'info', blocks: ['#room-rename-form', '#room-info-facts', '#room-view-actions'], description: 'The room\'s title, its details and hiding it from your list' },
  { id: 'members', section: 'room', title: 'Members', icon: 'users', blocks: ['#room-member-list'], description: 'The people and agents in the room' },
  { id: 'notifications', section: 'room', title: 'Notifications', icon: 'bell', blocks: ['.room-notify-setting'], description: 'How this room speaks to you, and muting it' },
  { id: 'agents', section: 'room', title: 'Agents', icon: 'bot', blocks: ['#room-scope-control', '#room-schedules'], description: 'Pausing the room\'s agents and the notes sent to them on a schedule',
    subs: [{ id: 'control', title: 'Control', anchor: 'room-scope-control' }, { id: 'schedules', title: 'Schedules', anchor: 'room-schedules' }] },
  { id: 'memory', section: 'room', title: 'Memory & notes', icon: 'book', blocks: ['.room-memory-setting', '.room-notes'], description: 'What the room remembers, and its notes',
    subs: [{ id: 'memory', title: 'Room memory', selector: '.room-memory-setting' }, { id: 'notes', title: 'Notes', selector: '.room-notes' }] },
  { id: 'workspace', section: 'room', title: 'Workspace', icon: 'git', blocks: ['.room-worktree', '#room-usage-link', '#room-usage'], description: 'The room\'s Git checkout and what it used',
    subs: [{ id: 'worktree', title: 'Git worktree', selector: '.room-worktree' }, { id: 'usage', title: 'Usage', anchor: 'room-usage' }] },
  { id: 'leave', section: 'room', title: 'Leave & remove', icon: 'logout', blocks: ['.room-info-actions'], description: 'Archive the room, leave the chat or delete the room' },
];

export const SETTINGS = [
  { anchor: 'project-details-folder', category: 'details', group: 'Project', name: 'Folder', description: 'An absolute path to a folder on the machine the project\'s agents run on.', keywords: ['path', 'directory', 'repository', 'repo'] },
  { anchor: 'project-scope-control', category: 'project-agents', group: 'Control', name: 'Agents in this project', description: 'Pause or resume every agent of the project.', keywords: ['pause', 'resume', 'stop', 'scope'] },
  { anchor: 'project-role-defaults', category: 'project-agents', group: 'Role defaults', name: 'Default agents per role', description: 'Which harness, model and machine a new agent of a role starts with.', keywords: ['harness', 'model', 'machine', 'spawn', 'default'] },
  { anchor: 'project-reviewer-agents', category: 'project-agents', group: 'Reviewers', name: 'Reviewer agents', description: 'Allow reviewer agents in this project.', keywords: ['review', 'reviewer'] },
  { anchor: 'project-home-lead-only', category: 'project-agents', group: 'Home room', name: 'Home room: lead only', description: 'Only roles that lead can be spawned into the home room.', keywords: ['lead', 'leader', 'spawn'] },
  { anchor: 'project-team-flow', category: 'team-flow', group: 'Team flow', name: 'Team flow', description: 'The team of agents the project starts with, and applying it.', keywords: ['team', 'wizard', 'setup', 'flow', 'agents'] },
  { anchor: 'worker-recovery-to-lead', category: 'recovery', group: 'Recovery', name: 'Send worker recoveries to the project lead', description: 'The lead gets a recovery first; if unavailable or late it goes to the Inbox.', keywords: ['inbox', 'lead', 'escalate'] },
  { anchor: 'worker-recovery-escalate-minutes', category: 'recovery', group: 'Recovery', name: 'Escalate after (minutes)', description: 'How long the lead has before a recovery goes to the Inbox.', keywords: ['timeout', 'delay', 'inbox'] },
  { anchor: 'project-auto-resume', category: 'recovery', group: 'Recovery', name: 'Auto-resume after a clean diagnostic report', description: 'Restart a stopped agent when its diagnostic report finds nothing wrong.', keywords: ['restart', 'diagnostic', 'resume'] },
  { anchor: 'project-worktrees', category: 'recovery', group: 'Recovery', name: 'Separate Git worktree for each task room', description: 'Agents in a task room work in their own checkout.', keywords: ['git', 'checkout', 'branch', 'isolation'] },
  { anchor: 'project-usage-limit', category: 'limits', group: 'Daily limits', name: 'Daily limits', description: 'Cost and turn caps per day for this project.', keywords: ['cost', 'cap', 'turn', 'budget', 'money', 'spend'] },
  { anchor: 'project-usage', category: 'limits', group: 'Usage', name: 'Project usage', description: 'What the project used, by day.', keywords: ['cost', 'tokens', 'turns', 'spend'] },
  { anchor: 'project-session-import-open', category: 'sessions', group: 'Claude Code sessions', name: 'Import sessions', description: 'Bring Claude Code sessions from the project\'s folder into the project.', keywords: ['import', 'history', 'claude code', 'transcript'], when: 'importer' },
  { anchor: 'project-archive', category: 'danger', group: 'Danger zone', name: 'Archive project', description: 'Make the project and all its rooms read-only.', keywords: ['read-only', 'close'], when: 'archive' },
  { anchor: 'project-reopen', category: 'danger', group: 'Danger zone', name: 'Reopen project', description: 'Let the project accept changes again.', keywords: ['unarchive', 'restore'], when: 'reopen' },
  { anchor: 'project-delete', category: 'danger', group: 'Danger zone', name: 'Delete project', description: 'Stop its agents, then delete the project.', keywords: ['remove', 'destroy'], when: 'remove' },
  { anchor: 'room-rename-title', category: 'general', group: 'Room', name: 'Title', description: 'The room\'s name in the sidebar and header.', keywords: ['rename', 'name'], when: 'renamable' },
  { anchor: 'room-info-facts', category: 'general', group: 'Room', name: 'Room details', description: 'The room\'s kind, project and id.', keywords: ['id', 'kind', 'created', 'info', 'facts'] },
  { anchor: 'room-hide', category: 'general', group: 'Your list', name: 'Hide room', description: 'Hide this room from your list. It stays for everyone else.', keywords: ['sidebar', 'remove from list'] },
  { anchor: 'room-member-list', category: 'members', group: 'Members', name: 'Members', description: 'Who is in the room, the people and the agents.', keywords: ['people', 'agents', 'access', 'invite'] },
  { anchor: 'room-notify-level', category: 'notifications', group: 'Notifications', name: 'Notify me about', description: 'Which messages of this room notify you.', keywords: ['level', 'sound', 'alerts', 'mentions'] },
  { anchor: 'room-notify-mute', category: 'notifications', group: 'Notifications', name: 'Mute', description: 'Silence the room for a while; it ends by itself.', keywords: ['silence', 'quiet', 'snooze'] },
  { anchor: 'room-scope-control', category: 'agents', group: 'Control', name: 'Room agent control', description: 'Pause or resume the agents homed in this room.', keywords: ['pause', 'resume', 'stop', 'scope'], when: 'scope' },
  { anchor: 'room-schedules', category: 'agents', group: 'Schedules', name: 'Schedules', description: 'Notes sent to an agent once or on a repeat.', keywords: ['schedule', 'cron', 'timer', 'repeat', 'reminder'] },
  { selector: '.room-memory-setting', category: 'memory', group: 'Room memory', name: 'Room memory', description: 'Whether what is said in the room feeds memory and recall.', keywords: ['recall', 'message memory', 'remember'], when: 'memory' },
  { selector: '.room-notes', category: 'memory', group: 'Notes', name: 'Notes', description: 'The room\'s notes, or the project memory in a home room.', keywords: ['knowledge', 'handbook', 'notes'], when: 'notes' },
  { selector: '.room-worktree', category: 'workspace', group: 'Git worktree', name: 'Git worktree', description: 'The room\'s own checkout.', keywords: ['git', 'branch', 'checkout', 'folder'], when: 'worktree' },
  { anchor: 'room-usage', category: 'workspace', group: 'Usage', name: 'Room usage', description: 'What the room used, by day.', keywords: ['cost', 'tokens', 'turns', 'spend'] },
  { anchor: 'room-lifecycle', category: 'leave', group: 'Leave & remove', name: 'Archive or reopen room', description: 'Make the room read-only, or let it accept changes again.', keywords: ['close', 'read-only', 'unarchive'], when: 'lifecycle' },
  { anchor: 'room-leave', category: 'leave', group: 'Leave & remove', name: 'Leave chat', description: 'Stop being a member of this chat.', keywords: ['exit', 'quit'], when: 'leave' },
  { anchor: 'room-remove', category: 'leave', group: 'Leave & remove', name: 'Delete room', description: 'Delete the room and what it holds.', keywords: ['remove', 'destroy'], when: 'remove_room' },
];

export const ROOM_REGISTRY = createRegistry({ sections: SECTIONS, categories: CATEGORIES, settings: SETTINGS, aliases: {}, sectionInPath: false });

// The address layer: #room/<id>/settings/... opens a room's settings, #project/<id>/settings/... a project's (in its home room). settingsAddress(hash) is
// {kind, id, path} (path is settings/<category>[/<sub>], or 'settings'), or null when the hash is not one of these.
export function settingsAddress(value) {
  const text = String(value || '').replace(/^#/, '');
  const [kind, rawID, word, ...rest] = text.split('/');
  if ((kind !== 'room' && kind !== 'project') || word !== 'settings' || !rawID) return null;
  let id;
  try { id = decodeURIComponent(rawID); } catch { return null; }
  return { kind, id, path: ['settings', ...rest].join('/') };
}

// What the address of a category is: the room (or, for a project category, the project) it is about.
export const addressFor = (category, sub, { roomID, projectID }) => {
  const path = ROOM_REGISTRY.categoryPath(category, sub);
  return category.section === 'project' ? `project/${encodeURIComponent(projectID)}/${path}` : `room/${encodeURIComponent(roomID)}/${path}`;
};
