#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');
const start = source.indexOf('function warmupScheduleValidationError');
assert.notStrictEqual(start, -1, 'warmupScheduleValidationError not found');
const bodyStart = source.indexOf('{', start);
let depth = 0;
let fnSource = '';
for (let i = bodyStart; i < source.length; i++) {
  if (source[i] === '{') depth++;
  if (source[i] === '}') depth--;
  if (depth === 0) {
    fnSource = source.slice(start, i + 1);
    break;
  }
}
assert(fnSource, 'warmupScheduleValidationError did not terminate');

const context = {};
vm.createContext(context);
vm.runInContext(fnSource, context);

assert.strictEqual(context.warmupScheduleValidationError(''), '');
assert.strictEqual(context.warmupScheduleValidationError('0 8 * * *'), '');
assert.strictEqual(context.warmupScheduleValidationError('* * * * *'), '');
assert.strictEqual(context.warmupScheduleValidationError('0 8 * * *\n\n30 14 * * *'), '');
assert(context.warmupScheduleValidationError('0 8 * *'), 'four fields must fail');
assert(context.warmupScheduleValidationError('60 8 * * *'), 'out-of-range minute must fail');
assert(context.warmupScheduleValidationError('*/5 8 * * *'), 'unsupported steps must fail');
assert(context.warmupScheduleValidationError('0 8 * * *\n61 9 * * *').startsWith('Line 2:'), 'multiline error must identify line');

assert(source.includes('id="claude-warmup-mode-select"'), 'warmup mode selector not rendered');
assert(source.includes('<textarea id="claude-warmup-schedule-input"'), 'schedule textarea not rendered');
assert(source.includes('warmup_mode: mode'), 'warmup mode not persisted');

console.log('frontend warmup schedule tests passed');
