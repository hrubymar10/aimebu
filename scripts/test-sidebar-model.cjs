// The room list's tree is DOM-free module code, so this runs it in a vm: the filter, the nesting of rooms under their parent,
// the archived and hidden tails, the direct messages, the offers and the fold state.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'sidebar', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { matchesRoom, treeOf, leafView, activityOrder, AGENT_CHAT_PROJECT };', context);
const { matchesRoom, treeOf, leafView, activityOrder, AGENT_CHAT_PROJECT } = context.api;

const room = (id, project, extra = {}) => ({ id, project_id: project, title: id, kind: 'task', lifecycle: 'open', unread_count: 0, last_activity_at: '', ...extra });
const projects = new Map([['a', { id: 'a', name: 'Alpha' }], ['b', { id: 'b', name: 'Beta' }], ['x', { id: 'x', name: 'Old', archived_at: '2026-01-01T00:00:00Z' }]]);
const map = list => new Map(list.map(item => [item.id, item]));
const state = (list, extra = {}) => ({ projects, rooms: map(list), filter: '', sort: 'name', manual: [], collapsed: new Set(), active: '', archiveFilter: new Map(), offers: [], ...extra });
// Objects made inside the vm have its own prototypes, so they are compared as JSON.
const same = (actual, expected, message) => assert.deepEqual(JSON.parse(JSON.stringify(actual)), expected, message);
const names = nodes => nodes.map(node => node.room?.id ?? node.node?.kind ?? node.id);

assert.equal(AGENT_CHAT_PROJECT, 'prj_agentdm');
assert.equal(matchesRoom(room('Deploy', 'a'), ' dep ', projects), true);
assert.equal(matchesRoom(room('x', 'b'), 'beta', projects), true, 'the project name matches too');
assert.equal(matchesRoom(room('x', 'b'), 'zzz', projects), false);
assert.equal(matchesRoom(room('x', 'b'), '   ', projects), true);
same([{ last_activity_at: 'a' }, { last_activity_at: 'c' }, {}].sort(activityOrder).map(item => item.last_activity_at || ''), ['c', 'a', '']);

// Nesting: a child sits under its open parent (one level, as drawn), a DM never nests and lists newest first.
const home = room('home', 'a', { kind: 'home' });
let tree = treeOf(state([home, room('child', 'a', { parent_id: 'home', unread_count: 2 }), room('dm1', 'a', { kind: 'dm', last_activity_at: '2026-02-01' }), room('dm2', 'a', { kind: 'dm', last_activity_at: '2026-03-01' })]));
same(names(tree.sections), ['home']);
same(names(tree.sections[0].children), ['child']);
same(names(tree.direct), ['dm2', 'dm1']);
assert.equal(tree.sections[0].nest, true);
assert.equal(tree.sections[0].collapsible, true);
same(tree.sections[0].controls, { first: true, last: false });
assert.equal(tree.sections[0].unread, 0, 'an open parent shows only its own unread count');
assert.equal(tree.many, true, 'project b is live although it has no rooms');
assert.equal(Array.from(tree.collapsible).join(), 'home');

// A folded parent adds its children's unread to itself; the active room counts for nothing.
tree = treeOf(state([home, room('child', 'a', { parent_id: 'home', unread_count: 2 })], { collapsed: new Set(['home']) }));
assert.equal(tree.sections[0].folded, true);
assert.equal(tree.sections[0].unread, 2);
tree = treeOf(state([home, room('routing', 'a', { unread_count: 4 }), room('missing-parent', 'a', { parent_id: 'gone', unread_count: 1 })], { collapsed: new Set(['home']), active: 'routing' }));
same(names(tree.sections), ['home'], 'rooms without a live parent use the project home row');
same(names(tree.sections[0].children), ['missing-parent', 'routing']);
assert.equal(tree.sections[0].unread, 1, 'the folded home counts orphaned children but not the active room');
assert.deepEqual([...tree.holdsActive], ['home'], 'the fallback home holds the active room');
tree = treeOf(state([home, room('child', 'a', { parent_id: 'home', unread_count: 2 })], { active: 'child' }));
assert.equal(tree.sections[0].unread, 0);
// The roots on screen, and the one that holds the open room (itself or a child), for the default fold; nothing is a root while a filter is on.
assert.deepEqual([[...tree.nested], [...tree.holdsActive], [...tree.collapsible]], [['home'], ['home'], ['home']]);
assert.deepEqual([...treeOf(state([home, room('child', 'a', { parent_id: 'home' })], { active: 'elsewhere' })).holdsActive], []);
// Deeper rooms and archived ones hold the project too: the open room's whole chain of parents counts, shown or not.
assert.deepEqual([...treeOf(state([home, room('child', 'a', { parent_id: 'home' }), room('grand', 'a', { parent_id: 'child' })], { active: 'grand' })).holdsActive], ['home'], 'a room two levels down holds its project');
assert.deepEqual([...treeOf(state([home, room('child', 'a', { parent_id: 'home' }), room('old', 'a', { parent_id: 'child', lifecycle: 'closed' })], { active: 'old' })).holdsActive], ['home'], 'an archived room holds its project');
assert.deepEqual([...treeOf(state([home, room('child', 'a', { parent_id: 'home' })], { filter: 'child' })).nested], [], 'a filter marks no root as seen');

