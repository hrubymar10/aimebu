#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'index.html'), 'utf8');
function extract(name) {
  const start = source.indexOf(`function ${name}`);
  assert.notStrictEqual(start, -1, `${name} missing`);
  const body = source.indexOf('{', start);
  let depth = 0;
  for (let i = body; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}') depth--;
    if (depth === 0) return source.slice(start, i + 1);
  }
  throw new Error(`${name} did not terminate`);
}

const row = {innerHTML: ''};
const context = {
  document: {
    getElementById: id => id === 'usages-account-maintenance-row' ? row : null,
    querySelectorAll: () => [],
  },
  esc: value => String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
  usageTokenAutoRefresh: false,
  usageTokenAutoRefreshAvailable: true,
  usageTokenAutoRefreshState: {},
  usageTokenPoll: null,
  usageWarmupMode: 'off',
  usageWarmupAvailable: false,
  usageWarmupSchedule: '',
  usageClaudeModelFlag: '--model haiku',
  usageCodexModelFlag: '--model gpt-5.6-luna',
  usagePercentDisplay: 'left',
  usagesRefreshInput: null,
  usagesEnvBadge: null,
  api: (_method, _path, payload) => {
    context.saved = payload;
    return {then: fn => { fn({}); return {catch: () => {}}; }};
  },
  loadUsages: () => ({then: fn => fn()}),
};
vm.createContext(context);
vm.runInContext([
  extract('renderAccountMaintenanceRows'),
  extract('renderUsageSettings'),
  extract('saveTokenAutoRefresh'),
].join('\n'), context);

context.renderUsageSettings({
  token_auto_refresh: true,
  claude_auto_refresh: false,
  token_auto_refresh_available: true,
  token_auto_refresh_state: {
    claude: {status: 'migrated'},
    codex: {status: 'waiting', reason: '<stop codex>'},
  },
});
assert(row.innerHTML.includes('id="token-auto-refresh-toggle" checked'), 'new setting drives the toggle');
assert(row.innerHTML.includes('Claude: migrated'), 'Claude migration state shown');
assert(row.innerHTML.includes('Codex: waiting: &lt;stop codex&gt;'), 'waiting reason shown and escaped');
assert(!row.innerHTML.includes('<stop codex>'), 'waiting reason must not inject HTML');
context.renderUsageSettings({
  token_auto_refresh: true,
  token_auto_refresh_available: true,
  token_auto_refresh_state: {claude: {status: 'pending'}, codex: {status: 'n/a'}},
});
assert(row.innerHTML.includes('Claude: pending'), 'pending conversion shown');
assert(row.innerHTML.includes('Codex: n/a'), 'missing profile shown neutrally');
assert(row.innerHTML.includes('aimebu becomes the only thing that refreshes'), 'server dependency described');
assert(html.includes('>Account maintenance</h4>'), 'generic section heading shown');
context.renderUsageSettings({
  token_auto_refresh: true,
  token_auto_refresh_available: false,
  token_auto_refresh_state: {claude: {status: 'migrated'}, codex: {status: 'off'}},
});
assert(row.innerHTML.includes('id="token-auto-refresh-toggle" checked'), 'enabled setting remains visible');
assert(!row.innerHTML.includes('id="token-auto-refresh-toggle" checked disabled'), 'OFF remains available without the executor');
context.saveTokenAutoRefresh(false);
assert.strictEqual(context.saved.token_auto_refresh, false, 'POST uses the new key');
assert(!Object.hasOwn(context.saved, 'claude_auto_refresh'), 'POST does not send the legacy key');
console.log('frontend token auto-refresh tests passed');
