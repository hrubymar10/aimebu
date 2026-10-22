// The per-room notification rules are pure functions in a browser module, so this runs them in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'room-notify.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { NOTIFY_LEVELS, MUTE_CHOICES, muteUntil, mutedNow, roomSilenced, messageNotifies };', context);
const { NOTIFY_LEVELS, MUTE_CHOICES, muteUntil, mutedNow, roomSilenced, messageNotifies } = context.api;

const now = Date.parse('2026-10-21T12:00:00Z');
const later = new Date(now + 3600 * 1000).toISOString(), earlier = new Date(now - 1000).toISOString();

// The levels are the four the server knows, in the order the settings list them, and each explains itself.
assert.deepEqual(Array.from(NOTIFY_LEVELS, level => level.value), ['default', 'all', 'mentions', 'none']);
assert.ok(NOTIFY_LEVELS.every(level => level.label && level.help));
assert.deepEqual(Array.from(MUTE_CHOICES, choice => choice.value), ['1h', '8h', 'tomorrow']);

// A mute is a time in the future; it ends by itself, and a room set to nothing is silent whatever the time.
assert.equal(mutedNow({ muted_until: later }, now), true);
assert.equal(mutedNow({ muted_until: earlier }, now), false);
assert.equal(mutedNow({}, now), false);
assert.equal(roomSilenced({ notify: 'none' }, now), true);
assert.equal(roomSilenced({ notify: 'all', muted_until: later }, now), true);
assert.equal(roomSilenced({ notify: 'all', muted_until: earlier }, now), false);
assert.equal(roomSilenced(undefined, now), false);

// The mute choices end an hour, eight hours, or the next local 08:00 from now.
const start = new Date(2026, 9, 21, 22, 30, 0);
assert.equal(muteUntil('1h', start).getTime() - start.getTime(), 3600 * 1000);
assert.equal(muteUntil('8h', start).getTime() - start.getTime(), 8 * 3600 * 1000);
const tomorrow = muteUntil('tomorrow', start);
assert.deepEqual([tomorrow.getFullYear(), tomorrow.getMonth(), tomorrow.getDate(), tomorrow.getHours(), tomorrow.getMinutes()], [2026, 9, 22, 8, 0]);
assert.equal(muteUntil('nonsense', start), null);

// Which messages are worth a sound: addressed ones by default and for mentions, everything from others for "all", nothing for "none" or a
// mute, never one's own and never the system's.
const me = 'usr_me';
const addressed = { author_kind: 'agent', author_id: 'agt_1', targets: ['usr_me', 'agt_2'] };
const ordinary = { author_kind: 'agent', author_id: 'agt_1', targets: [] };
for (const notify of ['default', 'mentions', undefined]) {
  assert.equal(messageNotifies({ notify }, addressed, me, now), true, `${notify}: addressed`);
  assert.equal(messageNotifies({ notify }, ordinary, me, now), false, `${notify}: ordinary`);
}
assert.equal(messageNotifies({ notify: 'all' }, ordinary, me, now), true);
assert.equal(messageNotifies({ notify: 'none' }, addressed, me, now), false);
assert.equal(messageNotifies({ notify: 'all', muted_until: later }, addressed, me, now), false);
assert.equal(messageNotifies({ notify: 'all', muted_until: earlier }, ordinary, me, now), true);
assert.equal(messageNotifies({ notify: 'all' }, { author_kind: 'user', author_id: me, targets: [] }, me, now), false, 'your own message');
assert.equal(messageNotifies({ notify: 'all' }, { author_kind: 'system', author_id: 'system', targets: [me] }, me, now), false, 'a system message');
assert.equal(messageNotifies({ notify: 'default' }, { author_kind: 'agent', author_id: 'agt_1' }, me, now), false, 'no targets in the payload');
assert.equal(messageNotifies(undefined, addressed, me, now), false);

console.log('room notify checks passed');
