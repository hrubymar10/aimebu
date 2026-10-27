// What stops the v1 import wizard from going on: every problem of a page is found at once, in the same words as before. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'import-problems.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { projectProblems, peopleProblems, dmProblems, problemSummary, escapeSelector };', context);
const { projectProblems, peopleProblems, dmProblems, problemSummary, escapeSelector } = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(escapeSelector('a b"c'), 'a\\ b\\"c', 'without CSS.escape a selector-unsafe character is escaped');

// Projects: name and absolute folder, every project and both fields at once; an existing project is left alone.
assert.deepEqual(plain(projectProblems({ ok: { name: 'Ok', folder: '/srv/ok' }, existing: { existing_id: 'p1', name: '', folder: '' } })), []);
assert.deepEqual(plain(projectProblems({ a: { name: 'A', folder: '' }, b: { name: '  ', folder: 'rel/path' }, c: { name: 'C', folder: '/ok' } })), [
  { message: 'Project "A" needs its workspace folder: an absolute path such as /Users/you/code/a.', selector: 'input[data-project-folder="a"]' },
  { message: 'Project "b" needs a name.', selector: 'input[data-project-name="b"]' },
  { message: 'Project "b" needs its workspace folder: an absolute path such as /Users/you/code/b.', selector: 'input[data-project-folder="b"]' },
], 'a project with no name and no folder reports both, under its alias');
assert.deepEqual(plain(projectProblems(undefined)), []);

// People: a valid, free, unique login for each new account; the second asker of a login is the one flagged.
assert.deepEqual(plain(peopleProblems({ ann: { login: 'ann-2' }, bob: { login: 'bobby' } }, ['sam'])), []);
const people = plain(peopleProblems({ a: { login: 'x' }, b: { login: 'sam' }, c: { login: 'twin' }, d: { login: 'twin' }, e: { login: 'fine' }, f: {} }, ['Sam', 'owner']));
assert.deepEqual(people.map(problem => problem.selector), ['input[data-new-login="a"]', 'input[data-new-login="b"]', 'input[data-new-login="d"]', 'input[data-new-login="f"]'], 'a too short, a taken (whatever its case), a repeated and a missing login are all reported, each at its own field');
assert.match(people[0].message, /The login for a must be 3 to 32 characters/);
assert.match(people[1].message, /The login "sam" is already an account here; map b to it instead/);
assert.match(people[2].message, /Two people would both be "twin"/);
assert.match(people[3].message, /The login for f must be 3 to 32 characters/);

// Direct messages: each one that is imported needs a project.
const rooms = [{ id: 'dm:a:b', dm: true }, { id: 'dm:c:d', dm: true }, { id: 'dm:e:f', dm: true }, { id: 'general', dm: false }, { id: 'dm:g:h', dm: true }];
const specs = { 'dm:a:b': { project: 'p' }, 'dm:c:d': { project: '' }, 'dm:e:f': { skip: true, project: '' }, 'dm:g:h': {} };
assert.deepEqual(plain(dmProblems(rooms, specs, room => `chat ${room.id}`)), [
  { message: 'Choose a project for the chat "chat dm:c:d", or turn it off.', selector: '[data-dm-room="dm\\:c\\:d"]' },
  { message: 'Choose a project for the chat "chat dm:g:h", or turn it off.', selector: '[data-dm-room="dm\\:g\\:h"]' },
], 'a chat with no project is reported, a skipped one and one with a project are not');
assert.deepEqual(plain(dmProblems(undefined, undefined, () => '')), []);

assert.equal(problemSummary(1), 'One thing needs fixing before you go on:');
assert.equal(problemSummary(3), '3 things need fixing before you go on:');
console.log('import problems test passed');
