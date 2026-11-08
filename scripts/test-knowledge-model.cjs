// The Memory viewer's pure rules (web/js/pages/knowledge/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'knowledge', 'model.js'), 'utf8');
const context = vm.createContext({ TextEncoder, encodeURIComponent, Map });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { awaitsApproval, scopePath, recordScope, editable, projectLabelText, usageRows, usageCount, usageShare, offNotice, emptyText, recordMeta, recordKey, authorLabel, eventTouches };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const ids = { projectID: 'p 1', roomID: 'r/1' };
assert.deepEqual(['project', 'user_profile', 'agent_shared_notes', 'task'].map(scope => m.scopePath(scope, ids)), ['/api/projects/p%201/memory', '/api/global-memory/user_profile', '/api/global-memory/agent_shared_notes', '/api/rooms/r%2F1/task-notes']);
assert.deepEqual([m.recordScope({ scope: 'user_profile' }, 'all'), m.recordScope({}, 'project'), m.recordScope({ scope: 'weird' }, 'task')], ['user_profile', 'project', 'task']);
assert.deepEqual([m.editable('project', { can_edit: true, enabled: true }), m.editable('project', { can_edit: true, enabled: false }), m.editable('user_profile', { can_edit: true }), m.editable('user_profile', {})], [true, false, true, false], 'a project switched off cannot be edited; a global scope needs only the right');
assert.deepEqual([m.projectLabelText('all'), m.projectLabelText('project')], ['Project facts from', 'Project']);
const records = [{ scope: 'project', body: 'abc', _cap: 100 }, { scope: 'project', body: 'de', _cap: 100 }, { scope: 'user_profile', scope_id: 'u1', scope_label: 'owner', body: 'xyz', _cap: 50 }, { scope: 'agent_shared_notes', body: 'n', _cap: 20 }];
const rows = plain(m.usageRows(records, 'all', 'Alpha'));
assert.deepEqual(rows, [{ scope: 'project', key: 'Alpha', used: 5, count: 2, cap: 100 }, { scope: 'user_profile', key: 'owner', used: 3, count: 1, cap: 50 }, { scope: 'agent_shared_notes', key: '', used: 1, count: 1, cap: 20 }]);
assert.deepEqual(rows.map(m.usageCount), ['5/100 · 2 records', '3/50 · 1 record', '1/20 · 1 record']);
assert.deepEqual([m.usageShare({ used: 5, cap: 100 }), m.usageShare({ used: 500, cap: 100 }), m.usageShare({ used: 5, cap: 0 })], ['5%', '100%', '0%']);
assert.equal(m.offNotice('Alpha'), 'Memory is off for Alpha: agents do not get its project facts, and saved records stay available.');
assert.deepEqual(['all', 'project', 'task', 'user_profile'].map(m.emptyText), ['No visible memory saved.', 'No project facts saved.', 'No notes saved for this task.', 'No global memory saved.']);
const item = { scope: 'user_profile', scope_id: 'u1', scope_label: 'owner', rev: 3, author_kind: 'user', author_id: 'usr_a', active: false };
assert.equal(m.recordMeta(item, { mode: 'all', when: 'now' }), 'Inactive · v3 · someone · now');
assert.equal(m.recordMeta({ ...item, author_name: 'owner' }, { mode: 'all', when: 'now' }), 'Inactive · v3 · owner · now', 'the author is a name, never an id');
assert.deepEqual([m.authorLabel({ author_kind: 'agent', author_id: 'agt_1' }), m.authorLabel({ author_kind: 'user', author_id: 'usr_9' }), m.authorLabel({ author_kind: 'user', author_id: 'owner-v1' }), m.authorLabel({ author_name: 'ivy', author_id: 'agt_2' })], ['an agent', 'someone', 'owner-v1', 'ivy']);
assert.deepEqual([m.recordKey(item, 'all', { projectName: 'Alpha', taskTitle: 'T' }), m.recordKey({ scope: 'project' }, 'all', { projectName: 'Alpha', taskTitle: 'T' }), m.recordKey({ scope: 'agent_shared_notes' }, 'all', { projectName: 'Alpha', taskTitle: 'T' }), m.recordKey({}, 'task', { projectName: 'Alpha', taskTitle: 'T' })], ['owner', 'Alpha', 'global', 'T']);
// A shared note an agent wrote and nobody approved is "waiting for approval", not "inactive" (which is what the owner's own switch-off says).
const agentNote = { scope: 'agent_shared_notes', scope_id: 'global', active: false, rev: 1, author_kind: 'agent', author_id: 'agt_1' };
assert.equal(m.awaitsApproval(agentNote, 'agent_shared_notes'), true);
assert.equal(m.awaitsApproval({ ...agentNote, active: true }, 'agent_shared_notes'), false);
assert.equal(m.awaitsApproval({ ...agentNote, author_kind: 'user' }, 'agent_shared_notes'), false, 'switched off by a person: inactive');
assert.equal(m.awaitsApproval(agentNote, 'user_profile'), false);
assert.ok(m.recordMeta(agentNote, { mode: 'agent_shared_notes', when: 'now' }).includes('Waiting for approval · '));
assert.ok(m.recordMeta({ ...agentNote, author_kind: 'user' }, { mode: 'agent_shared_notes', when: 'now' }).includes('Inactive · '));
const touches = (event, view) => m.eventTouches(event, view);
assert.deepEqual([touches({ kind: 'knowledge_created', project_id: 'p' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'knowledge_created', project_id: 'q' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'global_knowledge_updated' }, { mode: 'all', projectID: 'p' }),
  touches({ kind: 'global_knowledge_updated' }, { mode: 'user_profile' }), touches({ kind: 'global_knowledge_updated' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'knowledge_deleted', room_id: 'r' }, { mode: 'task', roomID: 'r' }), touches({ kind: 'message_created', project_id: 'p' }, { mode: 'project', projectID: 'p' })],
  [true, false, true, true, false, true, false]);
console.log('knowledge model tests passed');
