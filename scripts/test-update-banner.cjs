// The bar that reports the server's update check to the admins: when it may show (a state, an admin or the owner, no ack still covering that
// version and state), what it says in each state, and what Copy, Remind and Skip do. Runs the bar's controller (web/js/pages/banners/model.js)
// in a vm with fake timers; the bar's element, its place and its buttons are checked in Chromium by scripts/test-banners-page.cjs.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'banners', 'model.js'), 'utf8');
assert.ok(!/^import /m.test(source), 'the controllers import nothing');

function page() {
  const timers = new Map(); let nextTimer = 1;
  const context = { Date, console, timers, copyCalls: [] };
  vm.createContext(context);
  vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { updateBannerWanted, updateBannerText, UpdateBannerModel };', context);
  const setTimer = (fn, ms) => { timers.set(nextTimer, { fn, ms }); return nextTimer++; }, clearTimer = id => { timers.delete(id); };
  return { context, api: context.api, setTimer, clearTimer };
}
function make(options = {}) {
  const env = page();
  const calls = { status: 0, acked: [], changes: 0 };
  const banner = new env.api.UpdateBannerModel({
    getStatus: async () => { calls.status++; return options.status ?? null; },
    ack: async kind => { calls.acked.push(kind); if (options.refuse) throw new Error('refused'); },
    copy: text => { env.context.copyCalls.push(text); return Promise.resolve(true); },
    setTimeout: env.setTimer, clearTimeout: env.clearTimer, onChange: () => { calls.changes++; },
  });
  return { ...env, banner, calls };
}
// The bar's text as parts: a string, or [tag, text].
const tagged = (t, tag, text) => t.banner.text.some(part => Array.isArray(part) && (!tag || part[0] === tag) && part[1] === text);
const status = { checked: true, state: 'available', version: 'v2.1.0' };
const installed = { checked: true, state: 'installed', version: 'v2.1.0' };

(async () => {
  const { updateBannerWanted } = page().api;
  const rights = { admin: true, owner: false };
  assert.equal(updateBannerWanted({ status, rights, ack: null }), true);
  for (const [name, change] of [
    ['no status', { status: null }],
    ['nothing to do', { status: { state: '', version: '' } }],
    ['not an admin', { rights: { admin: false, owner: false } }],
    ['skipped', { ack: { version: 'v2.1.0', state: 'available', kind: 'skip' } }],
    ['remind not due yet', { ack: { version: 'v2.1.0', state: 'available', kind: 'remind', until: new Date(Date.now() + 3600e3).toISOString() } }],
  ]) {
    assert.equal(updateBannerWanted({ status, rights, ack: null, ...change }), false, name);
  }
  assert.equal(updateBannerWanted({ status, rights: { admin: false, owner: true }, ack: null }), true, 'the owner sees it too');
  assert.equal(updateBannerWanted({ status, rights, ack: { version: 'v2.1.0', state: 'available', kind: 'remind', until: new Date(Date.now() - 1000).toISOString() } }), true, 'a due remind shows it again');

  // The bar starts hidden (its element, place and buttons: scripts/test-banners-page.cjs).
  let t = make();
  assert.deepEqual([t.banner.hidden, t.banner.title, t.banner.text.length], [true, '', 0]);

  // A sign-in reads the account's rights and then the server's answer: available says what to run, installed says to restart.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.deepEqual([t.calls.status, t.banner.hidden], [1, false]);
  assert.ok(t.calls.changes > 0, 'the adapter is told to draw');
  assert.ok(tagged(t, 'strong', 'v2.1.0'), 'the version is named');
  assert.ok(tagged(t, 'code', 'aimebu update'), 'the command is named');
  t = make({ status: installed });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.hidden, false);
  assert.ok(!t.banner.text.some(part => Array.isArray(part) && part[0] === 'code'), 'installed asks for a restart, not the command');
  t = make({ status: { checked: true, state: 'unbuilt', version: 'v2.1.0-3-gabcdef1' } });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.hidden, false);
  assert.ok(tagged(t, 'code', 'aimebu version'), 'unbuilt names the rebuild step');
  assert.ok(!tagged(t, 'code', 'aimebu update'), 'unbuilt is not fixed by aimebu update');
  assert.ok(t.banner.text.some(part => typeof part === 'string' && /built binary is older/.test(part)), 'unbuilt says the binary is older');
  // A member gets nothing, and neither does anyone when the server has checked nothing.
  t = make({ status });
  await t.banner.setAccount({ admin: false, owner: false });
  assert.equal(t.banner.hidden, true);
  t = make({ status: null });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.hidden, true);
  // A failed read closes the bar instead of raising an error.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  t.banner.getStatus = async () => { throw new Error('gone'); };
  await t.banner.refresh();
  assert.equal(t.banner.hidden, true);

  // Copy puts the command on the clipboard and changes nothing else.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  t.banner.copy();
  await Promise.resolve();
  assert.deepEqual([...t.context.copyCalls, t.banner.hidden], ['aimebu update', false]);

  // Remind asks the server, then hides the bar for a day (a refused ask keeps the bar and explains in its tooltip).
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('remind');
  assert.deepEqual([t.calls.acked, t.banner.hidden], [['remind'], true]);
  // The bar wakes itself at the deadline: one timer about a day out, and when it fires after the deadline the bar is back.
  assert.equal(t.context.timers.size, 1, 'one wake-up is scheduled');
  const [{ fn: wake, ms: wait }] = [...t.context.timers.values()];
  assert.ok(wait > 23 * 3600e3 && wait <= 24 * 3600e3 + 1000, 'the wake-up is about 24 hours away');
  t.banner.ackRecord = { ...t.banner.ackRecord, until: new Date(Date.now() - 1000).toISOString() };
  wake();
  assert.equal(t.banner.hidden, false, 'the bar is back once the remind is due');
  assert.equal(t.context.timers.size, 0, 'a due remind leaves no timer behind');
  t = make({ status, refuse: true });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('remind');
  assert.equal(t.banner.hidden, false, 'a refused remind keeps the bar');
  assert.equal(t.banner.ackRecord, null, 'a refused remind is not remembered');
  assert.match(t.banner.title, /Could not save your answer: refused/, 'the failure is in the tooltip');
  t.banner.acknowledge = async () => {};
  await t.banner.answer('remind');
  assert.deepEqual([t.banner.hidden, t.banner.title], [true, ''], 'a later successful answer closes the bar and clears the tooltip');

  // Skip asks the server and hides the bar for good (until the version or state changes).
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('skip');
  assert.deepEqual([t.calls.acked, t.banner.hidden], [['skip'], true]);
  // A bar that goes away leaves no wake-up behind.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('remind');
  assert.equal(t.context.timers.size, 1);
  t.banner.stop();
  assert.equal(t.context.timers.size, 0, 'stop() clears the wake-up');
  console.log('update banner test passed');
})().catch(error => { console.error(error); process.exit(1); });
