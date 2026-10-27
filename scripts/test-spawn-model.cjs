// Picking a model in the Spawn dialog: a model known only by name (a v1 agent's, a Claude session's) is matched to the list by its family, and
// what the person picked survives a rebuild of the list (the catalog arriving late, the same tool picked again). Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'spawn-model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { modelFamily, matchModel, chooseModel, toolKey };', context);
const { modelFamily, matchModel, chooseModel, toolKey } = context.api;

const claude = [{ value: 'haiku', label: 'Haiku' }, { value: 'sonnet', label: 'Sonnet' }, { value: 'opus', label: 'Opus' }];

assert.equal(modelFamily('opus5.5'), 'opus');
assert.equal(modelFamily('claude-sonnet-4-5-20250929'), 'sonnet');
assert.equal(modelFamily('gpt-6-sol'), '');
assert.equal(modelFamily(undefined), '');

// A v1 slug and Claude's own id name the family; an exact or case-different id wins first; a model of no known family matches only itself.
assert.equal(matchModel(claude, 'opus5.5'), 'opus');
assert.equal(matchModel(claude, 'claude-opus-4-5-20251101'), 'opus');
assert.equal(matchModel(claude, 'Sonnet'), 'sonnet');
assert.equal(matchModel(claude, 'haiku'), 'haiku');
assert.equal(matchModel(claude, 'gpt-6-sol'), '');
assert.equal(matchModel(claude, ''), '');
assert.equal(matchModel([{ value: 'opus-4', label: 'Opus 4' }, { value: 'opus', label: 'Opus' }], 'opus'), 'opus');
assert.equal(matchModel([{ value: 'ollama-cloud/gemma4:31b', label: 'Gemma' }], 'ollama-cloud/gemma4:31b'), 'ollama-cloud/gemma4:31b');

const tool = { launcher_id: 'mln_1', tool: 'claude', variant: 'native' };
assert.equal(toolKey(tool), 'mln_1');
assert.equal(toolKey({ tool: 'claude', variant: 'docker' }), 'claude/docker');
assert.equal(toolKey(null), '');

// The person's pick for this tool survives a rebuild; the start's named model, then the harness's default, then the first, fill in otherwise.
const picked = { toolKey: 'mln_1', model: 'opus' };
assert.equal(chooseModel(claude, { picked, toolKey: 'mln_1', named: 'sonnet4.6', preferred: 'haiku' }), 'opus', 'the pick wins over the named model and the default');
assert.equal(chooseModel(claude, { picked, toolKey: 'mln_2', named: 'sonnet4.6', preferred: 'haiku' }), 'sonnet', 'a pick made for another tool does not apply');
assert.equal(chooseModel(claude, { picked: { toolKey: 'mln_1', model: 'gone' }, toolKey: 'mln_1', preferred: 'sonnet' }), 'sonnet', 'a model that is no longer offered falls back');
assert.equal(chooseModel(claude, { toolKey: 'mln_1', named: 'unknown-model', preferred: 'sonnet' }), 'sonnet');
assert.equal(chooseModel(claude, { toolKey: 'mln_1' }), 'haiku');
assert.equal(chooseModel([], { toolKey: 'mln_1', named: 'opus' }), '');
console.log('spawn model tests passed');
