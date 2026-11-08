// When the Harnesses step is offered on sign-in: only to someone who can manage harnesses, only while no harness with a model exists, never
// again after a skip (until a harness exists, which resets the skip), and once per browser session. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'harness-offer.js'), 'utf8');
const context = { sessionStorage: { getItem() { return null; }, setItem() {} } };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { offerHarnessSetup, anyHarnessConfigured, launcherReady, HARNESS_SKIP_PREFERENCE };', context);
const { offerHarnessSetup, anyHarnessConfigured, launcherReady, HARNESS_SKIP_PREFERENCE } = context.api;

assert.equal(HARNESS_SKIP_PREFERENCE, 'harness_setup_skipped');

const run = async ({ canSetUp = true, skipped = false, configured = false, storage = new Map(), throws = false } = {}) => {
  const calls = { shown: 0, skip: [] };
  const result = await offerHarnessSetup({
    user: 'usr_1', canSetUp, skipped, storage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    configured: async () => { if (throws) throw new Error('machines unreadable'); return configured; },
    show: async () => { calls.shown++; }, setSkipped: async value => { calls.skip.push(value); },
  });
  return { result, ...calls };
};

(async () => {
  // Offered: an admin, no harness, not skipped. The skip stays as it was.
  assert.deepEqual(await run(), { result: 'offered', shown: 1, skip: [] });
  // Someone who cannot manage harnesses is never offered it (they get the "ask the owner" hint where it matters), and nothing is read.
  assert.deepEqual(await run({ canSetUp: false }), { result: 'not-allowed', shown: 0, skip: [] });
  // A harness with a model exists: nothing to offer; a skip from earlier is reset so the offer can come back if they are all removed.
  assert.deepEqual(await run({ configured: true }), { result: 'configured', shown: 0, skip: [] });
  assert.deepEqual(await run({ configured: true, skipped: true }), { result: 'configured', shown: 0, skip: [false] });
  // Skipped, and still no harness: not offered again, on any browser (the preference is the account's).
  assert.deepEqual(await run({ skipped: true }), { result: 'skipped', shown: 0, skip: [] });
  // The answer cannot be read: stay quiet rather than ask someone who may have a harness.
  assert.deepEqual(await run({ throws: true }), { result: 'unknown', shown: 0, skip: [] });
  // Once per browser session: the second look in the same session (a reload after closing it without choosing) is quiet; another
  // session, or another person in the same tab, is offered it.
  const session = new Map();
  assert.equal((await run({ storage: session })).result, 'offered');
  assert.deepEqual(await run({ storage: session }), { result: 'session', shown: 0, skip: [] });
  assert.equal((await run({ storage: new Map() })).result, 'offered');
  session.set('aimebu2.harness-offered', 'usr_2');
  assert.equal((await run({ storage: session })).result, 'offered', 'another person in the same tab');
  // A browser that will not keep session state still gets the offer.
  const broken = await offerHarnessSetup({ user: 'u', canSetUp: true, skipped: false, configured: async () => false, show: async () => {}, setSkipped: async () => {}, storage: { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } } });
  assert.equal(broken, 'offered');
  // Whether a harness with a model exists: read from the machines and their launchers, as the API answers them.
  const launcher = (enabled, models) => ({ config: { id: 'mln', enabled }, models: Array.from({ length: models }, (_, i) => ({ slug: `m${i}` })) });
  const api = (machines, byMachine) => {
    const asked = [];
    const request = async path => {
      asked.push(path);
      if (path === '/api/machines') return { machines };
      const id = decodeURIComponent(path.split('/')[3]);
      const answer = byMachine[id];
      if (answer instanceof Error) throw answer;
      return { launchers: answer || [] };
    };
    return { request, asked };
  };
  const ok = { id: 'a', approved: true }, ok2 = { id: 'b', approved: true };
  assert.equal(await anyHarnessConfigured(api([], {}).request), false, 'no machine');
  assert.equal(await anyHarnessConfigured(api([ok], { a: [launcher(true, 2)] }).request), true, 'an enabled harness with models');
  assert.equal(await anyHarnessConfigured(api([ok], { a: [launcher(false, 2)] }).request), false, 'a disabled harness is not set up');
  assert.equal(await anyHarnessConfigured(api([{ id: 'a', approved: true, connected: false, last_seen: '2026-01-01T00:00:00Z' }], { a: [launcher(true, 1)] }).request), true, 'a harness on a disconnected machine still counts');
  assert.equal(await anyHarnessConfigured(api([ok], { a: [launcher(true, 0)] }).request), false, 'a harness with no model is not ready to spawn');
  const skipped = api([{ id: 'c', approved: false }, { id: 'd', approved: true, revoked_at: '2026-01-01T00:00:00Z' }, ok2], { c: [launcher(true, 1)], d: [launcher(true, 1)], b: [] });
  assert.equal(await anyHarnessConfigured(skipped.request), false, 'a pending or revoked machine does not count');
  assert.deepEqual(Array.from(skipped.asked), ['/api/machines', '/api/machines/b/launchers'], 'and its launchers are not even read');
  assert.equal(await anyHarnessConfigured(api([ok, ok2], { a: new Error('503'), b: [launcher(true, 1)] }).request), true, 'one unreadable machine does not hide a harness on another');
  await assert.rejects(anyHarnessConfigured(api([ok, ok2], { a: new Error('503'), b: [] }).request), /503/, 'nothing found and one unread: not known');
  // One test of "can start an agent" for the sign-in check and for the Settings list: enabled (or not said to be disabled) with a model.
  assert.deepEqual([launcherReady(true, [{}]), launcherReady(undefined, [{}]), launcherReady(false, [{}]), launcherReady(true, []), launcherReady(true, undefined), launcherReady(false, [])], [true, true, false, false, false, false]);
  console.log('harness offer test passed');
})().catch(error => { console.error(error); process.exit(1); });
