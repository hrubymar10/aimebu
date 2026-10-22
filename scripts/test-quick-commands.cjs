// The composer's "/" commands: which drafts open the list, which commands it offers, and what a pick does. The module is browser code with
// one import, so it runs in a vm with a stand-in list box.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'quick-commands.js'), 'utf8')
  .replace(/^import .*$/gm, '').replace(/^export /gm, '');
class Picker {
  constructor(draft, className, onPick) { this.draft = draft; this.onPick = onPick; this.popup = {}; this.shown = []; }
  show(items) { this.shown = items; }
  close() { this.shown = []; }
  keydown() { return false; }
}
const context = { Picker, setTimeout };
vm.createContext(context);
vm.runInContext(`${source}\nglobalThis.api = { QuickCommands, BUILTIN_COMMANDS };`, context);
const { QuickCommands, BUILTIN_COMMANDS } = context.api;

function fixture({ archive = true, spawn = true } = {}) {
  const listeners = {}, calls = [];
  const draft = {
    value: '', hasFocus: () => true,
    addEventListener(type, handler) { listeners[type] = handler; },
    dispatchEvent(event) { if (event.type === 'input') listeners.input?.(); },
  };
  context.Event = class { constructor(type) { this.type = type; } };
  const commands = new QuickCommands(draft, () => calls.push('archive'), () => calls.push('spawn'), () => archive, () => spawn);
  const offered = value => { draft.value = value; listeners.input(); return Array.from(commands.list.shown, item => item.key); };
  return { commands, draft, calls, offered };
}

assert.deepEqual(Array.from(BUILTIN_COMMANDS, command => command.name), ['/archive', '/spawn'], 'only the two built-in commands; macros stay on "<"');

// Only a draft that is, from its very first character, one slash command opens the list.
const { offered } = fixture();
assert.deepEqual(offered('/'), ['/archive', '/spawn']);
assert.deepEqual(offered('/ar'), ['/archive']);
assert.deepEqual(offered('/SP'), ['/spawn'], 'case does not matter');
for (const draft of ['', 'hi', 'hi /spawn', ' /spawn', '\n/spawn', 'line one\n/spawn', '/spawn now', '/spawn\nmore', '/spawn\n', '/nope', '<DO-CR>', '/do-cr']) {
  assert.deepEqual(offered(draft), [], `${JSON.stringify(draft)} offers nothing`);
}

// A command is offered only where its button would work.
assert.deepEqual(fixture({ archive: false }).offered('/'), ['/spawn'], 'no archive where the room cannot be archived');
assert.deepEqual(fixture({ spawn: false }).offered('/'), ['/archive'], 'no spawn where Spawn is unavailable');
assert.deepEqual(fixture({ archive: false, spawn: false }).offered('/'), []);

// A pick clears the draft and runs the flow once; permissions that went away while the list was open stop it.
{
  const f = fixture();
  f.offered('/spawn'); f.commands.pick(0);
  assert.deepEqual(f.calls, ['spawn']); assert.equal(f.draft.value, '', 'the draft is cleared');
  assert.equal(f.commands.list.shown.length, 0, 'the list is closed');
  f.offered('/arc'); f.commands.pick(0);
  assert.deepEqual(f.calls, ['spawn', 'archive']);
}
{
  let archive = true;
  const f = fixture(); f.commands.canArchive = () => archive;
  f.offered('/arc'); archive = false; f.commands.pick(0);
  assert.deepEqual(f.calls, [], 'archive is not run once the room can no longer be archived');
  assert.equal(f.draft.value, '/arc', 'the typed command stays');
}
console.log('quick-commands: ok');
