// The presence heartbeat's gate, in a vm with a fake clock and a stubbed DOM:
// it sends only for a visible page with recent input, at most once a minute,
// and never for a hidden tab or an idle one.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'presence.js'), 'utf8')
  .replace(/^import .*$/m, '')
  .replace(/^export /gm, '');

let now = Date.UTC(2026, 10, 20, 12, 0, 0);
let interval = null;
const listeners = {};
const calls = [];
const context = {
  Date: { now: () => now },
  setInterval: (fn, ms) => { interval = { fn, ms }; return 1; },
  clearInterval: () => { interval = null; },
  document: {
    visibilityState: 'visible',
    addEventListener: (name, handler) => { (listeners[name] ||= []).push(handler); },
    removeEventListener: () => {},
  },
  request: async (path, options) => { calls.push([path, options.method]); },
};
vm.createContext(context);
vm.runInContext(source + '\nglobalThis.presence = { PresenceHeartbeat, SEND_INTERVAL_MS, INPUT_WINDOW_MS, INPUT_EVENTS };', context);
const { PresenceHeartbeat, SEND_INTERVAL_MS, INPUT_WINDOW_MS, INPUT_EVENTS } = context.presence;

assert.equal(SEND_INTERVAL_MS, 60000, 'the send interval is a minute');
assert.equal(INPUT_WINDOW_MS, 60000, 'input older than a minute is not vouched for');
assert.ok(INPUT_EVENTS.includes('keydown') && INPUT_EVENTS.includes('pointerdown') && INPUT_EVENTS.includes('wheel') && INPUT_EVENTS.includes('touchstart'),
  'the input events are the ways a person shows they are using the page');

(async () => {
  const beat = new PresenceHeartbeat();

  // Starting on a hidden page sends nothing.
  context.document.visibilityState = 'hidden';
  beat.start();
  assert.deepEqual(calls, [], 'a hidden page stays silent');
  assert.ok(interval, 'the timer runs while hidden, ready for the page to show');

  // Input on a visible page sends one heartbeat.
  context.document.visibilityState = 'visible';
  for (const handler of listeners.pointerdown) handler();
  assert.deepEqual(calls, [['/api/presence', 'POST']], 'visible plus input sends the heartbeat');

  // More input within the minute is throttled; the minute over, input sends again.
  now += 10000;
  for (const handler of listeners.keydown) handler();
  assert.equal(calls.length, 1, 'input within the send interval is throttled');
  now += SEND_INTERVAL_MS + 1000;
  for (const handler of listeners.keydown) handler();
  assert.deepEqual(calls, [['/api/presence', 'POST'], ['/api/presence', 'POST']], 'input after the interval sends again');

  // The timer without fresh input sends nothing: an idle visible page is not activity.
  const last = calls.length;
  now += SEND_INTERVAL_MS + 1000;
  interval.fn();
  assert.equal(calls.length, last, 'an idle page sends nothing');

  // Returning to a visible page without recent input sends nothing either.
  context.document.visibilityState = 'hidden';
  now += SEND_INTERVAL_MS + 1000;
  for (const handler of listeners.visibilitychange) handler();
  assert.equal(calls.length, last, 'a hidden page stays silent');
  context.document.visibilityState = 'visible';
  for (const handler of listeners.visibilitychange) handler();
  assert.equal(calls.length, last, 'becoming visible without input sends nothing');

  // A refused or unreachable heartbeat is swallowed: the next input tries again.
  context.request = async () => { throw new Error('offline'); };
  now += SEND_INTERVAL_MS + 1000;
  for (const handler of listeners.pointerdown) handler();
  now += SEND_INTERVAL_MS + 1000;
  for (const handler of listeners.pointerdown) handler();
  await Promise.resolve(); // the rejection is caught inside maybeSend, not thrown to the caller

  // Stopping removes the timer: nothing further is sent, whatever the handlers still see.
  beat.stop();
  assert.equal(interval, null, 'the timer is gone');
  context.request = async (path, options) => { calls.push([path, options.method]); };
  now += SEND_INTERVAL_MS + 1000;
  for (const handler of listeners.pointerdown) handler();
  assert.equal(calls.length, last, 'a stopped heartbeat sends nothing');
})().then(() => console.log('test-presence: ok'), error => { console.error(error); process.exit(1); });
