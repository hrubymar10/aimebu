const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const freePort = () => new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolve(port)); }); });

const OWNER = { login: 'owner', password: 'test-password-123' };

// A scratch server and a browser for a page test. With `owner` the server starts with that account; without it there is no owner yet and the
// server waits for its setup token (its path is in /api/setup/status). `AUTH_BINARY` names a binary built already, so several scripts share one.
async function scratch({ owner = OWNER, env: extraEnv = {} } = {}) {
  const tmp = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-auth-'));
  const home = path.join(tmp, 'home'); fs.mkdirSync(home, { recursive: true });
  const env = { ...process.env, HOME: home, CODEX_HOME: path.join(home, '.codex'), CLAUDE_CONFIG_DIR: path.join(home, '.claude'), AIMEBU_CONFIG_DIR: path.join(tmp, 'v1'), AIMEBU2_CONFIG_DIR: tmp, TMPDIR: tmp };
  let binary = process.env.AUTH_BINARY;
  if (!binary) {
    binary = path.join(tmp, 'aimebu');
    const build = spawnSync('go', ['build', '-o', binary, './cmd/aimebu'], { stdio: 'inherit', env });
    if (build.status !== 0) throw new Error('go build failed');
  }
  const port = await freePort(), origin = `http://127.0.0.1:${port}`;
  const serverEnv = { ...env, AIMEBU2_USAGES: '0', AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_PORT: String(port), AIMEBU2_SENTRY_DSN: 'off', ...extraEnv }; // error reports are off in every test server; a test can pass its own DSN, or null to leave it unset
  for (const [name, value] of Object.entries(extraEnv)) if (value === null) delete serverEnv[name];
  if (owner) { serverEnv.AIMEBU2_OWNER_LOGIN = owner.login; serverEnv.AIMEBU2_OWNER_PASSWORD = owner.password; }
  const server = spawn(binary, ['server'], { env: serverEnv, stdio: ['ignore', 'pipe', 'pipe'] });
  let logs = ''; server.stderr.on('data', data => { logs += data; }); server.stdout.on('data', data => { logs += data; });
  for (let i = 0; i < 300; i++) {
    try { if ((await fetch(origin)).ok) break; } catch { /* not up yet */ }
    await new Promise(resolve => setTimeout(resolve, 100));
    if (i === 299) throw new Error(`server did not start: ${logs}`);
  }
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const open = async ({ width = 1280, height = 900, reduced = true, context } = {}) => {
    const page = await (context || browser).newPage(); page.setDefaultTimeout(30000);
    await page.evaluateOnNewDocument(() => { try { Object.defineProperty(Notification, 'permission', { configurable: true, get: () => 'denied' }); localStorage.setItem('aimebu2.composer.formatting', 'off'); } catch { /* not available */ } });
    if (reduced) await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
    await page.setViewport({ width, height });
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    return page;
  };
  const api = (page, pathname, options = {}) => page.evaluate(async (pathname, options) => {
    const csrf = window.__csrf || '';
    const response = await fetch(pathname, { method: options.method || 'GET', headers: { 'Content-Type': 'application/json', ...(csrf ? { 'X-CSRF-Token': csrf } : {}) }, body: options.body ? JSON.stringify(options.body) : undefined });
    const text = await response.text(); let body = null; try { body = text ? JSON.parse(text) : null; } catch { body = text; }
    return { status: response.status, body };
  }, pathname, options);
  // Signs in as `who` through the API in this page's browser context and returns the session (with its CSRF token for later calls).
  const signIn = async (page, who = OWNER) => {
    const result = await page.evaluate(async who => {
      const response = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(who) });
      const body = await response.json(); window.__csrf = body.csrf_token; return { status: response.status, body };
    }, who);
    return result;
  };
  // The owner's calls from Node (no browser): a client with the owner's cookie and CSRF token, for what a test sets up before it opens a page.
  const ownerClient = async () => {
    const login = await fetch(`${origin}/api/login`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(OWNER) });
    const cookie = login.headers.getSetCookie().map(value => value.split(';')[0]).join('; '), session = await login.json();
    return async (pathname, options = {}) => {
      const response = await fetch(`${origin}${pathname}`, { method: options.method || 'GET', headers: { 'Content-Type': 'application/json', Cookie: cookie, 'X-CSRF-Token': session.csrf_token }, body: options.body ? JSON.stringify(options.body) : undefined });
      const text = await response.text(); let body = null; try { body = text ? JSON.parse(text) : null; } catch { body = text; }
      return { status: response.status, body };
    };
  };
  const context = () => browser.createBrowserContext();
  const stop = async () => {
    try { await browser.close(); } catch { /* gone */ }
    const exited = new Promise(resolve => { if (server.exitCode !== null || server.signalCode) resolve(); else server.once('close', resolve); });
    try { server.kill('SIGTERM'); } catch { /* gone */ }
    const timedOut = Symbol();
    if (await Promise.race([exited, new Promise(resolve => setTimeout(resolve, 10000, timedOut))]) === timedOut) {
      try { server.kill('SIGKILL'); } catch { /* gone */ }
      await exited;
    }
    fs.rmSync(tmp, { recursive: true, force: true });
  };
  return { origin, tmp, browser, open, api, signIn, ownerClient, context, stop, errors, logs: () => logs, OWNER };
}

module.exports = { scratch, OWNER, freePort };
