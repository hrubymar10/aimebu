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
  'GET /api/rooms/r1/agent-name': { name: 'nova' }, 'GET /api/spawn-catalog?machine_id=m1': catalog, 'GET /api/spawn-catalog?machine_id=m2': catalog, 'GET /api/projects/p1/role-spawn-defaults/worker/next': { available: false }, 'GET /api/projects/p1/role-spawn-defaults/lead/next': { available: false },
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
      window.__panel.projectName = () => 'Alpha';
      window.__panel.projectRooms = () => [{ id: 'r1', kind: 'task', lifecycle: 'open', title: 'one', depth: 1 }, { id: 'r2', kind: 'task', lifecycle: 'open', title: 'general', depth: 0 }, { id: 'r3', kind: 'task', lifecycle: 'closed', title: 'old', depth: 1 }, { id: 'r4', kind: 'task', lifecycle: 'closing', title: 'busy', depth: 1 }];
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
  // "Continue a session…" is the item of the ⋯ menu in the title row: open the menu, then press the item.
  const continueSession = async page => { await page.$eval('#spawn-sessions-menu', node => node.click()); await page.waitForSelector('#spawn-sessions', { visible: true }); await page.$eval('#spawn-sessions', node => node.click()); };
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

  // A singleton-writer role finds the room's folder held by another live agent. That only matters when no worktree can be made for the newcomer (a
  // folder that is not a Git repository): then the dialog says so in plain words, and Spawn stays available. With a worktree available, with another role,
  // or with a worktree ticked for the room, there is nothing to say.
  {
    const writerRoles = roles.map(role => role.role_key === 'worker' ? { ...role, cap_singleton_writer: true } : role);
    const hint = page => page.evaluate(() => ({ text: document.querySelector('#spawn-writer-hint').textContent, hidden: document.querySelector('#spawn-writer-hint').hidden, off: document.querySelector('#spawn-form button[type=submit]').disabled }));
    const free = await mount({ auto: stable({ 'GET /api/roles': writerRoles }), withCard: true });
    await open(free);
    await free.evaluate(() => window.__opening); await settle(free);
    assert.equal((await hint(free)).hidden, true, 'a worktree can be made: the newcomer gets one, so nothing blocks and nothing is said');
    await free.close();
    const refused = machines.machines.map(machine => (machine.id === 'm1' ? { ...machine, project_worktree: { ok: false, reason: 'the folder is not a git repository' } } : machine));
    const page = await mount({ auto: stable({ 'GET /api/roles': writerRoles, 'GET /api/machines?project_id=p1': { machines: refused } }), withCard: true });
    await open(page);
    await page.evaluate(() => window.__opening); await settle(page);
    const held = await hint(page);
    assert.deepEqual([held.hidden, held.off, held.text], [false, false, 'ada is already working in this folder, and a separate copy of it cannot be made (the folder is not a git repository), so a new writer here will be refused until ada is stopped.'], 'with no worktree possible a writer role is told, in plain words, that the live writer holds the folder, and can still be tried');
    await select(page, 'role', 'lead');
    assert.equal((await hint(page)).hidden, true, 'another role is not');
    await select(page, 'role', 'worker');
    assert.equal((await hint(page)).hidden, false);
    // The worktree box that cannot be ticked looks disabled; the control height is one for the name, the selects and the dice.
    assert.deepEqual(await page.evaluate(() => { const label = document.querySelector('#spawn-form .spawn-worktree-option'), box = label.querySelector('input'), words = label.querySelector('span'), note = label.querySelector('.spawn-worktree-note');
      const heights = [...document.querySelectorAll('#spawn-form input:not([type=checkbox]):not([type=radio]), #spawn-form select, #spawn-name-dice')].filter(node => node.offsetParent).map(node => Math.round(node.getBoundingClientRect().height));
      return { disabled: box.disabled, box: getComputedStyle(box).opacity, words: getComputedStyle(words).opacity, cursor: getComputedStyle(box).cursor, note: getComputedStyle(note).opacity, oneHeight: new Set(heights).size === 1 && heights.length >= 4 }; }),
      { disabled: true, box: '0.5', words: '0.5', cursor: 'not-allowed', note: '1', oneHeight: true }, 'the disabled worktree box and its words are dimmed (its note is not), and every control is one height');
    await page.close();
    const ticked = await mount({ auto: stable({ 'GET /api/roles': writerRoles }), withCard: true });
    await open(ticked);
    await ticked.evaluate(() => window.__opening); await settle(ticked);
    await ticked.evaluate(() => { const box = document.querySelector('#spawn-form [name=worktree]'); box.checked = true; box.dispatchEvent(new Event('change', { bubbles: true })); });
    assert.equal((await hint(ticked)).hidden, true, 'a worktree for the room is a checkout of its own');
    await ticked.close();
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

  // A v1 agent whose folder another agent writes in: the refusal keeps the dialog open and offers three ways on, each of which sends the request
  // again: a copy in a worktree of its own, stopping the writer, or a role that does not write.
  {
    const writerRoles = roles.map(role => role.role_key === 'worker' ? { ...role, cap_singleton_writer: true } : role);
    const page = await mount({ auto: stable({ 'GET /api/roles': writerRoles }) });
    const resume = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: false, reason: '' };
    await open(page, { resume }); await ready(page);
    const occupied = () => page.evaluate(() => ({ hidden: document.querySelector('#spawn-occupied').hidden, text: document.querySelector('#spawn-occupied p').textContent, stop: document.querySelector('#spawn-occupied-stop').hidden, open: document.querySelector('#spawn-dialog').open }));
    const refuse = async () => {
      await page.$eval('#spawn-form button[type=submit]', node => node.click());
      const sent = await waitRequest(page, 'POST', '/api/rooms/r2/agents');
      await answer(page, sent, 409, { error: 'writer_occupied', message: 'ada@test is the writer of /work/old — stop it, or give this agent a non-writer role', writer_agent_id: 'a1', writer_slug: 'ada', path: '/work/old' });
      await settle(page);
      return sent;
    };
    await refuse();
    assert.deepEqual(await occupied(), { hidden: false, text: 'ada@test is the writer of /work/old — stop it, or give this agent a non-writer role', stop: false, open: true });
    assert.equal(await page.$eval('#spawn-occupied-stop', node => node.textContent), 'Stop ada and start');
    await page.$eval('#spawn-occupied-copy', node => node.click());
    const copy = await waitRequest(page, 'POST', '/api/rooms/r2/agents');
    assert.deepEqual(await body(page, copy), { role_key: 'worker', tool: 'codex', variant: 'native', model: 'gpt-6-sol', launcher_id: 'l1', machine_id: 'm1', resume_legacy: 'old-bot', resume_mode: 'fork', confirm_v1_idle: false, work_on_copy: true });
    await answer(page, copy, 200, agent('a2', 'old-bot')); await settle(page);
    assert.equal((await occupied()).open, false, 'the dialog closes once it started');
    await open(page, { resume }); await ready(page);
    await refuse();
    await page.$eval('#spawn-occupied-reader', node => node.click());
    const reader = await waitRequest(page, 'POST', '/api/rooms/r2/agents');
    assert.equal((await body(page, reader)).role_key, 'lead', 'the role that does not write is sent');
    await page.close();
  }

  // A new agent (no session) whose folder another agent writes in and that cannot get a worktree of its own: the refusal names the holder inline and
  // offers stopping it or a role that does not write, but no copy (that is for sessions), instead of closing the dialog on an error.
  {
    const writerRoles = roles.map(role => role.role_key === 'worker' ? { ...role, cap_singleton_writer: true } : role);
    const page = await mount({ auto: stable({ 'GET /api/roles': writerRoles }) });
    await open(page); await ready(page);
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const sent = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    await answer(page, sent, 409, { error: 'writer_occupied', message: 'ada@test is the writer of /work/old — stop it, or give this agent a non-writer role (it cannot get a worktree of its own: the project\'s worktree mode is off)', writer_agent_id: 'a1', writer_slug: 'ada', path: '/work/old' });
    await settle(page);
    const state = await page.evaluate(() => ({ open: document.querySelector('#spawn-dialog').open, hidden: document.querySelector('#spawn-occupied').hidden, copy: document.querySelector('#spawn-occupied-copy').hidden, stop: document.querySelector('#spawn-occupied-stop').hidden, text: document.querySelector('#spawn-occupied p').textContent }));
    assert.equal(state.open, true, 'the dialog stays open');
    assert.equal(state.hidden, false); assert.equal(state.copy, true, 'no copy for a new agent'); assert.equal(state.stop, false);
    assert.match(state.text, /ada@test is the writer of \/work\/old/);
    await page.$eval('#spawn-occupied-reader', node => node.click());
    const reader = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    assert.equal((await body(page, reader)).role_key, 'lead', 'the role that does not write is sent');
    await page.close();
  }

  // "Stop and start" for a holder in the middle of a turn: the stop only marks its slot as releasing, so the first request meets the writer again.
  // The dialog asks again until the slot is free, and when it never is, says the holder is still stopping instead of offering the same choices.
  {
    const writerRoles = roles.map(role => role.role_key === 'worker' ? { ...role, cap_singleton_writer: true } : role);
    const page = await mount({ auto: stable({ 'GET /api/roles': writerRoles, 'POST /api/agents/a1/control': agent('a1', 'ada') }), withCard: true });
    const resume = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: false, reason: '' };
    await open(page, { resume }); await ready(page);
    const occupiedAnswer = { error: 'writer_occupied', message: 'ada@test is the writer of /work/old', writer_agent_id: 'a1', writer_slug: 'ada', path: '/work/old' };
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/rooms/r2/agents'), 409, occupiedAnswer); await settle(page);
    await page.$eval('#spawn-occupied-stop', node => node.click());
    const first = await waitRequest(page, 'POST', '/api/rooms/r2/agents');
    assert.equal(await count(page, 'POST', '/api/agents/a1/control'), 1, 'the holder is stopped first');
    await answer(page, first, 409, occupiedAnswer);
    const resent = await waitRequest(page, 'POST', '/api/rooms/r2/agents'); // asked again a second later: the holder's slot was still releasing
    assert.notEqual(resent, first);
    await answer(page, resent, 200, agent('a2', 'old-bot')); await settle(page);
    assert.equal(await page.$eval('#spawn-dialog', node => node.open), false, 'the dialog closes once the retry started the agent');
    await page.close();
  }

  // The waiting is the opening's: a dialog closed (and opened for another room) while the holder is still releasing sends nothing more.
  {
    const writerRoles = roles.map(role => role.role_key === 'worker' ? { ...role, cap_singleton_writer: true } : role);
    const page = await mount({ auto: stable({ 'GET /api/roles': writerRoles, 'POST /api/agents/a1/control': agent('a1', 'ada') }), withCard: true });
    const resume = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: false, reason: '' };
    await open(page, { resume }); await ready(page);
    const occupiedAnswer = { error: 'writer_occupied', message: 'ada@test is the writer of /work/old', writer_agent_id: 'a1', writer_slug: 'ada', path: '/work/old' };
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/rooms/r2/agents'), 409, occupiedAnswer); await settle(page);
    await page.$eval('#spawn-occupied-stop', node => node.click());
    await answer(page, await waitRequest(page, 'POST', '/api/rooms/r2/agents'), 409, occupiedAnswer); await settle(page);
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
    assert.equal(await count(page, 'POST', '/api/rooms/r2/agents'), 2, 'a closed dialog does not ask again');
    await page.close();
  }

  // An imported agent is started where its v1 history is: in that room when it is open, and otherwise the dialog says where the history is, and an
  // archived room is offered as "Reopen … and start there", which reopens it (the room menu's call) before the start is sent.
  {
    const base = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: false, reason: '' };
    const startIn = page => page.evaluate(() => { const f = document.querySelector('#spawn-form').elements.start_room; return { value: f.value, options: [...f.options].map(o => o.textContent), notes: document.querySelector('#spawn-resume-notes').textContent }; });
    const openRoom = await mount();
    await open(openRoom, { resume: { ...base, history_room: { id: 'r1', title: 'one', kind: 'task', lifecycle: 'open' } } }); await ready(openRoom);
    let state = await startIn(openRoom);
    assert.equal(state.value, 'r1', 'the open room that holds the history is where the start goes');
    assert.match(state.notes, /Its v1 history is in "Alpha › one"\./);
    await openRoom.close();
    // History in "general" itself: the same rule picks it, and the default room is general either way.
    const inGeneral = await mount();
    await open(inGeneral, { resume: { ...base, history_room: { id: 'r2', title: 'general', kind: 'task', lifecycle: 'open' } } }); await ready(inGeneral);
    state = await startIn(inGeneral);
    assert.equal(state.value, 'r2', 'history in general starts the agent in general');
    assert.match(state.notes, /Its v1 history is in "Alpha › general"\./);
    await inGeneral.close();
    const closing = await mount();
    await open(closing, { resume: { ...base, history_room: { id: 'r4', title: 'busy', kind: 'task', lifecycle: 'closing' } } }); await ready(closing);
    state = await startIn(closing);
    assert.ok(!state.options.some(label => label.startsWith('Reopen')), 'a room that is closing cannot be reopened yet');
    assert.match(state.notes, /changing state/);
    await closing.close();
    const home = await mount();
    await open(home, { resume: { ...base, history_room: { id: 'h1', title: 'Alpha', kind: 'home', lifecycle: 'open' } } }); await ready(home);
    state = await startIn(home);
    assert.equal(state.value, 'r2', 'a home room is not offered: general stays the default');
    assert.match(state.notes, /project's home room, which only a lead role can start in/);
    await home.close();
    const archived = await mount();
    await open(archived, { resume: { ...base, history_room: { id: 'r3', title: 'old', kind: 'task', lifecycle: 'closed' } } }); await ready(archived);
    state = await startIn(archived);
    assert.equal(state.value, 'r2', 'an archived history room is offered, never chosen for the person');
    assert.ok(state.options.includes('Reopen Alpha › old and start there'), `the reopen choice is offered: ${state.options}`);
    assert.match(state.notes, /an archived room: pick "Reopen … and start there"/);
    await archived.evaluate(() => { const f = document.querySelector('#spawn-form').elements.start_room; f.value = 'reopen:r3'; f.dispatchEvent(new Event('change', { bubbles: true })); });
    await archived.$eval('#spawn-resume-confirm', node => node.click()).catch(() => {});
    await archived.$eval('#spawn-form button[type=submit]', node => node.click());
    const reopen = await waitRequest(archived, 'POST', '/api/rooms/r3/reopen');
    assert.deepEqual(await body(archived, reopen), { lifecycle_rev: 0 }, 'the room is reopened first, the way the room menu does it');
    assert.equal(await count(archived, 'POST', '/api/rooms/r3/agents'), 0, 'nothing is started until the room is open');
    await answer(archived, reopen, 200, { id: 'r3', project_id: 'p1', kind: 'task', title: 'old', lifecycle: 'open' });
    const spawn = await waitRequest(archived, 'POST', '/api/rooms/r3/agents');
    assert.equal((await body(archived, spawn)).resume_legacy, 'old-bot');
    // A start that fails after the reopen (a writer holds the folder) leaves the dialog open on the room itself: the way on goes there, with no second
    // reopen (the server would refuse it with the revision of the archived room), and the reopen choice is gone.
    await answer(archived, spawn, 409, { error: 'writer_occupied', message: 'ada@test is the writer of /work/old', writer_agent_id: 'a1', writer_slug: 'ada', path: '/work/old' }); await settle(archived);
    const after = await archived.evaluate(() => { const f = document.querySelector('#spawn-form').elements.start_room; return { value: f.value, options: [...f.options].map(o => o.value) }; });
    assert.deepEqual([after.value, after.options.some(value => value.startsWith('reopen:'))], ['r3', false], 'the choice is the reopened room, and the reopen option is gone');
    await archived.$eval('#spawn-occupied-reader', node => node.click());
    await waitRequest(archived, 'POST', '/api/rooms/r3/agents');
    assert.equal(await count(archived, 'POST', '/api/rooms/r3/reopen'), 1, 'a retry does not reopen the room again');
    await archived.close();
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
    assert.deepEqual([banner.confirm, banner.mode, banner.rooms, banner.room], [true, true, ['Alpha › one', 'Alpha › general'], 'Alpha › general']);
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
    await continueSession(page);
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

  // "Continue a session…" is the one item of a ⋯ menu in the title row: the menu is closed at rest, opens on its item, closes with Escape without closing the
  // dialog (and gives the focus back to its button), and the item opens the session picker.
  {
    const sessions = { sessions: [{ id: '11111111-aaaa-4aaa-8aaa-000000000001', title: 'Fix the login bug', cwd: '/work/project', modified_at: at, size: 2048, git_branch: 'main' }] };
    const page = await mount({ auto: stable({ 'GET /api/projects/p1/claude-sessions?machine_id=m1&launcher_id=l2': sessions }) });
    await open(page); await ready(page);
    await select(page, 'tool', '1');
    const menu = () => page.evaluate(() => ({ expanded: document.querySelector('#spawn-sessions-menu').getAttribute('aria-expanded'), itemShown: !!document.querySelector('#spawn-sessions')?.offsetParent, focus: document.activeElement?.id, dialog: document.querySelector('#spawn-dialog').open, inTitleRow: document.querySelector('#spawn-form .spawn-title-row').contains(document.querySelector('#spawn-sessions-menu')), button: [document.querySelector('#spawn-sessions-menu').getAttribute('aria-label'), document.querySelector('#spawn-sessions-menu').classList.contains('btn-icon'), document.querySelector('#spawn-sessions-menu').getAttribute('aria-haspopup')] }));
    assert.deepEqual(await menu().then(state => ({ ...state, focus: undefined })), { expanded: 'false', itemShown: false, focus: undefined, dialog: true, inTitleRow: true, button: ['More', true, 'menu'] }, 'at rest the title row has the ⋯ button and no entry');
    await page.evaluate(async () => { const { trackInputModality } = await import('/js/input-modality.js'); trackInputModality(); });
    await page.focus('#spawn-sessions-menu'); await page.keyboard.press('Enter');
    await page.waitForFunction(() => document.activeElement?.id === 'spawn-sessions');
    assert.deepEqual(await menu().then(state => ({ expanded: state.expanded, itemShown: state.itemShown, focus: state.focus, dialog: state.dialog })), { expanded: 'true', itemShown: true, focus: 'spawn-sessions', dialog: true }, 'the menu opens on its item');
    assert.equal(await page.$eval('#spawn-sessions', node => node.textContent.trim()), 'Continue a Claude Code session…');
    // The ring on the item is for the keyboard: opened with a click it is focused for the arrow keys but draws no outline.
    const ring = () => page.$eval('#spawn-sessions', node => ({ style: getComputedStyle(node).outlineStyle, input: document.documentElement.dataset.input }));
    assert.equal((await ring()).input, 'keyboard', 'opened with the keyboard: the page knows the last input was a key (the ring is the browser\'s own :focus-visible)');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => document.activeElement?.id === 'spawn-sessions-menu');
    await page.click('#spawn-sessions-menu');
    await page.waitForFunction(() => document.activeElement?.id === 'spawn-sessions');
    assert.deepEqual(await ring(), { style: 'none', input: 'pointer' }, 'opened with a click: focused for the arrow keys, but no ring');
    await page.keyboard.press('ArrowDown');
    assert.equal((await ring()).input, 'keyboard', 'a key press switches back to the keyboard');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => document.activeElement?.id === 'spawn-sessions-menu');
    assert.deepEqual(await menu().then(state => ({ expanded: state.expanded, itemShown: state.itemShown, focus: state.focus, dialog: state.dialog })), { expanded: 'false', itemShown: false, focus: 'spawn-sessions-menu', dialog: true }, 'Escape closes the menu, keeps the dialog and returns the focus');
    // A click elsewhere in the dialog closes it too.
    await page.click('#spawn-sessions-menu');
    await page.waitForFunction(() => document.querySelector('#spawn-sessions-menu').getAttribute('aria-expanded') === 'true');
    await page.click('#spawn-form h2');
    await page.waitForFunction(() => document.querySelector('#spawn-sessions-menu').getAttribute('aria-expanded') === 'false');
    await page.focus('#spawn-sessions-menu'); await page.keyboard.press('Enter');
    await page.waitForFunction(() => document.activeElement?.id === 'spawn-sessions');
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => document.querySelector('#cc-sessions-dialog').open && !document.querySelector('#spawn-dialog').open);
    await page.waitForSelector('#cc-sessions-list .cc-session-pick');
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

  // A Switch that is still out when the room changed does not close, refresh or mark busy the dialog that was opened afterwards for another agent.
  for (const [status, reply] of [[200, agent('a1', 'ada', { model: 'gpt-6-mini', desired_rev: 2 })], [409, { message: 'The agent is busy', error: 'busy' }]]) {
    const page = await mount({ withCard: true });
    await page.waitForSelector('[data-agent-id=a1]');
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
    await page.waitForFunction(() => document.querySelector('#switch-form').elements.model.options.length);
    await settle(page);
    await page.evaluate(() => { const el = document.querySelector('#switch-form').elements.model; el.value = 'gpt-6-mini'; el.dispatchEvent(new Event('change')); });
    await page.$eval('#switch-form button[type=submit]', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/agents/a1/switch');
    await page.evaluate(room => { window.__shown = window.__panel.setRoom(room); }, room('r2'));
    await page.waitForSelector('[data-agent-id=a9]');
    await page.$eval('[data-agent-id=a9] .agent-expand', node => node.click());
    await page.$eval('[data-agent-id=a9] .agent-switch', node => node.click());
    await page.waitForFunction(() => document.querySelector('#switch-form').elements.model.options.length && document.querySelector('#switch-agent-name').textContent.startsWith('zed'));
    await settle(page);
    const view = () => page.evaluate(() => ({ open: document.querySelector('#switch-dialog').open, name: document.querySelector('#switch-agent-name').textContent, error: document.querySelector('#switch-error').textContent,
      submit: document.querySelector('#switch-form button[type=submit]').disabled, cancel: document.querySelector('#switch-cancel').disabled, models: document.querySelector('#switch-form').elements.model.value }));
    const before = await view();
    const reads = await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && request.path.startsWith('/api/rooms/r1/')).length);
    await answer(page, held, status, reply); await settle(page);
    assert.deepEqual(await view(), before, `${status}: the dialog of the other agent is as it was`);
    assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && request.path.startsWith('/api/rooms/r1/')).length), reads, `${status}: the old room is not read again`);
    await page.close();
  }

  // A folder save that is still out when the dialog was opened again, or another machine was picked, neither says "saved" in the new view nor
  // reads the catalog again for it.
  for (const how of ['reopen', 'machine']) {
    const page = await mount();
    await open(page); await ready(page);
    await select(page, 'machine', 'm2');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    await page.$eval('#spawn-form [name=machine_folder]', node => { node.value = '/srv/new'; node.dispatchEvent(new Event('input')); });
    await page.$eval('#spawn-save-folder', node => node.click());
    const held = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    assert.deepEqual(await body(page, held), { folder: '/srv/new' });
    if (how === 'reopen') { await page.evaluate(() => document.querySelector('#spawn-cancel').click()); await open(page); await ready(page); } else await select(page, 'machine', 'm1');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    await settle(page);
    const catalogs = () => page.evaluate(() => window.__requests.filter(request => request.path.startsWith('/api/spawn-catalog')).length);
    const before = await catalogs();
    await answer(page, held, 200, { machine_id: 'm2', folder: '/srv/new' }); await settle(page);
    assert.equal(await page.$eval('#spawn-folder-status', node => node.textContent), '', `${how}: the new view does not say the folder was saved`);
    assert.equal(await catalogs(), before, `${how}: no catalog is read for the save that was left`);
    assert.equal((await form(page)).submit, false);
    await page.close();
  }

  // A machine the project's owner has not admitted yet: an admin, or the project's own owner, gets an "Admit this machine" button, which sends the admit
  // request and reads the machines again; an operator of the project, who may not admit it, gets none.
  for (const { admin, role, shown } of [{ admin: true, role: 'owner', shown: true }, { admin: false, role: 'owner', shown: true }, { admin: false, role: 'operator', shown: false }]) {
    const who = `${admin ? 'an admin' : `a non-admin ${role}`}`;
    const unadmitted = { machines: [machines.machines[0], { ...machines.machines[1], project_allowed: false }] };
    const page = await mount({ admin, auto: stable({ 'GET /api/machines?project_id=p1': unadmitted, 'POST /api/projects/p1/machine-folders/m2/admit': undefined }) });
    await page.evaluate(role => { window.__panel.projectRole = () => role; }, role);
    await open(page); await ready(page);
    await select(page, 'machine', 'm2');
    assert.equal(await page.$eval('#spawn-admit-machine', node => !node.hidden && getComputedStyle(node).display !== 'none'), shown, `${who}: the admit button is ${shown ? 'shown' : 'hidden'}`);
    if (shown && admin) {
      await page.$eval('#spawn-admit-machine', node => node.click());
      const admit = await waitRequest(page, 'POST', '/api/projects/p1/machine-folders/m2/admit');
      const reads = () => page.evaluate(() => window.__requests.filter(request => request.path === '/api/machines?project_id=p1').length);
      const before = await reads();
      await answer(page, admit, 204, undefined); await settle(page);
      assert.equal(await page.$eval('#spawn-folder-status', node => node.textContent), 'Machine admitted for this project.');
      assert.ok(await reads() > before, 'the machines are read again after the admit');
    }
    await page.close();
  }

  // Two saves for the same machine: only the newest speaks and holds the Save button. The older one, answered while the newer is still out,
  // changes no status, reads no catalog and does not enable the button; then the newer one ends normally. A save of a dialog that was opened
  // again does not enable the new dialog's button either.
  {
    const page = await mount({ auto: stable({ 'PUT /api/projects/p1/machine-folders/m2': undefined }) });
    await open(page); await ready(page);
    await select(page, 'machine', 'm2');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    const save = async folder => { await page.$eval('#spawn-form [name=machine_folder]', (node, folder) => { node.value = folder; node.dispatchEvent(new Event('input')); }, folder); await page.evaluate(() => { window.__save = window.__panel.spawnMachine.saveFolder(); }); };
    const catalogs = () => page.evaluate(() => window.__requests.filter(request => request.path.startsWith('/api/spawn-catalog')).length);
    const status = () => page.$eval('#spawn-folder-status', node => node.textContent);
    const disabled = () => page.$eval('#spawn-save-folder', node => node.disabled);
    await save('/srv/a');
    const first = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    await save('/srv/b');
    await page.waitForFunction(() => window.__requests.filter(request => request.method === 'PUT').length === 2);
    const second = await page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'PUT'));
    assert.notEqual(first, second);
    const before = await catalogs(), shown = await status();
    await answer(page, first, 200, { machine_id: 'm2', folder: '/srv/a' }); await settle(page);
    assert.deepEqual([await status(), await catalogs(), await disabled()], [shown, before, true], 'the older save changes nothing while the newer one is out');
    await answer(page, second, 200, { machine_id: 'm2', folder: '/srv/b' }); await settle(page);
    assert.deepEqual([await status(), await disabled()], ['Folder saved.', false]);
    assert.equal(await catalogs(), before + 1, 'the newer save reloads the catalog once');
    assert.equal(await page.evaluate(() => window.__panel.spawnMachine.folders.get('m2')), '/srv/b');
    // A save of the dialog's earlier opening neither says anything nor enables the button of the new one while its own save is out.
    await save('/srv/c');
    const old = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await open(page); await ready(page);
    assert.equal(await disabled(), false, 'a new opening starts with Save available');
    await select(page, 'machine', 'm2');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    await save('/srv/d');
    const fresh = await page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'PUT'));
    assert.notEqual(old, fresh);
    const beforeOld = await status();
    await answer(page, old, 200, { machine_id: 'm2', folder: '/srv/c' }); await settle(page);
    assert.deepEqual([await disabled(), await status()], [true, beforeOld], 'the old opening\'s save leaves the new one\'s button and status alone');
    await answer(page, fresh, 200, { machine_id: 'm2', folder: '/srv/d' }); await settle(page);
    assert.deepEqual([await status(), await disabled()], ['Folder saved.', false]);
    await page.close();
  }

  // A save for one machine, answered after another machine was picked, neither says anything in the new view nor takes over its Save button:
  // the button is available after the pick, stays held by the new machine's own save, and is freed by that save alone.
  {
    const two = { machines: [...machines.machines, { id: 'm3', name: 'Third', approved: true, connected: true, project_allowed: true }] };
    const page = await mount({ auto: stable({ 'GET /api/machines?project_id=p1': two, 'GET /api/projects/p1/machine-folders': { folders: [{ machine_id: 'm2', folder: '/srv/p1' }, { machine_id: 'm3', folder: '/srv/p3' }] },
      'GET /api/spawn-catalog?machine_id=m3': catalog, 'PUT /api/projects/p1/machine-folders/m2': undefined, 'PUT /api/projects/p1/machine-folders/m3': undefined }) });
    await open(page); await ready(page);
    const catalogs = () => page.evaluate(() => window.__requests.filter(request => request.path.startsWith('/api/spawn-catalog')).length);
    const status = () => page.$eval('#spawn-folder-status', node => node.textContent);
    const disabled = () => page.$eval('#spawn-save-folder', node => node.disabled);
    const pick = async id => { await select(page, 'machine', id); await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled); };
    const save = async folder => { await page.$eval('#spawn-form [name=machine_folder]', (node, folder) => { node.value = folder; node.dispatchEvent(new Event('input')); }, folder); await page.$eval('#spawn-save-folder', node => node.click()); };
    await pick('m2'); await save('/srv/a2');
    const first = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    assert.equal(await disabled(), true);
    await pick('m3');
    assert.equal(await disabled(), false, 'picking another machine makes Save available');
    await save('/srv/a3');
    const second = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m3');
    const shown = await status(), before = await catalogs();
    await answer(page, first, 200, { machine_id: 'm2', folder: '/srv/a2' }); await settle(page);
    assert.deepEqual([await disabled(), await status(), await catalogs()], [true, shown, before], 'the save of the machine that was left changes nothing and does not free the button');
    await answer(page, second, 200, { machine_id: 'm3', folder: '/srv/a3' }); await settle(page);
    assert.deepEqual([await disabled(), await status()], [false, 'Folder saved.']);
    // With no save of its own, the new machine's button is available whatever the old save does.
    await pick('m2'); await save('/srv/b2');
    const held = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    await pick('m3');
    await answer(page, held, 200, { machine_id: 'm2', folder: '/srv/b2' }); await settle(page);
    assert.deepEqual([await disabled(), await status()], [false, '']);
    await page.close();
  }

  // The tool catalog of an earlier opening, answered while the new opening is still waiting for its machines, does not fill the new one.
  {
    const page = await mount({ auto: stable({ 'GET /api/spawn-catalog?machine_id=m1': undefined }) });
    await open(page);
    const first = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await page.evaluate(() => { window.__auto['GET /api/machines?project_id=p1'] = null; });
    await open(page);
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines?project_id=p1').length === 2);
    const heldMachines = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/machines?project_id=p1'));
    await answer(page, first, 200, catalog); await settle(page);
    assert.deepEqual([(await form(page)).tools, (await form(page)).submit], [[], true], 'the new opening has no tools from the earlier opening');
    await answer(page, heldMachines, 200, machines);
    const second = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await answer(page, second, 200, catalog); await settle(page);
    assert.deepEqual([(await form(page)).tool, (await form(page)).submit], ['Codex', false], 'the new opening then completes normally');
    await page.close();
  }

  // A role default that picks another machine ends the save of the machine it left: the Save button is available again, and the old save, answered
  // while a save for the new machine is out, changes nothing.
  {
    const two = { machines: [...machines.machines, { id: 'm3', name: 'Third', approved: true, connected: true, project_allowed: true }] };
    const page = await mount({ auto: stable({ 'GET /api/machines?project_id=p1': two, 'GET /api/projects/p1/machine-folders': { folders: [{ machine_id: 'm2', folder: '/srv/p1' }, { machine_id: 'm3', folder: '/srv/p3' }] },
      'GET /api/spawn-catalog?machine_id=m3': catalog, 'GET /api/projects/p1/role-spawn-defaults/lead/next': undefined, 'PUT /api/projects/p1/machine-folders/m2': undefined, 'PUT /api/projects/p1/machine-folders/m3': undefined }) });
    await open(page); await ready(page);
    await select(page, 'machine', 'm2');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    await page.$eval('#spawn-form [name=machine_folder]', node => { node.value = '/srv/a'; node.dispatchEvent(new Event('input')); });
    await page.$eval('#spawn-save-folder', node => node.click());
    const first = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    const disabled = () => page.$eval('#spawn-save-folder', node => node.disabled), status = () => page.$eval('#spawn-folder-status', node => node.textContent);
    assert.equal(await disabled(), true);
    await select(page, 'role', 'lead');
    const preview = await waitRequest(page, 'GET', '/api/projects/p1/role-spawn-defaults/lead/next');
    await answer(page, preview, 200, { available: true, machine_id: 'm3', launcher_id: 'l1', model: 'gpt-6-mini' });
    await page.waitForFunction(() => document.querySelector('#spawn-form [name=machine]').value === 'm3' && !document.querySelector('#spawn-form button[type=submit]').disabled);
    assert.equal(await disabled(), false, 'the role default made another machine the one on show, and its Save is available');
    await page.$eval('#spawn-form [name=machine_folder]', node => { node.value = '/srv/b'; node.dispatchEvent(new Event('input')); });
    await page.$eval('#spawn-save-folder', node => node.click());
    const second = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m3');
    const shown = await status();
    await answer(page, first, 200, { machine_id: 'm2', folder: '/srv/a' }); await settle(page);
    assert.deepEqual([await disabled(), await status()], [true, shown], 'the old save changes nothing while the new one is out');
    await answer(page, second, 200, { machine_id: 'm3', folder: '/srv/b' }); await settle(page);
    assert.deepEqual([await disabled(), await status()], [false, 'Folder saved.']);
    await page.close();
  }

  // A Spawn that is still out when the dialog was cancelled changes nothing when it is answered: nobody is taken anywhere and no refusal is shown.
  for (const [status, reply] of [[200, agent('a2', 'nova')], [409, { message: 'that name is in use', error: 'name_in_use' }], [400, { message: 'refused', error: 'invalid' }]]) {
    const page = await mount({ current: room('r1') });
    await page.evaluate(() => { window.__panel.showRoom = target => window.__calls.push(['showRoom', target.id]); });
    await open(page); await ready(page);
    await page.$eval('#spawn-form button[type=submit]', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click());
    await calls(page);
    const reads = await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && request.path.startsWith('/api/rooms/r1/agents')).length);
    await answer(page, held, status, reply); await settle(page);
    const after = await page.evaluate(() => ({ error: document.querySelector('#agents-error').textContent, nameError: document.querySelector('#spawn-name-error').textContent, open: document.querySelector('#spawn-dialog').open }));
    assert.deepEqual([after, await calls(page)], [{ error: '', nameError: '', open: false }, []], `${status}: a spawn answered after Cancel changes nothing`);
    assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && request.path.startsWith('/api/rooms/r1/agents')).length) <= reads + 1, true, `${status}: at most the source's own refresh`);
    await page.close();
  }

  // A close event that arrives after the dialog was opened again (the browser queues it after close()) does not wipe the start from history that
  // the new opening set up.
  {
    const page = await mount();
    const resume = { slug: 'old-bot', legacy_id: 'old-bot', tool: 'codex', model: 'gpt-6-sol', resumable: true, cwd: '/work/old', v1_active: false, reason: '' };
    await open(page, { resume }); await ready(page);
    await page.evaluate(() => document.querySelector('#spawn-dialog').dispatchEvent(new Event('close')));
    await settle(page);
    const state = await page.evaluate(() => ({ title: document.querySelector('#spawn-form h2').textContent, banner: document.querySelector('#spawn-resume').hidden, rooms: document.querySelector('#spawn-form').elements.start_room.options.length }));
    assert.deepEqual([state.title, state.banner, state.rooms > 0], ['Start old-bot as a v2 agent', false, true], 'the late close event of an earlier opening changes nothing');
    await page.evaluate(() => document.querySelector('#spawn-cancel').click()); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#spawn-form h2').textContent, document.querySelector('#spawn-resume').hidden]), ['Spawn', true], 'a real close still clears it');
    await page.close();
  }

  // The Switch dialog: the tool catalog of an earlier opening, answered while the same agent's dialog was opened again and is still waiting for its
  // machines, does not fill the new opening; and a Switch answered after the dialog was closed changes nothing.
  {
    const page = await mount({ withCard: true, auto: stable({ 'GET /api/spawn-catalog?machine_id=m1': undefined }) });
    await page.waitForSelector('[data-agent-id=a1]');
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const openSwitch = () => page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
    await openSwitch();
    const first = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await page.evaluate(() => document.querySelector('#switch-cancel').click());
    await page.evaluate(() => { window.__auto['GET /api/machines?project_id=p1'] = null; });
    await openSwitch();
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines?project_id=p1').length === 2);
    const machinesHeld = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/machines?project_id=p1'));
    await answer(page, first, 200, catalog); await settle(page);
    const view = () => page.evaluate(() => ({ tools: document.querySelector('#switch-form').elements.tool.options.length, submit: document.querySelector('#switch-form button[type=submit]').disabled, reason: document.querySelector('#switch-tool-reason').textContent }));
    assert.deepEqual([(await view()).tools, (await view()).submit], [0, true], 'the new opening has no tools from the earlier opening');
    await answer(page, machinesHeld, 200, machines);
    await answer(page, await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1'), 200, catalog); await settle(page);
    assert.equal((await view()).tools > 0, true, 'the new opening then completes normally');
    await page.close();
  }

  for (const [status, reply] of [[200, agent('a1', 'ada', { model: 'gpt-6-mini', desired_rev: 2 })], [409, { message: 'The agent is busy', error: 'busy' }]]) {
    const page = await mount({ withCard: true });
    await page.waitForSelector('[data-agent-id=a1]');
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
    await page.waitForFunction(() => document.querySelector('#switch-form').elements.model.options.length);
    await settle(page);
    await page.evaluate(() => { const el = document.querySelector('#switch-form').elements.model; el.value = 'gpt-6-mini'; el.dispatchEvent(new Event('change')); });
    await page.$eval('#switch-form button[type=submit]', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/agents/a1/switch');
    await page.evaluate(() => document.querySelector('#switch-dialog').close());
    const reads = () => page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && request.path === '/api/rooms/r1/agents').length);
    const before = await reads();
    await answer(page, held, status, reply); await settle(page);
    assert.deepEqual([await page.$eval('#switch-error', node => node.textContent), await page.$eval('#switch-dialog', node => node.open)], ['', false], `${status}: a Switch answered after the dialog closed shows nothing`);
    assert.equal(await reads() <= before + 1, true, `${status}: at most the source's own refresh`);
    await page.close();
  }

  // A Switch opening that is still waiting for its folders or its machines when the room changes or the panel is reset does not go on: the dialog is
  // closed, and the answer starts no further read (not the machines after the folders, not the catalog after the machines) and changes nothing.
  for (const how of ['room', 'reset']) {
    for (const [rule, path, answerBody, next] of [['GET /api/projects/p1/machine-folders', '/api/projects/p1/machine-folders', { folders: [] }, '/api/machines'], ['GET /api/machines?project_id=p1', '/api/machines?project_id=p1', machines, '/api/spawn-catalog']]) {
      const page = await mount({ withCard: true, auto: stable({ [rule]: undefined }) });
      await page.waitForSelector('[data-agent-id=a1]');
      await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
      await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
      const held = await waitRequest(page, 'GET', path);
      const before = await page.evaluate(() => window.__requests.length);
      if (how === 'room') await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2')); else await page.evaluate(() => window.__panel.reset());
      const after = await page.evaluate(() => window.__requests.length);
      await answer(page, held, 200, answerBody); await settle(page);
      const asked = await page.evaluate(({ from, prefix }) => window.__requests.slice(from).filter(request => request.path.startsWith(prefix)).length, { from: after, prefix: next });
      assert.equal(asked, 0, `${how}: the Switch opening that was left asks for nothing more (${next})`);
      assert.equal(await page.$eval('#switch-dialog', node => node.open), false, `${how}: the dialog is closed`);
      await page.close();
    }
  }

  // A Spawn opening that is still waiting for its folders or its machines when the room changes or the panel is reset does not go on: the dialog is
  // closed, and the answer starts no further read (not the machines after the folders, not the catalog after the machines) and changes nothing.
  for (const how of ['room', 'reset']) {
    for (const [rule, path, answerBody, next] of [['GET /api/projects/p1/machine-folders', '/api/projects/p1/machine-folders', { folders: [] }, '/api/machines'], ['GET /api/machines?project_id=p1', '/api/machines?project_id=p1', machines, '/api/spawn-catalog']]) {
      const page = await mount({ withCard: true, auto: stable({ [rule]: undefined }) });
      await open(page);
      const held = await waitRequest(page, 'GET', path);
      const before = await page.evaluate(() => window.__requests.length);
      if (how === 'room') await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2')); else await page.evaluate(() => window.__panel.reset());
      const after = await page.evaluate(() => window.__requests.length);
      await answer(page, held, 200, answerBody); await settle(page);
      const asked = await page.evaluate(({ from, prefix }) => window.__requests.slice(from).filter(request => request.path.startsWith(prefix)).length, { from: after, prefix: next });
      assert.equal(asked, 0, `${how}: the Spawn opening that was left asks for nothing more (${next})`);
      assert.equal(await page.$eval('#spawn-dialog', node => node.open), false, `${how}: the dialog is closed`);
      await page.close();
    }
  }

  // A panel destroyed while a read of the Spawn dialog or its call is out takes the answer and does nothing with it: no machines, roles, tools or
  // name are stored, no read follows, no folder status, and a spawn that succeeds or is refused navigates, refreshes and says nothing.
  {
    const state = page => page.evaluate(() => { const store = window.__panel.spawn; return { machines: store.machine.machines.value.length, roles: store.roles.value.length, tools: store.catalog.value.length, name: store.name.value, status: store.machine.status.value, ready: store.rolesReady.value }; });
    const reads = (page, prefix) => page.evaluate(prefix => window.__requests.filter(request => request.path.startsWith(prefix)).length, prefix);
    // The machines of an opening.
    let page = await mount({ auto: stable({ 'GET /api/machines?project_id=p1': undefined }) });
    await open(page);
    let held = await waitRequest(page, 'GET', '/api/machines?project_id=p1');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, machines); await settle(page);
    assert.deepEqual([(await state(page)).machines, await reads(page, '/api/spawn-catalog')], [0, 0], 'a destroyed panel keeps no machine and reads no catalog');
    await page.close();
    // The folders, which the machines wait for.
    page = await mount({ auto: stable({ 'GET /api/projects/p1/machine-folders': undefined }) });
    await open(page);
    held = await waitRequest(page, 'GET', '/api/projects/p1/machine-folders');
    const asked = await reads(page, '/api/machines');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, { folders: [] }); await settle(page);
    assert.equal(await reads(page, '/api/machines'), asked, 'a destroyed panel asks for no machines after the folders');
    await page.close();
    // The roles.
    page = await mount({ auto: stable({ 'GET /api/roles': undefined }) });
    await open(page);
    held = await waitRequest(page, 'GET', '/api/roles');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, roles); await settle(page);
    assert.deepEqual([(await state(page)).roles, (await state(page)).ready], [0, false], 'a destroyed panel keeps no role');
    await page.close();
    // The tool catalog.
    page = await mount({ auto: stable({ 'GET /api/spawn-catalog?machine_id=m1': undefined }) });
    await open(page);
    held = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, catalog); await settle(page);
    assert.equal((await state(page)).tools, 0, 'a destroyed panel keeps no tool');
    await page.close();
    // The suggested name, and the role's default.
    page = await mount({ auto: stable({ 'GET /api/rooms/r1/agent-name': undefined, 'GET /api/projects/p1/role-spawn-defaults/worker/next': undefined }) });
    await open(page);
    const suggestion = await waitRequest(page, 'GET', '/api/rooms/r1/agent-name');
    const preview = await waitRequest(page, 'GET', '/api/projects/p1/role-spawn-defaults/worker/next');
    const before = await page.evaluate(() => window.__requests.length);
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, suggestion, 200, { name: 'late' }); await answer(page, preview, 200, { available: true, machine_id: 'm2', launcher_id: 'l2', model: 'opus' }); await settle(page);
    assert.deepEqual([(await state(page)).name, await page.evaluate(() => window.__requests.length)], ['', before], 'a destroyed panel keeps no name and acts on no default');
    await page.close();
    // A spawn, answered with a success and with a refusal.
    for (const [status, reply] of [[200, agent('a2', 'nova')], [409, { message: 'that name is in use', error: 'name_in_use' }], [400, { message: 'refused', error: 'invalid' }]]) {
      page = await mount();
      await open(page); await ready(page);
      await page.$eval('#spawn-form button[type=submit]', node => node.click());
      held = await waitRequest(page, 'POST', '/api/rooms/r1/agents');
      const total = await page.evaluate(() => window.__requests.length);
      await calls(page);
      await page.evaluate(() => window.__panel.destroy());
      await answer(page, held, status, reply); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__requests.length), await calls(page), await page.$eval('#agents-error', node => node.textContent), await page.evaluate(() => window.__panel.spawn.nameError.value)], [total, [], '', ''], `${status}: a destroyed panel does not navigate, read or say anything`);
      await page.close();
    }
    // The session picker: a session picked after the panel was destroyed opens nothing and asks for nothing.
    {
      const sessions = { sessions: [{ id: '11111111-aaaa-4aaa-8aaa-000000000001', title: 'Fix the login bug', cwd: '/work/project', modified_at: at, size: 2048, git_branch: 'main' }] };
      const picker = await mount({ auto: stable({ 'GET /api/projects/p1/claude-sessions?machine_id=m1&launcher_id=l2': sessions }) });
      await open(picker); await ready(picker);
      await select(picker, 'tool', '1');
      await continueSession(picker);
      await picker.waitForSelector('#cc-sessions-list .cc-session-pick');
      await picker.evaluate(() => window.__panel.destroy());
      const total = await picker.evaluate(() => window.__requests.length);
      await picker.$eval('#cc-sessions-list .cc-session-pick', node => node.click()); await settle(picker);
      assert.deepEqual([await picker.$eval('#spawn-dialog', node => node.open), await picker.evaluate(() => window.__requests.length), await picker.evaluate(() => window.__panel.spawn.resume.value)], [false, total, null], 'a session picked after destroy opens nothing');
      await picker.close();
    }
    // A folder save.
    page = await mount({ auto: stable({ 'PUT /api/projects/p1/machine-folders/m2': undefined }) });
    await open(page); await ready(page);
    await select(page, 'machine', 'm2');
    await page.waitForFunction(() => !document.querySelector('#spawn-form button[type=submit]').disabled);
    await page.$eval('#spawn-form [name=machine_folder]', node => { node.value = '/srv/new'; node.dispatchEvent(new Event('input')); });
    await page.$eval('#spawn-save-folder', node => node.click());
    held = await waitRequest(page, 'PUT', '/api/projects/p1/machine-folders/m2');
    const catalogs = await reads(page, '/api/spawn-catalog'), status = (await state(page)).status;
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, { machine_id: 'm2', folder: '/srv/new' }); await settle(page);
    assert.deepEqual([(await state(page)).status, await reads(page, '/api/spawn-catalog'), await page.evaluate(() => window.__panel.spawn.machine.folders.get('m2'))], [status, catalogs, '/srv/p1'], 'a destroyed panel keeps no saved folder and reads no catalog');
    await page.close();
  }

  // A panel destroyed while a read of the Switch dialog or its call is out takes the answer and does nothing with it.
  {
    const open = async (auto, destroy = true) => {
      const page = await mount({ withCard: true, auto: stable(auto) });
      await page.waitForSelector('[data-agent-id=a1]');
      await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
      await page.$eval('[data-agent-id=a1] .agent-switch', node => node.click());
      return page;
    };
    const state = page => page.evaluate(() => { const store = window.__panel.switch; return { machines: store.machine.machines.value.length, tools: store.catalog.value.length, error: store.error.value, sending: store.sending.value }; });
    const reads = (page, prefix) => page.evaluate(prefix => window.__requests.filter(request => request.path.startsWith(prefix)).length, prefix);
    let page = await open({ 'GET /api/projects/p1/machine-folders': undefined });
    let held = await waitRequest(page, 'GET', '/api/projects/p1/machine-folders');
    const asked = await reads(page, '/api/machines');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, { folders: [] }); await settle(page);
    assert.equal(await reads(page, '/api/machines'), asked, 'a destroyed panel asks for no machines after the folders');
    await page.close();
    page = await open({ 'GET /api/machines?project_id=p1': undefined });
    held = await waitRequest(page, 'GET', '/api/machines?project_id=p1');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, machines); await settle(page);
    assert.deepEqual([(await state(page)).machines, await reads(page, '/api/spawn-catalog')], [0, 0], 'a destroyed panel keeps no machine and reads no catalog');
    await page.close();
    // A refusal of the machines read says so in the dialog, and says nothing once the panel is gone.
    page = await open({ 'GET /api/machines?project_id=p1': undefined });
    held = await waitRequest(page, 'GET', '/api/machines?project_id=p1');
    await answer(page, held, 500, { message: 'The machines are down' }); await settle(page);
    assert.equal(await page.$eval('#switch-tool-reason', node => node.textContent), 'Machines unavailable: The machines are down');
    await page.close();
    page = await open({ 'GET /api/machines?project_id=p1': undefined });
    held = await waitRequest(page, 'GET', '/api/machines?project_id=p1');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 500, { message: 'The machines are down' }); await settle(page);
    assert.equal(await page.evaluate(() => window.__panel.switch.override.value), 'Loading available tools…', 'a destroyed panel says nothing of a refusal');
    await page.close();
    page = await open({ 'GET /api/spawn-catalog?machine_id=m1': undefined });
    held = await waitRequest(page, 'GET', '/api/spawn-catalog?machine_id=m1');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, held, 200, catalog); await settle(page);
    assert.equal((await state(page)).tools, 0, 'a destroyed panel keeps no tool');
    await page.close();
    for (const [status, reply] of [[200, agent('a1', 'ada', { model: 'gpt-6-mini', desired_rev: 2 })], [409, { message: 'The agent is busy', error: 'busy' }]]) {
      page = await open({});
      await page.waitForFunction(() => document.querySelector('#switch-form').elements.model.options.length);
      await settle(page);
      await page.evaluate(() => { const el = document.querySelector('#switch-form').elements.model; el.value = 'gpt-6-mini'; el.dispatchEvent(new Event('change')); });
      await page.$eval('#switch-form button[type=submit]', node => node.click());
      held = await waitRequest(page, 'POST', '/api/agents/a1/switch');
      const total = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => window.__panel.destroy());
      await answer(page, held, status, reply); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__requests.length), (await state(page)).error], [total, ''], `${status}: a destroyed panel reads nothing and shows nothing`);
      await page.close();
    }
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('spawn page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
