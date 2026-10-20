// The expiry planner is a browser module with no DOM or network use, so this test runs it in a vm:
// the cutoff rule, which messages are expired, and where the single divider goes.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'expiry.js'), 'utf8');
assert.match(source, /export function planExpiry/, 'planExpiry is missing from expiry.js');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { expiryCutoffMs, isExpired, planExpiry, validExpirySeconds, EXPIRY_DEFAULT_SECONDS, EXPIRY_MIN_SECONDS, EXPIRY_MAX_SECONDS };', context);
const { expiryCutoffMs, isExpired, planExpiry, validExpirySeconds } = context.api;

const now = Date.UTC(2026, 9, 17, 12, 0, 0);
const hour = 3600 * 1000;

// The cutoff: 0 means off; otherwise now minus the window.
assert.equal(context.api.EXPIRY_DEFAULT_SECONDS, 21600);
assert.equal(expiryCutoffMs(now, 0), 0);
assert.equal(expiryCutoffMs(now, 'x'), 0);
assert.equal(expiryCutoffMs(now, -5), 0);
assert.equal(expiryCutoffMs(now, 21600), now - 6 * hour);

// A message is expired when now - created >= window: exactly at the window it is.
const cutoff = expiryCutoffMs(now, 21600);
assert.equal(isExpired(now - 6 * hour, cutoff), true, 'exactly at the window is expired');
assert.equal(isExpired(now - 6 * hour + 1, cutoff), false, 'one millisecond younger is live');
assert.equal(isExpired(now - 7 * hour, cutoff), true);
assert.equal(isExpired(NaN, cutoff), false, 'an unreadable timestamp is never expired');
assert.equal(isExpired(now - 99 * hour, 0), false, 'expiry off never expires');

const msg = (id, ageHours) => ({ id, createdMs: now - ageHours * hour });

// Old above, live below: the divider goes before the first live message.
let plan = planExpiry([msg('a', 9), msg('b', 7), msg('c', 1), msg('d', 0.1)], cutoff);
assert.deepEqual([...plan.expired], ['a', 'b']);
assert.equal(plan.divider, 'c');
assert.equal(plan.count, 2);

// Nothing expired: no divider. Everything expired: no divider (there is no live line to draw above).
plan = planExpiry([msg('a', 1), msg('b', 0.5)], cutoff);
assert.equal(plan.divider, null); assert.equal(plan.count, 0);
plan = planExpiry([msg('a', 9), msg('b', 8)], cutoff);
assert.equal(plan.divider, null); assert.equal(plan.count, 2);
assert.equal(planExpiry([], cutoff).divider, null);

// Clock skew can interleave expired and live later on: the divider latches at the first transition, once.
plan = planExpiry([msg('a', 9), msg('b', 1), msg('c', 8), msg('d', 0.2)], cutoff);
assert.deepEqual([...plan.expired], ['a', 'c']);
assert.equal(plan.divider, 'b', 'one divider, at the first expired-to-live transition');

// Expiry off: nothing is marked.
plan = planExpiry([msg('a', 9000), msg('b', 1)], 0);
assert.equal(plan.count, 0); assert.equal(plan.divider, null);

// A live message first, then expired ones: no divider (the line only separates expired-then-live).
plan = planExpiry([msg('a', 1), msg('b', 9)], cutoff);
assert.equal(plan.divider, null);

// The setting's own validation mirrors the server: 0 or 60 to 2592000, whole numbers.
for (const ok of [0, 60, 21600, 2592000]) assert.equal(validExpirySeconds(ok), true, `${ok} is valid`);
for (const bad of [-1, 1, 59, 2592001, 1.5, NaN, '60', null]) assert.equal(validExpirySeconds(bad), false, `${bad} is invalid`);

// Only expirable messages (AI chatter) are marked; the boundary is by time for everyone.
const human = (id, ageHours) => ({ id, createdMs: now - ageHours * hour, expirable: false });
plan = planExpiry([msg('a', 9), human('h', 8), msg('c', 7), human('i', 1), msg('d', 0.1)], cutoff);
assert.deepEqual([...plan.expired], ['a', 'c'], 'an old human message is never marked');
assert.equal(plan.divider, 'i', 'the boundary sits above the first message newer than the cutoff, human or not');
plan = planExpiry([human('h', 9), human('i', 8), msg('d', 0.1)], cutoff);
assert.equal(plan.count, 0); assert.equal(plan.divider, null, 'nothing marked: no label either');
console.log('expiry planner test passed');
