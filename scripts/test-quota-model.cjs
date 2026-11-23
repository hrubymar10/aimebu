// The automatic-switching block's pure rules (web/js/pages/usages/quota-model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'usages', 'quota-model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { TOOLS, RANGES, clone, same, when, percent, describeDecision, isDirty, isSavable, numberOf, boxText, numbersProblem, protectProfile, afterResetFor, addable, protectNames, watchChoices, orderEntry, moveEntry, decisionText, decisionProfiles, decisionAction, resetText, trippedText, focusFallbacks };', context);
const plain = value => JSON.parse(JSON.stringify(value));
const m = Object.fromEntries(Object.entries(context.api).map(([name, value]) => [name, typeof value === 'function' ? (...args) => { const out = value(...args); return out === undefined ? undefined : plain(out); } : plain(value)]));

const draft = () => ({ enabled: true, profiles: [{ name: 'a', session_max: 95, weekly_max: 90 }, { name: 'b', session_max: 80, weekly_max: 70 }], protected: ['p'], after_reset: null, warmup_seconds: 60, min_dwell_minutes: 5 });

// Equality ignores key order; a draft is dirty against the stored copy, and unsaved defaults can be saved as they are.
assert.equal(m.same({ a: 1, b: [{ x: 1, y: 2 }] }, { b: [{ y: 2, x: 1 }], a: 1 }), true);
assert.equal(m.same({ a: 1 }, { a: 2 }), false);
const data = { tools: { claude: { config: draft(), saved: false }, codex: { config: draft(), saved: true } } };
assert.deepEqual([m.isDirty(data, { claude: draft() }, 'claude'), m.isDirty(data, { claude: { ...draft(), enabled: false } }, 'claude'), m.isDirty(data, {}, 'claude'), m.isDirty(null, { claude: draft() }, 'claude')], [false, true, false, false]);
assert.deepEqual([m.isSavable(data, { claude: draft() }, 'claude'), m.isSavable(data, { codex: draft() }, 'codex'), m.isSavable(data, { codex: { ...draft(), enabled: false } }, 'codex')], [true, false, true]);

// A number box: empty is not a number, and a number that is not finite shows as empty.
assert.equal(Number.isNaN(context.api.numberOf('')), true, 'an empty box is NaN');
assert.deepEqual([m.numberOf('9'), m.numberOf('1.5'), m.boxText(NaN), m.boxText(95), m.boxText(0)], [9, 1.5, '', '95', '0']);

// What stops a draft from being saved.
assert.equal(m.numbersProblem(draft()), '');
assert.match(m.numbersProblem({ ...draft(), profiles: [{ name: 'a', session_max: 0, weekly_max: 90 }] }), /limits for a must be whole numbers from 1 to 100/);
assert.match(m.numbersProblem({ ...draft(), profiles: [{ name: 'a', session_max: 95, weekly_max: 101 }] }), /limits for a/);
assert.match(m.numbersProblem({ ...draft(), profiles: [{ name: 'a', session_max: 9.5, weekly_max: 90 }] }), /limits for a/);
assert.match(m.numbersProblem({ ...draft(), after_reset: { profile: 'a', drop_points: 20, profiles: [{ name: 'a', session_max: 95, weekly_max: 95 }] } }), /after the reset needs at least two profiles/);
assert.equal(m.numbersProblem({ ...draft(), enabled: false, after_reset: { profile: 'a', drop_points: 20, profiles: [{ name: 'a', session_max: 95, weekly_max: 95 }] } }), '', 'one profile after a reset is fine while switching is off');
assert.match(m.numbersProblem({ ...draft(), after_reset: { profile: 'a', drop_points: 0, profiles: draft().profiles } }), /reset drop must be a whole number from 1 to 100/);
assert.match(m.numbersProblem({ ...draft(), warmup_seconds: 601 }), /Warm-up must be a whole number of seconds from 0 to 600/);
assert.match(m.numbersProblem({ ...draft(), min_dwell_minutes: -1 }), /minutes from 0 to 120/);

