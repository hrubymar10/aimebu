// When the bar that offers browser notifications may show, and what its buttons do: only while the browser has not been asked, once per page,
// once the tab is in view, never after Dismiss (which is saved on the account), and Enable is the only thing that raises the permission
// prompt. Runs the bar's controller (web/js/pages/banners/model.js) in a vm with a small stand-in for the page; the bar's element, its place and
// its buttons are checked in Chromium by scripts/test-banners-page.cjs.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'banners', 'model.js'), 'utf8');
assert.ok(!/^import /m.test(source), 'the controllers import nothing');

function page({ hidden = false } = {}) {
  const listeners = {};
  const document = { hidden, addEventListener: (name, fn) => { listeners[name] = fn; }, removeEventListener: (name, fn) => { if (listeners[name] === fn) delete listeners[name]; } };
  const permissionStatus = {};
  const navigator = { permissions: { query: async () => permissionStatus } };
  const context = {};
  vm.createContext(context);
  vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { bannerWanted, NotificationBannerModel, BANNER_DISMISSED_PREFERENCE };', context);
  return { context, document, navigator, listeners, api: context.api, permissionStatus };
}
// The controller, watching the page the way the bar's view does once it is drawn.
function make(options = {}) {
  const env = page(options);
  const calls = { asked: 0, saved: 0, changes: 0 };
  const notifications = { available: options.available ?? true, requestPermission: async () => { calls.asked++; if (options.refuse) throw new Error('refused'); } };
  let permission = options.permission ?? 'default';
  const banner = new env.api.NotificationBannerModel({ notifications, saveDismissed: () => { calls.saved++; return Promise.resolve(); }, permission: () => permission,
    isHidden: () => env.document.hidden, onChange: () => { calls.changes++; } });
  const unwatch = banner.watch(env.document, env.navigator.permissions);
  return { ...env, banner, calls, unwatch, setPermission: value => { permission = value; } };
}

(async () => {
  const { bannerWanted } = page().api;
  const wanted = { available: true, permission: 'default', dismissed: false, shown: false };
  assert.equal(bannerWanted(wanted), true);
  for (const [name, change] of [['not available', { available: false }], ['granted', { permission: 'granted' }], ['denied', { permission: 'denied' }], ['dismissed', { dismissed: true }], ['already shown', { shown: true }]]) {
    assert.equal(bannerWanted({ ...wanted, ...change }), false, name);
  }
  assert.equal(page().api.BANNER_DISMISSED_PREFERENCE, 'notification_banner_dismissed');

  // The bar starts hidden (its element, place and buttons: scripts/test-banners-page.cjs).
  let t = make();
  assert.deepEqual([t.banner.hidden, t.banner.enabling], [true, false]);

  // Attention shows it once; a second look does not bring back a closed bar.
  t.banner.attention(); assert.equal(t.banner.hidden, false);
  t.banner.close(false); t.banner.attention(); assert.equal(t.banner.hidden, true, 'shown once per page');
  assert.equal(t.calls.saved, 0, 'closing without Dismiss saves nothing');

  // Enable raises the prompt once and closes the bar, whatever the browser answers; the prompt is never raised by anything else.
  t = make(); t.banner.attention(); assert.equal(t.calls.asked, 0);
  await t.banner.ask(); assert.deepEqual([t.calls.asked, t.banner.hidden, t.banner.enabling], [1, true, false]);
  t = make({ refuse: true }); t.banner.attention(); await t.banner.ask(); assert.equal(t.banner.hidden, true, 'a refused prompt still ends the bar');

  // Dismiss is saved on the account and holds; the account's saved choice is read at sign-in.
  t = make(); t.banner.attention(); t.banner.close(true);
  assert.deepEqual([t.banner.hidden, t.calls.saved, t.banner.dismissed], [true, 1, true]);
  t.banner.reset(false); assert.equal(t.banner.dismissed, false); t.banner.attention(); assert.equal(t.banner.hidden, false, 'a new sign-in with nothing saved offers it again');
  t.banner.reset(true); t.banner.attention(); assert.equal(t.banner.hidden, true, 'a saved dismissal is respected');

  // Not while the browser has already been asked, nor where notifications do not exist.
  t = make({ permission: 'granted' }); t.banner.attention(); assert.equal(t.banner.hidden, true);
  t = make({ available: false }); t.banner.attention(); assert.equal(t.banner.hidden, true);

  // In a background tab it waits, and is offered when the person comes back to the tab.
  t = make({ hidden: true }); t.banner.attention(); assert.deepEqual([t.banner.hidden, t.banner.waiting], [true, true]);
  t.document.hidden = false; t.listeners.visibilitychange(); assert.deepEqual([t.banner.hidden, t.banner.waiting], [false, false]);
  // ... unless the browser was asked meanwhile (from Settings).
  t = make({ hidden: true }); t.banner.attention(); t.setPermission('granted'); t.document.hidden = false; t.listeners.visibilitychange(); assert.equal(t.banner.hidden, true);
  // The permission changes behind the bar's back (Settings, or the browser's own site settings): the shown bar goes, so does a waiting one,
  // by sync() (also run when the tab comes back and when the permissions API reports a change).
  t = make(); t.banner.attention(); assert.equal(t.banner.hidden, false);
  t.setPermission('granted'); t.banner.sync(); assert.equal(t.banner.hidden, true, 'granted in Settings');
  t = make({ hidden: true }); t.banner.attention(); assert.equal(t.banner.waiting, true);
  t.setPermission('denied'); t.banner.sync(); assert.deepEqual([t.banner.hidden, t.banner.waiting], [true, false], 'a waiting offer is dropped');
  t = make(); t.banner.attention(); await Promise.resolve(); await Promise.resolve();
  t.setPermission('denied'); t.permissionStatus.onchange(); assert.equal(t.banner.hidden, true, 'the permissions API reports a change');
  t = make(); t.banner.attention(); t.banner.sync(); assert.equal(t.banner.hidden, false, 'nothing changed, the bar stays');
  t = make(); t.banner.attention(); t.setPermission('granted'); t.document.hidden = true; t.document.hidden = false; t.listeners.visibilitychange();
  assert.equal(t.banner.hidden, true, 'coming back to the tab re-reads the permission');
  // Enable is off while the browser is asked.
  { let answer; t = make(); t.banner.attention(); t.banner.notifications.requestPermission = () => new Promise(resolve => { answer = resolve; });
    const asking = t.banner.ask(); assert.equal(t.banner.enabling, true, 'Enable is off while the prompt is up'); answer(); await asking; assert.equal(t.banner.enabling, false); }
  // The view's cleanup stops the watching: no listener is left on the page, and a late permission answer is not wired.
  t = make(); await Promise.resolve(); await Promise.resolve(); assert.equal(typeof t.listeners.visibilitychange, 'function'); assert.equal(typeof t.permissionStatus.onchange, 'function');
  t.unwatch(); assert.equal(t.listeners.visibilitychange, undefined); assert.equal(t.permissionStatus.onchange, null);
  t = make(); t.unwatch(); await Promise.resolve(); await Promise.resolve(); assert.equal(t.permissionStatus.onchange, undefined, 'an answer after the cleanup is ignored');
  console.log('notification banner test passed');
})().catch(error => { console.error(error); process.exit(1); });
