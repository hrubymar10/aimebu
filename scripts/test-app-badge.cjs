// The app icon badge (web/js/app-badge.js): set on a count, cleared on 0 or sign-out, and silent where the API is missing or rejects.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'app-badge.js'), 'utf8');
const context = vm.createContext({ Promise, Math, Number });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { updateAppBadge };', context);
const { updateAppBadge } = context.api;

function fake(extra = {}) {
  const calls = [];
  return { calls, setAppBadge: n => { calls.push(['set', n]); return Promise.resolve(); }, clearAppBadge: () => { calls.push(['clear']); return Promise.resolve(); }, ...extra };
}

(async () => {
  assert.equal(await updateAppBadge(3, undefined), false, 'no navigator');
  assert.equal(await updateAppBadge(3, {}), false, 'unsupported API');

  let nav = fake();
  assert.equal(await updateAppBadge(3, nav), true);
  assert.deepEqual(nav.calls, [['set', 3]]);

  nav = fake();
  await updateAppBadge(0, nav);
  assert.deepEqual(nav.calls, [['clear']], 'count 0 clears');

  nav = fake();
  await updateAppBadge(null, nav);
  assert.deepEqual(nav.calls, [['clear']], 'sign-out (null count) clears');

  nav = fake();
  await updateAppBadge(-2, nav);
  assert.deepEqual(nav.calls, [['clear']], 'a negative count clears');

  const calls = [];
  await updateAppBadge(0, { setAppBadge: n => { calls.push(n); return Promise.resolve(); } });
  assert.deepEqual(calls, [0], 'without clearAppBadge, setAppBadge(0) clears');

  nav = fake({ setAppBadge: () => Promise.reject(new Error('denied')) });
  assert.equal(await updateAppBadge(2, nav), false, 'a rejection is swallowed');
  nav = fake({ clearAppBadge: () => Promise.reject(new Error('denied')) });
  assert.equal(await updateAppBadge(0, nav), false, 'a rejected clear is swallowed');
  nav = fake({ setAppBadge: () => { throw new Error('sync'); } });
  assert.equal(await updateAppBadge(2, nav), false, 'a synchronous throw is swallowed');
  console.log('app-badge ok');
})().catch(e => { console.error(e); process.exit(1); });
