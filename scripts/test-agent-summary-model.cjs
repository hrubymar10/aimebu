// The stuck-or-dead summary over an agent's logs (web/js/pages/agents/summary-model.js), run in a vm: the state chip for every state and flag
// combination, the times, the last event and exit evidence, and the flags in their fixed order.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'agents', 'summary-model.js'), 'utf8');
const context = vm.createContext({ Date, Number, Math, JSON, Set, Object, Array, String });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { SUMMARY_FLAGS, span, chipOf, summaryOf };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));
const formatClock = at => new Date(at).toISOString().slice(11, 16);
const now = Date.parse('2026-11-23T12:00:00Z');
const ago = ms => new Date(now - ms).toISOString();
const card = patch => ({ desired: 'active', observed: 'idle', health_flags: [], ...patch });

// Spans as people read them.
assert.deepEqual([0, 45000, 192000, 600000, 2460000, 3600000, 5400000, 90000000, 172800000, -5000].map(m.span), ['0s', '45s', '3m 12s', '10m', '41m', '1h', '1h 30m', '1d 1h', '2d', '0s']);

// The chip for every state, and which one wins when several hold.
const chip = patch => plain(m.chipOf(card(patch), { formatClock }));
assert.deepEqual(chip({}), { kind: 'idle', label: 'Idle' });
assert.deepEqual(chip({ observed: 'in_turn' }), { kind: 'working', label: 'Working' });
assert.deepEqual(chip({ observed: 'interrupting' }), { kind: 'working', label: 'Working' });
assert.deepEqual(chip({ warm: { state: 'warm', sleeps_at: '2026-11-23T12:40:00Z' } }), { kind: 'idle', label: 'Idle, warm until 12:40' });
assert.deepEqual(chip({ warm: { state: 'warm', sleeps_at: 'later' } }), { kind: 'idle', label: 'Idle, warm' });
assert.deepEqual(chip({ warm: { state: 'sleeping' } }), { kind: 'sleeping', label: 'Sleeping' });
assert.deepEqual(chip({ warm: { state: 'na' } }), { kind: 'idle', label: 'Idle' });
assert.deepEqual(chip({ desired: 'stopped', observed: 'stopped' }), { kind: 'stopped', label: 'Stopped' });
assert.deepEqual(chip({ desired: 'stopped', observed: 'in_turn' }), { kind: 'stopped', label: 'Stopped' }, 'a stop asked for is what the chip says');
assert.deepEqual(chip({ observed: 'lost' }), { kind: 'recovery', label: 'Needs recovery' });
assert.deepEqual(chip({ recovery_block: true }), { kind: 'recovery', label: 'Needs recovery' });
assert.deepEqual(chip({ health_flags: [{ code: 'recovery_waiting' }] }), { kind: 'recovery', label: 'Needs recovery' });
assert.deepEqual(chip({ observed: 'in_turn', recovery_block: true }), { kind: 'recovery', label: 'Needs recovery' }, 'a decision outranks working');
assert.deepEqual(chip({ observed: 'in_turn', health_flags: [{ code: 'machine_offline' }] }), { kind: 'offline', label: 'Machine offline' }, 'an offline machine outranks everything');
assert.deepEqual(chip({ recovery_block: true, health_flags: [{ code: 'machine_offline' }] }), { kind: 'offline', label: 'Machine offline' });
assert.deepEqual(chip({ observed: 'in_turn', health_flags: [{ code: 'stuck' }] }), { kind: 'working', label: 'Working' }, 'stuck is a flag beside the chip, not a state');

// The summary's lines.
const summary = patch => plain(m.summaryOf(card(patch), now, { formatClock }));
let s = summary({ observed: 'in_turn', last_activity_at: ago(192000), turn_started_at: ago(2460000), activity: 'Working 11:19' });
assert.deepEqual([s.chip.label, s.activity, s.turn, s.event, s.exit, s.flags], ['Working', 'Last activity 3m 12s ago', 'Turn running for 41m', 'Working 11:19', '', []]);
assert.equal(summary({ last_activity_at: ago(600000), activity_at: ago(5000) }).activity, 'Last activity 5s ago', 'the newer of the server\'s time and the last event');
assert.equal(summary({}).activity, 'No activity recorded');
assert.equal(summary({ observed: 'in_turn' }).turn, 'In a turn', 'a turn that started before the page opened has no length');
assert.equal(summary({ turn_started_at: ago(1000) }).turn, '', 'an idle agent has no turn line');
s = summary({ observed: 'lost', last_exit_evidence: 'Process exited with code 17.\nCheckout is not mounted.' });
assert.deepEqual([s.exit, s.exitFull], ['Process exited with code 17.', 'Process exited with code 17.\nCheckout is not mounted.']);
assert.equal(summary({ last_exit_evidence: 'x'.repeat(500) }).exit.length, 200);
// The flags it names, in a fixed order; others are left to the card.
s = summary({ health_flags: [{ code: 'machine_offline' }, { code: 'held_answer' }, { code: 'something_else' }, { code: 'stuck' }, { code: 'out_of_quota', reset_at: '2026-11-23T14:05:00Z' }, { code: 'recovery_waiting' }] });
assert.deepEqual(s.flags.map(flag => flag.code), ['stuck', 'recovery_waiting', 'held_answer', 'out_of_quota', 'machine_offline']);
assert.deepEqual(s.flags.map(flag => flag.label), ['Stuck: no progress for a while', 'Waiting for a recovery decision', 'An answer is held', 'Out of quota until 14:05', 'Machine offline']);
assert.equal(summary({ health_flags: [{ code: 'out_of_quota' }] }).flags[0].label, 'Out of quota');
// Every combination of the five flags with every base state gives a chip and never throws.
const bases = [{}, { observed: 'in_turn' }, { desired: 'stopped', observed: 'stopped' }, { observed: 'lost' }, { warm: { state: 'sleeping' } }, { warm: { state: 'warm', sleeps_at: ago(-60000) } }, { recovery_block: true }];
for (let mask = 0; mask < 32; mask++) {
  const flags = m.SUMMARY_FLAGS.filter((_, index) => mask & (1 << index)).map(code => ({ code }));
  for (const base of bases) {
    const result = m.summaryOf(card({ ...base, health_flags: flags }), now, { formatClock });
    assert.equal(result.flags.length, flags.length);
    const codes = new Set(flags.map(flag => flag.code));
    const expected = codes.has('machine_offline') ? 'offline' : base.desired === 'stopped' ? 'stopped' : (base.recovery_block || base.observed === 'lost' || codes.has('recovery_waiting')) ? 'recovery' : base.observed === 'in_turn' ? 'working' : base.warm?.state === 'sleeping' ? 'sleeping' : 'idle';
    assert.equal(result.chip.kind, expected, `${JSON.stringify(base)} with ${[...codes]}`);
  }
}
console.log('agent summary model test passed');
