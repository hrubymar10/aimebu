#!/usr/bin/env node
// Smoke a real binary upgrade with a scratch Claude protocol stub.
// usage: node scripts/upgrade-smoke.cjs OLD_BINARY NEW_BINARY SCRATCH_DIR
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const net = require('node:net');
const path = require('node:path');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');

if (process.argv.length !== 5) {
  console.error(`usage: ${process.argv[1]} OLD_BINARY NEW_BINARY SCRATCH_DIR`);
  process.exit(2);
}
const isFile = p => { try { return fs.statSync(p).isFile(); } catch { return false; } };
const isDir = p => { try { return fs.statSync(p).isDirectory(); } catch { return false; } };
const OLD = path.resolve(process.argv[2]);
const NEW = path.resolve(process.argv[3]);
const PARENT = path.resolve(process.argv[4]);
const STATE = path.resolve(__dirname, 'test-upgrade-state.cjs');
if (!isFile(OLD) || !isFile(NEW) || !isDir(PARENT)) {
  console.error('old binary, new binary, and an existing scratch directory required');
  process.exit(1);
}

// The stand-in `claude`: speaks just enough of the harness protocol to hold a turn and a command across the upgrade.
const STUB = String.raw`#!/usr/bin/env node
'use strict';
const fs = require('node:fs');
const path = require('node:path');
if (process.argv.includes('--version')) { console.log('2.1.0'); process.exit(0); }
const root = path.dirname(path.dirname(fs.realpathSync(__filename)));
const args = process.argv;
const config = JSON.parse(args[args.indexOf('--mcp-config') + 1]);
const entry = Object.values(config.mcpServers)[0];
const url = entry.env.AIMEBU_URL + '/api/agent/mcp';
const key = process.env.AIMEBU_AGENT_KEY;
const session = args.includes('--session-id') ? args[args.indexOf('--session-id') + 1] : '00000000-0000-4000-8000-000000000001';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const stamp = () => new Date().toISOString().replace(/\.\d+Z$/, 'Z');
async function main() {
  const payload = JSON.parse(fs.readFileSync(0, 'utf8'));
  const note = payload.notes.find(n => (n.body || '').includes('release-probe')) || payload.notes[payload.notes.length - 1];
  const old = note.body.includes('old-release-probe');
  const kind = old ? 'old' : 'new';
  const event = msg => fs.appendFileSync(path.join(root, 'stub.log'), stamp() + ' ' + kind + ' ' + msg + '\n');
  event('started turn=' + payload.turn_id);
  if (old) {
    const request = { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'run_command', arguments: {
      cmd: 'while [ ! -f ' + path.join(root, 'job.release') + ' ]; do sleep 0.2; done; echo job-finished',
      request_key: 'upgrade-job-1', room_id: note.room_id, label: 'upgrade-job', timeout_seconds: 120 } } };
    const response = await fetch(url, { method: 'POST', body: JSON.stringify(request),
      headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json' }, signal: AbortSignal.timeout(40000) });
    const result = await response.json();
    const data = result.result || {};
    if (data.isError || result.error) { event('job_failed ' + JSON.stringify(result)); process.exit(2); }
    const job = JSON.parse(data.content[0].text);
    fs.writeFileSync(path.join(root, 'job.id'), job.id);
    fs.writeFileSync(path.join(root, 'old.turn'), payload.turn_id);
    event('job_started id=' + job.id + ' state=' + job.state);
    while (!fs.existsSync(path.join(root, 'turn.release'))) await sleep(100);
  } else {
    fs.writeFileSync(path.join(root, 'new.turn'), payload.turn_id);
  }
  const answer = { messages: [{ room: note.room_id, body: kind + '-answered', reply_to: note.message_id }] };
  console.log(JSON.stringify({ type: 'system', subtype: 'init', session_id: session }));
  console.log(JSON.stringify({ type: 'result', subtype: 'success', is_error: false, result: JSON.stringify(answer), structured_output: answer, session_id: session, usage: { input_tokens: 5, output_tokens: 5 } }));
  event('answered');
}
main().catch(e => { console.error(e); process.exit(1); });
`;

