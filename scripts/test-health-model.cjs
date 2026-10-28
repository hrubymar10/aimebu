// The Health panel's pure rules (web/js/pages/health/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'health', 'model.js'), 'utf8');
const context = vm.createContext({ Date, Number, Math });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { healthExplanation, healthLabel, healthSentence, formatBytes, previewSummary, removedLine, badgeView, retentionDays };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const codes = ['stuck', 'out_of_quota', 'machine_offline', 'held_answer', 'recovery_waiting'];
assert.deepEqual(codes.map(code => m.healthLabel({ code })), ['Not responding', 'Out of quota', 'Machine offline', 'Reply not delivered', 'Needs your decision']);
assert.equal(m.healthLabel({ code: 'mystery' }), 'mystery');
assert.match(m.healthLabel({ code: 'out_of_quota', reset_at: '2030-01-02T03:04:05Z' }), /^Out of quota until /);
assert.equal(m.healthLabel({ code: 'out_of_quota', reset_at: 'not a date' }), 'Out of quota');
assert.deepEqual(codes.map(code => m.healthSentence('olive', { code })), ['olive looks stuck', 'olive is out of quota', "olive's machine is offline", 'An answer to olive could not be delivered', 'olive needs a recovery decision']);
assert.equal(m.healthSentence('olive', { code: 'mystery' }), 'olive: mystery');
assert.ok(codes.every(code => m.healthExplanation({ code }).length > 20) && m.healthExplanation({ code: 'x' }) === 'Open the agent to see more.');

assert.deepEqual(plain([0, 12, 2048, 5 * 1024 * 1024, 50 * 1024 ** 3, 500 * 1024 ** 3].map(m.formatBytes)), ['0 B', '12 B', '2.00 KB', '5.00 MB', '50.0 GB', '500 GB']);
const items = [{ action: 'would_remove' }, { action: 'kept' }, { action: 'would_remove' }];
assert.equal(m.previewSummary(items, 3072), '2 items, 3.00 KB would be removed. Nothing has been removed yet.');
assert.equal(m.previewSummary([{ action: 'would_remove' }], 10), '1 item, 10 B would be removed. Nothing has been removed yet.');
assert.equal(m.previewSummary([{ action: 'kept' }], 0), 'Nothing to remove right now.');
assert.equal(m.removedLine([{ action: 'removed' }, { action: 'kept' }], 2048), 'Removed 1 item, 2.00 KB.');
assert.equal(m.removedLine([], 0), 'Removed 0 items, 0 B.');
assert.deepEqual(plain(m.badgeView(0)), { text: '0', hidden: true, issues: false, title: 'All agents healthy', label: 'Health' });
assert.deepEqual(plain(m.badgeView(3)), { text: '3', hidden: false, issues: true, title: '3 need attention', label: 'Health, 3 need attention' });
assert.deepEqual([m.retentionDays('30'), m.retentionDays('0'), m.retentionDays('2.6'), m.retentionDays('-1'), m.retentionDays('3651'), m.retentionDays('x'), m.retentionDays('')], [30, 0, 3, null, null, null, 0]);
console.log('health model tests passed');
