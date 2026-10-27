// The version line of a harness card: the version, or "unknown" with the reason a container launcher gave. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'harness-version.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { versionLabel, harnessName };', context);
const { versionLabel, harnessName } = context.api;

assert.equal(versionLabel({ version: '2.1.310 (Claude Code)' }), 'Version 2.1.310 (Claude Code)');
assert.equal(versionLabel({ version: '1.19.4', version_problem: 'stale' }), 'Version 1.19.4', 'a known version needs no reason');
assert.equal(versionLabel({}), 'Version unknown');
assert.equal(versionLabel({ version: '', version_problem: "Error: Directory '/x' is not mounted in the harness container." }),
  "Version unknown (Error: Directory '/x' is not mounted in the harness container.)");
// The harness reads the same as in v1 and bus_register: Claude Code is claude-code, the other tools keep their key.
assert.equal(harnessName('claude'), 'claude-code');
assert.equal(harnessName('claude-code'), 'claude-code');
assert.equal(harnessName('codex'), 'codex');
assert.equal(harnessName(''), '');
assert.equal(harnessName(undefined), '');
console.log('harness version tests passed');
