// The Usages page's pure rules (web/js/pages/usages/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'usages', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { toolOf, isSwitchable, sortProviders, providerStatus, profileKey, affectedText, impactText, canEnable, providerConfigured, movedOrder, usageSettingsBody, refreshMessage, WARMUP_MODES, warmupBody, warmupDisabled, tokenStatesPending, tokenStateLine, alertKind, ollamaBody, flowText, pollDelay, flowExpired, profileStatusLine, staleReason, switchView };', context);
const plain = value => JSON.parse(JSON.stringify(value));
// What the vm returns carries its own Object and Array, which deepStrictEqual tells apart: results are copied into this realm.
const m = Object.fromEntries(Object.entries(context.api).map(([name, value]) => [name, typeof value === 'function' ? (...args) => plain(value(...args)) : plain(value)]));

assert.deepEqual([m.toolOf('claude-code'), m.toolOf('codex'), m.isSwitchable('claude'), m.isSwitchable('mistral')], ['claude', 'codex', true, false]);
assert.deepEqual(m.sortProviders([{ provider_name: 'b', order_number: 2 }, { provider_name: 'a' }, { provider_name: 'c', order_number: 1 }]).map(item => item.provider_name), ['a', 'c', 'b'], 'providers go by their order number, a missing one first');
assert.deepEqual([m.providerStatus({ available: false }), m.providerStatus({ available: true, enabled: true }), m.providerStatus({ available: true })], ['Unavailable', 'Enabled', 'Not configured']);
assert.equal(m.profileKey('codex', { profile_name: 'main' }), 'codex/main');
assert.equal(m.profileKey('codex', {}), 'codex/local');

// Which agents a switch touches, as the card line and as the confirm's text.
const agents = [{ id: 'milo@x', running: true }, { id: 'ada@x' }];
assert.deepEqual([m.affectedText(agents), m.affectedText([])], ['Switch affects: milo@x (running), ada@x', 'No agents use this tool']);
assert.deepEqual([m.impactText(agents), m.impactText([])], ['Affected agents: milo@x, ada@x.', 'No affected agents.']);

// A provider can be switched on once it has credentials, or if the server reads it itself.
assert.deepEqual([m.canEnable({ provider_name: 'mistral' }, false), m.canEnable({ provider_name: 'mistral' }, true), m.canEnable({ provider_name: 'codex' }, false), m.canEnable({ provider_name: 'mistral', enabled: true }, false)], [false, true, true, true]);
assert.deepEqual([m.providerConfigured({ cookie_configured: true }), m.providerConfigured({ api_key_configured: true }), m.providerConfigured({})], [true, true, false]);
const three = [{ provider_name: 'a' }, { provider_name: 'b' }, { provider_name: 'c' }];
assert.deepEqual([plain(m.movedOrder(three, 1, -1)), plain(m.movedOrder(three, 1, 1)), m.movedOrder(three, 0, -1), m.movedOrder(three, 2, 1)], [['b', 'a', 'c'], ['a', 'c', 'b'], null, null]);

assert.deepEqual(plain(m.usageSettingsBody({ interval: '90', display: 'used', warning: '25', critical: '5' })), { refresh_interval_sec: 90, percent_display: 'used', alert_warning_remaining: 25, alert_critical_remaining: 5 });
assert.equal(m.refreshMessage({ status: 429, retryAfter: 30, message: 'slow down' }), 'Refreshed moments ago; try again in 30s');
assert.equal(m.refreshMessage({ status: 500, message: 'broken' }), 'broken');
assert.deepEqual(m.WARMUP_MODES.map(([value]) => value), ['off', 'force', 'scheduled', 'smart']);
assert.deepEqual(plain(m.warmupBody('scheduled', {})), { warmup_mode: 'scheduled', warmup_schedule: '0 9 * * *' });
assert.deepEqual(plain(m.warmupBody('scheduled', { warmup_schedule: '5 4 * * 1' })), { warmup_mode: 'scheduled', warmup_schedule: '5 4 * * 1' });
assert.deepEqual(plain(m.warmupBody('off', { warmup_schedule: '5 4 * * 1' })), { warmup_mode: 'off', warmup_schedule: '5 4 * * 1' }, 'turning it off keeps the schedule');
assert.deepEqual([m.warmupDisabled({}), m.warmupDisabled({ warmup_available: true }), m.warmupDisabled({ warmup_mode: 'force' })], [true, false, false]);

