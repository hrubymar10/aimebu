// The Spawn and Switch dialogs of the Agents panel in Chromium with every request held until the test answers it or answered by rule. web/ is
// served as is with its import map and markup, without app.js; no Go server. It runs unchanged against the pages that replaced the hand-written
// dialogs. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const PANEL = fs.existsSync(path.join(root, 'js', 'pages', 'agents', 'page.js')) ? '/js/pages/agents/page.js' : '/js/agents.js';
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const at = '2026-01-01T10:00:00Z';
const agent = (id, slug, patch = {}) => ({ id, room_id: 'r1', slug, role_key: 'worker', tool: 'codex', variant: 'native', model: 'gpt-6-sol', desired: 'active', observed: 'idle', desired_rev: 1, generation: 1,
  health_flags: [], last_activity_at: at, machine_id: 'm1', ...patch });
const catalog = { tools: [
  { tool: 'codex', variant: 'native', label: 'Codex', launcher_id: 'l1', available: true, cli_version: '0.9', models: [{ id: 'gpt-6-sol', label: 'GPT-6 Sol', default: true }, { id: 'gpt-6-mini', label: 'GPT-6 Mini' }] },
  { tool: 'claude', variant: 'native', label: 'Claude Code', launcher_id: 'l2', available: true, models: [{ id: 'opus', label: 'Opus' }, { id: 'sonnet', label: 'Sonnet', default: true, note: 'Everyday' }] },
  { tool: 'pi', variant: 'native', label: 'Pi', launcher_id: 'l3', available: false, unavailable_reason: 'not installed', models: [] }] };
