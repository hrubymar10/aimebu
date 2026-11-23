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
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { matchesRoom, treeOf, leafView, activityOrder, roomUnread, roomType, roomStatus, tracksStatus, AGENT_CHAT_PROJECT };', context);
const { matchesRoom, treeOf, leafView, activityOrder, roomUnread, roomType, roomStatus, tracksStatus, AGENT_CHAT_PROJECT } = context.api;

// A general room by default: it counts every unread message, which is what the nesting and folding checks below are about. Task and research rooms
// (told by their status, counting only what addresses the person) are checked further down.
const room = (id, project, extra = {}) => ({ id, project_id: project, title: id, kind: 'task', purpose: 'general', lifecycle: 'open', unread_count: 0, last_activity_at: '', ...extra });
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

// Rooms nest at any depth: a task under general is a child of general, which gets an expander of its own. Neither carries an unread badge,
// folded or not; a folded child does not sum its descendants.
tree = treeOf(state([home, room('gen', 'a', { parent_id: 'home', unread_count: 1 }), room('deep', 'a', { parent_id: 'gen', unread_count: 3 })]));
same(names(tree.sections[0].children), ['gen']);
same(names(tree.sections[0].children[0].children), ['deep']);
assert.equal(tree.sections[0].children[0].collapsible, true);
assert.equal(tree.sections[0].children[0].unread, 0, 'a child room shows no unread badge');
assert.equal(tree.sections[0].children[0].children[0].unread, 0, 'nor does a grandchild');
tree = treeOf(state([home, room('gen', 'a', { parent_id: 'home' }), room('deep', 'a', { parent_id: 'gen', unread_count: 3 })], { collapsed: new Set(['gen']) }));
assert.equal(tree.sections[0].children[0].folded, true);
assert.equal(tree.sections[0].children[0].unread, 0, 'a folded child does not sum its descendants');
assert.equal(tree.sections[0].unread, 0);
assert.equal(Array.from(tree.collapsible).join(), 'home', 'only project rows take part in the first-seen folding');

// The home row counts only its own unread messages, folded or expanded; nothing under it is added, at any depth.
const deepRooms = [home, room('gen', 'a', { parent_id: 'home', unread_count: 1 }), room('deep', 'a', { parent_id: 'gen', unread_count: 3 }), room('deeper', 'a', { parent_id: 'deep', unread_count: 5 })];
for (const collapsed of [new Set(), new Set(['home'])]) {
  tree = treeOf(state(deepRooms, { collapsed }));
  assert.equal(tree.sections[0].unread, 0, 'the home row with no unread of its own shows none');
}
const homeOwn = { ...home, unread_count: 7 };
for (const collapsed of [new Set(), new Set(['home'])]) {
  tree = treeOf(state([homeOwn, ...deepRooms.slice(1)], { collapsed }));
  assert.equal(tree.sections[0].unread, 7, 'the home row counts its own unread, the same folded or expanded');
}
tree = treeOf(state([homeOwn, ...deepRooms.slice(1)]));
assert.equal(tree.sections[0].children[0].unread, 0, 'an expanded general shows no badge');

// The active room counts for nothing, and a child never adds to the home row.
tree = treeOf(state([home, room('child', 'a', { parent_id: 'home', unread_count: 2 })], { collapsed: new Set(['home']) }));
assert.equal(tree.sections[0].folded, true);
assert.equal(tree.sections[0].unread, 0);
tree = treeOf(state([home, room('routing', 'a', { unread_count: 4 }), room('missing-parent', 'a', { parent_id: 'gone', unread_count: 1 })], { collapsed: new Set(['home']), active: 'routing' }));
same(names(tree.sections), ['home'], 'rooms without a live parent use the project home row');
same(names(tree.sections[0].children), ['missing-parent', 'routing']);
assert.equal(tree.sections[0].unread, 0, 'orphaned children add nothing to the folded home');
assert.deepEqual([...tree.holdsActive], ['home'], 'the fallback home holds the active room');
tree = treeOf(state([homeOwn, room('child', 'a', { parent_id: 'home', unread_count: 2 })], { active: 'home' }));
assert.equal(tree.sections[0].unread, 0, 'the open home room is read');
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
same(leafView(room('l', 'a', { kind: 'dm', unread_count: 3 }), 'other', true), { room: room('l', 'a', { kind: 'dm', unread_count: 3 }), nest: false, chip: true, controls: null, children: [], tail: [], collapsible: false, folded: false, unread: 3 });
assert.equal(leafView(room('l', 'a', { kind: 'dm', unread_count: 3 }), 'l').unread, 0);
assert.equal(leafView(room('old', 'a', { kind: 'dm', lifecycle: 'closed', unread_count: 3 }), 'other').unread, 0, 'archived history does not advertise unread activity');

