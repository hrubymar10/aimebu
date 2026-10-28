// The room list (rows, tails, folds, filter, collapse, unread totals, live refreshes) in Chromium with every request held until the test
// answers it or answered by rule. web/ is served as is with its import map and markup, without app.js; no Go server. It runs unchanged against
// the page that replaced the hand-written list. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const REPLACED = fs.existsSync(path.join(root, 'js', 'pages', 'sidebar', 'page.js'));
const MODULE = REPLACED ? '/js/pages/sidebar/page.js' : '/js/sidebar.js';
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const at = '2026-01-01T10:00:00Z', future = '2099-01-01T00:00:00Z';
const project = (id, name, patch = {}) => ({ id, name, role: 'owner', archived_at: '', ...patch });
const room = (id, projectID, title, patch = {}) => ({ id, project_id: projectID, title, kind: 'task', parent_id: '', lifecycle: 'open', unread_count: 0, hidden: false, notify: 'default', muted_until: '', member_count: 2, last_activity_at: at, agent_chat: false, lifecycle_rev: 1, ...patch });
const projects = [project('prj_a', 'Alpha'), project('prj_b', 'Beta'), project('prj_c', 'Gamma', { archived_at: '2026-01-02T00:00:00Z' })];
const rooms = () => ({
  prj_a: [room('rm_ah', 'prj_a', 'Alpha', { kind: 'home', last_activity_at: '2026-01-05T10:00:00Z' }), room('rm_gen', 'prj_a', 'general', { parent_id: 'rm_ah', unread_count: 3, last_activity_at: '2026-01-06T10:00:00Z' }),
    room('rm_api', 'prj_a', 'api-work', { parent_id: 'rm_ah', unread_count: 12, last_activity_at: '2026-01-07T10:00:00Z', attention_count: 2 }), room('rm_notes', 'prj_a', 'notes', { parent_id: 'rm_ah', notify: 'mentions', unread_count: 1 }),
    room('rm_mute', 'prj_a', 'muted-room', { parent_id: 'rm_ah', muted_until: future, unread_count: 5 }), room('rm_old', 'prj_a', 'old-room', { parent_id: 'rm_ah', lifecycle: 'closed' }),
    room('rm_hid', 'prj_a', 'hidden-room', { parent_id: 'rm_ah', hidden: true }), room('rm_dm', 'prj_a', 'ada', { kind: 'dm', unread_count: 2, last_activity_at: '2026-01-08T10:00:00Z' })],
  prj_b: [room('rm_bh', 'prj_b', 'Beta', { kind: 'home', last_activity_at: '2026-01-03T10:00:00Z' }), room('rm_bg', 'prj_b', 'beta-general', { parent_id: 'rm_bh', unread_count: 7 })],
  prj_c: [room('rm_ch', 'prj_c', 'Gamma', { kind: 'home' }), room('rm_cg', 'prj_c', 'gamma-room', { parent_id: 'rm_ch' })],
});
const chats = () => ({ project_id: 'prj_agentdm', rooms: [room('rm_chat', 'prj_agentdm', 'ada & bert', { kind: 'dm', agent_chat: true, unread_count: 1 })], joinable: [{ id: 'rm_join', title: 'bert & cleo', message_count: 4 }] });

