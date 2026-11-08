// The folder field's home folder: what a fill puts in, when it may, and what it never touches. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'workspace-folder.js'), 'utf8');
const context = { Event: class { constructor(type) { this.type = type; } } };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { folderStart, fillFolderStart, watchTypedOverStart, pathTypedOverStart };', context);
const { folderStart, fillFolderStart, watchTypedOverStart, pathTypedOverStart } = context.api;

assert.equal(folderStart('/Users/me'), '/Users/me/');
assert.equal(folderStart('/Users/me/'), '/Users/me/', 'one trailing slash, not two');
assert.equal(folderStart('/'), '/');
for (const unusable of ['', undefined, null, 'relative/path', 42, {}]) assert.equal(folderStart(unusable), '', `no start for ${JSON.stringify(unusable)}`);

const field = value => {
  const listeners = {}, events = [];
  const input = { value, selection: null, addEventListener: (type, handler) => { (listeners[type] ||= []).push(handler); }, setSelectionRange(a, b) { this.selection = [a, b]; },
    dispatchEvent(event) { events.push(event.type); for (const handler of listeners[event.type] || []) handler(event); } };
  // The person types: the value changes and the field says so.
  const type = text => { input.value += text; input.dispatchEvent(new context.Event('input')); };
  return { input, events, type };
};

// A fill goes into an empty field only, with the caret after the start, and tells the page.
let page = field('');
assert.equal(fillFolderStart(page.input, '/Users/me/'), true);
assert.equal(page.input.value, '/Users/me/', 'an empty field gets the home folder');
assert.deepEqual(page.input.selection, [10, 10], 'the caret is after the slash');
assert.deepEqual(page.events, ['input'], 'the page is told, so what it keeps in step with the field follows');
page = field('/srv/work/repo');
assert.equal(fillFolderStart(page.input, '/Users/me/'), false);
assert.equal(page.input.value, '/srv/work/repo', 'typed text is never replaced');
assert.deepEqual(page.events, []);
page = field('');
assert.equal(fillFolderStart(page.input, ''), false);
assert.equal(page.input.value, '', 'without a known home there is nothing to put in');

// Typing a whole path after the start replaces the start; a relative one continues after it; editing the start makes it the person's text.
assert.equal(pathTypedOverStart('/home/ana//srv/work', '/home/ana/'), '/srv/work');
assert.equal(pathTypedOverStart('/home/ana/code', '/home/ana/'), null, 'a relative rest is not a whole path');
assert.equal(pathTypedOverStart('/home/ana/', '/home/ana/'), null, 'nothing typed yet');
assert.equal(pathTypedOverStart('/home/ana//', ''), null, 'no start, nothing to step aside');
const armed = () => { const p = field(''); const arm = watchTypedOverStart(p.input); fillFolderStart(p.input, '/home/ana/', arm); return p; };
page = armed();
page.type('/');
assert.equal(page.input.value, '/', 'the first typed "/" replaces the start');
page.type('srv/work');
assert.equal(page.input.value, '/srv/work', 'and what follows is typed after it, with the start gone for good');
page = armed(); page.type('code/repo');
assert.equal(page.input.value, '/home/ana/code/repo', 'a relative path continues after the start');
page.type('/more');
assert.equal(page.input.value, '/home/ana/code/repo/more', 'a "/" later in the text is just a separator');
page = armed();
page.input.value = '/hom/'; page.input.dispatchEvent(new context.Event('input')); // the start was cut into
page.type('/x');
assert.equal(page.input.value, '/hom//x', 'once the start is edited it is the person\'s text and is left alone');
console.log('workspace folder test passed');
