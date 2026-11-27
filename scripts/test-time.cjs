// The agent card's state age is a pure function in a browser module, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
process.env.TZ = 'America/New_York';
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'time.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { stateAge, absoluteTime, absoluteTimes };', context);
const { stateAge, absoluteTime, absoluteTimes } = context.api;

assert.equal(stateAge(0), '0:00');
assert.equal(stateAge(44), '0:44');
assert.equal(stateAge(61), '1:01');
assert.equal(stateAge(3599), '59:59');
// From an hour on whole hours, from a day on whole days: never a clock that grows wider than the card.
assert.equal(stateAge(3600), '1h');
assert.equal(stateAge(3 * 3600 + 1800), '3h');
assert.equal(stateAge(86399), '23h');
assert.equal(stateAge(86400), '1d');
assert.equal(stateAge(4 * 86400 + 5), '4d');
// The case that overflowed: 104 hours 40 minutes.
assert.equal(stateAge(104 * 3600 + 40 * 60 + 5), '4d');
assert.equal(stateAge(-5), '0:00');
// The absolute date behind a relative time: date and time to the second in the person's zone, with the zone's abbreviation (fixed TZ, fixed instant).
const stamp = absoluteTime('2026-03-05T14:07:09Z');
assert.match(stamp, /2026/);
assert.match(stamp, /9:07:09/, `the local time to the second in America/New_York: ${stamp}`);
assert.match(stamp, /EST/, `the zone abbreviation: ${stamp}`);
assert.match(absoluteTime('2026-07-05T14:07:09Z'), /EDT/, 'daylight time gets its own abbreviation');
assert.equal(absoluteTime(''), '');
assert.equal(absoluteTime('not a time'), '');
assert.equal(absoluteTimes('a', '', undefined, 'b'), 'a · b');
console.log('time test passed');
