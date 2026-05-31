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
  messages: {
    general: [
      { id: 41, room_id: 'general', from: 'alice@aimebu', body: 'parent <message> with enough words to render a snippet' },
    ],
  },
  esc: htmlEscape,
};

vm.createContext(context);
vm.runInContext([
  extractFunction('findMessageInRoom'),
  extractFunction('messageSnippet'),
  extractFunction('replyReferenceHTML'),
].join('\n'), context);

const loaded = context.replyReferenceHTML({ id: 42, room_id: 'general', reply_to: 41 });
assert(loaded.includes('reply-reference'), 'reply reference renders a stub');
assert(loaded.includes('data-msg-id="41"'), 'reply reference is clickable by parent ID');
assert(loaded.includes('#41 alice@aimebu: parent &lt;message&gt;'), 'loaded parent quote includes escaped author and snippet');

const unloaded = context.replyReferenceHTML({ id: 43, room_id: 'general', reply_to: 99 });
assert(unloaded.includes('Reply to #99'), 'unloaded parent falls back to message ID');
assert(unloaded.includes('data-msg-id="99"'), 'unloaded parent still uses jump lookup path');

console.log('frontend reply tests passed');
