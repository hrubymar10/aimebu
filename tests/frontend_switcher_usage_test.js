#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const appPath = path.join(__dirname, '..', 'frontend', 'app.js');
const source = fs.readFileSync(appPath, 'utf8');

function extractFunction(name) {
  const start = source.indexOf(`function ${name}`);
  assert.notStrictEqual(start, -1, `${name} not found`);
  const bodyStart = source.indexOf('{', start);
  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}') depth--;
    if (depth === 0) return source.slice(start, i + 1);
  }
  throw new Error(`${name} did not terminate`);
}

const context = {
  console,
  usageProviders: [],
};

vm.createContext(context);
vm.runInContext([
  extractFunction('providerForSwitcherTool'),
  extractFunction('usageProviderHasProfile'),
  extractFunction('schedulePostSwitchUsageRefresh'),
].join('\n'), context);

(async () => {
  assert.strictEqual(context.providerForSwitcherTool('claude'), 'claude-code');
  assert.strictEqual(context.providerForSwitcherTool('codex'), 'codex');

  context.usageProviders = [{ provider_name: 'claude-code', profiles: [] }];
  let calls = 0;
  context.setTimeout = function (fn) {
    fn();
    return 1;
  };
  context.loadUsages = function () {
    calls++;
    if (calls === 2) {
      context.usageProviders[0].profiles = [{ profile_name: 'main' }];
    }
    return Promise.resolve();
  };
  context.schedulePostSwitchUsageRefresh('claude', 'main', 0);
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.strictEqual(calls, 2, 'post-switch refresh retries until the target profile appears');

  context.usageProviders = [{ provider_name: 'claude-code', profiles: [] }];
  calls = 0;
  context.loadUsages = function () {
    calls++;
    return Promise.resolve();
  };
  context.schedulePostSwitchUsageRefresh('claude', 'missing', 0);
  for (let i = 0; i < 30; i++) await Promise.resolve();
  assert.strictEqual(calls, 12, 'post-switch refresh stops at the retry cap');

  console.log('frontend switcher usage tests passed');
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