// fetch is held unless a rule answers it: a rule is a body, or { status, body }; a held request that is aborted rejects like fetch does.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise((resolve, reject) => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false, aborted: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    options.signal?.addEventListener('abort', () => { request.aborted = true; reject(new DOMException('aborted', 'AbortError')); });
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
  const answerAll = (extra = {}) => ({ 'GET /api/agent-dms': chats(), 'GET /api/projects': projects, ...Object.fromEntries(Object.entries(rooms()).map(([id, list]) => [`GET /api/projects/${id}/rooms`, list])), ...extra });
  const mount = async ({ auto = answerAll(), load = true, setup = '' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.evaluateOnNewDocument(() => { try { localStorage.clear(); } catch {} });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; });
    await page.evaluate(async ({ module }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner: true, admin: true });
      await (await import('/js/preferences.js')).loadPreferences('usr_me', { send: async () => ({ preferences: {} }) });
      const { Sidebar } = await import(module);
      window.__calls = [];
      const sidebar = window.__sidebar = new Sidebar(document.querySelector('#projects'), (room, messageID) => { window.__calls.push(['select', room.id, messageID || '']); });
      sidebar.bindSort(document.querySelector('#rooms-sort')); sidebar.bindCollapseAll(document.querySelector('#rooms-fold'));
      sidebar.watchPreview(); sidebar.shownSeq = id => (window.__shown || {})[id] || 0;
    }, { module: MODULE });
    if (setup) await page.evaluate(setup);
    if (load) { await page.evaluate(() => { window.__loading = window.__sidebar.load(); }); await page.evaluate(() => window.__loading); }
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && !request.aborted && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findLastIndex(request => !request.answered && !request.aborted && request.method === method && request.path === requestPath), method, requestPath);
  };
  const count = (page, method, requestPath) => page.evaluate((method, requestPath) => window.__requests.filter(request => request.method === method && request.path.startsWith(requestPath)).length, method, requestPath);
  const calls = page => page.evaluate(() => window.__calls.splice(0));
  // The rules that answered the first load stop answering: the next read of these paths is held.
  const hold = (page, ...keys) => page.evaluate(keys => { for (const key of keys) delete window.__auto[key]; }, keys);
  // The rows as the list shows them, in order: [id, text, unread badge, nested].
  const rows = page => page.$$eval('#projects .room-group', groups => groups.map(group => { const row = group.querySelector(':scope > .room-row'); return [group.dataset.roomId, row.querySelector('.room-name').textContent, row.querySelector('.unread-count:not([hidden])')?.textContent || '', !!group.closest('.room-children')]; }));
  const tops = page => page.$$eval('#projects > *', nodes => nodes.map(node => node.className || node.tagName.toLowerCase()));

  // Loading asks for the chats of agents and the projects at once, then every project's rooms at once, and draws only when all of them are in.
  {
    const page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    const dms = await waitRequest(page, 'GET', '/api/agent-dms'), list = await waitRequest(page, 'GET', '/api/projects');
    await answer(page, list, 200, projects);
    for (const id of ['prj_a', 'prj_b', 'prj_c']) await waitRequest(page, 'GET', `/api/projects/${id}/rooms`);
    assert.equal(await page.$$eval('#projects .room-group', nodes => nodes.length), 0, 'nothing is drawn before every read is in');
    for (const [id, list] of Object.entries(rooms())) await answer(page, await waitRequest(page, 'GET', `/api/projects/${id}/rooms`), 200, list);
    assert.equal(await page.$$eval('#projects .room-group', nodes => nodes.length), 0, 'nor before the chats are in');
    await answer(page, dms, 200, chats());
    await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual(await rows(page), [['rm_ah', '⌂ Alpha', '', false], ['rm_gen', '# general', '3', true], ['rm_api', '# api-work', '12', true], ['rm_notes', '# notes', '1', true], ['rm_mute', '# muted-room', '5', true], ['rm_old', '# old-room', '', true],
      ['rm_bh', '⌂ Beta', '', false], ['rm_bg', '# beta-general', '7', true], ['rm_ch', '⌂ Gamma', '', false], ['rm_cg', '# gamma-room', '', false], ['rm_dm', '@ ada', '2', false], ['rm_chat', '⇄ ada & bert', '1', false]]);
    const tail = await page.$$eval('#projects details', nodes => nodes.map(node => [node.className, node.querySelector('summary').textContent, node.open]));
    assert.deepEqual(tail, [['archived-rooms', 'Archived (1)', false], ['hidden-rooms', 'Hidden (1)', false], ['archived-projects', 'Archived projects (1)', false], ['archived-project', 'Gamma', false], ['agent-chat-offers', 'Agent chats you can join (1)', false]]);
    assert.deepEqual(await tops(page), ['room-filter', 'room-group', 'room-group', 'archived-projects', 'direct-rooms']);
    // The bells and the chip: a room with items waiting, a muted one, the project's name on a direct message when there are several projects.
    const marks = await page.evaluate(() => ({ attention: document.querySelector('[data-room-id=rm_api] .room-item-attention')?.title, muted: document.querySelector('[data-room-id=rm_mute] .room-muted')?.title.startsWith('Muted until'),
      chip: document.querySelector('[data-room-id=rm_dm] .room-project')?.textContent, none: document.querySelector('[data-room-id=rm_gen] .room-muted') === null, title: document.querySelector('[data-room-id=rm_api] .room-name').title,
      sidebarRooms: [...window.__sidebar.rooms.keys()].length, sidebarProjects: [...window.__sidebar.projects.keys()].join() }));
    assert.deepEqual(marks, { attention: '2 waiting for you', muted: true, chip: 'Alpha', none: true, title: 'api-work · 2 members', sidebarRooms: 13, sidebarProjects: 'prj_a,prj_b,prj_c' });
    // Rows open their room from the name and from the row beside it.
    await page.$eval('[data-room-id=rm_gen] .room-name', node => node.click());
    await page.$eval('[data-room-id=rm_api] .room-row', node => node.click());
    assert.deepEqual(await calls(page), [['select', 'rm_gen', ''], ['select', 'rm_api', '']]);
    await page.close();
  }

  // A refresh of one project changes its rows in place: the same elements stay, with the focus on one of their buttons and the fold the person
  // opened, the unread numbers and a new room follow, and an unchanged room keeps its record (the object the list holds for it).
  {
    const page = await mount();
    await page.evaluate(() => {
      window.__gen = document.querySelector('[data-room-id=rm_gen]'); window.__api = document.querySelector('[data-room-id=rm_api]'); window.__add = window.__gen.querySelector('.room-name');
      window.__kept = window.__sidebar.rooms.get('rm_notes'); window.__fold = document.querySelector('details.archived-rooms'); window.__fold.open = true; window.__add.focus();
    });
    await hold(page, 'GET /api/projects/prj_a/rooms');
    const changed = rooms().prj_a.map(item => (item.id === 'rm_gen' ? { ...item, unread_count: 0 } : item.id === 'rm_api' ? { ...item, unread_count: 14 } : item)).concat(room('rm_new', 'prj_a', 'zeta', { parent_id: 'rm_ah' }));
    await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_a'); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, changed);
    await page.evaluate(() => window.__refreshing); await settle(page);
    const state = await page.evaluate(() => ({ same: document.querySelector('[data-room-id=rm_gen]') === window.__gen && document.querySelector('[data-room-id=rm_api]') === window.__api, focus: document.activeElement === window.__add,
      fold: document.querySelector('details.archived-rooms') === window.__fold && window.__fold.open, kept: window.__sidebar.rooms.get('rm_notes') === window.__kept }));
    assert.deepEqual([state.same, state.focus, state.fold], [true, true, true]);
    // The hand-written list stores the new record for every room of a refreshed project; the page keeps the one it holds for an unchanged room.
    if (REPLACED) assert.equal(state.kept, true);
    assert.deepEqual((await rows(page)).filter(row => ['rm_gen', 'rm_api', 'rm_new'].includes(row[0])).map(row => row.slice(0, 3)), [['rm_gen', '# general', ''], ['rm_api', '# api-work', '14'], ['rm_new', '# zeta', '']]);
    await page.close();
  }

  // Two reads of one project's rooms can finish in either order: the older answer, which lacks a room created meanwhile, does not remove it.
  {
    const page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.evaluate(() => { window.__one = window.__sidebar.refreshProject('prj_a'); });
    const first = await waitRequest(page, 'GET', '/api/projects/prj_a/rooms');
    await page.evaluate(() => { window.__two = window.__sidebar.refreshProject('prj_a'); });
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/projects/prj_a/rooms' && !request.answered).length === 2);
    const second = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/projects/prj_a/rooms' && !request.answered));
    await answer(page, second, 200, rooms().prj_a.concat(room('rm_new', 'prj_a', 'zeta', { parent_id: 'rm_ah' })));
    await answer(page, first, 200, rooms().prj_a);
    await page.evaluate(() => Promise.all([window.__one, window.__two])); await settle(page);
    assert.equal((await rows(page)).some(row => row[0] === 'rm_new'), true, 'the older list does not remove the room the newer one has');
    await page.close();
  }

  // A reset ends every read: the answer of the projects starts no read of the rooms, and the answers of the rooms and the chats draw nothing.
  {
    let page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    let list = await waitRequest(page, 'GET', '/api/projects');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, list, 200, projects); await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/projects/'), await page.$$eval('#projects .room-group', nodes => nodes.length)], [0, 0], 'a reset takes the project list and reads no rooms');
    await page.close();
    page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    list = await waitRequest(page, 'GET', '/api/projects');
    await answer(page, list, 200, projects);
    const pending = [];
    for (const id of ['prj_a', 'prj_b', 'prj_c']) pending.push(await waitRequest(page, 'GET', `/api/projects/${id}/rooms`));
    const dms = await waitRequest(page, 'GET', '/api/agent-dms');
    await page.evaluate(() => window.__sidebar.reset());
    for (const [index, id] of ['prj_a', 'prj_b', 'prj_c'].entries()) await answer(page, pending[index], 200, rooms()[id]);
    await answer(page, dms, 200, chats());
    await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual([await page.$$eval('#projects .room-group', nodes => nodes.length), await page.evaluate(() => window.__sidebar.rooms.size)], [0, 0], 'the rooms answered after a reset draw nothing');
    await page.close();
  }

  // A refresh of a project that was forgotten while it was out does not bring the project back; a deleted room leaves; a project added later draws.
  {
    const page = await mount();
    await hold(page, 'GET /api/projects/prj_b/rooms');
    await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_b'); });
    const held = await waitRequest(page, 'GET', '/api/projects/prj_b/rooms');
    await page.evaluate(() => window.__sidebar.forgetProject('prj_b'));
    assert.equal((await rows(page)).some(row => row[0] === 'rm_bh' || row[0] === 'rm_bg'), false);
    await answer(page, held, 200, rooms().prj_b);
    await page.evaluate(() => window.__refreshing); await settle(page);
    assert.equal((await rows(page)).some(row => row[0] === 'rm_bh'), false, 'the answer for a forgotten project draws nothing');
    await page.evaluate(() => window.__sidebar.forgetRoom('rm_notes'));
    assert.equal((await rows(page)).some(row => row[0] === 'rm_notes'), false);
    await page.evaluate(project => { window.__adding = window.__sidebar.addProject(project); }, project('prj_d', 'Delta'));
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_d/rooms'), 200, [room('rm_dh', 'prj_d', 'Delta', { kind: 'home' })]);
    await page.evaluate(() => window.__adding); await settle(page);
    assert.equal((await rows(page)).some(row => row[0] === 'rm_dh'), true);
    await page.close();
  }

  // The open room is marked and previews its last message (or says there is none); every room previews when the person turned that on.
  {
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_gen' ? { ...item, last_message_body: 'The **latest** reply' } : item)) }) });
    await page.evaluate(() => window.__sidebar.setActive('rm_gen'));
    const active = () => page.evaluate(() => ({ rows: [...document.querySelectorAll('.room-row.active')].map(row => row.parentElement.dataset.roomId), previews: [...document.querySelectorAll('.room-preview')].map(node => [node.parentElement.dataset.roomId, node.textContent]) }));
    assert.deepEqual((await active()).rows, ['rm_gen']);
    assert.equal((await active()).previews.length, 1);
    assert.equal((await active()).previews[0][0], 'rm_gen');
    assert.deepEqual(await rows(page).then(list => list.find(row => row[0] === 'rm_gen').slice(2, 3)), [''], 'the open room counts as read');
    await page.evaluate(() => { localStorage.setItem('aimebu2.ui.sidebar_preview.usr_me', '1'); document.dispatchEvent(new CustomEvent('aimebu:sidebar-preview')); });
    assert.equal((await active()).previews.length > 5, true, 'every room previews');
    assert.equal((await active()).previews.find(item => item[0] === 'rm_api')[1], 'No messages yet');
    await page.close();
  }

  // The chevron of a root folds its rooms and remembers it, and a folded root shows its rooms' unread total; collapse all and expand all work on every
  // root and say what the next click does.
  {
    const page = await mount();
    const root = id => page.evaluate(id => { const group = document.querySelector(`[data-room-id=${id}]`); const toggle = group.querySelector(':scope > .room-row > .room-collapse'); const badge = group.querySelector(':scope > .room-row > .unread-count'); return { collapsed: group.classList.contains('collapsed'), label: toggle.getAttribute('aria-label'), expanded: toggle.getAttribute('aria-expanded'), badge: badge && !badge.hidden ? badge.textContent : '' }; }, id);
    assert.deepEqual(await root('rm_ah'), { collapsed: false, label: 'Collapse Alpha', expanded: 'true', badge: '' });
    await page.$eval('[data-room-id=rm_ah] > .room-row > .room-collapse', node => node.click());
    assert.deepEqual(await root('rm_ah'), { collapsed: true, label: 'Expand Alpha', expanded: 'false', badge: '21' });
    assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.rooms.collapsed')), '["rm_ah"]');
    const all = () => page.$eval('#rooms-fold', node => [node.title, node.classList.contains('is-collapsed'), node.disabled]);
    assert.deepEqual(await all(), ['Collapse all projects', false, false]);
    await page.$eval('#rooms-fold', node => node.click());
    assert.deepEqual([await all(), (await root('rm_bh')).collapsed], [['Expand all projects', true, false], true]);
    await page.$eval('#rooms-fold', node => node.click());
    assert.deepEqual([await all(), (await root('rm_ah')).collapsed, (await root('rm_bh')).collapsed], [['Collapse all projects', false, false], false, false]);
    await page.close();
    const reloaded = await mount({ setup: () => localStorage.setItem('aimebu2.rooms.collapsed', '["rm_bh"]') });
    await reloaded.evaluate(() => { window.__sidebar.collapsedRooms = new Set(JSON.parse(localStorage.getItem('aimebu2.rooms.collapsed'))); window.__sidebar.render(); });
    assert.equal(await reloaded.$eval('[data-room-id=rm_bh]', node => node.classList.contains('collapsed')), true, 'the folded roots come from the browser');
    await reloaded.close();
  }

  // The filter narrows the list to matching rooms and projects, says so when nothing matches, opens the archived projects while it is on, and keeps
  // its own box (and what was typed in it) through every redraw.
  {
    const page = await mount();
    await page.evaluate(() => { window.__box = document.querySelector('.room-filter-input'); });
    await page.type('.room-filter-input', 'gen');
    assert.deepEqual((await rows(page)).map(row => row[0]), ['rm_gen', 'rm_bg']);
    assert.equal(await page.evaluate(() => document.querySelector('.room-filter-input') === window.__box && window.__box.value), 'gen');
    await page.evaluate(() => window.__sidebar.setActive('rm_gen'));
    assert.equal(await page.evaluate(() => document.querySelector('.room-filter-input') === window.__box && window.__box.value === 'gen'), true, 'a redraw keeps the box');
    await page.type('.room-filter-input', 'zzz');
    assert.equal(await page.$eval('.rooms-empty', node => node.textContent), 'No rooms match “genzzz”.');
    await page.$eval('.room-filter-input', node => { node.value = 'gamma'; node.dispatchEvent(new Event('input')); });
    assert.equal(await page.$eval('details.archived-projects', node => node.open), true, 'the archived projects open while filtering');
    assert.equal((await rows(page)).some(row => row[0] === 'rm_cg'), true);
    await page.$eval('.room-filter-input', node => { node.value = ''; node.dispatchEvent(new Event('input')); });
    assert.equal((await rows(page)).length > 8, true);
    await page.close();
  }

  // A mute that runs out takes its bell with it without a reload; the agents with problems show on their room.
  {
    const soon = new Date(Date.now() + 700).toISOString();
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    assert.equal(await page.$('[data-room-id=rm_mute] .room-muted:not([hidden])') !== null, true);
    await page.waitForFunction(() => !document.querySelector('[data-room-id=rm_mute] .room-muted:not([hidden])'), { timeout: 5000 });
    await page.evaluate(() => window.__sidebar.setHealth([{ name: 'ada', room_ids: ['rm_api'], flags: [{ code: 'stuck' }] }]));
    assert.deepEqual(await page.$eval('[data-room-id=rm_api] .room-health', node => [node.textContent, node.title, node.hidden]), ['⚠ 1', 'ada: stuck', false]);
    await page.evaluate(() => window.__sidebar.setHealth([]));
    assert.equal(await page.$eval('[data-room-id=rm_api] .room-health', node => node.hidden), true);
    await page.close();
  }

  // A change of a room's preference (showing a hidden room) asks the server, reads the project again and opens the room; a refusal is told in a
  // dialog; a reset while the request is out takes the answer, reads nothing and opens nothing.
  {
    const hiddenFold = page => page.evaluate(() => { for (const fold of document.querySelectorAll('details.hidden-rooms')) fold.open = true; });
    let page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.$eval('[data-hidden-room-id=rm_hid]', node => node.click());
    let held = await waitRequest(page, 'PATCH', '/api/rooms/rm_hid/preferences');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, held), { hidden: false });
    await answer(page, held, 200, {});
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a.map(item => (item.id === 'rm_hid' ? { ...item, hidden: false } : item)));
    await settle(page);
    assert.deepEqual(await calls(page), [['select', 'rm_hid', '']]);
    assert.equal((await rows(page)).some(row => row[0] === 'rm_hid'), true, 'the shown room is in the list');
    await page.close();
    page = await mount();
    await page.$eval('[data-hidden-room-id=rm_hid]', node => node.click());
    await answer(page, await waitRequest(page, 'PATCH', '/api/rooms/rm_hid/preferences'), 409, { message: 'The room changed', error: 'stale' });
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#app-dialog-title').textContent, document.querySelector('.app-dialog-message').textContent]), ['Could not change the room', 'The room changed']);
    assert.deepEqual(await calls(page), []);
    await page.close();
    page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.$eval('[data-hidden-room-id=rm_hid]', node => node.click());
    held = await waitRequest(page, 'PATCH', '/api/rooms/rm_hid/preferences');
    await page.evaluate(() => window.__sidebar.reset());
    const before = await count(page, 'GET', '/api/projects/prj_a');
    await answer(page, held, 200, {}); await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/projects/prj_a'), await calls(page), await page.evaluate(() => !!document.querySelector('#app-dialog')?.open)], [before, [], false], 'a reset takes the answer: no read, no room opened, no dialog');
    await page.close();
  }

  // Reopening an archived room, joining a chat of agents and creating a room under a home room: each sends its call, reads again and opens the room; a
  // reset while the call is out reads nothing and opens nothing.
  {
    const fill = (page, title) => page.evaluate(title => { const dialog = document.querySelector('#app-dialog'); dialog.querySelector('[name=title]').value = title; dialog.querySelector('form').requestSubmit(); }, title);
    // reopen
    let page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.$eval('[data-room-id=rm_old] .reopen-room', node => node.click());
    let held = await waitRequest(page, 'POST', '/api/rooms/rm_old/reopen');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, held), { lifecycle_rev: 1 });
    await answer(page, held, 200, room('rm_old', 'prj_a', 'old-room', { parent_id: 'rm_ah' }));
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a.map(item => (item.id === 'rm_old' ? { ...item, lifecycle: 'open' } : item)));
    await settle(page);
    assert.equal(await page.$eval('[data-room-id=rm_old] .room-row', node => node.classList.contains('archived')), false);
    await page.close();
    page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.$eval('[data-room-id=rm_old] .reopen-room', node => node.click());
    held = await waitRequest(page, 'POST', '/api/rooms/rm_old/reopen');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, held, 200, room('rm_old', 'prj_a', 'old-room')); await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/projects/prj_a'), await calls(page), await page.evaluate(() => window.__sidebar.rooms.size)], [0 + (await count(page, 'GET', '/api/projects/prj_a')), [], 0], 'a reset takes the answer of a reopen');
    await page.close();
    // join
    page = await mount();
    await hold(page, 'GET /api/agent-dms');
    await page.$eval('[data-offer-id=rm_join] .agent-chat-join', node => node.click());
    held = await waitRequest(page, 'POST', '/api/rooms/rm_join/join');
    await answer(page, held, 200, {});
    const withJoined = { ...chats(), rooms: [...chats().rooms, room('rm_join', 'prj_agentdm', 'bert & cleo', { kind: 'dm', agent_chat: true })], joinable: [] };
    await answer(page, await waitRequest(page, 'GET', '/api/agent-dms'), 200, withJoined);
    await settle(page);
    assert.deepEqual(await calls(page), [['select', 'rm_join', '']]);
    assert.equal(await page.$('[data-offer-id=rm_join]'), null, 'the joined chat is no offer any more');
    await page.close();
    page = await mount();
    await hold(page, 'GET /api/agent-dms');
    await page.$eval('[data-offer-id=rm_join] .agent-chat-join', node => node.click());
    held = await waitRequest(page, 'POST', '/api/rooms/rm_join/join');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, held, 200, {}); await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/agent-dms'), await calls(page)], [1, []], 'a reset takes the answer of a join: no read, nothing opened');
    await page.close();
    // create
    page = await mount();
    await page.$eval('[data-room-id=rm_ah] .add-room', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    await fill(page, 'fresh-room');
    held = await waitRequest(page, 'POST', '/api/projects/prj_a/rooms');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, held), { title: 'fresh-room', parent_id: 'rm_ah', worktree: { enabled: false, base_ref: '' } });
    await answer(page, held, 200, room('rm_fresh', 'prj_a', 'fresh-room', { parent_id: 'rm_ah' }));
    await settle(page);
    assert.deepEqual(await calls(page), [['select', 'rm_fresh', '']]);
    assert.equal((await rows(page)).some(row => row[0] === 'rm_fresh'), true);
    await page.close();
    page = await mount();
    await page.$eval('[data-room-id=rm_ah] .add-room', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    await fill(page, 'fresh-room');
    held = await waitRequest(page, 'POST', '/api/projects/prj_a/rooms');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, held, 200, room('rm_fresh', 'prj_a', 'fresh-room', { parent_id: 'rm_ah' })); await settle(page);
    assert.deepEqual([await calls(page), await page.$$eval('#projects .room-group', nodes => nodes.length)], [[], 0], 'a reset takes the answer of a new room');
    await page.close();
  }

  // The dialog that asks for a new room's name belongs to the list it was opened in: answered after a reset, it sends nothing.
  {
    const page = await mount();
    await page.$eval('[data-room-id=rm_ah] .add-room', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    await page.evaluate(() => window.__sidebar.reset());
    await page.evaluate(() => { const dialog = document.querySelector('#app-dialog'); dialog.querySelector('[name=title]').value = 'late-room'; dialog.querySelector('form').requestSubmit(); });
    await settle(page);
    assert.deepEqual([await count(page, 'POST', '/api/projects/'), await calls(page)], [0, []], 'a name given after a reset creates nothing');
    await page.close();
  }

  // Read marks: one request per room at a time, the furthest mark asked for meanwhile follows it, and the count that comes back is the room's; a
  // reset while a mark is out lets it go and sends nothing for the old room.
  {
    let page = await mount();
    await page.evaluate(() => { window.__shown = { rm_api: 40 }; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); });
    const first = await waitRequest(page, 'POST', '/api/rooms/rm_api/read');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, first), { seq: 40 });
    await page.evaluate(() => { window.__shown.rm_api = 50; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); window.__shown.rm_api = 45; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); });
    assert.equal(await count(page, 'POST', '/api/rooms/rm_api/read'), 1, 'one mark in flight per room');
    await answer(page, first, 200, { unread_count: 5, read_cursor: 40 }); await settle(page);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.get('rm_api').unread_count), 5);
    const second = await waitRequest(page, 'POST', '/api/rooms/rm_api/read');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, second), { seq: 50 }, 'the furthest mark asked for follows');
    await answer(page, second, 200, { unread_count: 0, read_cursor: 50 }); await settle(page);
    assert.equal(await count(page, 'POST', '/api/rooms/rm_api/read'), 2);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.get('rm_api').unread_count), 0);
    await page.close();
    page = await mount();
    await page.evaluate(() => { window.__shown = { rm_api: 40 }; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); });
    const held = await waitRequest(page, 'POST', '/api/rooms/rm_api/read');
    await page.evaluate(() => { window.__shown.rm_api = 60; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); window.__sidebar.reset(); });
    await answer(page, held, 200, { unread_count: 1, read_cursor: 40 }); await settle(page);
    assert.equal(await count(page, 'POST', '/api/rooms/rm_api/read'), 1, 'a reset sends no follow-up for the old room');
    await page.close();
  }

  // The archive search: a query asks after a short wait, a newer query ends the older, an older answer does nothing, hits open their room at the
  // message, a failure says so, and a reset takes the wait and the request.
  {
    const type = (page, text) => page.$eval('.archive-search', (node, text) => { node.value = text; node.dispatchEvent(new Event('input')); }, text);
    const hits = [{ room_id: 'rm_old', room_name: 'old-room', snippet: 'the old thing', message_id: 'msg_1' }];
    let page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await type(page, 'old');
    assert.equal(await count(page, 'GET', '/api/projects/prj_a/archive'), 0, 'the query waits for a pause in the typing');
    const first = await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=old');
    await type(page, 'older');
    const second = await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=older');
    assert.equal(await page.evaluate(index => window.__requests[index].aborted, first), true, 'the newer query ends the older request');
    await answer(page, first, 200, [{ room_id: 'rm_old', room_name: 'stale', snippet: 'x', message_id: 'm0' }]);
    await answer(page, second, 200, hits); await settle(page);
    assert.deepEqual(await page.$$eval('.archive-hit', nodes => nodes.map(node => [node.querySelector('strong').textContent, node.querySelector('span').textContent])), [['old-room', 'the old thing']]);
    await page.$eval('.archive-hit', node => node.click()); await settle(page);
    assert.deepEqual(await calls(page), [['select', 'rm_old', 'msg_1']]);
    await type(page, '');
    assert.equal(await page.$eval('.archive-hits', node => node.hidden), true, 'an empty query clears the hits');
    await type(page, 'boom');
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=boom'), 500, { message: 'down' }); await settle(page);
    assert.equal(await page.$eval('.archive-search-error', node => node.textContent), 'Could not search archived messages.');
    await page.close();
    // a reset in the wait and a reset while the request is out
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await type(page, 'old');
    await page.evaluate(() => window.__sidebar.reset());
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.equal(await count(page, 'GET', '/api/projects/prj_a/archive'), 0, 'a reset ends the wait');
    await page.close();
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await type(page, 'old');
    const out = await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=old');
    await page.evaluate(() => window.__sidebar.reset());
    assert.equal(await page.evaluate(index => window.__requests[index].aborted, out), true, 'a reset ends the request');
    await answer(page, out, 200, hits); await settle(page);
    assert.deepEqual([await page.$$eval('.archive-hit', nodes => nodes.length), await calls(page)], [0, []]);
    await page.close();
    // a hit whose room the list does not hold asks for it, and a reset in between opens nothing
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await type(page, 'far');
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=far'), 200, [{ room_id: 'rm_far', room_name: 'far', snippet: 's', message_id: 'm9' }]); await settle(page);
    await page.$eval('.archive-hit', node => node.click());
    const loading = await waitRequest(page, 'GET', '/api/rooms/rm_far');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, loading, 200, room('rm_far', 'prj_a', 'far')); await settle(page);
    assert.deepEqual(await calls(page), [], 'a reset takes the room a hit asked for');
    await page.close();
  }

  // A mute that was running when the list was reset does not draw the list again when it runs out.
  {
    const soon = new Date(Date.now() + 500).toISOString();
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    await page.evaluate(() => window.__sidebar.reset());
    await new Promise(resolve => setTimeout(resolve, 1000));
    assert.equal(await page.$eval('#projects', node => node.children.length), 0, 'the list stays empty after a reset');
    await page.close();
  }

  // The sort menu: it opens on the chosen order, moves with the arrows, closes with Escape (the focus back on the button) or a click outside, and a
  // choice reorders the projects, keeps the choice in the browser and sends it to the server.
  {
    const page = await mount();
    const menu = () => page.evaluate(() => ({ hidden: document.querySelector('#rooms-sort-menu').hidden, expanded: document.querySelector('#rooms-sort').getAttribute('aria-expanded'), focus: document.activeElement.dataset.sort || document.activeElement.id,
      checked: [...document.querySelectorAll('.sort-item')].map(item => item.getAttribute('aria-checked')) }));
    assert.deepEqual([(await menu()).hidden, (await menu()).expanded, (await menu()).checked], [true, 'false', ['false', 'false', 'true']]);
    await page.$eval('#rooms-sort', node => node.click());
    assert.deepEqual(await menu(), { hidden: false, expanded: 'true', focus: 'manual', checked: ['false', 'false', 'true'] });
    await page.keyboard.press('ArrowDown');
    assert.equal((await menu()).focus, 'name');
    await page.keyboard.press('ArrowUp'); await page.keyboard.press('ArrowUp');
    assert.equal((await menu()).focus, 'activity');
    await page.keyboard.press('Escape');
    assert.deepEqual([(await menu()).hidden, (await menu()).focus], [true, 'rooms-sort']);
    await page.$eval('#rooms-sort', node => node.click());
    await page.mouse.click(700, 500);
    assert.deepEqual([(await menu()).hidden, (await menu()).expanded], [true, 'false']);
    const projectsInOrder = () => page.$$eval('#projects > .room-group', nodes => nodes.map(node => node.dataset.roomId));
    assert.deepEqual(await projectsInOrder(), ['rm_ah', 'rm_bh']);
    await page.$eval('#rooms-sort', node => node.click());
    await page.$eval('.sort-item[data-sort=name]', node => node.click());
    assert.deepEqual([(await menu()).hidden, (await menu()).checked, await page.evaluate(() => localStorage.getItem('aimebu2.ui.sidebar_sort.usr_me'))], [true, ['true', 'false', 'false'], 'name']);
    const saved = await waitRequest(page, 'PUT', '/api/me/preferences');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, saved), { preferences: { sidebar_sort: 'name' } });
    await page.$eval('#rooms-sort', node => node.click());
    await page.$eval('.sort-item[data-sort=activity]', node => node.click());
    assert.deepEqual(await projectsInOrder(), ['rm_ah', 'rm_bh'], 'by latest activity the project with the newer room leads');
    await page.close();
  }

  // The manual order of projects: the move buttons of a home row, the drag of its handle onto another project's row, the end rows disabled, the order
  // kept in the browser and sent to the server, and dragging that is not a project drag changes nothing.
  {
    const page = await mount();
    const order = () => page.$$eval('#projects > .room-group', nodes => nodes.map(node => node.dataset.roomId));
    const moves = () => page.evaluate(() => ({ upFirst: document.querySelector('[data-room-id=rm_ah] .project-move-up').disabled, downFirst: document.querySelector('[data-room-id=rm_ah] .project-move-down').disabled, downLast: document.querySelector('[data-room-id=rm_bh] .project-move-down').disabled,
      grip: document.querySelector('[data-room-id=rm_ah] .project-grip').getAttribute('aria-label') }));
    assert.deepEqual(await moves(), { upFirst: true, downFirst: false, downLast: true, grip: 'Drag to move Alpha' });
    await page.$eval('[data-room-id=rm_ah] .project-move-down', node => node.click());
    assert.deepEqual(await order(), ['rm_bh', 'rm_ah']);
    assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.ui.sidebar_project_order.usr_me')), '["prj_b","prj_a","prj_c"]');
    const saved = await waitRequest(page, 'PUT', '/api/me/preferences');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, saved), { preferences: { sidebar_project_order: '["prj_b","prj_a","prj_c"]' } });
    await page.$eval('[data-room-id=rm_ah] .project-move-up', node => node.click());
    assert.deepEqual(await order(), ['rm_ah', 'rm_bh']);
    const drag = (type, target, y) => page.evaluate((type, target, y) => { const row = document.querySelector(`[data-room-id=${target}] > .room-row`); const box = row.getBoundingClientRect(); window.__transfer ||= new DataTransfer();
      row.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: window.__transfer, clientY: box.top + box.height * y })); return [row.classList.contains('drop-before'), row.classList.contains('drop-after')]; }, type, target, y);
    await drag('dragstart', 'rm_ah', 0.5);
    assert.deepEqual(await drag('dragover', 'rm_bh', 0.9), [false, true], 'over the lower half the drop goes after');
    assert.deepEqual(await drag('dragover', 'rm_bh', 0.1), [true, false]);
    await drag('dragover', 'rm_bh', 0.9);
    await drag('drop', 'rm_bh', 0.9);
    assert.deepEqual(await order(), ['rm_bh', 'rm_ah'], 'the dropped project follows the row it was dropped on');
    assert.equal(await page.evaluate(() => document.querySelectorAll('.drop-before, .drop-after').length), 0);
    await drag('dragstart', 'rm_gen', 0.5); // a task room is no project
    assert.deepEqual(await drag('dragover', 'rm_bh', 0.9), [false, false]);
    await page.close();
  }

  // Saving the sort or the manual order is fire-and-forget: the list keeps its own cache and the server copy follows. A save still out when the list is
  // reset or destroyed, answered well or badly, changes nothing, asks for nothing more and shows no error; saves of one name go out one at a time.
  {
    const triggers = {
      sort: async page => { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=name]', node => node.click()); },
      manual: page => page.$eval('[data-room-id=rm_ah] .project-move-down', node => node.click()),
    };
    for (const [what, trigger] of Object.entries(triggers)) {
      for (const how of REPLACED ? ['reset', 'destroy'] : ['reset']) {
        for (const status of [200, 500]) {
          const page = await mount();
          await hold(page, 'PUT /api/me/preferences');
          await trigger(page);
          const saved = await waitRequest(page, 'PUT', '/api/me/preferences');
          await page.evaluate(how => { window.__sidebar[how](); window.__before = { sort: window.__sidebar.sort, manual: String(window.__sidebar.manual), sent: window.__requests.length }; }, how);
          await answer(page, saved, status, status === 200 ? { preferences: {} } : { error: 'nope' });
          await settle(page);
          assert.deepEqual(await page.evaluate(() => ({ sort: window.__sidebar.sort, manual: String(window.__sidebar.manual), sent: window.__requests.length })), await page.evaluate(() => window.__before), `${what}: a save answered ${status} after a ${how} changes nothing and sends nothing`);
          assert.deepEqual([await page.$$eval('#projects > *', nodes => nodes.length), await page.evaluate(() => !!document.querySelector('#app-dialog')?.open), await calls(page)], [0, false, []], `${what}: nor draws, opens a dialog or selects (${how}, ${status})`);
          await page.close();
        }
      }
    }
    // Two saves of one name: the second waits for the first, so the older value can never land after the newer one.
    for (const [what, move] of [['manual', page => page.$eval('[data-room-id=rm_ah] .project-move-down', node => node.click())], ['sort', async page => { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=name]', node => node.click()); }]]) {
      const page = await mount();
      await hold(page, 'PUT /api/me/preferences');
      await move(page);
      const first = await waitRequest(page, 'PUT', '/api/me/preferences');
      if (what === 'manual') await page.$eval('[data-room-id=rm_bh] .project-move-down', node => node.click());
      else { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=activity]', node => node.click()); }
      await settle(page);
      assert.equal(await count(page, 'PUT', '/api/me/preferences'), 1, `${what}: the second save waits while the first is out`);
      await answer(page, first, 500, { error: 'nope' });
      const second = await waitRequest(page, 'PUT', '/api/me/preferences');
      const bodies = await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT' && request.path === '/api/me/preferences').map(request => request.body));
      assert.equal(bodies.length, 2, `${what}: it goes out after the first fails`);
      assert.deepEqual(bodies[1], what === 'manual' ? { preferences: { sidebar_project_order: '["prj_a","prj_b","prj_c"]' } } : { preferences: { sidebar_sort: 'activity' } }, `${what}: it carries the latest value`);
      await answer(page, second, 200, { preferences: {} });
      await page.close();
    }
  }

  // A save queued behind one that is still out belongs to the person who made it: if someone else is signed in by the time the first answer comes,
  // it is dropped rather than sent with their cookie.
  for (const [what, first, second] of [['manual', page => page.$eval('[data-room-id=rm_ah] .project-move-down', node => node.click()), page => page.$eval('[data-room-id=rm_bh] .project-move-down', node => node.click())],
    ['sort', async page => { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=name]', node => node.click()); }, async page => { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=activity]', node => node.click()); }]]) {
    const page = await mount();
    await hold(page, 'PUT /api/me/preferences');
    await first(page);
    const out = await waitRequest(page, 'PUT', '/api/me/preferences');
    await second(page);
    await page.evaluate(async () => { (await import('/js/api.js')).setSession({ csrf_token: 'other', id: 'usr_other', owner: false, admin: false }); });
    await answer(page, out, 200, { preferences: {} });
    await settle(page);
    assert.equal(await count(page, 'PUT', '/api/me/preferences'), 1, `${what}: a queued save is dropped when another person is signed in`);
    await page.close();
  }

  // The same for an answer that arrives in the very turn in which the list is reset or destroyed (the request is past aborting): the code that runs on
  // it must not store, draw, read or open anything.
  for (const how of REPLACED ? ['reset', 'destroy'] : ['reset']) {
    const end = (page, index, status, body) => page.evaluate((index, status, body, how) => { window.__answer(index, status, body); window.__sidebar[how](); }, index, status, body, how);
    const quiet = async (page, extra, label) => {
      await settle(page);
      assert.deepEqual([await page.$$eval('#projects .room-group', nodes => nodes.length), await calls(page), await page.evaluate(() => !!document.querySelector('#app-dialog')?.open), ...extra], [0, [], false, ...extra.map(() => 0)], `${how}: ${label}`);
    };
    // the project list, a project's rooms and the chats of agents
    let page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await end(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects); await page.evaluate(() => window.__loading);
    await quiet(page, [await count(page, 'GET', '/api/projects/')], 'the project list answered as the list ends reads no rooms');
    await page.close();
    page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
    const pending = [];
    for (const id of ['prj_a', 'prj_b', 'prj_c']) pending.push(await waitRequest(page, 'GET', `/api/projects/${id}/rooms`));
    const dms = await waitRequest(page, 'GET', '/api/agent-dms');
    await page.evaluate((pending, bodies, how) => { for (const [position, index] of pending.entries()) window.__answer(index, 200, bodies[position]); window.__sidebar[how](); }, pending, Object.values(rooms()), how);
    await settle(page);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.size), 0, `${how}: the rooms answered as the list ends are not stored`);
    await answer(page, dms, 200, chats());
    await page.close();
    page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
    for (const id of ['prj_a', 'prj_b', 'prj_c']) await answer(page, await waitRequest(page, 'GET', `/api/projects/${id}/rooms`), 200, rooms()[id]);
    await end(page, await waitRequest(page, 'GET', '/api/agent-dms'), 200, chats()); await page.evaluate(() => window.__loading);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.size), 0, `${how}: nor the chats`);
    await page.close();
    // a refresh of one project
    page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms');
    await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_a'); });
    await end(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, [room('rm_new', 'prj_a', 'zeta')]); await page.evaluate(() => window.__refreshing);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.has('rm_new')), false, `${how}: a refresh answered as the list ends stores nothing`);
    await page.close();
    // a project added by hand and the chats of agents refreshed on their own
    page = await mount();
    await hold(page, 'GET /api/projects/prj_z/rooms', 'GET /api/agent-dms');
    await page.evaluate(() => { window.__adding = window.__sidebar.addProject({ id: 'prj_z', name: 'z' }); });
    await end(page, await waitRequest(page, 'GET', '/api/projects/prj_z/rooms'), 200, [room('rm_z', 'prj_z', 'zed')]); await page.evaluate(() => window.__adding);
    assert.deepEqual(await page.evaluate(() => [window.__sidebar.projects.has('prj_z'), window.__sidebar.rooms.has('rm_z')]), [false, false], `${how}: a project answered as the list ends is not stored`);
    await page.close();
    page = await mount();
    await hold(page, 'GET /api/agent-dms');
    await page.evaluate(() => { window.__chatting = window.__sidebar.refreshAgentChats(); });
    await end(page, await waitRequest(page, 'GET', '/api/agent-dms'), 200, chats()); await page.evaluate(() => window.__chatting);
    const stored = await page.evaluate(() => window.__sidebar.rooms.size);
    assert.equal(stored, await page.evaluate(() => 0), `${how}: chats answered as the list ends are not stored`);
    await page.close();
    // calls: show a hidden room, reopen, join, create
    for (const [name, start, method, path, body, follow] of [['preference', () => document.querySelector('[data-hidden-room-id=rm_hid]').click(), 'PATCH', '/api/rooms/rm_hid/preferences', {}, '/api/projects/prj_a/rooms'],
      ['reopen', () => document.querySelector('[data-room-id=rm_old] .reopen-room').click(), 'POST', '/api/rooms/rm_old/reopen', room('rm_old', 'prj_a', 'old-room'), '/api/projects/prj_a/rooms'],
      ['join', () => document.querySelector('[data-offer-id=rm_join] .agent-chat-join').click(), 'POST', '/api/rooms/rm_join/join', {}, '/api/agent-dms']]) {
      page = await mount();
      await hold(page, 'GET /api/projects/prj_a/rooms', 'GET /api/agent-dms');
      const before = await count(page, 'GET', follow);
      await page.evaluate(start);
      await end(page, await waitRequest(page, method, path), 200, body);
      await settle(page);
      assert.deepEqual([await count(page, 'GET', follow), await calls(page), await page.evaluate(() => !!document.querySelector('#app-dialog')?.open)], [before, [], false], `${how}: ${name} answered as the list ends reads nothing, opens nothing`);
      await page.close();
    }
    page = await mount();
    await page.$eval('[data-room-id=rm_ah] .add-room', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    await page.evaluate(() => { const dialog = document.querySelector('#app-dialog'); dialog.querySelector('[name=title]').value = 'fresh'; dialog.querySelector('form').requestSubmit(); });
    await end(page, await waitRequest(page, 'POST', '/api/projects/prj_a/rooms'), 200, room('rm_fresh', 'prj_a', 'fresh-room', { parent_id: 'rm_ah' }));
    await quiet(page, [], 'a new room answered as the list ends is not opened or drawn');
    await page.close();
    // the archive search and a hit
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await page.$eval('.archive-search', node => { node.value = 'old'; node.dispatchEvent(new Event('input')); });
    await end(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=old'), 200, [{ room_id: 'rm_old', room_name: 'old-room', snippet: 's', message_id: 'm1' }]);
    await settle(page);
    assert.equal(await page.$$eval('.archive-hit', nodes => nodes.length), 0, `${how}: hits answered as the list ends are not drawn`);
    await page.close();
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await page.$eval('.archive-search', node => { node.value = 'far'; node.dispatchEvent(new Event('input')); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=far'), 200, [{ room_id: 'rm_far', room_name: 'far', snippet: 's', message_id: 'm9' }]); await settle(page);
    await page.$eval('.archive-hit', node => node.click());
    await end(page, await waitRequest(page, 'GET', '/api/rooms/rm_far'), 200, room('rm_far', 'prj_a', 'far'));
    await settle(page);
    assert.deepEqual(await calls(page), [], `${how}: the room a hit asked for, answered as the list ends, is not opened`);
    await page.close();
  }

  // A list destroyed while a read or a call is out takes the answer and does nothing with it: nothing is stored or drawn, no read follows, no room is opened,
  // no dialog is shown, the archive search ends and the mute timer stops.
  if (REPLACED) {
    const destroy = page => page.evaluate(() => window.__sidebar.destroy());
    const empty = page => page.$eval('#projects', node => node.children.length);
    let page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    let held = await waitRequest(page, 'GET', '/api/projects');
    await destroy(page);
    await answer(page, held, 200, projects); await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/projects/'), await empty(page)], [0, 0], 'a destroyed list reads no rooms');
    await page.close();
    page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
    const pending = [];
    for (const id of ['prj_a', 'prj_b', 'prj_c']) pending.push(await waitRequest(page, 'GET', `/api/projects/${id}/rooms`));
    const dms = await waitRequest(page, 'GET', '/api/agent-dms');
    await destroy(page);
    for (const [index, id] of ['prj_a', 'prj_b', 'prj_c'].entries()) await answer(page, pending[index], 200, rooms()[id]);
    await answer(page, dms, 200, chats()); await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual([await empty(page), await page.evaluate(() => window.__sidebar.rooms.size)], [0, 0], 'the answers after destroy store and draw nothing');
    await page.close();
    // A refresh, a call and a dialog that finish after destroy do nothing.
    page = await mount();
    await hold(page, 'GET /api/projects/prj_a/rooms', 'GET /api/agent-dms');
    await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_a'); });
    held = await waitRequest(page, 'GET', '/api/projects/prj_a/rooms');
    await destroy(page);
    await answer(page, held, 200, [room('rm_new', 'prj_a', 'zeta')]); await page.evaluate(() => window.__refreshing); await settle(page);
    assert.equal(await page.evaluate(() => window.__sidebar.rooms.has('rm_new')), false, 'a refresh answered after destroy stores nothing');
    await page.close();
    for (const [name, start, path, body] of [['reopen', () => document.querySelector('[data-room-id=rm_old] .reopen-room').click(), '/api/rooms/rm_old/reopen', room('rm_old', 'prj_a', 'old-room')],
      ['join', () => document.querySelector('[data-offer-id=rm_join] .agent-chat-join').click(), '/api/rooms/rm_join/join', {}], ['preference', () => document.querySelector('[data-hidden-room-id=rm_hid]').click(), '/api/rooms/rm_hid/preferences', {}]]) {
      page = await mount();
      await hold(page, 'GET /api/projects/prj_a/rooms', 'GET /api/agent-dms');
      await page.evaluate(start);
      const call = await waitRequest(page, name === 'preference' ? 'PATCH' : 'POST', path);
      const total = await page.evaluate(() => window.__requests.length);
      await destroy(page);
      await answer(page, call, 200, body); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__requests.length), await calls(page), await page.evaluate(() => !!document.querySelector('#app-dialog')?.open)], [total, [], false], `${name}: a destroyed list reads nothing, opens nothing and shows nothing`);
      await page.close();
    }
    page = await mount();
    await page.$eval('[data-room-id=rm_ah] .add-room', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    await destroy(page);
    await page.evaluate(() => { const dialog = document.querySelector('#app-dialog'); dialog.querySelector('[name=title]').value = 'late-room'; dialog.querySelector('form').requestSubmit(); });
    await settle(page);
    assert.deepEqual([await count(page, 'POST', '/api/projects/'), await calls(page)], [0, []], 'a name given after destroy creates nothing');
    await page.close();
    page = await mount();
    await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
    await page.$eval('.archive-search', node => { node.value = 'old'; node.dispatchEvent(new Event('input')); });
    held = await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=old');
    await destroy(page);
    assert.equal(await page.evaluate(index => window.__requests[index].aborted, held), true, 'destroy ends the archive request');
    await page.close();
    page = await mount();
    await page.$eval('.archive-search', node => { node.value = 'old'; node.dispatchEvent(new Event('input')); });
    await destroy(page);
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.equal(await count(page, 'GET', '/api/projects/prj_a/archive'), 0, 'destroy ends the wait of the archive search');
    await page.close();
    page = await mount();
    await page.evaluate(() => { window.__shown = { rm_api: 40 }; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); });
    held = await waitRequest(page, 'POST', '/api/rooms/rm_api/read');
    await page.evaluate(() => { window.__shown.rm_api = 60; window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); window.__sidebar.destroy(); });
    await answer(page, held, 200, { unread_count: 1, read_cursor: 40 }); await settle(page);
    assert.equal(await count(page, 'POST', '/api/rooms/rm_api/read'), 1, 'destroy sends no follow-up mark');
    await page.close();
    const soon = new Date(Date.now() + 500).toISOString();
    page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    await destroy(page);
    await new Promise(resolve => setTimeout(resolve, 1000));
    assert.equal(await empty(page), 0, 'the list stays empty after destroy');
    await page.close();
  }

  // A join answered in the very turn in which the list is reset (the request is past aborting) reads no chats and opens nothing.
  {
    const page = await mount();
    await hold(page, 'GET /api/agent-dms');
    await page.$eval('[data-offer-id=rm_join] .agent-chat-join', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/rooms/rm_join/join');
    const before = await count(page, 'GET', '/api/agent-dms');
    await page.evaluate(index => { window.__answer(index, 200, {}); window.__sidebar.reset(); }, held);
    await settle(page);
    assert.deepEqual([await count(page, 'GET', '/api/agent-dms'), await calls(page)], [before, []], 'a join answered as the list is reset reads no chats and opens nothing');
    await page.close();
  }

  // A save of the sort or the order that is still queued when the list is reset is not sent (the first one, already out, is left alone); of several
  // queued saves of one name only the newest goes out.
  {
    const move = (page, id, direction) => page.$eval(`[data-room-id=${id}] .project-move-${direction}`, node => node.click());
    const saves = page => page.evaluate(() => window.__requests.filter(request => request.method === 'PUT' && request.path === '/api/me/preferences').map(request => request.body.preferences));
    let page = await mount();
    await hold(page, 'PUT /api/me/preferences');
    await move(page, 'rm_ah', 'down');
    const first = await waitRequest(page, 'PUT', '/api/me/preferences');
    await move(page, 'rm_bh', 'down');
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, first, 200, { preferences: {} }); await settle(page);
    assert.deepEqual(await saves(page), [{ sidebar_project_order: '["prj_b","prj_a","prj_c"]' }], 'a queued order save is not sent after a reset');
    await page.close();
    page = await mount();
    await hold(page, 'PUT /api/me/preferences');
    await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=name]', node => node.click());
    const named = await waitRequest(page, 'PUT', '/api/me/preferences');
    await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=activity]', node => node.click());
    await page.evaluate(() => window.__sidebar.reset());
    await answer(page, named, 200, { preferences: {} }); await settle(page);
    assert.deepEqual(await saves(page), [{ sidebar_sort: 'name' }], 'a queued sort save is not sent after a reset');
    await page.close();
    page = await mount();
    await hold(page, 'PUT /api/me/preferences');
    await move(page, 'rm_ah', 'down');
    const out = await waitRequest(page, 'PUT', '/api/me/preferences');
    await move(page, 'rm_bh', 'down'); await move(page, 'rm_ah', 'down');
    await answer(page, out, 200, { preferences: {} });
    await waitRequest(page, 'PUT', '/api/me/preferences'); await settle(page);
    assert.deepEqual(await saves(page), [{ sidebar_project_order: '["prj_b","prj_a","prj_c"]' }, { sidebar_project_order: '["prj_b","prj_a","prj_c"]' }], 'of two queued saves of one name only the newest goes out');
    await page.close();
  }

  // A destroyed list sends nothing: every entry that reads, calls or saves is asked once the list is gone, and no request goes out.
  if (REPLACED) {
    const entries = {
      load: store => store.load(), addProject: store => store.addProject({ id: 'prj_a', name: 'Alpha' }), refreshProject: store => store.refreshProject('prj_a'), refreshChats: store => store.refreshAgentChats(),
      setPreference: store => store.setPreference({ id: 'rm_gen', project_id: 'prj_a' }, 'hidden', true), reopen: store => store.reopen({ id: 'rm_old', project_id: 'prj_a' }),
      join: store => store.join({ id: 'rm_join', title: 'x' }), createRoom: store => store.createRoom({ id: 'rm_ah', project_id: 'prj_a' }, { title: 'x', worktree: false }, store.generation),
      openHit: store => store.openHit({ room_id: 'rm_far', project_id: 'prj_a' }), moveProject: store => store.moveProject('prj_a', 1), setSort: store => store.setSort('name'),
      markSeen: store => store.markSeen({ id: 'rm_gen', unread_count: 3, notify: 'default' }), archive: store => store.searchArchive('prj_a', 'old'),
    };
    for (const [name, call] of Object.entries(entries)) {
      const page = await mount();
      await page.evaluate(() => { window.__sidebar.shownSeq = () => 5; window.__sidebar.destroy(); window.__sent = window.__requests.length; });
      await page.evaluate(`(async () => { try { await (${call.toString()})(window.__sidebar.store); } catch {} })()`);
      await settle(page); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 400)));
      assert.equal(await page.evaluate(() => window.__requests.length - window.__sent), 0, `${name}: a destroyed list sends no request`);
      await page.close();
    }
    // The same entries on a live list do send (the check above is not vacuous).
    for (const [name, call] of Object.entries(entries)) {
      const page = await mount({ auto: answerAll() });
      await page.evaluate(() => { window.__sidebar.shownSeq = () => 5; window.__sent = window.__requests.length; });
      await page.evaluate(`(async () => { try { await Promise.race([(${call.toString()})(window.__sidebar.store), new Promise(resolve => setTimeout(resolve, 400))]); } catch {} })()`);
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 400)));
      assert.ok(await page.evaluate(() => window.__requests.length - window.__sent) > 0, `${name}: a live list sends`);
      await page.close();
    }
    // A queued sort or order save is not sent once the list is destroyed either.
    const page = await mount();
    await hold(page, 'PUT /api/me/preferences');
    await page.$eval('[data-room-id=rm_ah] .project-move-down', node => node.click());
    const first = await waitRequest(page, 'PUT', '/api/me/preferences');
    await page.$eval('[data-room-id=rm_bh] .project-move-down', node => node.click());
    await page.evaluate(() => window.__sidebar.destroy());
    await answer(page, first, 200, { preferences: {} }); await settle(page);
    assert.equal(await count(page, 'PUT', '/api/me/preferences'), 1, 'a queued order save is not sent after a destroy');
    await page.close();
  }

  // What the callers see: a read or call that fails in the very turn in which the list ends settles quietly (no error thrown, no dialog), a call that was
  // still working when the list ended says so with false, and the store keeps nothing of an answer that came too late.
  if (REPLACED) {
    for (const how of ['reset', 'destroy']) {
      const end = (page, index, status, body) => page.evaluate((index, status, body, how) => { window.__answer(index, status, body); window.__sidebar[how](); }, index, status, body, how);
      const quietly = page => page.evaluate(async () => { const result = await window.__call.then(value => ({ value }), error => ({ error: error.message })); return { ...result, dialog: !!document.querySelector('#app-dialog')?.open }; });
      for (const [name, hold1, start, request] of [
        ['addProject', 'GET /api/projects/prj_z/rooms', () => { window.__call = window.__sidebar.addProject({ id: 'prj_z', name: 'z' }); }, ['GET', '/api/projects/prj_z/rooms']],
        ['refreshAgentChats', 'GET /api/agent-dms', () => { window.__call = window.__sidebar.refreshAgentChats(); }, ['GET', '/api/agent-dms']],
        ['refreshProject', 'GET /api/projects/prj_a/rooms', () => { window.__call = window.__sidebar.refreshProject('prj_a'); }, ['GET', '/api/projects/prj_a/rooms']],
        ['createRoom', 'POST /api/projects/prj_a/rooms', () => { const store = window.__sidebar.store; window.__call = store.createRoom({ id: 'rm_ah', project_id: 'prj_a' }, { title: 'x', worktree: false }, store.generation); }, ['POST', '/api/projects/prj_a/rooms']],
      ]) {
        const page = await mount();
        await hold(page, hold1);
        await page.evaluate(start);
        await end(page, await waitRequest(page, ...request), 500, { error: 'nope' });
        const result = await quietly(page);
        assert.deepEqual([result.error, result.dialog], [undefined, false], `${how}: ${name} failing as the list ends throws nothing and shows nothing`);
        await page.close();
      }
      // a call that is still reading when the list ends says it did not finish, and opens nothing
      for (const [name, start, method, path, body] of [
        ['setPreference', () => { window.__call = window.__sidebar.store.setPreference({ id: 'rm_hid', project_id: 'prj_a' }, 'hidden', false); }, 'PATCH', '/api/rooms/rm_hid/preferences', {}],
        ['reopen', () => { window.__call = window.__sidebar.store.reopen({ id: 'rm_old', project_id: 'prj_a' }); }, 'POST', '/api/rooms/rm_old/reopen', room('rm_old', 'prj_a', 'old-room')],
      ]) {
        const page = await mount();
        await hold(page, 'GET /api/projects/prj_a/rooms');
        await page.evaluate(() => { window.__sidebar.setActive('rm_old'); });
        await page.evaluate(start);
        await answer(page, await waitRequest(page, method, path), 200, body);
        await waitRequest(page, 'GET', '/api/projects/prj_a/rooms');
        await page.evaluate(how => window.__sidebar[how](), how);
        const result = await quietly(page);
        assert.deepEqual([result.value, result.error, result.dialog, await calls(page)], [false, undefined, false, []], `${how}: ${name} still reading as the list ends says it did not finish`);
        await page.close();
      }
      // a refresh that started after the load did is not undone by the load's older list
      if (how === 'reset') {
        const page = await mount({ auto: {}, load: false });
        await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
        await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
        const older = await waitRequest(page, 'GET', '/api/projects/prj_a/rooms');
        for (const id of ['prj_b', 'prj_c']) await answer(page, await waitRequest(page, 'GET', `/api/projects/${id}/rooms`), 200, rooms()[id]);
        await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_a'); });
        const newer = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/projects/prj_a/rooms' && !request.answered));
        await answer(page, newer, 200, rooms().prj_a.concat(room('rm_late', 'prj_a', 'late')));
        await page.evaluate(() => window.__refreshing);
        await answer(page, older, 200, rooms().prj_a);
        await answer(page, await waitRequest(page, 'GET', '/api/agent-dms'), 200, chats());
        await page.evaluate(() => window.__loading);
        assert.equal(await page.evaluate(() => window.__sidebar.rooms.has('rm_late')), true, 'the load\'s older list does not remove a room the newer refresh listed');
        await page.close();
      }
      // a project added by hand whose list a newer refresh has already replaced does not undo that refresh
      if (how === 'reset') {
        let page = await mount();
        await hold(page, 'GET /api/projects/prj_z/rooms');
        await page.evaluate(() => { window.__adding = window.__sidebar.addProject({ id: 'prj_z', name: 'z' }); });
        const older = await waitRequest(page, 'GET', '/api/projects/prj_z/rooms');
        await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_z'); });
        const newer = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/projects/prj_z/rooms' && !request.answered));
        await answer(page, newer, 200, [room('rm_z', 'prj_z', 'zed'), room('rm_z2', 'prj_z', 'zed two')]);
        await page.evaluate(() => window.__refreshing);
        await answer(page, older, 200, [room('rm_z', 'prj_z', 'zed')]);
        await page.evaluate(() => window.__adding);
        assert.equal(await page.evaluate(() => window.__sidebar.rooms.has('rm_z2')), true, 'an older list of an added project does not remove a room the newer refresh listed');
        await page.close();
        // a read mark answered for a record that a refresh has replaced is not written into it
        page = await mount();
        await page.evaluate(() => { window.__old = window.__sidebar.rooms.get('rm_api'); window.__sidebar.shownSeq = () => 5; window.__sidebar.markSeen(window.__old); });
        const mark = await waitRequest(page, 'POST', '/api/rooms/rm_api/read');
        await hold(page, 'GET /api/projects/prj_a/rooms');
        await page.evaluate(() => { window.__refreshing = window.__sidebar.refreshProject('prj_a'); });
        await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a.map(item => (item.id === 'rm_api' ? { ...item, unread_count: 14 } : item)));
        await page.evaluate(() => window.__refreshing);
        await answer(page, mark, 200, { unread_count: 0, read_cursor: 5 }); await settle(page);
        assert.deepEqual(await page.evaluate(() => [window.__old.unread_count, window.__sidebar.rooms.get('rm_api').unread_count, window.__sidebar.rooms.get('rm_api') === window.__old]), [12, 14, false], 'a stale record is left alone');
        await page.close();
      }
      // the store keeps nothing of a hit or an error that arrives too late
      for (const [label, status, body] of [['hits', 200, [{ room_id: 'rm_old', room_name: 'old-room', snippet: 's', message_id: 'm1' }]], ['an error', 500, { error: 'nope' }]]) {
        const page = await mount();
        await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
        await page.$eval('.archive-search', node => { node.value = 'old'; node.dispatchEvent(new Event('input')); });
        await end(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=old'), status, body);
        await settle(page);
        assert.equal(await page.evaluate(() => window.__sidebar.store.archiveHits.size), 0, `${how}: ${label} answered as the list ends are not kept`);
        await page.close();
      }
      // the room a hit asked for is not kept either
      {
        const page = await mount();
        await page.evaluate(() => { document.querySelector('details.archived-rooms').open = true; });
        await page.$eval('.archive-search', node => { node.value = 'far'; node.dispatchEvent(new Event('input')); });
        await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/archive/search?q=far'), 200, [{ room_id: 'rm_far', room_name: 'far', snippet: 's', message_id: 'm9' }]); await settle(page);
        await page.$eval('.archive-hit', node => node.click());
        await end(page, await waitRequest(page, 'GET', '/api/rooms/rm_far'), 200, room('rm_far', 'prj_a', 'far'));
        await settle(page);
        assert.equal(await page.evaluate(() => window.__sidebar.rooms.has('rm_far')), false, `${how}: the room a hit asked for, answered as the list ends, is not kept`);
        await page.close();
      }
    }
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('sidebar page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
