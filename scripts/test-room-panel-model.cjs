// The room panel's rules are DOM-free module code, so this runs them in a vm: what the header says, who may rename, which controls the settings dialog
// shows and how, and what the worktree, memory and notes sections say.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'room-panel', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { roomPath, kindIcon, headerStatus, membersLabel, canRename, facts, lifecycleControl, removeControl, canLeave, worktreeView, memoryValue, notesView, closeBlockedLabel };', context);
const { roomPath, kindIcon, headerStatus, membersLabel, canRename, facts, lifecycleControl, removeControl, canLeave, worktreeView, memoryValue, notesView, closeBlockedLabel } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.deepEqual(['home', 'task', 'dm'].map(kind => kindIcon({ kind })), ['⌂', '#', '@']);
assert.equal(kindIcon({ kind: 'task', agent_chat: true }), '⇄');
assert.equal(kindIcon({ kind: 'dm', agent_chat: true }), '⇄', 'a chat between agents wins over its kind');

assert.deepEqual([headerStatus({ lifecycle: 'open' }, false), headerStatus({ lifecycle: 'closing' }, false), headerStatus({ lifecycle: 'closed' }, false), headerStatus({ lifecycle: 'closed' }, true), headerStatus({}, false)], ['', 'Closing…', 'Archived', 'Project archived', '']);
assert.deepEqual([membersLabel(1, ''), membersLabel(2, ''), membersLabel(0, 'Archived'), membersLabel(3, 'Closing…')], ['1 member', '2 members', '0 members · Archived', '3 members · Closing…']);

// A task room's title is for owners and operators, the home room's for its owner or an admin, a private chat has none.
assert.deepEqual([['owner', false], ['operator', false], ['viewer', false], [undefined, false], ['owner', true]].map(([role, admin]) => canRename({ kind: 'task' }, role, admin)), [true, true, false, false, true]);
assert.deepEqual([['owner', false], ['operator', false], ['viewer', true], ['viewer', false]].map(([role, admin]) => canRename({ kind: 'home' }, role, admin)), [true, false, true, false]);
assert.equal(canRename({ kind: 'dm' }, 'owner', true), false);

assert.deepEqual(plain(facts({ id: 'rm_1', title: 'general', kind: 'task', lifecycle: 'closed', member_count: 3 }, true)), [
  { label: 'Kind', value: 'Task room', debug: true, copy: false }, { label: 'Status', value: 'Archived', debug: false, copy: false }, { label: 'Members', value: '3', debug: false, copy: false }, { label: 'Room ID', value: 'rm_1', debug: true, copy: true }]);
assert.deepEqual(plain(facts({ id: 'rm_1', title: 'general', kind: 'task' }, false)).slice(0, 3), [{ label: 'Name', value: 'general', debug: false, copy: false }, { label: 'Kind', value: 'Task room', debug: true, copy: false }, { label: 'Status', value: 'Open', debug: false, copy: false }], 'without a title field the name is a fact; a room with no lifecycle is open');

assert.deepEqual([{ kind: 'task' }, { kind: 'task', lifecycle: 'closing' }, { kind: 'task', lifecycle: 'closed' }, { kind: 'home' }].map(room => plain(lifecycleControl(room))), [
  { hidden: false, disabled: false, glyph: 'archive', label: 'Archive room' }, { hidden: false, disabled: true, glyph: 'archive', label: 'Closing…' },
  { hidden: false, disabled: false, glyph: 'reopen', label: 'Reopen room' }, { hidden: true, disabled: false, glyph: 'archive', label: 'Archive room' }]);
assert.deepEqual([['task', 'owner', 'open'], ['task', 'owner', 'closed'], ['task', 'operator', 'closed'], ['home', 'owner', 'closed'], ['dm', 'owner', 'closed']].map(([kind, role, lifecycle]) => plain(removeControl({ kind, lifecycle }, role))), [
  { hidden: false, disabled: false, title: 'Delete this room and its messages for good' }, { hidden: false, disabled: false, title: 'Delete this room and its messages for good' },
  { hidden: true, disabled: false, title: 'Delete this room and its messages for good' }, { hidden: true, disabled: false, title: 'Delete this room and its messages for good' }, { hidden: true, disabled: false, title: 'Delete this room and its messages for good' }]);
assert.deepEqual([canLeave({ agent_chat: true }), canLeave({}), canLeave({ agent_chat: false })], [true, false, false]);