const sleep = ms => new Promise(r => setTimeout(r, ms));
const stamp = () => new Date().toISOString().replace(/\.\d+Z$/, 'Z');
const log = (...a) => console.log(...a);
const T = fs.mkdtempSync(path.join(PARENT, 'upgrade-'));
for (const name of ['bin', 'home', 'codex', 'claude', 'project']) fs.mkdirSync(path.join(T, name));
const stub = path.join(T, 'bin', 'claude');
fs.writeFileSync(stub, STUB);
fs.chmodSync(stub, 0o700);
const C = path.join(T, 'config-' + process.pid);
const touch = name => fs.writeFileSync(path.join(T, name), '');
const exists = name => fs.existsSync(path.join(T, name));
const env = {
  ...process.env, HOME: path.join(T, 'home'), CODEX_HOME: path.join(T, 'codex'), CLAUDE_CONFIG_DIR: path.join(T, 'claude'),
  AIMEBU_CONFIG_DIR: C, AIMEBU_SENTRY_DSN: 'off', AIMEBU_BIND: '127.0.0.1',
  AIMEBU_OWNER_LOGIN: 'owner', AIMEBU_OWNER_PASSWORD: 'correct horse battery staple',
  AIMEBU_WEB_BUNDLE: 'off', AIMEBU_USAGES: '0', AIMEBU_ALLOW_NATIVE: '1', AIMEBU_WARM_IDLE: '0',
  PATH: path.join(T, 'bin') + ':' + process.env.PATH,
};
delete env.AIMEBU_URL;

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => resolve(port)); });
  });
}
const cookies = new Map();
let csrf = '';
let base = '';
let oldpid = null;

function cli(which, ...args) {
  const p = spawnSync(which === 'old' ? OLD : NEW, args, { env, encoding: 'utf8', timeout: 120000 });
  const out = (p.stdout || '').trim(), err = (p.stderr || '').trim();
  log('CLI', stamp(), which, ...args, 'exit', p.status, JSON.stringify(out), JSON.stringify(err));
  if (p.status !== 0) throw new Error('CLI failed');
  return p.stdout;
}
async function req(method, p, data, key) {
  const headers = {};
  if (data !== undefined) headers['Content-Type'] = 'application/json';
  if (csrf && method !== 'GET') headers['X-CSRF-Token'] = csrf;
  if (key) headers['Idempotency-Key'] = key;
  if (cookies.size) headers.Cookie = [...cookies].map(([k, v]) => k + '=' + v).join('; ');
  const response = await fetch(base + p, { method, headers, body: data !== undefined ? JSON.stringify(data) : undefined, signal: AbortSignal.timeout(15000) });
  for (const c of response.headers.getSetCookie()) {
    const [pair] = c.split(';');
    const i = pair.indexOf('=');
    cookies.set(pair.slice(0, i).trim(), pair.slice(i + 1));
  }
  const text = await response.text();
  if (!response.ok) {
    log('HTTP_ERROR', stamp(), method, p, response.status, text.slice(0, 700));
    throw new Error(`HTTP ${response.status}`);
  }
  const result = JSON.parse(text);
  if ((method !== 'GET' && p !== '/api/login') || p === '/api/buildinfo' || p === '/api/machines') {
    log('HTTP', stamp(), method, p, response.status, JSON.stringify(result).slice(0, 700));
  }
  return result;
}
async function until(label, predicate, seconds = 60) {
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline) {
    const v = await predicate();
    if (v) { log('PASS', stamp(), label, v); return v; }
    await sleep(200);
  }
  throw new Error('timeout: ' + label);
}
function alive(pid) {
  const p = spawnSync('ps', ['-o', 'stat=', '-p', String(pid)], { encoding: 'utf8' });
  return p.status === 0 && p.stdout.trim() !== '' && !p.stdout.trim().startsWith('Z');
}
const messages = room => req('GET', '/api/rooms/' + room + '/messages');
const answered = async (room, body) => (await messages(room)).some(m => m.body === body && m.author_kind === 'agent');
const node = (args, e) => {
  const p = spawnSync(process.execPath, args, { env: e, encoding: 'utf8', timeout: 30000 });
  if (p.status !== 0) { process.stderr.write(p.stderr || ''); throw new Error('state helper failed: ' + args[1]); }
  return p.stdout.trim();
};
const pyBool = v => (v ? 'True' : 'False');

