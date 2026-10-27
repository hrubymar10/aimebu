// The General page's pure rules (web/js/pages/general/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'general', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { THEMES, themeKey, previewKey, knownTheme, themeAttribute };', context);
const m = context.api;

assert.deepEqual(JSON.parse(JSON.stringify(m.THEMES.map(([value]) => value))), ['dark', 'light', 'red-dark', 'red-light', 'blue-dark', 'blue-light', 'green-dark', 'green-light', 'high-contrast-dark', 'high-contrast-light']);
assert.equal(new Set(m.THEMES.map(([value]) => value)).size, m.THEMES.length, 'a theme is offered once');
assert.deepEqual([m.themeKey('u1'), m.previewKey('u1')], ['aimebu2.ui.theme.u1', 'aimebu2.ui.sidebar_preview.u1']);
assert.deepEqual([m.knownTheme('light'), m.knownTheme('gone'), m.knownTheme(''), m.knownTheme(null)], ['light', 'dark', 'dark', 'dark'], 'a theme that is no longer offered falls back to the default');
assert.deepEqual([m.themeAttribute('dark'), m.themeAttribute('red-light')], [null, 'red-light'], 'the default theme is no attribute at all');
console.log('general model tests passed');
