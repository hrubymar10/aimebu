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
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { modelFamily, modelVersion, matchModel, chooseModel, toolKey, sessionEntryState, modelChangeWarning };', context);
const { modelFamily, modelVersion, matchModel, chooseModel, toolKey, modelChangeWarning } = context.api;
const sessionEntryState = (...args) => JSON.parse(JSON.stringify(context.api.sessionEntryState(...args))); // plain objects: a vm context has its own Object

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
// Versions are told apart: a session recorded on one version is not silently moved to another.
assert.equal(modelVersion('claude-opus-4-5-20251101'), '4.5');
assert.equal(modelVersion('opus5.5'), '5.5');
assert.equal(modelVersion('claude-3-5-sonnet-20241022'), '3.5');
assert.equal(modelVersion('claude-sonnet-4-20250514'), '4');
assert.equal(modelVersion('opus'), '');
assert.equal(modelVersion('gpt-6-sol'), '');
const versioned = [{ value: 'claude-opus-5-5', label: 'Opus 5.5' }, { value: 'claude-opus-4-5-20251101', label: 'Opus 4.5' }, { value: 'sonnet', label: 'Sonnet' }];
assert.equal(matchModel(versioned, 'claude-opus-4-5-20251101'), 'claude-opus-4-5-20251101', 'the exact id');
assert.equal(matchModel(versioned, 'claude-opus-4-5-20250101'), 'claude-opus-4-5-20251101', 'the same version, another date');
assert.equal(matchModel(versioned, 'opus4.5'), 'claude-opus-4-5-20251101', 'a v1 slug names its version');
assert.equal(matchModel([versioned[0]], 'claude-opus-4-5-20251101'), '', 'another version is never taken for it');
assert.equal(matchModel([versioned[0], { value: 'opus', label: 'Opus' }], 'claude-opus-4-5-20251101'), 'opus', 'an alias stands in when no entry has the version');
assert.equal(matchModel(versioned, 'opus'), 'claude-opus-5-5', 'a name with no version takes the first of its family');

// The session entry says up front when the launcher cannot continue a session, and offers the one that can.
const catalogOf = [
  { tool: 'claude', label: 'Claude Code (Docker)', containerized: true, available: true },
  { tool: 'codex', label: 'Codex', containerized: false, available: true },
  { tool: 'claude', label: 'Claude Code', containerized: false, available: true },
];
assert.deepEqual(sessionEntryState(catalogOf[2], catalogOf), { enabled: true, note: '', switchTo: -1 }, 'the native launcher can');
assert.deepEqual(sessionEntryState(catalogOf[1], catalogOf), { enabled: true, note: '', switchTo: -1 }, 'another tool is not judged here');
assert.deepEqual(sessionEntryState(null, catalogOf), { enabled: true, note: '', switchTo: -1 });
assert.deepEqual(sessionEntryState({ ...catalogOf[0], shares_claude_home: true }, catalogOf), { enabled: true, note: '', switchTo: -1 }, 'a Docker launcher that shares the home can');
const blocked = sessionEntryState(catalogOf[0], catalogOf);
assert.equal(blocked.enabled, false);
assert.equal(blocked.switchTo, 2, 'the native entry is offered');
assert.match(blocked.note, /Claude Code \(Docker\) keeps its own Claude home/);
const onlyDocker = sessionEntryState(catalogOf[0], [catalogOf[0], { ...catalogOf[2], available: false }]);
assert.equal(onlyDocker.switchTo, -1);
assert.match(onlyDocker.note, /claude_home_shared/, 'with nothing that can, it says what is needed');
const sharing = [catalogOf[0], { ...catalogOf[0], launcher_id: 'b', label: 'Shared', shares_claude_home: true }];
assert.equal(sessionEntryState(catalogOf[0], sharing).switchTo, 1, 'a Docker launcher that shares the home is offered too');

// A session continued on another model is warned about, versions included; the same id, or the same family and version, is not.
const opus = { value: 'opus', label: 'Opus' }, sonnet = { value: 'sonnet', label: 'Sonnet' };
assert.equal(modelChangeWarning('claude-opus-4-5-20251101', { value: 'claude-opus-4-5-20251101', label: 'Opus 4.5' }), '', 'the same id');
assert.equal(modelChangeWarning('claude-opus-4-5-20251101', { value: 'claude-opus-4-5-20250101', label: 'Opus 4.5 (older build)' }), '', 'the same version');
assert.equal(modelChangeWarning('opus5.5', { value: 'claude-opus-5-5', label: 'Opus 5.5' }), '', 'a v1 slug and the id of its version');
assert.equal(modelChangeWarning('claude-opus-4-5-20251101', { value: 'claude-opus-5-5', label: 'Opus 5.5' }), 'This session ran on claude-opus-4-5-20251101; the agent will continue it on Opus 5.5.', 'another version of the family');
assert.equal(modelChangeWarning('claude-opus-4-5-20251101', opus), 'This session ran on claude-opus-4-5-20251101; the agent will continue it on Opus, an alias that may be another version.', 'an alias may be another version');
assert.equal(modelChangeWarning('claude-opus-4-5-20251101', sonnet), 'This session ran on claude-opus-4-5-20251101; the agent will continue it on Sonnet.');
assert.equal(modelChangeWarning('', sonnet), '');
assert.equal(modelChangeWarning('claude-opus-4-5', null), '');
assert.match(modelChangeWarning('claude-opus-4-5', { value: 'gpt-6-sol', label: 'GPT 6 Sol' }), /continue it on GPT 6 Sol/);
console.log('spawn model tests passed');
