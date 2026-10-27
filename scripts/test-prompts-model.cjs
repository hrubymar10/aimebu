// The Prompts page's pure rules (web/js/pages/prompts/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'prompts', 'model.js'), 'utf8');
assert.ok(!/^import /m.test(source), 'model.js stays import-free so it can run here');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { scopeOptions, pickScope, layerOf, sourceLabel, resetLabel, savedText, resetText, noEditText, countText, savePath, saveBody, resetPath, previewText, keepDrafts };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

// Scopes: global first, then each project; the one shown before when it still exists, else global.
const options = m.scopeOptions([{ id: 'p1', name: 'One' }, { id: 'p2', name: 'Two' }]);
assert.deepEqual(plain(options), [['', 'Global (all projects)'], ['p1', 'Project: One'], ['p2', 'Project: Two']]);
assert.equal(m.pickScope(options, 'p2'), 'p2');
assert.equal(m.pickScope(options, 'gone'), '', 'a scope that no longer exists is not opened');
assert.equal(m.pickScope(options, ''), '', 'nothing known falls back to global');

// The layer a scope edits, and how its source reads.
const slot = { key: 'tail', title: 'Closing', source: 'global', global: { rev: 3 }, project: null, effective: 'g', default: 'shipped' };
assert.equal(m.layerOf(slot, '').rev, 3);
assert.equal(m.layerOf(slot, 'p1'), null);
assert.deepEqual([m.sourceLabel(slot, ''), m.sourceLabel(slot, 'p1'), m.sourceLabel({ ...slot, source: 'project', project: { rev: 1 } }, 'p1'), m.sourceLabel({ ...slot, source: 'default', global: null }, '')],
  ['Global override (saved here)', 'Global override', 'Project override (this project)', 'Shipped text']);
assert.deepEqual([m.resetLabel(''), m.resetLabel('p1'), m.resetText(slot, ''), m.resetText(slot, 'p1'), m.savedText(slot)],
  ['Reset to shipped', 'Reset to global/shipped', '“Closing” is back to the shipped text.', '“Closing” is back to the next level up.', 'Saved “Closing”.']);
assert.ok(m.noEditText('p1').includes('project owner') && m.noEditText('').includes('owner or an admin'));

// Writes: the layer's revision, 0 to create; the key and scope are encoded.
assert.deepEqual(plain(m.saveBody(slot, '', 'new')), { project_id: '', value: 'new', rev: 3 });
assert.deepEqual(plain(m.saveBody(slot, 'p1', 'new')), { project_id: 'p1', value: 'new', rev: 0 }, 'a scope without a layer creates one');
assert.equal(m.savePath({ key: 'a/b' }), '/api/prompts/a%2Fb');
assert.equal(m.resetPath({ ...slot, project: { rev: 9 } }, 'p 1'), '/api/prompts/tail?project_id=p%201&rev=9');

// The count is in code points, as the server counts the limit.
assert.equal(m.countText('👍é', 2000), '2 / 2000');

// The preview: drafts over saved text, a blank box falls back to the shipped text, locked parts in brackets.
const slots = ['identity', 'retry_notice', 'inbox_ask', 'mentions', 'human_reply', 'tail'].map(key => ({ key, effective: `${key} saved`, default: `${key} shipped` }));
const preview = m.previewText(slots, new Map([['tail', 'tail draft'], ['mentions', '   ']])).split('\n\n');
assert.equal(preview.length, 12);
assert.deepEqual([preview[0], preview[1], preview[5], preview[7], preview[10], preview[11]], ['[Opening line (set by the agent adapter)]', 'identity saved', '[Response contract and schema (fixed)]', 'mentions shipped', 'tail draft', '[Closing line (adapter)]']);

// Drafts across a reload: kept while they differ from the new text, dropped for the slot just written and for slots that went away.
const drafts = new Map([['tail', 'mine'], ['identity', 'identity saved'], ['mentions', 'typed'], ['gone', 'x']]);
assert.deepEqual(plain([...m.keepDrafts(drafts, slots)]), [['tail', 'mine'], ['mentions', 'typed']]);
assert.deepEqual(plain([...m.keepDrafts(drafts, slots, 'tail')]), [['mentions', 'typed']]);

console.log('prompts model tests passed');
