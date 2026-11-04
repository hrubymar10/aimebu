// The Inbox's pure rules (web/js/pages/inbox/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'inbox', 'model.js'), 'utf8');
const context = vm.createContext({ JSON, Array, Object, Map, String, Number });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { humanize, readablePreview, readableReport, deliveryEvidence, resolvedItems, bulkLabel, countView, groupItems, groupTitle, cardKey, decisionOptions, retryWarning, evidenceText, boundaryText, recoveryResult };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(m.humanize('plain text'), 'plain text');
assert.deepEqual([m.humanize(null), m.humanize(5)], ['', '5']);
assert.equal(m.humanize('{"messages":[{"body":"First"},{"text":"Second"}]}'), 'First\n\nSecond');
assert.equal(m.humanize('[{"content":"Only"}]'), 'Only');
assert.equal(m.humanize('{"state":"held","count":2,"ok":true}'), 'state: held · count: 2 · ok: yes');
assert.equal(m.humanize('{"messages":[{"body":"A long draft that was cut off by a'), 'A long draft that was cut off by a', 'a draft cut off by the server keeps its text');
assert.equal(m.humanize('{"messages":[{"nothing":'), 'The details are incomplete. Open the debug inspector for the raw record.');
assert.equal(m.humanize('{}'), 'No readable details. Open the debug inspector for the raw record.');
assert.equal(m.readablePreview(''), '');
assert.equal(m.readablePreview('{"messages":[{"body":"Hi"}]}'), 'Hi');
assert.equal(m.readablePreview('{"messages":[{}]}'), 'The reply draft contains no readable text. View the room for context.');
assert.equal(m.readablePreview('{"body":"cut'), 'cut', 'a draft cut off by the server keeps its text');
assert.equal(m.readablePreview('{"messages":'), 'The reply draft is incomplete. View the room for context.');
assert.equal(m.readableReport({ status: 'clean', details: { a_b: true, list: [1, 2] } }), 'status: clean\na b: Yes\nlist: 1\nlist: 2');
assert.equal(m.readableReport('not json'), 'The diagnostic details could not be displayed.');
assert.equal(m.readableReport({}), 'No diagnostic details available.');
assert.equal(m.deliveryEvidence({ held_reason: 'room_closed' }), 'The room was archived before this reply could be posted.');
assert.equal(m.deliveryEvidence({ held_reason: 'some_reason', delivery_error: '{"x":1}' }), 'some reason The reply did not pass validation.');
assert.equal(m.deliveryEvidence({ held_reason: 'other', delivery_error: 'Boom' }), 'other Boom');
assert.equal(m.deliveryEvidence({}), 'The reply was held; view the room for context.');

