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
  '\nglobalThis.api = { InboxNotifications, notificationSnippet };';

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
    static async requestPermission() { log.asked = (log.asked || 0) + 1; return 'granted'; }
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
  return { notifier, api: context.api, log, opened, projects, advance: ms => { clock += ms; }, setHidden: value => { context.document.hidden = value; } };
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

{ // Whatever needs the person's attention tells the app (it may then offer browser notifications), whether or not it makes a sound: an
  // item, or a message that addresses them. A room that is quiet for them, a message that does not address them, and their own messages do not.
  const attention = [];
  const t = setup({ muted: true }); t.notifier.onAttention = () => attention.push('x');
  t.notifier.arrived([item], t.projects, new Map([['r1', room()]]));
  assert.equal(attention.length, 1, 'an item');
  t.advance(10000); t.notifier.message(mention, room(), t.projects, 'me', false);
  assert.equal(attention.length, 2, 'a message that addresses them');
  t.advance(10000); t.notifier.message(mention, room(), t.projects, 'me', true);
  assert.equal(attention.length, 3, 'also while they are looking at that room');
  t.advance(10000);
  t.notifier.arrived([item], t.projects, new Map([['r1', room({ notify: 'none' })]]));
  t.notifier.message(plain, room(), t.projects, 'me', false);
  t.notifier.message({ ...mention, author_kind: 'user', author_id: 'me' }, room({ notify: 'all' }), t.projects, 'me', false);
  assert.equal(attention.length, 3, 'a quiet room, an unaddressed message and their own message are not attention');
}

{ // The snippet is one line and short: whitespace flattened, cut with an ellipsis, nothing for nothing.
  const { notificationSnippet } = setup().api;
  assert.equal(notificationSnippet('  a\n\n b\t c '), 'a b c');
  assert.equal(notificationSnippet(''), '');
  assert.equal(notificationSnippet(undefined), '');
  assert.equal(notificationSnippet('x'.repeat(140)), 'x'.repeat(140), 'exactly the limit stays whole');
  assert.equal(notificationSnippet('x'.repeat(141)), 'x'.repeat(139) + '…');
}
{ // The text of a message in the desktop note is the person's choice, off by default; a failed read falls back to the plain note.
  const off = setup(); assert.equal(off.notifier.showText, false);
  let asked = 0; off.notifier.snippetOf = async () => { asked++; return 'secret'; };
  off.notifier.message(mention, room(), off.projects, 'me', false);
  assert.deepEqual([off.log.notes[0].body, asked], ['Project · A message for you', 0], 'off: no text, and the message is not even fetched');
}
(async () => {
  const on = setup(); on.notifier.showText = true;
  assert.equal(on.notifier.showText, true);
  on.notifier.snippetOf = async (roomID, messageID) => { on.asked = [roomID, messageID]; return 'Can you review the branch?'; };
  on.notifier.message(mention, room(), on.projects, 'me', false);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(on.asked, ['r1', 'm1']);
  assert.equal(on.log.notes[0].body, 'Project · Can you review the branch?', 'on: the text rides in the note');
  const failed = setup(); failed.notifier.showText = true; failed.notifier.snippetOf = async () => '';
  failed.notifier.message(plain, room({ notify: 'all' }), failed.projects, 'me', false);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(failed.log.notes[0].body, 'Project · New message', 'an empty or failed read shows the plain note');
  // A reply that is not a list (an error body) gives nothing, and the person coming back to the tab or switching the text off while the text
  // is on its way shows no note, or the plain one.
  assert.equal(await setup().notifier.snippetOf('r1', 'm1', async () => ({ messages: [{ id: 'm1', body: 'x' }] })), '', 'only the list the server really sends counts');
  const gone = setup(); gone.notifier.showText = true; let release;
  gone.notifier.snippetOf = () => new Promise(resolve => { release = resolve; });
  gone.notifier.message(mention, room(), gone.projects, 'me', false);
  gone.setHidden(false);
  release('late text'); await new Promise(resolve => setImmediate(resolve));
  assert.equal(gone.log.notes.length, 0, 'the tab came back while the text was on its way: no note');
  const off = setup(); off.notifier.showText = true; let letgo;
  off.notifier.snippetOf = () => new Promise(resolve => { letgo = resolve; });
  off.notifier.message(mention, room(), off.projects, 'me', false);
  off.notifier.showText = false; letgo('secret text'); await new Promise(resolve => setImmediate(resolve));
  assert.equal(off.log.notes[0].body, 'Project · A message for you', 'switched off meanwhile: the plain note');
  const visible = setup({ hidden: false }); visible.notifier.showText = true; let fetched = 0; visible.notifier.snippetOf = async () => { fetched++; return 'x'; };
  visible.notifier.message(mention, room(), visible.projects, 'me', false);
  assert.equal(fetched, 0, 'a visible tab shows no note, so nothing is fetched');
  // snippetOf reads the room's own list, picks the message by id and cuts it.
  const reader = setup(); let url = '';
  const text = await reader.notifier.snippetOf('r 1', 'm9', async path => { url = path; return [{ id: 'm8', body: 'other' }, { id: 'm9', body: '  long  '.repeat(60) }]; });
  assert.equal(url, '/api/rooms/r%201/messages?around=m9&limit=3');
  assert.ok(text.length <= 140 && text.endsWith('…') && !/\s{2}/.test(text), text);
  assert.equal(await reader.notifier.snippetOf('r1', 'm1', async () => { throw new Error('403'); }), '', 'no access: nothing');
  assert.equal(await reader.notifier.snippetOf('r1', 'm1', async () => [{ id: 'zzz', body: 'x' }]), '', 'not in the answer: nothing');
})().catch(error => { console.error(error); process.exit(1); });

(async () => { // Asking for the permission (the Settings button) tells whoever offers notifications, so the bar can re-read the answer; nothing when it
  // was asked before or notifications are unavailable.
  const t = setup({ permission: 'default' }); let told = 0; t.notifier.onPermission = () => { told++; };
  await t.notifier.requestPermission();
  assert.deepEqual([t.log.asked, told], [1, 1]);
  const asked = setup({ permission: 'granted' }); asked.notifier.onPermission = () => { told++; };
  await asked.notifier.requestPermission();
  assert.deepEqual([asked.log.asked || 0, told], [0, 1], 'already answered: no prompt, nobody told');
  console.log('inbox notifications test passed');
})().catch(error => { console.error(error); process.exit(1); });
