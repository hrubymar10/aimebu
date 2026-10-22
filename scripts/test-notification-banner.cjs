// When the bar that offers browser notifications may show, and what its buttons do: only while the browser has not been asked, once per page,
// once the tab is in view, never after Dismiss (which is saved on the account), and Enable is the only thing that raises the permission
// prompt. Runs the module sources in a vm with a small stand-in for the page.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const read = name => fs.readFileSync(path.join(__dirname, '..', 'web', 'js', name), 'utf8');
const source = read('icon-button.js').replace(/^export /gm, '') + '\n' + read('notification-banner.js').replace(/^import .*$/gm, '').replace(/^export /gm, '') +
  '\nglobalThis.api = { bannerWanted, NotificationBanner, BANNER_DISMISSED_PREFERENCE };';

function element(tag) {
  const node = { tag, className: '', hidden: false, title: '', textContent: '', attrs: {}, children: [], onclick: null, disabled: false,
    append(...items) { this.children.push(...items); }, replaceChildren(...items) { this.children = items; },
    setAttribute(name, value) { this.attrs[name] = value; }, getAttribute(name) { return this.attrs[name] ?? null; }, after(item) { this.next = item; } };
  return node;
}
function page({ hidden = false } = {}) {
  const listeners = {};
  const document = { hidden, createElement: element, addEventListener: (name, fn) => { listeners[name] = fn; } };
  const permissionStatus = {};
  const navigator = { permissions: { query: async () => permissionStatus } };
  const context = { document, navigator };
  vm.createContext(context);
  vm.runInContext(source, context);
  return { context, document, listeners, api: context.api, anchor: element('header'), permissionStatus };
}
function make(options = {}) {
  const env = page(options);
  const calls = { asked: 0, saved: 0 };
  const notifications = { available: options.available ?? true, requestPermission: async () => { calls.asked++; if (options.refuse) throw new Error('refused'); } };
  let permission = options.permission ?? 'default';
  const banner = new env.api.NotificationBanner({ notifications, saveDismissed: () => { calls.saved++; return Promise.resolve(); }, anchor: env.anchor, permission: () => permission });
  return { ...env, banner, calls, setPermission: value => { permission = value; } };
}

(async () => {
  const { bannerWanted } = page().api;
  const wanted = { available: true, permission: 'default', dismissed: false, shown: false };
  assert.equal(bannerWanted(wanted), true);
  for (const [name, change] of [['not available', { available: false }], ['granted', { permission: 'granted' }], ['denied', { permission: 'denied' }], ['dismissed', { dismissed: true }], ['already shown', { shown: true }]]) {
    assert.equal(bannerWanted({ ...wanted, ...change }), false, name);
  }
  assert.equal(page().api.BANNER_DISMISSED_PREFERENCE, 'notification_banner_dismissed');

  // The bar sits right after its anchor, hidden, as a labelled region with two icon buttons (a tooltip and a name each, no text).
  let t = make();
  assert.equal(t.anchor.next, t.banner.bar);
  assert.deepEqual([t.banner.bar.hidden, t.banner.bar.getAttribute('role'), t.banner.bar.getAttribute('aria-label')], [true, 'region', 'Browser notifications']);
  for (const button of [t.banner.enable, t.banner.dismiss]) assert.ok(button.title && button.getAttribute('aria-label') === button.title && button.textContent === '', 'icon button');

  // Attention shows it once; a second look does not bring back a closed bar.
  t.banner.attention(); assert.equal(t.banner.bar.hidden, false);
  t.banner.close(false); t.banner.attention(); assert.equal(t.banner.bar.hidden, true, 'shown once per page');
  assert.equal(t.calls.saved, 0, 'closing without Dismiss saves nothing');

  // Enable raises the prompt once and closes the bar, whatever the browser answers; the prompt is never raised by anything else.
  t = make(); t.banner.attention(); assert.equal(t.calls.asked, 0);
  await t.banner.ask(); assert.deepEqual([t.calls.asked, t.banner.bar.hidden, t.banner.enable.disabled], [1, true, false]);
  t = make({ refuse: true }); t.banner.attention(); await t.banner.ask(); assert.equal(t.banner.bar.hidden, true, 'a refused prompt still ends the bar');

  // Dismiss is saved on the account and holds; the account's saved choice is read at sign-in.
  t = make(); t.banner.attention(); t.banner.dismiss.onclick();
  assert.deepEqual([t.banner.bar.hidden, t.calls.saved, t.banner.dismissed], [true, 1, true]);
  t.banner.reset(false); assert.equal(t.banner.dismissed, false); t.banner.attention(); assert.equal(t.banner.bar.hidden, false, 'a new sign-in with nothing saved offers it again');
  t.banner.reset(true); t.banner.attention(); assert.equal(t.banner.bar.hidden, true, 'a saved dismissal is respected');

  // Not while the browser has already been asked, nor where notifications do not exist.
  t = make({ permission: 'granted' }); t.banner.attention(); assert.equal(t.banner.bar.hidden, true);
  t = make({ available: false }); t.banner.attention(); assert.equal(t.banner.bar.hidden, true);

  // In a background tab it waits, and is offered when the person comes back to the tab.
  t = make({ hidden: true }); t.banner.attention(); assert.deepEqual([t.banner.bar.hidden, t.banner.waiting], [true, true]);
  t.document.hidden = false; t.listeners.visibilitychange(); assert.deepEqual([t.banner.bar.hidden, t.banner.waiting], [false, false]);
  // ... unless the browser was asked meanwhile (from Settings).
  t = make({ hidden: true }); t.banner.attention(); t.setPermission('granted'); t.document.hidden = false; t.listeners.visibilitychange(); assert.equal(t.banner.bar.hidden, true);
  // The permission changes behind the bar's back (Settings, or the browser's own site settings): the shown bar goes, so does a waiting one,
  // by sync() (also run when the tab comes back and when the permissions API reports a change).
  t = make(); t.banner.attention(); assert.equal(t.banner.bar.hidden, false);
  t.setPermission('granted'); t.banner.sync(); assert.equal(t.banner.bar.hidden, true, 'granted in Settings');
  t = make({ hidden: true }); t.banner.attention(); assert.equal(t.banner.waiting, true);
  t.setPermission('denied'); t.banner.sync(); assert.deepEqual([t.banner.bar.hidden, t.banner.waiting], [true, false], 'a waiting offer is dropped');
  t = make(); t.banner.attention(); await Promise.resolve(); await Promise.resolve();
  t.setPermission('denied'); t.permissionStatus.onchange(); assert.equal(t.banner.bar.hidden, true, 'the permissions API reports a change');
  t = make(); t.banner.attention(); t.banner.sync(); assert.equal(t.banner.bar.hidden, false, 'nothing changed, the bar stays');
  t = make(); t.banner.attention(); t.setPermission('granted'); t.document.hidden = true; t.document.hidden = false; t.listeners.visibilitychange();
  assert.equal(t.banner.bar.hidden, true, 'coming back to the tab re-reads the permission');
  console.log('notification banner test passed');
})().catch(error => { console.error(error); process.exit(1); });
