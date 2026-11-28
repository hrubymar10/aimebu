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
  slice('export function describeEvent', 'const keep = 200;').replace('export function', 'function'),
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
assert.equal(view.text, 'Credential alert: reauth required');
assert.equal(view.where, 'claude/main', 'the row names whose login it is');
assert.equal(view.details, 'the provider rejected the refresh token');

// The update-check notice reads as a sentence for each state, with the version beside it; an empty state is the notice clearing.
const update = payload => describe({ kind: 'update_status', payload_json: JSON.stringify(payload) });
assert.deepEqual([update({ state: 'available', version: 'v2.1.0' }).text, update({ state: 'available', version: 'v2.1.0' }).details], ['Update available', 'v2.1.0']);
assert.match(update({ state: 'installed', version: 'v2.1.0' }).text, /restart/);
assert.match(update({ state: 'unbuilt', version: 'v2.1.0-3-gabc' }).text, /newer than the built binary/);
assert.deepEqual([update({ state: '', version: '' }).text, update({ state: '', version: '' }).details], ['Update notice cleared', '']);
assert.equal(update({}).text, 'Update notice cleared');

// The reply loop guard reads as a sentence.
assert.equal(describe({ kind: 'loop_guard', payload_json: '{"hops":"4"}' }).text, 'Agents stopped answering each other');
assert.equal(describe({ kind: 'machine_scope_updated', payload_json: '{"actor_id":"usr_owner"}', names: { usr_owner: 'Owner' } }).text,
  'Machine scope changed by Owner');

// A kind with no detail, and a detail with no kind, still read sensibly.
assert.equal(describe(alert({ kind: 'login_imported' })).text, 'Credential alert: login imported');
assert.equal(describe(alert({ detail: 'only detail' })).details, 'only detail');
assert.equal(describe(alert({})).details, '');

// Markup in the detail is returned as text, untouched: the feed renders it with
// textContent, so it is never parsed. (Escaping here would double-escape.)
view = describe(alert({ kind: 'expiring', detail: '<img src=x onerror=alert(1)> & "quoted"' }));
assert.equal(view.details, '<img src=x onerror=alert(1)> & "quoted"');
assert.match(source, /node\('div', 'system-event-details', view\.details\)/, 'the feed must render details through node(), which uses textContent');
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
assert.equal(describe(quota('quota_hold', { tool: 'claude', reason: 'active snapshot not ok' })).text, 'Automatic account switch held');
view = describe(quota('quota_switch_dry_run', { tool: 'claude', from: 'main', to: 'bkp', reason: 'the active profile is over its limit or unusable' }));
assert.equal(view.text, 'Automatic account switch due, not made: dry run is on', 'an event written while dry run existed still renders');
assert.equal(view.details, 'claude · main → bkp · the active profile is over its limit or unusable');
view = describe(quota('quota_dry_run_removed', { tool: 'claude' }));
assert.equal(view.text, 'Automatic account switching has no dry run any more: it now switches for real');
assert.match(view.details, /claude/);
view = describe(quota('switch_overwritten', { tool: 'codex' }));
assert.equal(view.details, 'tool: codex');
assert.match(view.text, /installed again/);
assert.equal(describe(quota('quota_switch', { tool: '<b>x</b>' })).details, '<b>x</b>', 'markup stays text');
assert.equal(describe({ kind: 'agent_stopped', payload_json: JSON.stringify({ tool: 'claude', from: 'a', to: 'b' }) }).details, '', 'quota keys must not leak into other events');

