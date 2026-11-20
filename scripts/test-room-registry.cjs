// The registry of a room's and a project's settings (web/js/pages/shell/room-registry.js), run in a vm: consistent with itself, with the dialog's markup and
// sources (every block and anchor it names is drawn by something), with the facts the room model gives (a `when` names a real one), and with the addresses
// (#room/<id>/settings/<category>[/<sub>], #project/<id>/settings/<category>[/<sub>]). Search over it finds a setting by name and never one the viewer may not see.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const web = path.join(__dirname, '..', 'web');
const read = (...parts) => fs.readFileSync(path.join(web, ...parts), 'utf8');
const strip = text => text.replace(/^import .*$/gm, '').replace(/^export /gm, '');
const context = vm.createContext({});
vm.runInContext([strip(read('js/pages/shell/model.js')), strip(read('js/pages/shell/registry.js')), strip(read('js/pages/shell/search.js')), strip(read('js/pages/room-panel/model.js')),
  'globalThis.api = { createRegistry, searchSettings, roomFacts };'].join('\n'), context);
// room-registry.js has names the global registry has (SECTIONS, ...): it runs in a scope of its own.
const roomApi = vm.runInContext(`(function (createRegistry) {\n${strip(read('js/pages/shell/room-registry.js'))}\nreturn { SECTIONS, CATEGORIES, SETTINGS, ROOM_REGISTRY, settingsAddress, addressFor };\n})`, context)(context.api.createRegistry);
const { searchSettings, roomFacts } = context.api;
const { SECTIONS, CATEGORIES, SETTINGS, ROOM_REGISTRY, settingsAddress, addressFor } = roomApi;
const plain = value => JSON.parse(JSON.stringify(value));

const walk = dir => fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? walk(path.join(dir, entry.name)) : [path.join(dir, entry.name)]);
const source = [read('index.html'), ...walk(path.join(web, 'js')).filter(file => file.endsWith('.js') && !file.endsWith('room-registry.js')).map(file => fs.readFileSync(file, 'utf8'))].join('\n');
const drawsId = id => source.includes(`id="${id}"`) || source.includes(`id=\${\`${id}`) || source.includes(`.id = '${id}'`) || source.includes(`id = '${id}'`);
const drawsClass = name => new RegExp(`class(?:Name)?=["'][^"']*\\b${name}\\b`).test(source) || source.includes(`className = '${name}`) || source.includes(`'${name}'`);

