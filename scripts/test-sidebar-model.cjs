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
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { matchesRoom, treeOf, leafView, activityOrder, roomUnread, roomMentioned, roomType, roomStatus, tracksStatus, projectStatusCounts, memoProjectStatusCounts, AGENT_CHAT_PROJECT, countedActive, rowMenu };', context);
const { matchesRoom, treeOf, leafView, activityOrder, roomUnread, roomMentioned, roomType, roomStatus, tracksStatus, projectStatusCounts, memoProjectStatusCounts, AGENT_CHAT_PROJECT, countedActive, rowMenu } = context.api;

// A general room by default: it counts every unread message, which is what the nesting and folding checks below are about. Task and research rooms
// (told by their status, counting only what addresses the person) are checked further down.
const room = (id, project, extra = {}) => ({ id, project_id: project, title: id, kind: 'task', purpose: 'general', lifecycle: 'open', unread_count: 0, last_activity_at: '', ...extra });
const projects = new Map([['a', { id: 'a', name: 'Alpha' }], ['b', { id: 'b', name: 'Beta' }], ['x', { id: 'x', name: 'Old', archived_at: '2026-01-01T00:00:00Z' }]]);
const map = list => new Map(list.map(item => [item.id, item]));
const state = (list, extra = {}) => ({ projects, rooms: map(list), filter: '', sort: 'name', manual: [], collapsed: new Set(), active: '', archiveFilter: new Map(), offers: [], ...extra });
// Objects made inside the vm have its own prototypes, so they are compared as JSON.
const same = (actual, expected, message) => assert.deepEqual(JSON.parse(JSON.stringify(actual)), expected, message);
const names = nodes => nodes.map(node => node.room?.id ?? node.node?.kind ?? node.id);

const statusRoom = (id, patch = {}) => room(id, 'a', { purpose: 'task', ...patch });
const statusRooms = [statusRoom('needs', { finished_unseen: true }), statusRoom('question', { attention_open: 1, working: true }), statusRoom('waiting', { gate: 'paused' }), statusRoom('working', { working: true }), statusRoom('idle'), statusRoom('closed', { lifecycle: 'closed', working: true }), room('dm', 'a', { kind: 'dm', purpose: 'task', working: true }), statusRoom('other', { project_id: 'b', working: true })];
same(projectStatusCounts('a', statusRooms), { needs: 2, waiting: 1, working: 1 });
same(projectStatusCounts('a', [statusRoom('held', { gate: 'paused', working: true })]), { needs: 0, waiting: 1, working: 0 }, 'a hold takes priority over activity');
same(projectStatusCounts('a', [statusRoom('idle')]), { needs: 0, waiting: 0, working: 0 });
assert.equal(projectStatusCounts('a', [], true), null, 'a project still loading has unknown counts');
const selectStatus = memoProjectStatusCounts();
const firstStatus = selectStatus('a', statusRooms);
assert.equal(selectStatus('a', [...statusRooms]), firstStatus, 'unchanged counts keep their memoised tuple');
assert.notEqual(selectStatus('a', [statusRoom('working', { working: true })]), firstStatus, 'changed counts replace the tuple');

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
same(leafView(room('l', 'a', { kind: 'dm', unread_count: 3 }), 'other', true), { room: room('l', 'a', { kind: 'dm', unread_count: 3 }), nest: false, chip: true, controls: null, children: [], tail: [], collapsible: false, folded: false, unread: 3, mentioned: false, below: false, mention: false });
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
// An agent that finished with a message the person has not seen (the server derives it from their read cursor) shows as Finished, below Waiting and Working.
same(statusOf({ finished_unseen: true }), ['finished', 'Finished']);
same(statusOf({ finished_unseen: true, attention_open: 1 }), ['waiting', 'Waiting'], 'a person being waited on outranks a finished agent');
same(statusOf({ finished_unseen: true, working: true }), ['working', 'Working'], 'an agent working outranks a finished one');
same(statusOf({ finished_unseen: true, purpose: 'research', user_message_count: 3 }), ['finished', 'Finished'], 'a research report not yet seen is finished, not done');
same(statusOf({ finished_unseen: false, purpose: 'research', user_message_count: 3 }), ['done', 'Done'], 'once seen it reads as done');
assert.equal(statusOf({ finished_unseen: true, lifecycle: 'closed' }), null);
assert.equal(statusOf({ finished_unseen: true, kind: 'dm' }), null, 'a direct message keeps its unread count instead');
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

