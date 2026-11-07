// The Health panel (the toolbar badge, the popover list, the limit rows and the disk section) in Chromium with every request held until the
// test answers it or answered by rule. web/ is served as is with its import map; no Go server. It runs unchanged against the page that
// replaced the hand-written panel. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const HEALTH = fs.existsSync(path.join(root, 'js', 'pages', 'health', 'page.js')) ? '/js/pages/health/page.js' : '/js/health.js';
const DISK = fs.existsSync(path.join(root, 'js', 'pages', 'health', 'disk.js')) ? '/js/pages/health/disk.js' : '/js/disk-usage.js';
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><main id="app-view"><button id="health-toggle" type="button" title="All agents healthy" aria-label="Health"><span id="health-count" hidden>0</span></button>
<div id="health-popover" class="health-popover popover-panel" hidden><p>All agents healthy</p></div></main></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const reset = '2030-01-02T03:04:05Z';
const agents = [
  { id: 'a1', name: 'olive', room_ids: ['r1'], flags: [{ code: 'stuck' }, { code: 'held_answer' }] },
  { id: 'a2', name: 'rex', room_ids: ['r2'], flags: [{ code: 'out_of_quota', reset_at: reset }] },
  { id: 'a3', name: 'fine', room_ids: ['r1'], flags: [] },
];
const guard = (soft = 'soft') => ({ profiles: {}, tools: {
  claude: { config: {}, held: soft === 'soft', result: { state: soft, profile: 'local', percent: 96, window: 'weekly', soft: 95, hard: 99 } },
  codex: { config: {}, held: false, result: { state: 'ok', profile: 'main', percent: 40, window: 'session', soft: 98, hard: 99 } } } });
const disk = (patch = {}) => ({ total_bytes: 4096, free_bytes: 5 * 1024 ** 3, volume_bytes: 100 * 1024 ** 3, low_disk: false, warnings: [], projects: [{ project_id: 'p1', name: 'Alpha', bytes: 3000, attachments: 1000, worktrees: 2000 }],
  categories: [{ label: 'Attachments', bytes: 1000 }], machines: [], worktrees: [{ room: 'task', room_id: 'r9', state: 'dirty', dirty_files: 3, bytes: 2000, path: '/tmp/w/task' }],
  settings: { enabled: true, hour: 4, attachment_days: 90 }, last_cleanup: null, ...patch });

