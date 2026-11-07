// The usage alerts and stale-age rules (web/js/usage-alerts.js), run in a vm. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'usage-alerts.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { mergeAlerts, staleAfterSeconds, staleAge };', context);
const { mergeAlerts, staleAfterSeconds, staleAge } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const conflict = (tool, root) => ({ kind: 'refresher_conflict', message: `${tool}: another token refresher may own the same live credentials: ${root}; this tool will not refresh` });
const merged = plain(mergeAlerts([conflict('claude', '/h/.aimebu/switcher'), { kind: 'expiring', message: 'claude live: the access token expires in 4 min.' }, conflict('codex', '/h/.aimebu/switcher')]));
assert.equal(merged.length, 2, 'the two per-tool sentences are one alert');
assert.equal(merged[0].kind, 'refresher_conflict');
assert.match(merged[0].text, /^Token refresh is paused for claude, codex: another aimebu \(v1\) or token refresher manages these logins\./);
assert.doesNotMatch(merged[0].text, /\/h\/\.aimebu/, 'the path is in the details, not the sentence');
assert.deepEqual(merged[0].details, ['claude: another token refresher may own the same live credentials: /h/.aimebu/switcher', 'codex: another token refresher may own the same live credentials: /h/.aimebu/switcher']);
assert.deepEqual(merged[1], { kind: 'expiring', text: 'claude live: the access token expires in 4 min.', details: [] }, 'another alert is kept as it is');
assert.match(plain(mergeAlerts([conflict('claude', '/x')]))[0].text, /manages this login\./, 'one tool says "this login"');
assert.deepEqual(plain(mergeAlerts([{ kind: 'refresher_conflict', message: 'something the server said differently' }])), [{ kind: 'refresher_conflict', text: 'something the server said differently', details: [] }], 'a conflict in another shape is kept whole');
assert.deepEqual(plain(mergeAlerts(undefined)), []);
assert.deepEqual(plain(mergeAlerts(null)), []);

assert.equal(staleAfterSeconds(120), 300);
assert.equal(staleAfterSeconds(600), 1200, 'twice the interval');
assert.equal(staleAfterSeconds(0), 300);
const now = Date.parse('2026-11-07T12:00:00Z'), ago = seconds => new Date(now - seconds * 1000).toISOString();
assert.equal(staleAge(ago(200), 120, now), '', 'fresh');
assert.equal(staleAge(ago(300), 120, now), '', 'at the limit is not stale');
assert.equal(staleAge(ago(900), 120, now), '15 min');
assert.equal(staleAge(ago(7200), 120, now), '2h');
assert.equal(staleAge(ago(2 * 86400 + 5), 120, now), '2d');
assert.equal(staleAge('', 120, now), '', 'no time, no claim');
assert.equal(staleAge('garbage', 120, now), '');
console.log('usage alerts: ok');
