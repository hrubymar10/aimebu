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
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { PROVIDERS_MAX, emptyProvider, editableProvider, addProvider, removeProvider, updateProvider, providersError, providersBody, providerInUse, removeWithModelsLabel, inUseMessage, keyState, suggestionState, addSuggestions, MODELS_MAX, familyName, harnessLabel, addFoundModels, detectionFailure, lines, pairs, harnessFromLauncher, harnessStatus, harnessTitle, persistedDefinition, formFromHarness, launcherBody, enabledBody, createBody, addModel, removeModel, setDefaultModel, updateModel, modelsError, initialModels, detectNotes, machineSummary, FAMILIES, tabTitle, staleModelsMessage, chosenHarness, tabKeyTarget, defaultLabel };', context);
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

// Mistral Vibe is a family like the others, and a read keeps the revision of the stored models so a save can name it.
assert.deepEqual(plain(m.FAMILIES).map(([key]) => key), ['claude', 'codex', 'pi', 'opencode', 'vibe']);
assert.equal(m.familyName('vibe'), 'Mistral Vibe');
assert.equal(m.harnessFromLauncher({ ...row, models_rev: 'r7' }).models_rev, 'r7');
assert.equal(m.harnessFromLauncher(row).models_rev, '', 'a row without a revision reads as an empty one');
assert.ok(!('models_rev' in plain(m.persistedDefinition(m.harnessFromLauncher({ ...row, models_rev: 'r7' })))), 'the revision is never sent back with the harness definition');

// The tab of a harness in the list beside the detail.
assert.equal(m.tabTitle({ ...harness, label: 'Docker Codex' }), 'Docker Codex: Available, 1 models');
assert.equal(m.tabTitle({ ...harness, enabled: false, models: [] }), 'harness-docker: Disabled, 0 models');
assert.equal(m.chosenHarness([{ id: 'a' }, { id: 'b' }], 'b'), 'b', 'the chosen harness stays chosen');
assert.equal(m.chosenHarness([{ id: 'a' }, { id: 'b' }], 'gone'), 'a', 'a harness that is gone falls back to the first');
assert.equal(m.chosenHarness([], 'a'), '', 'no harness, nothing chosen');
assert.deepEqual([['ArrowDown', 0], ['ArrowDown', 2], ['ArrowUp', 0], ['ArrowUp', 2], ['Home', 1], ['End', 1], ['x', 1]].map(([key, at]) => m.tabKeyTarget(key, at, 3)), [1, 0, 2, 1, 0, 2, -1], 'the arrow keys wrap; Home and End jump; other keys do nothing');

// A role default is named by its machine, harness and model, or by its ids while the harness is unknown.
const item = { launcher_id: 'mln_x', model: 'm' };
assert.equal(m.defaultLabel(item, null, undefined), 'mln_x · m');
assert.equal(m.defaultLabel(item, { harness: { label: 'Codex', models: [{ slug: 'm', label: 'Model M' }] }, machine: 'Host' }, undefined), 'Host · Codex · Model M');
assert.equal(m.defaultLabel(item, { harness: { name: 'native', models: [] }, machine: 'Host' }, undefined), 'Host · native · m', 'a model that is not listed shows its id');
assert.equal(m.defaultLabel(item, null, { machine: 'Other', label: 'Pi', models: [{ slug: 'm', label: 'Local' }] }), 'Other · Pi · Local', 'another machine\'s harness is named by its own machine');
assert.equal(m.staleModelsMessage({ label: 'Claude Code', name: 'native' }), 'Someone changed the models of Claude Code in the meantime. The list was reloaded; make your changes again.');
assert.equal(m.staleModelsMessage({ name: 'native' }), 'Someone changed the models of native in the meantime. The list was reloaded; make your changes again.');