async function main() {
  const port = await freePort();
  env.AIMEBU_PORT = String(port);
  base = 'http://127.0.0.1:' + port;
  const oldVersion = cli('old', 'version').trim();
  const newVersion = cli('new', 'version').trim();
  cli('old', 'server', 'start');
  const login = await req('POST', '/api/login', { login: 'owner', password: 'correct horse battery staple' });
  csrf = login.csrf_token;
  const stateEnv = { ...env, AIMEBU_TEST_COOKIE: [...cookies].map(([k, v]) => k + '=' + v).join('; '), AIMEBU_TEST_CSRF: csrf };
  const project = await req('POST', '/api/projects', { name: 'upgrade-probe', folder: path.join(T, 'project') });
  const rooms = await req('GET', '/api/projects/' + project.id + '/rooms');
  const room = rooms.find(r => r.kind === 'task').id;
  await until('old machine connected', async () => (await req('GET', '/api/machines')).machines.some(m => m.connected && m.supervisor_version === oldVersion));
  const machine = (await req('GET', '/api/machines')).machines[0].id;
  const created = await req('POST', '/api/machines/' + machine + '/launchers', { tool: 'claude', name: 'scratch', command: stub, enabled: true, supports_toolbox: true, visible_roots: [path.join(T, 'project')] });
  const claude = created.config;
  await req('PUT', '/api/machines/' + machine + '/launchers/' + claude.id + '/models', { models: [{ slug: 'haiku', label: 'Haiku', default: true }] });
  const launcherReady = async () => (await req('GET', '/api/machines/' + machine + '/launchers')).launchers.some(row => row.config.id === claude.id && row.available);
  await until('scratch launcher available', launcherReady);
  await req('POST', '/api/rooms/' + room + '/agents', { name: 'oldwork', role_key: 'worker', tool: 'claude', variant: 'native', launcher_id: claude.id, model: 'haiku' }, 'old-agent');
  const attachment = node([STATE, 'seed', base, room], stateEnv);
  await req('POST', '/api/rooms/' + room + '/messages', { body: '@oldwork old-release-probe' });
  await until('old job started', () => exists('job.id'));
  const jobid = fs.readFileSync(path.join(T, 'job.id'), 'utf8');
  log('OLD_TURN', fs.readFileSync(path.join(T, 'old.turn'), 'utf8'), 'JOB', jobid);
  oldpid = Number(/pid (\d+)/.exec(cli('old', 'supervisor', 'status'))[1]);
  assert(alive(oldpid));
  const snapshot = path.join(T, 'seed-state.json');
  node([STATE, 'snapshot', base, project.id, room, attachment, snapshot], stateEnv);
  log('PASS seeded projects rooms messages agents attachments and in-flight turn');
  cli('old', 'server', 'stop');
  assert(alive(oldpid));
  cli('new', 'server', 'start');
  log('BUILD', JSON.stringify(await req('GET', '/api/buildinfo')));
  node([STATE, 'compare', base, project.id, room, attachment, snapshot], stateEnv);
  await until('new machine connected', async () => (await req('GET', '/api/machines')).machines.some(m => m.connected && m.supervisor_version === newVersion));
  log('OLD_ALIVE_DURING_NEW', oldpid, pyBool(alive(oldpid)));
  assert(alive(oldpid));
  const journals = () => { try { return fs.readdirSync(path.join(C, 'supervisor'), { recursive: true }).filter(f => f.endsWith('.sqlite')).map(f => path.join(C, 'supervisor', f)); } catch { return []; } };
  log('JOURNALS_DURING_OVERLAP', JSON.stringify(journals()));
  await until('scratch launcher available after swap', launcherReady);
  await req('POST', '/api/rooms/' + room + '/agents', { name: 'newwork', role_key: 'reviewer', tool: 'claude', variant: 'native', launcher_id: claude.id, model: 'haiku' }, 'new-agent');
  await req('POST', '/api/rooms/' + room + '/messages', { body: '@newwork new-release-probe' });
  await until('new answer', () => answered(room, 'new-answered'));
  assert(alive(oldpid));
  log('OLD_STILL_RUNNING', oldpid, pyBool(alive(oldpid)));
  touch('turn.release');
  await until('old answer', () => answered(room, 'old-answered'));
  assert(alive(oldpid));
  log('OLD_ALIVE_WITH_JOB', oldpid, pyBool(alive(oldpid)));
  touch('job.release');
  await until('job exit 0 note', async () => (await messages(room)).find(m => m.author_kind === 'system' && (m.body || '').startsWith('Command upgrade-job: finished (exit 0'))?.body);
  await until('old supervisor exited', () => !alive(oldpid));
  await until('old journal removed', () => !fs.existsSync(path.join(C, 'supervisor', 'journal.sqlite')));
  log('FINAL_MESSAGES', JSON.stringify(await messages(room)));
  log('FINAL_AGENTS', JSON.stringify(await req('GET', '/api/rooms/' + room + '/agents')));
  const events = (await req('GET', '/api/system-events')).events;
  const lost = events.map(e => e.kind).filter(k => k.includes('lost') || k.includes('interrupt'));
  log('LOST_OR_INTERRUPTED_EVENTS', JSON.stringify(lost));
  assert(lost.length === 0);
  log('FINAL_JOURNALS', JSON.stringify(journals()));
  const doctor = spawnSync(NEW, ['doctor', '--json'], { env, encoding: 'utf8', timeout: 30000 });
  log('DOCTOR_EXIT', doctor.status);
  log('DOCTOR_SCHEMA', JSON.stringify(JSON.parse(doctor.stdout).checks.filter(x => x.name === 'sqlite database')));
  assert.equal(doctor.status, 0);
  if (process.env.AIMEBU_UPGRADE_BACKUP_OUT) {
    cli('new', 'backup', '--out', process.env.AIMEBU_UPGRADE_BACKUP_OUT);
    log('PASS seeded backup archive');
  }
  log('LOG_EXCERPTS');
  for (const name of ['server/aimebu.log', 'supervisor/aimebu.log']) {
    const text = fs.readFileSync(path.join(C, name), 'utf8');
    assert(!['unexpected machine frame', 'turn_lost', 'agent_keys'].some(x => text.toLowerCase().includes(x)));
    log(name);
    for (const line of text.split('\n')) {
      if (['supervisor', 'migration 14', 'turn answered', 'turn_lost', 'unexpected machine frame', 'agent_keys', 'command'].some(x => line.toLowerCase().includes(x))) log(line.slice(0, 700));
    }
  }
}

