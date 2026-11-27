#!/usr/bin/env node
// API-only seed and content comparison for the existing supervisor smoke.
'use strict';
const fs = require('node:fs');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const [mode, base, project, room, attachment, snapshotFile] = process.argv.slice(2);
const headers = { Cookie: process.env.AIMEBU_TEST_COOKIE || '' };
async function request(path, options = {}) {
  const response = await fetch(base + path, { ...options, headers: { ...headers, ...(options.headers || {}) } });
  if (!response.ok) throw new Error(`${options.method || 'GET'} ${path}: HTTP ${response.status} ${await response.text()}`);
  return response;
}
async function state() {
  const json = async path => (await request(path)).json();
  const select = (rows, keys) => rows.map(row => keys.map(key => row[key] ?? null));
  const bytes = Buffer.from(await (await request('/api/attachments/' + attachment)).arrayBuffer());
  return {
    projects: select(await json('/api/projects'), ['id', 'name', 'folder']),
    rooms: select(await json(`/api/projects/${project}/rooms`), ['id', 'project_id', 'kind', 'title']),
    agents: select(await json(`/api/rooms/${room}/agents`), ['id', 'slug', 'role_key', 'tool', 'model']),
    messages: select(await json(`/api/rooms/${room}/messages`), ['id', 'body', 'author_kind', 'attachments']),
    attachment: crypto.createHash('sha256').update(bytes).digest('hex'),
  };
}
async function main() {
  if (mode === 'seed') {
    const targetRoom = project;
    const form = new FormData();
    form.append('file', new Blob([Buffer.from('upgrade attachment content\n')], { type: 'text/plain' }), 'upgrade.txt');
    const uploaded = await (await request(`/api/rooms/${targetRoom}/attachments`, {
      method: 'POST', headers: { 'X-CSRF-Token': process.env.AIMEBU_TEST_CSRF || '' }, body: form,
    })).json();
    await request(`/api/rooms/${targetRoom}/messages`, {
      method: 'POST', headers: { 'X-CSRF-Token': process.env.AIMEBU_TEST_CSRF || '', 'Content-Type': 'application/json' },
      body: JSON.stringify({ body: 'seeded message', attachments: [uploaded.id] }),
    });
    const actual = Buffer.from(await (await request('/api/attachments/' + uploaded.id)).arrayBuffer());
    assert.equal(actual.toString(), 'upgrade attachment content\n');
    process.stdout.write(uploaded.id + '\n');
    return;
  }
  if (!base || !project || !room || !attachment || !snapshotFile) throw new Error('state arguments missing');
  if (mode === 'snapshot') {
    const before = await state();
    assert(before.messages.some(message => message[1] === 'seeded message'));
    fs.writeFileSync(snapshotFile, JSON.stringify(before));
    return;
  }
  if (mode !== 'compare') throw new Error('unknown state mode');
  const before = JSON.parse(fs.readFileSync(snapshotFile));
  const after = await state();
  for (const name of ['projects', 'rooms', 'agents']) {
    assert.deepEqual(after[name], before[name], `${name} changed across migration`);
    console.log(`PASS migrated ${name} count and content`);
  }
  const ids = new Set(before.messages.map(message => message[0]));
  const survivors = after.messages.filter(message => ids.has(message[0]));
  const extra = process.env.AIMEBU_UPGRADE_CORRUPT_EXPECTED_MESSAGES === '1' ? 1 : 0;
  assert.equal(survivors.length, before.messages.length + extra, 'messages changed across migration');
  assert.deepEqual(survivors, before.messages, 'messages changed across migration');
  console.log('PASS migrated messages count and content');
  assert.equal(after.attachment, before.attachment, 'attachment changed across migration');
  console.log('PASS migrated attachment bytes');
}
main().catch(error => { console.error('FAIL upgrade state: ' + error.message); process.exitCode = 1; });