// fetch is held unless a rule answers it: a rule is a body, or { status, body }.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path}`];
    if (rule) request.answer(rule.status || 200, rule.status ? rule.body : rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const answerAll = (extra = {}) => ({ 'GET /api/health': { agents }, 'GET /api/limit-guard': guard(), 'GET /api/disk': disk(), ...extra });
  const mount = async ({ auto = answerAll(), owner = true, refresh = true, cards = {} } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ owner, refresh, cards, module, disk }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner, admin: owner });
      const { Health } = await import(module);
      const { DiskUsage } = await import(disk);
      const rooms = new Map([['r1', { id: 'r1', title: 'One' }], ['r2', { id: 'r2', title: 'Two' }]]);
      window.__sidebar = { rooms, health: null, setHealth(rows) { this.health = rows.map(row => row.id); } };
      window.__agents = { cards: new Map(Object.entries(cards)), room: null };
      window.__calls = [];
      window.__item = true;
      window.__health = new Health(window.__sidebar, window.__agents, (room, agent) => window.__calls.push(['room', room?.id, agent]), async (row, flag) => { window.__calls.push(['item', row.id, flag.code]); if (window.__hold) await window.__hold; return window.__item; }, new DiskUsage());
      if (refresh) window.__refreshed = window.__health.refresh();
    }, { owner, refresh, cards, module: HEALTH, disk: DISK });
    return page;
  };
  const items = page => page.$$eval('#health-popover .health-item', nodes => nodes.map(node => [node.dataset.healthAgent, node.dataset.code, node.querySelector('.health-item-title').textContent, node.querySelector('.health-item-hint').textContent]));
  const badge = page => page.evaluate(() => { const toggle = document.querySelector('#health-toggle'), count = document.querySelector('#health-count'); return [count.textContent, count.hidden, toggle.title, toggle.getAttribute('aria-label'), toggle.classList.contains('has-issues'), toggle.getAttribute('aria-expanded'), toggle.getAttribute('aria-haspopup')]; });
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const open = async page => { await page.$eval('#health-toggle', el => el.click()); await page.waitForFunction(() => !document.querySelector('#health-popover').hidden); };
  const count = (page, method, requestPath) => page.evaluate((method, requestPath) => window.__requests.filter(request => request.method === method && request.path === requestPath).length, method, requestPath);
  try {
    // Nothing wrong: one calm line, a hidden count, and the toggle says so; the popover is a labelled dialog that opens and closes.
    {
      const page = await mount({ auto: answerAll({ 'GET /api/health': { agents: [{ id: 'a3', name: 'fine', room_ids: ['r1'], flags: [] }] }, 'GET /api/limit-guard': guard('ok'), 'GET /api/disk': { status: 403, body: { message: 'no' } } }) });
      await page.evaluate(() => window.__refreshed);
      assert.deepEqual(await badge(page), ['0', true, 'All agents healthy', 'Health', false, 'false', 'dialog']);
      assert.deepEqual(await page.evaluate(() => { const p = document.querySelector('#health-popover'); return [p.getAttribute('role'), p.getAttribute('aria-label'), p.hidden, [...p.querySelectorAll('.health-limit')].map(n => n.dataset.tool + ':' + n.dataset.state)]; }), ['dialog', 'Health', true, ['claude:ok', 'codex:ok']]);
      assert.equal(await page.$eval('#health-popover p:last-child', el => el.textContent), 'All agents healthy');
      await open(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#health-toggle').getAttribute('aria-expanded'), document.activeElement.id]), ['true', 'health-popover']);
      await page.keyboard.press('Escape');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#health-popover').hidden, document.querySelector('#health-toggle').getAttribute('aria-expanded'), document.activeElement.id]), [true, 'false', 'health-toggle']);
      await page.close();
    }
    // Problems: one entry per flag in plain words, the sidebar told which agents are affected, the badge counting agents plus held tools.
    {
      const page = await mount();
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length === 3);
      assert.deepEqual(await items(page), [
        ['a1', 'stuck', 'olive looks stuck', 'A turn has run with no sign of activity for a while. Open the agent to stop it or wait.'],
        ['a1', 'held_answer', 'An answer to olive could not be delivered', 'The agent wrote a reply that could not be posted. Open the Inbox to dismiss it or see the room.'],
        ['a2', 'out_of_quota', `rex is out of quota until ${await page.evaluate(reset => new Date(reset).toLocaleString(), reset)}`, 'The account hit its usage limit, so the agent cannot work until it resets or you switch accounts.']]);
      assert.deepEqual(await badge(page), ['3', false, '3 need attention', 'Health, 3 need attention', true, 'false', 'dialog']);
      assert.deepEqual(await page.evaluate(() => window.__sidebar.health), ['a1', 'a2']);
      assert.deepEqual(await page.$$eval('#health-popover .health-limit', nodes => nodes.map(node => [node.dataset.tool, node.dataset.state, node.querySelector('.health-limit-text').textContent, node.querySelector('.health-limit-fill').style.width, [...node.querySelectorAll('.health-limit-tick')].map(t => t.style.left)])),
        [['claude', 'soft', 'Claude (local) is at 96% of its weekly window: new turns are held, running ones finish.', '96%', ['95%', '99%']], ['codex', 'ok', 'Codex (main) is at 40% of its session window.', '40%', ['98%', '99%']]]);
      assert.deepEqual(await page.$$eval('#health-popover > *', nodes => nodes.map(node => node.className || node.tagName.toLowerCase())), ['health-disk', 'health-limits', 'row-btn health-item', 'row-btn health-item', 'row-btn health-item']);
      await page.close();
    }
    // An entry closes the list and goes to its Inbox item; one without an item (or a flag the Inbox does not hold) opens the agent's room.
    {
      const page = await mount();
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length === 3);
      await open(page);
      await page.$eval('[data-health-agent=a1][data-code=held_answer]', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__calls, document.querySelector('#health-popover').hidden]), [[['item', 'a1', 'held_answer']], true]);
      await page.evaluate(() => { window.__item = false; });
      await open(page);
      await page.$eval('[data-health-agent=a2]', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => window.__calls.slice(1)), [['item', 'a2', 'out_of_quota'], ['room', 'r2', 'a2']]);
      await page.close();
    }
    // Two reads answered out of order: the newer draws. A failed read leaves what is drawn.
    {
      const page = await mount({ auto: { 'GET /api/limit-guard': guard('ok'), 'GET /api/disk': { status: 403, body: { message: 'no' } } }, refresh: false });
      await page.evaluate(() => { window.__health.refresh(); window.__health.refresh(); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/health').length === 2);
      await answer(page, 1, 200, { agents: [agents0()] });
      await answer(page, 0, 200, { agents: [agents0(), agents0('b')] });
      function agents0(key = 'a') { return { id: key, name: key, room_ids: [], flags: [{ code: 'stuck' }] }; }
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length > 0);
      await settle(page);
      assert.equal(await page.$$eval('#health-popover .health-item', nodes => nodes.length), 1, 'the newer answer is the one drawn');
      await page.close();
    }
    // The limit guard is the owner's: a person who is not the owner never asks; a 403 or 404 is not asked again for that sign-in; opening the list asks again.
    {
      const other = await mount({ owner: false });
      await other.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length === 3);
      await open(other);
      assert.equal(await count(other, 'GET', '/api/limit-guard'), 0, 'no request that can only answer 403');
      assert.equal(await other.$$eval('#health-popover .health-limit', nodes => nodes.length), 0);
      await other.close();
      const gone = await mount({ auto: answerAll({ 'GET /api/limit-guard': { status: 403, body: { message: 'owner only' } } }) });
      await gone.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length === 3);
      await open(gone);
      await settle(gone);
      assert.equal(await count(gone, 'GET', '/api/limit-guard'), 1, 'a refusal is not asked again');
      assert.deepEqual(await badge(gone), ['2', false, '2 need attention', 'Health, 2 need attention', true, 'true', 'dialog']);
      await gone.close();
      const page = await mount();
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-limit').length === 2);
      assert.equal(await count(page, 'GET', '/api/limit-guard'), 1);
      await page.evaluate(() => window.__auto['GET /api/limit-guard'] = { profiles: {}, tools: { claude: { held: false, result: { state: 'ok', profile: 'local', percent: 10, window: 'weekly', soft: 95, hard: 99 } }, codex: { held: false, result: { state: 'off' } } } });
      await open(page);
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-limit').length === 1);
      assert.deepEqual(await badge(page), ['2', false, '2 need attention', 'Health, 2 need attention', true, 'true', 'dialog'], 'the tool no longer held leaves the count');
      assert.equal(await count(page, 'GET', '/api/limit-guard'), 2);
      await page.close();
    }
    // Disk: shown for those who can read it, hidden for everyone else; opening the list reads it again, the newer read wins.
    {
      const page = await mount({ auto: answerAll(), refresh: true });
      await page.waitForFunction(() => document.querySelector('#health-popover .health-item'));
      await open(page);
      await page.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      assert.deepEqual(await page.evaluate(() => ({ head: document.querySelector('.health-disk-head').textContent, projects: [...document.querySelectorAll('.health-disk-projects li')].map(li => [li.dataset.project, li.querySelector('.health-disk-name').textContent, li.querySelector('.health-disk-size').textContent, li.title]),
        kinds: [...document.querySelectorAll('.health-disk-kinds li')].map(li => li.textContent), trees: [...document.querySelectorAll('.health-disk-worktrees li')].map(li => [li.dataset.state, li.textContent, li.title]), foot: document.querySelector('.health-disk-foot').textContent, label: document.querySelector('#health-disk').getAttribute('aria-label'),
        settings: [document.querySelector('#cleanup-enabled').checked, document.querySelector('#cleanup-hour').value, document.querySelector('#cleanup-attachment-days').value] })),
        { head: 'Disk4.00 KBClean now', projects: [['p1', 'Alpha', '2.93 KB', 'Attachments 1000 B · Worktrees 1.95 KB'], ['', 'Other', '1.07 KB', 'Database, sounds, logs and anything that belongs to no project']], kinds: ['Attachments1000 B'], trees: [['dirty', 'taskUncommitted work (3 files)1.95 KB', '/tmp/w/task']],
          foot: '5.00 GB free of 100 GB. No cleanup has removed anything yet.', label: 'Disk usage', settings: [true, '4', '90'] });
      const cols = await page.$$eval('.health-disk-projects li', rows => rows.map(li => ['.health-disk-bar', '.health-disk-size'].map(sel => Math.round(li.querySelector(sel).getBoundingClientRect().left))));
      assert.ok(cols.length === 2 && cols.every(c => c[0] === cols[0][0] && c[1] === cols[0][1]), `bar and value columns line up: ${JSON.stringify(cols)}`);
      await page.close();
    }
    // A second writer's worktree is listed under its agent and has a Remove button (off while an agent still works in it); a room's own has none.
    {
      const trees = [{ room: 'task', room_id: 'r9', state: 'ready', bytes: 10, path: '/tmp/w/task' },
        { room: 'task · ada', agent: 'ada', agent_id: 'a1', room_id: 'r9', state: 'ready', bytes: 20, path: '/tmp/w/task-ada' },
        { room: 'task · bert', agent: 'bert', agent_id: 'a2', room_id: 'r9', state: 'active', bytes: 30, path: '/tmp/w/task-bert' }];
      const page = await mount({ auto: answerAll({ 'GET /api/disk': disk({ worktrees: trees }) }) });
      await page.evaluate(() => window.__refreshed);
      await page.waitForFunction(() => document.querySelector('#health-popover .health-item') || true);
      await open(page);
      await page.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.health-disk-worktrees li')].map(li => { const button = li.querySelector('.health-worktree-remove'); return [li.querySelector('span').textContent, button ? button.disabled : null]; })),
        [['task', null], ['task · ada', false], ['task · bert', true]]);
      await page.close();
    }
    // The disk reads race: the newer answer is drawn even when the older arrives last.
    {
      const page = await mount({ auto: { 'GET /api/health': { agents: [] }, 'GET /api/limit-guard': guard('ok') } });
      await page.evaluate(() => window.__refreshed);
      await page.$eval('#health-toggle', el => el.click());
      await waitRequest(page, 'GET', '/api/disk');
      await page.$eval('#health-toggle', el => el.click());
      await page.$eval('#health-toggle', el => el.click());
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/disk').length === 2);
      await answer(page, await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/disk' && !request.answered && request !== window.__requests.filter(r => r.path === '/api/disk')[0])), 200, disk({ total_bytes: 2048 }));
      await answer(page, await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/disk' && !request.answered)), 200, disk({ total_bytes: 1024 }));
      await page.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      await settle(page);
      assert.equal(await page.$eval('.health-disk-total', el => el.textContent), '2.00 KB', 'the newer read is the one drawn');
      await page.close();
    }
    // Clean now: it shows what it would remove, a second press removes it, a refresh does not wipe the preview, and a failure says why.
    {
      const plan = { items: [{ action: 'would_remove', target: '/x/y/z/old.log', bytes: 2048 }, { action: 'kept', target: '/x/keep', bytes: 10 }], bytes: 2048 };
      const page = await mount({ auto: answerAll({ 'GET /api/disk': disk(), 'POST /api/cleanup': undefined }) });
      await page.waitForFunction(() => document.querySelector('#health-popover .health-item'));
      await open(page);
      await page.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      await page.$eval('.health-disk-clean', el => el.click());
      const dry = await waitRequest(page, 'POST', '/api/cleanup');
      assert.equal(await page.$eval('.health-disk-plan', el => el.textContent), 'Checking what can be removed…');
      await answer(page, dry, 200, plan);
      await page.waitForFunction(() => document.querySelector('.health-disk-items'));
      assert.deepEqual(await page.evaluate(() => ({ summary: document.querySelector('.health-disk-summary').textContent, rows: [...document.querySelectorAll('.health-disk-items li')].map(li => [li.dataset.action, li.textContent, li.title]),
        buttons: [...document.querySelectorAll('.health-disk-actions button')].map(b => [b.textContent, b.disabled]), region: document.querySelector('.health-disk-plan').getAttribute('aria-label'), focus: document.activeElement.textContent, body: window.__requests.at(-1).body })),
        { summary: '1 item, 2.00 KB would be removed. Nothing has been removed yet.', rows: [['would_remove', 'Will removez/old.log2.00 KB', '/x/y/z/old.log'], ['kept', 'Keptx/keep10 B', '/x/keep']], buttons: [['Remove 2.00 KB', false], ['Cancel', false]], region: 'Cleanup preview', focus: 'Remove 2.00 KB', body: { dry_run: true } });
      await page.evaluate(() => { window.__health.disk.refresh(); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/disk').length >= 2);
      await settle(page);
      assert.equal(await page.$$eval('.health-disk-items li', rows => rows.length), 2, 'a refresh that lands while the preview is open does not wipe it');
      await page.$eval('.health-disk-actions .btn-primary', el => el.click());
      const real = await waitRequest(page, 'POST', '/api/cleanup');
      assert.equal(await page.$eval('.health-disk-plan', el => el.textContent), 'Cleaning up…');
      await answer(page, real, 200, { items: [{ action: 'removed', target: '/x/y/z/old.log', bytes: 2048 }], bytes: 2048 });
      await page.waitForFunction(() => document.querySelector('.health-disk-plan')?.textContent.startsWith('Removed 1 item'));
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.health-disk-plan').textContent, window.__requests.filter(request => request.method === 'POST').map(request => request.body)]), ['Removed 1 item, 2.00 KB.', [{ dry_run: true }, { dry_run: false }]]);
      await page.close();
      const failing = await mount({ auto: answerAll({ 'POST /api/cleanup': { status: 500, body: { message: 'Cleanup is not available' } } }) });
      await failing.waitForFunction(() => document.querySelector('#health-popover .health-item'));
      await open(failing);
      await failing.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      await failing.$eval('.health-disk-clean', el => el.click());
      await failing.waitForFunction(() => document.querySelector('.health-disk-plan .error')?.textContent === 'Cleanup is not available');
      await failing.close();
    }
    // The daily cleanup's settings save what changed; a bad retention is refused with its reason and the box goes back.
    {
      const page = await mount({ auto: answerAll({ 'PUT /api/cleanup/settings': {} }) });
      await page.waitForFunction(() => document.querySelector('#health-popover .health-item'));
      await open(page);
      await page.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      await page.$eval('#cleanup-enabled', el => el.click());
      await page.select('#cleanup-hour', '6');
      await page.$eval('#cleanup-attachment-days', el => { el.value = '30'; el.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => window.__requests.filter(request => request.method === 'PUT').length === 3);
      assert.deepEqual(await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').map(request => request.body)), [{ enabled: false }, { hour: 6 }, { attachment_days: 30 }]);
      await page.$eval('#cleanup-attachment-days', el => { el.value = '99999'; el.dispatchEvent(new Event('change', { bubbles: true })); });
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.health-disk-plan .error').textContent, document.querySelector('#cleanup-attachment-days').value, window.__requests.filter(request => request.method === 'PUT').length]), ['Enter a number of days from 0 to 3650.', '90', 3]);
      await page.close();
    }
    // The disk section does not depend on the agent read: when that fails the popover holds its one line, with the disk section above it.
    {
      const page = await mount({ auto: answerAll({ 'GET /api/health': { status: 500, body: { message: 'down' } } }) });
      await open(page);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [!!document.querySelector('#health-disk'), [...document.querySelectorAll('#health-popover > *')].map(node => node.tagName.toLowerCase() + (node.className ? `.${node.className}` : '')), document.querySelector('#health-popover').textContent.includes('All agents healthy')]), [true, ['section.health-disk', 'div.health-limits', 'p'], true]);
      await page.close();
    }
    // A panel destroyed while a read or a write is held sends nothing more and draws nothing.
    {
      const read = await mount({ auto: { 'GET /api/limit-guard': guard(), 'GET /api/disk': disk() } });
      const held = await waitRequest(read, 'GET', '/api/health');
      await read.evaluate(() => window.__health.destroy());
      await answer(read, held, 200, { agents });
      await settle(read);
      assert.deepEqual(await read.evaluate(() => [window.__sidebar.health, window.__requests.length, document.querySelectorAll('#health-popover .health-item').length, document.querySelector('#health-count').textContent]), [null, 1, 0, '0'], 'a read answered after destroy draws nothing and asks for nothing more');
      await read.evaluate(() => { window.__health.schedule(); window.__health.refresh(); window.__health.refreshLimits(); });
      await new Promise(resolve => setTimeout(resolve, 700));
      assert.equal(await read.evaluate(() => window.__requests.length), 1, 'a destroyed panel sends no read');
      await read.close();
      const preview = await mount({ auto: answerAll() });
      await preview.waitForFunction(() => document.querySelector('#health-popover .health-item'));
      await open(preview);
      await preview.waitForFunction(() => !document.querySelector('#health-disk').hidden);
      await preview.$eval('.health-disk-clean', el => el.click());
      const dry = await waitRequest(preview, 'POST', '/api/cleanup');
      await preview.evaluate(() => window.__health.destroy());
      await answer(preview, dry, 200, { items: [{ action: 'would_remove', target: '/a/b', bytes: 1 }], bytes: 1 });
      await settle(preview);
      assert.deepEqual(await preview.evaluate(() => [window.__health.disk.plan.value, window.__requests.filter(request => request.method === 'POST').length]), [{ checking: true }, 1], 'a preview answered after destroy draws nothing');
      await preview.evaluate(async () => { await window.__health.disk.run(); await window.__health.disk.preview(); await window.__health.disk.save({ enabled: true }); await window.__health.disk.refresh(); });
      assert.equal(await preview.evaluate(() => window.__requests.filter(request => request.method !== 'GET').length), 1, 'a destroyed disk panel sends no cleanup and no setting');
      await preview.close();
    }
    // An entry whose Inbox lookup is still held when the panel is destroyed goes nowhere once it answers.
    {
      const page = await mount();
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-item').length === 3);
      await open(page);
      await page.evaluate(() => { window.__item = false; window.__hold = new Promise(resolve => { window.__unhold = resolve; }); document.querySelector('[data-health-agent=a2]').click(); });
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'item'));
      await page.evaluate(() => { window.__health.destroy(); window.__unhold(); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'room')), [], 'a destroyed panel opens no room');
      await page.close();
    }
    // Two reads of the limit guard answered out of order: the newer one is the one drawn and counted.
    {
      const page = await mount({ auto: { 'GET /api/health': { agents: [{ id: 'a3', name: 'fine', room_ids: [], flags: [] }] }, 'GET /api/disk': { status: 403, body: { message: 'no' } } } });
      await waitRequest(page, 'GET', '/api/limit-guard');
      await page.$eval('#health-toggle', el => el.click());
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/limit-guard').length === 2);
      await answer(page, await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/limit-guard' && request !== window.__requests.filter(r => r.path === '/api/limit-guard')[0])), 200, guard('ok'));
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-limit').length === 2);
      await answer(page, await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/limit-guard' && !request.answered)), 200, guard('soft'));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#health-count').hidden, [...document.querySelectorAll('#health-popover .health-limit')].map(n => n.dataset.state)]), [true, ['ok', 'ok']], 'an older read does not overwrite a newer one');
      await page.close();
    }
    // An older refusal arriving after a newer success does not stop the limit guard being asked again.
    {
      const page = await mount({ auto: { 'GET /api/health': { agents: [] }, 'GET /api/disk': { status: 403, body: { message: 'no' } } } });
      const older = await waitRequest(page, 'GET', '/api/limit-guard');
      await page.$eval('#health-toggle', el => el.click());
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/limit-guard').length === 2);
      await answer(page, await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/limit-guard' && request !== window.__requests.filter(r => r.path === '/api/limit-guard')[0])), 200, guard('ok'));
      await page.waitForFunction(() => document.querySelectorAll('#health-popover .health-limit').length === 2);
      await answer(page, older, 403, { message: 'owner only' });
      await settle(page);
      await page.evaluate(() => { window.__auto['GET /api/limit-guard'] = { profiles: {}, tools: {} }; });
      await page.evaluate(() => window.__health.refreshLimits());
      await settle(page);
      assert.equal(await count(page, 'GET', '/api/limit-guard'), 3, 'the guard is still asked after an obsolete refusal');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('health page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });
