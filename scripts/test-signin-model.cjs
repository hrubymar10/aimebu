// The sign-in page's pure rules (web/js/pages/signin/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'signin', 'model.js'), 'utf8');
const context = vm.createContext({ URLSearchParams, String });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { inviteToken, cardView, INVALID_LEAD, THROTTLED_LEAD };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(m.inviteToken('#invite=abc%20d'), 'abc d');
assert.equal(m.inviteToken('invite=x&other=1'), 'x');
assert.equal(m.inviteToken(''), '');
assert.equal(m.inviteToken('#room=1'), '');

const join = { kind: 'join', project_name: 'Alpha', role: 'operator' };
assert.deepEqual(plain(m.cardView(join, '')), { lead: 'You are invited to Alpha as an operator. Choose a login and a password to create your account.', fields: true, loginLabel: 'Login', loginRequired: true, readOnly: false, submitText: 'Create account and sign in', switchText: 'I already have an account', switchHidden: false });
assert.match(m.cardView({ ...join, role: 'owner' }, '').lead, /as an owner\./);
assert.match(m.cardView({ ...join, role: 'viewer' }, '').lead, /as a viewer\./);
assert.match(m.cardView({ ...join, role: 'auditor' }, '').lead, /as auditor\./);

const reset = m.cardView({ kind: 'reset', login: 'sam' }, '');
assert.deepEqual(plain(reset), { lead: 'Choose a new password for sam.', fields: true, loginLabel: 'Account', loginRequired: false, readOnly: true, loginValue: 'sam', submitText: 'Set password and sign in', switchText: 'I already have an account', switchHidden: true });

assert.deepEqual(plain(m.cardView({ error: { status: 404 } }, '')), { lead: m.INVALID_LEAD, fields: false, loginLabel: 'Login', loginRequired: true, readOnly: false, submitText: '', switchText: 'Sign in', switchHidden: false });
assert.equal(m.cardView({ error: { status: 429 } }, '').lead, m.THROTTLED_LEAD);
assert.equal(m.cardView({ error: { status: 429 } }, '').switchHidden, false);

const done = m.cardView(join, 'newcomer');
assert.deepEqual([done.submitText, done.readOnly, done.loginValue, done.switchHidden, done.loginRequired], ['Sign in', true, 'newcomer', true, false]);
assert.deepEqual([m.cardView(null, '').lead, m.cardView(null, '').submitText], ['', '']);
console.log('signin-model: ok');