// A closed parent sends its child to the project home; a closed room goes to the archived tail of its project (narrowed by the archive search).
tree = treeOf(state([home, room('old', 'a', { lifecycle: 'closed', title: 'Retro' }), room('older', 'a', { lifecycle: 'closed', title: 'Alpha plan' }), room('hid', 'a', { hidden: true }), room('orphan', 'a', { parent_id: 'old' })], { archiveFilter: new Map([['a', ' retro ']]) }));
same(names(tree.sections), ['home']);
same(names(tree.sections[0].children), ['orphan']);
const tail = tree.sections[0].tail;
same(tail.map(node => node.kind), ['archived', 'hidden']);
assert.equal(tail[0].count, 2);
same(names(tail[0].rooms), ['old']);
same(names(tail[1].rooms), ['hid']);

// Filtering drops the tails and the nesting, and says so when nothing matches.
tree = treeOf(state([home, room('child', 'a', { parent_id: 'home', title: 'Deploy' }), room('old', 'a', { lifecycle: 'closed' })], { filter: 'deploy' }));
same(names(tree.sections), ['child']);
assert.equal(tree.filtering, true);
assert.equal(tree.sections[0].nest, false);
assert.equal(tree.sections[0].tail.length, 0);
tree = treeOf(state([home], { filter: 'nothing' }));
assert.equal(tree.noMatch, 'No rooms match “nothing”.');
same(tree.sections, []);

// A project with no home room still shows its tail; several live projects set the chip and the first/last controls.
tree = treeOf(state([room('h1', 'a', { kind: 'home' }), room('h2', 'b', { kind: 'home' }), room('old', 'b', { lifecycle: 'closed' })], { sort: 'name' }));
assert.equal(tree.many, true);
same(tree.sections.filter(node => node.room).map(node => [node.room.id, node.chip, node.controls.first, node.controls.last]), [['h1', true, true, false], ['h2', true, false, true]]);
tree = treeOf(state([room('t1', 'b'), room('old', 'b', { lifecycle: 'closed' })]));
same(names(tree.sections), ['t1', 'archived'], 'the tail follows the rooms when no home room carries it');
assert.equal(tree.sections[1].tailOnly, true);

// Archived projects list their rooms, home first, and only with a match while filtering; offers narrow by title.
tree = treeOf(state([room('r', 'x', { title: 'b' }), room('h', 'x', { kind: 'home', title: 'z' })]));
same(tree.archived.map(entry => [entry.project.id, names(entry.rooms)]), [['x', ['h', 'r']]]);
tree = treeOf(state([room('r', 'x', { title: 'b' })], { filter: 'nothing' }));
same(tree.archived, []);
tree = treeOf(state([], { offers: [{ id: 'o1', title: 'Alpha chat' }, { id: 'o2', title: 'Beta chat' }], filter: 'alpha' }));
same(tree.offers.map(offer => offer.id), ['o1']);

// A leaf row is a view with nothing under it.
same(leafView(room('l', 'a', { unread_count: 3 }), 'other', true), { room: room('l', 'a', { unread_count: 3 }), nest: false, chip: true, controls: null, children: [], tail: [], collapsible: false, folded: false, unread: 3 });
assert.equal(leafView(room('l', 'a', { unread_count: 3 }), 'l').unread, 0);
assert.equal(leafView(room('old', 'a', { lifecycle: 'closed', unread_count: 3 }), 'other').unread, 0, 'archived history does not advertise unread activity');
console.log('sidebar model: ok');