// Limit guard events: the profile, how full it is and what happens next.
view = describe(quota('limit_guard_hard', { tool: 'claude', profile: 'a', percent: '96', window: 'weekly', next: 'switching' }));
assert.equal(view.text, 'Usage limit reached');
assert.equal(view.details, 'claude · a · 96% weekly · the quota switcher moves off it');
assert.equal(describe(quota('limit_guard_soft', { tool: 'codex', profile: 'b', percent: '91', window: 'session' })).details, 'codex · b · 91% session');
assert.equal(describe(quota('limit_guard_hard', { tool: 'codex', profile: 'b', percent: '97', window: 'session', next: 'held' })).details, 'codex · b · 97% session · new turns stay held');
assert.equal(describe(quota('limit_guard_hard', { tool: 'claude', profile: 'main', percent: '100', window: 'session', next: 'dry_run' })).details, 'claude · main · 100% session · the quota switcher would move off it, but it is in dry run');
assert.equal(describe(quota('limit_guard_clear', { tool: 'codex', profile: 'b', percent: '5', window: 'weekly' })).text, 'Usage back under the limit: new turns resume');

// The rejected-login event names the tool, the profile and the way back in,
// and — when the server recorded them — when the poller first saw the
// rejection and the tail of the rejected credential's hash.
view = describe(quota('usage_login_rejected', { tool: 'codex', profile: 'work', hint: 'Run codex login.' }));
assert.equal(view.text, 'Usage login rejected: codex work');
assert.equal(view.details, 'Run codex login.');
assert.equal(describe(quota('usage_login_rejected', {})).text, 'Usage login rejected', 'a payload without its pieces still renders');
assert.equal(describe(quota('usage_login_rejected', { tool: 'claude', profile: 'main', hint: 'Run claude /login.' })).text, 'Usage login rejected: claude main');
const rejectedAt = describe(quota('usage_login_rejected', { tool: 'codex', profile: 'work', hint: 'Run codex login.', first_seen: '2026-11-24T17:32:00Z', token_tail: 'dead42' }));
assert.ok(rejectedAt.details.startsWith('Run codex login. First rejected at '), `the event says when the rejection was first seen: ${rejectedAt.details}`);
assert.ok(rejectedAt.details.endsWith('(token hash tail dead42).'), `the event names the credential by its hash tail: ${rejectedAt.details}`);
assert.equal(describe(quota('usage_login_rejected', { tool: 'codex', profile: 'work', first_seen: 'not a time' })).details, '', 'an unusable first-seen time is dropped, not shown');
assert.equal(describe({ kind: 'agent_stopped', payload_json: JSON.stringify({ tool: 'codex', profile: 'work', hint: 'x' }) }).details, '', 'login-rejected keys must not leak into other events');

// Who an id is: the server names the ids of an event (an agent as slug@project, a user by login), so the feed does not show agt_… or usr_…. The server's name
// is used for an agent of a room that is not open; what the open room knows is the fallback; the id is the last resort.
const named = (kind, payload, names, extra = {}) => ({ kind, payload_json: JSON.stringify(payload), names, ...extra });
assert.equal(describe(named('agent_spawn_confirmed', {}, { agt_1: 'hana@tmp' }, { agent_id: 'agt_1' })).where, 'hana@tmp');
assert.equal(describe(named('turn_process_exit', { exit_code: 0 }, { agt_1: 'hana@tmp' }, { agent_id: 'agt_1', room_id: 'r1' }), { agentName: () => 'Other', roomTitle: () => 'general' }).where, 'hana@tmp · general', 'the server\'s name wins over a card\'s');
assert.equal(describe(named('agent_created', {}, undefined, { agent_id: 'agt_1' }), { agentName: () => 'hana' }).where, 'hana', 'no server name: the open room\'s card');
assert.equal(describe(named('agent_created', {}, {}, { agent_id: 'agt_1' })).where, 'agt_1', 'no name at all: the id');
// The people of an account event.
assert.equal(describe(named('user_invite_redeemed', { invite_id: 'inv_1', user_id: 'usr_1' }, { usr_1: 'wren' })).text, 'Invite redeemed by wren');
assert.equal(describe(named('user_invite_redeemed', { invite_id: 'inv_1', user_id: 'usr_1' }, {})).text, 'Invite redeemed', 'an account nobody can name: the plain label');
assert.deepEqual([describe(named('user_invite_created', { invite_id: 'inv_1', kind: 'join', by: 'usr_2' }, { usr_2: 'owner' })).text, describe(named('user_invite_created', { invite_id: 'inv_1', kind: 'join', by: 'usr_2' }, { usr_2: 'owner' })).details], ['Invite created by owner', 'join link']);
assert.equal(describe(named('user_invite_created', { invite_id: 'inv_0', kind: 'join' }, {})).text, 'Invite created', 'an invite from before the payload said who made it');
assert.equal(describe(named('user_created', { user_id: 'usr_1' }, { usr_1: 'wren' })).text, 'User created: wren');
assert.equal(describe(named('user_deactivated', { user_id: 'usr_1' }, { usr_1: 'wren' })).text, 'User deactivated: wren');
assert.equal(describe(named('user_admin_changed', { user_id: 'usr_1', admin: 'true', by: 'usr_2' }, { usr_1: 'wren', usr_2: 'owner' })).text, 'Admin changed: wren is now an admin, by owner');
assert.equal(describe(named('user_admin_changed', { user_id: 'usr_1', admin: 'false' }, { usr_1: 'wren' })).text, 'Admin changed: wren is no longer an admin');
// A name is text, never markup.
assert.equal(describe(named('user_created', { user_id: 'usr_1' }, { usr_1: '<b>x</b>' })).text, 'User created: <b>x</b>');