const item = (patch = {}) => ({ id: 'i', kind: 'question', project_id: 'p', room_id: 'r', rev: 1, ...patch });
assert.deepEqual(plain(m.resolvedItems([item({ kind: 'delivery_failed', can_decide: true, room_lifecycle: 'closed' }), item({ kind: 'delivery_failed', can_decide: true, room_lifecycle: 'closing' }), item({ kind: 'delivery_failed', can_decide: true, room_lifecycle: 'open' }), item({ kind: 'delivery_failed', can_decide: false, room_lifecycle: 'closed' }), item({ kind: 'recovery', can_decide: true, room_lifecycle: 'closed' })])).length, 2);
assert.deepEqual(plain(m.resolvedItems(undefined)), []);
assert.equal(m.bulkLabel(3), 'Dismiss all resolved (3)');
assert.deepEqual([plain(m.countView(0)), plain(m.countView(4))], [{ text: '0', hidden: true, name: 'Inbox', title: 'aimebu' }, { text: '4', hidden: false, name: 'Inbox, 4 waiting', title: '(4) aimebu' }]);
assert.deepEqual(plain(m.groupItems([item({ id: 'a' }), item({ id: 'b', room_id: 'r2' }), item({ id: 'c' })])).map(group => [group.key, group.items.map(entry => entry.id)]), [['p/r', ['a', 'c']], ['p/r2', ['b']]]);
const names = { projects: new Map([['p', { name: 'Alpha' }]]), rooms: new Map([['r', { title: 'Login' }]]) };
assert.deepEqual([m.groupTitle(item(), names.projects, names.rooms), m.groupTitle(item({ project_id: 'x', room_id: 'y' }), names.projects, names.rooms), m.groupTitle(item({ kind: 'usage_alert' }), names.projects, names.rooms)], ['Alpha / Login', 'Project / Room', 'Usages']);
assert.deepEqual([m.cardKey(item()), m.cardKey(item({ kind: 'recovery', rev: 2, room_lifecycle: 'closed' })), m.cardKey(item({ kind: 'usage_alert', question: 'Q' }))], ['i', 'i|2|closed', 'i|Q']);
assert.deepEqual([plain(m.decisionOptions(item({ kind: 'recovery', turn_id: 't' }))).map(o => o[0]), plain(m.decisionOptions(item({ kind: 'recovery' }))).map(o => o[0]), plain(m.decisionOptions(item({ kind: 'recovery', recovery_phase: 'quiescence_wait', turn_id: 't' }))).map(o => o[0]), plain(m.decisionOptions(item({ kind: 'usage_limit', options: ['Continue: now', 'Stop'] })))],
  [['Resume', 'Retry', 'Abandon'], ['Continue', 'Abandon'], ['Override', 'Abandon'], [['Continue: now', 'continue'], ['Stop', 'abandon']]]);
// A turn that failed before it did anything is offered Retry alone, with the sentence the harness gave and the raw output kept apart.
const failed = item({ kind: 'recovery', turn_id: 't', definite_failure: true, result_text: 'Failed to authenticate: OAuth session expired · Please run /login', evidence: 'Process exited with code 1.\nClaude Code error result: {"type":"result","is_error":true}' });
assert.deepEqual(plain(m.decisionOptions(failed)).map(o => o[0]), ['Retry', 'Abandon']);
assert.deepEqual(plain(m.decisionOptions(item({ kind: 'recovery', turn_id: 't', definite_failure: false }))).map(o => o[0]), ['Resume', 'Retry', 'Abandon']);
assert.match(m.retryWarning(failed), /^Retry is safe: the turn failed before it did anything/);
assert.equal(m.evidenceText(failed), 'Failed to authenticate: OAuth session expired · Please run /login');
assert.deepEqual(plain(m.recoveryResult(failed)), { text: 'Failed to authenticate: OAuth session expired · Please run /login', raw: failed.evidence });
assert.equal(m.recoveryResult(item({ kind: 'recovery', evidence: 'x' })), null, 'no sentence from the harness, no split');
assert.equal(m.recoveryResult(item({ kind: 'delivery_failed', result_text: 'x' })), null);
assert.deepEqual([m.retryWarning(item()), m.retryWarning(item({ turn_id: 't' })).startsWith('Resume may repeat'), m.retryWarning(item({ recovery_phase: 'quiescence_wait' })).startsWith('The old process')], ['', true, true]);
assert.deepEqual([m.evidenceText(item({ kind: 'recovery', evidence: 'Seen.' })), m.evidenceText(item({ kind: 'recovery', diagnostic_report: {} })), m.evidenceText(item({ kind: 'recovery', turn_exec: 'lost' })), m.evidenceText(item({ kind: 'recovery' })), m.evidenceText(item({ kind: 'delivery_failed' }))],
  ['Seen.', 'A diagnostic report is available below.', 'The previous process may still be running; no diagnostic was run.', 'No diagnostic report is available yet.', 'The reply was held; view the room for context.']);
assert.equal(m.boundaryText(item({ boundary_requester: 'lead', boundary_machine: 'Remote', boundary_path: '/w' })), 'Requested by lead · Machine Remote · Folder /w');
console.log('inbox model tests passed');
