// The project's settings form (the home room's settings) and the room's agent control and usage in Chromium, with requests held until the
// test answers them or answered by rule. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="room-info-dialog" open><section id="room-project"><div id="project-settings-body" class="settings-subsection project-settings-body"></div></section>
<section id="room-scope-control" class="scope-control" aria-label="Room agent control"></section><section id="room-usage" class="usage-summary" aria-label="Room usage"></section></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const limit = { rev: 3, cost_mode: 'inherit', turn_mode: 'custom', daily_turn_cap: 50, daily_cost_cap: null, inherited_cost_cap: 5, inherited_turn_cap: null, day_start: '2026-02-03T00:00:00Z', tz: 'UTC', daily_turns: 2, daily_cost: 0.5, cost_coverage: 'known', effective_state: 'active' };
const usage = { turns: 4, by_agent: [{ key: 'a1', label: 'leader', turns: 4 }], by_model: [] };
const roles = [{ role_key: 'worker', inherited: true, global_agents: [{}, {}], default_agents: [], pick_mode: 'in_turn' }, { role_key: 'leader', inherited: false, global_agents: [], default_agents: [{ launcher_id: 'l1', model: 'm1' }], pick_mode: 'in_turn' }];
const rule = (status, body) => ({ status, body });
const base = () => ({
  'GET /api/projects/p1/control': { desired: 'active', effective_state: 'active', rev: 7 },
  'GET /api/projects/p1/usage-limit': limit,
  'GET /api/projects/p1/usage?range=today': usage,
  'GET /api/projects/p1/role-spawn-defaults': roles,
  'GET /api/machines?project_id=p1': { machines: [{ id: 'm1', name: 'Box' }] },
  'GET /api/spawn-catalog?machine_id=m1': { tools: [{ launcher_id: 'l1', label: 'Claude', models: [{ id: 'm1', label: 'Opus' }, { id: 'm2', label: 'Sonnet' }] }] },
});

