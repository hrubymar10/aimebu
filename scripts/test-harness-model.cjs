// The Harnesses page's pure rules (web/js/pages/harnesses/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'harnesses', 'model.js'), 'utf8');
assert.ok(!/^import /m.test(source), 'model.js stays import-free so it can run here');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { MODELS_MAX, familyName, harnessLabel, addFoundModels, detectionFailure, lines, pairs, harnessFromLauncher, harnessStatus, harnessTitle, persistedDefinition, formFromHarness, launcherBody, enabledBody, createBody, addModel, removeModel, setDefaultModel, updateModel, modelsError, initialModels, detectNotes, machineSummary };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

// A launcher row reads as the saved definition plus what the supervisor observed.
const row = { config: { id: 'mln_1', tool: 'codex', name: 'harness-docker', label: '', command: 'docker', args: ['--a'], env_refs: { T: 'S' }, enabled: true, containerized: true, agent_server_url: '', path_maps: [{ host: '/h', visible: '/v' }], visible_roots: ['/r'], supports_toolbox: true }, resolved_command: '/usr/bin/docker', version: '29.1', available: true, active_agents: 2, models: [{ slug: 'm', label: 'M', note: '', default: true }] };
const harness = m.harnessFromLauncher(row);
assert.deepEqual([harness.variant, harness.launcher_path, harness.version, harness.active_agents, harness.models.length], ['harness-docker', '/usr/bin/docker', '29.1', 2, 1]);
assert.equal(m.harnessFromLauncher({ config: { id: 'x', name: 'n' } }).active_agents, 0, 'missing counts are zero');
assert.deepEqual([m.harnessStatus(harness), m.harnessStatus({ ...harness, available: false }), m.harnessStatus({ ...harness, enabled: false })], ['Available', 'Unavailable', 'Disabled']);
assert.equal(m.harnessTitle(harness), 'Codex · harness-docker', 'an unlabelled harness is named by its family and variant');
assert.equal(m.familyName('mystery'), 'mystery');

// Saves send only the saved definition, never what was observed or the models.
const definition = plain(m.persistedDefinition(harness));
for (const key of ['variant', 'launcher_path', 'version', 'available', 'unavailable_reason', 'active_agents', 'models']) assert.ok(!(key in definition), `${key} is not sent back`);
assert.deepEqual(plain(m.enabledBody(harness, false)), { ...definition, enabled: false }, 'the Enabled switch sends the saved definition with the new value only');
const form = m.formFromHarness(harness);
assert.deepEqual(plain(form), { label: '', command: 'docker', args: '--a', env_refs: 'T=S', containerized: true, agent_server_url: '', path_maps: '/h → /v', visible_roots: '/r', supports_toolbox: true });
const edited = { ...form, label: ' Box ', command: ' /opt/docker ', args: 'one\n\n two ', env_refs: 'A=B\nC = D', path_maps: '/a → /b', visible_roots: '' };
assert.deepEqual(plain(m.launcherBody(harness, edited, false)), { ...definition, label: 'Box', enabled: false, command: '/opt/docker', args: ['one', 'two'], env_refs: { A: 'B', C: 'D' }, containerized: true, agent_server_url: '', path_maps: [{ host: '/a', visible: '/b' }], visible_roots: [], supports_toolbox: true });
assert.throws(() => m.launcherBody(harness, { ...form, env_refs: 'nope' }, true), /Use one = pair per line/);
assert.throws(() => m.pairs('=x', '='), /pair per line/, 'a pair needs both sides');
assert.deepEqual(plain(m.createBody({ tool: 'pi', label: 'Pi', command: 'pi', containerized: true })), { tool: 'pi', label: 'Pi', command: 'pi', args: [], env_refs: {}, enabled: true, containerized: true, path_maps: [], visible_roots: [], supports_toolbox: false }, 'a containerized Pi has no agent toolbox');
assert.equal(m.createBody({ tool: 'pi', containerized: false }).supports_toolbox, true);

