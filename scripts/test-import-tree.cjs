// Unit test for web/js/import-tree.js (pure functions, run in a vm like the other web tests).
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'import-tree.js'), 'utf8').replace(/^export /gm, '');
const sandbox = {};
vm.createContext(sandbox);
vm.runInContext(source + '\nthis.api = { roomChoice, applyChoice, choiceValue, parseChoice, planTree, mergeGroups, newProjectAlias, GENERAL };', sandbox);
const api = sandbox.api;
// Values built inside the vm have another realm's prototypes, so compare them after a JSON round trip.
const plain = value => JSON.parse(JSON.stringify(value));
const { roomChoice, applyChoice, choiceValue, parseChoice, newProjectAlias } = api;
const planTree = (...args) => plain(api.planTree(...args));
const mergeGroups = tree => plain(api.mergeGroups(tree));

// roomChoice and applyChoice round-trip.
assert.equal(roomChoice({ skip: true, project: 'p' }, 'a'), 'skip');
assert.equal(roomChoice({ project: '' }, 'a'), 'skip');
assert.equal(roomChoice({ project: 'p', target: 'root' }, 'a'), 'root:p');
assert.equal(roomChoice({ project: 'p', target: 'sub', title: 'general' }, 'a'), 'general:p');
assert.equal(roomChoice({ project: 'p', target: 'sub' }, 'a'), 'general:p');
assert.equal(roomChoice({ project: 'p', target: 'sub', title: 'a' }, 'a'), 'new:p');
assert.equal(roomChoice({ project: 'p', target: 'sub', title: 'other' }, 'a'), 'custom:p');
assert.equal(roomChoice({ project: 'p' }, 'a'), 'new:p', 'the old separate-room target reads as a sub-room named like the v1 room');
for (const value of ['root:p', 'general:p', 'new:p', 'skip']) {
  const spec = applyChoice({ lifecycle: 'open' }, value, 'a');
  assert.equal(roomChoice(spec, 'a'), value);
  assert.equal(spec.lifecycle, 'open', 'a choice keeps the lifecycle');
}
const custom = applyChoice({ project: 'p', target: 'sub', title: 'other' }, 'custom:p', 'a');
assert.equal(custom.title, 'other');

// An alias may hold ':' or '|' (a mapping file can use any text): a choice names exactly the project it was made for.
for (const alias of ['team', 'team|secret', 'a:b', 'root:team', 'with space%', 'ünï']) {
  for (const kind of ['root', 'general', 'new']) {
    const value = choiceValue(kind, alias);
    assert.deepEqual(plain(parseChoice(value)), { kind, alias });
    const spec = applyChoice({}, value, 'r');
    assert.equal(spec.project, alias, `${kind} into ${alias}`);
    assert.equal(roomChoice(spec, 'r'), value);
  }
}
assert.notEqual(choiceValue('root', 'team|secret'), choiceValue('root', 'team'), 'team and team|secret are different choices');
assert.equal(applyChoice({ project: 'x' }, choiceValue('root', 'team|secret'), 'r').project, 'team|secret', 'the case that was split on "|" and landed in team');
assert.equal(applyChoice({}, 'root:%E0%A4%A', 'r').skip, true, 'an undecodable alias is not guessed at');

// planTree: where messages land, merges, drops.
const mapping = { projects: { p: { name: 'Proj' }, q: { name: 'Quiet' } }, rooms: {
  'p-leader': { project: 'p', target: 'root' }, p: { project: 'p', target: 'sub', title: 'general', lifecycle: 'open' },
  extra: { project: 'p', target: 'sub', title: 'general' }, side: { project: 'p', target: 'sub', title: 'side' },
  gone: { skip: true, project: 'p' }, lost: {}, 'dm:a:b': { project: 'p' }, 'dm:c:d': { skip: true },
} };
const rooms = [{ id: 'p-leader', messages: 5 }, { id: 'p', messages: 7 }, { id: 'extra', messages: 2, memory_disabled: true }, { id: 'side', messages: 1 }, { id: 'gone', messages: 9 }, { id: 'lost', messages: 4 },
  { id: 'dm:a:b', messages: 3, dm: true }, { id: 'dm:c:d', messages: 6, dm: true }];
const tree = planTree(mapping, rooms);
assert.deepEqual(tree.projects.map(project => project.alias), ['p'], 'a project nothing lands in is not shown');
const proj = tree.projects[0];
assert.deepEqual(proj.root, { rooms: ['p-leader'], messages: 5, memoryOff: [] });
assert.deepEqual(proj.subs.map(sub => [sub.title, sub.rooms, sub.messages, sub.archived]), [['general', ['p', 'extra'], 9, false], ['side', ['side'], 1, true]]);
assert.deepEqual(proj.subs.map(sub => sub.memoryOff), [['extra'], []], 'a shared target has memory off when any room merged into it did');
assert.deepEqual(tree.dropped, [{ id: 'gone', messages: 9, reason: 'not imported' }, { id: 'lost', messages: 4, reason: 'no project' }]);
assert.deepEqual(tree.direct, [{ id: 'dm:a:b', messages: 3 }]);
assert.deepEqual(mergeGroups(tree), [{ where: '"general" under Proj', rooms: ['p', 'extra'] }]);
assert.equal(mergeGroups(planTree({ projects: { p: { name: 'P' } }, rooms: { a: { project: 'p', target: 'root' }, b: { project: 'p', target: 'root' } } }, [{ id: 'a', messages: 1 }, { id: 'b', messages: 1 }]))[0].where, 'the root of P');
// A default project catches a room with none; with no default it is dropped.
assert.equal(planTree({ projects: { d: { name: 'D' } }, default_project: 'd', rooms: { x: {} } }, [{ id: 'x', messages: 1 }]).projects[0].subs[0].title, 'x');

// Aliases.
assert.equal(newProjectAlias('My Cool App!', new Set()), 'my-cool-app');
assert.equal(newProjectAlias('My Cool App!', new Set(['my-cool-app'])), 'my-cool-app-2');
assert.equal(newProjectAlias('  ', new Set()), 'project');
console.log('import tree test passed');
