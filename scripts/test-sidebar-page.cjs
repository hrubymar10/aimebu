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

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('sidebar page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
