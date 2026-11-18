// The Settings registry (web/js/pages/shell/registry.js), run in a vm: it is consistent with itself, with the shell's rights, and with the pages -- a row a
// page draws without an entry here (so without a place in the navigation and in search) fails, and so does an entry that points at nothing.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const pages = path.join(__dirname, '..', 'web', 'js', 'pages');
const load = file => fs.readFileSync(path.join(pages, 'shell', file), 'utf8').replace(/^export /gm, '');
const context = vm.createContext({});
vm.runInContext(`${load('model.js')}\n${load('registry.js')}\nglobalThis.api = { TABS, tabHidden, SECTIONS, CATEGORIES, SETTINGS, ALIASES, TRANSIENT_ID_PREFIXES, categoryPath, settingPath, parsePath, resolveTarget, categoryRights };`, context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));
const { TABS, tabHidden, SECTIONS, CATEGORIES, SETTINGS, ALIASES, TRANSIENT_ID_PREFIXES } = api;

const walk = dir => fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? walk(path.join(dir, entry.name)) : [path.join(dir, entry.name)]);
const sourceFiles = walk(pages).filter(file => file.endsWith('.js') && !file.includes(`${path.sep}shell${path.sep}`));
sourceFiles.push(path.join(pages, '..', 'components', 'usage-summary.js'));
const sources = sourceFiles.map(file => fs.readFileSync(file, 'utf8'));
const allSource = sources.join('\n');

// Structure.
const unique = (list, what) => assert.equal(new Set(list).size, list.length, `${what} are unique`);
unique(SECTIONS.map(section => section.id), 'section ids');
unique(CATEGORIES.map(category => category.id), 'category ids');
const pageKeys = TABS.map(([key]) => key).filter(key => key !== 'agents');
for (const category of CATEGORIES) {
  assert.ok(SECTIONS.some(section => section.id === category.section), `${category.id}: its section exists`);
  assert.ok(pageKeys.includes(category.page), `${category.id}: its page is one of the shell's pages`);
  assert.ok(category.title && category.icon && category.description, `${category.id}: title, icon and description`);
  assert.doesNotThrow(() => tabHidden(api.categoryRights(category), { owner: false, admin: false, usagesHidden: false, users: 'shown' }), `${category.id}: its rights key is one tabHidden knows`);
  for (const block of category.blocks || []) assert.ok(allSource.includes(`id="${block}"`), `${category.id}: block ${block} is drawn by a page`);
  for (const sub of category.subs || []) assert.ok(SETTINGS.some(setting => setting.category === category.id && setting.anchor === sub.anchor), `${category.id}: sub ${sub.id} points at one of its settings`);
}
for (const section of SECTIONS) assert.ok(CATEGORIES.some(category => category.section === section.id), `${section.id}: has a category`);
for (const key of pageKeys) assert.ok(CATEGORIES.some(category => category.page === key), `the ${key} page has a category`);
assert.deepEqual(plain(Object.keys(ALIASES).sort()), plain(pageKeys.slice().sort()), 'every page name keeps an alias');
const anchors = SETTINGS.filter(setting => setting.anchor).map(setting => setting.anchor);
unique(anchors, 'setting anchors');
for (const setting of SETTINGS) {
  assert.ok(CATEGORIES.some(category => category.id === setting.category), `${setting.anchor || setting.template}: its category exists`);
  assert.ok(setting.anchor || setting.template, 'an entry has an anchor or a template');
  assert.ok(setting.name && setting.description && setting.group && Array.isArray(setting.keywords), `${setting.anchor || setting.template}: name, description, group, keywords`);
}

