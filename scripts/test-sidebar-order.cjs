// The room list's ordering is DOM-free module code, so this runs it in a vm: the three sorts, the manual order and the moves
// that edit it (nothing is pinned).
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'sidebar', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { SORTS, DEFAULT_SORT, orderProjects, orderRooms, manualSequence, moveProject, placeProject, projectActivity };', context);
const { SORTS, DEFAULT_SORT, orderProjects, orderRooms, manualSequence, moveProject, placeProject, projectActivity } = context.api;
const ids = list => Array.from(list, item => item.id ?? item);

assert.deepEqual(Array.from(SORTS, ([key]) => key), ['name', 'activity', 'manual']);
assert.equal(DEFAULT_SORT, 'manual');

// Creation order is b, a, c, d. Activity: c is the newest, then a, d has none, b is only a DM.
const projects = [{ id: 'b', name: 'Beta' }, { id: 'a', name: 'alpha' }, { id: 'c', name: 'Gamma' }, { id: 'd', name: 'Delta' }];
const rooms = [
  { id: 'r1', project_id: 'a', kind: 'home', last_activity_at: '2026-10-02T00:00:00Z' },
  { id: 'r2', project_id: 'c', kind: 'task', last_activity_at: '2026-10-05T00:00:00Z' },
  { id: 'r3', project_id: 'b', kind: 'dm', last_activity_at: '2026-10-09T00:00:00Z' },
  { id: 'r4', project_id: 'a', kind: 'task', last_activity_at: '2026-10-01T00:00:00Z' },
];
assert.equal(projectActivity(projects[0], rooms), '', 'a direct message is not a project\'s activity');
assert.equal(projectActivity(projects[1], rooms), '2026-10-02T00:00:00Z');

// name: case-insensitive, natural; activity: newest first, projects with none after them by name; manual with nothing saved is creation order.
assert.deepEqual(ids(orderProjects(projects, rooms, 'name')), ['a', 'b', 'd', 'c']);
assert.deepEqual(ids(orderProjects(projects, rooms, 'activity')), ['c', 'a', 'b', 'd']);
assert.deepEqual(ids(orderProjects(projects, rooms, 'manual')), ['b', 'a', 'c', 'd']);
assert.deepEqual(ids(orderProjects(projects, rooms, 'nonsense')), ['b', 'a', 'c', 'd'], 'an unknown sort keeps creation order');
assert.deepEqual(ids(orderProjects([{ id: 'x', name: 'Project 10' }, { id: 'y', name: 'Project 2' }], [], 'name')), ['y', 'x'], 'numbers sort naturally');

// A saved order wins; new projects follow it in creation order; ids that no longer exist are ignored.
assert.deepEqual(ids(orderProjects(projects, rooms, 'manual', ['c', 'gone', 'b'])), ['c', 'b', 'a', 'd']);

// Rooms: the home room leads; name and activity order the rest; manual leaves creation order.
const inside = [{ id: 't2', title: 'zeta', kind: 'task', last_activity_at: '2026-10-01' }, { id: 'h', title: 'Home', kind: 'home', last_activity_at: '2026-09-01' }, { id: 't1', title: 'Alpha', kind: 'task', last_activity_at: '2026-10-03' }];
assert.deepEqual(ids(orderRooms(inside, 'name')), ['h', 't1', 't2']);
assert.deepEqual(ids(orderRooms(inside, 'activity')), ['h', 't1', 't2']);
assert.deepEqual(ids(orderRooms(inside, 'manual')), ['h', 't2', 't1']);

// Moves edit the full manual order.
assert.deepEqual(Array.from(manualSequence(projects, [])), ['b', 'a', 'c', 'd']);
assert.deepEqual(Array.from(moveProject(projects, [], 'a', -1)), ['a', 'b', 'c', 'd']);
assert.deepEqual(Array.from(moveProject(projects, [], 'a', 1)), ['b', 'c', 'a', 'd']);
assert.deepEqual(Array.from(moveProject(projects, [], 'b', -1)), [], 'the first project cannot move up: the order is unchanged');
assert.deepEqual(Array.from(moveProject(projects, ['b', 'a', 'c', 'd'], 'd', 1)), ['b', 'a', 'c', 'd'], 'the last project cannot move down');
assert.deepEqual(Array.from(moveProject(projects, [], 'nope', 1)), [], 'an unknown project does not move');
// Drag and drop: before or after a target.
assert.deepEqual(Array.from(placeProject(projects, [], 'd', 'b', false)), ['d', 'b', 'a', 'c']);
assert.deepEqual(Array.from(placeProject(projects, [], 'b', 'd', true)), ['a', 'c', 'd', 'b']);
assert.deepEqual(Array.from(placeProject(projects, [], 'a', 'a', true)), [], 'dropping on itself does nothing');
assert.deepEqual(Array.from(placeProject(projects, [], 'a', 'nope', true)), [], 'an unknown target does nothing');
// A moved order is the whole list, saved as it is.
const moved = Array.from(moveProject(projects, [], 'c', -1));
assert.deepEqual(ids(orderProjects(projects, rooms, 'manual', moved)), ['b', 'c', 'a', 'd']);
// A carried order that lists only some projects (the pins of an older version) puts them first; the rest follow in creation order.
assert.deepEqual(ids(orderProjects(projects, rooms, 'manual', ['d', 'c'])), ['d', 'c', 'b', 'a']);
console.log('sidebar order test passed');