// The mention dot: a child room with an unread message that names the person shows one (the child shows no count); a home row and a direct message
// have their own bubble and no dot; the open room and an archived room are read. A folded row shows the dot when any room under it is mentioned.
const mentionOf = (extra, active = '') => roomMentioned({ id: 'r', kind: 'task', lifecycle: 'open', unread_count: 9, unread_addressed: 2, ...extra }, active);
assert.equal(mentionOf({}), true, 'a task room with an addressed message is mentioned');
assert.equal(mentionOf({ purpose: 'general' }), true, 'so is a general room');
assert.equal(mentionOf({ unread_addressed: 0, unread_count: 20 }), false, 'chatter alone is no mention');
assert.equal(mentionOf({ unread_addressed: undefined }), false, 'an older server that sends no addressed count shows none');
assert.equal(mentionOf({}, 'r'), false, 'the open room is read');
assert.equal(mentionOf({ lifecycle: 'closed' }), false, 'an archived room shows none');
assert.equal(mentionOf({ kind: 'home' }), false, 'a home row has its bubble');
assert.equal(mentionOf({ kind: 'dm' }), false, 'a direct message has its bubble');
const mention = id => { const walk = (list, found) => list.reduce((hit, view) => hit || (view.room.id === id ? view : walk(view.children, null)), found); return walk(tree.sections, null).mention; };
const mentionRooms = [home, room('gen', 'a', { parent_id: 'home' }), room('deep', 'a', { parent_id: 'gen', purpose: 'task', unread_addressed: 1 }), room('quiet', 'a', { parent_id: 'home', purpose: 'task', unread_count: 30 })];
tree = treeOf(state(mentionRooms));
assert.deepEqual([mention('home'), mention('gen'), mention('deep'), mention('quiet')], [false, false, true, false], 'expanded: only the mentioned room shows the dot');
tree = treeOf(state(mentionRooms, { collapsed: new Set(['gen']) }));
assert.deepEqual([mention('home'), mention('gen')], [false, true], 'a folded child shows the dot of the room under it');
tree = treeOf(state(mentionRooms, { collapsed: new Set(['home']) }));
assert.deepEqual([mention('home'), mention('gen')], [true, false], 'a folded home shows the dot of any descendant, at any depth');
assert.equal(tree.sections[0].unread, 0, 'and still adds no count');
tree = treeOf(state(mentionRooms.map(item => (item.id === 'deep' ? { ...item, unread_addressed: 0 } : item)), { collapsed: new Set(['home']) }));
assert.equal(mention('home'), false, 'read clears the dot');
tree = treeOf(state(mentionRooms, { collapsed: new Set(['home']), active: 'deep' }));
assert.equal(mention('home'), false, 'the open room does not light the folded home');

// Mark unread in the open room: the room the person marked (`held`) shows its count and dot although it is open; any other open room still reads
// as read. countedActive names the room that counts as read for being open.
assert.equal(countedActive('r', ''), 'r');
assert.equal(countedActive('r', 'r'), '', 'the marked open room does not count as read');
assert.equal(countedActive('r', 'other'), 'r', 'a mark held for a room that is no longer open changes nothing');
assert.equal(countedActive('', ''), '');
tree = treeOf(state([room('home', 'a', { kind: 'home', unread_count: 4 }), room('deep', 'a', { parent_id: 'home', purpose: 'task', unread_count: 2, unread_addressed: 1 })], { active: 'home', held: 'home' }));
assert.equal(tree.sections[0].unread, 4, 'the open home marked unread shows its own count (the home bubble)');
tree = treeOf(state([room('home', 'a', { kind: 'home', unread_count: 4 }), room('deep', 'a', { parent_id: 'home', purpose: 'task', unread_count: 2, unread_addressed: 1 })], { active: 'deep', held: 'deep' }));
assert.equal(mention('deep'), true, 'the open child marked unread shows its mention dot');
tree = treeOf(state([room('home', 'a', { kind: 'home', unread_count: 4 }), room('deep', 'a', { parent_id: 'home', purpose: 'task', unread_count: 2, unread_addressed: 1 })], { active: 'deep', held: '' }));
assert.equal(mention('deep'), false, 'without the mark the open room is read');
same(leafView(room('d', 'a', { kind: 'dm', unread_count: 3 }), countedActive('d', 'd')).unread, 3, 'a direct message marked unread while open keeps its count');

// The row menu: Mark as read while something is unread, else Mark unread; then what the person may do with the room.
const plan = (extra, options) => JSON.parse(JSON.stringify(rowMenu({ id: 'r', kind: 'task', purpose: 'task', lifecycle: 'open', unread_count: 0, ...extra }, options)));
assert.equal(plan({ unread_count: 2 }).read, 'read');
assert.equal(plan({ unread_addressed: 1 }).read, 'read', 'a mention alone counts as unread');
assert.equal(plan({}).read, 'unread');
assert.equal(plan({ kind: 'home' }).settings, 'Project settings');
assert.equal(plan({}).settings, 'Room settings');
assert.equal(plan({}, { role: 'owner' }).archive, true, 'the owner may archive an open task room');
assert.equal(plan({}, { role: 'operator' }).archive, true, 'so may an operator');
assert.equal(plan({}, { role: 'viewer' }).archive, false, 'a viewer may not');
assert.equal(plan({ kind: 'home' }, { role: 'owner' }).archive, false, 'a home room is never archived');
assert.equal(plan({ kind: 'dm' }, { role: 'owner' }).archive, false, 'nor a direct message');
assert.equal(plan({ lifecycle: 'closing' }, { role: 'owner' }).archive, false, 'nor a room already closing');
assert.equal(plan({ lifecycle: 'closed' }, { role: 'owner' }).reopen, true, 'a closed room reopens');
assert.equal(plan({ kind: 'dm' }).newRoom, false, 'no task room under a direct message');
assert.equal(plan({}).newRoom, true);
assert.equal(plan({ agent_chat: true, kind: 'dm' }).leave, true, 'a chat between agents may be left');
assert.equal(plan({}).leave, false);
same(plan({ lifecycle: 'closed', unread_count: 1 }, { role: 'owner', archivedProject: true }), { read: 'read', settings: 'Room settings', notify: true, newRoom: false, hide: true, archive: false, reopen: false, leave: false },
  'an archived project keeps only the read marks, the settings, hiding and the notification choices');

console.log('sidebar model: ok');