function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, headers: options.headers || {}, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const found = window.__auto[`${request.method} ${request.path}`];
    if (found) request.answer(found.status || 200, found.status ? found.body : found);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  // `held` lists the project source methods to hold; the project is p1 and the person's role and admin flag are chosen per test.
  const mount = async ({ auto = base(), role = 'owner', admin = false, archived = false, held = [], load = true, settings = { auto_resume_after_clean_diagnostic: true, worktree_mode: 'auto', home_lead_only: false }, rooms = [{ id: 'r1', project_id: 'p1', kind: 'home' }] } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ role, admin, archived, held, load, settings, rooms }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: admin, admin });
      const { ProjectSettings } = await import('/js/pages/project/page.js');
      window.__calls = []; window.__waiting = {}; window.__hooks = [];
      const wrap = (name, run) => (...args) => {
        window.__calls.push([name, ...args]);
        if (!held.includes(name)) return run(...args);
        return new Promise((resolve, reject) => { (window.__waiting[name] ||= []).push(() => Promise.resolve(run(...args)).then(resolve, reject)); });
      };
      const source = { get: wrap('get', async () => structuredClone(settings)), update: wrap('update', async (id, fields) => fields), edit: wrap('edit', async (id, fields) => ({ id, name: fields.name ?? 'Alpha', folder: fields.folder ?? '/srv/alpha' })) };
      window.__release = name => (window.__waiting[name] || []).shift()?.();
      const project = { id: 'p1', name: 'Alpha', folder: '/srv/alpha', role, ...(archived ? { archived_at: '2026-02-03T04:05:06Z' } : {}) };
      const settingsPage = window.__project = new ProjectSettings(source, () => ({ projects: [project, { ...project, id: 'p2' }], rooms, selectedProjectID: 'p1' }));
      settingsPage.onLifecycle = async id => { window.__hooks.push(['lifecycle', id]); };
      settingsPage.onRemoved = async id => { window.__hooks.push(['removed', id]); };
      window.__room = new (await import('/js/pages/project/room-panels.js')).RoomScopeControl(document.querySelector('#room-scope-control'));
      window.__roomUsage = new (await import('/js/pages/project/room-panels.js')).RoomUsage(document.querySelector('#room-usage'));
      if (load) await settingsPage.load('p1');
    }, { role, admin, archived, held, load, settings, rooms });
    return page;
  };
  const ready = page => page.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled && document.querySelector('.usage-limit-summary').textContent.includes('turns /') && document.querySelectorAll('.project-role-default').length === 2 && document.querySelector('.usage-summary-body dl'));
  const calls = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).map(request => [request.path, request.body]), method);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const confirmOk = async page => { await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.$eval('#app-dialog .app-dialog-ok', el => el.click()); };
  try {
    // The editable project folder uses the shared directory completion field.
    {
      const page = await mount({ admin: true });
      await ready(page);
      await page.focus('#project-details-folder');
      await page.$eval('#project-details-folder', input => { input.value = '/srv/al'; input.dispatchEvent(new Event('input', { bubbles: true })); });
      const index = await waitRequest(page, 'GET', '/api/machines/directories?path=%2Fsrv%2F&prefix=al');
      await answer(page, index, 200, { directories: ['alpha'] });
      await page.waitForSelector('#project-details-folder + .folder-suggestions [role=option]');
      await page.keyboard.press('ArrowDown'); // Tab completes only a highlighted suggestion
      await page.keyboard.press('Tab');
      assert.equal(await page.$eval('#project-details-folder', input => input.value), '/srv/alpha/');
      await page.close();
    }
    // What the form is made of, in order, with the rows named and described; the values from the server's answers.
    {
      const page = await mount();
      await ready(page);
      assert.deepEqual(await page.evaluate(() => [...document.querySelector('#project-settings-body').children].map(node => `${node.tagName.toLowerCase()}${node.id ? `#${node.id}` : ''}`)),
        ['form#project-details', 'section#project-scope-control', 'section#project-usage-limit', 'section', 'section#project-usage', 'div', 'div', 'div#project-session-import', 'div#project-lifecycle', 'p#project-settings-error']);
      assert.deepEqual(await page.evaluate(() => ({ name: [document.querySelector('#project-details-name').value, document.querySelector('#project-details-name').disabled], folder: [document.querySelector('#project-details-folder').value, document.querySelector('#project-details-folder').disabled],
        save: document.querySelector('#project-details-save').disabled, status: document.querySelector('#project-details-status').textContent,
        flags: ['#project-auto-resume', '#project-worktrees', '#project-home-lead-only'].map(id => [document.querySelector(id).checked, document.querySelector(id).disabled]),
        desired: document.querySelector('#project-scope-control').dataset.desired, scope: document.querySelector('#project-scope-control .scope-control-status').textContent })),
        { name: ['Alpha', false], folder: ['/srv/alpha', true], save: false, status: 'Only an admin can change the folder.', flags: [[true, false], [true, false], [false, false]], desired: 'active', scope: 'State: Active' });
      assert.deepEqual(await page.evaluate(() => ({ cost: [document.querySelector('#project-cost-mode').value, document.querySelector('#project-cost-mode option').textContent, document.querySelector('#project-cost-cap').hidden],
        turn: [document.querySelector('#project-turn-mode').value, document.querySelector('#project-turn-cap').value, document.querySelector('#project-turn-cap').hidden, document.querySelector('#project-turn-mode option').textContent],
        described: document.querySelector('#project-cost-mode').getAttribute('aria-describedby'), summary: document.querySelector('.usage-limit-summary').textContent.replace(/^.+?\(UTC\)/, 'DAY (UTC)'),
        rows: [...document.querySelectorAll('.usage-popover-rows dt')].length > 0, groups: [...document.querySelectorAll('.usage-summary-groups li')].map(li => li.textContent.split(' ')[0]) })),
        { cost: ['inherit', 'Inherit (default: $5.00)', true], turn: ['custom', '50', false, 'Inherit (default: no limit)'], described: 'project-cost-mode-desc', summary: 'DAY (UTC), all rooms: 2 turns / 50 cap; $0.50 reported cost / no cap. Dispatch: active.', rows: true, groups: ['leader'] });
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.project-role-default')].map(card => [card.querySelector('h4').textContent, card.querySelector('select').value, card.querySelector('select').options[0].textContent, card.querySelector(':scope > div[hidden]') !== null, card.querySelectorAll('.spawn-default-row').length])),
        [['worker', 'inherit', 'Inherit global (2 agents)', true, 0], ['leader', 'custom', 'Inherit global (0 agents)', false, 1]]);
      assert.deepEqual(await page.evaluate(() => ({ session: [document.querySelector('#project-session-import').hidden, document.querySelector('#project-session-import-open').disabled, document.querySelector('#project-session-import-open').title],
        rows: ['archive', 'reopen', 'delete'].map(key => [key, document.querySelector(`#project-${key}-row`).hidden, document.querySelector(`#project-${key}`).disabled]), note: document.querySelector('#project-lifecycle > p').textContent, group: document.querySelector('#project-lifecycle').hidden })),
        { session: [false, false, 'Import Claude Code sessions'], rows: [['archive', false, false], ['reopen', true, false], ['delete', true, false]], note: '', group: false });
      await page.close();
    }
    // Who may change what: an admin edits the folder; someone who is neither sees the form read-only and the archive controls off with the reason.
    {
      const admin = await mount({ role: 'viewer', admin: true });
      await ready(admin);
      assert.deepEqual(await admin.evaluate(() => [document.querySelector('#project-details-name').disabled, document.querySelector('#project-details-folder').disabled, document.querySelector('#project-details-status').textContent, document.querySelector('#project-archive').disabled]), [false, false, '', false]);
      await admin.close();
      const viewer = await mount({ role: 'viewer' });
      await ready(viewer);
      assert.deepEqual(await viewer.evaluate(() => [document.querySelector('#project-details-name').disabled, document.querySelector('#project-details-folder').disabled, document.querySelector('#project-details-save').disabled, document.querySelector('#project-details-status').textContent,
        document.querySelector('#project-archive').disabled, document.querySelector('#project-lifecycle > p').textContent]), [true, true, true, 'Only the project owner can change these.', true, 'Only the project owner or an admin can archive, reopen or delete a project.']);
      await viewer.close();
      const archived = await mount({ archived: true });
      await ready(archived);
      assert.deepEqual(await archived.evaluate(() => [document.querySelector('#project-details-save').disabled, document.querySelector('#project-session-import').hidden, document.querySelector('#project-session-import-open').disabled,
        ['archive', 'reopen', 'delete'].map(key => document.querySelector(`#project-${key}-row`).hidden), document.querySelector('#project-lifecycle > p').textContent.replace(/Archived .+ It/, 'Archived X. It')]),
        [true, true, true, [true, false, false], 'Archived X. It is read-only.']);
      await archived.close();
    }
    // Details: nothing changed says so; a rename sends only the name; a refusal says why.
    {
      const page = await mount();
      await ready(page);
      await page.$eval('#project-details-save', el => el.click());
      assert.equal(await page.$eval('#project-details-status', el => el.textContent), 'Nothing changed.');
      await page.$eval('#project-details-name', el => { el.focus(); el.select(); });
      await page.keyboard.type('Beta');
      await page.$eval('#project-details-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-details-status').textContent === 'Saved.');
      await page.$eval('#project-details-folder', el => { el.value = '/elsewhere'; });
      await page.$eval('#project-details-save', el => el.click());
      assert.equal(await page.$eval('#project-details-status', el => el.textContent), 'Nothing changed.', 'a folder the owner may not change is not sent');
      assert.deepEqual(await page.evaluate(() => [window.__calls.filter(call => call[0] === 'edit'), document.querySelector('#project-details-name').value]), [[['edit', 'p1', { name: 'Beta' }]], 'Beta']);
      await page.close();
    }
    // The switches save at once and go back when the server refuses; each is off while it saves.
    {
      const page = await mount({ held: ['update'] });
      await ready(page);
      await page.$eval('#project-worktrees', el => el.click());
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#project-worktrees').disabled, document.querySelector('#project-worktrees').checked, window.__calls.filter(call => call[0] === 'update')]), [true, false, [['update', 'p1', { worktree_mode: 'off' }]]]);
      await page.evaluate(() => window.__release('update'));
      await page.waitForFunction(() => !document.querySelector('#project-worktrees').disabled);
      assert.equal(await page.$eval('#project-worktrees', el => el.checked), false);
      await page.$eval('#project-home-lead-only', el => el.click());
      await page.evaluate(() => { window.__project.store.source.update = async () => { throw new Error('Not allowed'); }; window.__release('update'); });
      await page.waitForFunction(() => !document.querySelector('#project-home-lead-only').disabled);
      await page.$eval('#project-auto-resume', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-settings-error').textContent === 'Not allowed');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#project-auto-resume').checked, document.querySelector('#project-auto-resume').disabled]), [true, false], 'a refused switch goes back');
      await page.close();
    }
    // Daily limits: a value box only for Custom, a refusal for a bad value, a save of what was entered, and a stale revision refreshing.
    {
      const page = await mount({ auto: { ...base(), 'PUT /api/projects/p1/usage-limit': { ...limit, rev: 4, cost_mode: 'custom', daily_cost_cap: 7.5 } } });
      await ready(page);
      await page.select('#project-cost-mode', 'custom');
      assert.equal(await page.$eval('#project-cost-cap', el => el.hidden), false);
      await page.$eval('#project-usage-limit button[type=submit]', el => el.click());
      assert.equal(await page.$eval('.usage-limit-error', el => el.textContent), 'Enter a positive value for each custom limit, or choose Inherit or No limit.');
      await page.type('#project-cost-cap', '7.5');
      await page.$eval('#project-usage-limit button[type=submit]', el => el.click());
      await page.waitForFunction(() => document.querySelector('.usage-limit-error').textContent === '' && document.querySelector('.usage-limit-summary').textContent.includes('$7.50 cap'));
      assert.deepEqual((await calls(page, 'PUT'))[0], ['/api/projects/p1/usage-limit', { cost_mode: 'custom', daily_cost_cap: 7.5, turn_mode: 'custom', daily_turn_cap: 50, rev: 3 }]);
      await page.close();
      const stale = await mount({ auto: { ...base(), 'PUT /api/projects/p1/usage-limit': rule(409, { error: 'stale_rev', message: 'stale' }) } });
      await ready(stale);
      await stale.$eval('#project-usage-limit button[type=submit]', el => el.click());
      await stale.waitForFunction(() => document.querySelector('.usage-limit-error').textContent === 'Limits changed elsewhere. Review the refreshed values before saving.');
      await stale.close();
      const failing = await mount({ auto: { ...base(), 'PUT /api/projects/p1/usage-limit': rule(500, { message: 'No disk' }) } });
      await ready(failing);
      await failing.select('#project-turn-mode', 'none');
      await failing.$eval('#project-usage-limit button[type=submit]', el => el.click());
      await failing.waitForFunction(() => document.querySelector('.usage-limit-error').textContent === 'No disk');
      assert.equal(await failing.$eval('#project-turn-mode', el => el.value), 'custom', 'a save that did not take puts the form back');
      await failing.close();
    }
    // Agent control: stopping asks first; a pause sends the revision and an idempotency key; a failure to resume offers a retry.
    {
      const page = await mount({ auto: { ...base(), 'POST /api/projects/p1/control': { desired: 'paused', effective_state: 'paused', rev: 8, results: [{ error: 'busy' }] } } });
      await ready(page);
      assert.deepEqual(await page.$$eval('#project-scope-control [data-scope-action]', buttons => buttons.map(b => [b.dataset.scopeAction, b.title, b.getAttribute('aria-label'), b.disabled, b.className, b.querySelector('.icon-mask').className.replace('icon-mask ', '')])),
        [['pause', 'Pause', 'Pause', false, 'btn btn-glyph', 'icon-mask-pause'], ['stop', 'Stop', 'Stop', false, 'btn btn-danger btn-glyph', 'icon-mask-stop'], ['resume', 'Retry resume', 'Retry resume', true, 'btn btn-glyph', 'icon-mask-play']]);
      await page.$eval('#project-scope-control [data-scope-action=stop]', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await settle(page);
      assert.equal((await calls(page, 'POST')).length, 0, 'a cancelled stop sends nothing');
      await page.$eval('#project-scope-control [data-scope-action=pause]', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-scope-control .scope-control-results').textContent.length > 0);
      assert.deepEqual([(await calls(page, 'POST'))[0], await page.evaluate(() => [window.__requests.find(request => request.method === 'POST').headers['Idempotency-Key'].length > 8, document.querySelector('#project-scope-control').dataset.desired, document.querySelector('#project-scope-control .scope-control-results').textContent])],
        [['/api/projects/p1/control', { action: 'pause', rev: 7 }], [true, 'paused', 'Agent controls needing attention: busy.']]);
      await page.close();
    }
    // The room's own control and usage are the same components over the room's routes; reset clears the control.
    {
      const page = await mount({ auto: { ...base(), 'GET /api/rooms/r1/control': { desired: 'paused', effective_state: 'paused', rev: 1 }, 'GET /api/rooms/r1/usage?range=today': usage } });
      await page.evaluate(() => { window.__room.load('r1'); window.__roomUsage.load('r1'); });
      await page.waitForFunction(() => document.querySelector('#room-scope-control .scope-control-status').textContent === 'State: Paused' && document.querySelector('#room-usage .usage-popover-rows'));
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-scope-control').dataset.desired, document.querySelector('#room-scope-control > h3').textContent, document.querySelector('#room-usage > h3').textContent, document.querySelector('#room-scope-control .settings-card')]), ['paused', 'Agents homed in this room', 'Usage', null]);
      await page.select('#room-usage select', '7d');
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/rooms/r1/usage?range=7d'));
      await page.evaluate(() => window.__room.reset());
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-scope-control .scope-control-status').textContent, document.querySelector('#room-scope-control').dataset.desired]), ['', '']);
      await page.close();
    }
    // Default agents per role: choosing Custom shows the picker, saving sends the list, and a failed save says why.
    {
      const page = await mount({ auto: { ...base(), 'PUT /api/projects/p1/role-spawn-defaults/worker': { inherited: false }, 'PUT /api/projects/p1/role-spawn-defaults/leader': rule(403, { message: 'Not yours' }) } });
      await ready(page);
      const card = key => `.project-role-default:nth-child(${key})`;
      await page.select(`${card(1)} select`, 'custom');
      assert.equal(await page.$eval(`${card(1)} > div`, el => el.hidden), false);
      await page.$eval(`${card(1)} [aria-label="Add default agent"]`, el => el.click());
      await page.$eval(`${card(1)} .btn-primary`, el => el.click());
      await page.waitForFunction(() => document.querySelector('.project-role-default .settings-desc[role=status]').textContent === 'Saved.');
      assert.deepEqual((await calls(page, 'PUT')).find(([path]) => path.endsWith('/worker')), ['/api/projects/p1/role-spawn-defaults/worker', { inherit: false, default_agents: [{ launcher_id: 'l1', model: 'm1' }], pick_mode: 'in_turn' }]);
      await page.$eval(`${card(2)} .btn-primary`, el => el.click());
      await page.waitForFunction(() => document.querySelectorAll('.project-role-default .settings-desc[role=status]')[1].textContent === 'Not yours');
      await page.close();
    }
    // Archive and delete: each asks first; archive reads the project again and lists agents that could not be stopped; delete needs the name typed.
    {
      const page = await mount({ auto: { ...base(), 'POST /api/projects/p1/archive': { stop_failed: [{ error: 'unknown execution' }] }, 'DELETE /api/projects/p1': {} }, archived: false });
      await ready(page);
      await page.$eval('#project-archive', el => el.click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Archive “Alpha”?'));
      await confirmOk(page);
      await page.waitForFunction(() => window.__hooks.length === 1 && document.querySelector('#project-lifecycle > p').textContent.startsWith('Archived. 1 agent could not be stopped: unknown execution.'));
      assert.deepEqual(await page.evaluate(() => window.__hooks), [['lifecycle', 'p1']]);
      await page.close();
      const archived = await mount({ archived: true, auto: { ...base(), 'POST /api/projects/p1/reopen': {}, 'DELETE /api/projects/p1': {} } });
      await ready(archived);
      await archived.$eval('#project-reopen', el => el.click());
      await archived.waitForFunction(() => window.__hooks.some(hook => hook[0] === 'lifecycle'));
      await archived.$eval('#project-delete', el => el.click());
      await archived.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await archived.$eval('#app-dialog input', el => { el.value = 'wrong'; });
      await archived.$eval('#app-dialog .app-dialog-ok', el => el.click());
      assert.equal(await archived.$eval('#app-dialog .error', el => el.textContent), 'That is not the project name.');
      await archived.$eval('#app-dialog input', el => { el.value = 'Alpha'; });
      await archived.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await archived.waitForFunction(() => window.__hooks.some(hook => hook[0] === 'removed'));
      assert.deepEqual((await calls(archived, 'DELETE'))[0], ['/api/projects/p1', { name: 'Alpha' }]);
      await archived.close();
    }
    // A question answered after the project shown has changed is about the old project: nothing is sent, and no callback runs.
    {
      const page = await mount({ auto: { ...base(), 'POST /api/projects/p1/archive': {}, 'POST /api/projects/p2/archive': {} } });
      await ready(page);
      await page.$eval('#project-archive', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await page.evaluate(() => { window.__project.load('p2'); });
      await page.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__requests.filter(request => request.method === 'POST').length, window.__hooks.length]), [0, 0], 'a confirmation for p1 sends nothing once p2 is shown');
      await page.close();
      const archived = await mount({ archived: true, auto: { ...base(), 'DELETE /api/projects/p1': {}, 'DELETE /api/projects/p2': {} } });
      await ready(archived);
      await archived.$eval('#project-delete', el => el.click());
      await archived.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await archived.evaluate(() => { window.__project.load('p2'); });
      await archived.$eval('#app-dialog input', el => { el.value = 'Alpha'; });
      await archived.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await settle(archived);
      assert.deepEqual(await archived.evaluate(() => [window.__requests.filter(request => request.method === 'DELETE').length, window.__hooks.length]), [0, 0], 'a deletion confirmed for p1 deletes nothing once p2 is shown');
      await archived.close();
    }
    // A call answered after another project was shown is not the page's business: no callback runs for it.
    {
      const page = await mount();
      await ready(page);
      await page.$eval('#project-archive', el => el.click());
      await confirmOk(page);
      const held = await waitRequest(page, 'POST', '/api/projects/p1/archive');
      await page.evaluate(() => { window.__project.load('p2'); });
      await answer(page, held, 200, { stop_failed: [{ error: 'late' }] });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__hooks.length, window.__project.store.lifecycleNote.value]), [0, ''], 'an archive of p1 answered while p2 is shown runs no callback and writes no note');
      await page.close();
      const archived = await mount({ archived: true });
      await ready(archived);
      await archived.$eval('#project-delete', el => el.click());
      await archived.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await archived.$eval('#app-dialog input', el => { el.value = 'Alpha'; });
      await archived.$eval('#app-dialog .app-dialog-ok', el => el.click());
      const removing = await waitRequest(archived, 'DELETE', '/api/projects/p1');
      await archived.evaluate(() => { window.__project.load('p2'); });
      await answer(archived, removing, 200, {});
      await settle(archived);
      assert.deepEqual(await archived.evaluate(() => [window.__hooks.length, window.__project.store.projectID.value]), [0, 'p2'], 'a deletion of p1 answered while p2 is shown runs no callback and leaves p2 shown');
      await archived.close();
    }
    // An old call finishing does not clear the busy mark of a newer one (or of the project shown now).
    {
      const page = await mount();
      await ready(page);
      await page.evaluate(() => { window.__old = window.__project.store.lifecycleCall('archive', 'p1', '/archive', { method: 'POST' }); });
      const old = await waitRequest(page, 'POST', '/api/projects/p1/archive');
      await page.evaluate(() => { window.__project.load('p2'); });
      await page.evaluate(() => { window.__new = window.__project.store.lifecycleCall('reopen', 'p2', '/reopen', { method: 'POST' }); });
      await waitRequest(page, 'POST', '/api/projects/p2/reopen');
      await answer(page, old, 200, {});
      await page.evaluate(() => window.__old);
      assert.equal(await page.evaluate(() => window.__project.store.lifecycleBusy.value), 'reopen', 'the older call does not clear what the newer one holds');
      await page.close();
    }
    // Two reads of the same agent control answered out of order: the newer one draws.
    {
      const auto = base(); delete auto['GET /api/projects/p1/control'];
      const page = await mount({ auto, load: false });
      await page.evaluate(() => { window.__project.store.scope.load('p1'); window.__project.store.scope.load('p1'); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/projects/p1/control').length === 2);
      await answer(page, 1, 200, { desired: 'paused', effective_state: 'paused', rev: 2 });
      await answer(page, 0, 200, { desired: 'active', effective_state: 'active', rev: 1 });
      await settle(page);
      assert.equal(await page.$eval('#project-scope-control .scope-control-status', el => el.textContent), 'State: Paused');
      await page.close();
    }
    // Importing sessions needs the home room.
    {
      const page = await mount({ rooms: [] });
      await ready(page);
      await page.$eval('#project-session-import-open', el => el.click());
      assert.equal(await page.$eval('#project-settings-error', el => el.textContent), 'This project has no home room to import into.');
      await page.close();
    }
    // Reads answered out of order: only the newest project's answers draw. A page destroyed while reads or writes are held sends nothing more.
    {
      const page = await mount({ auto: {}, load: false, held: ['get'] });
      await page.evaluate(() => { window.__project.load('p1'); window.__project.load('p1'); });
      await page.waitForFunction(() => window.__waiting.get?.length === 2);
      await page.evaluate(() => { window.__project.store.source.get = async () => ({}); window.__release('get'); window.__release('get'); });
      await settle(page);
      assert.equal(await page.$$eval('#project-auto-resume:not(:disabled)', nodes => nodes.length), 1);
      await page.close();
      const reading = await mount({ auto: {}, load: false, held: ['get'] });
      await reading.evaluate(() => { window.__project.load('p1'); });
      await reading.waitForFunction(() => window.__waiting.get?.length === 1);
      await settle(reading);
      const sent = await reading.evaluate(() => window.__requests.length);
      await reading.evaluate(() => { window.__project.destroy(); window.__room.destroy(); window.__roomUsage.destroy(); window.__release('get'); window.__project.load('p1'); window.__room.load('r1'); window.__roomUsage.load('r1'); });
      await settle(reading);
      assert.deepEqual(await reading.evaluate(() => [window.__requests.length, document.querySelectorAll('#project-settings-body *').length]), [sent, 0], 'nothing is read or drawn once the page is destroyed');
      await reading.close();
      const writing = await mount();
      await ready(writing);
      await writing.$eval('#project-archive', el => el.click());
      await confirmOk(writing);
      const post = await waitRequest(writing, 'POST', '/api/projects/p1/archive');
      await writing.evaluate(() => window.__project.destroy());
      const total = await writing.evaluate(() => window.__requests.length);
      await answer(writing, post, 200, { stop_failed: [{ error: 'late' }] });
      await settle(writing);
      assert.deepEqual(await writing.evaluate(() => [window.__requests.length, window.__hooks.length, window.__project.store.lifecycleNote.value]), [total, 0, ''], 'an archive answered after destroy reloads nothing and says nothing');
      await writing.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('project page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });
