// The v1 import wizard's pure rules (web/js/pages/import-v1/model.js), run in a vm. import-tree.js runs in its own vm here, so the room
// wording is checked against the room-choice rules the wizard really passes in.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.join(__dirname, '..', 'web', 'js');
const load = (file, names) => {
  const source = fs.readFileSync(path.join(web, file), 'utf8');
  const context = {};
  vm.createContext(context);
  vm.runInContext(`${source.replace(/^export /gm, '')}\nglobalThis.api = { ${names} };`, context);
  return { source, api: context.api };
};
const loaded = load('pages/import-v1/model.js', 'SYSTEM_ROOM, COUNT_LABELS, countRank, stepsFor, draftKey, skippedPersonWarning, newLogin, defaultPeople, dmTitle, roomDestination, macroBody, footerTotals, accountingRows, reportCountRows, stepStatus, ownerHint, macroCards, summaryLists, number, NEW_PROJECT, NEW_ACCOUNT, IMPORT_PHASES, ONLY_CHANCE');
assert.ok(!/^import /m.test(loaded.source), 'model.js stays import-free so it can run here');
const m = loaded.api;
const tree = load('import-tree.js', 'roomChoice, parseChoice, planTree').api;
const rules = { roomChoice: tree.roomChoice, parseChoice: tree.parseChoice, planTree: tree.planTree };
// Values built inside the vm have another realm's prototypes, so compare them after a JSON round trip.
const plain = value => JSON.parse(JSON.stringify(value));

// The draft key must never change: a saved draft under the old key would be lost.
assert.equal(m.draftKey('abc'), 'aimebu2.import-draft.abc');
assert.equal(m.SYSTEM_ROOM, '_system');
assert.equal(m.number(1234567), '1,234,567');
assert.equal(m.number(undefined), '0');

// Steps: a page with nothing to show for this backup is skipped; Projects and Summary always show.
assert.deepEqual(plain(m.stepsFor({})).map(step => step.id), ['projects', 'summary']);
assert.deepEqual(plain(m.stepsFor({ rooms: [{ id: '_system' }] })).map(step => step.id), ['projects', 'summary'], 'the system room alone is no Rooms page');
const full = {
  humans: ['ann'], rooms: [{ id: 'r' }, { id: 'd', dm: true }], counts: { memory: 1 }, macros: { a: 'x' }, roles: [{ key: 'k' }],
};
assert.deepEqual(plain(m.stepsFor(full)), [
  { id: 'projects', title: 'Projects' }, { id: 'people', title: 'People' }, { id: 'rooms', title: 'Rooms' }, { id: 'dms', title: 'Direct messages' },
  { id: 'memory', title: 'Memory' }, { id: 'macros', title: 'Macros' }, { id: 'roles', title: 'Roles' }, { id: 'summary', title: 'Summary' },
]);
assert.deepEqual(plain(m.stepsFor({ memory_projects: ['p'] })).map(step => step.id), ['projects', 'memory', 'summary'], 'memory scopes alone show the Memory page');

// Summary order: the history first, unknown keys in the middle, bookkeeping last.
assert.ok(m.countRank('projects') < m.countRank('messages'));
assert.equal(m.countRank('something_new'), 100);
assert.equal(m.countRank('sounds'), 200);

// Skipping a person: what is lost is counted, and the sentence reads the same with nothing at stake.
assert.equal(m.skippedPersonWarning(undefined), 'No account: their messages keep their name as a label, and nobody can sign in as them. Choose an account, or "Create a new account…", to keep them.');
assert.equal(m.skippedPersonWarning({ memory_profiles: 1, reactions: 2, read_positions: 1, room_view: 3 }),
  'No account: their messages keep their name as a label, and nobody can sign in as them. Their 1 memory profile, 2 reactions, 1 read position, 3 pinned or hidden rooms would not be imported, and an import cannot be repeated. Choose an account, or "Create a new account…", to keep them.');

// Logins: lowercase, at least 3 characters, starting with a letter, never one already taken here or chosen for someone else.
const preview = { destination_users: ['owner', 'Bob'], humans: ['owner', 'Bob', 'Ann Lee', '42'] };
assert.equal(m.newLogin('Ann Lee', preview, { new_users: {} }), 'ann-lee');
assert.equal(m.newLogin('bob', preview, { new_users: {} }), 'bob-2', 'a login in use here is taken, whatever its case');
assert.equal(m.newLogin('ann lee', preview, { new_users: { x: { login: 'ANN-LEE' } } }), 'ann-lee-2', 'a login chosen for someone else is taken');
assert.equal(m.newLogin('42', preview, { new_users: {} }), 'user');
assert.equal(m.newLogin('Al', preview, { new_users: {} }), 'user-al');
assert.equal(m.newLogin('x'.repeat(40), preview, { new_users: {} }).length, 28);

// Default people: only an undecided skip becomes a new account; a decided person keeps their choice.
const mapping = { humans: { owner: 'owner', Bob: '-', 'Ann Lee': '-', 42: 'owner' }, new_users: {} };
m.defaultPeople(preview, mapping, human => human !== 'Bob');
assert.deepEqual(plain(mapping), { humans: { owner: 'owner', Bob: '-', 'Ann Lee': 'ann-lee', 42: 'owner' }, new_users: { 'Ann Lee': { login: 'ann-lee', role: 'viewer' } } });
m.defaultPeople(preview, mapping);
assert.equal(mapping.humans.Bob, 'bob-2');
const fresh = { humans: { owner: '-' } };
m.defaultPeople({ humans: ['owner'] }, fresh);
assert.deepEqual(plain(fresh.new_users), { owner: { login: 'owner', role: 'viewer' } }, 'new_users is created when missing');

// DM titles follow the person mapping: the owner's own accounts and skipped people are left out, unless nobody else is left.
const dmMapping = { owner_login: 'me', humans: { a: 'me', b: '-', c: 'carol' } };
const people = [{ kind: 'user', id: 'a', name: 'A' }, { kind: 'agent', id: 'x', name: 'Agent' }];
assert.equal(m.dmTitle({ participants: people }, dmMapping), 'Agent');
assert.equal(m.dmTitle({ participants: [...people, { kind: 'user', id: 'c', name: 'Carol' }] }, dmMapping), 'Agent · Carol');
assert.equal(m.dmTitle({ participants: [{ kind: 'user', id: 'a', name: 'A' }, { kind: 'user', id: 'b', name: 'B' }] }, dmMapping), 'A · B', 'with nobody else left, everyone names it');
assert.equal(m.dmTitle({ id: 'd1', title: 'Chat' }, dmMapping), 'Chat');
assert.equal(m.dmTitle({ id: 'd1' }, dmMapping), 'd1');

// Room destinations, through import-tree.js's real rules.
const roomMapping = {
  projects: { p: { name: 'Proj' } },
  rooms: {
    home: { project: 'p', target: 'root' }, gen: { project: 'p', target: 'sub', title: 'general', lifecycle: 'open' },
    own: { project: 'p', lifecycle: 'closed' }, off: { skip: true, project: 'p' }, dm: { project: 'p' }, dmoff: { skip: true },
    merged1: { project: 'p', target: 'sub', title: 'team', lifecycle: 'closed' }, merged2: { project: 'p', target: 'sub', title: 'team', lifecycle: 'open' },
  },
};
const rooms = ['home', 'gen', 'own', 'off', 'merged1', 'merged2'].map(id => ({ id, messages: 1 })).concat([{ id: 'dm', dm: true, messages: 1 }, { id: 'dmoff', dm: true, messages: 1 }]);
const planned = tree.planTree(roomMapping, rooms);
const where = id => m.roomDestination(rooms.find(room => room.id === id), roomMapping, planned, rules);
assert.equal(where('home'), '⌂ Proj');
assert.equal(where('gen'), 'Proj › general');
assert.equal(where('own'), 'Proj › own (archived)', 'a room kept as itself is archived by its own flag');
assert.equal(where('off'), 'not imported');
assert.equal(where('merged1'), 'Proj › team', 'a merged target is open when any room in it is open');
assert.equal(where('dm'), 'direct message in Proj');
assert.equal(where('dmoff'), 'not imported');
assert.equal(m.roomDestination({ id: 'gone' }, roomMapping, planned, rules), 'not imported', 'a room with no choice is not imported');

// Macro bodies: empty, short in full, long folded behind the first non-blank line.
assert.deepEqual(plain(m.macroBody('')), { kind: 'empty', text: '(empty)' });
assert.deepEqual(plain(m.macroBody('a\nb')), { kind: 'short', text: 'a\nb' });
const long = `\n${'x'.repeat(90)}\n${'y'.repeat(300)}`;
assert.deepEqual(plain(m.macroBody(long)), { kind: 'long', text: long, summary: `${'x'.repeat(80)}… (${long.length} characters)` });
assert.equal(m.macroBody('1\n2\n3\n4\n5\n6\n7').kind, 'long', 'more than six lines folds');
assert.equal(m.macroBody('z'.repeat(1500)).summary, `${'z'.repeat(80)}… (1,500 characters)`);

// Footer totals.
const totalsPreview = {
  suggested: { projects: { p: {}, q: {} } },
  rooms: [{ id: 'a', messages: 1000 }, { id: 'b', messages: 5 }, { id: 'd', dm: true, messages: 9 }, { id: '_system', messages: 7 }],
  counts: { memory: 10 }, memory_counts: { projects: { s1: 3, s2: 2 }, profile_owners: { ann: 1, bob: 2 }, shared: 2 },
};
const totalsMapping = { projects: { p: {} }, rooms: { a: { project: 'p' }, b: { skip: true } }, memory_projects: { s1: 'p', s2: '-' }, humans: { ann: 'ann', bob: '-' } };
assert.equal(m.footerTotals('projects', totalsPreview, totalsMapping), '1 project selected · 1 project removed, their rooms are not imported');
assert.equal(m.footerTotals('projects', totalsPreview, { ...totalsMapping, projects: { p: {}, q: {} } }), '2 projects selected');
assert.equal(m.footerTotals('rooms', totalsPreview, totalsMapping), '1 of 2 rooms selected · 1,000 messages go in · 1 room not imported');
assert.equal(m.footerTotals('memory', totalsPreview, totalsMapping), '6 of 10 memory records selected · 4 records not imported', 'a skipped person\'s profile is not counted');
assert.equal(m.footerTotals('memory', totalsPreview, { ...totalsMapping, skip_profiles: true, skip_shared_notes: true }), '3 of 10 memory records selected · 7 records not imported');
assert.equal(m.footerTotals('people', totalsPreview, totalsMapping), '');

// Accounting: the headline counts what is brought across (imported or already there); only positive parts are listed.
assert.equal(m.accountingRows({}), null);
assert.equal(m.accountingRows({ accounting: [{ key: 'messages', total: 0, parts: [] }] }), null);
const accounting = plain(m.accountingRows({ accounting: [
  { key: 'messages', total: 1200, parts: [{ key: 'imported', count: 1000 }, { key: 'already_present', count: 50 }, { key: 'room_skipped', count: 150, items: ['r1', 'r2'] }, { key: 'other', count: 0 }] },
  { key: 'rooms', total: 3, parts: [{ key: 'imported', count: 2 }, { key: 'room_skipped', count: 1, items: ['r1'] }] },
  { key: 'reactions', total: 4, parts: [{ key: 'imported', count: 4 }, { key: 'brand_new', count: 0 }] },
  { key: 'mystery', total: 2, parts: [{ key: 'some_part', count: 2 }] },
] }));
assert.equal(accounting.headline, 'Bringing across 1,050 of 1,200 messages, 2 of 3 rooms.');
assert.equal(accounting.left, 150);
assert.equal(accounting.leftNote, '150 messages are left out; the lists below say why.');
assert.deepEqual(accounting.groups[0], {
  key: 'messages', title: 'Messages: 1,200 in v1', note: '150 left out', open: true, total: '1,200',
  rows: [
    { key: 'imported', label: 'Imported', count: '1,000', skipped: false, items: [], itemsLabel: '' },
    { key: 'already_present', label: 'Already in v2', count: '50', skipped: false, items: [], itemsLabel: '' },
    { key: 'room_skipped', label: 'In rooms you chose not to import', count: '150', skipped: true, items: ['r1', 'r2'], itemsLabel: 'Show all 2' },
  ],
});
assert.equal(accounting.groups[1].rows[1].label, 'Rooms you chose not to import', 'rooms word their own skips');
assert.equal(accounting.groups[1].rows[1].itemsLabel, 'Show it');
assert.deepEqual([accounting.groups[2].note, accounting.groups[2].open, accounting.groups[2].rows.length], ['all brought across', false, 1]);
assert.deepEqual([accounting.groups[3].title, accounting.groups[3].rows[0].label], ['mystery: 2 in v1', 'some part'], 'an unknown key falls back to its raw name');
assert.equal(plain(m.accountingRows({ accounting: [{ key: 'rooms', total: 1, parts: [{ key: 'imported', count: 1 }] }] })).leftNote, '');

