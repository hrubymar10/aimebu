// The Machines page's pure rules (web/js/pages/machines/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'machines', 'model.js'), 'utf8');
const context = vm.createContext({ Date });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { cardView, warmLine };', context);
const view = (machine, owner = true) => JSON.parse(JSON.stringify(context.api.cardView(machine, owner)));

const host = { name: 'This computer', is_host: true, approved: true, connected: true, supervisor_version: '1.2' };
assert.deepEqual(view(host), { title: 'This computer · Host', status: 'Connected', detail: 'Supervisor 1.2', approve: false, revoke: false, stop: true, locked: false }, 'the host cannot be revoked');
assert.deepEqual(view({ name: 'Box', approved: false }), { title: 'Box', status: 'Pending approval', detail: '', approve: true, revoke: true, stop: false, locked: false }, 'a pending machine is approved, not stopped');
assert.deepEqual(view({ name: 'Box', approved: true, connected: false }), { title: 'Box', status: 'Offline', detail: '', approve: false, revoke: true, stop: true, locked: false });
assert.deepEqual(view({ name: 'Old', approved: true, revoked_at: '2026-01-01T00:00:00Z' }), { title: 'Old', status: 'Revoked', detail: '', approve: false, revoke: false, stop: false, locked: false }, 'a revoked machine offers nothing');
assert.equal(view(host, false).locked, true, 'an admin who is not the owner gets the pairing controls off');
assert.match(view({ name: 'Box', approved: true, supervisor_version: '2.0', last_seen: '2026-02-03T04:05:06Z' }).detail, /^Supervisor 2\.0 · Last seen .+/);
console.log('machines model tests passed');

// Warm agents: the card says how many have a live process and what the machine's processes use, with the memory labelled as not per agent.
const warmView = warm => view({ ...host, warm }).detail;
assert.equal(warmView(undefined), 'Supervisor 1.2', 'a machine that keeps nothing warm says nothing');
assert.equal(warmView({ idle: 2, busy: 0, started: 3, reused: 4, rss_bytes: 0, container_wide_bytes: 0 }), 'Supervisor 1.2 · Warm agents: 2 warm');
assert.equal(warmView({ idle: 1, busy: 1, started: 3, reused: 4, rss_bytes: 480 << 20, container_wide_bytes: 3 * (1 << 30) }),
  'Supervisor 1.2 · Warm agents: 1 warm, 1 working · 480 MB in warm processes · 3.0 GB container memory, all containers');
assert.equal(warmView({ idle: 1, busy: 0, started: 1, reused: 0, rss_bytes: 0, container_wide_bytes: 0, unconfirmed_ping_ends: 2 }), 'Supervisor 1.2 · Warm agents: 1 warm · 2 unconfirmed ping ends');
console.log('machines model: warm ok');
