// The preference saver is browser-module code with no DOM, so this runs it in a vm: the saves of one name go out in order, so a slow
// older value cannot land after a newer one, and different names do not wait for each other.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'preferences.js'), 'utf8')
  .replace(/^import \{ captureError \}.*$/gm, 'const captureError = () => false;')
  .replace(/^import .*$/gm, 'const request = () => { throw new Error("no network in this test"); }; let currentUser = "user1";').replace(/^export /gm, '');
const context = { Object, Promise, String, Map };
vm.createContext(context);
vm.runInContext(source + '\nglobalThis.api = { loadPreferences, savePreference, PREFERENCE_KEYS, PREFERENCE_LIMITS, setSession: id => { currentUser = id; } };', context);
const { loadPreferences, savePreference, PREFERENCE_KEYS, PREFERENCE_LIMITS, setSession } = context.api;

(async () => {
  assert.ok('sidebar_sort' in PREFERENCE_KEYS && 'sidebar_project_order' in PREFERENCE_KEYS);
  assert.equal(PREFERENCE_LIMITS.sidebar_project_order, 4096);
  // Nothing is sent before sign-in, nor for a name the server does not know.
  const sent = [];
  const gates = [];
  const send = (url, options) => new Promise(resolve => {
    const entry = { value: options?.body?.preferences, done: () => resolve({}) };
    sent.push(entry);
    gates.push(entry);
  });
  await savePreference('sidebar_sort', 'name', { send });
  assert.equal(sent.length, 0, 'no save before sign-in');
  await loadPreferences('user1', { send: async () => ({ preferences: {} }), storage: { getItem: () => null, setItem() {} } });
  // What the server holds is answered too, for the preferences that have no browser cache (the offer to set up a harness is decided at sign-in).
  assert.deepEqual(JSON.parse(JSON.stringify(await loadPreferences('user1', { send: async () => ({ preferences: { harness_setup_skipped: '1', theme: 'dark' } }), storage: { getItem: () => null, setItem() {} } }))), { harness_setup_skipped: '1', theme: 'dark' });
  assert.deepEqual(JSON.parse(JSON.stringify(await loadPreferences('user1', { send: async () => { throw new Error('offline'); }, storage: { getItem: () => null, setItem() {} } }))), {}, 'an unreadable server answers nothing stored');
  await savePreference('nonsense', 'x', { send });
  assert.equal(sent.length, 0, 'an unknown name is not sent');

  // Two saves of one name: the second is not sent until the first has finished, and then it is the last word.
  const first = savePreference('sidebar_project_order', '["a","b"]', { send });
  const second = savePreference('sidebar_project_order', '["b","a"]', { send });
  const third = savePreference('sidebar_project_order', '["b","a","c"]', { send });
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 1, 'the second save waits for the first');
  assert.equal(sent[0].value.sidebar_project_order, '["a","b"]');
  sent[0].done(); await first; await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 2); assert.equal(sent[1].value.sidebar_project_order, '["b","a"]');
  sent[1].done(); await second; await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 3); assert.equal(sent[2].value.sidebar_project_order, '["b","a","c"]', 'the newest value goes last');
  sent[2].done(); await third;

  // A different name does not wait behind a slow one.
  sent.length = 0;
  savePreference('sidebar_project_order', '["x"]', { send });
  savePreference('sidebar_sort', 'manual', { send });
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.deepEqual(sent.map(entry => Object.keys(entry.value)[0]).sort(), ['sidebar_project_order', 'sidebar_sort']);
  sent.forEach(entry => entry.done());

  // A failed save does not block the one after it.
  const failing = (url, options) => { if (options.body.preferences.sidebar_sort === 'bad') return Promise.reject(new Error('down')); sent.push({ value: options.body.preferences }); return Promise.resolve({}); };
  sent.length = 0;
  await savePreference('sidebar_sort', 'bad', { send: failing });
  await savePreference('sidebar_sort', 'name', { send: failing });
  assert.deepEqual(sent.map(entry => entry.value.sidebar_sort), ['name']);

  // A save queued behind a slow one belongs to the person who made it: when the session changes before it would go out (a sign-out, or
  // another person signing in on the same page), it is dropped instead of reaching the other person's preferences.
  sent.length = 0;
  const slow = savePreference('sidebar_project_order', '["a","b"]', { send });
  const queued = savePreference('sidebar_project_order', '["b","a"]', { send });
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 1);
  setSession('user2');
  await loadPreferences('user2', { send: async () => ({ preferences: {} }), storage: { getItem: () => null, setItem() {} } });
  sent[0].done(); await slow; await queued; await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 1, "the first person's queued save was not sent with the second person's session");
  // The second person's own saves still go out, and a sign-out stops them.
  const own = savePreference('sidebar_sort', 'name', { send }); await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 2); sent[1].done(); await own;
  const blocked = savePreference('sidebar_sort', 'manual', { send });
  setSession(''); await blocked;
  assert.equal(sent.length, 2, 'nothing is sent after sign-out');
  // A caller that no longer wants a queued save says so with `live`: the save behind a slow one is dropped, the slow one is left alone.
  sent.length = 0;
  setSession('user3');
  await loadPreferences('user3', { send: async () => ({ preferences: {} }), storage: { getItem: () => null, setItem() {} } });
  let wanted = true;
  const out = savePreference('sidebar_sort', 'name', { send });
  const dropped = savePreference('sidebar_sort', 'manual', { send, live: () => wanted });
  await new Promise(resolve => setTimeout(resolve, 10));
  wanted = false; sent[0].done(); await out; await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(sent.length, 1, 'a queued save its caller no longer wants is not sent');
  await dropped;
  console.log('preferences test passed');
})().catch(error => { console.error(error); process.exit(1); });
