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

function htmlEscape(value) {
  return String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

const context = {
  agentID: 'bob',
  rooms: [{
    id: 'general',
    members: ['alice@aimebu', 'bob', 'sam@alpha', 'sam@beta'],
  }],
  reactionQuickPick: ['👍', '✅'],
  esc: htmlEscape,
  assert,
};

vm.createContext(context);
vm.runInContext([
  extractFunction('agentSlugFromID'),
  extractFunction('ambiguousSlugsForRoom'),
  extractFunction('reactionAgentLabel'),
  extractFunction('reactionSummariesWithLocalMe'),
  extractFunction('reactionTitle'),
  extractFunction('reactionPickerHTML'),
  extractFunction('reactionsHTML'),
].join('\n'), context);

assert.strictEqual(context.reactionsHTML({ id: 1, reactions: [] }), '', 'reactionless messages render no row');

const row = context.reactionsHTML({
  id: 2,
  room_id: 'general',
  reactions: [{ emoji: '👍', count: 2, agents: ['alice@aimebu', 'bob'] }],
});
assert(row.includes('message-reactions'), 'reaction row is rendered when reactions exist');
assert(row.includes('message-reaction-pill mine'), 'local reactor is marked as mine from agents');
assert(row.includes('alice, bob'), 'native title uses bare slugs for unique reactors');

const collisionRow = context.reactionsHTML({
  id: 3,
  room_id: 'general',
  reactions: [{ emoji: '✅', count: 2, agents: ['sam@alpha', 'sam@beta'] }],
});
assert(collisionRow.includes('sam@alpha, sam@beta'), 'native title expands ambiguous slugs to full IDs');

const patchStart = source.indexOf('function patchReactionRow');
const patchEnd = source.indexOf('function applyReactionEvent', patchStart);
assert(patchStart !== -1 && patchEnd !== -1, 'patchReactionRow body found');
const patchBody = source.slice(patchStart, patchEnd);
assert(patchBody.includes('node.remove()'), 'last reaction removes the row');
assert(patchBody.includes('insertBefore(next'), 'first reaction injects the row');

console.log('frontend reaction tests passed');
