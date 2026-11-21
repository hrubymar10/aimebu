// The bar that reports the server's update check to the admins: when it may show (a state, an admin or the owner, no ack still covering that
// version and state), what it says in each state, and what Copy, Remind and Skip do. Runs the module sources in a vm with a small stand-in
// for the page, the same way the notification banner test does.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const read = name => fs.readFileSync(path.join(__dirname, '..', 'web', 'js', name), 'utf8');
const source = read('icon-button.js').replace(/^export /gm, '') + '\n' +
  read('update-banner.js').replace(/^import .*$/gm, '').replace(/^export /gm, '') +
  '\nfunction copyText(text) { copyCalls.push(text); return Promise.resolve(true); }' +
  '\nglobalThis.api = { updateBannerWanted, UpdateBanner, copied: text => copyCalls.push(text) };\nglobalThis.copyCalls = [];';

function element(tag) {
  const node = { tag, className: '', hidden: false, title: '', textContent: '', attrs: {}, children: [], onclick: null, disabled: false,
    append(...items) { this.children.push(...items); }, replaceChildren(...items) { this.children = items; },
    setAttribute(name, value) { this.attrs[name] = value; }, getAttribute(name) { return this.attrs[name] ?? null; }, after(item) { this.next = item; } };
  return node;
}
function page() {
  const document = { hidden: false, createElement: element };
  const timers = new Map(); let nextTimer = 1;
  const context = { document, Date, console, timers, setTimeout: (fn, ms) => { timers.set(nextTimer, { fn, ms }); return nextTimer++; }, clearTimeout: id => { timers.delete(id); } };
  vm.createContext(context);
  vm.runInContext(source, context);
  return { context, document, api: context.api, anchor: element('header') };
}
function make(options = {}) {
  const env = page();
  const calls = { status: 0, acked: [] };
  const banner = new env.api.UpdateBanner({
    getStatus: async () => { calls.status++; return options.status ?? null; },
    ack: async kind => { calls.acked.push(kind); if (options.refuse) throw new Error('refused'); },
    anchor: env.anchor,
  });
  return { ...env, banner, calls };
}
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

  // The bar sits right after its anchor, hidden, as a labelled region with three icon buttons (a tooltip and a name each, no text).
  let t = make();
  assert.equal(t.anchor.next, t.banner.bar);
  assert.deepEqual([t.banner.bar.hidden, t.banner.bar.getAttribute('role'), t.banner.bar.getAttribute('aria-label')], [true, 'region', 'aimebu update']);
  for (const button of [t.banner.copy, t.banner.remind, t.banner.skip]) assert.ok(button.title && button.getAttribute('aria-label') === button.title && button.textContent === '', 'icon button');

  // A sign-in reads the account's rights and then the server's answer: available says what to run, installed says to restart.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.deepEqual([t.calls.status, t.banner.bar.hidden], [1, false]);
  assert.ok(t.banner.text.children.some(node => node.textContent === 'v2.1.0'), 'the version is named');
  assert.ok(t.banner.text.children.some(node => node.tag === 'code' && node.textContent === 'aimebu update'), 'the command is named');
  t = make({ status: installed });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.bar.hidden, false);
  assert.ok(!t.banner.text.children.some(node => node.tag === 'code'), 'installed asks for a restart, not the command');
  t = make({ status: { checked: true, state: 'unbuilt', version: 'v2.1.0-3-gabcdef1' } });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.bar.hidden, false);
  assert.ok(t.banner.text.children.some(node => node.tag === 'code' && node.textContent === 'aimebu version'), 'unbuilt names the rebuild step');
  assert.ok(!t.banner.text.children.some(node => node.tag === 'code' && node.textContent === 'aimebu update'), 'unbuilt is not fixed by aimebu update');
  assert.ok(t.banner.text.children.some(node => typeof node === 'string' && /built binary is older/.test(node)), 'unbuilt says the binary is older');
  // A member gets nothing, and neither does anyone when the server has checked nothing.
  t = make({ status });
  await t.banner.setAccount({ admin: false, owner: false });
  assert.equal(t.banner.bar.hidden, true);
  t = make({ status: null });
  await t.banner.setAccount({ admin: true, owner: false });
  assert.equal(t.banner.bar.hidden, true);
  // A failed read closes the bar instead of raising an error.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  t.banner.getStatus = async () => { throw new Error('gone'); };
  await t.banner.refresh();
  assert.equal(t.banner.bar.hidden, true);

  // Copy puts the command on the clipboard and changes nothing else.
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  t.banner.copy.onclick();
  await Promise.resolve();
  assert.deepEqual([...t.context.copyCalls, t.banner.bar.hidden], ['aimebu update', false]);

  // Remind asks the server, then hides the bar for a day (a refused ask keeps the bar and explains in its tooltip).
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('remind');
  assert.deepEqual([t.calls.acked, t.banner.bar.hidden], [['remind'], true]);
  // The bar wakes itself at the deadline: one timer about a day out, and when it fires after the deadline the bar is back.
  assert.equal(t.context.timers.size, 1, 'one wake-up is scheduled');
  const [{ fn: wake, ms: wait }] = [...t.context.timers.values()];
  assert.ok(wait > 23 * 3600e3 && wait <= 24 * 3600e3 + 1000, 'the wake-up is about 24 hours away');
  t.banner.ackRecord = { ...t.banner.ackRecord, until: new Date(Date.now() - 1000).toISOString() };
  wake();
  assert.equal(t.banner.bar.hidden, false, 'the bar is back once the remind is due');
  assert.equal(t.context.timers.size, 0, 'a due remind leaves no timer behind');
  t = make({ status, refuse: true });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('remind');
  assert.equal(t.banner.bar.hidden, false, 'a refused remind keeps the bar');
  assert.equal(t.banner.ackRecord, null, 'a refused remind is not remembered');
  assert.match(t.banner.bar.title, /Could not save your answer: refused/, 'the failure is in the tooltip');
  t.banner.acknowledge = async () => {};
  await t.banner.answer('remind');
  assert.deepEqual([t.banner.bar.hidden, t.banner.bar.title], [true, ''], 'a later successful answer closes the bar and clears the tooltip');

  // Skip asks the server and hides the bar for good (until the version or state changes).
  t = make({ status });
  await t.banner.setAccount({ admin: true, owner: false });
  await t.banner.answer('skip');
  assert.deepEqual([t.calls.acked, t.banner.bar.hidden], [['skip'], true]);
  console.log('update banner test passed');
})().catch(error => { console.error(error); process.exit(1); });