const ready = { enabled: true, state: 'ready', path: '/w/x', relative_path: 'x' };
assert.deepEqual(plain(worktreeView(ready, { lifecycle: 'open' })), { status: 'ready: /w/x', createHidden: true, createLabel: 'Create worktree', removeHidden: false, removeLabel: 'Remove worktree' });
assert.deepEqual(plain(worktreeView({ enabled: true, state: 'failed', reason: 'git said no', relative_path: 'x', dirty_base_count: 2 }, { lifecycle: 'open' })), {
  status: 'failed: x · git said no · The base has 2 uncommitted changes, which are not in the worktree.', createHidden: false, createLabel: 'Retry worktree', removeHidden: false, removeLabel: 'Cancel worktree' });
assert.deepEqual(plain(worktreeView({ enabled: false }, { lifecycle: 'open' })), { status: 'Using the project folder.', createHidden: false, createLabel: 'Create worktree', removeHidden: true, removeLabel: 'Cancel worktree' });
assert.deepEqual(plain(worktreeView(ready, { lifecycle: 'closed' })), { status: 'ready: /w/x', createHidden: true, createLabel: 'Create worktree', removeHidden: false, removeLabel: 'Remove worktree' });
assert.deepEqual(plain(worktreeView({ enabled: true, state: "removed", reason: "worktree directory is missing", path: "/w/x" }, { lifecycle: "closed" })), { status: "removed: /w/x · worktree directory is missing", createHidden: true, createLabel: "Create worktree", removeHidden: true, removeLabel: "Remove worktree" });
assert.equal(worktreeView({ enabled: true, state: "removed", path: "/w/x" }, { lifecycle: "open" }).createHidden, true);

assert.deepEqual([{ memory_enabled: false }, { memory_enabled: true }, {}].map(memoryValue), ['off', 'inherit', 'inherit']);
assert.deepEqual([plain(notesView({ kind: 'task' })), plain(notesView({ kind: 'home' })), notesView({ kind: 'dm' }).hidden], [
  { hidden: false, heading: 'Task notes', text: 'Notes the agents keep for this task. They stay with this room.', button: 'Open task notes' },
  { hidden: false, heading: 'Project memory', text: 'The home room has no task notes of its own: its agents use the project’s memory (project facts).', button: 'Open project memory' }, true]);
assert.deepEqual([closeBlockedLabel('agent_stop_invalid_transition'), closeBlockedLabel('open_children_left'), closeBlockedLabel('')], ['an agent could not be stopped', 'open children left', 'unknown reason']);
// The path above a room's title: the project, then the rooms between it and the project's home room; home rooms and chats outside a project have none.
{
  const rooms = { home: { id: 'rm_h', kind: 'home', project_id: 'p1', title: 'Alpha' }, a: { id: 'rm_a', kind: 'task', project_id: 'p1', parent_id: 'rm_h', title: 'Auth' }, b: { id: 'rm_b', kind: 'task', project_id: 'p1', parent_id: 'rm_a', title: 'Login' } };
  const lookup = { room: id => Object.values(rooms).find(item => item.id === id), project: id => (id === 'p1' ? { name: 'Alpha project' } : undefined), home: () => rooms.home };
  assert.deepEqual(plain(roomPath(rooms.home, lookup)), [], 'a home room is its project');
  assert.deepEqual(plain(roomPath(rooms.a, lookup)), [{ label: 'Alpha project', roomID: 'rm_h', project: true }]);
  assert.deepEqual(plain(roomPath(rooms.b, lookup)), [{ label: 'Alpha project', roomID: 'rm_h', project: true }, { label: 'Auth', roomID: 'rm_a' }], 'project, then the room above');
  assert.deepEqual(plain(roomPath({ id: 'rm_d', kind: 'dm', title: 'ada' }, lookup)), [], 'a chat outside a project has no path');
  assert.deepEqual(plain(roomPath({ ...rooms.a, project_id: 'gone' }, lookup)), [], 'nothing is guessed about a project that is not loaded');
  const loop = { id: 'rm_x', kind: 'task', project_id: 'p1', parent_id: 'rm_y', title: 'X' }, other = { id: 'rm_y', kind: 'task', project_id: 'p1', parent_id: 'rm_x', title: 'Y' };
  assert.equal(roomPath(loop, { ...lookup, room: id => (id === 'rm_y' ? other : loop) }).length, 2, 'a parent loop ends instead of spinning');
}
console.log('room panel model: ok');
