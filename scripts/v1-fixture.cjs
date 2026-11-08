// A small v1 backup for the import wizard's page test: the SQLite file and attachment folder a v1 install leaves under its config dir.
// Built with node's own sqlite (no dependencies). The shapes follow v1's store (a JSON `data` column per row), as the Go importer tests do.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { DatabaseSync } = require('node:sqlite');

const DAY = 24 * 3600 * 1000;
const ago = days => new Date(Date.now() - days * DAY).toISOString().replace(/\.\d+Z$/, 'Z');

// What the fixture holds, for the test to count against:
//   rooms: repo-leader + repo (a project and its working room; `repo` is old, so it is suggested archived), a DM, and v1's _system room
//   people: alice (human, maps to the owner), bob (human); agent bot@repo
//   messages: 5 in rooms + 1 presence line ("bot joined") + 1 in the DM + 1 in _system, 1 reaction, 1 attachment, 2 memory records, 2 macros, 1 room view preference (alice pinned repo-leader)
const FIXTURE = { rooms: 4, messages: 8, imported: 5, presence: 1, skippedRooms: 2, skippedMessages: 2, attachments: 1, memory: 2, macros: 2, roomPrefs: 1, humans: ['alice', 'bob'], projectName: 'repo' };

function writeV1Backup(root) {
  const dir = path.join(root, 'server');
  fs.mkdirSync(path.join(dir, 'attachments'), { recursive: true });
  const db = new DatabaseSync(path.join(dir, 'aimebu.sqlite'));
  for (const statement of [
    'CREATE TABLE schema_meta(version INTEGER)', 'INSERT INTO schema_meta(version) VALUES (1)',
    'CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)', 'CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)',
    'CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)', 'CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)',
    'CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)', 'CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)',
    'CREATE TABLE macros(key TEXT PRIMARY KEY,value TEXT NOT NULL,seen_default INTEGER NOT NULL DEFAULT 0)',
    'CREATE TABLE settings(id INTEGER PRIMARY KEY CHECK(id=1),data TEXT NOT NULL)',
    'CREATE TABLE role_overrides(key TEXT PRIMARY KEY,data TEXT NOT NULL)', 'CREATE TABLE role_custom(key TEXT PRIMARY KEY,data TEXT NOT NULL)',
    'CREATE TABLE prompts(key TEXT PRIMARY KEY,value TEXT NOT NULL)',
    'CREATE TABLE room_prefs (agent_id TEXT NOT NULL, room_id TEXT NOT NULL, hidden INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (agent_id, room_id))',
  ]) db.exec(statement);
  const put = (table, key, value) => db.prepare(`INSERT INTO ${table}(id,data) VALUES (?,?)`).run(key, JSON.stringify(value));
  const message = (id, room, from, kind, body, extra = {}) => db.prepare('INSERT INTO messages(id,room_id,data) VALUES (?,?,?)').run(id, room, JSON.stringify({ id, room_id: room, from, from_kind: kind, body, created_at: ago(1), ...extra }));
  const recent = ago(2), old = ago(60);
  put('rooms', '_system', { id: '_system', members: [], created_at: old, created_by: 'system' });
  put('rooms', 'repo-leader', { id: 'repo-leader', members: ['alice', 'bob', 'bot@repo'], created_at: recent, created_by: 'alice' });
  put('rooms', 'repo', { id: 'repo', members: ['alice', 'bot@repo'], created_at: old, created_by: 'alice' });
  put('rooms', 'dm:alice:bot@repo', { id: 'dm:alice:bot@repo', members: ['alice', 'bot@repo'], created_at: old, created_by: 'alice' });
  put('agents', 'alice', { id: 'alice', kind: 'human', name: 'alice' });
  put('agents', 'bob', { id: 'bob', kind: 'human', name: 'bob' });
  put('agents', 'bot@repo', { id: 'bot@repo', kind: 'ai', name: 'bot', harness: 'codex', model: 'gpt-6-sol' });
  message(1, 'repo-leader', 'alice', 'human', 'plan the work');
  message(2, 'repo-leader', 'bot@repo', 'ai', 'on it', { reply_to: 1, from_harness: 'codex', from_model: 'gpt-6-sol' });
  message(3, 'repo-leader', 'bob', 'human', 'see the picture', { attachments: [{ id: 'file1' }] });
  message(4, 'repo', 'alice', 'human', 'an older working room message', { created_at: old });
  message(8, 'repo-leader', '_system', 'system', 'bot joined');
  message(5, 'repo', 'bot@repo', 'ai', 'done', { created_at: old });
  message(6, 'dm:alice:bot@repo', 'alice', 'human', 'private');
  message(7, '_system', 'system', 'system', 'server started', { from_kind: 'system' });
  put('attachments', 'file1', { id: 'file1', name: 'image.png', mime: 'image/png', size: 3, uploaded_at: recent, ext: 'png' });
  fs.writeFileSync(path.join(dir, 'attachments', 'file1.png'), 'PNG');
  db.prepare('INSERT INTO reactions(message_id,data) VALUES (2,?)').run(JSON.stringify([{ agent_id: 'alice', emoji: '✅', created_at: recent }]));
  put('memory', 'fact', { id: 'fact', scope: 'project_facts', scope_key: 'repo', body: 'a project fact', version: 1, author: 'alice', created_at: recent, updated_at: recent });
  put('memory', 'profile', { id: 'profile', scope: 'user_profile', scope_key: 'alice', body: 'a preference', version: 1, author: 'alice', created_at: recent, updated_at: recent });
  db.exec("INSERT INTO macros(key,value,seen_default) VALUES ('greet','Hello <name>',0),('name','world',0)");
  db.prepare('INSERT INTO room_prefs(agent_id,room_id,hidden,pinned) VALUES (?,?,0,1)').run('alice', 'repo-leader');
  db.close();
}

module.exports = { writeV1Backup, FIXTURE };