// Who started, stopped or paused an agent reads as one row: "owen@test: started by wren". The system or the limit guard is named when it was not a person.
const control = (payload, names = { agt_1: 'owen@test', usr_1: 'wren' }, extra = {}) => ({ kind: 'agent_control_requested', agent_id: 'agt_1', payload_json: JSON.stringify(payload), names, ...extra });
assert.equal(describe(control({ desired: 'active', actor_kind: 'user', actor_id: 'usr_1' })).text, 'owen@test: started by wren');
assert.equal(describe(control({ desired: 'stopped', actor_kind: 'user', actor_id: 'usr_1' })).text, 'owen@test: stopped by wren');
assert.equal(describe(control({ desired: 'not_accepting', actor_kind: 'user', actor_id: 'usr_1' })).text, 'owen@test: paused by wren');
assert.equal(describe(control({ desired: 'stopped', actor_kind: 'user', actor_id: 'usr_9' })).text, 'owen@test: stopped by someone', 'a person nobody can name');
assert.equal(describe(control({ desired: 'stopped', actor_kind: 'system', actor_id: 'x' })).text, 'owen@test: stopped by the system');
assert.equal(describe(control({ desired: 'active', actor_kind: 'system', reason: 'provider_limit_retry' })).text, 'owen@test: started by the limit guard');
assert.equal(describe(control({ desired: 'stopped', actor_kind: 'user', actor_id: 'usr_1' }, undefined, { room_id: 'r1' }), { roomTitle: () => 'general' }).where, 'general', 'the agent is in the title, the room is where');

// (What the bell shows is decided by the server, which marks each event `notable`; the store's tests hold that list.)
assert.equal(describe(control({ desired: 'active', actor: 'usr_1' }, undefined)).text, 'owen@test: started by wren', 'a recovery decision names its actor `actor`, not `actor_id`');
assert.equal(describe({ kind: 'agent_context_cleared', agent_id: 'agt_1', names: { agt_1: 'owen@test', usr_1: 'wren' }, payload_json: '{"actor_id":"usr_1"}' }).text, 'Context cleared · owen@test');
assert.equal(describe({ kind: 'agent_context_cleared', agent_id: 'agt_1', names: { agt_1: 'owen@test', usr_1: 'wren' }, payload_json: '{"actor_id":"usr_1"}' }).details, 'requested by wren');
assert.equal(describe({ kind: 'agent_context_compacted', agent_id: 'agt_1', names: { agt_1: 'owen@test' }, payload_json: '{"pre_tokens":"180k","post_tokens":"40k"}' }).text, 'Context compacted · owen@test');

// Every other kind keeps its own label.
assert.equal(describe({ kind: 'agent_stopped', payload_json: '{}' }).text, 'Agent stopped');
console.log('system events describe test passed');
