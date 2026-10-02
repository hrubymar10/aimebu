#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');

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
  esc: String,
  usagesProviderRows: { innerHTML: '' },
  usageProviders: [],
  copilotLoginState: { status: 'disconnected', enterpriseHost: '' },
  ollamaCookieEditorOpen: false,
  mistralCookieEditorOpen: false,
  usageFailureMessage: () => '',
  saveUsageProviderOrder: (order) => { context.savedOrder = Array.from(order); },
};
vm.createContext(context);
vm.runInContext([
  extractFunction('renderUsageProviderRows'),
  extractFunction('usageProviderOrderControls'),
  extractFunction('usageProviderToggle'),
  extractFunction('renderCopilotProviderRow'),
  extractFunction('renderMistralProviderRow'),
  extractFunction('renderOllamaProviderRow'),
  extractFunction('moveUsageProvider'),
].join('\n'), context);

context.usageProviders = [
  { provider_name: 'codex', label: 'Codex', enabled: true, available: true },
  { provider_name: 'claude-code', label: 'Claude Code', enabled: false, available: true },
  { provider_name: 'github-copilot', label: 'GitHub Copilot', enabled: false, available: true },
  { provider_name: 'mistral', label: 'Mistral', enabled: false, available: true },
  { provider_name: 'ollama-cloud', label: 'Ollama Cloud', enabled: false, available: true },
];
context.renderUsageProviderRows(context.usageProviders);
const html = context.usagesProviderRows.innerHTML;

for (const name of ['codex', 'claude-code']) {
  assert(html.includes(`data-provider="${name}"`), `${name} toggle carries its provider name`);
}
assert(!html.includes('data-provider=""'), 'no toggle has an empty provider name');
for (const name of ['github-copilot', 'mistral', 'ollama-cloud']) {
  assert(html.includes(`data-provider="${name}" disabled`), `${name} cannot be enabled without credentials`);
}
for (const row of context.usageProviders) {
  assert.strictEqual(
    (html.match(new RegExp(`data-usage-provider-move="${row.provider_name}"`, 'g')) || []).length,
    2,
    `${row.provider_name} has named up and down controls`
  );
}
assert(html.includes('copilot-provider-row') && html.includes('copilot-login-btn'), 'Copilot uses its sign-in row');
assert(html.includes('mistral-provider-row') && html.includes('mistral-cookie-input'), 'Mistral uses its credential row');
assert(html.includes('ollama-provider-row') && html.includes('ollama-auth-mode-select'), 'Ollama uses its credential row');
assert(html.includes('aria-label="Move codex up"') === false, 'display label is preferred for move controls');
assert(
  context.usageProviderOrderControls({ provider_name: 'codex' }, 0, 1).includes('aria-label="Move codex up"'),
  'provider name is the fallback move label'
);

context.usageProviders[4].auth_mode = 'api_key';
context.usageProviders[4].api_key_configured = true;
context.usageProviders[2].token_configured = true;
context.usageProviders[3].cookie_configured = true;
context.ollamaCookieEditorOpen = true;
context.renderUsageProviderRows(context.usageProviders);
for (const name of ['github-copilot', 'mistral', 'ollama-cloud']) {
  assert(
    context.usagesProviderRows.innerHTML.includes(`data-provider="${name}"`)
      && !context.usagesProviderRows.innerHTML.includes(`data-provider="${name}" disabled`),
    `${name} can be enabled with credentials`
  );
}
assert(context.usagesProviderRows.innerHTML.includes('>Signed in</span>'), 'disabled Copilot still shows its saved login');
assert(context.usagesProviderRows.innerHTML.includes('copilot-logout-btn'), 'disabled Copilot can still sign out');
assert(!context.usagesProviderRows.innerHTML.includes('data-provider="github-copilot" checked'), 'disabled Copilot toggle stays off');
assert(
  context.usagesProviderRows.innerHTML.includes('<option value="api_key" selected>API key</option>'),
  'opening configured Ollama credentials preserves the saved API key mode'
);
assert(
  !context.usagesProviderRows.innerHTML.includes('<option value="auto" selected>Auto</option>'),
  'opening configured Ollama credentials does not reset the selector to Auto'
);

context.usageProviders[4].auth_mode = 'cookie';
context.renderUsageProviderRows(context.usageProviders);
assert(
  context.usagesProviderRows.innerHTML.includes('data-provider="ollama-cloud" disabled'),
  'Ollama cookie mode cannot be enabled with only an API key'
);

context.usageProviders[4].auth_mode = 'api_key';
for (const row of context.usageProviders.slice(2)) row.enabled = true;
context.renderUsageProviderRows(context.usageProviders);
for (const name of ['github-copilot', 'mistral', 'ollama-cloud']) {
  assert(context.usagesProviderRows.innerHTML.includes(`data-provider="${name}" checked`), `${name} toggle reflects enabled state`);
}

context.moveUsageProvider('codex', 'down');
assert.deepStrictEqual(context.savedOrder, [
  'claude-code', 'codex', 'github-copilot', 'mistral', 'ollama-cloud',
], 'moving a provider saves an order of API provider names');

console.log('frontend usage provider settings tests passed');
