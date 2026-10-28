// The sender profile's pure rules (web/js/pages/sender-profile/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'sender-profile', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { senderFacts, messageRows, profileRows, profileNote, startNotes };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const messages = [
  { seq: 2, author_kind: 'agent', author_id: 'bot', author_model: 'sonnet', author_harness: 'codex', created_at: 'B' },
  { seq: 1, author_kind: 'agent', author_id: 'bot', created_at: 'A' },
  { seq: 3, author_kind: 'user', author_id: 'bot', created_at: 'C' },
  { seq: 4, author_kind: 'agent', author_id: 'other', created_at: 'D' }];
assert.deepEqual(plain(m.senderFacts(messages, 'bot')), { count: 2, first: 'A', last: 'B', model: 'sonnet', harness: 'codex' });
assert.deepEqual(plain(m.senderFacts(messages.values(), 'bot')).count, 2, 'any iterable of messages');
assert.deepEqual(plain(m.senderFacts([], 'bot')), { count: 0, first: '', last: '', model: '', harness: '' });
assert.equal(m.senderFacts([{ seq: 1, author_kind: 'agent', author_id: 'bot', author_model: 'old', created_at: 'A' }, { seq: 2, author_kind: 'agent', author_id: 'bot', created_at: 'B' }], 'bot').model, 'old', 'the newest message that names a model');

const date = value => (value ? `d(${value})` : '');
assert.deepEqual(plain(m.messageRows({ count: 2, first: 'A', last: 'B', model: 'sonnet', harness: 'codex' }, 'sonnet · Codex (historical)', date)),
  [['Runtime', 'sonnet · Codex'], ['Model', 'sonnet'], ['Harness', 'codex'], ['First heard', 'd(A)'], ['Last heard', 'd(B)'], ['Messages loaded', '2']]);
assert.deepEqual(plain(m.messageRows({ count: 0, first: '', last: '', model: '', harness: '' }, 'unknown (departed)', date)), [['Runtime', 'unknown']]);

const relative = value => `rel(${value})`;
assert.deepEqual(plain(m.profileRows({ native_session_id: 's', resume_command: 'r', cwd: '/w', branch: 'main', machine_name: 'Host', project_name: 'Alpha', imported: true, last_seen: 'L', spawned_by: 'owner', spawned_at: 'S' }, date, relative)),
  [['Session', 's', true], ['Resume', 'r', true], ['CWD', '/w', true], ['Branch', 'main', true], ['Machine', 'Host'], ['Project', 'Alpha'], ['Last active in v1', 'd(L)'], ['Spawned', 'owner · rel(S)']]);
assert.deepEqual(plain(m.profileRows({ cwd: '/w', spawned_by: 'owner' }, date, relative)), [['CWD', '/w', true], ['Spawned', 'owner']]);
assert.deepEqual(plain(m.profileRows({}, date, relative)), []);

assert.equal(m.profileNote('departed', 'loading', null), 'This agent is no longer in the room.');
assert.equal(m.profileNote('departed', 'failed', null), 'This agent is no longer in the room.');
assert.equal(m.profileNote('historical', 'loading', null), '');
assert.equal(m.profileNote('historical', 'done', { cwd: '/w' }), 'This sender comes from imported v1 history.');
assert.equal(m.profileNote('historical', 'done', {}), 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.');
assert.equal(m.profileNote('historical', 'failed', null), 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.');
assert.equal(m.profileNote('historical', 'none', null), 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.');

assert.equal(m.startNotes({ resumable: true, cwd: '/w', outside_folder: true, v1_active: true }), 'Continues the v1 session in /w. That folder is outside the project folder; the owner approves it first. v1 saw it a moment ago and may still be using the session.');
assert.equal(m.startNotes({ resumable: true, cwd: '/w' }), 'Continues the v1 session in /w.');
assert.equal(m.startNotes({ resumable: false, reason: 'No session was kept.', outside_folder: true }), 'No session was kept.');
assert.equal(m.startNotes({ resumable: false }), '');
console.log('sender profile model: ok');