// The pages: every row they draw has an entry, and every entry is drawn.
const regexOf = text => new RegExp(`^${text.replace(/[.*+?^()|[\]\\]/g, '\\$&').replace(/\$\{[^}]*\}/g, '.+')}$`);
const drawn = [];
for (const source of sources) {
  for (const line of source.split('\n')) {
    if (line.includes('<${SettingsRow')) {
      const found = /\bid=(?:"([^"]+)"|\$\{`([^`]+)`\})/.exec(line);
      if (found) drawn.push((found[1] || found[2]).replace(/^\$\{idPrefix\}/, ''));
    }
    for (const found of line.matchAll(/(?<![.\w])toggle\('([a-z-]+)'/g)) drawn.push(found[1]);
  }
}
assert.ok(drawn.length > 40, `the scan sees the pages' rows (${drawn.length})`);
const known = SETTINGS.flatMap(setting => [setting.anchor, setting.template]).filter(Boolean);
for (const id of drawn) {
  if (TRANSIENT_ID_PREFIXES.some(prefix => id.startsWith(prefix)) && !known.includes(id)) continue; // an editor or a dialog a category opens
  const matcher = regexOf(id);
  assert.ok(known.some(entry => entry === id || matcher.test(entry)), `a page draws "${id}", which has no entry in the Settings registry (web/js/pages/shell/registry.js)`);
}
const templates = drawn.filter(id => id.includes('${')).map(regexOf);
// A prompt slot's textarea is prompt-<key>, the key being one of the shipped slots the server lists.
const slotSource = fs.readFileSync(path.join(__dirname, '..', 'internal', 'adapter', 'prompt', 'prompt.go'), 'utf8');
const slotKeys = [...slotSource.matchAll(/\{"([a-z_]+)", "/g)].map(found => found[1]);
assert.ok(slotKeys.length >= 6, 'the shipped prompt slots are found');
const slotEntries = known.filter(entry => entry.startsWith('prompt-') && slotKeys.includes(entry.slice(7)));
assert.equal(slotEntries.length, slotKeys.length, 'every shipped prompt slot has an entry');
for (const entry of known) {
  if (slotEntries.includes(entry)) continue;
  const literal = entry.includes('${') ? null : entry;
  assert.ok(drawn.includes(entry) || templates.some(matcher => matcher.test(entry)) || (literal && allSource.includes(literal)), `registry entry "${entry}" is not drawn by any page`);
}

// The General page is shared by several categories: v2-only.css hides the blocks a category does not show, one rule per category, naming its blocks.
const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'v2-only.css'), 'utf8');
for (const category of CATEGORIES.filter(item => item.blocks)) {
  const rule = css.split('\n').find(line => line.includes(`#settings-general[data-cat="${category.id}"]`));
  assert.ok(rule, `v2-only.css hides the other blocks of the General page for ${category.id}`);
  for (const block of category.blocks) assert.ok(rule.includes(`#${block}`), `...and keeps ${block} for ${category.id}`);
}

// Paths.
const find = id => CATEGORIES.find(category => category.id === id);
assert.equal(api.categoryPath(find('appearance')), 'settings/personal/appearance');
assert.equal(api.categoryPath(find('storage'), 'prompt-cache'), 'settings/server/storage/prompt-cache');
assert.deepEqual(plain(api.parsePath('#settings/agents/prompts/mentions')), { section: 'agents', category: 'prompts', sub: 'mentions' });
assert.deepEqual(plain(api.parsePath('settings/agents')), { section: 'agents', category: '', sub: '' });
assert.deepEqual(plain(api.parsePath('settings')), { section: '', category: '', sub: '' });
assert.deepEqual(plain(api.parsePath('settings/server/roles')), { section: 'server', category: '', sub: '' }, 'a category under the wrong section is not found');
assert.deepEqual(plain(api.parsePath('settings/nope/x')), { section: '', category: '', sub: '' });
assert.equal(api.parsePath('room_01ABC'), null, 'a room id is not a settings path');
assert.equal(api.parsePath('settingsx/personal'), null);
assert.deepEqual(plain(api.resolveTarget('usages')), { section: 'server', category: 'usages', sub: '' }, 'the old page names resolve');
assert.deepEqual(plain(api.resolveTarget('nope')), { section: '', category: '', sub: '' });
assert.equal(api.settingPath(SETTINGS.find(setting => setting.anchor === 'prompt-mentions')), 'Agents › Prompts › Mentions');

// Rights: today's rules, per category.
const shownFor = rights => plain(CATEGORIES.filter(category => !tabHidden(api.categoryRights(category), { owner: false, admin: false, usagesHidden: false, users: 'shown', ...rights })).map(category => category.id));
assert.equal(shownFor({ owner: true }).length, CATEGORIES.length, 'the owner sees every category');
assert.deepEqual(shownFor({ usagesHidden: true }), ['appearance', 'chat', 'notifications', 'users', 'telemetry', 'storage', 'isolation'], 'anyone else keeps their own settings, Users (once the access check allows) and what the General page always showed');
assert.equal(shownFor({ admin: true, usagesHidden: true }).includes('usages'), false, 'a refused usage read takes Accounts & usage away');
assert.equal(shownFor({ admin: true }).includes('backup'), true, 'an admin exports the backup');
console.log('settings registry tests passed');