// Structure.
const unique = (list, what) => assert.equal(new Set(list).size, list.length, `${what} are unique`);
unique(SECTIONS.map(section => section.id), 'section ids');
unique(CATEGORIES.map(category => category.id), 'category ids (an address needs only the category)');
assert.deepEqual(plain(SECTIONS.map(section => section.id)), ['project', 'room'], 'the project comes above the room');
for (const category of CATEGORIES) {
  assert.ok(SECTIONS.some(section => section.id === category.section), `${category.id}: its section exists`);
  assert.ok(category.title && category.icon && category.description && category.blocks.length, `${category.id}: title, icon, description, blocks`);
  assert.ok(SETTINGS.some(setting => setting.category === category.id), `${category.id}: holds a setting`);
  for (const block of category.blocks) assert.ok(block.startsWith('#') ? drawsId(block.slice(1)) : drawsClass(block.slice(1)), `${category.id}: block ${block} is drawn`);
  for (const sub of category.subs || []) assert.ok(sub.anchor ? drawsId(sub.anchor) : drawsClass(sub.selector.slice(1)), `${category.id}: sub ${sub.id} points at something drawn`);
}
const iconNames = [...read('js/pages/shell/icons.js').matchAll(/^  ([a-z]+): '/gm)].map(found => found[1]);
for (const category of CATEGORIES) assert.ok(iconNames.includes(category.icon), `${category.id}: the icon ${category.icon} exists`);
// A block belongs to one category (the dialog hides every block the shown category does not list).
const blocks = CATEGORIES.flatMap(category => category.blocks);
unique(blocks, 'blocks');
const facts = Object.keys(roomFacts({ kind: 'task', lifecycle: 'open', agent_chat: false }, { renamable: true, role: 'owner', worktreeHidden: false, project: null }));
for (const setting of SETTINGS) {
  const name = setting.anchor || setting.selector;
  assert.ok(CATEGORIES.some(category => category.id === setting.category), `${name}: its category exists`);
  assert.ok(setting.anchor ? drawsId(setting.anchor) : drawsClass(setting.selector.slice(1)), `${name}: is drawn`);
  assert.ok(setting.name && setting.description && setting.group && Array.isArray(setting.keywords), `${name}: name, description, group, keywords`);
  if (setting.when) assert.ok(facts.includes(setting.when), `${name}: when ${setting.when} is a fact the room model gives`);
}
unique(SETTINGS.map(setting => setting.anchor || setting.selector), 'setting anchors');

// Addresses.
assert.deepEqual(plain(settingsAddress('#room/rm_1/settings/members')), { kind: 'room', id: 'rm_1', path: 'settings/members' });
assert.deepEqual(plain(settingsAddress('project/pr_1/settings/limits/usage')), { kind: 'project', id: 'pr_1', path: 'settings/limits/usage' });
assert.deepEqual(plain(settingsAddress('#room/rm_1/settings')), { kind: 'room', id: 'rm_1', path: 'settings' });
assert.equal(settingsAddress('#settings/personal/chat'), null, 'the global Settings address is not one of these');
assert.equal(settingsAddress('#rm_1'), null, 'neither is a room id');
assert.equal(settingsAddress('#room//settings'), null);
assert.equal(settingsAddress('#room/rm_1/other'), null);
const members = ROOM_REGISTRY.categoryById('members'), limits = ROOM_REGISTRY.categoryById('limits');
assert.equal(addressFor(members, '', { roomID: 'rm 1', projectID: 'pr_1' }), 'room/rm%201/settings/members');
assert.equal(addressFor(limits, 'usage', { roomID: 'rm_1', projectID: 'pr_1' }), 'project/pr_1/settings/limits/usage', 'a project category is under the project');
assert.deepEqual(plain(ROOM_REGISTRY.parsePath('settings/limits/usage')), { section: 'project', category: 'limits', sub: 'usage' }, 'the category tells the section');
assert.deepEqual(plain(ROOM_REGISTRY.parsePath('settings/nope')), { section: '', category: '', sub: '' });
assert.equal(ROOM_REGISTRY.parsePath('other/x'), null);
for (const category of CATEGORIES) assert.deepEqual(plain(ROOM_REGISTRY.parsePath(ROOM_REGISTRY.categoryPath(category))), { section: category.section, category: category.id, sub: '' }, `${category.id}: its path parses back`);

// Facts: what each kind of room shows.
const task = roomFacts({ kind: 'task', lifecycle: 'open', project_id: 'p' }, { renamable: true, role: 'owner', worktreeHidden: false });
assert.deepEqual([task.home, task.scope, task.memory, task.notes, task.worktree, task.lifecycle, task.leave, task.remove_room], [false, true, true, true, true, true, false, true]);
const home = roomFacts({ kind: 'home', lifecycle: 'open', project_id: 'p' }, { role: 'owner', project: { importer: true, archive: true, reopen: false, remove: true } });
assert.deepEqual([home.home, home.lifecycle, home.leave, home.remove_room, home.archive, home.reopen, home.importer], [true, false, false, false, true, false, true], 'a home room cannot be archived, left or deleted as a room; the project can');
const dm = roomFacts({ kind: 'dm', lifecycle: 'open', project_id: 'p' }, { role: 'owner' });
assert.deepEqual([dm.scope, dm.remove_room], [false, false]);
assert.deepEqual(plain(roomFacts(null)), {}, 'no room: no facts');
const before = roomFacts({ kind: 'home', lifecycle: 'open', project_id: 'p' }, {});
assert.deepEqual([before.archive, before.remove, before.importer], [true, true, true], 'before the project loads its categories are not dropped');
const agentChat = roomFacts({ kind: 'task', lifecycle: 'open', agent_chat: true }, {});
assert.deepEqual([agentChat.memory, agentChat.leave], [false, true]);

// Search over the registry, with the viewer's facts.
const shown = facts => setting => !(setting.when && !facts[setting.when]);
const find = (query, visibleSetting, visible = () => true) => plain(searchSettings(query, { registry: ROOM_REGISTRY, visible, visibleSetting }).map(result => result.path));
assert.equal(find('schedule', shown(task))[0], 'Room › Agents › Schedules');
assert.ok(find('usage', shown(home)).includes('Project › Limits & usage › Project usage'));
assert.ok(find('mute', shown(task)).includes('Room › Notifications › Mute'));
assert.ok(!find('leave', shown(home)).includes('Room › Leave & remove › Leave chat'), 'a setting the viewer may not see is not found');
assert.ok(find('leave', shown(agentChat)).includes('Room › Leave & remove › Leave chat'));
assert.deepEqual(find('qqqzzz', shown(task)), []);
assert.deepEqual(find('recoveries', shown(task), id => ROOM_REGISTRY.categoryById(id).section === 'room'), [], 'a category the viewer may not open is not searched');
console.log('room registry tests passed');
