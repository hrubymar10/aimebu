// The first-run setup's pure rules (web/js/pages/setup/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'setup', 'model.js'), 'utf8');
const context = vm.createContext({ String });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { MIN_PASSWORD, problemText, checkOwner, nextTheme };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(m.problemText({ status: 429, retryAfter: 7, message: 'slow' }), 'Too many attempts. Try again in 7 seconds.');
assert.equal(m.problemText({ status: 429, message: 'slow' }), 'slow');
assert.equal(m.problemText({ status: 401, retryAfter: 7, message: 'nope' }), 'nope');

const password = 'a-long-password-1';
assert.deepEqual(plain(m.checkOwner('  Boss ', password, password)), { name: 'boss', error: '' });
const login = 'Choose a login of 3 to 32 lowercase letters, digits, - or _, starting with a letter.';
for (const bad of ['', 'ab', '1abc', 'has space', 'a'.repeat(33), 'UPPER!']) assert.equal(m.checkOwner(bad, password, password).error, login, `"${bad}"`);
assert.equal(m.checkOwner('a-_9', password, password).error, '', 'dash, underscore and digits are allowed');
assert.equal(m.checkOwner('a'.repeat(32), password, password).error, '');
assert.equal(m.checkOwner('boss', 'short', 'short').error, 'Use a password of at least 12 characters.');
assert.equal(m.checkOwner('boss', 'x'.repeat(m.MIN_PASSWORD), 'x'.repeat(m.MIN_PASSWORD)).error, '');
assert.equal(m.checkOwner('boss', password, 'different-password').error, 'The two passwords do not match.');
assert.equal(m.checkOwner('bad!', 'short', 'x').error, login, 'the login is checked first');

const themes = [['dark', 'Dark'], ['light', 'Light'], ['red-dark', 'Red Dark']];
assert.deepEqual(['ArrowRight', 'ArrowDown'].map(key => m.nextTheme(themes, 'dark', key)), ['light', 'light']);
assert.deepEqual(['ArrowLeft', 'ArrowUp'].map(key => m.nextTheme(themes, 'light', key)), ['dark', 'dark']);
assert.equal(m.nextTheme(themes, 'red-dark', 'ArrowRight'), 'dark', 'wraps forward');
assert.equal(m.nextTheme(themes, 'dark', 'ArrowLeft'), 'red-dark', 'wraps back');
assert.equal(m.nextTheme(themes, 'dark', 'Enter'), null);
console.log('setup-model: ok');
