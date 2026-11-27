#!/usr/bin/env node
// Restore a backup into a disposable data root and compare safe snapshots.
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const { DatabaseSync } = require('node:sqlite');
const [binary, archive, scratch] = process.argv.slice(2);
if (!binary || !archive || !scratch) throw new Error('binary, archive and scratch directory required');
const root = path.resolve(scratch, 'config');
const realRoot = path.resolve(process.env.AIMEBU_REAL_DATA_ROOT || path.join(os.homedir(), '.aimebu'));
assert.notEqual(root, realRoot, 'refusing real data root');
const env = { ...process.env, AIMEBU_CONFIG_DIR: root, AIMEBU_BIND: '127.0.0.1', AIMEBU_USAGES: '0', AIMEBU_WARM_IDLE: '0', AIMEBU_SENTRY_DSN: 'off' };
function run(command, args, options = {}) {
  const result = spawnSync(command, args, { encoding: 'utf8', timeout: 120000, env, ...options });
  if (result.error || result.status !== 0) throw new Error(`${command} ${args.join(' ')}: ${result.error || result.stderr || `exit ${result.status}`}`);
  return result.stdout;
}
function extract(source, entry, target) {
  const fd = fs.openSync(target, 'w', 0o600);
  try { run('tar', ['-xOzf', source, entry], { stdio: ['ignore', fd, 'pipe'] }); }
  finally { fs.closeSync(fd); }
}
function rows(file) {
  const queries = {
    projects: 'SELECT id,name,folder FROM project ORDER BY id',
    rooms: 'SELECT id,project_id,kind,title FROM room ORDER BY id',
    messages: 'SELECT id,room_id,author_kind,body FROM message ORDER BY id',
    agents: 'SELECT id,slug,project_id,model FROM agent ORDER BY id',
    attachments: 'SELECT id,project_id,room_id,sha256,size FROM attachment ORDER BY id',
  };
  const db = new DatabaseSync(file, { readOnly: true });
  try { return Object.fromEntries(Object.entries(queries).map(([name, query]) => [name, db.prepare(query).all()])); }
  finally { db.close(); }
}
function compare(actual, expected, verb) {
  for (const name of Object.keys(expected)) {
    assert.deepEqual(actual[name], expected[name], `${name} changed ${verb}`);
    console.log(`PASS ${verb} ${name} count and content`);
  }
}
function digest(file) { return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex'); }
async function main() {
  const manifestFile = path.join(scratch, 'manifest.json');
  const sourceDB = path.join(scratch, 'source.sqlite');
  extract(archive, 'manifest.json', manifestFile);
  const manifest = JSON.parse(fs.readFileSync(manifestFile));
  assert(!manifest.missing_attachments?.length, 'backup has missing attachments');
  extract(archive, 'server/aimebu.sqlite', sourceDB);
  const baseline = rows(sourceDB);
  const attachments = manifest.files.filter(file => file.path.startsWith('server/attachments/'));
  console.log('PASS backup manifest and attachment files');
  const serverEnv = { ...env };
  const net = require('node:net');
  const port = await new Promise((resolve, reject) => {
    const listener = net.createServer();
    listener.once('error', reject);
    listener.listen(0, '127.0.0.1', () => { const value = listener.address().port; listener.close(() => resolve(value)); });
  });
  serverEnv.AIMEBU_PORT = String(port);
  let started = false;
  try {
    const restore = spawnSync(binary, ['restore', archive, '--yes'], { env: serverEnv, encoding: 'utf8', timeout: 120000 });
    assert.equal(restore.status, 0, restore.stderr || restore.error?.message);
    console.log('PASS restore into isolated data root');
    compare(rows(path.join(root, 'server/aimebu.sqlite')), baseline, 'restored');
    for (const entry of attachments) {
      const file = path.resolve(root, entry.path);
      assert(file.startsWith(root + path.sep), 'attachment escapes temporary root');
      assert.equal(digest(file), entry.sha256, `attachment ${entry.path} changed`);
    }
    console.log('PASS restored attachment contents');
    const start = spawnSync(binary, ['server', 'start'], { env: serverEnv, encoding: 'utf8', timeout: 120000 });
    assert.equal(start.status, 0, start.stderr || start.error?.message);
    started = true;
    const deadline = Date.now() + 30000;
    for (;;) {
      try { const response = await fetch(`http://127.0.0.1:${port}/api/session`, { signal: AbortSignal.timeout(2000) }); if (response.status === 200) break; }
      catch (error) { if (Date.now() > deadline) throw error; }
      if (Date.now() > deadline) throw new Error('restored server did not answer');
      await new Promise(resolve => setTimeout(resolve, 200));
    }
    console.log('PASS restored server answers');
    const doctor = spawnSync(binary, ['doctor', '--json'], { env: serverEnv, encoding: 'utf8', timeout: 60000 });
    assert.equal(doctor.status, 0, doctor.stdout + '\n' + doctor.stderr);
    console.log('PASS live doctor');
    const snapshot = path.join(scratch, 'restored-snapshot.tar.gz');
    run(binary, ['backup', '--out', snapshot], { env: serverEnv });
    const afterDB = path.join(scratch, 'after.sqlite');
    extract(snapshot, 'server/aimebu.sqlite', afterDB);
    compare(rows(afterDB), baseline, 'migrated');
  } finally {
    const stop = spawnSync(binary, ['server', 'stop', '--with-supervisor'], { env: serverEnv, encoding: 'utf8', timeout: 40000 });
    if (started) assert.equal(stop.status, 0, 'server cleanup failed: ' + stop.stderr);
    console.log('PASS server cleanup');
  }
}
main().catch(error => { console.error('FAIL backup upgrade: ' + error.message); process.exitCode = 1; });
