// The top bar menu's pure rules (web/js/pages/topbar/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'topbar', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { nextIndex, MOVE_KEYS, visibleIndexes };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.deepEqual(plain(m.MOVE_KEYS), ['ArrowDown', 'ArrowUp', 'Home', 'End']);
assert.equal(m.nextIndex(4, 0, 'ArrowDown'), 1);
assert.equal(m.nextIndex(4, 3, 'ArrowDown'), 0, 'wraps forward');
assert.equal(m.nextIndex(4, 1, 'ArrowUp'), 0);
assert.equal(m.nextIndex(4, 0, 'ArrowUp'), 3, 'wraps back');
assert.equal(m.nextIndex(4, 2, 'Home'), 0);
assert.equal(m.nextIndex(4, 0, 'End'), 3);
assert.equal(m.nextIndex(1, 0, 'ArrowDown'), 0);
assert.equal(m.nextIndex(0, -1, 'ArrowDown'), -1, 'nowhere to go with nothing shown');
assert.deepEqual(plain(m.visibleIndexes([{ hidden: false }, { hidden: true }, { hidden: false }])), [0, 2]);
assert.deepEqual(plain(m.visibleIndexes([])), []);
console.log('topbar-model: ok');
