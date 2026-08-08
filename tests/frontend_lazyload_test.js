#!/usr/bin/env node
'use strict';

// Tests the noMoreOlder latch-clearing invariant: fetchRoomMessages must clear
// noMoreOlder[roomID] when it replaces the message array, for the same reason
// pruneMessageCache does — the oldest loaded messages are gone, so "seen the
// beginning" no longer holds. Without this, lazy-loading never fires again after
// a room switch-back.

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
  messages: {},
  noMoreOlder: {},
  loadingOlder: {},
  lastMessagePreview: {},
  maxSeenMsgID: 0,
  agentID: 'bob',
  api: function(method, p) {
    return Promise.resolve({
      messages: [
        { id: 101, from: 'alice', body: 'newest' },
        { id: 99, from: 'alice', body: 'older' },
      ],
    });
  },
  recomputeAttentionCounts: function() {},
  renderMessages: function() {},
  console: console,
};

vm.createContext(context);
vm.runInContext(extractFunction('fetchRoomMessages'), context);

async function runTests() {
  // Simulate the bug condition: latch was set when the user scrolled to the
  // beginning of a room, then the room was re-entered (fetchRoomMessages ran).
  context.noMoreOlder['r1'] = true;
  context.messages['r1'] = [];

  await context.fetchRoomMessages('r1');

  assert.strictEqual(
    context.noMoreOlder['r1'],
    false,
    'fetchRoomMessages must clear noMoreOlder when replacing the message array ' +
    '(latch prevented lazy-loading after room switch-back; same invariant as pruneMessageCache)',
  );

  // Array is replaced and reversed to oldest-first
  assert.strictEqual(context.messages['r1'].length, 2, 'messages populated');
  assert.strictEqual(context.messages['r1'][0].id, 99, 'reversed: oldest first');
  assert.strictEqual(context.messages['r1'][1].id, 101, 'reversed: newest last');

  // Preview stored from the last (newest) message
  assert.strictEqual(
    context.lastMessagePreview['r1'],
    'alice: newest',
    'preview set from last message',
  );

  console.log('ok');
}

runTests().catch(function(err) { console.error(err.message || err); process.exit(1); });
