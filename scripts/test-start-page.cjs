// What a sign-in opens when the browser has nothing to restore, a stale remembered room, a remembered room, or an address (a deep link), in
// Chromium with every request held until the test answers it or answered by rule, and the event stream a socket that answers ready at once.
// web/ is served as is with its import map and markup; app.js itself runs, against a mocked server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const types = { '.html': 'text/html', '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.json': 'application/json', '.webmanifest': 'application/manifest+json' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  const name = url.pathname === '/' || url.pathname === '/fixture.html' ? 'index.html' : url.pathname;
  const file = path.join(root, name);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(name)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const at = '2026-01-01T10:00:00Z';
const project = (id, name, patch = {}) => ({ id, name, role: 'owner', archived_at: '', ...patch });
const room = (id, projectID, title, patch = {}) => ({ id, project_id: projectID, title, kind: 'task', purpose: 'general', parent_id: '', lifecycle: 'open', unread_count: 0, hidden: false, notify: 'default', muted_until: '', member_count: 2, last_activity_at: at, agent_chat: false, lifecycle_rev: 1, ...patch });
const projects = [project('prj_a', 'Alpha'), project('prj_b', 'Beta')];
const rooms = () => ({
  prj_a: [room('rm_ah', 'prj_a', 'Alpha', { kind: 'home' }), room('rm_gen', 'prj_a', 'general', { parent_id: 'rm_ah' }), room('rm_api', 'prj_a', 'api-work', { parent_id: 'rm_ah' })],
  prj_b: [room('rm_bh', 'prj_b', 'Beta', { kind: 'home' }), room('rm_bg', 'prj_b', 'beta-general', { parent_id: 'rm_bh' })],
});

