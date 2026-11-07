// The Users page's pure rules (web/js/pages/users/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'users', 'model.js'), 'utf8');
const context = vm.createContext({ URL });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { ROLES, chosenProject, uniqueAccounts, accountActions, memberRemoval, inviteHint };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.deepEqual(plain(m.ROLES), ['viewer', 'operator', 'owner']);
const projects = [{ id: 'a' }, { id: 'b' }];
assert.equal(m.chosenProject(projects, 'b'), 'b');
assert.equal(m.chosenProject(projects, 'zzz'), 'a', 'a project the person does not manage falls back to the first');
assert.equal(m.chosenProject([], 'a'), '');
assert.deepEqual(plain(m.uniqueAccounts([{ id: 1 }, { id: 2 }, { id: 1, x: 1 }])), [{ id: 1 }, { id: 2 }], 'an account in several projects is listed once');

const owner = { id: 'o', owner: true }, admin = { id: 'a', admin: true }, plainUser = { id: 'u' }, gone = { id: 'g', disabled: true };
const as = (user, globalOwner, currentUser = 'o') => plain(m.accountActions(user, { globalOwner, currentUser }));
assert.deepEqual(as(owner, true), { badge: 'Owner', adminToggle: false, reset: true, deactivate: false }, 'the owner is never deactivated nor made an admin');
assert.deepEqual(as(admin, true), { badge: 'Admin', adminToggle: true, reset: true, deactivate: true });
assert.deepEqual(as(plainUser, true), { badge: '', adminToggle: true, reset: true, deactivate: true });
assert.deepEqual(as(admin, false), { badge: 'Admin', adminToggle: false, reset: false, deactivate: false }, 'an admin does not act on another admin');
assert.deepEqual(as(plainUser, false), { badge: '', adminToggle: false, reset: true, deactivate: true });
assert.deepEqual(as(plainUser, true, 'u'), { badge: '', adminToggle: true, reset: true, deactivate: false }, 'nobody deactivates their own account');
assert.deepEqual(as(gone, true), { badge: '', adminToggle: false, reset: false, deactivate: false }, 'a deactivated account has no actions');

assert.match(m.inviteHint('http://localhost:8080/#invite=x'), /^This address \(localhost\) only works on this computer\./);
assert.match(m.inviteHint('http://127.0.0.1:1/#invite=x'), /\(127\.0\.0\.1\)/);
assert.match(m.inviteHint('http://[::1]:1/#invite=x'), /only works/);
assert.equal(m.inviteHint('https://aimebu.example/#invite=x'), '');
assert.equal(m.inviteHint('not a url'), '', 'a link that does not parse gets no hint');
console.log('users model tests passed');

const removal = (user, members, me) => plain(m.memberRemoval(user, members, me));
const team = [{ id: 'o', role: 'owner' }, { id: 'p', role: 'owner' }, { id: 'v', role: 'viewer' }];
assert.equal(removal(team[0], team, 'o').disabled, true, 'not yourself');
assert.equal(removal(team[1], team, 'o').disabled, false, 'another owner when there are two');
assert.equal(removal(team[2], team, 'o').disabled, false, 'a viewer');
assert.deepEqual(removal(team[0], [team[0], team[2]], 'x'), { disabled: true, reason: 'A project needs at least one owner' }, 'the last owner');