// Custom providers: a stored provider is edited with its limits as text, and goes out with the limits only as a pair and the key variable's NAME only.
{
  const stored = { id: 'acme', name: 'Acme', npm: '@ai-sdk/openai-compatible', base_url: 'https://acme.example/v1', key_env: 'ACME_KEY', context_tokens: 128000, output_tokens: 8000 };
  const editable = m.editableProvider(stored);
  assert.deepEqual(plain([editable.context_tokens, editable.output_tokens]), ['128000', '8000']);
  assert.deepEqual(plain(m.providersBody([editable], 'p1')), { providers: [stored], expected_rev: 'p1' });
  assert.deepEqual(plain(m.providersBody([m.editableProvider({ ...stored, key_env: '', context_tokens: 0, output_tokens: 0, name: '' })], 'p1', true)), { providers: [{ id: 'acme', name: 'acme', npm: '@ai-sdk/openai-compatible', base_url: 'https://acme.example/v1' }], expected_rev: 'p1', remove_models: true }, 'no key variable, no limits, a name defaulting to the id, and remove_models only when asked');
  const bad = change => m.providersError([{ ...editable, ...change }]);
  assert.equal(bad({}), '');
  assert.match(bad({ id: 'Bad Id' }), /lowercase letters/); assert.match(bad({ base_url: ' ' }), /endpoint URL/); assert.match(bad({ key_env: 'lower' }), /upper case/);
  assert.match(bad({ output_tokens: '' }), /both token limits or neither/); assert.match(bad({ context_tokens: '0', output_tokens: '5' }), /whole numbers above zero/);
  assert.equal(bad({ context_tokens: '', output_tokens: '', key_env: '' }), '', 'limits and the key are optional');
  assert.match(m.providersError([editable, editable]), /Two providers are called acme/);
  assert.equal(m.addProvider(Array.from({ length: m.PROVIDERS_MAX }, () => editable), undefined).length, m.PROVIDERS_MAX, 'the list stops at its limit');
  assert.deepEqual(plain(m.removeProvider([1, 2, 3], 1)), [1, 3]);
  assert.equal(m.harnessFromLauncher({ config: { id: 'x', name: 'n' }, providers: [stored], providers_rev: 'p9' }).providers_rev, 'p9');
  assert.equal('providers' in m.persistedDefinition({ id: 'x', providers: [stored], providers_rev: 'p9' }), false, 'a launcher save never carries the providers');
  assert.deepEqual(plain(m.providerInUse({ code: 'provider_in_use', body: { provider_id: 'acme', model_count: 2 } })), { providers: [{ id: 'acme', count: 2 }], count: 2 }, 'an older answer names one provider');
  const two = m.providerInUse({ code: 'provider_in_use', body: { provider_id: 'acme', model_count: 2, in_use: [{ provider_id: 'acme', model_count: 2 }, { provider_id: 'lab', model_count: 1 }] } });
  assert.deepEqual(plain(two), { providers: [{ id: 'acme', count: 2 }, { id: 'lab', count: 1 }], count: 3 }, 'every provider that would lose models is listed');
  assert.equal(m.providerInUse({ code: 'other' }), null);
  assert.deepEqual([m.removeWithModelsLabel(m.providerInUse({ code: 'provider_in_use', body: { provider_id: 'acme', model_count: 1 } })), m.removeWithModelsLabel(two)], ['Remove acme with its 1 model', 'Remove acme, lab with their 3 models']);
  assert.equal(m.inUseMessage(two), 'acme (2), lab (1) still have models listed.');
  // The key's state: a word from the saved variable and the answer, never a value.
  const key = (provider, saved, status) => m.keyState(provider, saved, status).text;
  assert.equal(key({ ...editable, key_env: '' }, stored, null), 'No key variable');
  assert.equal(key(editable, { key_env: 'OTHER' }, null), 'Save to check the key');
  assert.equal(key(editable, stored, null), 'Key not checked');
  assert.equal(key(editable, stored, { machine_online: false }), 'Machine offline: key not checked');
  assert.equal(key(editable, stored, { machine_online: true, supervisor_supported: false }), 'Supervisor update needed to check the key');
  assert.equal(key(editable, stored, { machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', set: true }] }), 'ACME_KEY is set');
  assert.equal(key(editable, stored, { machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', set: false }] }), 'ACME_KEY is not set on that machine');
  assert.equal(key(editable, stored, { machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', set: null }], error: 'no answer' }), 'Key not checked: no answer');
  // Import suggestions: one already there or with a problem is not taken; the others join the unsaved list, an inline key says what to do.
  const suggestion = (id, extra = {}) => ({ provider: { id, name: id, npm: '@ai-sdk/openai-compatible', base_url: `https://${id}.example/v1`, key_env: '' }, models: [], ...extra });
  const picked = [suggestion('acme'), suggestion('lab', { inline_key: true }), suggestion('bad', { problem: 'nope' }), suggestion('team', { already_added: true }), suggestion('fresh')];
  assert.deepEqual(['acme', 'lab', 'bad', 'team'].map(id => m.suggestionState(picked.find(item => item.provider.id === id), [editable])), ['already', 'ok', 'problem', 'already']);
  const result = m.addSuggestions([editable], picked);
  assert.deepEqual(plain(result.providers.map(item => item.id)), ['acme', 'lab', 'fresh']);
  assert.equal(result.taken, 2); assert.match(result.status, /2 added to the list; its key was written into the OpenCode config/);
  assert.equal(m.addSuggestions([editable], [suggestion('acme')]).status, 'Nothing new to add.');
  // Only what was added counts: an inline-key suggestion that was not taken (the list is full, or its id is there) does not.
  const full = Array.from({ length: m.PROVIDERS_MAX - 1 }, (_, index) => m.editableProvider({ ...stored, id: `p${index}` }));
  const capped = m.addSuggestions(full, [suggestion('one'), suggestion('two', { inline_key: true })]);
  assert.deepEqual([capped.taken, /key was written/.test(capped.status)], [1, false], 'the inline key of a suggestion that did not fit is not mentioned');
}
console.log('harness model tests passed');