// What a room is: an icon and tooltip from the server's kind, purpose and agent_chat, never from who is in it.
const typeOf = extra => { const { key, icon, label } = roomType({ kind: 'task', ...extra }); return [key, icon, label]; };
same(typeOf({ kind: 'home' }), ['home', 'house', 'Project home room']);
same(typeOf({}), ['task', 'list-checks', 'Task room'], 'a task room without a purpose is a task room');
same(typeOf({ purpose: 'task' }), ['task', 'list-checks', 'Task room']);
same(typeOf({ purpose: 'research' }), ['research', 'search', 'Research room']);
same(typeOf({ purpose: 'general' }), ['general', 'hash', 'General room']);
same(typeOf({ kind: 'lobby' }), ['general', 'hash', 'General room']);
same(typeOf({ purpose: 'general', team_flow_room_key: 'routing' }), ['routing', 'route', 'Routing room'], 'the team flow\'s routing room has its own icon');
same(typeOf({ purpose: 'task', team_flow_room_key: 'routing' }), ['routing', 'route', 'Routing room'], 'whatever purpose a routing room was stored with');
same(typeOf({ purpose: 'general', team_flow_room_key: 'general' }), ['general', 'hash', 'General room'], 'the flow\'s general and advice rooms stay general rooms');
assert.equal(tracksStatus({ kind: 'task', purpose: 'task', team_flow_room_key: 'routing' }), false, 'a routing room shows no task status');
same(typeOf({ kind: 'dm' }), ['dm', 'human', 'Direct message']);
same(typeOf({ kind: 'dm', agent_chat: true }), ['agent-chat', 'switch', 'Chat between agents']);
same(typeOf({ kind: 'dm', purpose: 'research' }), ['dm', 'human', 'Direct message'], 'a private chat is a chat whatever its purpose says');
same(typeOf({ kind: 'home', purpose: 'general' }), ['home', 'house', 'Project home room']);

// A task or research room shows a status in place of a count; every other room shows no status.
const statusOf = extra => { const status = roomStatus({ kind: 'task', lifecycle: 'open', ...extra }); return status && [status.key, status.label]; };
same(statusOf({}), ['idle', 'Idle']);
same(statusOf({ working: true }), ['working', 'Working']);
same(statusOf({ attention_open: 1 }), ['waiting', 'Waiting']);
same(statusOf({ attention_open: 2, working: true }), ['waiting', 'Waiting'], 'a person being waited on outranks an agent working');
same(statusOf({ attention_open: 1, attention_count: 0 }), ['waiting', 'Waiting'], 'an item the person has seen is still open');
same(statusOf({ lifecycle: 'closing', working: true, attention_open: 1 }), ['closing', 'Closing…'], 'closing outranks the rest');
same(statusOf({ purpose: 'research' }), ['idle', 'Idle'], 'a research room nobody wrote in has no report yet');
same(statusOf({ purpose: 'research', user_message_count: 3 }), ['done', 'Done'], 'a quiet research room that has been written in is done');
same(statusOf({ purpose: 'research', user_message_count: 3, working: true }), ['working', 'Working']);
assert.equal(statusOf({ lifecycle: 'closed' }), null, 'an archived room shows no status');
for (const extra of [{ purpose: 'general' }, { kind: 'home' }, { kind: 'dm' }, { kind: 'lobby' }]) assert.equal(statusOf(extra), null, `${JSON.stringify(extra)} shows no status`);
assert.deepEqual([{}, { purpose: 'research' }, { purpose: 'general' }, { kind: 'home' }, { kind: 'dm' }].map(extra => tracksStatus({ kind: 'task', ...extra })), [true, true, false, false, false]);

// The unread badge: only a project's home room and a direct message carry one (their own count); every other room shows none.
const unreadOf = (extra, active = '') => roomUnread({ id: 'r', kind: 'task', lifecycle: 'open', unread_count: 9, unread_addressed: 2, ...extra }, active);
assert.equal(unreadOf({}), 0, 'a task room shows no count, addressed or not');
assert.equal(unreadOf({ purpose: 'research' }), 0);
assert.equal(unreadOf({ purpose: 'general' }), 0, 'a general room shows no count');
assert.equal(unreadOf({ team_flow_room_key: 'routing' }), 0, 'a routing room shows no count');
assert.equal(unreadOf({ kind: 'lobby' }), 0);
assert.equal(unreadOf({ kind: 'dm' }), 9, 'a direct message counts every unread message');
assert.equal(unreadOf({ kind: 'home' }), 9, 'a home room counts its own unread messages');
assert.equal(unreadOf({ kind: 'home', lifecycle: 'closed' }), 0);
assert.equal(unreadOf({ kind: 'home' }, 'r'), 0, 'the open room is read');
// An addressed message in a task child adds nothing to the folded home: the child keeps its 🔔 (attention_count), which this model does not touch.
tree = treeOf(state([room('home', 'a', { kind: 'home', unread_count: 2 }), room('t1', 'a', { parent_id: 'home', purpose: 'task', unread_count: 20, unread_addressed: 1 }),
  room('g', 'a', { parent_id: 'home', unread_count: 3 })], { collapsed: new Set(['home']) }));
assert.equal(tree.sections[0].unread, 2, 'the folded home shows only its own 2');
console.log('sidebar model: ok');