// Token auto-refresh: pending until the server has converted each tool.
assert.deepEqual([m.tokenStatesPending({ settings: { token_auto_refresh_state: { claude: { status: 'pending' } } } }), m.tokenStatesPending({ settings: { token_auto_refresh_state: { claude: { status: 'migrated' } } } }), m.tokenStatesPending(null)], [true, false, false]);
assert.deepEqual([m.tokenStateLine('claude', { status: 'migrated' }), m.tokenStateLine('codex', { status: 'waiting', reason: 'no docker' }), m.tokenStateLine('codex', { status: 'weird' }), m.tokenStateLine('claude', undefined)], ['Claude: migrated', 'Codex: waiting: no docker', 'Codex: off', 'Claude: off']);
assert.equal(m.alertKind('rejected<x>'), 'rejectedx', 'a server value only becomes part of a class name as letters and underscores');
assert.deepEqual(plain(m.ollamaBody('cookie', ' ', 'k')), { auth_mode: 'cookie', api_key: 'k' });
assert.deepEqual(plain(m.ollamaBody('auto', 'c=1', '')), { auth_mode: 'auto', cookie: 'c=1' });

// GitHub sign-in.
assert.deepEqual([m.flowText({ user_code: 'AB-12' }), m.flowText({ user_code: 'AB-12', status: 'pending' })], ['Code: AB-12 · Waiting for GitHub', 'Code: AB-12 · pending']);
assert.deepEqual([m.pollDelay({ retry_after: 9 }, { interval: 2 }), m.pollDelay({ interval: 3 }, {}), m.pollDelay({}, { interval: 2 }), m.pollDelay({}, {}), m.pollDelay({ interval: 0 }, { interval: 0 })], [9, 3, 2, 5, 5]);
assert.deepEqual([m.flowExpired({ expires_at: '2026-01-01T00:00:10Z' }, Date.parse('2026-01-01T00:00:00Z')), m.flowExpired({ expires_at: '2026-01-01T00:00:10Z' }, Date.parse('2026-01-01T00:00:10Z'))], [false, true]);

// A profile's status line and Switch button.
const clock = date => `T${date.getUTCMinutes()}`;
assert.equal(m.profileStatusLine({ status: 'ok', last_refresh_at: '2026-01-01T10:07:00Z' }, clock), 'Updated T7');
assert.equal(m.profileStatusLine({ status: 'token_expired' }, clock), 'Stale – token expired');
assert.equal(m.profileStatusLine({ status: 'token_expired', last_refresh_at: '2026-01-01T10:07:00Z' }, clock), 'Stale – token expired · Updated T7', 'the reason and the update time are separate');
assert.equal(m.profileStatusLine({ stale: true, status: 'ok' }, clock), 'Stale snapshot');
assert.equal(m.profileStatusLine({ status: 'not_configured' }, clock), 'not configured');
assert.equal(m.profileStatusLine({}, clock), 'No snapshot');
assert.equal(m.staleReason({ status: 'backing_off', retry_at: '2026-01-01T10:07:00Z' }, clock), 'Rate limited by the provider, retrying at T7');
assert.equal(m.staleReason({ status: 'backing_off' }, clock), 'Rate limited by the provider', 'no retry time, none claimed');
assert.equal(m.staleReason({ status: 'fetch_error', error: 'Claude usage endpoint returned HTTP 429.' }, clock), 'Rate limited by the provider');
assert.equal(m.staleReason({ status: 'fetch_error', error: 'timeout' }, clock), 'Last fetch failed');
assert.equal(m.staleReason({ status: 'token_expired' }, clock), '');
const base = { switchable: true, enabled: true, owner: true, configured: true, profile: { active: false }, hasActive: true, eligible: { eligible: true } };
assert.deepEqual(plain(m.switchView(base)), { hidden: false, disabled: false, title: '' });
assert.equal(m.switchView({ ...base, profile: { active: true } }).hidden, true, 'the active profile has no Switch');
for (const change of [{ switchable: false }, { enabled: false }, { owner: false }, { configured: false }]) assert.equal(m.switchView({ ...base, ...change }).hidden, true, JSON.stringify(change));
assert.deepEqual(plain(m.switchView({ ...base, hasActive: false })), { hidden: false, disabled: true, title: 'Import the current login first' });
assert.deepEqual(plain(m.switchView({ ...base, eligible: { eligible: false, reason: 'Busy' } })), { hidden: false, disabled: true, title: 'Busy' });
assert.deepEqual(plain(m.switchView({ ...base, eligible: { eligible: false, absent: true } })), { hidden: false, disabled: false, title: '' }, 'an absent tool can still be switched');
console.log('usages model tests passed');
