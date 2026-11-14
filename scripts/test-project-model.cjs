// The Project page's pure rules (web/js/pages/project/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'project', 'model.js'), 'utf8');
const context = vm.createContext({ Date, Number });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { money, scopeStatus, scopeButtons, scopeFailures, limitDraft, inheritText, limitSummary, limitRequest, detailsView, detailsChanges, lifecycleView, stopFailedNote };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(m.money(3), '$3.00');
assert.deepEqual([m.scopeStatus({ effective_state: 'paused' }, 'p'), m.scopeStatus({ effective_state: 'odd' }, 'p'), m.scopeStatus(null, 'p'), m.scopeStatus(null, null)], ['State: Paused', 'State: odd', 'Loading control state…', '']);
const buttons = (state, flags = {}) => plain(m.scopeButtons(state, { busy: false, retryResume: false, ...flags }));
assert.deepEqual(buttons({ desired: 'active' }), { pause: { disabled: false }, stop: { disabled: false }, resume: { disabled: true, label: 'Retry resume' } }, 'an active scope can be paused or stopped');
assert.deepEqual(buttons({ desired: 'paused' }), { pause: { disabled: true }, stop: { disabled: false }, resume: { disabled: false, label: 'Resume' } });
assert.deepEqual(buttons({ desired: 'stopped' }), { pause: { disabled: true }, stop: { disabled: true }, resume: { disabled: false, label: 'Resume' } });
assert.equal(buttons({ desired: 'active' }, { retryResume: true }).resume.disabled, false, 'a resume that left agents needing attention can be retried');
assert.ok(Object.values(buttons(null)).every(button => button.disabled) && Object.values(buttons({ desired: 'active' }, { busy: true })).every(button => button.disabled));
assert.deepEqual(plain(m.scopeFailures({ results: [{ error: 'busy' }, {}] }, 'resume')), { retryResume: true, text: 'Agent controls needing attention: busy. Retry Resume after the agent exits.' });
assert.deepEqual(plain(m.scopeFailures({ results: [{ error: 'busy' }] }, 'pause')), { retryResume: false, text: 'Agent controls needing attention: busy.' });
assert.deepEqual(plain(m.scopeFailures({}, 'resume')), { retryResume: false, text: '' });

assert.deepEqual(plain(m.limitDraft({ cost_mode: 'custom', daily_cost_cap: 5, turn_mode: 'inherit', daily_turn_cap: 9 })), { costMode: 'custom', cost: '5', turnMode: 'inherit', turn: '' });
assert.deepEqual([m.inheritText(null, 'cost'), m.inheritText(2.5, 'cost'), m.inheritText(40, 'turn'), m.inheritText(null, 'turn')], ['Inherit (default: no limit)', 'Inherit (default: $2.50)', 'Inherit (default: 40 turns)', 'Inherit (default: no limit)']);
assert.equal(m.limitSummary(null, 'p'), 'Loading today’s usage…');
assert.equal(m.limitSummary(null, null), '');
const state = { day_start: '2026-02-03T00:00:00Z', tz: 'UTC', daily_turns: 3, daily_turn_cap: 100, daily_cost: 1.5, daily_cost_cap: null, cost_coverage: 'partial', turns_with_estimated_cost: 1, effective_state: 'active' };
assert.match(m.limitSummary(state, 'p'), /\(UTC\), all rooms: 3 turns \/ 100 cap; \$1\.50 partly estimated cost \/ no cap\. Dispatch: active\.$/);
assert.match(m.limitSummary({ ...state, cost_coverage: 'known' }, 'p'), /\$1\.50 reported cost/);
assert.match(m.limitSummary({ ...state, cost_coverage: 'partial', turns_with_estimated_cost: 0 }, 'p'), /partly reported/);
assert.match(m.limitSummary({ ...state, cost_coverage: 'none' }, 'p'), /unreported/);
const draft = (patch = {}) => ({ costMode: 'inherit', cost: '', turnMode: 'inherit', turn: '', ...patch });
assert.deepEqual(plain(m.limitRequest(draft({ costMode: 'custom', cost: '2.5', turnMode: 'none' }), 4)), { body: { cost_mode: 'custom', daily_cost_cap: 2.5, turn_mode: 'none', daily_turn_cap: null, rev: 4 } });
for (const bad of [{ costMode: 'custom', cost: '' }, { costMode: 'custom', cost: '0' }, { costMode: 'custom', cost: 'x' }, { turnMode: 'custom', turn: '1.5' }, { turnMode: 'custom', turn: '-1' }, { turnMode: 'custom', turn: '  ' }]) {
  assert.deepEqual(plain(m.limitRequest(draft(bad), 1)), { problem: 'Enter a positive value for each custom limit, or choose Inherit or No limit.' }, JSON.stringify(bad));
}

assert.deepEqual(plain(m.detailsView({ role: 'owner' }, false)), { canName: true, canFolder: false, archived: false, status: 'Only an admin can change the folder.' });
assert.deepEqual(plain(m.detailsView({ role: 'viewer' }, true)), { canName: true, canFolder: true, archived: false, status: '' });
assert.deepEqual(plain(m.detailsView({ role: 'viewer', archived_at: 'x' }, false)), { canName: false, canFolder: false, archived: true, status: 'Only the project owner can change these.' });
assert.deepEqual(plain(m.detailsView(undefined, true)), { canName: false, canFolder: false, archived: false, status: '' });
const loaded = { name: 'A', folder: '/a' };
assert.deepEqual(plain(m.detailsChanges({ name: 'B', folder: '/b' }, loaded, { canName: true, canFolder: false })), { name: 'B' }, 'a field the person may not change is not sent');
assert.deepEqual(plain(m.detailsChanges({ name: 'A', folder: '/b' }, loaded, { canName: true, canFolder: true })), { folder: '/b' });
assert.deepEqual(plain(m.detailsChanges({ name: 'A', folder: '/a' }, loaded, { canName: true, canFolder: true })), {});

assert.deepEqual(plain(m.lifecycleView(null, true)), { show: false, archive: false, reopen: false, remove: false, importer: false, note: '' });
assert.deepEqual(plain(m.lifecycleView({}, true)), { show: true, archive: true, reopen: false, remove: true, importer: true, note: '' });
assert.equal(m.lifecycleView({}, false).note, 'Only the project owner or an admin can archive, reopen or delete a project.');
const archived = plain(m.lifecycleView({ archived_at: '2026-02-03T04:05:06Z' }, true));
assert.deepEqual([archived.archive, archived.reopen, archived.remove, archived.importer], [false, true, true, false], 'an archived project can be reopened or deleted, not imported into');
assert.match(archived.note, /^Archived .+ It is read-only\.$/);
assert.match(m.stopFailedNote([{ error: 'a' }, { error: 'b' }]), /^Archived\. 2 agents could not be stopped: a; b\. Choose Abandon/);
assert.match(m.stopFailedNote([{ error: 'a' }]), /^Archived\. 1 agent could not/);
console.log('project model tests passed');
