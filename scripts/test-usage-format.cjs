// The shared usage window helpers (web/js/usage-format.js) that the popover and the Usages
// page rows both use, run in a vm: window labels, and the monthly daily target.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'usage-format.js'), 'utf8');
const context = vm.createContext({ Date });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { windowLabel, isMonthlyWindow, dailyTarget, dailyTargetText };', context);
const f = context.api;

assert.deepEqual([f.windowLabel('monthly_spend'), f.windowLabel('monthly'), f.windowLabel('weekly_scoped:team-research')], ['Monthly spend', 'Monthly', 'Weekly (team research)']);

// Only a calendar-month span (28 to 31 days, including a 30-day sentinel) is a monthly window.
const DAY = 86400;
assert.deepEqual([f.isMonthlyWindow({ window_duration_seconds: 30 * DAY }), f.isMonthlyWindow({ window_duration_seconds: 28 * DAY }), f.isMonthlyWindow({ window_duration_seconds: 31 * DAY }), f.isMonthlyWindow({ window_duration_seconds: 7 * DAY }), f.isMonthlyWindow({}), f.isMonthlyWindow({ window_duration_seconds: 5 * 3600 })], [true, true, true, false, false, false]);

// The target divides the remaining share by the local calendar days left, counting today; a
// reset exactly at local midnight starts the next allowance without adding a day.
const now = new Date(2026, 5, 15, 12, 0, 0);
const monthly = (reset, used) => ({ key: 'monthly', window_duration_seconds: 30 * DAY, percent_used: used, reset_at: reset.toISOString() });
assert.equal(f.dailyTarget(monthly(new Date(2026, 5, 20, 9, 0), 40), now), 10, 'six days left (today plus five, the reset day is partial) split 60% left');
assert.equal(f.dailyTarget(monthly(new Date(2026, 5, 20, 0, 0), 40), now), 12, 'a midnight reset adds no partial day');
assert.equal(f.dailyTarget(monthly(new Date(2026, 5, 15, 23, 0), 99), now), 1, 'today counts even when it is almost over');
assert.deepEqual([f.dailyTarget(monthly(new Date(2026, 5, 15, 6, 0), 40), now), f.dailyTarget({ window_duration_seconds: 30 * DAY, percent_used: 40 }, now), f.dailyTarget(monthly(new Date(2026, 5, 20, 9, 0)), now), f.dailyTarget({ ...monthly(new Date(2026, 5, 20, 9, 0), 40), window_duration_seconds: 7 * DAY }, now)], [null, null, null, null], 'an expired reset, a missing reset, unknown usage, and a non-monthly window show no target');
assert.equal(f.dailyTargetText(monthly(new Date(2026, 5, 20, 9, 0), 40), now), '≈ 10% / day to last until reset');
assert.equal(f.dailyTargetText(monthly(new Date(2026, 5, 20, 9, 0), 40.25), now), '≈ 9.96% / day to last until reset', 'two decimals at most');

console.log('ok usage-format');
