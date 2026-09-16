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
  usageSwitcherEnabled: false,
  usageGroupBy: 'providers',
  switcherInFlight: {},
  EXPIRY_ICON_OK: '',
  EXPIRY_ICON_SOON: '',
  EXPIRY_ICON_EXPIRED: '',
  esc: String,
  usageFailureMessage: () => '',
  renderUsageWindowRow: () => '<div class="window"></div>',
  renderCreditsRow: () => '',
  usageErrorLine: () => '',
  formatRelativeAge: () => 'just now',
  statusLabel: (status) => String(status || 'unknown'),
  usageProviderIconClass: () => 'icon',
  usageProviderIcon: () => '<i></i>',
  renderUsagesGroupButton: () => {},
  renderUsagesSidebar: () => {},
  localStorage: {
    setItem: (key, value) => { context.savedPreference = [key, value]; },
  },
};

vm.createContext(context);
vm.runInContext([
  extractFunction('switcherToolForProvider'),
  extractFunction('providerForSwitcherTool'),
  extractFunction('usageProviderHasProfile'),
  extractFunction('schedulePostSwitchUsageRefresh'),
  extractFunction('usageSidebarGroups'),
  extractFunction('toggleUsagesGroupBy'),
  extractFunction('renderUsageProfiles'),
  extractFunction('renderUsageProviderTile'),
  extractFunction('windowLabel'),
  extractFunction('switcherProfileUsage'),
  extractFunction('renderSwitcherSection'),
  extractFunction('renderSwitcherProfileRow'),
].join('\n'), context);

(async () => {
  assert.strictEqual(context.providerForSwitcherTool('claude'), 'claude-code');
  assert.strictEqual(context.providerForSwitcherTool('codex'), 'codex');
  assert.strictEqual(context.windowLabel('monthly'), 'Monthly');

  const grouped = context.usageSidebarGroups([
    { provider_name: 'codex', profiles: [
      { profile_name: 'codex-main', active: true },
      { profile_name: 'codex-backup', active: false },
    ] },
    { provider_name: 'claude-code', profiles: [
      { profile_name: 'claude-main', active: true },
      { profile_name: 'claude-work', active: false },
      { profile_name: 'claude-backup', active: false },
    ] },
  ], 'active');
  assert.strictEqual(grouped.primary.length, 2, 'active grouping keeps one top group per provider');
  assert.strictEqual(grouped.primary[0].profiles[0].profile_name, 'codex-main');
  assert.strictEqual(grouped.secondary.length, 2, 'providers with inactive profiles remain below the divider');
  assert.deepStrictEqual(Array.from(grouped.secondary[1].profiles, (p) => p.profile_name), ['claude-work', 'claude-backup']);

  const providerGrouped = context.usageSidebarGroups([{ provider_name: 'codex', profiles: [
    { profile_name: 'main', active: true },
    { profile_name: 'backup', active: false },
  ] }], 'providers');
  assert.strictEqual(providerGrouped.primary[0].profiles.length, 2, 'provider grouping remains unchanged');
  assert.strictEqual(providerGrouped.secondary.length, 0, 'provider grouping has no split section');

  context.toggleUsagesGroupBy();
  assert.strictEqual(context.usageGroupBy, 'active', 'group toggle switches to active-first mode');
  assert.deepStrictEqual(Array.from(context.savedPreference), ['aimebu_usages_group_by', 'active'], 'group toggle persists locally');
  assert(source.includes('viewBox="0 0 16 16" fill="currentColor"'), 'group icon is an inline currentColor SVG');

  context.usageProviders = [{
    provider_name: 'codex',
    label: 'Codex',
    profiles: [{ profile_name: 'local', active: true, has_credentials: true }],
  }];
  let rendered = context.renderUsageProviderTile(context.usageProviders[0], context.usageProviders[0].profiles);
  assert(rendered.includes('switcher-tile-profile'), 'one implicit profile uses the shared profile-row renderer');
  assert(rendered.includes('>local<'), 'implicit profile identity is visible');
  assert(!rendered.includes('>SWITCH<'), 'switcher-off hides mutation controls only');

  context.usageProviders[0].profiles = [
    { profile_name: 'main', active: true, has_credentials: true },
    { profile_name: 'backup', active: false, has_credentials: true },
  ];
  rendered = context.renderUsageProviderTile(context.usageProviders[0], context.usageProviders[0].profiles);
  assert.strictEqual((rendered.match(/<div class="switcher-tile-profile/g) || []).length, 2, 'two profiles use two shared rows');
  assert(!rendered.includes('>SWITCH<'), 'profile data remains visible when switching is disabled');
  context.usageSwitcherEnabled = true;
  rendered = context.renderUsageProviderTile(context.usageProviders[0], context.usageProviders[0].profiles);
  assert(rendered.includes('data-profile="backup"'), 'switcher-on adds the inactive-profile control');
  rendered = context.renderUsageProviderTile(context.usageProviders[0], [context.usageProviders[0].profiles[1]]);
  assert(!rendered.includes('>main<'), 'a split provider tile excludes the active profile from the lower group');
  assert(rendered.includes('>backup<'), 'a split provider tile keeps the requested inactive profile');

  context.usageProviders[0].switch_eligible = false;
  context.usageProviders[0].switch_absent = true;
  context.usageProviders[0].switch_ineligible_reason = 'no live credentials';
  rendered = context.renderUsageProviderTile(context.usageProviders[0], context.usageProviders[0].profiles);
  assert(rendered.includes('data-profile="backup"'), 'absent active credentials still allow switching away in the usage tile');
  let switcherSection = context.renderSwitcherSection(
    'codex',
    context.usageProviders[0].profiles,
    { eligible: false, absent: true, reason: 'no live credentials' },
    false,
    'switch'
  );
  assert(switcherSection.includes('data-profile="backup"'), 'absent active credentials still allow switching away in the switcher panel');

  context.usageProviders[0].switch_absent = false;
  rendered = context.renderUsageProviderTile(context.usageProviders[0], context.usageProviders[0].profiles);
  assert(!rendered.includes('data-profile="backup"'), 'normal ineligibility disables switching in the usage tile');
  switcherSection = context.renderSwitcherSection(
    'codex',
    context.usageProviders[0].profiles,
    { eligible: false, absent: false, reason: 'invalid credentials' },
    false,
    'switch'
  );
  assert(!switcherSection.includes('data-profile="backup"'), 'normal ineligibility disables switching in the switcher panel');
  context.usageSwitcherEnabled = false;

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
