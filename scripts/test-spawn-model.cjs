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
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { writerHolder, writerHolderText, modelFamily, modelVersion, matchModel, chooseModel, toolKey, sessionEntryState, modelChangeWarning, NEW_ROOM, toolRows, resumeToolChoice, worktreeOption, hintParts, spawnReason, validName, startRoomChoices, startRoomLabel, resumeBanner, spawnRequest, switchRows, switchStart, switchModel, switchUnchanged, switchReason };', context);
const { modelFamily, modelVersion, matchModel, chooseModel, toolKey, modelChangeWarning } = context.api;
const plain = value => JSON.parse(JSON.stringify(value)); // plain objects: a vm context has its own Object
const { switchRows, switchStart, switchModel, switchUnchanged, switchReason } = context.api;
const { NEW_ROOM, toolRows, resumeToolChoice, worktreeOption, hintParts, spawnReason, validName, startRoomChoices, startRoomLabel, resumeBanner, spawnRequest } = context.api;
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
assert.deepEqual(sessionEntryState(catalogOf[1], catalogOf), { enabled: false, hidden: true, note: '', switchTo: -1 }, 'another tool has no sessions to continue, so the entry is hidden');
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

// The tool list: unavailable entries say why and where to look, once.
assert.deepEqual(plain(toolRows([{ tool: 'codex', label: 'Codex', available: true }, { tool: 'pi', available: false }, { label: 'Vibe', available: false, unavailable_reason: 'no models; see Settings → Harnesses' }, { label: 'Dock', available: false, unavailable_reason: 'not installed' }])),
  [{ value: '0', label: 'Codex', disabled: false }, { value: '1', label: 'pi — unavailable; check Settings → Harnesses', disabled: true }, { value: '2', label: 'Vibe — no models; see Settings → Harnesses', disabled: true }, { value: '3', label: 'Dock — not installed; check Settings → Harnesses', disabled: true }]);

// A session's start: a row the session cannot go through says why in its label (a container with a Claude home of its own, or another tool).
assert.deepEqual(plain(toolRows([{ tool: 'claude', label: 'Claude Code', available: true }, { tool: 'claude', label: 'Claude Code (Docker)', available: true, containerized: true }, { tool: 'codex', label: 'Codex', available: true }], [false, true, true])),
  [{ value: '0', label: 'Claude Code', disabled: false }, { value: '1', label: 'Claude Code (Docker) — keeps its own Claude home, so it cannot continue this session', disabled: false }, { value: '2', label: 'Codex — not the tool of this session', disabled: false }]);

// A start from v1 history picks the tool that can continue it and says so when none can.
const catalog = [{ tool: 'claude', containerized: true, available: true }, { tool: 'codex', available: true }, { tool: 'claude', available: true, launcher_id: 'l2' }];
assert.deepEqual(plain(resumeToolChoice(catalog, { tool: 'claude', resumable: true })), { index: 2, disabled: [true, true, false], note: '' });
assert.deepEqual(plain(resumeToolChoice(catalog, { tool: 'claude', resumable: true, launcher_id: 'l2' })).index, 2);
assert.deepEqual(plain(resumeToolChoice(catalog, { tool: 'codex', resumable: false })), { index: 1, disabled: null, note: '' });
assert.deepEqual(plain(resumeToolChoice(catalog.slice(0, 2), { tool: 'claude', resumable: true })), { index: -1, disabled: [true, true], note: 'claude is not available natively on the chosen machine, so the session cannot be continued here.' });
assert.equal(resumeToolChoice([{ tool: 'claude', containerized: true, available: true, shares_claude_home: true }], { tool: 'claude', resumable: true }).index, 0, 'a container that shares the host Claude home can continue a session');
assert.deepEqual(plain(resumeToolChoice(catalog, undefined)), { index: null, disabled: null, note: '' });
assert.equal(resumeToolChoice([{ tool: 'codex', available: true }], { tool: 'claude', resumable: false }).index, null, 'a fresh start leaves the choice alone');

