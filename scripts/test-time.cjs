// The agent card's state age is a pure function in a browser module, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'time.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { stateAge };', context);
const { stateAge } = context.api;

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
console.log('time test passed');
