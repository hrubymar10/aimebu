#!/usr/bin/env node
// Serve the embedded UI and start a detached supervisor from an empty directory.
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const cp = require('node:child_process');

const source = process.argv[2];
if (process.argv.length !== 3 || !source || !fs.existsSync(source)) {
  console.error('usage: node scripts/test-standalone.cjs /path/to/aimebu');
  process.exit(2);
}

function freePort() {
  return new Promise((resolve, reject) => {
    const socket = net.createServer();
    socket.once('error', reject);
    socket.listen(0, '127.0.0.1', () => {
      const port = socket.address().port;
      socket.close(() => resolve(port));
    });
  });
}
function delay(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }
function command(binary, args, cwd, env) {
  return cp.spawnSync(binary, args, { cwd, env, encoding: 'utf8', timeout: 20000 });
}

async function main() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-standalone-'));
  const empty = path.join(root, 'empty');
  fs.mkdirSync(empty);
  const binary = path.join(empty, 'aimebu');
  fs.copyFileSync(path.resolve(source), binary);
  fs.chmodSync(binary, 0o755);
  for (const name of ['home', 'codex', 'claude', 'config']) fs.mkdirSync(path.join(root, name));
  const port = await freePort();
  const env = {
    HOME: path.join(root, 'home'),
    CODEX_HOME: path.join(root, 'codex'),
    CLAUDE_CONFIG_DIR: path.join(root, 'claude'),
    AIMEBU_CONFIG_DIR: path.join(root, 'config'),
    AIMEBU_SENTRY_DSN: 'off',
    AIMEBU_PORT: String(port),
    AIMEBU_BIND: '127.0.0.1',
    AIMEBU_OWNER_LOGIN: 'owner',
    AIMEBU_OWNER_PASSWORD: 'standalone test password',
    AIMEBU_USAGES: '0',
    PATH: process.platform === 'darwin' ? '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin' : '/usr/bin:/bin',
  };
  const logPath = path.join(root, 'server.log');
  const log = fs.openSync(logPath, 'w');
  const server = cp.spawn(binary, ['server'], { cwd: empty, env, stdio: ['ignore', log, log] });
  let failed = null;
  try {
    const deadline = Date.now() + 45000;
    let ready = false;
    while (Date.now() < deadline) {
      if (server.exitCode !== null) throw new Error(`server exited ${server.exitCode}`);
      try {
        const response = await fetch(`http://127.0.0.1:${port}/`, { signal: AbortSignal.timeout(2000) });
        if (response.ok && (await response.text()).includes('/bundle/app.js')) { ready = true; break; }
      } catch { /* server is still starting */ }
      await delay(250);
    }
    if (!ready) throw new Error('web UI did not start with its embedded bundle');
    const bundle = await fetch(`http://127.0.0.1:${port}/bundle/app.js`, { signal: AbortSignal.timeout(5000) });
    if (!bundle.ok || !(await bundle.arrayBuffer()).byteLength) throw new Error('embedded web bundle was empty');
    let supervisor = false;
    while (Date.now() < deadline) {
      const status = command(binary, ['supervisor', 'status'], empty, env);
      if (status.status === 0 && status.stdout.includes('supervisor is running')) { supervisor = true; break; }
      await delay(250);
    }
    if (!supervisor) throw new Error('detached supervisor did not start');
    console.log(`standalone web UI and detached supervisor passed on port ${port}`);
  } catch (error) {
    failed = error;
    console.error(`standalone test failed: ${error}\n${fs.readFileSync(logPath, 'utf8')}`);
  } finally {
    server.kill('SIGTERM');
    for (let attempt = 0; attempt < 40 && server.exitCode === null; attempt++) await delay(250);
    if (server.exitCode === null) server.kill('SIGKILL');
    command(binary, ['server', 'stop', '--with-supervisor'], empty, env);
    fs.closeSync(log);
    fs.rmSync(root, { recursive: true, force: true });
  }
  if (failed) process.exitCode = 1;
}
main().catch(error => { console.error(error); process.exitCode = 1; });
