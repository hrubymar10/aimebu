// The pure parts of the settings backup UI (which browser preferences travel, how a section reads), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'settings-backup-format.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { backupPrefKeys, collectPrefs, applyPrefs, describeSection };', context);
const { backupPrefKeys, collectPrefs, applyPrefs, describeSection } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));
const storage = initial => { const data = { ...initial }; return { getItem: key => (key in data ? data[key] : null), setItem: (key, value) => { data[key] = String(value); }, data }; };

// Keys that carry the user's id are named by what they are, so a backup moves between accounts and hosts.
assert.equal(backupPrefKeys('usr_1').theme, 'aimebu2.ui.theme.usr_1');
const store = storage({ 'aimebu2.ui.theme.usr_1': 'light', 'aimebu2.usages.group': 'active', 'aimebu2.lastRoom': 'room_x', 'aimebu2.ui.theme.usr_2': 'dark', secret: 'x' });
assert.deepEqual(plain(collectPrefs(store, 'usr_1')), { theme: 'light', usages_group: 'active' }, 'only known preferences of this user, never the last room or anything else');
assert.deepEqual(plain(collectPrefs(storage({}), 'usr_1')), {});
// A value that is too long is not exported.
assert.deepEqual(plain(collectPrefs(storage({ 'aimebu2.ui.theme.usr_1': 'x'.repeat(300) }), 'usr_1')), {});

// Applying writes this user's keys, ignores unknown names and non-string values.
const target = storage({});
applyPrefs(target, 'usr_9', { theme: 'blue-dark', inbox_muted: '1', evil: 'x', usages_group: 5, rooms_collapsed: 'y'.repeat(300) });
assert.deepEqual(plain(target.data), { 'aimebu2.ui.theme.usr_9': 'blue-dark', 'aimebu2:inbox-muted:usr_9': '1' });

// A section reads as counts.
assert.equal(describeSection({ section: 'macros', new: 2, overwrite: 1, unchanged: 3, invalid: [{ key: 'x', reason: 'r' }] }), 'Macros: 2 new, 1 to overwrite, 3 already the same, 1 skipped');
assert.equal(describeSection({ section: 'quota_switcher', new: 0, overwrite: 0, unchanged: 0, unavailable: 'not running' }), 'Quota switcher: not running');
console.log('settings backup tests passed');
