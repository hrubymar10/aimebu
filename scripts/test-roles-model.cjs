// The Roles page's pure rules (web/js/pages/roles/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'roles', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { CAPS, roleOptionLabel, formFromRole, formState, roleData, extendsOptions, chipsOf, summaryOf, editHeading, resetQuestion };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const reviewer = { id: 'r1', rev: 3, role_key: 'reviewer', label: 'Reviewer', emoji: '🔎', body: 'Review it.', cardinality: 'multi', builtin: true, default_agents: [{ launcher_id: 'l1', model: 'a', extra: 'dropped' }], pick_mode: 'randomly' };
const sec = { id: 'r2', rev: 1, role_key: 'sec', label: 'Security', emoji: '🛡️', body: '', resolved_body: 'Review it. Look at secrets.', extends: 'reviewer', cardinality: 'singleton', cap_is_lead: true, cap_can_spawn: true, cap_singleton_writer: true };

// A new role starts blank, with many holders and the in-turn pick order; a stored one fills every field and copies its defaults.
assert.deepEqual(plain(m.formFromRole(null)), { cardinality: 'multi', default_agents: [], pick_mode: 'in_turn', role_key: '', label: '', emoji: '', body: '', extends: '', cap_is_lead: false, cap_can_spawn: false, cap_singleton_writer: false });
const form = m.formFromRole(reviewer);
assert.deepEqual(plain([form.role_key, form.label, form.cardinality, form.pick_mode, form.default_agents]), ['reviewer', 'Reviewer', 'multi', 'randomly', [{ launcher_id: 'l1', model: 'a' }]]);
assert.notEqual(form.default_agents[0], reviewer.default_agents[0], 'the form owns its copy of the defaults');

// An edit changes the state string; a copy of the same values does not.
const base = m.formState(form);
assert.equal(m.formState(m.formFromRole(reviewer)), base);
for (const change of [{ label: 'Other' }, { cap_is_lead: true }, { cardinality: 'singleton' }, { pick_mode: 'in_turn' }, { default_agents: [] }]) assert.notEqual(m.formState({ ...form, ...change }), base, JSON.stringify(change));

// A save sends trimmed text, the capabilities and defaults, and the id and revision of a stored role only.
assert.deepEqual(plain(m.roleData({ ...form, role_key: ' reviewer ', label: ' R ', emoji: ' x ' }, reviewer)), { role_key: 'reviewer', label: 'R', emoji: 'x', body: 'Review it.', extends: '', cardinality: 'multi', cap_is_lead: false, cap_can_spawn: false, cap_singleton_writer: false, default_agents: [{ launcher_id: 'l1', model: 'a' }], pick_mode: 'randomly', id: 'r1', rev: 3 });
assert.equal('id' in plain(m.roleData(m.formFromRole(null), undefined)), false);

// What a role may extend: every other role by key, sorted; a parent that is gone is kept and named.
assert.deepEqual(plain(m.extendsOptions([sec, reviewer], sec)), [['', 'None'], ['reviewer', '🔎 Reviewer (reviewer)']]);
assert.deepEqual(plain(m.extendsOptions([reviewer, sec], null)).map(([value]) => value), ['', 'reviewer', 'sec']);
assert.deepEqual(plain(m.extendsOptions([reviewer], { ...sec, extends: 'gone' })).at(-1), ['gone', 'gone (missing)']);

// Chips, summary and texts.
assert.deepEqual(plain(m.chipsOf(sec, [sec, reviewer])), [['one holder'], ['extends Reviewer', 'Adds to the instructions of reviewer'], ['leader'], ['can spawn']]);
assert.deepEqual(plain(m.chipsOf(reviewer, [])), [['many holders'], ['built in']]);
assert.equal(m.chipsOf({ ...sec, extends: 'gone' }, []).find(([text]) => text.startsWith('extends'))[0], 'extends gone');
assert.equal(m.summaryOf(sec), 'Review it. Look at secrets.');
assert.equal(m.summaryOf({ body: '' }), 'No instructions of its own.');
assert.equal(m.summaryOf({ body: 'x '.repeat(200) }).length, 158);
assert.equal(m.summaryOf({ body: '  one\n\n two  ' }), 'one two');
assert.deepEqual([m.editHeading(null), m.editHeading(reviewer), m.roleOptionLabel(reviewer)], ['New role', 'Editing Reviewer', 'Reviewer (reviewer)']);
console.log('roles model tests passed');
