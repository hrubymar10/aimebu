// The shared context menu's plain rules (web/js/components/context-menu-model.js) in a vm: where the menu goes on the screen, the keys that move
// between its items, the keys that open it, and which rows it draws.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'components', 'context-menu-model.js'), 'utf8');
assert.doesNotMatch(source, /^import /m, 'the model must stay import-free');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { placeMenu, stepIndex, opensMenu, menuRows, MENU_MARGIN };', context);
const { placeMenu, stepIndex, opensMenu, menuRows, MENU_MARGIN } = context.api;
const same = (actual, expected, message) => assert.deepEqual(JSON.parse(JSON.stringify(actual)), expected, message);

// Placement: at the point; flipped left or up at the right or bottom edge; clamped to the margin; a menu taller than the screen scrolls.
const viewport = { width: 800, height: 600 }, size = { width: 200, height: 300 };
assert.equal(MENU_MARGIN, 8);
same(placeMenu({ x: 100, y: 100 }, size, viewport), { left: 100, top: 100, maxHeight: null }, 'room for it at the point');
same(placeMenu({ x: 700, y: 100 }, size, viewport), { left: 500, top: 100, maxHeight: null }, 'flipped to the left of the point at the right edge');
same(placeMenu({ x: 100, y: 500 }, size, viewport), { left: 100, top: 200, maxHeight: null }, 'flipped above the point at the bottom edge');
same(placeMenu({ x: 150, y: 200 }, { width: 200, height: 450 }, viewport), { left: 150, top: 142, maxHeight: null }, 'neither below nor above fits: as low as fits');
same(placeMenu({ x: -20, y: -20 }, size, viewport), { left: 8, top: 8, maxHeight: null }, 'a point off the screen is clamped to the margin');
same(placeMenu({ x: 100, y: 100 }, { width: 200, height: 900 }, viewport), { left: 100, top: 8, maxHeight: 584 }, 'too tall: the whole height less the margins, scrolling');
same(placeMenu({ x: 10, y: 10 }, { width: 400, height: 100 }, { width: 300, height: 600 }), { left: 8, top: 10, maxHeight: null }, 'wider than a phone: kept at the margin');

// Keys between items: the arrows wrap, Home and End go to the ends, nothing else moves; with no item focused the arrows start at an end.
assert.equal(stepIndex('ArrowDown', 0, 3), 1);
assert.equal(stepIndex('ArrowDown', 2, 3), 0, 'down from the last wraps to the first');
assert.equal(stepIndex('ArrowUp', 0, 3), 2, 'up from the first wraps to the last');
assert.equal(stepIndex('ArrowDown', -1, 3), 0);
assert.equal(stepIndex('ArrowUp', -1, 3), 2);
assert.equal(stepIndex('Home', 2, 3), 0);
assert.equal(stepIndex('End', 0, 3), 2);
assert.equal(stepIndex('a', 0, 3), null);
assert.equal(stepIndex('ArrowDown', 0, 0), null, 'no item: nothing to move to');

// The keys that open the menu of what has the focus.
assert.equal(opensMenu({ key: 'ContextMenu' }), true);
assert.equal(opensMenu({ key: 'F10', shiftKey: true }), true);
assert.equal(opensMenu({ key: 'F10' }), false);
assert.equal(opensMenu({ key: 'F10', shiftKey: true, ctrlKey: true }), false);
assert.equal(opensMenu({ key: 'Enter' }), false);

// Rows: hidden items go, then empty groups; separators only between rows.
const ids = rows => [...rows].map(row => row.separator ? '|' : row.group !== undefined ? `[${row.items.map(item => item.id).join(',')}]` : row.id);
assert.deepEqual(ids(menuRows([{ separator: true }, { id: 'a' }, { separator: true }, { separator: true }, { id: 'b', hidden: true }, { separator: true }, { id: 'c' }, { separator: true }])), ['a', '|', 'c']);
assert.deepEqual(ids(menuRows([{ id: 'a' }, { separator: true }, { group: 'Levels', items: [{ id: 'x', hidden: true }] }, { separator: true }, { id: 'b' }])), ['a', '|', 'b'], 'a group with nothing shown goes');
assert.deepEqual(ids(menuRows([{ group: 'Levels', items: [{ id: 'x' }, { id: 'y', hidden: true }, { id: 'z' }] }])), ['[x,z]']);
assert.deepEqual(ids(menuRows([null, { id: 'a', hidden: true }])), []);
console.log('context menu model: ok');