// fetch is held unless a rule answers it: a rule is a body, or { status, body }. Self-contained: it runs in the page before app.js starts.
// `setup` is plain data (evaluateOnNewDocument takes no functions): `{ lastRoom }` writes the remembered room before the app starts.
function bootMocks(auto, setup = {}) {
  window.__requests = []; window.__auto = auto; window.__sockets = [];
  window.fetch = (input, options = {}) => new Promise((resolve, reject) => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false, aborted: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    options.signal?.addEventListener('abort', () => { request.aborted = true; reject(new DOMException('aborted', 'AbortError')); });
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path}`];
    if (rule) request.answer(rule.status || 200, rule.status ? rule.body : rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
  // The event stream: a socket that comes up at once and says ready (no events after it), so the start is "Live" and stays quiet.
  window.WebSocket = class {
    constructor(url) { window.__sockets.push(this); setTimeout(() => { this.onopen?.(); this.onmessage?.({ data: JSON.stringify({ type: 'ready', seq: 99, heartbeat: 0 }) }); }, 0); }
    send() {} close() {}
  };
  try { localStorage.clear(); if (setup.lastRoom) localStorage.setItem('aimebu2.lastRoom', setup.lastRoom); } catch {}
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  // The answers a sign-in needs beside the lists. `harness_setup_skipped` keeps the Harnesses offer from opening the onboarding step.
  const startupRules = extra => ({
    'GET /api/session': { signed_in: true, csrf_token: 'token', id: 'usr_me', user_id: 'usr_me', login: 'me', owner: true, admin: true, first_run: false, telemetry_prompt: false },
    'GET /api/events/cursor': { seq: 7 },
    'GET /api/me/preferences': { preferences: { harness_setup_skipped: '1' } },
    'GET /api/buildinfo': { version: 'test-start' },
    'GET /api/retention': { messages_considered_expired_after_seconds: 0 },
    'GET /api/agent-isolation': { docker_only: false, reviewer_agents_enabled: false },
    'GET /api/inbox': [],
    'GET /api/health': { agents: [], supervisor_restart_pending: false, supervisor_upgrade_required: false },
    'GET /api/usages': [],
    'GET /api/system-events?limit=100&notable=1': [],
    'GET /api/machines': { machines: [] },
    'GET /api/import-v1/status': { state: 'idle' },
    'GET /api/agent-dms': { project_id: 'prj_agentdm', rooms: [], joinable: [] },
    'GET /api/update-status': {},
    'GET /api/team-flows': { templates: [] },
    'GET /api/team-flows/recommended-models': { rows: [] },
    'GET /api/projects/prj_a/team-flow': {},
    'GET /api/projects/prj_a/settings': { home_lead_only: false, reviewer_agents_enabled: false },
    'GET /api/projects/prj_a/legacy-agents?role=leader': { agents: [] },
    'GET /api/projects/prj_a/legacy-agents?role=dispatcher': { agents: [] },
    'GET /api/projects/prj_a/legacy-agents?role=advisor': { agents: [] },
    'GET /api/projects/prj_a/role-spawn-defaults': [],
    'GET /api/machines?project_id=prj_a': { machines: [] },
    'GET /api/limit-guard': {},
    ...Object.fromEntries(['rm_ah', 'rm_gen', 'rm_api', 'rm_bg', 'rm_bh', 'rm_gone'].flatMap(id => [
      [`GET /api/rooms/${id}/mention-users`, { logins: [] }],
      [`GET /api/rooms/${id}/messages?limit=25`, []],
      [`GET /api/rooms/${id}/messages?after=0&limit=100`, []],
      [`GET /api/rooms/${id}/agents`, []],
      [`GET /api/rooms/${id}/members`, { humans: [] }],
    ])),
    'GET /api/projects': projects,
    ...Object.fromEntries(Object.entries(rooms()).map(([id, list]) => [`GET /api/projects/${id}/rooms`, list])),
    ...extra,
  });
  const mount = async ({ hash = '', lastRoom = '', viewport = null } = {}) => {
    const context = await browser.createBrowserContext();
    const page = await context.newPage();
    if (viewport) await page.setViewport(viewport);
    page.on('pageerror', error => errors.push(`pageerror: ${error.message}`));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(`console: ${message.text()}`); });
    await page.evaluateOnNewDocument(bootMocks, startupRules(), { lastRoom });
    await page.goto(`${origin}/fixture.html${hash}`);
    // The page is up when the startup status is gone (ready) and the app view is shown.
    await page.waitForFunction(() => !document.querySelector('#app-view').hidden && document.querySelector('#startup-status').hidden, { timeout: 20000 });
    return page;
  };
  const state = page => page.evaluate(() => ({
    noRoom: document.querySelector('#app-view').classList.contains('no-room'),
    chatOpen: document.querySelector('#app-view').classList.contains('chat-open'),
    title: document.querySelector('#room-title').textContent,
    emptyShown: !!document.querySelector('#chat-empty').getClientRects().length,
    emptyText: document.querySelector('#chat-empty-text').textContent,
    emptyAction: document.querySelector('#chat-empty-action').textContent,
    error: document.querySelector('#chat-error').textContent,
    lastRoom: localStorage.getItem('aimebu2.lastRoom'),
    address: decodeURIComponent(location.hash.slice(1)),
    composerDisabled: document.querySelector('#composer textarea').disabled,
    activeRow: document.querySelector('.room-item.active .room-item-name')?.textContent || '',
  }));

  // 1. A clean start (no remembered room, no address) opens NO room: the empty main panel with the pick-a-room hint.
  {
    const page = await mount();
    const seen = await state(page);
    assert.equal(seen.noRoom, true, 'clean start: no room is selected');
    assert.equal(seen.chatOpen, false, 'clean start: the chat is not drawn');
    assert.equal(seen.title, 'Choose a room', 'clean start: the header says what to do');
    assert.equal(seen.emptyShown, true, 'clean start: the empty state shows');
    assert.equal(seen.emptyText, 'Pick a room from the list to read it and talk to its agents.', 'clean start: the pick-a-room hint');
    assert.equal(seen.error, '', 'clean start: no error line');
    assert.equal(seen.lastRoom, null, 'clean start: nothing remembered');
    assert.equal(seen.composerDisabled, true, 'clean start: the composer is off with no room');
    assert.equal(await page.$$eval('#projects .room-group', nodes => nodes.length) > 0, true, 'clean start: the list is still drawn');
    // The hint answers like the empty state always did: "Show rooms" opens the list (the mobile drawer, or the collapsed column).
    assert.equal(seen.emptyAction, 'Show rooms', 'clean start: the empty state keeps its action');
    await page.close();
  }
  // 2. A remembered room still opens, as today.
  {
    const page = await mount({ lastRoom: 'rm_gen' });
    const seen = await state(page);
    assert.equal(seen.noRoom, false, 'remembered room: a room is open');
    assert.equal(seen.chatOpen, true, 'remembered room: the chat is drawn');
    assert.equal(seen.title, 'general', 'remembered room: the room opened');
    assert.equal(seen.activeRow, 'general', 'remembered room: its row is active');
    assert.equal(seen.lastRoom, 'rm_gen', 'remembered room: still remembered');
    assert.equal(seen.address, 'rm_gen', 'remembered room: the address follows the room');
    assert.equal(await page.$eval('#chat-empty', node => getComputedStyle(node).display), 'none', 'remembered room: no empty state');
    await page.close();
  }
  // 3. A stale remembered room (gone from the list): no room, no error.
  {
    const page = await mount({ lastRoom: 'rm_gone' });
    const seen = await state(page);
    assert.equal(seen.noRoom, true, 'stale remembered room: no room is selected');
    assert.equal(seen.chatOpen, false, 'stale remembered room: the chat is not drawn');
    assert.equal(seen.emptyShown, true, 'stale remembered room: the hint shows');
    assert.equal(seen.error, '', 'stale remembered room: no error line');
    assert.equal(seen.title, 'Choose a room', 'stale remembered room: the header says what to do');
    assert.equal(errors.length, 0, `stale remembered room: nothing went wrong on the page: ${JSON.stringify(errors)}`);
    await page.close();
  }
  // 4. An explicit deep link opens its room, remembered room or not.
  {
    const page = await mount({ hash: '#rm_api' });
    const seen = await state(page);
    assert.equal(seen.noRoom, false, 'deep link: a room is open');
    assert.equal(seen.title, 'api-work', 'deep link: the addressed room opened');
    assert.equal(seen.address, 'rm_api', 'deep link: the address is kept');
    await page.close();
    // A deep link wins over a remembered room of another project.
    const both = await mount({ hash: '#rm_bg', lastRoom: 'rm_gen' });
    assert.equal(await both.$eval('#room-title', node => node.textContent), 'beta-general', 'deep link: the address wins over the remembered room');
    await both.close();
  }
  // 5. A phone with nothing to restore starts on the Rooms list, with no room selected.
  {
    const page = await mount({ viewport: { width: 390, height: 844 } });
    const seen = await state(page);
    assert.equal(seen.noRoom, true, 'phone: no room is selected');
    assert.equal(seen.chatOpen, false, 'phone: no chat is open');
    assert.equal(seen.activeRow, '', 'phone: no room row is active');
    assert.equal(await page.$eval('.sidebar', node => !!node.getClientRects().length && getComputedStyle(node).visibility === 'visible'), true, 'phone: the Rooms list is visible');
    assert.equal(await page.$eval('#projects', node => !!node.getClientRects().length), true, 'phone: the projects are visible in the list');
    assert.equal(await page.$eval('.chat', node => !!node.getClientRects().length), false, 'phone: the empty chat card does not cover the list');
    assert.equal(errors.length, 0, `phone: nothing went wrong on the page: ${JSON.stringify(errors)}`);
    await page.close();
  }

  await browser.close();
  server.close();
  if (errors.length) { console.error(errors.join('\n')); process.exit(1); }
  console.log('test-start-page: ok');
}

main().catch(error => { console.error(error); process.exit(1); });
