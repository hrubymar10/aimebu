// What the Claude Code session picker shows of a session is a few pure functions in a browser module, so this runs them in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'claude-sessions.js'), 'utf8');
const context = { relativeTime: iso => `ago(${iso})`, setGlyph: () => {} };
vm.createContext(context);
vm.runInContext(source.replace(/^import .*$/gm, '').replace(/^export /gm, '').replace(/^class ClaudeSessionPicker[\s\S]*$/m, '')
  + '\nglobalThis.api = { formatSize, sessionFacts, sessionFlags, listingStatus };', context);
const { formatSize, sessionFacts, sessionFlags, listingStatus } = context.api;

// Sizes read at the scale of a transcript: bytes, kilobytes, megabytes with a decimal below ten.
assert.equal(formatSize(512), '512 B');
assert.equal(formatSize(2048), '2 KB');
assert.equal(formatSize(3.5 * 1024 * 1024), '3.5 MB');
assert.equal(formatSize(120 * 1024 * 1024), '120 MB');

// The facts under a title: branch, when it was used, size and the start of the id, in that order, leaving out what is not known.
assert.deepEqual(Array.from(sessionFacts({ id: 'aaaaaaaa-1111', git_branch: 'main', modified_at: '2026-10-24T10:00:00Z', size: 2048 })),
  ['main', 'used ago(2026-10-24T10:00:00Z)', '2 KB', 'aaaaaaaa']);
assert.deepEqual(Array.from(sessionFacts({ id: 'bbbbbbbb-2222', size: 10 })), ['10 B', 'bbbbbbbb']);

// The flags: an agent that continues it stops it being picked; the rest only warn or note.
const flag = session => Array.from(sessionFlags(session), item => Array.from(item));
assert.deepEqual(flag({ id: 'x' }), []);
assert.deepEqual(flag({ agent: { slug: 'olive' } }), [['continued by olive', 'stop']]);
assert.deepEqual(flag({ active: true, outside_folder: true, entrypoint: 'sdk-cli' }),
  [['open in a terminal', 'warn'], ['outside the project folder', 'note'], ['started by an agent', 'note']]);
assert.deepEqual(flag({ entrypoint: 'cli' }), []);

// What the scan could not show is said, not dropped.
assert.equal(listingStatus(2, 'none', {}), '2 sessions');
assert.equal(listingStatus(1, 'none'), '1 session');
assert.equal(listingStatus(0, 'none', {}), 'none');
assert.equal(listingStatus(3, 'none', { unreadable: 2 }), '3 sessions (2 sessions could not be read)');
assert.equal(listingStatus(0, 'none', { unreadable: 1 }), 'none (1 session could not be read)');
assert.equal(listingStatus(200, 'none', { truncated: true, unreadable: 4 }), '200 sessions (only the newest are shown; 4 sessions could not be read)');
console.log('claude sessions ok');
