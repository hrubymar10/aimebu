// The Memory viewer's pure rules (web/js/pages/knowledge/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'knowledge', 'model.js'), 'utf8');
const context = vm.createContext({ TextEncoder, encodeURIComponent, Map });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { scopePath, recordScope, editable, projectLabelText, usageRows, usageMeta, usageShare, statusLine, emptyText, recordMeta, eventTouches };', context);
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
assert.deepEqual(rows.map(m.usageMeta), ['Project facts · Alpha · 5 / 100 bytes · 2 records', 'User profile · owner · 3 / 50 bytes · 1 record', 'Shared agent notes · 1 / 20 bytes · 1 record']);
assert.deepEqual([m.usageShare({ used: 5, cap: 100 }), m.usageShare({ used: 500, cap: 100 }), m.usageShare({ used: 5, cap: 0 })], ['5%', '100%', '0%']);
assert.equal(m.statusLine({ records, cap_bytes: 100, enabled: true }, 'all', 'Alpha'), "4 records: 2 project facts of Alpha, 1 user profiles, 1 shared notes. Other projects' facts are in the Project facts view.");
assert.equal(m.statusLine({ records: records.slice(0, 2), cap_bytes: 100, enabled: false }, 'project', 'Alpha'), '5 / 100 bytes · 2 records · Memory is off for Alpha: agents do not get its project facts, and saved records stay available.');
assert.equal(m.statusLine({ records: [records[2], { ...records[2], body: 'q' }], cap_bytes: 50 }, 'user_profile', ''), '2 records · 1 profiles · 50 bytes per profile');
assert.deepEqual(['all', 'project', 'task', 'user_profile'].map(m.emptyText), ['No visible memory saved.', 'No project facts saved.', 'No notes saved for this task.', 'No global memory saved.']);
const item = { scope: 'user_profile', scope_id: 'u1', scope_label: 'owner', rev: 3, author_kind: 'user', author_id: 'a', active: false };
assert.equal(m.recordMeta(item, { mode: 'all', profileUsage: new Map(), cap: 50, when: 'now' }), 'User profile owner · Inactive · Revision 3 · user a · now');
assert.equal(m.recordMeta(item, { mode: 'user_profile', profileUsage: new Map([['u1', 7]]), cap: 50, when: 'now' }), 'owner · 7 / 50 bytes · Inactive · Revision 3 · user a · now');
const touches = (event, view) => m.eventTouches(event, view);
assert.deepEqual([touches({ kind: 'knowledge_created', project_id: 'p' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'knowledge_created', project_id: 'q' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'global_knowledge_updated' }, { mode: 'all', projectID: 'p' }),
  touches({ kind: 'global_knowledge_updated' }, { mode: 'user_profile' }), touches({ kind: 'global_knowledge_updated' }, { mode: 'project', projectID: 'p' }), touches({ kind: 'knowledge_deleted', room_id: 'r' }, { mode: 'task', roomID: 'r' }), touches({ kind: 'message_created', project_id: 'p' }, { mode: 'project', projectID: 'p' })],
  [true, false, true, true, false, true, false]);
console.log('knowledge model tests passed');