// The worktree box: a task room only, refused with the machine's reason, not for a continued session.
assert.deepEqual(plain(worktreeOption({ roomKind: 'task', resumable: false })), { hidden: false, disabled: false, refused: false, note: '' });
assert.deepEqual(plain(worktreeOption({ roomKind: 'task', check: { ok: false, reason: 'not a repo' } })), { hidden: false, disabled: true, refused: true, note: 'No worktree here: not a repo' });
assert.equal(worktreeOption({ roomKind: 'task', check: { ok: false } }).note, 'No worktree here: the project folder cannot have one.');
assert.deepEqual([worktreeOption({ roomKind: 'home' }).hidden, worktreeOption({ roomKind: 'task', resumable: true }).hidden], [true, true]);

// A hint names Settings → Harnesses as a button for an admin, and gives anyone else plain words.
assert.deepEqual(plain(hintParts('No harness yet. Add a harness in Settings → Harnesses.', true)), { before: 'No harness yet. Add a harness in ', link: 'Settings → Harnesses', after: '.' });
assert.equal(hintParts('No harness yet. Add a harness in Settings → Harnesses.', false).text, 'No harness yet. Ask the owner to add a harness.');
assert.equal(hintParts('This harness has no models yet. Add one under Settings → Harnesses.', false).text, 'This harness has no models yet. Ask the owner to add one.');
assert.equal(hintParts('x; check Settings → Harnesses', false).text, 'x; ask the owner to check the harnesses');
assert.equal(hintParts('x. Check Settings → Harnesses.', false).text, 'x. Ask the owner to check the harnesses.');
assert.equal(hintParts('disabled; enable it in Settings → Harnesses', false).text, 'disabled; ask the owner to enable it');
assert.deepEqual(plain(hintParts('No models', true)), { text: 'No models' });