const machines = { machines: [{ id: 'm1', name: 'Host', is_host: true, approved: true, connected: true, project_allowed: true }, { id: 'm2', name: 'Remote', approved: true, connected: true, project_allowed: true }] };
const roles = [{ role_key: 'worker', label: 'Worker', emoji: '🔧', cap_is_lead: false }, { role_key: 'lead', label: 'Lead', emoji: '🧭', cap_is_lead: true }];
const stable = (extra = {}) => ({ 'GET /api/projects/p1/machine-folders': { folders: [{ machine_id: 'm2', folder: '/srv/p1' }] }, 'GET /api/machines?project_id=p1': machines, 'GET /api/roles': roles,
  'GET /api/rooms/r1/agent-name': { name: 'nova' }, 'GET /api/spawn-catalog?machine_id=m1': catalog, 'GET /api/projects/p1/role-spawn-defaults/worker/next': { available: false }, 'GET /api/projects/p1/role-spawn-defaults/lead/next': { available: false },
  'GET /api/rooms/r1/agents': [agent('a1', 'ada')], 'GET /api/rooms/r1/members': { humans: [] },
  'GET /api/rooms/r2/agent-name': { name: 'luna' }, 'GET /api/rooms/r2/agents': [agent('a9', 'zed', { room_id: 'r2' })], 'GET /api/rooms/r2/members': { humans: [] }, ...extra });

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
  const room = (id = 'r1', patch = {}) => ({ id, project_id: 'p1', kind: 'task', title: id, lifecycle: 'open', ...patch });
  const mount = async ({ auto = stable(), admin = true, current = room(), withCard = false } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; });
    await page.evaluate(async ({ module, admin, current, withCard }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner: admin, admin });
      const { AgentsPanel } = await import(module);
      const { HttpAgentSource } = await import('/js/agent-source.js');
      window.__calls = [];
      window.__panel = new AgentsPanel(new HttpAgentSource(), () => {}, () => {}, () => {}, () => {});
      window.__panel.projectRole = () => 'owner';
      window.__panel.showRoom = target => { window.__calls.push(['showRoom', target.id]); };
      window.__panel.showRoomByID = async (project, id) => { window.__calls.push(['showRoomByID', project, id]); };
      window.__panel.projectRooms = () => [{ id: 'r1', kind: 'task', lifecycle: 'open', title: 'one', depth: 1 }, { id: 'r2', kind: 'task', lifecycle: 'open', title: 'general', depth: 0 }, { id: 'r3', kind: 'task', lifecycle: 'archived', title: 'old', depth: 1 }];
      if (withCard) await window.__panel.setRoom(current); else window.__panel.setRoomFacts(current);
    }, { module: PANEL, admin, current, withCard });
    return page;
  };
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findLastIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const count = (page, method, requestPath) => page.evaluate((method, requestPath) => window.__requests.filter(request => request.method === method && request.path === requestPath).length, method, requestPath);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const calls = page => page.evaluate(() => window.__calls.splice(0));
  const body = (page, index) => page.evaluate(index => window.__requests[index].body, index);
  const open = async (page, options) => { await page.evaluate(options => { window.__opening = window.__panel.openSpawn(options); }, options); };
  const ready = async page => { await page.evaluate(() => window.__opening); await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled); await settle(page); };
  const form = page => page.evaluate(() => { const f = document.querySelector('#spawn-form'), tool = f.elements.tool, model = f.elements.model;
    return { title: f.querySelector('h2').textContent, roles: [...f.elements.role.options].map(o => o.textContent), role: f.elements.role.value, tools: [...tool.options].map(o => [o.textContent, o.disabled]), tool: tool.selectedOptions[0]?.textContent || '',
      models: [...model.options].map(o => [o.value, o.textContent, o.title]), model: model.value, reason: document.querySelector('#spawn-tool-reason').textContent, version: document.querySelector('#spawn-version').textContent,
      submit: f.querySelector('button[type=submit]').disabled, name: f.elements.name.value, nameError: document.querySelector('#spawn-name-error').textContent, nameHidden: f.querySelector('.spawn-name').hidden,
      machine: f.elements.machine.value, error: document.querySelector('#agents-error').textContent, hint: !document.querySelector('#spawn-home-hint').hidden }; });
  const select = (page, name, value) => page.evaluate((name, value) => { const el = document.querySelector('#spawn-form').elements[name]; el.value = value; el.dispatchEvent(new Event('change')); }, name, value);

  // The dialog opens on a free name, the roles, the first machine's tools with their versions and the default model, and Start stays off until
  // those have arrived; Start sends what the dialog shows and the dialog closes and the list is read again.
  {
    const page = await mount({ auto: stable({ 'GET /api/rooms/r1/agent-name': undefined, 'GET /api/spawn-catalog?machine_id=m1': undefined }) });
    await open(page);
    const name = await waitRequest(page, 'GET', '/api/rooms/r1/agent-name');
    const held = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    const loading = await form(page);
    assert.deepEqual([loading.submit, loading.reason, await page.$eval('#spawn-dialog', node => node.open)], [true, 'Loading available tools…', true]);
    await answer(page, held, 200, catalog);
    await answer(page, name, 200, { name: 'nova' });
    await ready(page);
    const state = await form(page);
    assert.deepEqual(state.roles, ['🔧 Worker', '🧭 Lead']); assert.equal(state.role, 'worker');
    assert.deepEqual(state.tools, [['Codex', false], ['Claude Code', false], ['Pi — not installed; check Settings → Harnesses', true]]);
    assert.deepEqual([state.tool, state.model, state.version, state.name, state.submit, state.machine], ['Codex', 'gpt-6-sol', 'CLI version 0.9', 'nova', false, 'm1']);
    assert.deepEqual(state.models, [['gpt-6-sol', 'GPT-6 Sol', ''], ['gpt-6-mini', 'GPT-6 Mini', '']]);
    assert.equal(await page.$eval('#spawn-form .spawn-worktree-option', node => node.hidden), false, 'a task room offers a worktree');
    await select(page, 'tool', '1');
    const claude = await form(page);
    assert.deepEqual([claude.tool, claude.model, claude.version], ['Claude Code', 'sonnet', '']); assert.equal(claude.models[1][2], 'Everyday');
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const spawn = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    assert.deepEqual(await body(page, spawn), { role_key: 'worker', tool: 'claude', variant: 'native', model: 'sonnet', name: 'nova', launcher_id: 'l2', machine_id: 'm1' });
    await answer(page, spawn, 200, agent('a2', 'nova', { tool: 'claude', model: 'sonnet' }));
    await page.waitForFunction(() => !document.querySelector('#spawn-dialog').open);
    await settle(page);
    assert.equal(await count(page, 'GET', '/api/rooms/r1/agents') >= 1, true, 'the list is read again after a spawn');
    await page.close();
  }

  // A name the server refuses is answered under the field and the dialog stays open; another failure closes the dialog and is shown in the
  // panel; a name that cannot be valid is refused before anything is sent.
  {
    const page = await mount();
    await open(page); await ready(page);
    await page.$eval('#spawn-form [name=name]', node => { node.value = 'A!'; });
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    assert.match((await form(page)).nameError, /^A name is 3 to 21 characters/);
    assert.equal(await count(page, 'POST', '/api/rooms/r1/agents'), 0);
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('#spawn-form [name=name]')), true);
    await page.$eval('#spawn-form [name=name]', node => { node.value = 'taken'; node.dispatchEvent(new Event('input')); });
    assert.equal((await form(page)).nameError, '', 'typing clears the error');
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/rooms/r1/agents'), 409, { message: 'that name is in use', error: 'name_in_use' }); await settle(page);
    const taken = await form(page);
    assert.deepEqual([taken.nameError, await page.$eval('#spawn-dialog', node => node.open), taken.name], ['that name is in use', true, 'taken']);
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/rooms/r1/agents'), 400, { message: 'The role needs a harness and a model', error: 'invalid' }); await settle(page);
    assert.deepEqual([(await form(page)).error, await page.$eval('#spawn-dialog', node => node.open)], ['The role needs a harness and a model', false]);
    await page.close();
  }

  // The home room offers the lead roles alone, with its hint, unless the project turned the rule off; a start from v1 history is not under it.
  {
    const page = await mount({ current: room('r1', { kind: 'home' }), auto: stable({ 'GET /api/projects/p1/settings': { home_lead_only: true } }) });
    await open(page); await ready(page);
    const lead = await form(page);
    assert.deepEqual([lead.roles, lead.hint], [['🧭 Lead'], true]);
    await page.evaluate(() => document.querySelector('#spawn-dialog').close());
    const free = await mount({ current: room('r1', { kind: 'home' }), auto: stable({ 'GET /api/projects/p1/settings': { home_lead_only: false } }) });
    await open(free); await ready(free);
    assert.deepEqual([(await form(free)).roles, (await form(free)).hint], [['🔧 Worker', '🧭 Lead'], false]);
    await free.close(); await page.close();
  }

  // Answers meant for an earlier opening fill nothing: the roles of a dialog that was closed and opened again, a name that arrives after the
  // person typed, and the dice that is answered after the dialog closed.
  {
    const page = await mount({ auto: stable({ 'GET /api/roles': undefined }) });
    await open(page);
    const rolesFirst = await waitRequest(page, 'GET', '/api/roles');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await open(page);
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/roles').length === 2);
    const rolesSecond = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/roles'));
    assert.notEqual(rolesFirst, rolesSecond);
    await answer(page, rolesSecond, 200, roles);
    await answer(page, rolesFirst, 200, [{ role_key: 'old', label: 'Old role', cap_is_lead: false }]);
    await ready(page);
    assert.deepEqual((await form(page)).roles, ['🔧 Worker', '🧭 Lead'], 'the first opening answered late and filled nothing');
    await page.close();
  }
  // The machines of an earlier opening, answered after the dialog was closed and opened again, do not replace the list of the new one.
  {
    const page = await mount({ auto: stable({ 'GET /api/machines?project_id=p1': undefined }) });
    await open(page);
    const first = await waitRequest(page, 'GET', '/api/machines?project_id=p1');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await open(page);
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines?project_id=p1').length === 2);
    const second = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/machines?project_id=p1'));
    await answer(page, second, 200, machines);
    await answer(page, first, 200, { machines: [{ id: 'm9', name: 'Old', is_host: true, approved: true, connected: true, project_allowed: true }] });
    await ready(page);
    assert.deepEqual(await page.$eval('#spawn-form [name=machine]', node => [...node.options].map(option => option.value)), ['m1', 'm2'], 'the first opening answered late and replaced nothing');
    assert.equal((await form(page)).machine, 'm1');
    await page.close();
  }

  {
    const page = await mount({ auto: stable({ 'GET /api/rooms/r1/agent-name': undefined, 'GET /api/rooms/r1/agent-name?exclude=': undefined }) });
    await open(page); await ready(page);
    const suggested = await waitRequest(page, 'GET', '/api/rooms/r1/agent-name');
    await page.$eval('#spawn-form [name=name]', node => { node.value = 'mine'; node.dispatchEvent(new Event('input')); });
    await answer(page, suggested, 200, { name: 'late' }); await settle(page);
    assert.equal((await form(page)).name, 'mine', 'a suggestion that arrives after typing is dropped');
    await page.$eval('#spawn-name-dice', node => node.click());
    assert.equal(await page.$eval('#spawn-name-dice', node => node.disabled), true, 'the dice waits for its answer');
    const dice = await waitRequest(page, 'GET', '/api/rooms/r1/agent-name?exclude=mine');
    await answer(page, dice, 200, { name: 'fresh' }); await settle(page);
    assert.deepEqual([(await form(page)).name, await page.$eval('#spawn-name-dice', node => node.disabled)], ['fresh', false]);
    await page.$eval('#spawn-name-dice', node => node.click());
    const closing = await waitRequest(page, 'GET', '/api/rooms/r1/agent-name?exclude=fresh');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await answer(page, closing, 200, { name: 'after-close' }); await settle(page);
    assert.notEqual((await form(page)).name, 'after-close');
    await page.close();
  }

  // The tool the person picked is kept when the machine is changed and the catalog reloads, and the answer for the machine that was left is
  // dropped.
  {
    const page = await mount({ auto: stable({ 'GET /api/spawn-catalog?machine_id=m2': undefined }) });
    await open(page); await ready(page);
    await select(page, 'tool', '1');
    await select(page, 'model', 'opus');
    await select(page, 'machine', 'm2');
    const remote = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m2');
    assert.deepEqual([(await form(page)).submit, (await form(page)).reason], [true, 'Loading available tools…']);
    await answer(page, remote, 200, catalog); await settle(page);
    const state = await form(page);
    assert.deepEqual([state.tool, state.model, state.submit], ['Claude Code', 'opus', false], 'the person\'s tool and model survive a reload');
    await page.close();
  }

  // A task room can ask for a git worktree: the option and its base ref follow the box, the machine can refuse it with its reason, and the
  // request carries the choice only from a task room.
  {
    const refused = machines.machines.map(machine => (machine.id === 'm1' ? { ...machine, project_worktree: { ok: false, reason: 'the folder is not a git repository' } } : machine));
    const page = await mount({ auto: stable({ 'GET /api/machines?project_id=p1': { machines: refused } }) });
    await open(page); await ready(page);
    const option = () => page.evaluate(() => { const f = document.querySelector('#spawn-form'); return { hidden: f.querySelector('.spawn-worktree-option').hidden, disabled: f.elements.worktree.disabled, note: f.querySelector('.spawn-worktree-note').textContent, base: f.querySelector('.spawn-worktree-base').hidden }; });
    assert.deepEqual(await option(), { hidden: false, disabled: true, note: 'No worktree here: the folder is not a git repository', base: true });
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    const page2 = await mount();
    await open(page2); await ready(page2);
    await page2.$eval('#spawn-form [name=worktree]', node => node.click());
    assert.equal(await page2.$eval('#spawn-form .spawn-worktree-base', node => node.hidden), false);
    await page2.$eval('#spawn-form [name=base_ref]', node => { node.value = 'main'; });
    await page2.$eval('#spawn-form button[type=submit]', node => node.click());
    const spawn = await waitRequest(page2, 'POST', '/api/rooms/r1/agents');
    const sent = await body(page2, spawn);
    assert.deepEqual([sent.worktree, sent.base_ref], [true, 'main']);
    await answer(page2, spawn, 400, { message: 'refused', error: 'invalid' }); await settle(page2);
    await open(page2); await ready(page2);
    assert.deepEqual([await page2.$eval('#spawn-form [name=worktree]', node => node.checked), await page2.$eval('#spawn-form [name=base_ref]', node => node.value)], [false, ''], 'each opening starts without a worktree');
    await page2.close();
    const home = await mount({ current: room('r1', { kind: 'home' }), auto: stable({ 'GET /api/projects/p1/settings': { home_lead_only: false } }) });
    await open(home); await ready(home);
    assert.equal(await home.$eval('#spawn-form .spawn-worktree-option', node => node.hidden), true, 'only a task room offers it');
    await home.close(); await page.close();
  }

  // Starting a v1 agent: the dialog says what will happen, offers the rooms of the project, has no name field, and will not send until the
  // person confirms that v1 is not using the session.
  {
    const page = await mount();
    const resume = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: true, outside_folder: true, reason: '' };
    await open(page, { resume }); await ready(page);
    const state = await form(page);
    assert.deepEqual([state.title, state.nameHidden, state.tool, state.model], ['Start old-bot as a v2 agent', true, 'Codex', 'gpt-6-sol']);
    const banner = await page.evaluate(() => ({ text: document.querySelector('#spawn-resume-text').textContent, notes: document.querySelector('#spawn-resume-notes').textContent, confirm: !document.querySelector('#spawn-resume-confirm-label').hidden,
      mode: !document.querySelector('#spawn-resume-mode').hidden, rooms: [...document.querySelector('#spawn-form').elements.start_room.options].map(o => o.textContent), room: document.querySelector('#spawn-form').elements.start_room.selectedOptions[0].textContent }));
    assert.equal(banner.text, 'This continues the v1 session of old-bot, working in /work/old.');
    assert.match(banner.notes, /outside the project folder.*v1 saw this agent a moment ago/);
    assert.deepEqual([banner.confirm, banner.mode, banner.rooms, banner.room], [true, true, ['one', 'general'], 'general']);
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    assert.equal(await page.$eval('#spawn-resume-notes', node => node.textContent), 'Confirm that v1 is not running this session any more.');
    assert.equal(await count(page, 'POST', '/api/rooms/r2/agents'), 0);
    await page.$eval('#spawn-resume-confirm', node => node.click());
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const spawn = await waitRequest(page, 'POST', '/api/rooms/r2/agents');
    assert.deepEqual(await body(page, spawn), { role_key: 'worker', tool: 'codex', variant: 'native', model: 'gpt-6-sol', launcher_id: 'l1', machine_id: 'm1', resume_legacy: 'old-bot', resume_mode: 'continue', confirm_v1_idle: true });
    await answer(page, spawn, 200, agent('a2', 'old-bot')); await settle(page);
    assert.deepEqual(await calls(page), [['showRoom', 'r2']], 'the person is taken to the room it started in');
    // A copy needs no confirmation.
    await open(page, { resume }); await ready(page);
    await page.$eval('#spawn-resume-mode input[value=fork]', node => node.click());
    assert.equal(await page.$eval('#spawn-resume-confirm-label', node => node.hidden), true);
    assert.equal((await form(page)).title, 'Start old-bot on a copy of its v1 session');
    await page.close();
  }

  // The session picker closes the dialog, lists the sessions of the machine, and a pick opens the dialog again for that session.
  {
    const sessions = { sessions: [{ id: '11111111-aaaa-4aaa-8aaa-000000000001', title: 'Fix the login bug', cwd: '/work/project', modified_at: at, size: 2048, git_branch: 'main' }] };
    const page = await mount({ auto: stable({ 'GET /api/projects/p1/claude-sessions?machine_id=m1&launcher_id=l2': sessions }) });
    await open(page); await ready(page);
    await select(page, 'tool', '1');
    await page.$eval('#spawn-sessions', node => node.click());
    await page.waitForFunction(() => document.querySelector('#cc-sessions-dialog').open);
    assert.equal(await page.$eval('#spawn-dialog', node => node.open), false);
    await page.waitForSelector('#cc-sessions-list .cc-session-pick');
    await page.$eval('#cc-sessions-list .cc-session-pick', node => node.click());
    await page.waitForFunction(() => document.querySelector('#spawn-dialog').open && document.querySelector('#spawn-form h2').textContent === 'Continue a Claude Code session');
    await ready(page);
    assert.equal(await page.$eval('#spawn-resume-card', node => node.textContent.includes('11111111')), true);
    assert.equal((await form(page)).nameHidden, false, 'a continued session is started under a name');
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const spawn = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    const sent = await body(page, spawn);
    assert.deepEqual([sent.resume_session, sent.resume_mode, sent.new_room, sent.tool], ['11111111-aaaa-4aaa-8aaa-000000000001', 'continue', true, 'claude']);
    await page.close();
  }

  // The Switch dialog opens on the agent's own tool and model, says so while nothing differs, sends what is picked with the card's revision and
  // the room, closes on success and shows a refusal; it is closed by a change of room, and an answer that arrives after that fills nothing.
  {
    const page = await mount({ withCard: true, auto: stable({ 'GET /api/rooms/r1/agents': [agent('a1', 'ada', { desired_rev: 7 })] }) });
    await page.waitForSelector('[data-agent-id=a1]');
    const sw = () => page.evaluate(() => { const f = document.querySelector('#switch-form'); return { open: document.querySelector('#switch-dialog').open, name: document.querySelector('#switch-agent-name').textContent,
      tool: f.elements.tool.selectedOptions[0]?.textContent || '', model: f.elements.model.value, models: [...f.elements.model.options].map(o => o.value), reason: document.querySelector('#switch-tool-reason').textContent, version: document.querySelector('#switch-version').textContent,
      submit: f.querySelector('button[type=submit]').disabled, error: document.querySelector('#switch-error').textContent, cancel: document.querySelector('#switch-cancel').disabled }; });
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
    await page.waitForFunction(() => document.querySelector('#switch-dialog').open);
    assert.deepEqual([(await sw()).submit, (await sw()).name], [true, 'ada · codex · gpt-6-sol']);
    await settle(page);
    await page.waitForFunction(() => document.querySelector('#switch-form').elements.tool.options.length);
    await settle(page);
    const same = await sw();
    assert.deepEqual([same.tool, same.model, same.version, same.submit, same.reason], ['Codex', 'gpt-6-sol', 'CLI version 0.9', true, 'Choose a different model or tool.']);
    await page.evaluate(() => { const el = document.querySelector('#switch-form').elements.model; el.value = 'gpt-6-mini'; el.dispatchEvent(new Event('change')); });
    assert.deepEqual([(await sw()).submit, (await sw()).reason], [false, '']);
    await page.$eval('#switch-form button[type=submit]', node => node.click());
    const refused = await waitRequest(page, 'POST', '/api/agents/a1/switch');
    assert.deepEqual([(await sw()).submit, (await sw()).cancel], [true, true], 'the buttons are off while it is sent');
    assert.deepEqual(await body(page, refused), { room_id: 'r1', tool: 'codex', variant: 'native', launcher_id: 'l1', model: 'gpt-6-mini', machine_id: 'm1', rev: 7 });
    await answer(page, refused, 409, { message: 'The agent is busy and cannot be switched', error: 'busy' }); await settle(page);
    assert.deepEqual([(await sw()).error, (await sw()).open, (await sw()).submit, (await sw()).cancel], ['The agent is busy and cannot be switched', true, false, false]);
    await page.$eval('#switch-form button[type=submit]', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/agents/a1/switch'), 200, agent('a1', 'ada', { model: 'gpt-6-mini', desired_rev: 8 })); await settle(page);
    assert.equal((await sw()).open, false);
    // A room change closes it; the catalog of the earlier opening is dropped.
    await page.evaluate(() => { window.__auto['GET /api/spawn-catalog?machine_id=m1'] = null; });
    await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
    const held = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2'));
    assert.equal((await sw()).open, false, 'a change of room closes the dialog');
    await answer(page, held, 200, catalog); await settle(page);
    assert.deepEqual([(await sw()).tool, (await sw()).submit], ['', true], 'the late catalog fills nothing');
    await page.close();
  }

  // A Spawn that is still out when the dialog was closed and opened again for another room, or the panel was reset, changes nothing of the new
  // view: the new dialog stays open and keeps its name, nothing is shown in the panel, nobody is taken to the room of the old call.
  for (const [how, status, reply] of [['room', 200, agent('a2', 'nova')], ['reset', 200, agent('a2', 'nova')], ['room', 400, { message: 'The role needs a harness', error: 'invalid' }], ['room', 409, { message: 'that name is in use', error: 'name_in_use' }], ['reset', 400, { message: 'refused', error: 'invalid' }]]) {
    const page = await mount();
    await open(page); await ready(page);
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    if (how === 'room') {
      await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2'));
      await open(page); await ready(page);
    } else await page.evaluate(() => window.__panel.reset());
    const reads = () => page.evaluate(() => window.__requests.filter(request => request.method === 'GET').map(request => request.path));
    const before = (await reads()).length;
    await calls(page);
    await answer(page, held, status, reply); await settle(page);
    const after = await page.evaluate(() => ({ open: document.querySelector('#spawn-dialog').open, name: document.querySelector('#spawn-form [name=name]').value, nameError: document.querySelector('#spawn-name-error').textContent, error: document.querySelector('#agents-error').textContent }));
    assert.deepEqual(after, { open: how === 'room', name: how === 'room' ? 'luna' : after.name, nameError: '', error: '' }, `${how} ${status}: the old call does not touch the new view`);
    assert.deepEqual(await calls(page), [], `${how} ${status}: nobody is taken anywhere`);
    assert.equal((await reads()).slice(before).some(path => path.startsWith('/api/rooms/r1/')), false, `${how} ${status}: the room that was left is not read again`);
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('spawn page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