async function cleanup() {
  touch('turn.release');
  touch('job.release');
  for (const which of ['new', 'old']) {
    const p = spawnSync(which === 'old' ? OLD : NEW, ['server', 'stop', '--with-supervisor'], { env, encoding: 'utf8', timeout: 30000 });
    log('CLEANUP_CLI', which, p.status, JSON.stringify((p.stdout || '').trim()), JSON.stringify((p.stderr || '').trim()));
  }
  let remaining = [];
  for (let i = 0; i < 50; i++) {
    const p = spawnSync('ps', ['-axo', 'pid,command'], { encoding: 'utf8' });
    remaining = p.stdout.split('\n').filter(line => {
      const parts = line.trim().split(/\s+(.*)/s);
      return parts.length >= 2 && parts[1] && [OLD, NEW, stub].some(x => parts[1].startsWith(x));
    });
    if (!remaining.length) break;
    await sleep(200);
  }
  log('CLEANUP_PROCESS_LINES', JSON.stringify(remaining));
  assert(remaining.length === 0);
  fs.rmSync(T, { recursive: true, force: true });
}

(async () => {
  let failure = null;
  try { await main(); } catch (e) { failure = e; console.error(e); }
  try { await cleanup(); } catch (e) { failure = failure || e; console.error(e); }
  process.exit(failure ? 1 : 0);
})();