// The counts table: all-zero rows go, "Skipped" shows only when something is in it, and the rows keep the reading order.
const counts = plain(m.reportCountRows({ imported: { messages: 1200, projects: 1, zero: 0, sounds: 2, brand_new: 3 }, skipped: { messages: 0 } }));
assert.equal(counts.showSkipped, false);
assert.deepEqual(counts.rows.map(row => row.key), ['projects', 'messages', 'brand_new', 'sounds']);
assert.deepEqual(counts.rows[1], { key: 'messages', title: 'Messages', explanation: 'Chat history, with its original numbers.', imported: '1,200', skipped: '0', skippedNonZero: false });
assert.deepEqual([counts.rows[2].title, counts.rows[2].explanation], ['brand new', '']);
const skipped = plain(m.reportCountRows({ imported: { rooms: 1 }, skipped: { macros: 2 } }));
assert.equal(skipped.showSkipped, true);
assert.deepEqual(skipped.rows.map(row => [row.key, row.skipped, row.skippedNonZero]), [['rooms', '0', false], ['macros', '2', true]]);
assert.deepEqual(plain(m.reportCountRows({})), { showSkipped: false, rows: [] });

// Page status lines.
const statusPreview = { counts: { rooms: 3, messages: 9, attachments: 1, memory: 4 }, rooms: [{ id: 'a' }, { id: 'b' }, { id: '_system' }, { id: 'd1', dm: true }, { id: 'd2', dm: true }] };
const statusMapping = { rooms: { a: { project: 'p' }, b: { skip: true, project: 'p' }, d1: { skip: false }, d2: { skip: true } } };
assert.match(m.stepStatus('projects', statusPreview, statusMapping), /^The backup holds 3 rooms, 9 messages, 1 attachments and 4 memory records\. First/);
assert.match(m.stepStatus('rooms', statusPreview, statusMapping), /^1 of 2 rooms will be imported\./, 'the system room and direct messages are not rooms here');
assert.match(m.stepStatus('dms', statusPreview, statusMapping), /^1 of 2 chats are going to be imported\./);
assert.match(m.stepStatus('dms', statusPreview, { rooms: { d1: { skip: true }, d2: { skip: true } } }), /^None of the 2 chats are going/);
assert.match(m.stepStatus('dms', statusPreview, { rooms: {} }), /^All 2 chats are going/, 'a chat with no choice yet counts as picked, as before');
assert.match(m.stepStatus('dms', { rooms: [{ id: 'd', dm: true }] }, { rooms: { d: { skip: true } } }), /^The chat is not going to be imported\./);
assert.match(m.stepStatus('dms', { rooms: [{ id: 'd', dm: true }] }, { rooms: { d: {} } }), /^The chat is going to be imported\./);
assert.match(m.stepStatus('memory', statusPreview, statusMapping), /^The backup holds 4 memory records\./);
assert.match(m.stepStatus('people', statusPreview, statusMapping), /messages show under the account you map them to/);
assert.match(m.stepStatus('roles', statusPreview, statusMapping), /^Importable v1 roles/);
assert.equal(m.stepStatus('summary', statusPreview, statusMapping), '');

// The owner hint: only with several people, none suggested as or mapped to the owner.
const hintPreview = { humans: ['ann', 'bob'], suggested: { humans: { ann: '-', bob: '-' } } };
assert.match(m.ownerHint(hintPreview, { owner_login: 'sam', humans: { ann: '-', bob: '-' } }), /^None of these people is mapped to your account \(sam\)/);
assert.equal(m.ownerHint(hintPreview, { owner_login: 'sam', humans: { ann: 'sam', bob: '-' } }), '', 'someone mapped to the owner');
assert.equal(m.ownerHint({ ...hintPreview, suggested: { humans: { ann: 'sam' } } }, { owner_login: 'sam', humans: { ann: '-', bob: '-' } }), '', 'the server suggested an owner: a manual unmap is not a mismatch');
assert.equal(m.ownerHint({ humans: ['ann'], suggested: { humans: {} } }, { owner_login: 'sam', humans: { ann: '-' } }), '', 'a single person');

