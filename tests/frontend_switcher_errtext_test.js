#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');
const start = source.indexOf('function switcherErrText');
assert.notStrictEqual(start, -1, 'switcherErrText not found');
const bodyStart = source.indexOf('{', start);
let depth = 0;
let end = bodyStart;
for (; end < source.length; end++) {
  if (source[end] === '{') depth++;
  if (source[end] === '}' && --depth === 0) break;
}
const context = {};
vm.createContext(context);
vm.runInContext(source.slice(start, end + 1), context);

const text = context.switcherErrText;
assert.strictEqual(text(new Error('HTTP 409: login pending for claude/main')), 'login pending for claude/main');
assert.strictEqual(text(new Error('HTTP 400: migration is not enabled')), 'migration is not enabled');
assert.strictEqual(text(new Error('network down')), 'network down');
assert.strictEqual(text('plain'), 'plain');
assert.strictEqual(text(null), 'null');
console.log('frontend_switcher_errtext_test: ok');
