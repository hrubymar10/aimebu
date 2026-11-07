// The settings shell's pure rules (web/js/pages/shell/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'shell', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { TABS, tabHidden, sectionTitle };', context);
const { TABS, tabHidden, sectionTitle } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.deepEqual(plain(TABS.map(([key]) => key)), ['general', 'usages', 'notifications', 'telemetry', 'roles', 'users', 'prompts', 'macros', 'agents', 'machines', 'harnesses'], 'the nav order, with the Agents caption before Machines');
const shown = rights => plain(TABS.filter(([key]) => !tabHidden(key, { owner: false, admin: false, usagesHidden: false, users: 'shown', ...rights })).map(([key]) => key));
assert.deepEqual(shown({ owner: true }), plain(TABS.map(([key]) => key)), 'the owner sees every tab');
assert.deepEqual(shown({ admin: true, usagesHidden: true }), ['general', 'notifications', 'telemetry', 'roles', 'users', 'prompts', 'macros', 'agents', 'machines', 'harnesses'], 'an admin sees the instance tabs, not Usages');
assert.deepEqual(shown({ usagesHidden: true }), ['general', 'notifications', 'telemetry', 'users'], 'anyone else loses the instance tabs and the Agents caption, but keeps Telemetry (their own fingerprint)');
assert.deepEqual(shown({ owner: true, users: 'hidden' }).includes('users'), false, 'a refused access check takes the Users tab away');
assert.equal(tabHidden('users', { users: 'pending' }), false, 'a pending Users tab still holds its place');
assert.deepEqual([sectionTitle('general'), sectionTitle('machines'), sectionTitle('nope')], ['General', 'Machines', '']);
console.log('shell model tests passed');
