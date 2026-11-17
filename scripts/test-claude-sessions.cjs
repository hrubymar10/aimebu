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
  + '\nglobalThis.api = { formatSize, sessionFacts, sessionFlags, listingStatus, rowTitles };', context);
const { formatSize, sessionFacts, sessionFlags, listingStatus, rowTitles } = context.api;

// Sizes read at the scale of a transcript: bytes, kilobytes, megabytes with a decimal below ten.
assert.equal(formatSize(512), '512 B');
assert.equal(formatSize(2048), '2 KB');
assert.equal(formatSize(3.5 * 1024 * 1024), '3.5 MB');
assert.equal(formatSize(120 * 1024 * 1024), '120 MB');

// The facts under a title: branch, when it was used, size and the start of the id, in that order, leaving out what is not known.
assert.deepEqual(Array.from(sessionFacts({ id: 'aaaaaaaa-1111', git_branch: 'main', modified_at: '2026-10-24T10:00:00Z', size: 2048 })),
  ['main', 'used ago(2026-10-24T10:00:00Z)', '2 KB', 'aaaaaaaa']);
assert.deepEqual(Array.from(sessionFacts({ id: 'bbbbbbbb-2222', size: 10 })), ['10 B', 'bbbbbbbb']);
assert.deepEqual(Array.from(sessionFacts({ id: 'cccccccc-3333', git_branch: 'HEAD', size: 10 })), ['10 B', 'cccccccc'], '"HEAD" is no branch (a detached HEAD, or no repository): it is not shown');

// The flags: an agent that continues it only rules out a second continuation (a copy is still possible), so every flag warns or notes.
const flag = session => Array.from(sessionFlags(session), item => Array.from(item));
assert.deepEqual(flag({ id: 'x' }), []);
assert.deepEqual(flag({ agent: { slug: 'olive' } }), [['continued by olive', 'note']]);
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
// Rows with the same title are told apart by when they were last used; a title that is alone, or has no time to add, stays as it is.
const named = rowTitles([{ title: 'aimebu turn', modified_at: '2026-11-16T22:41:00Z' }, { title: 'aimebu turn', modified_at: '2026-11-15T09:05:00Z' }, { title: 'fix the build', modified_at: '2026-11-14T10:00:00Z' }, { modified_at: '2026-11-14T10:00:00Z' }, { title: 'aimebu turn' }]);
assert.ok(named[0].startsWith('aimebu turn · ') && named[1].startsWith('aimebu turn · ') && named[0] !== named[1], `repeated titles carry their time: ${named}`);
assert.deepEqual([named[2], named[3], named[4]], ['fix the build', 'Untitled session', 'aimebu turn'], 'a unique title, an untitled one and one with no time are unchanged');
console.log('claude sessions ok');
