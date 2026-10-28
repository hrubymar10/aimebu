// The Members section's pure rules (web/js/pages/members/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'members', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { agentState, holderText };', context);
const { agentState, holderText } = context.api;

const state = patch => agentState({ desired: 'active', observed: 'idle', ...patch });
assert.equal(state({}), 'Active · Idle');
assert.equal(state({ observed: 'in_turn' }), 'Active · Working');
assert.equal(state({ desired: 'not_accepting' }), 'Not accepting · Idle');
assert.equal(state({ desired: 'stopped', observed: 'stopped' }), 'Stopped · Stopped');
assert.equal(state({ desired: 'stopped', observed: 'in_turn' }), 'Interrupt requested · Working');
assert.equal(state({ observed: 'interrupting' }), 'Interrupt requested · Interrupting');
assert.equal(state({ observed: 'lost' }), 'Active · Lost');
assert.equal(state({ revoked: true, desired: 'active' }), 'Revoked');
assert.equal(state({ desired: undefined, observed: undefined }), 'Active · Idle');

assert.equal(holderText({ slug: 'zeno', in_this_room: true }), 'held by zeno');
assert.equal(holderText({ slug: 'rex', in_this_room: false }), 'held by rex (another room)');
assert.equal(holderText(null), 'vacant');
assert.equal(holderText(undefined), 'vacant');
console.log('members model: ok');
