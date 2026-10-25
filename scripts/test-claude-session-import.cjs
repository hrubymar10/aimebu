// The role each session of a bulk import starts with is a pure function of the listing, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'claude-session-import.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^import .*$/gm, '').replace(/^export /gm, '').replace(/^class ClaudeSessionImport[\s\S]*$/m, '')
  + '\nglobalThis.api = { presetRoles };', context);
const presetRoles = (sessions, roles) => Array.from(context.api.presetRoles(sessions, roles));

// The first session of a folder works there; the others of that folder review, so one import does not run into the writer rule.
assert.deepEqual(presetRoles([{ cwd: '/a' }, { cwd: '/a' }, { cwd: '/b' }, { cwd: '/a' }, { cwd: '/b' }], ['worker', 'reviewer']),
  ['worker', 'reviewer', 'worker', 'reviewer', 'reviewer']);
// Without a reviewer role everything falls back to the writer role; with neither named, to the first role offered.
assert.deepEqual(presetRoles([{ cwd: '/a' }, { cwd: '/a' }], ['worker']), ['worker', 'worker']);
assert.deepEqual(presetRoles([{ cwd: '/a' }, { cwd: '/a' }], ['helper', 'critic']), ['helper', 'helper']);
assert.deepEqual(presetRoles([], ['worker']), []);
console.log('claude session import ok');
