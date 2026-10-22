// What the notifier does with an inbox item or a chat message: sound and desktop notification by the room's level and mute, the global mute,
// the open room, own and system messages, and the window in which an item and its message speak once. Runs the module source in a vm with
// stand-ins for the audio element, the Notification API and the clock.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const read = name => fs.readFileSync(path.join(__dirname, '..', 'web', 'js', name), 'utf8');
const source = read('room-notify.js').replace(/^export /gm, '') + '\n' +
  read('inbox-notifications.js').replace(/^import .*$/gm, '').replace(/^export /gm, '') +
  '\nglobalThis.api = { InboxNotifications };';

function setup({ muted = false, permission = 'granted', hidden = true } = {}) {
  const log = { plays: 0, notes: [] };
  let clock = Date.parse('2026-10-21T12:00:00Z');
  class FakeDate extends Date { static now() { return clock; } }
  class FakeAudio {
    constructor() { this.listeners = {}; this.attrs = {}; }
    addEventListener(name, fn) { this.listeners[name] = fn; }
    getAttribute(name) { return this.attrs[name] ?? null; }
    set src(value) { this.attrs.src = value; }
    pause() {} play() { log.plays++; return Promise.resolve(); }
  }
  class FakeNotification {
    constructor(title, options) { log.notes.push({ title, ...options }); }
    close() {}
    static permission = permission;
  }
  const storage = new Map(muted ? [['aimebu2:inbox-muted:me', '1']] : []);
  const context = {
    Date: FakeDate, Audio: FakeAudio, Notification: FakeNotification, Map, Array, Number, Math, Promise, String,
    document: { addEventListener() {}, hidden },
    window: { isSecureContext: true, Notification: FakeNotification, focus() {} },
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    currentUser: 'me', savePreference() {},
  };
  context.window.Notification = FakeNotification;
  vm.createContext(context);
  vm.runInContext(`var currentUser = 'me'; var savePreference = () => {};` + source, context);
  const opened = [];
  const notifier = new context.api.InboxNotifications(item => opened.push(['item', item.id]), (room, id) => opened.push(['room', room.id, id]));
  const projects = new Map([['p1', { name: 'Project' }]]);
  return { notifier, log, opened, projects, advance: ms => { clock += ms; } };
}

const room = (extra = {}) => ({ id: 'r1', project_id: 'p1', title: 'general', notify: 'default', ...extra });
const mention = { message_id: 'm1', author_kind: 'agent', author_id: 'agt_1', targets: ['me'] };
const plain = { message_id: 'm2', author_kind: 'agent', author_id: 'agt_1', targets: [] };
const item = { id: 'i1', room_id: 'r1', project_id: 'p1', question: 'Approve?' };

