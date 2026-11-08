// The onboarding's pure rules (web/js/pages/onboarding/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'onboarding', 'model.js'), 'utf8');
const context = vm.createContext({ String });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { PRINCIPLES, HARNESS_MISSING, NO_MODEL_NOTE, PROJECT_REQUIRED, checkProject, harnessStatusText, howView };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(m.PRINCIPLES.length, 7);
assert.ok(m.PRINCIPLES.every(([head, rest]) => head && rest));

assert.deepEqual(plain(m.checkProject('  Alpha ', ' /srv/alpha ')), { name: 'Alpha', folder: '/srv/alpha', error: '', focus: '' });
assert.deepEqual(plain(m.checkProject('', '/srv')), { name: '', folder: '/srv', error: 'Enter a project name and a workspace folder.', focus: 'name' });
assert.deepEqual(plain(m.checkProject('Alpha', '   ')), { name: 'Alpha', folder: '', error: 'Enter a project name and a workspace folder.', focus: 'folder' });
assert.equal(m.checkProject('', '').focus, 'name');

assert.equal(m.harnessStatusText({ ready: false, ticked: 0, note: '' }), m.HARNESS_MISSING);
assert.equal(m.harnessStatusText({ ready: true, ticked: 0, note: '' }), 'A harness with a model is ready.');
assert.equal(m.harnessStatusText({ ready: false, ticked: 1, note: '' }), 'One harness is ticked: Continue adds it and looks up the models.');
assert.equal(m.harnessStatusText({ ready: false, ticked: 3, note: '' }), '3 harnesses are ticked: Continue adds them and looks up the models.');
assert.equal(m.harnessStatusText({ ready: false, ticked: 0, note: m.NO_MODEL_NOTE }), m.NO_MODEL_NOTE, 'a note stays until something is ready or ticked');
assert.equal(m.harnessStatusText({ ready: true, ticked: 0, note: m.NO_MODEL_NOTE }), 'A harness with a model is ready.');
assert.equal(m.harnessStatusText({ ready: false, ticked: 2, note: m.NO_MODEL_NOTE }).startsWith('2 harnesses'), true);

assert.deepEqual(plain(m.howView(true, true)), { spawnHidden: false, setupHidden: true, noteHidden: true, noteText: '', focus: 'spawn' });
const setup = m.howView(false, true);
assert.deepEqual([setup.spawnHidden, setup.setupHidden, setup.noteHidden, setup.focus], [true, false, false, 'setup']);
assert.match(setup.noteText, /Set one up, then spawn your first agent\.$/);
const ask = m.howView(false, false);
assert.deepEqual([ask.spawnHidden, ask.setupHidden, ask.noteHidden, ask.focus], [true, true, false, 'done']);
assert.match(ask.noteText, /Ask the owner to add one/);
assert.deepEqual([m.howView(true, false).setupHidden, m.howView(true, false).focus], [true, 'spawn']);
console.log('onboarding-model: ok');
