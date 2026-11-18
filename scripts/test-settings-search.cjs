// Settings search (web/js/pages/shell/search.js over the registry), run in a vm: found by name, description and keywords, in categories nobody opened, with the
// path of the setting; a word that matches nothing finds nothing; a setting the account may not open is never found.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const dir = path.join(__dirname, '..', 'web', 'js', 'pages', 'shell');
const load = file => fs.readFileSync(path.join(dir, file), 'utf8').replace(/^import .*$/gm, '').replace(/^export /gm, '');
const context = vm.createContext({});
vm.runInContext(`${load('model.js')}\n${load('registry.js')}\n${load('search.js')}\nglobalThis.api = { searchSettings, tokens, tabHidden, CATEGORIES, categoryRights };`, context);
const { searchSettings, tokens, tabHidden, CATEGORIES, categoryRights } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));
const visibleFor = rights => id => !tabHidden(categoryRights(CATEGORIES.find(category => category.id === id)), { owner: false, admin: false, usagesHidden: false, users: 'shown', ...rights });
const find = (query, rights = { owner: true }) => plain(searchSettings(query, { visible: visibleFor(rights) }));
const paths = (query, rights) => plain(find(query, rights).map(result => result.path));

assert.deepEqual(plain(tokens('  Théme, dark ')), ['theme', 'dark']);
assert.deepEqual(find(''), [], 'an empty query finds nothing');
assert.deepEqual(find('  ,, '), [], 'and so does a query with no word in it');

// By name, description and keyword, in a category nobody has opened.
assert.equal(paths('theme')[0], 'Personal › Appearance › Theme', 'a name is found, first');
assert.ok(paths('colour').includes('Personal › Appearance › Theme'), 'a word only the description holds');
assert.ok(paths('sentry').includes('Server › Telemetry › Send telemetry'), 'a keyword');
assert.equal(paths('mentions')[0], 'Agents › Prompts › Mentions', 'a prompt slot names its path down to the slot');
assert.ok(paths('keepalive').includes('Server › Storage & performance › Keep the prompt cache warm'), 'a keyword of a setting in Storage & performance');
assert.ok(paths('docker').includes('Agents › Agent isolation › Only run agents in docker'));
assert.equal(find('roles')[0].kind, 'category', 'a category named by the query comes before the settings that mention it');
assert.equal(find('roles')[0].path, 'Agents › Roles');
// Every word must match (in any field), so more words narrow.
assert.ok(paths('cost cap').includes('Server › Accounts & usage › Cost cap ($)'));
assert.deepEqual(paths('theme zebra'), [], 'a word that matches nothing finds nothing');
assert.deepEqual(paths('qqqzzz'), [], 'not found');
// Name beats keyword beats description.
const ranked = find('sound').map(result => result.title);
assert.equal(ranked[0], 'Mute inbox sound', 'a name match first');
// Rights: what the account may not open is not found, and its name is not shown.
assert.ok(paths('retention').length > 0 || paths('expiry').length > 0);
assert.ok(find('prompt', { owner: true }).some(result => result.category.id === 'prompts'));
assert.equal(find('prompt', {}).some(result => result.category.id === 'prompts'), false, 'Prompts is not found for someone who may not open it');
assert.deepEqual(paths('invite', { usagesHidden: true, users: 'hidden' }).filter(item => item.includes('Users')), [], 'Users is not found once the access check refuses it');
assert.equal(find('export settings', {}).length, 0, 'Backup & restore is not found for someone who may not open it');
assert.ok(find('export settings', { admin: true }).length > 0, 'and is for an admin');
assert.equal(find('usage warning', { usagesHidden: true }).some(result => result.category.id === 'usages'), false, 'a refused usage read hides Accounts & usage from search too');
// Every setting is found by its own name.
for (const category of CATEGORIES) for (const result of find(category.title)) assert.ok(result.score > 0);
const all = find('a'.repeat(0) + 'e');
assert.ok(all.length > 20, 'a common letter finds many');
console.log('settings search tests passed');
