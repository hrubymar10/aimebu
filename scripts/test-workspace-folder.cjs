// The folder field starts at the machine's home folder: what it fills, when, and what it never touches. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'workspace-folder.js'), 'utf8');
const context = { Event: class { constructor(type) { this.type = type; } } };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { folderStart, prefillFolderOnFocus };', context);
const { folderStart, prefillFolderOnFocus } = context.api;

assert.equal(folderStart('/Users/me'), '/Users/me/');
assert.equal(folderStart('/Users/me/'), '/Users/me/', 'one trailing slash, not two');
assert.equal(folderStart('/'), '/');
for (const unusable of ['', undefined, null, 'relative/path', 42, {}]) assert.equal(folderStart(unusable), '', `no start for ${JSON.stringify(unusable)}`);

const field = value => {
  const listeners = {}, events = [];
  const input = { value, selection: null, addEventListener: (type, handler) => { listeners[type] = handler; }, setSelectionRange(a, b) { this.selection = [a, b]; }, dispatchEvent(event) { events.push(event.type); } };
  return { input, focus: () => listeners.focus(), events };
};

let home = '/Users/me';
let page = field('');
prefillFolderOnFocus(page.input, () => home);
page.focus();
assert.equal(page.input.value, '/Users/me/', 'an empty field starts at the home folder');
assert.deepEqual(page.input.selection, [10, 10], 'the caret is after the slash');
assert.deepEqual(page.events, ['input'], 'the page is told so what it keeps in step with the field follows');

page = field('/srv/work/repo');
prefillFolderOnFocus(page.input, () => home);
page.focus();
assert.equal(page.input.value, '/srv/work/repo', 'typed text is never replaced');
assert.deepEqual(page.events, []);

page = field('');
prefillFolderOnFocus(page.input, () => '');
page.focus();
assert.equal(page.input.value, '', 'without a known home the field stays empty');

// The home is read when the field gets focus, not when it is wired: a sign-in after the page loaded still counts.
page = field('');
home = '';
prefillFolderOnFocus(page.input, () => home);
home = '/home/ana';
page.focus();
assert.equal(page.input.value, '/home/ana/');
page = field('');
prefillFolderOnFocus(page.input, '/opt/home');
page.focus();
assert.equal(page.input.value, '/opt/home/', 'a plain value works too');
console.log('workspace folder test passed');