// The model list: exact ids, the first is the default, duplicates are ignored, OpenCode ids are provider/model.
let models = m.addModel([], 'codex', ' gpt-a ', '', ' note ');
assert.deepEqual(plain(models), [{ slug: 'gpt-a', label: 'gpt-a', note: 'note', default: true }]);
models = m.addModel(models, 'codex', 'gpt-b', 'B');
assert.equal(m.addModel(models, 'codex', 'gpt-b'), models, 'a listed slug leaves the list as it is');
assert.throws(() => m.addModel(models, 'codex', 'bad id'), /"bad id" is not a valid model id/);
assert.throws(() => m.addModel([], 'opencode', 'model'), /provider\/model/);
assert.equal(m.addModel([], 'opencode', 'p/model').length, 1);
assert.deepEqual(plain(m.setDefaultModel(models, 'gpt-b')).map(model => model.default), [false, true]);
assert.deepEqual(plain(m.removeModel(models, 'gpt-a')), [{ slug: 'gpt-b', label: 'B', note: '', default: true }], 'removing the default makes the first one left the default');
assert.deepEqual(plain(m.removeModel(m.setDefaultModel(models, 'gpt-b'), 'gpt-a')).map(model => model.default), [true]);
assert.equal(m.updateModel(models, 'gpt-b', { note: 'n' })[1].note, 'n');
assert.equal(models[1].note, '', 'updates return a new list and leave the old one alone');
assert.equal(m.modelsError([]), '', 'an empty list is allowed');
assert.equal(m.modelsError(m.setDefaultModel(models, 'none')), 'Choose exactly one default model.');
assert.equal(m.modelsError(models), '');

// Auto-fill after adding from auto-detect: at most MODELS_MAX, already listed ones skipped, the first is the default.
const candidates = Array.from({ length: 66 }, (_, index) => ({ slug: `pi-${index}`, label: index === 1 ? '' : `Pi ${index}`, already_listed: index === 0 }));
const filled = m.initialModels(candidates);
assert.deepEqual([filled.models.length, filled.cut, filled.models[0].slug, filled.models[0].default, filled.models[1].default, filled.models[0].label], [m.MODELS_MAX, 1, 'pi-1', true, false, 'pi-1']);
assert.deepEqual(plain(m.initialModels(undefined)), { models: [], cut: 0 });
assert.equal(m.detectNotes([{ saved: true, cut: 0 }]), '');
assert.equal(m.detectNotes([{ saved: true, cut: 2, kept: 64 }, { saved: false, cut: 0, reason: 'It failed.' }]), 'A harness has no models yet. It failed. Open Models to retry Auto-detect, in a folder you choose if the harness needs one, or add a model by hand. A harness offered more models than a list can hold, so only the first 64 were added.');
assert.ok(m.detectNotes([{ saved: false }, { saved: false }]).startsWith('2 harnesses have no models yet. Open Models'));

assert.equal(m.machineSummary({ approved: false }), 'Approval pending');
assert.equal(m.machineSummary({ approved: true }), 'Never connected');
assert.ok(m.machineSummary({ last_seen: '2026-10-21T10:00:00Z' }).startsWith('Last seen '));
// The label a card names a harness by, and the capped list from detected models.
assert.equal(m.harnessLabel({ tool: 'codex', containerized: true }), 'Codex (Docker)');
assert.equal(m.harnessLabel({ tool: 'pi', label: 'Mine' }), 'Mine');
const nearlyFull = Array.from({ length: m.MODELS_MAX - 1 }, (_, index) => ({ slug: `m-${index}`, label: '', note: '', default: index === 0 }));
const fill = m.addFoundModels(nearlyFull, 'codex', [{ slug: 'x-1', label: 'X1' }, { slug: 'x-2', label: 'X2' }]);
assert.deepEqual([fill.models.length, fill.added, fill.status], [m.MODELS_MAX, 1, `1 added to the list (limit ${m.MODELS_MAX}). Save models to keep them.`]);
assert.equal(m.addFoundModels(fill.models, 'codex', [{ slug: 'x-3' }]).status, `The list is full (limit ${m.MODELS_MAX}).`);
assert.equal(m.addFoundModels([], 'codex', []).status, 'Nothing new to add.');

const notVisible = { code: 'detect_failed', detectDir: '/home/x/.aimebu2/detect', message: "Couldn't detect the models: the folder does not exist or is not visible in the harness container." };
assert.deepEqual(plain(m.detectionFailure(notVisible, true)), { summary: 'Detection blocked — please mount /home/x/.aimebu2/detect to your harness Docker, or pick a mounted folder below.', detail: notVisible.message });
assert.deepEqual(plain(m.detectionFailure({ code: 'detect_failed', message: "Couldn't detect the models: pi reports no models — is it logged in? Run /login in it" }, true)), { summary: 'pi reports no models — is it logged in? Run /login in it', detail: "Couldn't detect the models: pi reports no models — is it logged in? Run /login in it" });
assert.deepEqual(plain(m.detectionFailure({ code: 'detect_failed', message: 'timed out' }, true)), { summary: 'timed out', detail: '' });
assert.deepEqual(plain(m.detectionFailure({ code: 'detect_failed', message: 'exit status 1' }, false)), { summary: 'Model detection failed. Check the harness or try another folder.', detail: 'exit status 1' });
assert.deepEqual(plain(m.detectionFailure({}, false)), { summary: "Couldn't detect the models.", detail: '' });
console.log('harness model tests passed');
