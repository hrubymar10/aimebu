// The collapsed-root unread total is a pure function in a browser module, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'sidebar', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { roomUnread, ReadMarker };', context);
const { roomUnread, ReadMarker } = context.api;

const home = { id: 'home', unread_count: 2 };
const a = { id: 'a', unread_count: 3 }, b = { id: 'b', unread_count: 4 }, quiet = { id: 'q' };

// Missing, zero or negative counts are zero.
assert.equal(roomUnread({ id: 'x', unread_count: -5 }, ''), 0);
assert.equal(roomUnread({ id: 'x', unread_count: 4 }, 'x'), 0);

// Read marks: one request per room in flight; a further mark asked for meanwhile follows it; a sign-out forgets both.
(async () => {
  const settle = () => new Promise(resolve => setImmediate(resolve));
  const sent = [], pending = [];
  const marker = new ReadMarker((room, seq) => { sent.push(`${room}:${seq}`); return new Promise(resolve => pending.push(resolve)); });
  const states = [];
  marker.mark('r', 5, state => states.push(state));
  marker.mark('r', 5); marker.mark('r', 4);
  await settle();
  assert.deepEqual(sent, ['r:5'], 'a repeated or lower mark while one is in flight is not sent');
  marker.mark('r', 9, state => states.push(state)); marker.mark('r', 7);
  pending.shift()({ unread_count: 4 }); await settle(); await settle();
  assert.deepEqual(sent, ['r:5', 'r:9'], 'the highest mark asked for meanwhile follows the one in flight');
  pending.shift()({ unread_count: 0 }); await settle(); await settle();
  assert.deepEqual(states.map(state => state.unread_count), [4, 0]);
  assert.equal(sent.length, 2, 'nothing more is sent once the wanted mark went out');
  // User A's request is still in flight at sign-out: user B's mark for the same room is sent, and A's late result
  // neither reports nor re-issues anything.
  const lateStates = [];
  marker.mark('r', 3, state => lateStates.push(['a', state])); marker.mark('r', 8);
  await settle();
  marker.reset();
  marker.mark('r', 3, state => lateStates.push(['b', state]));
  await settle();
  assert.deepEqual(sent.slice(2), ['r:3', 'r:3'], 'a mark after a sign-out was skipped because of the previous user\'s request');
  pending.shift()({ unread_count: 1 }); await settle(); await settle();
  assert.equal(sent.length, 4, 'a request from before the sign-out re-issued its pending mark');
  pending.shift()({ unread_count: 0 }); await settle();
  assert.deepEqual(lateStates, [['b', { unread_count: 0 }]], 'a request from before the sign-out reported its result');
  // An onState that throws is contained: no unhandled rejection, and the room can be marked again.
  const unhandled = [];
  const watch = reason => unhandled.push(reason);
  process.on('unhandledRejection', watch);
  const calm = new ReadMarker(async () => ({ unread_count: 0 }));
  calm.mark('t', 1, () => { throw new Error('onState failed'); });
  await settle(); await settle(); await settle();
  process.off('unhandledRejection', watch);
  assert.deepEqual(unhandled, [], 'a throwing onState surfaced as an unhandled rejection');
  const again = [];
  calm.mark('t', 2, state => again.push(state));
  await settle(); await settle();
  assert.equal(again.length, 1, 'the room could not be marked again after a throwing onState');
  // idle(): settles at once with nothing in flight, else only after the mark in flight and the one queued behind it are answered (a mark unread is
  // sent after them, so the server applies it last); a reset lets every waiter go.
  const held = [], gate = new ReadMarker(() => new Promise(resolve => held.push(resolve)));
  let idle = false;
  await gate.idle('u');
  gate.mark('u', 4); gate.mark('u', 6);
  gate.idle('u').then(() => { idle = true; });
  await settle();
  held.shift()({}); await settle(); await settle();
  assert.equal(idle, false, 'idle settled while the queued mark was still out');
  held.shift()({}); await settle(); await settle();
  assert.equal(idle, true, 'idle did not settle once the room had nothing out');
  let released = false;
  gate.mark('u', 9); gate.idle('u').then(() => { released = true; });
  gate.reset(); await settle();
  assert.equal(released, true, 'a reset kept a waiter waiting');
  console.log('room unread test passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
