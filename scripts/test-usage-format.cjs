// The shared usage window helpers (web/js/usage-format.js) that the popover and the Usages
// page rows both use, run in a vm: window labels, and the monthly daily target.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'usage-format.js'), 'utf8');
const context = vm.createContext({ Date });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { windowLabel, isMonthlyWindow, dailyTarget, dailyTargetText, sparkGeometry };', context);
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

// The 48-hour sparkline geometry, and the marker a rejected login leaves on it:
// a marker only when the rejection falls inside the span and on a visible gap
// (after the series' first sample), never for an ordinary gap.
const HOUR = 3600000;
const sparkNow = Date.parse('2026-11-24T12:00:00Z');
const sparkStart = (sparkNow - 40 * HOUR) / 1000;
const sparkWindow = rejected => ({
  key: 'session',
  history: [
    { t: sparkStart, p: 10 },
    { t: sparkStart + 10 * 3600, p: 20 },
    { t: sparkStart + 20 * 3600, p: 30 },
  ],
  ...(rejected ? { rejected_at: rejected } : {}),
});
const spark = f.sparkGeometry(sparkWindow(), 'used', sparkNow);
assert.equal(spark.marker, null, 'an ordinary gap (the server being down) carries no marker');
assert.equal(spark.label, 'Session over the last 48 hours: 10% to 30% used');
const marked = f.sparkGeometry(sparkWindow(sparkStart + 12 * 3600), 'used', sparkNow);
assert.ok(marked.marker, 'a rejected series carries a marker at the first rejection');
assert.equal(Number(marked.marker.x) > 0 && Number(marked.marker.x) < 100, true, `the marker sits inside the line: ${marked.marker.x}`);
assert.ok(marked.marker.label.startsWith('Login rejected'), `the marker says what happened: ${marked.marker.label}`);
assert.equal(f.sparkGeometry(sparkWindow(sparkStart - 3600), 'used', sparkNow).marker, null, 'a rejection before the first sample leaves no gap to mark');
assert.equal(f.sparkGeometry(sparkWindow((sparkNow - 49 * HOUR) / 1000), 'used', sparkNow).marker, null, 'a rejection older than the span is not marked');