// Default and Mentions speak for what addresses you, and say so; All speaks for everything from others; Nothing never does.
for (const [notify, message, plays] of [['default', mention, 1], ['default', plain, 0], ['mentions', mention, 1], ['mentions', plain, 0], ['all', plain, 1], ['none', mention, 0]]) {
  const t = setup();
  t.notifier.message(message, room({ notify }), t.projects, 'me', false);
  assert.equal(t.log.plays, plays, `${notify} ${message.message_id} sound`);
  assert.equal(t.log.notes.length, plays, `${notify} ${message.message_id} desktop note while the tab is hidden`);
}
{ // The desktop note names the room and project, says whether it is for you, and opens the room on the message.
  const t = setup();
  t.notifier.message(mention, room(), t.projects, 'me', false);
  assert.deepEqual([t.log.notes[0].title, t.log.notes[0].body, t.log.notes[0].tag], ['aimebu · general', 'Project · A message for you', 'aimebu-room-r1']);
  const noteless = setup(); noteless.notifier.message(plain, room({ notify: 'all' }), noteless.projects, 'me', false);
  assert.equal(noteless.log.notes[0].body, 'Project · New message');
}
{ // No desktop note while the tab is visible, none without permission; the sound still plays.
  const visible = setup({ hidden: false }); visible.notifier.message(mention, room(), visible.projects, 'me', false);
  assert.deepEqual([visible.log.plays, visible.log.notes.length], [1, 0]);
  const denied = setup({ permission: 'denied' }); denied.notifier.message(mention, room(), denied.projects, 'me', false);
  assert.deepEqual([denied.log.plays, denied.log.notes.length], [1, 0]);
}
{ // The global mute silences every sound and keeps the desktop note; a mute on the room silences both, until it ends.
  const muted = setup({ muted: true }); muted.notifier.message(mention, room(), muted.projects, 'me', false);
  assert.deepEqual([muted.log.plays, muted.log.notes.length], [0, 1]);
  const t = setup();
  const until = new Date(Date.parse('2026-10-21T13:00:00Z')).toISOString();
  t.notifier.message(mention, room({ muted_until: until }), t.projects, 'me', false);
  assert.deepEqual([t.log.plays, t.log.notes.length], [0, 0], 'a running mute');
  t.advance(2 * 3600 * 1000);
  t.notifier.message(mention, room({ muted_until: until }), t.projects, 'me', false);
  assert.deepEqual([t.log.plays, t.log.notes.length], [1, 1], 'the mute has ended');
}
{ // One's own messages and the system's never speak.
  const t = setup();
  t.notifier.message({ ...mention, author_kind: 'user', author_id: 'me' }, room({ notify: 'all' }), t.projects, 'me', false);
  t.notifier.message({ ...mention, author_kind: 'system', author_id: 'system' }, room({ notify: 'all' }), t.projects, 'me', false);
  assert.equal(t.log.plays, 0);
}
{ // A burst in one room is one sound; another room is its own; a few seconds later it speaks again.
  const t = setup();
  t.notifier.message(plain, room({ notify: 'all' }), t.projects, 'me', false);
  t.notifier.message(plain, room({ notify: 'all' }), t.projects, 'me', false);
  t.notifier.message(plain, room({ id: 'r2', notify: 'all' }), t.projects, 'me', false);
  assert.equal(t.log.plays, 2);
  t.advance(3500);
  t.notifier.message(plain, room({ notify: 'all' }), t.projects, 'me', false);
  assert.equal(t.log.plays, 3);
}
{ // An inbox item and the message it came from speak once, in either order; a room set to Nothing keeps its items quiet.
  const rooms = new Map([['r1', room()]]);
  const first = setup();
  first.notifier.message(mention, room(), first.projects, 'me', false);
  first.notifier.arrived([item], first.projects, rooms);
  assert.deepEqual([first.log.plays, first.log.notes.length], [1, 1], 'message then item');
  const second = setup();
  second.notifier.arrived([item], second.projects, rooms);
  second.notifier.message(mention, room(), second.projects, 'me', false);
  assert.deepEqual([second.log.plays, second.log.notes.length], [1, 1], 'item then message');
  const quiet = setup();
  quiet.notifier.arrived([item], quiet.projects, new Map([['r1', room({ notify: 'none' })]]));
  assert.deepEqual([quiet.log.plays, quiet.log.notes.length], [0, 0], 'Nothing silences the room\'s items');
  const away = setup();
  away.notifier.arrived([item], away.projects, rooms);
  assert.deepEqual([away.log.plays, away.log.notes.length], [1, 1], 'an item alone still chimes');
}
{ // The room that is open in a focused tab needs no alert, and the Inbox item its message opens does not chime for what is on screen.
  const t = setup();
  t.notifier.message(mention, room(), t.projects, 'me', true);
  t.notifier.arrived([item], t.projects, new Map([['r1', room()]]));
  assert.deepEqual([t.log.plays, t.log.notes.length], [0, 0]);
  const unaddressed = setup();
  unaddressed.notifier.message(plain, room(), unaddressed.projects, 'me', true);
  unaddressed.notifier.arrived([item], unaddressed.projects, new Map([['r1', room()]]));
  assert.equal(unaddressed.log.plays, 1, 'a message that did not address you claims nothing, so a later item still chimes');
}

console.log('inbox notifications test passed');