// What the line under the tool says.
const codex = { available: true, models: [] };
assert.equal(spawnReason({ needsUpdate: true, tool: null, catalogCount: 0, modelCount: 0 }), 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.');
assert.equal(spawnReason({ tool: null, catalogCount: 0 }), 'No harness yet. Add a harness in Settings → Harnesses.');
assert.equal(spawnReason({ tool: null, catalogCount: 2 }), 'No tool is available on a connected machine. Check Settings → Harnesses.');
assert.equal(spawnReason({ tool: { unavailable_reason: 'not installed' }, catalogCount: 1 }), 'not installed; check Settings → Harnesses');
assert.equal(spawnReason({ tool: { unavailable_reason: 'see Settings → Harnesses' }, catalogCount: 1 }), 'see Settings → Harnesses');
assert.equal(spawnReason({ tool: codex, modelCount: 0 }), 'This harness has no models yet. Add one under Settings → Harnesses.');
assert.equal(spawnReason({ tool: { available: true, toolbox: false, toolbox_reason: 'no channel' }, modelCount: 1 }), 'No agent toolbox: no channel.');
assert.equal(spawnReason({ tool: { available: true }, modelCount: 1 }), '');

assert.deepEqual(['nova', 'a-b_1', '', 'ab', 'Ab1', '1abc', 'abc-'].map(validName), [true, true, true, false, false, false, false]);

// The rooms a start from history can go to.
const rooms = [{ id: 'r1', title: 'one', kind: 'task', lifecycle: 'open', depth: 1 }, { id: 'r2', title: 'general', kind: 'task', lifecycle: 'open', depth: 0 }, { id: 'r3', title: 'old', kind: 'task', lifecycle: 'archived' }, { id: 'h', title: 'home', kind: 'home', lifecycle: 'open' }];
assert.deepEqual(plain(startRoomChoices(rooms, null, 'r1')), { options: [], value: '' });
assert.deepEqual(plain(startRoomChoices(rooms, { slug: 'old' }, 'r1')), { options: [{ value: 'r1', label: 'one' }, { value: 'r2', label: 'general' }], value: 'r2' });
// Named by where they sit: the project, the rooms they are nested under, then the room.
assert.deepEqual(plain(startRoomChoices(rooms, { slug: 'old' }, 'r1', 'Alpha')).options.map(option => option.label), ['Alpha › one', 'Alpha › general']);
{
  const tree = [{ id: 'h', kind: 'home', title: 'Alpha' }, { id: 'a', kind: 'task', title: 'Auth', parent_id: 'h', lifecycle: 'open' }, { id: 'b', kind: 'task', title: 'Login', parent_id: 'a', lifecycle: 'open' }, { id: 'x', kind: 'task', title: 'Loop', parent_id: 'y', lifecycle: 'open' }, { id: 'y', kind: 'task', title: 'Other', parent_id: 'x', lifecycle: 'open' }];
  assert.deepEqual([startRoomLabel(tree[1], tree, 'Alpha'), startRoomLabel(tree[2], tree, 'Alpha'), startRoomLabel(tree[2], tree, ''), startRoomLabel(tree[3], tree, 'Alpha')], ['Alpha › Auth', 'Alpha › Auth › Login', 'Auth › Login', 'Alpha › Other › Loop'], 'a nested room names the rooms above it, not the home room, and a parent loop ends');
}
assert.equal(startRoomChoices(rooms.filter(room => room.id !== 'r2'), { slug: 'old' }, 'r1').value, 'r1', 'without general the open room is chosen when it is a task room');
assert.equal(startRoomChoices(rooms.filter(room => room.id !== 'r2'), { slug: 'old' }, 'h').value, 'r1');
assert.deepEqual(plain(startRoomChoices(rooms, { kind: 'claude_session' }, 'r1')).options[0], { value: NEW_ROOM, label: 'A new room for this session' });
assert.equal(startRoomChoices(rooms, { kind: 'claude_session' }, 'r1').value, NEW_ROOM);
assert.deepEqual(plain(startRoomChoices([], { slug: 'old' }, 'r1')), { options: [], value: '' });

// The banner of a start from history.
const relative = value => `rel(${value})`;
const banner = (resume, patch = {}) => plain(resumeBanner({ resume, mode: 'continue', hasRoom: true, toolNote: '', warning: '', relative, ...patch }));
assert.deepEqual([banner(null).hidden, banner(null).title, banner(null).confirmText], [true, 'Spawn', ' v1 is not running this session any more']);
const v1 = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', resumable: true, cwd: '/work/old', v1_active: true, outside_folder: true };
assert.deepEqual([banner(v1).title, banner(v1).text, banner(v1).choosing, banner(v1).confirmHidden], ['Start old-bot as a v2 agent', 'This continues the v1 session of old-bot, working in /work/old.', true, false]);
assert.equal(banner(v1).notes, 'It works in /work/old, outside the project folder, so the owner must approve it in the Inbox before it starts. v1 saw this agent a moment ago and may still be using the session. Stop it in v1 first: two processes on one session corrupt it.');
const fork = banner(v1, { mode: 'fork', warning: 'W' });
assert.deepEqual([fork.title, fork.confirmHidden, fork.forking, fork.notes.endsWith('W')], ['Start old-bot on a copy of its v1 session', true, true, true]);
assert.equal(banner({ slug: 'x', reason: 'No session was kept.' }).text, 'No session was kept.');
assert.equal(banner({ slug: 'x' }).text, 'This agent starts fresh with the same name.');
assert.equal(banner(v1, { hasRoom: false }).notes.startsWith('This project has no open task room to start it in.'), true);
const session = { kind: 'claude_session', resumable: true, session_id: '11111111-aaaa', cwd: '/w', modified_at: 'T', model: 'opus', active: true, held_by: 'ada', outside_folder: false };
const cont = banner(session, { toolNote: 'N', warning: 'W' });
assert.deepEqual([cont.title, cont.held, cont.card, cont.confirmText, cont.confirmHidden], ['Continue a Claude Code session', true, [['Session', '11111111', true], ['Folder', '/w', true], ['Last active', 'rel(T)', false], ['Ran on', 'opus', true]], ' I closed this session in my terminal', false]);
assert.equal(cont.notes, 'ada already continues this session, so it can only be copied. A Claude process has this session open. Close it first: two processes on one session corrupt it. N W');
assert.deepEqual([banner(session, { mode: 'fork' }).title, banner(session, { mode: 'fork' }).confirmHidden], ['Work on a copy of a Claude Code session', true]);
assert.equal(banner({ ...session, modified_at: '' }).card[2][1], 'unknown');

// What a Spawn sends, or why it does not.
const ready = { role: 'worker', tool: { available: true, tool: 'codex', variant: 'native', launcher_id: 'l1', provider: 'p' }, model: 'm', typed: ' nova ', machineID: 'mch', mode: 'continue', target: { kind: 'task' }, worktree: true, baseRef: 'main' };
assert.deepEqual(plain(spawnRequest(ready)), { params: { role: 'worker', tool: 'codex', variant: 'native', launcher_id: 'l1', model: 'm', provider: 'p', machine_id: 'mch', name: 'nova', worktree: true, base_ref: 'main' } });
assert.deepEqual(plain(spawnRequest({ ...ready, typed: '', target: { kind: 'home' } })).params.worktree, false);
// Working on a copy in a worktree of its own: a fork, and the flag; it means nothing for a fresh agent.
const sessionResume = { kind: 'claude_session', session_id: 'sid', resumable: true, tool: 'codex' };
assert.deepEqual(plain(spawnRequest({ ...ready, resume: sessionResume, mode: 'continue', workOnCopy: true })).params, { role: 'worker', tool: 'codex', variant: 'native', launcher_id: 'l1', model: 'm', provider: 'p', machine_id: 'mch', name: 'nova', resume_session: 'sid', resume_mode: 'fork', confirm_session_closed: false, new_room: false, work_on_copy: true });
assert.equal('work_on_copy' in plain(spawnRequest({ ...ready, workOnCopy: true })).params, false);
assert.equal('name' in spawnRequest({ ...ready, typed: '  ' }).params, false);
assert.deepEqual([{ ...ready, role: '' }, { ...ready, model: '' }, { ...ready, tool: { available: false } }, { ...ready, tool: null }].map(item => spawnRequest(item).skip), [true, true, true, true]);
assert.match(spawnRequest({ ...ready, typed: 'A!' }).nameError, /^A name is 3 to 21 characters/);
assert.equal(spawnRequest({ ...ready, resume: { ...v1, tool: 'claude' } }).error, 'To continue this session the tool must be claude.');
assert.equal(spawnRequest({ ...ready, resume: { ...v1, tool: 'codex' } }).confirm, 'Confirm that v1 is not running this session any more.');
const confirmed = plain(spawnRequest({ ...ready, resume: { ...v1, tool: 'codex' }, confirmed: true })).params;
assert.deepEqual([confirmed.resume_legacy, confirmed.resume_mode, confirmed.confirm_v1_idle, 'worktree' in confirmed, 'name' in confirmed], ['old-bot', 'continue', true, false, false]);
const forked = plain(spawnRequest({ ...ready, resume: { ...v1, tool: 'codex' }, mode: 'fork' })).params;
assert.deepEqual([forked.resume_mode, forked.confirm_v1_idle], ['fork', false]);
const notResumable = plain(spawnRequest({ ...ready, resume: { slug: 'old', legacy_id: 'old', resumable: false } })).params;
assert.deepEqual([notResumable.resume_legacy, 'resume_mode' in notResumable, notResumable.worktree, 'name' in notResumable], ['old', false, true, false]);
assert.equal(spawnRequest({ ...ready, resume: { ...session, tool: 'claude' }, tool: { available: true, tool: 'claude' } }).confirm, 'Confirm that you closed this session.');
const sent = plain(spawnRequest({ ...ready, tool: { available: true, tool: 'claude' }, resume: { ...session, tool: 'claude', active: false }, newRoom: true, typed: 'cc1' })).params;
assert.deepEqual([sent.resume_session, sent.resume_mode, sent.confirm_session_closed, sent.new_room, sent.name, 'worktree' in sent], ['11111111-aaaa', 'continue', false, true, 'cc1', false]);
assert.equal(spawnRequest({ ...ready, tool: { available: true, tool: 'claude' }, resume: { ...session, tool: 'claude' }, mode: 'fork', typed: 'cc1' }).params.confirm_session_closed, false);


// The Switch dialog: the rows, the tool it starts on, the model a tool opens on, when a pick is no change, and what the line under the tool says.
assert.deepEqual(plain(switchRows([{ tool: 'codex', label: 'Codex', available: true }, { tool: 'pi', available: false }, { label: 'Vibe', available: false, unavailable_reason: 'no models' }])),
  [{ value: '0', label: 'Codex', disabled: false }, { value: '1', label: 'pi — unavailable', disabled: true }, { value: '2', label: 'Vibe — no models', disabled: true }]);
const own = [{ tool: 'claude', variant: 'native', available: true, models: [{ id: 'opus' }, { id: 'sonnet', default: true }] }, { tool: 'codex', variant: 'native', available: true, models: [{ id: 'gpt-6-sol' }, { id: 'gpt-6-mini', default: true }] }, { tool: 'pi', variant: 'native', available: false, models: [] }];
assert.equal(switchStart(own, { tool: 'codex', variant: 'native' }), 1, 'the agent\'s own tool');
assert.equal(switchStart(own, { tool: 'pi', variant: 'native' }), 0, 'an unavailable tool falls back to the first available');
assert.equal(switchStart([own[2]], { tool: 'x' }), -1);
assert.equal(switchModel(own[1], { tool: 'codex', variant: 'native', model: 'gpt-6-sol' }), 'gpt-6-sol', 'the agent keeps its own model on its own tool');
assert.equal(switchModel(own[1], { tool: 'codex', variant: 'native', model: 'gone' }), 'gpt-6-mini', 'else the harness default');
assert.equal(switchModel(own[0], { tool: 'codex', variant: 'native', model: 'gpt-6-sol' }), 'sonnet', 'another tool opens on its default');
assert.equal(switchModel({ tool: 'x', models: [{ id: 'a' }, { id: 'b' }] }, {}), 'a', 'with no default, the first');
assert.equal(switchModel(null, {}), '');
const agent = { tool: 'codex', variant: 'native', model: 'gpt-6-sol', machine_id: 'm1' };
assert.deepEqual([switchUnchanged({ tool: own[1], model: 'gpt-6-sol', agent, machineID: 'm1' }), switchUnchanged({ tool: own[1], model: 'gpt-6-mini', agent, machineID: 'm1' }), switchUnchanged({ tool: own[1], model: 'gpt-6-sol', agent, machineID: 'm2' }), switchUnchanged({ tool: own[0], model: 'gpt-6-sol', agent, machineID: 'm1' }), switchUnchanged({ tool: own[1], model: 'gpt-6-sol', agent: { ...agent, machine_id: '' }, machineID: 'm2' })], [true, false, false, false, true]);
assert.equal(switchReason({ needsUpdate: true, tool: null }), 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.');
assert.equal(switchReason({ tool: null }), 'No harness is available on a connected machine. Add one in Settings → Harnesses.');
assert.equal(switchReason({ tool: { available: true }, modelCount: 0 }), 'This harness has no models yet. Add one under Settings → Harnesses.');
assert.equal(switchReason({ tool: { available: true }, modelCount: 1, unchanged: true }), 'Choose a different model or tool.');
assert.equal(switchReason({ tool: { available: true, toolbox: false, toolbox_reason: 'no channel' }, modelCount: 1 }), 'No agent toolbox: no channel.');
assert.equal(switchReason({ tool: { available: true }, modelCount: 1 }), '');

// A writer role is held back by another live writer of the room on the machine, unless the room gets a worktree of its own.
const { writerHolder, writerHolderText } = context.api;
const agents = [{ slug: 'alice', role_key: 'worker', desired: 'active', machine_id: 'm1' }, { slug: 'bob', role_key: 'reviewer', desired: 'active', machine_id: 'm1' }];
const writerRoles = new Set(['worker']);
const holder = extra => plain(writerHolder({ writer: true, agents, writerRoles, machineID: 'm1', worktree: false, worktreeRefused: true, ...extra }));
assert.equal(holder().slug, 'alice');
assert.equal(writerHolder({ writer: false, agents, writerRoles, machineID: 'm1', worktree: false }), null, 'a non-writer role is never held back');
assert.equal(holder({ worktree: true }), null, 'a worktree is a checkout of its own');
assert.equal(holder({ worktreeRefused: false }), null, 'with a worktree available the newcomer gets one: nothing blocks, so nothing is said');
assert.equal(holder({ machineID: 'm2' }), null, 'another machine is another checkout');
assert.equal(holder({ agents: [{ ...agents[0], machine_id: '' }] }), null, 'an agent whose machine is unknown is not counted');
assert.equal(holder({ machineID: '' }), null, 'nor is anyone before a machine is chosen');
assert.equal(holder({ agents: [{ ...agents[0], desired: 'stopped' }] }), null, 'a stopped writer holds nothing');
assert.equal(holder({ agents: [agents[1]] }), null, 'a reviewer is not a writer');
assert.equal(writerHolderText(agents[0], 'No worktree here: the project folder is not a Git repository'), 'alice is already working in this folder, and a separate copy of it cannot be made (the project folder is not a Git repository), so a new writer here will be refused until alice is stopped.');
assert.equal(writerHolderText(agents[0]), 'alice is already working in this folder, and a separate copy of it cannot be made, so a new writer here will be refused until alice is stopped.');
console.log('spawn model tests passed');