// Protecting takes the profile out of both orders and off the watch; unprotecting only lifts the protection.
const protectedDraft = { ...draft(), after_reset: { profile: 'a', drop_points: 20, profiles: draft().profiles } };
const clone = JSON.parse(JSON.stringify(protectedDraft));
const vmDraft = vm.runInContext('(' + JSON.stringify(clone) + ')', context);
context.api.protectProfile(vmDraft, 'a', true);
assert.deepEqual(plain(vmDraft).protected, ['p', 'a']);
assert.deepEqual([plain(vmDraft).profiles.map(entry => entry.name), plain(vmDraft).after_reset.profiles.map(entry => entry.name), plain(vmDraft).after_reset.profile], [['b'], ['b'], 'b'], 'the watched profile moves to one that is still listed');
context.api.protectProfile(vmDraft, 'a', false);
assert.deepEqual(plain(vmDraft).protected, ['p']);

// Lists of profiles.
assert.deepEqual([m.addable(['a', 'b', 'c', 'p'], draft().profiles, ['p']), m.protectNames(['a', 'b'], draft()), m.watchChoices({ profile: 'z', profiles: [{ name: 'a' }] }), m.watchChoices({ profile: '', profiles: [{ name: 'a' }] })], [['c'], ['a', 'b', 'p'], ['a', 'z'], ['a']]);
assert.deepEqual([m.orderEntry('x'), m.afterResetFor(draft())], [{ name: 'x', session_max: 95, weekly_max: 95 }, { profile: 'a', drop_points: 20, profiles: draft().profiles }]);
const entries = vm.runInContext('[1, 2, 3]', context);
context.api.moveEntry(entries, 0, 1);
assert.deepEqual(plain(entries), [2, 1, 3]);

// What the server says, in words.
assert.deepEqual([m.describeDecision({ action: 'switch', from: 'a', to: 'b' }), m.describeDecision({ action: 'switch', from: 'a', to: 'b', dry_run: true, reason: 'low' }), m.describeDecision({ action: 'stay', from: 'a' }), m.describeDecision({ action: 'hold', reason: 'warming up' }), m.describeDecision({ action: 'idle', reason: 'ignored' }), m.describeDecision({ action: 'odd' })],
  ['switched a → b', 'would switch a → b: low', 'stay on a', 'hold: warming up', 'idle', 'odd']);
assert.deepEqual([m.percent(41.6), m.percent('x'), m.when(null), m.when('nope')], ['42%', '-', 'never', 'unknown']);
assert.equal(m.decisionAction({ action: 'sw<i>tch!' }), 'switch', 'only letters of the action reach a data attribute');
assert.deepEqual(m.decisionProfiles({ profiles: { a: { status: 'ok', session: 1, weekly: 2 }, b: null } }), ['a: ok, session 1%, weekly 2%', 'b: ?, session -, weekly -']);
assert.match(m.decisionText({ at: '2026-01-01T00:00:00Z', action: 'hold', dry_run: true }), /: hold$/, 'no dry-run suffix any more');
assert.match(m.decisionText({ at: '2026-01-01T00:00:00Z', action: 'switch', from: 'a', to: 'b', dry_run: true }), /: would switch a → b$/, 'an old journal entry of a dry-run switch still says it was not made');
assert.match(m.resetText({ reset_latched_at: '2026-01-01T00:00:00Z', weekly_max_seen: 91.2 }), /\(the weekly peak was 91%\); the order after the reset is in effect\. Re-arm when the next week starts\.$/);
assert.match(m.trippedText({ tripped_at: null }), /paused: it switched too often in one hour \(never\)/);

// Focus after a change: the control itself, else its sibling in the row.
assert.deepEqual([m.focusFallbacks('order:a:down'), m.focusFallbacks('order:a:up'), m.focusFallbacks('claude-save')], [['order:a:up', 'order:a:remove'], ['order:a:down', 'order:a:remove'], []]);
console.log('quota model tests passed');
