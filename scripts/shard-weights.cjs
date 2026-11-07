#!/usr/bin/env node
// Writes scripts/shard-weights.json: how long the slow top-level Go tests take, which scripts/gotestprogress uses to split a package over
// several `go test` processes (-shards, GO_TEST_SHARDS) into groups of about equal length. A test missing from the file counts as the package's "*" average
// (the median of the listed ones when there is none), so the file never has to be complete or current; it only has to be roughly right, and it is regenerated from any run:
//
//   go test -race -json ./internal/server ./internal/store ./cmd/aimebu > /tmp/events.json
//   node scripts/shard-weights.cjs /tmp/events.json > scripts/shard-weights.json
//
// Only passing top-level tests that ran for at least ten seconds are listed; the entry "*" of a package is the average of all the others, keyed by the last two elements of the package's
// import path. Times from a run under heavy load are inflated evenly, which does not matter: only the proportions are used.
const fs = require('node:fs');

const MIN_SECONDS = 10; // shorter tests are listed only through their package's average ("*"), which keeps the file short

const [file] = process.argv.slice(2);
if (!file) { console.error('usage: node scripts/shard-weights.cjs <go test -json output>'); process.exit(2); }
// Many tests call t.Parallel: they pause until the sequential ones are done and then share the CPUs, so their reported elapsed time includes
// the wait. The weight is the time a test actually ran: from "run" (or "cont") to its end, minus the stretches it was paused.
const weights = {};
const state = {}; // package + test -> { since, active }
const rest = {}; // package -> the tests under MIN_SECONDS: their total and count
for (const line of fs.readFileSync(file, 'utf8').split('\n')) {
  if (!line.startsWith('{')) continue;
  let event;
  try { event = JSON.parse(line); } catch { continue; }
  if (!event.Test || event.Test.includes('/')) continue;
  const id = `${event.Package} ${event.Test}`, at = Date.parse(event.Time) / 1000;
  if (event.Action === 'run') state[id] = { since: at, active: 0 };
  else if (event.Action === 'cont' && state[id]) state[id].since = at;
  else if (event.Action === 'pause' && state[id]) { state[id].active += at - state[id].since; state[id].since = null; }
  else if (event.Action === 'pass' && state[id]) {
    const active = state[id].active + (state[id].since === null ? 0 : at - state[id].since);
    const key = event.Package.split('/').slice(-2).join('/');
    if (active < MIN_SECONDS) { const small = (rest[key] ||= { total: 0, count: 0 }); small.total += active; small.count++; continue; }
    (weights[key] ||= {})[event.Test] = Number(active.toFixed(1));
  }
}
for (const [key, small] of Object.entries(rest)) (weights[key] ||= {})['*'] = Number((small.total / small.count).toFixed(2)); // what a test not listed weighs
const sorted = Object.fromEntries(Object.keys(weights).sort().map(key => [key, Object.fromEntries(Object.entries(weights[key]).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])))]));
process.stdout.write(`${JSON.stringify(sorted, null, 1)}\n`);
