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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="room-info-dialog" open><section id="room-project"><div id="project-settings-body" class="settings-subsection project-settings-body"></div></section>
<section id="room-scope-control" class="scope-control" aria-label="Room agent control"></section><section id="room-usage" class="usage-summary" aria-label="Room usage"></section></dialog><section class="chat" style="display:flex;flex-direction:column;height:700px;width:100%"><header>Home</header><div id="messages" class="messages">No messages in this room yet</div><div class="send-bar">Composer</div></section></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const limit = { rev: 3, cost_mode: 'inherit', turn_mode: 'custom', daily_turn_cap: 50, daily_cost_cap: null, inherited_cost_cap: 5, inherited_turn_cap: null, day_start: '2026-02-03T00:00:00Z', tz: 'UTC', daily_turns: 2, daily_cost: 0.5, cost_coverage: 'known', effective_state: 'active' };
const usage = { turns: 4, by_role: [{ key: 'leader', label: 'leader', agents: 1, turns: 4 }], by_model: [] };
const roles = [{ role_key: 'worker', inherited: true, global_agents: [{}, {}], default_agents: [], pick_mode: 'in_turn' }, { role_key: 'leader', inherited: false, global_agents: [], default_agents: [{ launcher_id: 'l1', model: 'm1' }], pick_mode: 'in_turn' }];
const rolePrompts = [
  { role_id: 'role_worker_default', role_key: 'worker', label: 'Worker', emoji: '', builtin: true, resolved_body: 'worker instructions', flow_override: false, global_body_extension: 'global rules', project_extension: { body: 'project rules', rev: 2, updated_at: '2026-11-24T10:00:00Z' }, delivered_body: 'worker instructions\n\nglobal rules\n\nproject rules' },
  { role_id: 'role_dispatcher_default', role_key: 'dispatcher', label: 'Dispatcher', emoji: '', builtin: true, resolved_body: 'dispatcher instructions', flow_override: true, delivered_body: 'the flow replaces the base, no extensions yet' },
];
const flows = [
  { key: 'leader-workers', label: 'Leader and workers', description: 'Leader coordinates workers.', core_roles: ['leader'], spawn_rules: [{ actor_role: 'leader', allowed_roles: ['worker', 'reviewer'] }], diagram: { rooms: [{ key: 'home', label: 'Home', roles: ['leader'] }, { key: 'general', label: 'General', roles: ['worker'] }], edges: [{ from: 'home', to: 'general' }] } },
  { key: 'leader-dispatcher', label: 'Leader and dispatcher', description: 'Dispatcher coordinates workers.', core_roles: ['leader', 'dispatcher'], spawn_rules: [{ actor_role: 'leader', allowed_roles: ['dispatcher'] }, { actor_role: 'dispatcher', allowed_roles: ['worker', 'reviewer'] }], diagram: { rooms: [{ key: 'home', label: 'Home', roles: ['leader'] }, { key: 'routing', label: 'Routing', roles: ['leader', 'dispatcher'] }], edges: [{ from: 'home', to: 'routing' }] } },
  { key: 'leader-advisor', label: 'Leader and advisor', description: 'A fast leader consults a stronger advisor.', core_roles: ['leader', 'advisor'], spawn_rules: [{ actor_role: 'leader', allowed_roles: ['worker', 'reviewer'] }], diagram: { rooms: [], edges: [] } },
  { key: 'custom', label: 'Custom', description: 'Choose your own team.', core_roles: ['leader'], spawn_rules: [], diagram: { rooms: [], edges: [] } },
];
const rule = (status, body) => ({ status, body });
const base = () => ({
  'GET /api/projects/p1/control': { desired: 'active', effective_state: 'active', rev: 7 },
  'GET /api/projects/p1/usage-limit': limit,
  'GET /api/projects/p1/usage?range=today': usage,
  'GET /api/projects/p1/role-spawn-defaults': roles,
  'GET /api/projects/p1/role-prompts': rolePrompts,
  'GET /api/projects/p1/settings': { reviewer_agents: 'inherit', reviewer_agents_global_enabled: true, reviewer_agents_enabled: true },
  'GET /api/machines?project_id=p1': { machines: [{ id: 'm1', name: 'Box', approved: true, connected: true, project_allowed: true }] },
  'GET /api/spawn-catalog?machine_id=m1': { tools: [{ tool: 'claude', variant: 'native', launcher_id: 'l1', label: 'Claude', available: true, models: [{ id: 'm1', label: 'Opus', default: true }, { id: 'm2', label: 'Sonnet' }] }] },
  'GET /api/team-flows': { templates: flows },
  'GET /api/projects/p1/team-flow': { key: 'custom', rev: 1, review_pending: false, template: flows[3], conflicts: [] },
  'POST /api/projects/p1/team-flow/preview': { key: 'custom', create_rooms: [], role_changes: [], conflicts: [] },
  'POST /api/projects/p1/team-setup/preview': { can_create: true, settings: { worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10, reviewer_agents: 'inherit', reviewer_agents_global_enabled: true }, created_rooms: [], conflicts: [] },
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
  const mount = async ({ auto = base(), role = 'owner', admin = false, archived = false, folderInUse = false, held = [], load = true, settings = { auto_resume_after_clean_diagnostic: true, worktree_mode: 'auto', home_lead_only: false }, rooms = [{ id: 'r1', project_id: 'p1', kind: 'home' }] } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ role, admin, archived, folderInUse, held, load, settings, rooms }) => {
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
      const project = { id: 'p1', name: 'Alpha', folder: '/srv/alpha', role, folder_in_use: folderInUse, ...(archived ? { archived_at: '2026-02-03T04:05:06Z' } : {}) };
      const settingsPage = window.__project = new ProjectSettings(source, () => ({ projects: [project, { ...project, id: 'p2' }], rooms, selectedProjectID: 'p1' }));
      settingsPage.onLifecycle = async id => { window.__hooks.push(['lifecycle', id]); };
      settingsPage.onRemoved = async id => { window.__hooks.push(['removed', id]); };
      settingsPage.onFlowApplied = async id => { window.__hooks.push(['flow', id]); };
      window.__room = new (await import('/js/pages/project/room-panels.js')).RoomScopeControl(document.querySelector('#room-scope-control'));
      window.__roomUsage = new (await import('/js/pages/project/room-panels.js')).RoomUsage(document.querySelector('#room-usage'));
      settingsPage.store.flow.roomID = 'r1';
      if (load) await settingsPage.load('p1');
    }, { role, admin, archived, folderInUse, held, load, settings, rooms });
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
      await page.keyboard.press('Tab');
      assert.equal(await page.$eval('#project-details-folder', input => input.value), '/srv/alpha/');
      assert.equal(await page.evaluate(() => document.activeElement.id), 'project-details-folder', 'Tab keeps focus in the folder field');
      await page.$eval('#project-details-folder', input => { input.value = '/srv/alpha'; input.dispatchEvent(new Event('input', { bubbles: true })); });
      const exact = await waitRequest(page, 'GET', '/api/machines/directories?path=%2Fsrv%2F&prefix=alpha');
      await answer(page, exact, 200, { directories: ['alpha'] });
      await page.waitForSelector('#project-details-folder + .folder-suggestions [role=option]');
      await page.keyboard.press('/');
      assert.equal(await page.$eval('#project-details-folder', input => input.value), '/srv/alpha/', 'slash completes an exact directory name once');
      assert.equal(await page.evaluate(() => document.activeElement.id), 'project-details-folder');
      await page.$eval('#project-details-folder', input => { input.value = '/srv/al'; input.dispatchEvent(new Event('input', { bubbles: true })); });
      const partial = await waitRequest(page, 'GET', '/api/machines/directories?path=%2Fsrv%2F&prefix=al');
      await answer(page, partial, 200, { directories: ['alpha'] });
      await page.waitForSelector('#project-details-folder + .folder-suggestions [role=option]');
      await page.keyboard.press('/');
      assert.equal(await page.$eval('#project-details-folder', input => input.value), '/srv/al/', 'slash remains a typed separator for a partial name');
      await page.close();
    }
    // What the form is made of, in order, with the rows named and described; the values from the server's answers.
    {
      const page = await mount();
      await ready(page);
      assert.deepEqual(await page.evaluate(() => [...document.querySelector('#project-settings-body').children].map(node => `${node.tagName.toLowerCase()}${node.id ? `#${node.id}` : ''}`)),
        ['form#project-details', 'section#project-scope-control', 'section#project-usage-limit', 'section#project-role-defaults', 'section#project-role-prompts', 'section#project-team-flow', 'div#project-reviewers', 'section#project-usage', 'div#project-home-room', 'div#project-recovery', 'div#project-session-import', 'div#project-lifecycle', 'p#project-settings-error']);
      assert.deepEqual(await page.evaluate(() => ({ name: [document.querySelector('#project-details-name').value, document.querySelector('#project-details-name').disabled], folder: [document.querySelector('#project-details-folder').value, document.querySelector('#project-details-folder').disabled],
        save: document.querySelector('#project-details-save').disabled, status: document.querySelector('#project-details-status').textContent,
        flags: ['#project-auto-resume', '#project-worktrees', '#project-home-lead-only'].map(id => [document.querySelector(id).checked, document.querySelector(id).disabled]),
        desired: document.querySelector('#project-scope-control').dataset.desired, scope: document.querySelector('#project-scope-control .scope-control-status').textContent })),
        { name: ['Alpha', false], folder: ['/srv/alpha', false], save: true, status: 'Only an admin can change the folder.', flags: [[true, false], [true, false], [false, false]], desired: 'active', scope: 'Project agents: Active' });
      assert.deepEqual(await page.evaluate(() => ({ cost: [document.querySelector('#project-cost-mode').value, document.querySelector('#project-cost-mode option').textContent, document.querySelector('#project-cost-cap').hidden],
        turn: [document.querySelector('#project-turn-mode').value, document.querySelector('#project-turn-cap').value, document.querySelector('#project-turn-cap').hidden, document.querySelector('#project-turn-mode option').textContent],
        described: document.querySelector('#project-cost-mode').getAttribute('aria-describedby'), summary: document.querySelector('.usage-limit-summary').textContent.replace(/^.+?\(UTC\)/, 'DAY (UTC)'),
        rows: [...document.querySelectorAll('.usage-popover-rows dt')].length > 0, groups: [...document.querySelectorAll('.usage-summary-groups li')].map(li => li.textContent.split(' ')[0]) })),
        { cost: ['inherit', 'Inherit (default: $5.00)', true], turn: ['custom', '50', false, 'Inherit (default: no limit)'], described: 'project-cost-mode-desc', summary: 'DAY (UTC), all rooms: 2 turns / 50 cap; $0.50 reported cost / no cap. Dispatch: active.', rows: true, groups: ['leader'] });
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.project-role-default')].map(card => [card.querySelector('h4').textContent, card.querySelector('select').value, card.querySelector('select').options[0].textContent, !!card.querySelector(':scope > div[hidden]'), card.querySelectorAll('.spawn-default-row').length])),
        [['Worker', 'inherit', 'Inherit global (2 agents)', true, 0], ['Leader', 'custom', 'Inherit global (0 agents)', false, 1]]);
      assert.deepEqual(await page.evaluate(() => ({ session: [document.querySelector('#project-session-import').hidden, document.querySelector('#project-session-import-open').disabled, document.querySelector('#project-session-import-open').title],
        rows: ['archive', 'reopen', 'delete'].map(key => [key, document.querySelector(`#project-${key}-row`).hidden, document.querySelector(`#project-${key}`).disabled]), note: document.querySelector('#project-lifecycle > p').textContent, group: document.querySelector('#project-lifecycle').hidden })),
        { session: [false, false, 'Import Claude Code sessions'], rows: [['archive', false, false], ['reopen', true, false], ['delete', false, false]], note: '', group: false });
      await page.close();
    }
    // Project settings changes the flow with the same wizard as setup: the leader is kept, a new dispatcher is added, workers are kept, and the
    // server's plan decides the statuses. Nothing starts before Apply, and the progress of the change stays on screen until it ends.
    {
      const agent = (id, slug, extra = {}) => ({ agent_id: id, slug, machine_id: 'm1', tool: 'claude', variant: 'native', launcher_id: 'l1', launcher_label: 'Claude', model: 'm1', ...extra });
      const plan = { from_key: 'custom', to_key: 'leader-dispatcher', roles: [
        { role_key: 'leader', status: 'kept', agents: [agent('agt_l', 'ada')] }, { role_key: 'dispatcher', status: 'new', agents: [] },
        { role_key: 'worker', status: 'kept', agents: [agent('agt_w', 'bo'), agent('agt_s', 'iris', { state: 'stopped' }), ...Array.from({ length: 27 }, (_, index) => agent(`agt_s${index}`, `idle${index}`, { state: 'stopped' }))] }, { role_key: 'reviewer', status: 'kept', agents: [] }] };
      const auto = base();
      auto['GET /api/rooms/r1/agent-name'] = { name: 'cy' };
      auto['GET /api/team-flows/recommended-models'] = { rows: [{ role_key: 'dispatcher', flow_key: 'leader-dispatcher', tool: 'claude', models: [{ model: 'm2', why: 'cheap and quick routing' }, { model: 'not-offered-yet', why: 'the strongest model for rare advice' }] },
        { role_key: 'worker', flow_key: 'leader-dispatcher', tool: 'claude', models: [{ model: 'm2', why: 'executes: speed and cost matter' }] }, { role_key: 'reviewer', flow_key: 'leader-dispatcher', tool: 'codex', models: [{ model: 'x1', why: 'another vendor than the worker' }] }] };
      const page = await mount({ auto });
      await page.setViewport({ width: 1280, height: 1000 });
      await page.evaluate(() => { const settings = document.querySelector('#room-info-dialog'); settings.close(); settings.showModal(); });
      await ready(page);
      await page.waitForSelector('#project-team-flow .team-flow-current');
      assert.deepEqual(await page.$eval('#project-team-flow', node => [node.querySelector('.team-flow-current').textContent, node.querySelectorAll('.team-flow-card').length, node.querySelector('.btn-primary').textContent.trim()]), ['Current: D · Custom', 0, 'Change team flow'], 'settings show the current flow and one button; the wizard opens on demand');
      await page.$eval('#project-team-flow .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-modal .team-flow-card').length === 5);
      await page.waitForFunction(() => document.querySelector('.team-flow-modal')?.matches(':modal'));
      assert.deepEqual(await page.$$eval('#team-flow-modal .team-flow-option', rows => rows.filter(row => row.querySelector('.team-flow-current-badge')).map(row => [row.dataset.flowKey, row.querySelector('.team-flow-current-badge').textContent])), [['custom', 'Current']], 'the current flow is identified by text on its card');
      assert.deepEqual(await page.evaluate(() => { const modal = document.querySelector('.team-flow-modal'); return [modal.matches(':modal'), modal.parentElement === document.body, document.querySelector('#room-info-dialog').open, modal.getBoundingClientRect().width]; }), [true, true, false, 960], 'the wizard replaces settings with a wide body-level modal');
      await page.$eval('.team-flow-modal-close', node => node.click());
      await page.waitForFunction(() => !document.querySelector('.team-flow-modal')?.open);
      await page.waitForFunction(() => document.querySelector('#room-info-dialog').open);
      assert.equal(await page.$eval('#room-info-dialog', node => node.open), true, 'closing the wizard returns to settings');
      await page.$eval('#project-team-flow .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-modal .team-flow-card').length === 5);
      // The expand label must still sit below every diagram node (D included), and no node may be clipped.
      for (const width of [1280, 600, 390]) {
        await page.setViewport({ width, height: 1000 });
        await page.waitForFunction(() => document.querySelectorAll('#team-flow-modal .team-flow-thumbnail').length === 3);
        if (width === 390) assert.equal(await page.$eval('.team-flow-modal', node => Math.abs(node.getBoundingClientRect().width - innerWidth) <= 1), true, 'the wizard fills a phone screen');
        assert.deepEqual(await page.$$eval('#team-flow-modal .team-flow-option', options => options.filter(option => option.querySelector('.team-flow-thumbnail')).map(option => {
          const label = option.querySelector('.team-flow-diagram-open span').getBoundingClientRect(), frame = option.querySelector('.team-flow-thumbnail').getBoundingClientRect();
          const nodes = [...option.querySelectorAll('.team-flow-thumbnail-agent rect')].map(node => node.getBoundingClientRect());
          return [nodes.filter(node => node.left < label.right && node.right > label.left && node.top < label.bottom && node.bottom > label.top).length,
            nodes.filter(node => node.left < frame.left - .5 || node.right > frame.right + .5 || node.top < frame.top - .5 || node.bottom > frame.bottom + .5).length];
        })), [[0, 0], [0, 0], [0, 0]], `no diagram node is covered by the expand label or clipped at ${width}px`);
      }
      await page.evaluate(() => { window.__auto['POST /api/projects/p1/team-flow/migration/preview'] = undefined; delete window.__auto['POST /api/projects/p1/team-flow/migration/preview']; });
      assert.equal(await page.$eval('#team-flow-modal .btn-primary', node => node.disabled), true, 'the current flow cannot be "changed" to itself');
      await page.$eval('#team-flow-modal .team-flow-option[data-flow-key="leader-dispatcher"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .btn-primary:not(.team-flow-diagram-open)').disabled);
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      const planRequest = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/preview');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, planRequest), { flow_key: 'leader-dispatcher', flow_rev: 1 });
      await answer(page, planRequest, 200, plan);
      const title = () => page.$eval('#team-flow-home-title', node => node.textContent);
      const banner = () => page.$eval('#team-flow-modal .team-wizard-banner', node => node.textContent);
      const next = async expected => { await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click()); await page.waitForFunction(text => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === text, {}, expected); };
      await page.waitForFunction(() => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === 'Leader');
      assert.equal(await banner(), 'Leader is kept between D → B migration');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-kept', node => node.textContent), /ada · Box · Claude · Opus \(current\)/);
      assert.equal(await page.$('#team-flow-modal .team-setup-name'), null, 'a kept agent keeps its name');
      await page.$eval('#team-flow-modal .team-wizard-kept .btn', node => node.click());
      await page.waitForSelector('#team-setup-leader-agt_l-model');
      assert.equal(await page.$eval('#team-setup-leader-agt_l-model', node => getComputedStyle(node).height), '40px', 'the wizard keeps its full-size controls outside room settings');
      await page.select('#team-setup-leader-agt_l-model', 'm2');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-kept p', node => node.textContent), /will switch/);
      await next('Dispatcher');
      assert.equal(await banner(), 'Dispatcher is new in B and will be added');
      await page.waitForFunction(() => document.querySelector('#team-setup-dispatcher-name')?.value === 'cy');
      await page.evaluate(() => { delete window.__auto['GET /api/projects/p1/team-flow']; void window.__project.store.flow.load('p1'); });
      const refreshedFlow = await waitRequest(page, 'GET', '/api/projects/p1/team-flow');
      await page.waitForFunction(() => document.querySelector('#team-flow-modal .team-flow-modal-status')?.textContent.includes('Loading team flows'));
      assert.deepEqual(await page.$eval('#team-flow-modal', node => [node.open, !!node.querySelector('.team-flow-modal-close'), !!node.querySelector('.team-setup-wizard')]), [true, true, false], 'a flow refresh keeps an exit in the open dialog');
      await answer(page, refreshedFlow, 200, { key: 'custom', rev: 1, review_pending: false, template: flows[3], conflicts: [] });
      await page.waitForFunction(() => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === 'Dispatcher');
      await page.$eval('#team-flow-modal .team-flow-actions .btn:not(.btn-primary)', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === 'Leader');
      assert.equal(await page.$eval('#team-setup-leader-agt_l-model', node => node.value), 'm2', 'a successful flow refresh keeps the wizard step and model choice');
      await next('Dispatcher');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-recommended', node => node.textContent), /^💡 Recommended modelsClaude · SelectedSonnet — cheap and quick routing$/, 'the live catalog filters recommendations under the selected harness label');
      await next('Workers and reviewers');
      assert.equal(await banner(), 'Workers are kept between D → B migration');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-kept-list', node => node.textContent), /Workers.*bo · Box · Claude · Opus/s);
      // Stopped agents are not listed one by one: the active ones are, and the stopped ones are one line.
      assert.deepEqual(await page.$$eval('#team-flow-modal .team-wizard-kept-list .team-wizard-kept', rows => rows.map(row => row.textContent.split(' · ')[0])), ['bo'], 'only the active worker is listed');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-kept-list .team-wizard-stopped', node => node.textContent), '28 stopped agents, unchanged');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-kept-list', node => node.textContent.includes('iris')), false, 'no stopped agent is named');
      assert.deepEqual(await page.$$eval('#team-flow-modal .team-wizard-recommended', nodes => nodes.map(node => node.textContent)), ['💡 Recommended models for workersClaude · SelectedSonnet — executes: speed and cost matter'], 'workers get their row; reviewers have no offered recommendation, so that panel stays hidden');
      // Opening "Change model or harness" on a worker and leaving the choice alone is not a change: it must not reach the server as one.
      await page.$eval('#team-flow-modal .team-wizard-kept-list .team-wizard-kept .btn', node => node.click());
      await page.waitForSelector('#team-setup-worker-agt_w-model');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-kept-list .team-wizard-kept p', node => node.textContent), /\(current\)/, 'an untouched pick still says current');
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      // The preview that carries the targets is where the server validates them: its refusal stays on this step, and a retry goes through.
      const refused = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/preview');
      await answer(page, refused, 400, { error: 'invalid_request', message: 'new dispatcher target required' });
      await page.waitForFunction(() => document.querySelector('#team-flow-modal .error[role=alert]')?.textContent.includes('new dispatcher target required'));
      assert.equal(await page.$eval('#team-flow-modal #team-flow-home-title', node => node.textContent), 'Workers and reviewers');
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      const finalPlan = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/preview');
      const planBody = await page.evaluate(index => window.__requests[index].body, finalPlan);
      assert.deepEqual([planBody.flow_key, planBody.flow_rev, planBody.dispatcher?.name, planBody.dispatcher?.model, planBody.changes, planBody.leader], ['leader-dispatcher', 1, 'cy', 'm1', [{ agent_id: 'agt_l', target: { machine_id: 'm1', tool: 'claude', variant: 'native', launcher_id: 'l1', model: 'm2' } }], undefined], 'the preview carries the new dispatcher and only the kept leader\'s real model change (the untouched worker pick is dropped)');
      await answer(page, finalPlan, 200, { ...plan, can_apply: true, created_rooms: [{ key: 'routing', title: 'routing' }], conflicts: [{ agent_id: 'agt_old', slug: 'ian', role_key: 'leader', role_label: 'Leader', room_id: 'rm_general', room_title: 'general', reason: 'Leader in task room' }] });
      await page.waitForFunction(() => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === 'Summary' && document.querySelector('#team-flow-modal .team-wizard-notice'));
      const summary = await page.$eval('#team-flow-modal .team-setup-summary', node => node.textContent);
      assert.match(summary, /D · Custom → B · Leader and dispatcher/);
      assert.match(summary, /Leader.*ada: switches to the chosen model after its current turn \(Box · Claude · Sonnet\)/s);
      assert.match(summary, /Dispatcher.*cy · Box · Claude · Opus is added/s);
      assert.match(summary, /Worker.*bo: keeps running with the new rules \(not restarted\)/s);
      assert.match(summary, /28 stopped agents, unchanged/);
      assert.doesNotMatch(summary, /iris|idle0/, 'the summary does not name the stopped agents');
      assert.match(summary, /Rooms to create: routing/);
      assert.match(summary, /This can take a while: each agent first finishes its current turn before the change applies; you will have to wait until then\./);
      assert.equal(await page.$eval('#team-flow-modal .team-flow-removal input', node => node.checked), false, 'membership removal is opt-in');
      await page.$eval('#team-flow-modal .team-flow-removal input', node => node.click());
      await page.evaluate(() => { document.querySelector('#team-flow-modal .team-flow-actions .btn-primary').click(); });
      const migrate = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migrate');
      assert.equal(await page.evaluate(index => !!window.__requests[index].headers['Idempotency-Key'], migrate), true, 'apply uses an idempotency key');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body.remove_memberships, migrate), [{ agent_id: 'agt_old', room_id: 'rm_general' }], 'only ticked memberships are removed');
      const running = { id: 'mg1', state: 'draining', can_cancel: true, agents: [
        { agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'switch', state: 'draining', blocking: true, keeps: true }, { agent_id: 'agt_w', slug: 'bo', role_key: 'worker', action: 'notice', state: 'waiting' }, { agent_id: 'new', slug: 'cy', role_key: 'dispatcher', action: 'spawn', state: 'waiting' }] };
      await answer(page, migrate, 202, { migration: running });
      await page.waitForSelector('#team-flow-modal .team-wizard-progress');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-progress', node => node.textContent), /Changing the team flow….*ada \(leader\) switches model: Finishing its current turn.*bo \(worker\) keeps running with the new rules: Waiting.*cy \(dispatcher\) is added: Waiting/s);
      // An agent the change waits for can be interrupted, after a confirmation; the others cannot.
      assert.deepEqual(await page.$$eval('#team-flow-modal .team-wizard-interrupt', nodes => nodes.map(node => [node.title, node.getAttribute('aria-label'), node.closest('li').textContent.includes('ada')])), [['Interrupt ada now', 'Interrupt ada now', true]], 'only the blocking agent has Interrupt now, with a tooltip');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-background', node => node.title), 'Continue in background');
      await page.$eval('#team-flow-modal .team-wizard-interrupt', node => node.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open && document.querySelector('#app-dialog-title').textContent === 'Interrupt agent');
      assert.match(await page.$eval('#app-dialog .app-dialog-message', node => node.textContent), /Interrupt ada now\? Its current turn is cut off and its request is queued again\. ada keeps running \(a machine with an older supervisor stops it and starts it again\), and the change goes on once the turn is gone\./);
      await page.$eval('#app-dialog .app-dialog-cancel', node => node.click());
      await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.some(request => request.path.endsWith('/migration/interrupt'))), false, 'declining the confirmation interrupts nothing');
      await page.$eval('#team-flow-modal .team-wizard-interrupt', node => node.click());
      await confirmOk(page);
      const firstInterrupt = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/interrupt');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, firstInterrupt), { agent_id: 'agt_l' });
      // The server keeps the agent blocking until its turn has really ended (the request may not have reached the turn): the button stays,
      // and a second click asks again.
      await answer(page, firstInterrupt, 200, { migration: running });
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-modal .team-wizard-interrupt'); return button && !button.disabled; });
      await page.$eval('#team-flow-modal .team-wizard-interrupt', node => node.click());
      await confirmOk(page);
      const interrupt = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/interrupt');
      assert.notEqual(interrupt, firstInterrupt, 'the second click sends a second request');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, interrupt), { agent_id: 'agt_l' });
      await answer(page, interrupt, 200, { migration: { ...running, agents: running.agents.map(agent => agent.agent_id === 'agt_l' ? { ...agent, blocking: false } : agent) } });
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .team-wizard-interrupt'));
      // Cancel is offered while the server allows it, and answers with the migration as it stands.
      await page.$eval('#team-flow-modal .team-wizard-progress .btn', node => node.click());
      const cancel = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/cancel');
      await answer(page, cancel, 200, { migration: { ...running, state: 'cancelled', can_cancel: false } });
      await page.waitForFunction(() => document.querySelector('#team-flow-modal .team-wizard-progress')?.textContent.includes('Cancelled. Everything stays as it was.'));
      assert.equal(await page.$('#team-flow-modal .team-wizard-progress .btn'), null, 'nothing to cancel any more');
      await page.close();
    }
    // The store is emptied while another project loads and the wizard can render once more before its parent removes it: it must render nothing, not throw.
    {
      const page = await mount();
      await page.evaluate(async () => {
        const { TeamWizard } = await import('/js/pages/project/team-setup-page.js');
        const { TeamFlowStore } = await import('/js/pages/project/team-flow-store.js');
        const { html, render } = await import('/js/ui.js');
        const host = document.createElement('div'); host.id = 'null-state'; document.body.append(host);
        const flow = new TeamFlowStore();
        render(html`<${TeamWizard} flow=${flow} canEdit=${true} project=${null} />`, host);
      });
      await settle(page); await settle(page);
      assert.equal(await page.$eval('#null-state', node => node.textContent), '', 'no flow state: nothing is drawn');
      assert.deepEqual(errors.splice(0), [], 'rendering the wizard without a flow state throws nothing');
      await page.close();
    }
    // Continue in background: the dialog closes, the change keeps going and its progress shows in Settings -> Team flow, where an agent it waits for can be interrupted.
    {
      const running = { id: 'mg-bg', state: 'draining', can_cancel: true, agents: [
        { agent_id: 'agt_x', slug: 'dax', role_key: 'dispatcher', action: 'handoff_stop', state: 'draining', blocking: true },
        { agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'switch', state: 'draining', blocking: true, keeps: true }, { agent_id: 'new', slug: 'cy', role_key: 'dispatcher', action: 'spawn', state: 'waiting' }] };
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: running };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#team-flow-modal .team-wizard-progress');
      await page.$eval('#team-flow-modal .team-wizard-background', node => node.click());
      await page.waitForFunction(() => !document.querySelector('.team-flow-modal')?.open);
      await page.waitForSelector('#project-team-flow #team-flow-progress .team-wizard-progress');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#team-flow-modal')?.open, document.querySelector('#room-info-dialog').open]), [false, true], 'the wizard is closed and settings is back');
      assert.match(await page.$eval('#team-flow-progress', node => node.textContent), /Changing the team flow….*ada \(leader\) switches model: Finishing its current turn.*cy \(dispatcher\) is added: Waiting/s);
      assert.equal(await page.$('#team-flow-progress .team-wizard-background'), null, 'it is already in the background');
      await settle(page);
      assert.equal(await page.$eval('.team-flow-modal', node => node.open), false, 'the poll does not bring the wizard back');
      // Interrupt now, next to the agent that blocks the change.
      assert.deepEqual(await page.$$eval('#team-flow-progress .team-wizard-interrupt', nodes => nodes.map(node => node.title)), ['Interrupt dax now', 'Interrupt ada now']);
      // An agent the new flow does not keep hands off and stops; a kept one keeps running (or restarts, on an older supervisor): the confirmation says which.
      await page.$eval('#team-flow-progress .team-wizard-interrupt', node => node.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.match(await page.$eval('#app-dialog .app-dialog-message', node => node.textContent), /Interrupt dax now\? Its current turn is cut off\. The new flow does not keep dax: it hands off to the leader if it still can, then stops\./);
      await page.$eval('#app-dialog .app-dialog-cancel', node => node.click());
      await page.$eval('#team-flow-progress li:nth-child(2) .team-wizard-interrupt', node => node.click());
      await confirmOk(page);
      const interrupt = await waitRequest(page, 'POST', '/api/projects/p1/team-flow/migration/interrupt');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, interrupt), { agent_id: 'agt_l' });
      await answer(page, interrupt, 200, { migration: { ...running, agents: [running.agents[0], { ...running.agents[1], blocking: false }, running.agents[2]] } });
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-progress .team-wizard-interrupt').length === 1);
      // It goes on by itself and finishes: the flow is reloaded and the outcome stays until dismissed.
      await page.evaluate(() => {
        window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg-bg', state: 'done', can_cancel: false, agents: [{ agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'switch', state: 'done' }] } };
        window.__auto['GET /api/projects/p1/team-flow'] = { key: 'leader-dispatcher', rev: 2, review_pending: false, conflicts: [] };
      });
      await page.waitForFunction(() => document.querySelector('#team-flow-progress')?.textContent.includes('Team flow changed.') && document.querySelector('#project-team-flow .team-flow-current')?.textContent.includes('B · Leader and dispatcher'), { timeout: 8000 });
      await page.$eval('#team-flow-progress [aria-label=Dismiss]', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-progress'));
      await page.close();
    }
    // Interrupt now follows the server's `blocking`: it goes once the agent is stopping and comes back if the change waits on it again.
    {
      const agentRow = blocking => ({ agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'switch', state: 'draining', blocking, keeps: true });
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg-flip', state: 'draining', can_cancel: true, agents: [agentRow(true)] } };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#team-flow-modal .team-wizard-background');
      await page.$eval('#team-flow-modal .team-wizard-background', node => node.click());
      await page.waitForSelector('#team-flow-progress .team-wizard-interrupt');
      await page.evaluate(agent => { window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg-flip', state: 'draining', can_cancel: true, agents: [agent] } }; }, agentRow(false));
      await page.waitForFunction(() => !document.querySelector('#team-flow-progress .team-wizard-interrupt'), { timeout: 8000 });
      await page.evaluate(agent => { window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg-flip', state: 'draining', can_cancel: true, agents: [agent] } }; }, agentRow(true));
      await page.waitForSelector('#team-flow-progress .team-wizard-interrupt', { timeout: 8000 });
      // An interrupted kept agent the change is waiting to see restart says so, with a tooltip.
      await page.evaluate(agent => { window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg-flip', state: 'draining', can_cancel: true, agents: [{ ...agent, restarting: true }] } }; }, agentRow(false));
      await page.waitForSelector('#team-flow-progress .team-wizard-restarting', { timeout: 8000 });
      assert.deepEqual(await page.$eval('#team-flow-progress .team-wizard-restarting', node => [node.textContent, node.title]), ['Waiting for restart', 'ada was interrupted; it starts again by itself and the change waits for it']);
      await page.close();
    }
    // A finished change shows its outcome in settings after a remount, until it is dismissed; the dismissal is remembered for the project.
    {
      const finished = { id: 'mg-outcome', state: 'done', can_cancel: false, agents: [{ agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'notice', state: 'done' }] };
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: finished };
      const first = await mount({ auto });
      await first.evaluate(() => localStorage.removeItem('aimebu2.teamFlowDismissed.p1'));
      await first.evaluate(() => void window.__project.load('p1'));
      await first.waitForSelector('#project-team-flow #team-flow-progress');
      assert.match(await first.$eval('#team-flow-progress', node => node.textContent), /Team flow changed\./);
      const second = await mount({ auto: auto });
      await second.waitForSelector('#project-team-flow #team-flow-progress');
      await second.close();
      await first.$eval('#team-flow-progress [aria-label=Dismiss]', node => node.click());
      assert.equal(await first.evaluate(() => localStorage.getItem('aimebu2.teamFlowDismissed.p1')), 'mg-outcome');
      await first.close();
      const third = await mount({ auto: auto });
      await ready(third); await settle(third); await settle(third);
      assert.equal(await third.$('#team-flow-progress'), null, 'a dismissed outcome does not come back');
      await third.close();
    }
    // A background refresh must not pull a running migration over the main page or reopen settings.
    {
      const running = { id: 'mg-background', state: 'draining', can_cancel: true, agents: [] };
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: running };
      const page = await mount({ auto, load: false });
      await page.evaluate(() => { document.querySelector('#room-info-dialog').close(); void window.__project.load('p1'); });
      await ready(page); await settle(page); await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-info-dialog').open, document.querySelector('#team-flow-modal')?.open]), [false, false], 'a background migration leaves both dialogs closed');
      await page.close();
    }
    // A same-project refresh retains an in-progress wizard choice only while the wizard is open.
    {
      const page = await mount();
      const selections = await page.evaluate(async () => {
        const { TeamFlowStore } = await import('/js/pages/project/team-flow-store.js');
        let wizardOpen = true;
        const source = { templates: async () => ({ templates: [{ key: 'current' }, { key: 'draft' }] }), get: async () => ({ key: 'current' }), preview: async () => ({}) };
        const flow = new TeamFlowStore({ source, retainSelection: () => wizardOpen });
        await flow.load('p1'); await flow.select('draft'); await flow.load('p1');
        const whileOpen = flow.selected.peek();
        wizardOpen = false; await flow.load('p1');
        const afterClose = flow.selected.peek();
        flow.destroy();
        return [whileOpen, afterClose];
      });
      assert.deepEqual(selections, ['draft', 'current']);
      await page.close();
    }
    // A completed change keeps its per-agent summary visible until the person closes it; a project already changing shows progress on opening.
    {
      const running = { id: 'mg2', state: 'applying', can_cancel: false, agents: [{ agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'notice', state: 'running' }] };
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: running };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#team-flow-modal .team-wizard-progress');
      assert.equal(await page.$('#team-flow-modal .team-wizard-progress .btn'), null, 'no cancel after the point of no return');
      await page.evaluate(() => { window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg2', state: 'done', can_cancel: false, agents: [
        { agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'notice', state: 'done' },
        { agent_id: 'agt_d', slug: 'cy', role_key: 'dispatcher', action: 'reactivate', state: 'done' }] } }; window.__auto['GET /api/projects/p1/team-flow'] = { key: 'leader-dispatcher', rev: 2, review_pending: false, conflicts: [] }; });
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Team flow changed');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-progress', node => node.textContent), /Team flow changed\..*ada \(leader\) keeps running with the new rules: Done.*cy \(dispatcher\) is started again: Done/s);
      assert.match(await page.$eval('#project-team-flow .team-flow-current', node => node.textContent), /D · Custom/, 'the completed summary remains until Done');
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#project-team-flow .team-flow-current')?.textContent.includes('B · Leader and dispatcher') && !document.querySelector('#team-flow-modal .team-wizard-progress'), { timeout: 8000 });
      assert.equal(await page.$eval('.team-flow-modal', node => node.open), false, 'Done closes the wizard and returns to settings');
      assert.equal(await page.evaluate(() => window.__hooks.at(-1)[0]), 'flow', 'the page is told the flow changed');
      await page.close();
    }
    // A drain that waits on a long turn says so and keeps Cancel; a lost or failed change is terminal and says why.
    {
      const since = new Date(Date.now() - 20 * 60000).toISOString();
      const running = { id: 'mg3', state: 'draining', can_cancel: true, stalled_since: since, agents: [
        { agent_id: 'agt_l', slug: 'ada', role_key: 'leader', action: 'notice', state: 'draining' }, { agent_id: 'agt_w', slug: 'bo', role_key: 'worker', action: 'notice', state: 'waiting' },
        { agent_id: 'agt_d', slug: 'dax', role_key: 'dispatcher', action: 'reactivate', state: 'waiting' }] };
      const auto = base(); auto['GET /api/projects/p1/team-flow/migration'] = { migration: running };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#team-flow-modal .team-wizard-progress');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-progress [role=alert]', node => node.textContent), /^ada has not finished its current turn for 20 minutes\. Keep waiting, or cancel to leave everything as it was\.$/);
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-progress .btn', node => node.textContent.trim()), 'Cancel');
      assert.match(await page.$eval('#team-flow-modal .team-wizard-progress ul', node => node.textContent), /dax \(dispatcher\) is started again: Waiting/, 'a stopped dispatcher or advisor brought back by a later switch is reactivated, not added');
      await page.evaluate(() => { window.__auto['GET /api/projects/p1/team-flow/migration'] = { migration: { id: 'mg3', state: 'failed', can_cancel: false, error: 'The dispatcher did not stop in time', agents: [] } }; });
      await page.waitForFunction(() => document.querySelector('#team-flow-modal .team-wizard-progress')?.textContent.includes('The change failed.'), { timeout: 8000 });
      assert.match(await page.$eval('#team-flow-modal .team-wizard-progress', node => node.textContent), /The dispatcher did not stop in time/);
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-progress [role=alert]', node => node.classList.contains('error')), true, 'a settled change shows no stalled warning, only its error');
      await page.close();
    }
    // Leaving a role: the banner says what happens to it and nothing asks for a model; the disabled combined flow cannot be chosen.
    {
      const agent = (id, slug, extra = {}) => ({ agent_id: id, slug, machine_id: 'm1', tool: 'claude', variant: 'native', launcher_id: 'l1', launcher_label: 'Claude', model: 'm1', ...extra });
      const auto = base();
      auto['GET /api/projects/p1/team-flow'] = { key: 'leader-dispatcher', rev: 3, review_pending: false, template: flows[1], conflicts: [] };
      auto['POST /api/projects/p1/team-flow/migration/preview'] = { from_key: 'leader-dispatcher', to_key: 'leader-workers', can_apply: true, created_rooms: [], conflicts: [], roles: [
        { role_key: 'leader', status: 'kept', agents: [agent('agt_l', 'ada')] }, { role_key: 'dispatcher', status: 'leaving', agents: [agent('agt_d', 'dax'), agent('agt_ds1', 'old1', { state: 'stopped' }), agent('agt_ds2', 'old2', { state: 'stopped' })] }, { role_key: 'worker', status: 'kept', agents: [] }] };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#project-team-flow .btn-primary');
      await page.$eval('#project-team-flow .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-modal .team-flow-card').length === 5);
      assert.equal(await page.$eval('#team-flow-modal .team-flow-coming .team-flow-card', node => node.disabled), true);
      await page.$eval('#team-flow-modal .team-flow-coming .team-flow-card', node => node.click());
      assert.equal(await page.$eval('#team-flow-modal .team-flow-option.selected', node => node.dataset.flowKey), 'leader-dispatcher', 'the coming-later card cannot be selected');
      await page.$eval('#team-flow-modal .team-flow-option[data-flow-key="leader-workers"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .team-flow-actions .btn-primary').disabled);
      const next = async expected => { await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click()); await page.waitForFunction(text => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === text, {}, expected); };
      await next('Leader');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-banner', node => node.textContent), 'Leader is kept between B → A migration');
      await next('Dispatcher');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-banner', node => node.textContent), "Dispatcher isn't used in A. It will hand off its context to the leader and will be stopped");
      assert.deepEqual(await page.$eval('#team-flow-modal', node => [node.querySelectorAll('.team-setup-target').length, node.querySelector('.team-wizard-leaving').textContent]), [0, 'dax · Box · Claude · Opus']);
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-stopped', node => node.textContent), '2 stopped agents, unchanged', 'the leaving role lists active agents and counts the stopped ones');
      assert.equal(await page.$('#team-flow-modal .team-wizard-recommended'), null, 'the recommendations panel is hidden while the table has no row');
      await page.close();
    }
    // A role the new flow needs whose agent was stopped by an earlier switch is started again under its own name: no name field, no "new agent" wording.
    {
      const agent = (id, slug, extra = {}) => ({ agent_id: id, slug, machine_id: 'm1', tool: 'claude', variant: 'native', launcher_id: 'l1', launcher_label: 'Claude', model: 'm1', ...extra });
      const plan = { from_key: 'leader-workers', to_key: 'leader-dispatcher', roles: [
        { role_key: 'leader', status: 'kept', agents: [agent('agt_l', 'ada')] }, { role_key: 'dispatcher', status: 'new', agents: [agent('agt_d', 'dax', { state: 'stopped' })] }, { role_key: 'worker', status: 'kept', agents: [] }] };
      const auto = base();
      auto['GET /api/projects/p1/team-flow'] = { key: 'leader-workers', rev: 5, review_pending: false, template: flows[0], conflicts: [] };
      auto['POST /api/projects/p1/team-flow/migration/preview'] = { ...plan, can_apply: true, created_rooms: [], conflicts: [] };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#project-team-flow .btn-primary');
      await page.$eval('#project-team-flow .btn-primary', node => node.click());
      await page.waitForSelector('#team-flow-modal .team-flow-option[data-flow-key="leader-dispatcher"] .team-flow-card');
      await page.$eval('#team-flow-modal .team-flow-option[data-flow-key="leader-dispatcher"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .team-flow-actions .btn-primary').disabled);
      const next = async expected => { await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click()); await page.waitForFunction(text => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === text, {}, expected); };
      await next('Leader'); await next('Dispatcher');
      assert.equal(await page.$eval('#team-flow-modal .team-wizard-banner', node => node.textContent), 'Dispatcher was stopped earlier and will be started again in B');
      assert.deepEqual(await page.$eval('#team-flow-modal', node => [!!node.querySelector('.team-setup-name'), node.querySelectorAll('.team-setup-target select').length > 0, node.textContent.includes('dax keeps its name')]), [false, true, true], 'the stopped agent keeps its name; only the model is chosen');
      await next('Workers and reviewers');
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-modal #team-flow-home-title')?.textContent === 'Summary' && document.querySelector('#team-flow-modal .team-wizard-notice'));
      assert.equal(await page.evaluate(() => { const finalBody = window.__requests.filter(request => request.path.endsWith('/migration/preview')).at(-1).body; return ['name' in (finalBody.dispatcher || {}), !!finalBody.dispatcher]; }).then(String), 'false,true', 'the target is sent, a name never is, for an agent that is started again');
      assert.match(await page.$eval('#team-flow-modal .team-setup-summary', node => node.textContent), /Dispatcher.*dax · Box · Claude · Opus is started again/s);
      await page.close();
    }
    // Choosing Custom from another flow keeps the existing apply: no agents change.
    {
      const auto = base();
      auto['GET /api/projects/p1/team-flow'] = { key: 'leader-workers', rev: 3, review_pending: false, template: flows[0], conflicts: [] };
      const page = await mount({ auto });
      await ready(page);
      await page.waitForSelector('#project-team-flow .btn-primary');
      await page.$eval('#project-team-flow .btn-primary', node => node.click());
      await page.waitForSelector('#team-flow-modal .team-flow-option[data-flow-key="custom"] .team-flow-card');
      await page.$eval('#team-flow-modal .team-flow-option[data-flow-key="custom"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .team-flow-actions .btn-primary').disabled);
      assert.equal(await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.textContent.trim()), 'Use Custom');
      await page.$eval('#team-flow-modal .team-flow-actions .btn-primary', node => node.click());
      const apply = await waitRequest(page, 'PUT', '/api/projects/p1/team-flow');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, apply), { key: 'custom', rev: 3 });
      await answer(page, apply, 200, { key: 'custom', rev: 4, review_pending: false, created_rooms: [], conflicts: [] });
      await page.waitForFunction(() => !document.querySelector('#team-flow-modal .team-flow-card') && document.querySelector('#project-team-flow .team-flow-current')?.textContent.includes('Custom'));
      await page.close();
    }
    // A failed flow read leaves the composer usable and offers a retry; a successful retry restores the pending setup state.
    {
      const auto = base(); auto['GET /api/team-flows'] = rule(503, { error: 'temporarily unavailable' });
      auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      const page = await mount({ auto });
      await page.evaluate(async () => {
        const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js');
        window.__setupChanges = [];
        window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {}, onSetupChange: pending => window.__setupChanges.push(pending) });
        window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' });
      });
      await page.waitForSelector('#team-flow-home .error');
      assert.equal(await page.evaluate(() => window.__setupChanges.at(-1)), false, 'a failed load does not lock the composer');
      assert.equal(await page.$eval('#team-flow-home button', button => button.textContent), 'Retry');
      await page.evaluate(templates => { window.__auto['GET /api/team-flows'] = { templates }; }, flows);
      await page.$eval('#team-flow-home button', button => button.click());
      await page.waitForFunction(() => window.__setupChanges.at(-1) === true && !!document.querySelector('#team-flow-home .team-setup-wizard'));
      await page.close();
    }
    // A stale load must not re-disable the composer while a newer flow read is pending.
    {
      const page = await mount();
      await page.evaluate(async () => {
        const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js');
        window.__setupChanges = [];
        window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onSetupChange: pending => window.__setupChanges.push(pending) });
        window.__homeFlow.setRoom({ id: 'r1', kind: 'home', project_id: 'p1' });
      });
      await page.waitForFunction(() => window.__setupChanges.at(-1) === false);
      await page.evaluate(() => { delete window.__auto['GET /api/projects/p1/team-flow']; window.__setupChanges = []; window.__firstLoad = window.__homeFlow.load('p1'); window.__secondLoad = window.__homeFlow.load('p1'); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/projects/p1/team-flow' && !request.answered).length === 2);
      const reads = await page.evaluate(() => window.__requests.map((request, index) => request.path === '/api/projects/p1/team-flow' && !request.answered ? index : -1).filter(index => index >= 0));
      await answer(page, reads[0], 200, { key: '', rev: 0, review_pending: true });
      await page.evaluate(() => window.__firstLoad);
      assert.deepEqual(await page.evaluate(() => window.__setupChanges), [], 'a superseded read cannot mark setup pending');
      await answer(page, reads[1], 200, { key: 'custom', rev: 1, review_pending: false, template: flows[3] });
      await page.evaluate(() => window.__secondLoad);
      assert.deepEqual(await page.evaluate(() => window.__setupChanges), [false], 'the current flow read enables the composer');
      await page.close();
    }
    // The inline home setup starts with A for a new project; migrated C can be kept in one click and stays dismissed after reload.
    {
      const auto = base(); auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      auto['GET /api/projects/p1/settings'] = { reviewer_agents: 'on', reviewer_agents_global_enabled: true, reviewer_agents_enabled: true };
      auto['POST /api/projects/p1/team-setup/preview'] = { can_create: true, settings: { worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10, reviewer_agents: 'on', reviewer_agents_global_enabled: true }, created_rooms: [], conflicts: [] };
      auto['GET /api/spawn-catalog?machine_id=m1'] = { tools: [
        { tool: 'claude', variant: 'native', launcher_id: 'l1', label: 'Claude Code', available: true, models: [{ id: 'm1', label: 'Opus', default: true }, { id: 'm2', label: 'Sonnet' }] },
        { tool: 'codex', variant: 'native', launcher_id: 'l2', label: 'Codex', available: true, models: [{ id: 'gpt-6-sol', label: 'GPT-6 Sol' }, { id: 'gpt-6-luna', label: 'GPT-6 Luna' }, { id: 'gpt-6-astra', label: 'GPT-6 Astra' }] },
      ] };
      auto['GET /api/team-flows/recommended-models'] = { rows: ['leader', 'dispatcher', 'worker', 'reviewer'].flatMap(role => ['leader-workers', 'leader-dispatcher'].flatMap(flow => [
        { role_key: role, flow_key: flow, tool: 'claude', models: [{ model: 'm2', why: 'Claude choice' }] },
        { role_key: role, flow_key: flow, tool: 'codex', models: [{ model: 'gpt-6-sol', why: 'Codex choice' }, { model: 'missing', why: 'not offered' }] },
      ])) };
      const page = await mount({ auto });
      await page.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeRole = ''; window.__homeFlow = new TeamFlowHome({ canEdit: () => window.__homeRole === 'owner', onApplied: () => {} }); window.__homeRole = 'owner'; window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' }); });
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-home .team-flow-card').length === 5);
      assert.deepEqual(await page.evaluate(() => {
        const host = document.querySelector('#team-flow-home'), card = host.querySelector('.team-setup-wizard');
        return [getComputedStyle(document.querySelector('#messages')).display, getComputedStyle(document.querySelector('.send-bar')).display,
          host.clientHeight > 600, Math.abs(card.clientWidth - host.clientWidth + 32) < 3];
      }), ['none', 'block', true, true], 'setup fills the message area while the composer stays visible');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-flow-card', cards => cards.map(card => card.disabled)), [false, false, false, true, false], 'owner permission is read after the home page mounts');
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#team-flow-home .team-flow-card')].map(node => [node.getAttribute('aria-checked'), [...node.querySelectorAll('.team-flow-thumbnail-agent text')].map(text => text.textContent)])),
        [['true', ['Leader', 'Worker', 'Worker', 'Worker']], ['false', ['Leader', 'Dispatcher', 'Worker', 'Worker', 'Worker']], ['false', ['Leader', 'Worker', 'Worker', 'Worker', 'Advisor']], ['false', []], ['false', []]], 'the topology comes from spawn rules, not the room diagram; core roles outside the spawn path attach to the leader');
      assert.equal(await page.$eval('#team-flow-home .team-flow-option[data-flow-key="leader-advisor"] .team-flow-thumbnail-consults title', node => node.textContent), 'consults', 'the advisor hangs off the leader with a dashed edge named consults');
      assert.equal(await page.$eval('#team-flow-home .team-flow-option[data-flow-key="leader-advisor"] .team-flow-thumbnail-consults line', node => getComputedStyle(node).strokeDasharray !== 'none'), true);
      assert.equal(await page.$eval('#team-flow-home .team-flow-option[data-flow-key="leader-advisor"] .team-flow-letter', node => node.textContent), 'C', 'letters follow the display order');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-flow-option', options => [options.map(option => option.dataset.flowKey), options.map(option => option.querySelector('.team-flow-letter').textContent)]), [['leader-workers', 'leader-dispatcher', 'leader-advisor', 'coming-later', 'custom'], ['A', 'B', 'C', '·', 'D']], 'the "coming later" card sits right before Custom, which is always the last option');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-flow-option', options => options.map(option => option.querySelector('.team-flow-recommendation')?.textContent || '')), ['Recommended for small to medium projects.', "Recommended for large projects; the dispatcher handles routing so the leader's context stays lean.", "Recommended when a fast, cheaper leader should get a stronger model's judgment on key decisions.", '', 'For setting up rooms and roles yourself.'], 'each flow says when it fits');
      assert.deepEqual(await page.$eval('#team-flow-home .team-flow-coming .team-flow-card', card => [card.disabled, card.getAttribute('aria-disabled'), card.textContent.includes('Leader, advisor and dispatcher')]), [true, 'true', true], 'the combined flow is shown but cannot be chosen');
      // Rows stay compact so many variants fit on one screen: every card is at most 125px tall on a desktop width, and the first four fit well inside the setup area.
      assert.equal(await page.$$eval('#team-flow-home .team-flow-option', options => Math.max(...options.map(option => option.getBoundingClientRect().height)) <= 125), true, 'each flow card is compact');
      const fitted = () => page.$$eval('#team-flow-home .team-flow-option:nth-child(-n+2)', options => options.map(option => {
        const frame = option.querySelector('.team-flow-thumbnail').getBoundingClientRect();
        const worker = option.querySelector('.team-flow-thumbnail-agent:last-child rect').getBoundingClientRect();
        const expand = option.querySelector('.team-flow-diagram-open span').getBoundingClientRect();
        return worker.left >= frame.left && worker.right <= frame.right && worker.top >= frame.top && worker.bottom <= expand.top - 2;
      }));
      assert.deepEqual(await fitted(), [true, true], 'desktop thumbnails fit above the expand control');
      assert.equal(await page.$('script[src*="mermaid"]'), null, 'thumbnail does not load Mermaid at page start');
      await page.$eval('#team-flow-home .team-flow-diagram-open', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home .team-flow-lightbox')?.open && !!document.querySelector('#team-flow-home .team-flow-lightbox svg'));
      assert.deepEqual(await page.$eval('#team-flow-home .team-flow-lightbox svg', svg => [svg.textContent.includes('Leader'), svg.textContent.includes('Worker'), svg.textContent.includes('Home')]), [true, true, false], 'expanded view shows the agent topology too');
      await page.$eval('#team-flow-home .team-flow-lightbox-close', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-home .team-flow-lightbox')?.open);
      await page.setViewport({ width: 390, height: 844 });
      assert.equal(await page.$eval('#team-flow-home .team-flow-card', row => getComputedStyle(row).gridTemplateColumns.split(' ').length), 1, 'phone layout stacks the diagram under readable text');
      assert.equal(await page.$$eval('#team-flow-home .team-flow-option', options => Math.max(...options.map(option => option.getBoundingClientRect().height)) <= 240), true, 'phone cards remain scannable');
      assert.deepEqual(await fitted(), [true, true], 'phone thumbnails fit above the expand control');
      await page.setViewport({ width: 1280, height: 800 });
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-wizard-recommended-group', nodes => nodes.map(node => [node.querySelector('strong').textContent, node.querySelector('li').textContent])), [['Claude Code · Selected', 'Sonnet — Claude choice'], ['Codex', 'GPT-6 Sol — Codex choice']], 'leader lists every available harness against its live models');
      await page.select('#team-flow-home [aria-label="Leader tool"]', 'l2');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-wizard-recommended-group', nodes => nodes.map(node => node.querySelector('strong').textContent)), ['Claude Code', 'Codex · Selected'], 'the current harness is highlighted after a tool change');
      await page.select('#team-flow-home [aria-label="Leader tool"]', 'l1');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers and reviewers');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-wizard-recommended', nodes => nodes.map(node => [...node.querySelectorAll('.team-wizard-recommended-group')].map(group => group.querySelector('strong').textContent))), [['Claude Code · Selected', 'Codex'], ['Claude Code · Selected', 'Codex']], 'workers and reviewers both include Claude and Codex');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Project settings');
      await page.waitForFunction(() => document.querySelector('#team-setup-reviewer-agents')?.value === 'on');
      assert.equal(await page.$eval('#team-flow-home .team-setup-limit-hint', node => node.textContent), 'Leader is not counted.', 'flow A excludes only its leader');
      await page.$$eval('#team-flow-home .team-flow-actions .btn:not(.btn-primary)', nodes => nodes[0].click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers and reviewers');
      await page.$$eval('#team-flow-home .team-flow-actions .btn:not(.btn-primary)', nodes => nodes[0].click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      await page.$$eval('#team-flow-home .team-flow-actions .btn:not(.btn-primary)', nodes => nodes[0].click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Choose how your AI team works');
      await page.$eval('#team-flow-home .team-flow-option:nth-child(2) .team-flow-card', node => node.click());
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
      assert.equal(await page.$eval('#team-flow-home .btn-primary', node => node.textContent.trim()), 'Next');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Dispatcher');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-wizard-recommended-group', nodes => nodes.map(node => node.querySelector('strong').textContent)), ['Claude Code · Selected', 'Codex'], 'dispatcher includes both harnesses');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers and reviewers');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-wizard-recommended', nodes => nodes.map(node => [...node.querySelectorAll('.team-wizard-recommended-group')].map(group => group.querySelector('strong').textContent))), [['Claude Code · Selected', 'Codex'], ['Claude Code · Selected', 'Codex']], 'flow B workers and reviewers include both harnesses');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Project settings');
      assert.equal(await page.$eval('#team-flow-home [aria-label="Project folder"]', node => node.readOnly), true);
      assert.deepEqual(await page.$eval('#team-flow-home .team-setup-settings', node => [node.querySelector('[type=number]').min, node.querySelector('[type=number]').closest('label').textContent.trim(), node.querySelector('.team-setup-limit-hint').textContent]),
        ['1', 'Maximum workers and reviewersLeader and dispatcher are not counted.', 'Leader and dispatcher are not counted.'], 'the worker cap excludes the flow core roles');
      await page.evaluate(() => { delete window.__auto['POST /api/projects/p1/team-setup/preview']; });
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      const setupPreview = await waitRequest(page, 'POST', '/api/projects/p1/team-setup/preview');
      const draft = await page.evaluate(index => window.__requests[index].body, setupPreview);
      assert.deepEqual([draft.flow_key, draft.flow_rev, draft.leader?.machine_id, draft.dispatcher?.machine_id, draft.settings?.max_live_agents], ['leader-dispatcher', 0, 'm1', 'm1', 10]);
      assert.deepEqual(Object.keys(draft.settings).sort(), ['max_live_agents', 'memory_enabled', 'reviewer_agents', 'worktree_mode'], 'setup sends only accepted settings keys');
      assert.equal(draft.settings.reviewer_agents, 'on', 'setup previews the loaded reviewer override');
      assert.equal(await calls(page, 'PUT').then(rows => rows.some(([path]) => path.includes('/team-flow'))), false, 'wizard does not apply the flow early');
      await answer(page, setupPreview, 200, { can_create: true, created_rooms: [{ key: 'routing', title: 'routing' }], conflicts: [{ agent_id: 'agt_old', slug: 'ian', role_key: 'leader', role_label: 'Leader', room_id: 'rm_general', room_title: 'general', reason: 'Leader in task room' }] });
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Summary' && !document.querySelector('#team-flow-home .btn-primary')?.disabled);
      assert.equal(await page.$eval('#team-flow-home .team-flow-removal input', node => node.checked), false, 'setup removal starts unchecked');
      await page.$eval('#team-flow-home .team-flow-removal input', node => node.click());
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      const create = await waitRequest(page, 'POST', '/api/projects/p1/team-setup');
      assert.deepEqual(await page.evaluate(index => Object.keys(window.__requests[index].body.settings).sort(), create), ['max_live_agents', 'memory_enabled', 'reviewer_agents', 'worktree_mode'], 'create uses the same accepted settings keys');
      assert.equal(await page.evaluate(index => window.__requests[index].body.settings.reviewer_agents, create), 'on', 'create keeps the loaded reviewer override');
      assert.equal(await page.evaluate(index => !!window.__requests[index].headers['Idempotency-Key'], create), true, 'create uses an idempotency key');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body.remove_memberships, create), [{ agent_id: 'agt_old', room_id: 'rm_general' }], 'setup removes only checked memberships');
      await answer(page, create, 200, { flow: { key: 'leader-dispatcher', rev: 1, review_pending: false }, agents: [{ role: 'leader', agent_id: 'agt_1' }, { role: 'dispatcher', agent_id: 'agt_2' }], created_rooms: [{ key: 'routing' }] });
      await page.waitForFunction(() => !document.querySelector('#team-flow-home .team-flow-home-card'));
      await page.close();
      const migrated = base(); migrated['GET /api/projects/p1/team-flow'] = { key: 'custom', rev: 4, review_pending: true, template: flows[3] };
      const kept = await mount({ auto: migrated });
      await kept.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {} }); window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' }); });
      await kept.waitForFunction(() => document.querySelector('#team-flow-home .team-flow-option.selected')?.textContent.includes('Custom'));
      await kept.waitForFunction(() => !document.querySelector('#team-flow-home .btn-primary')?.disabled);
      assert.equal(await kept.$eval('#team-flow-home .btn-primary', node => node.textContent.trim()), 'Keep current');
      await kept.$eval('#team-flow-home .btn-primary', node => node.click());
      const apply = await waitRequest(kept, 'PUT', '/api/projects/p1/team-flow');
      assert.deepEqual(await kept.evaluate(index => window.__requests[index].body, apply), { key: 'custom', rev: 4 });
      await answer(kept, apply, 200, { key: 'custom', rev: 5, review_pending: false, created_rooms: [], conflicts: [] });
      await kept.waitForFunction(() => !document.querySelector('#team-flow-home .team-flow-home-card'));
      await kept.evaluate(() => { window.__auto['GET /api/projects/p1/team-flow'] = { key: 'custom', rev: 5, review_pending: false, template: null }; window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' }); });
      await settle(kept);
      assert.equal(await kept.$('#team-flow-home .team-flow-home-card'), null, 'the server-backed acknowledgment keeps setup dismissed');
      await kept.close();
    }
    // Flow D: the advisor step preselects a stronger model than the leader's, follows the leader's harness until touched, and sends `advisor`.
    {
      const auto = base(); auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      auto['GET /api/rooms/r1/agent-name'] = { name: 'ada' }; auto['GET /api/rooms/r1/agent-name?exclude=ada'] = { name: 'bea' }; auto['GET /api/rooms/r1/agent-name?exclude=zed'] = { name: 'cy' };
      auto['GET /api/projects/p1/role-spawn-defaults'] = [{ role_key: 'advisor', inherited: false, global_agents: [], default_agents: [{ launcher_id: 'l1', model: 'strong' }], pick_mode: 'in_turn' }];
      auto['GET /api/spawn-catalog?machine_id=m1'] = { tools: [{ tool: 'claude', variant: 'native', launcher_id: 'l1', label: 'Claude', available: true, models: [{ id: 'strong', label: 'Strong' }, { id: 'fast', label: 'Fast', default: true }] }] };
      const page = await mount({ auto });
      await page.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {} }); window.__homeFlow.setRoom({ id: 'r1', kind: 'home', project_id: 'p1' }); });
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-home .team-flow-card').length === 5);
      await page.$eval('#team-flow-home .team-flow-option[data-flow-key="leader-advisor"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
      const title = () => page.$eval('#team-flow-home-title', node => node.textContent);
      const next = async expected => { await page.$eval('#team-flow-home .btn-primary', node => node.click()); await page.waitForFunction(text => document.querySelector('#team-flow-home-title')?.textContent === text, {}, expected); };
      await next('Leader');
      assert.equal(await page.$eval('#team-flow-home [aria-label="Leader tool"]', node => node.closest('fieldset').querySelector('select[name=model]').value), 'fast', 'the leader falls back to the harness default');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-setup-target select', nodes => { const rects = nodes.map(node => node.getBoundingClientRect()); return [new Set(rects.map(rect => Math.round(rect.top))).size, new Set(rects.map(rect => Math.round(rect.height))).size, Math.round(rects[0].height)]; }), [1, 1, 40], 'Machine, Tool and Model selects share one row and the 40px control height');
      assert.equal(await page.$eval('#team-flow-home .model-picker input[type=search]', node => node.getBoundingClientRect().top >= document.querySelector('#team-flow-home .model-picker select').getBoundingClientRect().bottom), true, 'the model filter sits under the Model select');
      // The Name field is Spawn's: a free name is prefilled when the step opens, the dice asks for another, and typing wins over both.
      await page.waitForFunction(() => document.querySelector('#team-setup-leader-name')?.value === 'ada');
      await page.$eval('#team-setup-leader-name-dice', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-setup-leader-name')?.value === 'bea');
      await page.$eval('#team-setup-leader-name', node => { node.focus(); node.select(); });
      await page.keyboard.type('zed');
      assert.equal(await page.$eval('#team-setup-leader-name', node => node.value), 'zed');
      assert.deepEqual(await page.$eval('#team-setup-leader-name', node => [node.maxLength, node.placeholder]), [21, 'Automatic']);
      await next('Advisor');
      await page.waitForFunction(() => document.querySelector('#team-setup-advisor-name')?.value === 'cy');
      assert.equal(await page.$eval('#team-flow-home select[name=model]', node => node.value), 'strong', 'the advisor preselects its role default');
      assert.equal(await page.$eval('#team-flow-home .team-setup-hint', node => node.textContent), "Pick a stronger model than the leader's.");
      await next('Workers and reviewers');
      await next('Project settings');
      assert.equal(await page.$eval('#team-flow-home .team-setup-limit-hint', node => node.textContent), 'Leader and advisor are not counted.');
      await page.evaluate(() => { delete window.__auto['POST /api/projects/p1/team-setup/preview']; });
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      const setupPreview = await waitRequest(page, 'POST', '/api/projects/p1/team-setup/preview');
      const draft = await page.evaluate(index => window.__requests[index].body, setupPreview);
      assert.deepEqual([draft.flow_key, draft.leader?.model, draft.advisor?.model, draft.advisor?.launcher_id, draft.dispatcher], ['leader-advisor', 'fast', 'strong', 'l1', undefined]);
      assert.deepEqual([draft.leader?.name, draft.advisor?.name], ['zed', 'cy'], 'the chosen names are sent with their targets');

      await answer(page, setupPreview, 200, { can_create: true, created_rooms: [{ key: 'advice', title: 'advice' }], conflicts: [] });
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Summary' && !document.querySelector('#team-flow-home .btn-primary')?.disabled);
      assert.match(await page.$eval('.team-setup-summary', node => node.textContent), /C · Leader and advisor.*Leader: zed · .*Fast.*Advisor: cy · .*Strong.*Rooms to create: advice/s);
      assert.equal(await title(), 'Summary');
      await page.close();
    }
    // A v1 agent that held the leader role is offered next to a new leader, continuing it is the default, and it is sent with the Spawn's v1 parameters.
    {
      const auto = base(); auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      auto['GET /api/rooms/r1/agent-name'] = { name: 'ada' };
      auto['GET /api/projects/p1/legacy-agents?role=leader'] = { agents: [{ legacy_id: 'quin@alpha', slug: 'quin', tool: 'claude', model: 'm2', cwd: '/srv/alpha', role_key: 'leader', resumable: true, v1_active: true }] };
      auto['GET /api/projects/p1/role-spawn-defaults'] = [{ role_key: 'leader', inherited: false, global_agents: [], default_agents: [{ launcher_id: 'l1', model: 'm1' }], pick_mode: 'in_turn' }];
      const page = await mount({ auto });
      await page.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {} }); window.__homeFlow.setRoom({ id: 'r1', kind: 'home', project_id: 'p1' }); });
      await page.waitForFunction(() => document.querySelectorAll('#team-flow-home .team-flow-card').length === 5);
      await page.$eval('#team-flow-home .team-flow-option[data-flow-key="leader-workers"] .team-flow-card', node => node.click());
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      await page.waitForSelector('#team-flow-home .team-setup-legacy');
      assert.deepEqual(await page.$$eval('#team-flow-home .team-setup-legacy label:has(input[type=radio])', nodes => nodes.map(node => [node.textContent.trim(), node.querySelector('input').checked])), [['Continue v1 leader quin@alpha', true], ['New leader', false]], 'the v1 leader is offered, and is the default');
      assert.equal(await page.$('#team-setup-leader-name'), null, 'a continued agent keeps its name, so there is no Name field');
      assert.equal(await page.$eval('#team-flow-home select[name=model]', node => node.value), 'm2', 'the model v1 ran is preselected');
      assert.equal(await page.$eval('#team-flow-home .team-flow-actions .btn-primary', node => node.disabled), true, 'a v1 session seen a moment ago needs the confirmation first');
      await page.$eval('.team-setup-legacy-confirm', node => node.click());
      await page.waitForFunction(() => !document.querySelector('#team-flow-home .team-flow-actions .btn-primary').disabled);
      await page.$eval('#team-flow-home .team-flow-actions .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers and reviewers');
      await page.$eval('#team-flow-home .team-flow-actions .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Project settings');
      await page.evaluate(() => { delete window.__auto['POST /api/projects/p1/team-setup/preview']; });
      await page.$eval('#team-flow-home .team-flow-actions .btn-primary', node => node.click());
      const setupPreview = await waitRequest(page, 'POST', '/api/projects/p1/team-setup/preview');
      const draft = await page.evaluate(index => window.__requests[index].body, setupPreview);
      assert.deepEqual([draft.leader.resume_legacy, draft.leader.confirm_v1_idle, draft.leader.name, draft.leader.model, draft.leader.tool], ['quin@alpha', true, undefined, 'm2', 'claude'], 'the leader is sent with the Spawn\'s v1 parameters');
      await answer(page, setupPreview, 200, { can_create: true, created_rooms: [], conflicts: [] });
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Summary');
      assert.match(await page.$eval('.team-setup-summary', node => node.textContent), /Leader: quin@alpha \(continues its v1 session\)/);
      // Back to the leader step: choosing a new leader goes back to a plain, named one.
      for (let i = 0; i < 3; i++) await page.$eval('#team-flow-home .team-flow-actions .btn:not(.btn-primary)', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      await page.$eval('.team-setup-legacy input[value=new]', node => node.click());
      await page.waitForSelector('#team-setup-leader-name');
      await page.evaluate(() => { delete window.__auto['POST /api/projects/p1/team-setup/preview']; });
      for (let i = 0; i < 3; i++) { await page.$eval('#team-flow-home .team-flow-actions .btn-primary', node => node.click()); await settle(page); }
      const plain = await waitRequest(page, 'POST', '/api/projects/p1/team-setup/preview');
      const plainDraft = await page.evaluate(index => window.__requests[index].body, plain);
      assert.deepEqual([plainDraft.leader.resume_legacy, plainDraft.leader.confirm_v1_idle, plainDraft.leader.name], [undefined, undefined, 'ada'], 'a new leader carries no v1 parameters');
      await page.close();
    }
    // Initial setup follows the inherited reviewer switch and can opt a project back in.
    {
      const auto = base();
      auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      auto['GET /api/projects/p1/settings'] = { reviewer_agents: 'inherit', reviewer_agents_global_enabled: false, reviewer_agents_enabled: false };
      auto['POST /api/projects/p1/team-setup/preview'] = { can_create: true, settings: { worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10, reviewer_agents: 'inherit', reviewer_agents_global_enabled: false }, created_rooms: [], conflicts: [] };
      const page = await mount({ auto });
      await page.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {} }); window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' }); });
      await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Leader');
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers');
      assert.equal(await page.$eval('#team-flow-home .team-setup-defaults', node => node.textContent.includes('Reviewers')), false);
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Project settings');
      assert.equal(await page.$eval('#team-setup-reviewer-agents', node => node.value), 'inherit');
      await page.select('#team-setup-reviewer-agents', 'on');
      await page.$eval('#team-flow-home .team-flow-actions .btn:not(.btn-primary)', node => node.click());
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Workers and reviewers');
      assert.equal(await page.$eval('#team-flow-home .team-setup-defaults', node => node.textContent.includes('Reviewers')), true);
      await page.close();
    }
    // A failed settings read must not replace an existing project override with the wizard's initial Inherit value.
    {
      const auto = base();
      auto['GET /api/projects/p1/team-flow'] = { key: '', rev: 0, review_pending: true };
      auto['GET /api/projects/p1/settings'] = rule(503, { error: 'temporarily unavailable' });
      auto['POST /api/projects/p1/team-setup/preview'] = rule(503, { error: 'temporarily unavailable' });
      const page = await mount({ auto });
      await page.evaluate(async () => { const { TeamFlowHome } = await import('/js/pages/project/team-flow-home.js'); window.__homeFlow = new TeamFlowHome({ canEdit: () => true, onApplied: () => {} }); window.__homeFlow.setRoom({ kind: 'home', project_id: 'p1' }); });
      await page.waitForFunction(() => window.__requests.some(row => row.path === '/api/projects/p1/settings' && row.answered) && window.__requests.some(row => row.path === '/api/projects/p1/team-setup/preview' && row.answered));
      for (const title of ['Leader', 'Workers and reviewers', 'Project settings']) {
        await page.waitForFunction(() => { const button = document.querySelector('#team-flow-home .btn-primary'); return !!button && !button.disabled; }); // the setup page is a lazily fetched chunk
        await page.$eval('#team-flow-home .btn-primary', node => node.click());
        await page.waitForFunction(title => document.querySelector('#team-flow-home-title')?.textContent === title, {}, title);
      }
      await page.$eval('#team-flow-home .team-setup-settings input[type=checkbox]', node => node.click());
      await page.evaluate(() => { delete window.__auto['POST /api/projects/p1/team-setup/preview']; });
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      const preview = await waitRequest(page, 'POST', '/api/projects/p1/team-setup/preview');
      assert.equal(await page.evaluate(index => Object.hasOwn(window.__requests[index].body.settings, 'reviewer_agents'), preview), false, 'failed loads omit the untouched reviewer override from preview');
      await answer(page, preview, 200, { can_create: true, created_rooms: [], conflicts: [] });
      await page.waitForFunction(() => document.querySelector('#team-flow-home-title')?.textContent === 'Summary' && !document.querySelector('#team-flow-home .btn-primary')?.disabled);
      await page.$eval('#team-flow-home .btn-primary', node => node.click());
      const create = await waitRequest(page, 'POST', '/api/projects/p1/team-setup');
      assert.equal(await page.evaluate(index => Object.hasOwn(window.__requests[index].body.settings, 'reviewer_agents'), create), false, 'failed loads omit the untouched reviewer override from create');
      await page.close();
    }
    // Two roles outside the spawn path hang under the leader one below the other: no two boxes overlap and none is clipped.
    {
      const page = await mount();
      await page.evaluate(async () => {
        const { TeamFlowCards } = await import('/js/pages/project/team-flow-page.js');
        const { html, render } = await import('/js/ui.js');
        const host = document.createElement('div'); host.id = 'two-attached'; host.style.cssText = 'position:fixed;inset:0;z-index:99999;background:#000;width:900px'; document.body.append(host);
        const template = { key: 'x', label: 'X', description: 'Two attached roles.', core_roles: ['leader', 'advisor', 'auditor'], spawn_rules: [{ actor_role: 'leader', allowed_roles: ['worker'] }] };
        render(html`<${TeamFlowCards} templates=${[template]} selected="x" onSelect=${() => {}} />`, host);
      });
      await page.waitForSelector('#two-attached .team-flow-thumbnail-agent');
      assert.deepEqual(await page.$$eval('#two-attached .team-flow-thumbnail-agent text', nodes => nodes.map(node => node.textContent)), ['Leader', 'Worker', 'Worker', 'Worker', 'Advisor', 'Auditor']);
      assert.deepEqual(await page.$eval('#two-attached', host => {
        const frame = host.querySelector('.team-flow-thumbnail').getBoundingClientRect();
        const boxes = [...host.querySelectorAll('.team-flow-thumbnail-agent rect')].map(node => node.getBoundingClientRect());
        const overlaps = boxes.flatMap((a, i) => boxes.slice(i + 1).filter(b => a.left < b.right && a.right > b.left && a.top < b.bottom && a.bottom > b.top)).length;
        return [overlaps, boxes.filter(box => box.top < frame.top - .5 || box.bottom > frame.bottom + .5).length, host.querySelectorAll('.team-flow-thumbnail-consults').length];
      }), [0, 0, 2], 'two attached roles do not overlap each other or the workers and stay inside the frame, each with its own dashed edge');
      await page.close();
    }
    // A worker-room reference displays exactly the granted message, as plain text and without opening the source room.
    {
      const auto = base();
      auto['GET /api/messages/referenced/77?source_message_id=src1'] = { no: 77, available: true, room_id: 'routing', room: 'routing', from: 'lead', body: 'Private brief\nOnly this message.' };
      auto['GET /api/messages/referenced/78?source_message_id=src1'] = { no: 78, available: false };
      const page = await mount({ auto });
      await page.evaluate(async () => {
        const { ReferencedMessages } = await import('/js/pages/chat/rows.js');
        const { html, render } = await import('/js/ui.js');
        const host = document.createElement('div'); host.id = 'reference-test'; document.body.append(host);
        render(html`<${ReferencedMessages} message=${{ id: 'src1', body: 'Read #77 and #78' }} />`, host);
      });
      await page.waitForSelector('#reference-test .chat-reference');
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#reference-test .chat-reference').length, document.querySelector('#reference-test .chat-reference pre').textContent, document.querySelectorAll('#reference-test a').length]), [1, 'Private brief\nOnly this message.', 0]);
      await page.close();
    }
    // Who may change what: an admin edits the folder; someone who is neither sees the form read-only and the archive controls off with the reason.
    {
      const admin = await mount({ role: 'viewer', admin: true });
      await ready(admin);
      assert.deepEqual(await admin.evaluate(() => [document.querySelector('#project-details-name').disabled, document.querySelector('#project-details-folder').disabled, document.querySelector('#project-details-status').textContent, document.querySelector('#project-archive').disabled]), [false, false, '', false]);
      await admin.close();
      const occupied = await mount({ admin: true, folderInUse: true });
      await ready(occupied);
      assert.deepEqual(await occupied.evaluate(() => [document.querySelector('#project-details-folder').readOnly, document.querySelector('#project-details-status').textContent]), [true, 'The folder is fixed while this project has agents or worktrees.']);
      await occupied.close();
      const viewer = await mount({ role: 'viewer' });
      await ready(viewer);
      await viewer.waitForFunction(() => document.querySelector('#project-team-flow .team-flow-current')?.textContent.includes('Custom'));
      await settle(viewer);
      assert.deepEqual(await viewer.evaluate(() => [window.__requests.filter(request => request.path.endsWith('/team-flow/preview')).length, document.querySelector('#project-team-flow .team-flow-preview'), document.querySelector('#project-team-flow .error').textContent.trim(), document.querySelector('#project-team-flow .team-flow-settings .btn-primary').disabled, document.querySelectorAll('#team-flow-modal .team-flow-card').length]), [0, null, '', true, 0], 'viewer sees the current flow without requesting a restricted preview');
      assert.deepEqual(await viewer.evaluate(() => [document.querySelector('#project-details-name').disabled, document.querySelector('#project-details-folder').readOnly, document.querySelector('#project-details-save').disabled, document.querySelector('#project-details-status').textContent,
        document.querySelector('#project-archive').disabled, document.querySelector('#project-lifecycle > p').textContent]), [true, true, true, 'Only the project owner can change these.', true, 'Only the project owner or an admin can archive, reopen or delete a project.']);
      await viewer.close();
      const archived = await mount({ archived: true });
      await ready(archived);
      assert.deepEqual(await archived.evaluate(() => [document.querySelector('#project-details-save').disabled, document.querySelector('#project-session-import').hidden, document.querySelector('#project-session-import-open').disabled,
        ['archive', 'reopen', 'delete'].map(key => document.querySelector(`#project-${key}-row`).hidden), document.querySelector('#project-lifecycle > p').textContent.replace(/Archived .+ It/, 'Archived X. It')]),
        [true, true, true, [true, false, false], 'Archived X. It is read-only.']);
      await archived.close();
    }
    // Details: Save enables only for an editable change; a rename sends only the name.
    {
      const page = await mount();
      await ready(page);
      assert.equal(await page.$eval('#project-details-save', el => el.disabled), true, 'unchanged details do not offer a Save action');
      await page.$eval('#project-details-name', el => { el.focus(); el.select(); });
      await page.keyboard.type('Beta');
      await page.$eval('#project-details-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-details-status').textContent === 'Saved.');
      assert.equal(await page.$eval('#project-details-folder', el => el.readOnly), true, 'the folder stays selectable but read-only for a non-admin');
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
    // A project override is saved independently of the instance default.
    {
      const page = await mount({ settings: { worker_recovery_to_lead: true, worker_recovery_escalate_minutes: 30 } });
      await ready(page);
      assert.equal(await page.$eval('#worker-recovery-to-lead', el => el.checked), true, 'worker recoveries go to the lead by default');
      assert.equal(await page.$eval('#worker-recovery-escalate-minutes', el => el.value), '30');
      await page.$eval('#worker-recovery-to-lead', el => el.click());
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'update' && call[2]?.worker_recovery_to_lead === false));
      await page.$eval('#worker-recovery-escalate-minutes', el => { el.value = '45'; el.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'update' && call[2]?.worker_recovery_escalate_minutes === 45));
      await page.close();
    }
    {
      const page = await mount({ settings: { auto_resume_after_clean_diagnostic: true, worktree_mode: 'auto', home_lead_only: false, reviewer_agents: 'inherit', reviewer_agents_global_enabled: false, reviewer_agents_enabled: false } });
      await ready(page);
      assert.equal(await page.$eval('#project-reviewer-agents', el => el.value), 'inherit');
      await page.select('#project-reviewer-agents', 'on');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'update' && call[2]?.reviewer_agents === 'on'));
      assert.equal(await page.$eval('#project-reviewer-agents', el => el.value), 'on');
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
        [['pause', 'Pause project agents', 'Pause project agents', false, 'btn btn-glyph', 'icon-mask-pause'], ['stop', 'Stop project agents', 'Stop project agents', false, 'btn btn-danger btn-glyph', 'icon-mask-stop'], ['resume', 'Retry resume project agents', 'Retry resume project agents', true, 'btn btn-glyph', 'icon-mask-play']]);
      await page.$eval('#project-scope-control [data-scope-action=stop]', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await settle(page);
      assert.equal((await calls(page, 'POST')).filter(([path]) => path.endsWith('/control')).length, 0, 'a cancelled stop sends nothing');
      await page.$eval('#project-scope-control [data-scope-action=pause]', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-scope-control .scope-control-results').textContent.length > 0);
      assert.deepEqual([(await calls(page, 'POST')).find(([path]) => path.endsWith('/control')), await page.evaluate(() => [window.__requests.find(request => request.method === 'POST' && request.path.endsWith('/control')).headers['Idempotency-Key'].length > 8, document.querySelector('#project-scope-control').dataset.desired, document.querySelector('#project-scope-control .scope-control-results').textContent])],
        [['/api/projects/p1/control', { action: 'pause', rev: 7 }], [true, 'paused', 'Agent controls needing attention: busy.']]);
      await page.close();
    }
    // The room's own control and usage are the same components over the room's routes; reset clears the control.
    {
      const page = await mount({ auto: { ...base(), 'GET /api/rooms/r1/control': { desired: 'paused', effective_state: 'paused', rev: 1 }, 'GET /api/rooms/r1/usage?range=today': usage } });
      await page.evaluate(() => { window.__room.load('r1'); window.__roomUsage.load('r1'); });
      await page.waitForFunction(() => document.querySelector('#room-scope-control .scope-control-status').textContent === 'Room agents: Paused' && document.querySelector('#room-usage .usage-popover-rows'));
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-scope-control').dataset.desired, document.querySelector('#room-scope-control > h3').textContent, document.querySelector('#room-usage > h3').textContent, document.querySelector('#room-scope-control .settings-card')]), ['paused', 'Agents homed in this room', "This room's usage", null]);
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
      assert.equal(await page.$eval(`${card(1)} > div:last-child`, el => el.hidden), false);
      await page.$eval(`${card(1)} [aria-label="Add default agent"]`, el => el.click());
      await page.$eval('#project-role-defaults-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-role-defaults .settings-desc[role=status]').textContent === 'Saved.');
      assert.deepEqual((await calls(page, 'PUT')).find(([path]) => path.endsWith('/worker')), ['/api/projects/p1/role-spawn-defaults/worker', { inherit: false, default_agents: [{ launcher_id: 'l1', model: 'm1' }], pick_mode: 'in_turn' }]);
      await page.select(`${card(2)} select`, 'inherit');
      await page.$eval('#project-role-defaults-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-role-defaults .settings-desc[role=status]').textContent === 'Not yours');
      await page.select(`${card(1)} select`, 'inherit');
      await page.$eval('#project-role-defaults-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-role-defaults .settings-desc[role=status]').textContent === 'Not yours');
      await page.$eval('#project-role-defaults-save', el => el.click());
      await page.waitForFunction(() => window.__requests.filter(request => request.method === 'PUT' && request.path.includes('/role-spawn-defaults/')).length === 5);
      assert.deepEqual((await calls(page, 'PUT')).map(([path]) => path.split('/').at(-1)), ['worker', 'leader', 'worker', 'leader', 'leader'], 'a retry after a partial save sends only the role that failed');
      await page.close();
    }
    // Role prompt extensions: the project's textarea under the role's own text, the preview the server composed, and a refused save says why.
    {
      const type = (selector, value) => page.$eval(selector, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
      const renewed = [{ ...rolePrompts[0], project_extension: { body: 'Project rules, renewed.', rev: 3, updated_at: 'x' }, delivered_body: 'worker instructions\n\nglobal rules\n\nProject rules, renewed.' }, rolePrompts[1]];
      const page = await mount({ auto: { ...base(), 'PUT /api/projects/p1/role-prompts/worker': { body: 'Project rules, renewed.', rev: 3, updated_at: 'x' } } });
      await ready(page);
      await page.waitForFunction(() => document.querySelectorAll('.project-role-prompt').length === 2);
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.project-role-prompt h4')].map(node => node.textContent)), ['Worker', 'Dispatcher']);
      assert.deepEqual(await page.evaluate(() => ['#project-role-extension-worker', '#project-role-extension-dispatcher'].map(selector => document.querySelector(selector).value)), ['project rules', ''], 'each card holds the saved project extension');
      await page.waitForFunction(() => document.querySelector('.project-role-prompt .settings-desc')?.textContent.includes('global extension'));
      assert.equal(await page.$eval('#project-role-prompts details textarea', el => el.readOnly && el.value === 'worker instructions\n\nglobal rules\n\nproject rules'), true, 'the preview is the delivered text, read only');
      assert.equal(await page.$eval('#project-role-extension-worker-save', el => el.disabled), true, 'an unchanged card cannot be saved');
      await page.evaluate(() => { delete window.__auto['GET /api/projects/p1/role-prompts']; });
      await type('#project-role-extension-worker', 'Project rules, renewed.');
      assert.equal(await page.$eval('#project-role-extension-worker-save', el => el.disabled), false);
      await page.$eval('#project-role-extension-worker-save', el => el.click());
      const read = await waitRequest(page, 'GET', '/api/projects/p1/role-prompts');
      await answer(page, read, 200, renewed);
      await page.waitForFunction(() => document.querySelector('#project-role-prompts .settings-desc[role=status]').textContent === 'Saved.');
      assert.deepEqual((await calls(page, 'PUT')).at(-1), ['/api/projects/p1/role-prompts/worker', { body: 'Project rules, renewed.', rev: 2 }]);
      assert.equal(await page.$eval('#project-role-extension-worker', el => el.value), 'Project rules, renewed.', 'the read after the save fills the box from the server');
      // A refused save says why and keeps the draft.
      const refused = rule(409, { message: 'Changed elsewhere' });
      await page.evaluate(refused => { window.__auto['PUT /api/projects/p1/role-prompts/worker'] = refused; }, refused);
      await type('#project-role-extension-worker', 'Conflicting edit.');
      await page.$eval('#project-role-extension-worker-save', el => el.click());
      await page.waitForFunction(() => document.querySelector('#project-role-prompts .settings-desc[role=status]').textContent === 'Changed elsewhere');
      assert.equal(await page.$eval('#project-role-extension-worker', el => el.value), 'Conflicting edit.');
      await page.close();
    }
    // A viewer gets no extension editor and sends no read.
    {
      const page = await mount({ role: 'viewer' });
      await ready(page);
      await page.waitForFunction(() => !!document.querySelector('#project-role-prompts'));
      assert.equal(await page.evaluate(() => [document.querySelectorAll('.project-role-prompt').length, window.__requests.some(request => request.path.includes('/role-prompts'))].join('|')), '0|false', 'a viewer reads no role prompts');
      await page.close();
    }
    // A pending save from another project cannot change this project's rows or status; custom roles use their stored labels.
    {
      const otherRoles = [
        { role_key: 'worker', role_label: 'Worker', inherited: true, global_agents: [], default_agents: [], pick_mode: 'in_turn' },
        { role_key: 'leader', role_label: 'Leader', inherited: true, global_agents: [], default_agents: [], pick_mode: 'in_turn' },
        { role_key: 'quality-gate', role_label: 'Quality gate', inherited: true, global_agents: [], default_agents: [], pick_mode: 'in_turn' },
      ];
      const page = await mount({ auto: { ...base(), 'GET /api/projects/p2/role-spawn-defaults': otherRoles, 'GET /api/projects/p2/settings': { reviewer_agents: 'inherit' } } });
      await ready(page);
      await page.select('.project-role-default:first-child select', 'custom');
      await page.$eval('.project-role-default:first-child [aria-label="Add default agent"]', el => el.click());
      await page.$eval('#project-role-defaults-save', el => el.click());
      const oldSave = await waitRequest(page, 'PUT', '/api/projects/p1/role-spawn-defaults/worker');
      await page.evaluate(() => window.__project.load('p2'));
      await page.waitForFunction(() => document.querySelectorAll('.project-role-default').length === 3 && document.querySelector('.project-role-default:last-child h4').textContent === 'Quality gate');
      await answer(page, oldSave, 200, { inherited: false });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#project-role-defaults [role=status]').textContent, document.querySelector('#project-role-defaults-save').disabled, document.querySelector('.project-role-default:first-child select').value]), ['', true, 'inherit'], 'a save from the previous project cannot alter the new project');
      await page.close();
    }
    // Archive and delete: each asks first; archive reads the project again and lists agents that could not be stopped; delete needs the name typed.
    {
      const live = await mount({ auto: { ...base(), 'DELETE /api/projects/p1': {} } });
      await ready(live);
      assert.equal(await live.$eval('#project-delete', el => el.hidden || el.disabled), false, 'a live project offers Delete directly');
      await live.$eval('#project-delete', el => el.click());
      await live.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.match(await live.$eval('#app-dialog', el => el.textContent), /stops the project's agents.*cannot be undone/s);
      await live.$eval('#app-dialog input', el => { el.value = 'Alpha'; });
      await live.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await live.waitForFunction(() => window.__hooks.some(hook => hook[0] === 'removed'));
      assert.deepEqual((await calls(live, 'DELETE'))[0], ['/api/projects/p1', { name: 'Alpha' }]);
      await live.close();
      const busy = await mount({ auto: { ...base(), 'DELETE /api/projects/p1': rule(409, { message: 'An agent is still stopping.' }) } });
      await ready(busy);
      await busy.$eval('#project-delete', el => el.click());
      await busy.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await busy.$eval('#app-dialog input', el => { el.value = 'Alpha'; });
      await busy.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await busy.waitForFunction(() => document.querySelector('#project-settings-error').textContent.includes('An agent is still stopping.') && window.__hooks.some(hook => hook[0] === 'lifecycle'));
      await busy.close();
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
      assert.deepEqual(await page.evaluate(() => [window.__requests.filter(request => request.method === 'POST' && request.path.endsWith('/archive')).length, window.__hooks.length]), [0, 0], 'a confirmation for p1 sends nothing once p2 is shown');
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
      assert.equal(await page.$eval('#project-scope-control .scope-control-status', el => el.textContent), 'Project agents: Paused');
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
