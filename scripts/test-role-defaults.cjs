// The pure helpers behind the role defaults on Settings → Harnesses: where a model sits in a role's list, turning it on or off, moving,
// removing, and the body that saves a role. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'role-defaults-logic.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { MAX_ROLE_DEFAULTS, PICK_MODES, positionOf, rolesFor, withModel, movedByKey, canAddDefault, roleBody };', context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const a = { launcher_id: 'l1', model: 'opus' }, b = { launcher_id: 'l1', model: 'sonnet' }, c = { launcher_id: 'l2', model: 'opus' };
const role = { id: 'r1', rev: 4, role_key: 'worker', label: 'Worker', emoji: '🛠️', body: 'text', extends: '', cardinality: 'multi', cap_is_lead: false, cap_can_spawn: true, cap_singleton_writer: false, default_agents: [a, b], pick_mode: 'randomly' };

assert.equal(api.MAX_ROLE_DEFAULTS, 32);
assert.deepEqual(plain(api.PICK_MODES), [['in_turn', 'In turn'], ['randomly', 'Randomly']]);

// positionOf matches the launcher and the model together: the same model id on another launcher is another entry.
assert.equal(api.positionOf(role, 'l1', 'opus'), 0);
assert.equal(api.positionOf(role, 'l1', 'sonnet'), 1);
assert.equal(api.positionOf(role, 'l2', 'opus'), -1);
assert.equal(api.positionOf({}, 'l1', 'opus'), -1);
assert.equal(api.positionOf(undefined, 'l1', 'opus'), -1);
assert.deepEqual(plain(api.rolesFor([role, { default_agents: [c] }, { default_agents: [] }], 'l2', 'opus')), [{ default_agents: [c] }]);

// withModel: on appends once, off removes only that entry, and the input is never changed.
const before = JSON.stringify(role.default_agents);
assert.deepEqual(plain(api.withModel(role.default_agents, 'l2', 'opus', true)), [a, b, c]);
assert.deepEqual(plain(api.withModel(role.default_agents, 'l1', 'opus', true)), [a, b], 'a model already listed keeps its place');
assert.deepEqual(plain(api.withModel(role.default_agents, 'l1', 'opus', false)), [b]);
assert.deepEqual(plain(api.withModel(role.default_agents, 'l2', 'opus', false)), [a, b], 'removing an absent model changes nothing');
assert.deepEqual(plain(api.withModel(undefined, 'l1', 'opus', true)), [a]);
assert.deepEqual(plain(api.withModel(null, 'l1', 'opus', false)), []);
assert.equal(JSON.stringify(role.default_agents), before, 'the list is not mutated');

// movedByKey: one place up or down, found by launcher and model; off either end, or of an entry that is gone, is a no-op.
assert.deepEqual(plain(api.movedByKey([a, b, c], 'l1', 'opus', 1)), [b, a, c]);
assert.deepEqual(plain(api.movedByKey([a, b, c], 'l2', 'opus', -1)), [a, c, b]);
assert.deepEqual(plain(api.movedByKey([a, b, c], 'l1', 'opus', -1)), [a, b, c]);
assert.deepEqual(plain(api.movedByKey([a, b, c], 'l2', 'opus', 1)), [a, b, c]);
assert.deepEqual(plain(api.movedByKey([a, b, c], 'l9', 'gone', 1)), [a, b, c], 'an entry that is gone changes nothing');
assert.deepEqual(plain(api.movedByKey(undefined, 'l1', 'opus', 1)), []);

// The limit of 32 defaults per role (the server's own).
const full = Array.from({ length: 32 }, (_, index) => ({ launcher_id: 'l', model: `m${index}` }));
assert.equal(api.canAddDefault({ default_agents: full }), false);
assert.equal(api.canAddDefault({ default_agents: full.slice(1) }), true);
assert.equal(api.canAddDefault({}), true);

// The save body carries everything the role editor sends, with the patch on top and extra fields (resolved_body, builtin) left out.
const body = api.roleBody({ ...role, resolved_body: 'x', builtin: true }, { pick_mode: 'in_turn' });
assert.deepEqual(plain(body), { id: 'r1', rev: 4, role_key: 'worker', label: 'Worker', emoji: '🛠️', body: 'text', extends: '', cardinality: 'multi', cap_is_lead: false, cap_can_spawn: true, cap_singleton_writer: false, default_agents: [a, b], pick_mode: 'in_turn' });
assert.deepEqual(plain(api.roleBody({ id: 'r', rev: 1, role_key: 'k', label: 'K', cardinality: 'singleton' })), { id: 'r', rev: 1, role_key: 'k', label: 'K', emoji: '', body: '', extends: '', cardinality: 'singleton', cap_is_lead: false, cap_can_spawn: false, cap_singleton_writer: false, default_agents: [], pick_mode: 'in_turn' }, 'missing fields get the editor defaults');

