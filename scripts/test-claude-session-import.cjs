// The role each session of a bulk import starts with is a pure function of the listing, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'claude-import', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { presetRoles, usableTool, defaultModel, machineLabel, finalStatus };', context);
const presetRoles = (sessions, roles) => Array.from(context.api.presetRoles(sessions, roles));

// Sessions may share a folder and each starts as a worker by default.
assert.deepEqual(presetRoles([{ cwd: '/a' }, { cwd: '/a' }, { cwd: '/b' }], ['worker', 'reviewer']), ['worker', 'worker', 'worker']);
assert.deepEqual(presetRoles([{ cwd: '/a' }, { cwd: '/a' }], ['helper', 'critic']), ['helper', 'helper']);
assert.deepEqual(presetRoles([], ['worker']), []);
const plain = value => JSON.parse(JSON.stringify(value));
const { usableTool, defaultModel, machineLabel, finalStatus } = context.api;
// Only a native Claude, or a Docker one that shares the host's Claude home, can continue a session.
assert.deepEqual([{ tool: 'claude', available: true }, { tool: 'claude', available: true, containerized: true, shares_claude_home: true }, { tool: 'claude', available: true, containerized: true }, { tool: 'claude', available: false }, { tool: 'codex', available: true }].map(tool => !!usableTool(tool)), [true, true, false, false, false]);
assert.equal(defaultModel({ models: [{ id: 'a' }, { id: 'b', default: true }] }), 'b');
assert.equal(defaultModel({ models: [{ id: 'a' }] }), 'a');
assert.equal(defaultModel({}), '');
assert.deepEqual([machineLabel({ name: 'Box', is_host: true, connected: true }), machineLabel({ name: 'Away' })], ['Box · Host', 'Away · Offline']);
assert.equal(finalStatus(2, 3, true), '2 of 3 sessions started; the project\'s agent or spawn-rate limit was reached, import the rest later.');
assert.equal(finalStatus(1, 1, false), '1 of 1 session started.');
console.log('claude session import ok');
