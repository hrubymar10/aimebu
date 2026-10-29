// The top bar menu's pure rules (web/js/pages/topbar/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'topbar', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { nextIndex, MOVE_KEYS, visibleIndexes, TUCK_ORDER, planTuck };', context);
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

// planTuck: what the phone bar gives up, in order, so that the rest fits
assert.deepEqual(plain(m.TUCK_ORDER), [3, 0, 2, 1], 'Sign out, System events, Memory, then Health');
const bar = widths => widths.map((width, at) => ({ item: at - 1, width })); // the first control (Rooms) is never tucked; then System events, Health, Memory, Sign out
const plan = (available, entries) => plain(m.planTuck({ available, gap: 2, moreWidth: 40, entries }));
const five = bar([40, 40, 40, 40, 40]);
assert.deepEqual(plan(500, five), [], 'everything fits: nothing is tucked');
assert.deepEqual(plan(208, five), [], 'an exact fit is a fit');
assert.deepEqual(plan(207, five), [3, 0], 'one pixel short: Sign out goes first, and the menu button takes the room it freed, so System events goes too');
assert.deepEqual(plan(166, five), [3, 0], 'two tucked controls plus the menu button fit exactly');
assert.deepEqual(plan(165, five), [3, 0, 2], 'then Memory');
assert.deepEqual(plan(124, five), [3, 0, 2], 'Rooms, Health and the menu button');
assert.deepEqual(plan(123, five), [3, 0, 2, 1], 'Health goes last');
assert.deepEqual(plan(10, five), [3, 0, 2, 1], 'and the loop ends when nothing is left to tuck');
assert.deepEqual(plan(0, [{ item: -1, width: 40 }, { item: -1, width: 40 }]), [], 'controls that are never tucked are never tucked');
assert.deepEqual(plan(150, [{ item: -1, width: 40 }, { item: 1, width: 40 }, { item: 3, width: 40 }, { item: -1, width: 40 }]), [3, 1], 'controls absent from the bar (System events, Memory) are skipped in the order');
assert.deepEqual(plan(200, [{ item: -1, width: 40 }, { item: 1, width: 40 }, { item: -1, width: 40 }, { item: 3, width: 112 }]), [3], 'a wide Sign out is enough');
assert.deepEqual(plan(500, []), [], 'an empty bar');
console.log('topbar-model: ok');