// Macro cards: conflicts first, then new, same, imported; the status counts what is chosen, in the Summary's buckets.
const cards = plain(m.macroCards(
  { macros: { a: 'x', b: 'v1', c: 'same', d: 'done', e: 'v1e' }, imported_macros: ['d'], macro_defaults: ['a'], macros_left_out: { z: 'deleted_in_v1', y: 'v1_builtin_example' } },
  { skip_macros: ['e'], macro_conflicts: { b: 'use_v1' } },
  { b: 'v2', c: 'same', e: 'v2e' },
));
assert.deepEqual(cards.keys, ['a', 'b', 'c', 'd', 'e']);
assert.equal(cards.status, '4 of 5 macros chosen: 1 new, 1 replace the v2 text, 1 already match v2, 1 already imported from this backup.');
assert.equal(cards.leftOut, "Left out: <y> (v1's built-in example, unchanged; v2 has its own), <z> (deleted in v1).");
assert.deepEqual(cards.cards.map(card => [card.key, card.state, card.label]), [['b', 'conflict', 'Conflict: replacing the v2 text'], ['e', 'conflict', 'Conflict: keeping the v2 text'], ['a', 'new', 'New'], ['c', 'same', 'Already in v2'], ['d', 'imported', 'Already imported']]);
assert.deepEqual(cards.cards.find(card => card.key === 'a'), { key: 'a', state: 'new', label: 'New', legend: '<a> · v1 seeded default ', imported: false, included: true, body: 'x', current: null, exists: false, same: false, conflict: 'keep_v2' });
assert.deepEqual([cards.cards[1].included, cards.cards[1].current, cards.cards[1].exists, cards.cards[1].same], [false, 'v2e', true, false]);
assert.equal(plain(m.macroCards({ macros: { a: 'x' } }, { skip_macros: ['a'] }, {})).status, '0 of 1 macros chosen.');

// Summary lists: in reading order, empty ones left out, room placements through import-tree.js.
const lists = plain(m.summaryLists(
  { skipped_no_project: ['solo'], new_accounts: [{ login: 'bobby', role: 'operator', v1_name: 'bob' }], role_conflicts: { r2: 'keep_v2', r1: 'use_v1' }, macro_conflicts: { m: 'use_v1' }, not_carried: { plans: 31 }, skipped_memory: [{ scope: 'project', scope_key: 'p', id: 'k1', reason: 'gone' }], carried_over_files: ['f.txt'] },
  { rooms: [{ id: 'home' }, { id: '_system' }, { id: 'dmoff', dm: true, participants: [{ kind: 'agent', id: 'x', name: 'Agent' }] }] },
  { owner_login: 'me', humans: {}, projects: { p: { name: 'Proj' } }, rooms: { home: { project: 'p', target: 'root' }, dmoff: { skip: true } } },
  rules,
));
assert.deepEqual(lists.map(list => list.title), ['Where each room goes', 'Rooms not imported: no project', 'Accounts created', 'Direct messages not imported', 'Memory skipped', 'Role conflicts', 'Macro conflicts', 'Not carried over', 'Files kept for v2']);
assert.deepEqual(lists[0], { title: 'Where each room goes', items: ['home → ⌂ Proj', 'Agent → not imported · dmoff'], note: 'Check each placement before importing.' });
assert.deepEqual(lists[2].items, ['bobby (operator) for bob']);
assert.deepEqual(lists[3], { title: 'Direct messages not imported', items: ['Agent · dmoff'], note: '' });
assert.deepEqual(lists[5].items, ['r1: use v1 role', 'r2: keep v2 role'], 'sorted by key');
assert.deepEqual(lists[6].items, ['<m>: use v1 text']);
assert.deepEqual(lists[7].items, ['plans: 31 rows']);

console.log('import-v1 model tests passed');
