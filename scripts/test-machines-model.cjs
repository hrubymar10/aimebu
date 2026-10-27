// The Machines page's pure rules (web/js/pages/machines/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'machines', 'model.js'), 'utf8');
const context = vm.createContext({ Date });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { cardView };', context);
const view = (machine, owner = true) => JSON.parse(JSON.stringify(context.api.cardView(machine, owner)));

const host = { name: 'This computer', is_host: true, approved: true, connected: true, supervisor_version: '1.2' };
assert.deepEqual(view(host), { title: 'This computer · Host', status: 'Connected', detail: 'Supervisor 1.2', approve: false, revoke: false, stop: true, locked: false }, 'the host cannot be revoked');
assert.deepEqual(view({ name: 'Box', approved: false }), { title: 'Box', status: 'Pending approval', detail: '', approve: true, revoke: true, stop: false, locked: false }, 'a pending machine is approved, not stopped');
assert.deepEqual(view({ name: 'Box', approved: true, connected: false }), { title: 'Box', status: 'Offline', detail: '', approve: false, revoke: true, stop: true, locked: false });
assert.deepEqual(view({ name: 'Old', approved: true, revoked_at: '2026-01-01T00:00:00Z' }), { title: 'Old', status: 'Revoked', detail: '', approve: false, revoke: false, stop: false, locked: false }, 'a revoked machine offers nothing');
assert.equal(view(host, false).locked, true, 'an admin who is not the owner gets the pairing controls off');
assert.match(view({ name: 'Box', approved: true, supervisor_version: '2.0', last_seen: '2026-02-03T04:05:06Z' }).detail, /^Supervisor 2\.0 · Last seen .+/);
console.log('machines model tests passed');