// The store: saves of one role go out one after another, each on the revision and list the one before left, so a quick tick, move and
// pick mode all land; a failed save reloads the stored roles and reports the error.
const storeSource = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'role-defaults.js'), 'utf8').replace(/^import .*$/gm, '').replace(/^export /gm, '');
const events = [];
const storeContext = { document: { dispatchEvent: event => events.push(event.detail.roleID) }, CustomEvent: class { constructor(type, init) { this.type = type; this.detail = init.detail; } }, request: async () => { throw new Error('no network'); },
  roleBody: context.api.roleBody, withModel: context.api.withModel, movedByKey: context.api.movedByKey };
vm.createContext(storeContext);
vm.runInContext(storeSource + '\nglobalThis.RoleDefaults = RoleDefaults; globalThis.ROLE_DEFAULTS_CHANGED = ROLE_DEFAULTS_CHANGED;', storeContext);
const { RoleDefaults, ROLE_DEFAULTS_CHANGED } = storeContext;
assert.equal(ROLE_DEFAULTS_CHANGED, 'role-defaults-change');

(async () => {
  // A server stand-in that answers slowly and refuses a stale revision, like the real one.
  const stored = new Map([['r1', { ...role, rev: 0, default_agents: [], pick_mode: 'in_turn' }]]);
  const log = [];
  const send = async (url, options = {}) => {
    if (!options.method) return [...stored.values()].map(item => plain(item));
    const body = JSON.parse(JSON.stringify(options.body)); const id = url.split('/').pop();
    log.push(`${body.rev}:${body.default_agents.map(item => item.model).join(',')}:${body.pick_mode}`);
    await new Promise(resolve => setTimeout(resolve, 20));
    const current = stored.get(id);
    if (current.rev !== body.rev) { const error = new Error('definition changed; reload it'); error.status = 409; throw error; }
    const saved = { ...current, ...body, rev: current.rev + 1 }; stored.set(id, saved);
    return plain(saved);
  };
  const store = new RoleDefaults(send);
  await store.load();
  const first = store.roles[0];
  // All four go out without waiting for each other; the queue keeps them in order and chains the revision.
  const results = await Promise.all([store.setModel(first, 'l1', 'opus', true), store.setModel(first, 'l1', 'sonnet', true), store.moveEntry(first, 'l1', 'opus', 1), store.setMode(first, 'randomly')]);
  assert.deepEqual(log, ['0:opus:in_turn', '1:opus,sonnet:in_turn', '2:sonnet,opus:in_turn', '3:sonnet,opus:randomly'], 'each save is sent on the revision and list the one before left');
  assert.deepEqual(plain(stored.get('r1').default_agents), [{ launcher_id: 'l1', model: 'sonnet' }, { launcher_id: 'l1', model: 'opus' }]);
  assert.equal(stored.get('r1').pick_mode, 'randomly');
  assert.deepEqual(results.map(item => item.rev), [1, 2, 3, 4]);
  assert.deepEqual(plain(store.roles[0].default_agents), [{ launcher_id: 'l1', model: 'sonnet' }, { launcher_id: 'l1', model: 'opus' }], 'the list is what the last save left');
  assert.equal(events.length, 4, 'every save tells the other view');
  // Removing and turning one off.
  await store.removeEntry(store.roles[0], 'l1', 'sonnet');
  assert.deepEqual(plain(stored.get('r1').default_agents), [{ launcher_id: 'l1', model: 'opus' }]);
  await store.setModel(store.roles[0], 'l1', 'opus', false);
  assert.deepEqual(plain(stored.get('r1').default_agents), []);
  // Queued removals name their entry, not its place: with [A, B, C] drawn, removing A and then B (both clicked before the first save is
  // back) leaves C, not [B] with C deleted by a stale position.
  await store.update('r1', () => ({ default_agents: [a, b, c] }));
  const drawn = store.roles[0];
  await Promise.all([store.removeEntry(drawn, a.launcher_id, a.model), store.removeEntry(drawn, b.launcher_id, b.model)]);
  assert.deepEqual(plain(stored.get('r1').default_agents), [c], 'two quick removals both land on the right entries');
  // The same for moves: C is moved up and then A removed, whichever order they were drawn in.
  await store.update('r1', () => ({ default_agents: [a, b, c] }));
  await Promise.all([store.moveEntry(store.roles[0], c.launcher_id, c.model, -1), store.removeEntry(store.roles[0], a.launcher_id, a.model)]);
  assert.deepEqual(plain(stored.get('r1').default_agents), [c, b]);
  // A revision changed elsewhere: the save fails, the stored role is read again, and the next save works on it.
  stored.set('r1', { ...stored.get('r1'), rev: 40 });
  await assert.rejects(store.setMode(store.roles[0], 'in_turn'), /definition changed/);
  assert.equal(store.roles[0].rev, 40, 'the roles are reloaded after a refused save');
  await store.setMode(store.roles[0], 'in_turn');
  assert.equal(stored.get('r1').rev, 41);
  // A role that is gone.
  await assert.rejects(store.update('missing', () => ({})), /no longer exists/);
  console.log('role defaults ok');
})().catch(error => { console.error(error); process.exit(1); });
