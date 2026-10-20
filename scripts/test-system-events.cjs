// describeEvent is a browser module (it imports the api and DOM helpers), so this test
// lifts the pure pieces out of the source and runs them in a vm: the label table, the
// detail keys and describeEvent itself. No browser, no server.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'system-events.js'), 'utf8');

function slice(startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `${startMarker} is missing from system-events.js`);
  const end = source.indexOf(endMarker, start);
  assert.notEqual(end, -1, `${endMarker} is missing after ${startMarker}`);
  return source.slice(start, end);
}

const code = [
  slice('const labels = {', '// Payload fields worth showing'),
  slice('const detailKeys', 'export function describeEvent'),
  slice('export function describeEvent', '// Browser-side preferences').replace('export function', 'function'),
  'globalThis.describeEvent = describeEvent;',
].join('\n');
const context = { globalThis: null };
context.globalThis = context;
vm.createContext(context);
vm.runInContext(code, context);
const describe = (event, names) => context.describeEvent(event, names);

const alert = (payload, extra = {}) => ({ kind: 'credential_alert', payload_json: JSON.stringify(payload), ...extra });

// The label and the payload's own kind + detail are shown.
let view = describe(alert({ kind: 'reauth_required', detail: 'claude/main: the provider rejected the refresh token' }));
assert.equal(view.text, 'Credential alert');
assert.equal(view.details, 'reauth required: claude/main: the provider rejected the refresh token');

// The reply loop guard reads as a sentence.
assert.equal(describe({ kind: 'loop_guard', payload_json: '{"hops":"4"}' }).text, 'Agents stopped answering each other');

// A kind with no detail, and a detail with no kind, still read sensibly.
assert.equal(describe(alert({ kind: 'login_imported' })).details, 'login imported');
assert.equal(describe(alert({ detail: 'only detail' })).details, 'only detail');
assert.equal(describe(alert({})).details, '');

// Markup in the detail is returned as text, untouched: the feed renders it with
// textContent, so it is never parsed. (Escaping here would double-escape.)
view = describe(alert({ kind: 'expiring', detail: '<img src=x onerror=alert(1)> & "quoted"' }));
assert.equal(view.details, 'expiring: <img src=x onerror=alert(1)> & "quoted"');
assert.match(source, /node\('span', 'system-event-details', view\.details\)/, 'the feed must render details through node(), which uses textContent');
assert.doesNotMatch(source, /innerHTML\s*=\s*[^;]*view\.details/, 'details must never be written with innerHTML');

// `kind` and `detail` must not leak into other events through detailKeys.
view = describe({ kind: 'agent_stopped', payload_json: JSON.stringify({ kind: 'x', detail: 'y', reason: 'done' }) });
assert.equal(view.details, 'reason: done', 'the credential branch must not widen detailKeys');

// A malformed payload does not throw, and the where line still works.
assert.equal(describe({ kind: 'credential_alert', payload_json: '{not json' }).details, '');
view = describe(alert({ kind: 'expired', detail: 'claude live' }, { agent_id: 'a1', room_id: 'r1' }), { agentName: () => 'Ada', roomTitle: () => 'Main' });
assert.equal(view.where, 'Ada · Main');

// Quota events: the tool, the move (or the watched profile) and the fixed reason.
const quota = (kind, payload) => ({ kind, payload_json: JSON.stringify(payload) });
view = describe(quota('quota_switch', { tool: 'claude', from: 'a', to: 'b', reason: 'the active profile is over its limit or unusable' }));
assert.equal(view.text, 'Automatic account switch');
assert.equal(view.details, 'claude · a → b · the active profile is over its limit or unusable');
assert.equal(describe(quota('quota_reset_seen', { tool: 'claude', profile: 'a', reason: 'the watched weekly window reset' })).details, 'claude · a · the watched weekly window reset');
assert.equal(describe(quota('quota_switcher_tripped', { tool: 'codex', reason: 'too many automatic switch attempts this hour' })).text, 'Automatic switching tripped');
assert.equal(describe(quota('quota_switch_failed', { tool: 'claude', from: 'a', to: 'b', reason: 'switch failed' })).text, 'Automatic account switch failed');
assert.equal(describe(quota('quota_switch', { tool: '<b>x</b>' })).details, '<b>x</b>', 'markup stays text');
assert.equal(describe({ kind: 'agent_stopped', payload_json: JSON.stringify({ tool: 'claude', from: 'a', to: 'b' }) }).details, '', 'quota keys must not leak into other events');

// Limit guard events: the profile, how full it is and what happens next.
view = describe(quota('limit_guard_hard', { tool: 'claude', profile: 'a', percent: '96', window: 'weekly', next: 'switching' }));
assert.equal(view.text, 'Usage limit reached');
assert.equal(view.details, 'claude · a · 96% weekly · the quota switcher moves off it');
assert.equal(describe(quota('limit_guard_soft', { tool: 'codex', profile: 'b', percent: '91', window: 'session' })).details, 'codex · b · 91% session');
assert.equal(describe(quota('limit_guard_hard', { tool: 'codex', profile: 'b', percent: '97', window: 'session', next: 'held' })).details, 'codex · b · 97% session · new turns stay held');
assert.equal(describe(quota('limit_guard_clear', { tool: 'codex', profile: 'b', percent: '5', window: 'weekly' })).text, 'Usage back under the limit: new turns resume');

// Every other kind keeps its own label.
assert.equal(describe({ kind: 'agent_stopped', payload_json: '{}' }).text, 'Agent stopped');
console.log('system events describe test passed');
