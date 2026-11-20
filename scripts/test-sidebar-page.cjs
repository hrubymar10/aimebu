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
const room = (id, projectID, title, patch = {}) => ({ id, project_id: projectID, title, kind: 'task', purpose: 'general', parent_id: '', lifecycle: 'open', unread_count: 0, hidden: false, notify: 'default', muted_until: '', member_count: 2, last_activity_at: at, agent_chat: false, lifecycle_rev: 1, ...patch });
// The icon of a row is a mask with a tooltip, not text: the tests name each kind by one character so a row reads as "<kind> <title>".
const GLYPHS = { home: '⌂', general: '#', task: '☑', research: '?', dm: '@', 'agent-chat': '⇄' };
// A time in an answer that is `ms` after the page gives the answer. A time worked out here, in Node, before the page is opened, runs out while the
// page is still starting when the machine is busy (a browser launch, a navigation and the modules take longer under load), and a test that waits for a
// mute to run out then never sees it running, or takes an expired one for a pending one.
const inMs = ms => `@now+${ms}`;
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
    // "@now+1500" in an answer is a time 1500 ms after the answer is given, worked out here in the page (see inMs).
    const timed = body => JSON.stringify(body).replace(/"@now\+(\d+)"/g, (_, ms) => JSON.stringify(new Date(Date.now() + Number(ms)).toISOString()));
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : timed(body), { status, headers: { 'Content-Type': 'application/json' } })); };
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
  const mount = async ({ auto = answerAll(), load = true, setup = '', hold = false } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.evaluateOnNewDocument(hold => { window.__holdFolds = hold; try { localStorage.clear(); } catch {} }, hold);
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
      // The app says which room it opens (or that there is none) right after the first load, and until then no project folds. Most tests are not about
      // that, so a load here says it at once unless the test holds the folds (`hold`).
      const loadList = sidebar.load.bind(sidebar);
      sidebar.load = async () => { const result = await loadList(); if (!window.__holdFolds) sidebar.releaseFolds(); return result; };
    }, { module: MODULE });
    if (setup) await page.evaluate(setup);
    if (load) { await page.evaluate(() => { window.__loading = window.__sidebar.load(); }); await page.evaluate(() => window.__loading); }
    return page;
  };
  // A project's actions live in the menu behind its ☰ button: open it, press the item, wait for the list to draw again.
  const projectAct = (page, id, item) => page.evaluate(async (id, item) => {
    const row = document.querySelector(`[data-room-id=${id}] > .room-item`); row.querySelector('.project-menu-btn').click();
    await new Promise(resolve => setTimeout(resolve, 30)); row.querySelector(`.project-menu-${item}`).click();
  }, id, item);
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
  const rows = page => page.$$eval('#projects .room-group', (groups, glyphs) => groups.map(group => { const row = group.querySelector(':scope > .room-item'); return [group.dataset.roomId, `${glyphs[row.querySelector('.room-item-icon').dataset.roomType]} ${row.querySelector('.room-item-name').textContent}`, row.querySelector('.room-item-unread:not([hidden])')?.textContent || '', !!group.closest('.room-children')]; }), GLYPHS);
  const tops = page => page.$$eval('#projects > *', nodes => nodes.map(node => node.className || node.tagName.toLowerCase()));

  // Owners see restricted routing rooms with a lock, while ordinary rooms have no lock.
  {
    const auto = answerAll();
    auto['GET /api/projects/prj_a/rooms'] = [...rooms().prj_a, room('rm_route', 'prj_a', 'routing', { parent_id: 'rm_ah', restricted: true })];
    const page = await mount({ auto });
    assert.equal(await page.$eval('[data-room-id=rm_route] .room-restricted', node => node.getAttribute('aria-label')), 'Restricted room');
    assert.equal(await page.$('[data-room-id=rm_gen] .room-restricted'), null);
    await page.close();
  }

  // A parentless task room stays inside its project's collapsed home row instead of becoming a standalone root.
  {
    const auto = answerAll();
    auto['GET /api/projects/prj_b/rooms'] = [...rooms().prj_b, room('rm_route', 'prj_b', 'routing', { unread_count: 4, restricted: true })];
    const page = await mount({ auto });
    assert.deepEqual(await page.evaluate(() => {
      const root = document.querySelector('[data-room-id=rm_bh]');
      const route = document.querySelector('[data-room-id=rm_route]');
      return { roots: [...document.querySelectorAll('#projects > .room-group')].map(node => node.dataset.roomId), collapsed: root.classList.contains('collapsed'), nested: root.querySelector('.room-children [data-room-id=rm_route]') === route, visible: !!route.getClientRects().length, unread: root.querySelector(':scope > .room-item .room-item-unread')?.textContent };
    }), { roots: ['rm_ah', 'rm_bh'], collapsed: true, nested: true, visible: false, unread: '11' });
    await page.$eval('[data-room-id=rm_bh] .room-collapse', node => node.click());
    assert.equal(await page.$eval('[data-room-id=rm_route]', node => !!node.getClientRects().length), true, 'expanding the project reveals routing');
    await page.close();
  }

  // Loading asks for the chats of agents and the projects at once, then every project's rooms at once. The project list draws each project as a
  // placeholder in its final place; a project's rooms replace its placeholder when its own list lands, in whatever order the lists arrive, and the
  // rest of the list does not move or lose the focus it had.
  {
    const page = await mount({ auto: {}, load: false, hold: true });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    const dms = await waitRequest(page, 'GET', '/api/agent-dms'), list = await waitRequest(page, 'GET', '/api/projects');
    assert.equal(await page.$$eval('#projects .room-group', nodes => nodes.length), 0, 'nothing is drawn before the project list is in');
    await answer(page, list, 200, projects);
    for (const id of ['prj_a', 'prj_b', 'prj_c']) await waitRequest(page, 'GET', `/api/projects/${id}/rooms`);
    await settle(page);
    const places = () => page.$$eval('#projects > .room-group', nodes => nodes.map(node => node.dataset.projectId ? ['placeholder', node.dataset.projectId, `${node.querySelector('.room-item-icon').dataset.roomType === 'home' ? '⌂' : '?'} ${node.querySelector('.room-item-name').textContent.trim()}`] : ['rooms', node.dataset.roomId, node.classList.contains('collapsed')]));
    assert.deepEqual(await places(), [['placeholder', 'prj_a', '⌂ Alpha'], ['placeholder', 'prj_b', '⌂ Beta']], 'each live project waits in its place (the archived one is not drawn yet)');
    const height = await page.$eval('.project-placeholder', node => Math.round(node.getBoundingClientRect().height));
    // The second project lands first: it is drawn below the first one's placeholder, and folded as it will be once the app has opened a room.
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_b/rooms'), 200, rooms().prj_b); await settle(page);
    assert.deepEqual(await places(), [['placeholder', 'prj_a', '⌂ Alpha'], ['rooms', 'rm_bh', true]], 'a list that lands first is drawn in its own place');
    assert.equal(await page.$eval('[data-room-id=rm_bh]', node => Math.round(node.getBoundingClientRect().height)), height, 'a folded project is as tall as its placeholder: nothing below it moves');
    // Focus is on a row; the other project landing above it does not take it.
    await page.focus('[data-room-id=rm_bh] .room-item-name');
    const before = await page.evaluate(() => { window.__focused = document.activeElement; return document.activeElement.closest('[data-room-id]')?.dataset.roomId; });
    const top = await page.$eval('[data-room-id=rm_bh]', node => Math.round(node.getBoundingClientRect().top));
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a); await settle(page);
    assert.equal(before, 'rm_bh');
    assert.equal(await page.evaluate(() => document.activeElement === window.__focused), true, 'the focused row is the same element after the other project landed');
    assert.deepEqual((await places()).map(row => row.slice(0, 2).join(':')), ['rooms:rm_ah', 'rooms:rm_bh']);
    assert.ok(await page.$eval('[data-room-id=rm_bh]', node => Math.round(node.getBoundingClientRect().top)) >= top - 1, 'the list below a landed project does not move up');
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_c/rooms'), 200, rooms().prj_c);
    await settle(page);
    assert.equal(await page.$('[data-room-id=rm_chat]'), null, 'the chats between agents are not drawn before their own list is in');
    await answer(page, dms, 200, chats());
    await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual(await page.evaluate(() => [[...window.__sidebar.rooms.keys()][0], window.__sidebar.firstRoom().id]), ['rm_bh', 'rm_ah'], 'the lists landed in the other order, yet a start with nothing to restore opens the first project\'s first room');
    // The app then opens a room, the folds are released, and what was drawn is what stays: no reflow at that moment.
    const drawn = await tops(page), shapes = await places();
    await page.evaluate(() => window.__sidebar.setActive(window.__sidebar.firstRoom().id)); await settle(page);
    assert.deepEqual(await tops(page), drawn, 'opening the first room changes nothing that was drawn');
    assert.deepEqual(await places(), shapes);
    await page.close();
  }
  // The room the app means to open keeps its project open while the rest are drawn folded.
  {
    const page = await mount({ auto: answerAll(), load: false, hold: true });
    await page.evaluate(() => { window.__sidebar.expect('rm_gen'); window.__loading = window.__sidebar.load(); }); await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual(await page.$$eval('#projects > .room-group', nodes => nodes.map(node => [node.dataset.roomId, node.classList.contains('collapsed')])), [['rm_ah', false], ['rm_bh', true]], 'the project holding the expected room is open, the others folded');
    await page.close();
  }
  // A start with nothing to restore opens the first room of the first project, so that project is the one drawn open; with a stale address the
  // fallback is the remembered room's project. Either way, selecting what the app opens changes nothing that was drawn.
  for (const [label, ids, open, setActive] of [['nothing to restore', [], ['rm_ah', false, 'rm_bh', true], 'rm_ah'], ['a stale address falls back to the remembered room', ['rm_gone', 'rm_bg'], ['rm_ah', true, 'rm_bh', false], 'rm_bg']]) {
    const page = await mount({ auto: answerAll(), load: false, hold: true });
    await page.evaluate(ids => { window.__sidebar.expect(ids); window.__loading = window.__sidebar.load(); }, ids); await page.evaluate(() => window.__loading); await settle(page);
    const fold = () => page.$$eval('#projects > .room-group', nodes => nodes.map(node => [node.dataset.roomId, node.classList.contains('collapsed')]).flat());
    assert.deepEqual(await fold(), open, `${label}: the project that will open is drawn open, the others folded`);
    const drawn = await tops(page), shapes = await fold();
    await page.evaluate(room => window.__sidebar.setActive(room), setActive); await settle(page);
    assert.deepEqual(await fold(), shapes, `${label}: opening the room does not fold or unfold anything`);
    assert.deepEqual(await tops(page), drawn);
    await page.close();
  }
  // With every row's last message shown, a project row has a preview line, and so does its placeholder: nothing moves when its rooms land.
  {
    const page = await mount({ auto: {}, load: false, hold: true, setup: "localStorage.setItem('aimebu2.ui.sidebar_preview.usr_me', '1')" });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects); await settle(page);
    const height = await page.$eval('.project-placeholder[data-project-id=prj_b]', node => Math.round(node.getBoundingClientRect().height));
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_b/rooms'), 200, rooms().prj_b); await settle(page);
    assert.equal(await page.$eval('[data-room-id=rm_bh]', node => Math.round(node.getBoundingClientRect().height)), height, 'with previews on, a folded project is as tall as its placeholder');
    const hover = await page.$eval('.project-placeholder .room-item', node => { const style = getComputedStyle(node); return [style.pointerEvents, style.cursor, style.backgroundColor]; });
    assert.deepEqual(hover.slice(0, 2), ['none', 'default'], 'a placeholder is not a control');
    // Filtering while a list is out says nothing about rooms that are still arriving.
    await page.evaluate(() => window.__sidebar.store.setFilter('zzz-nothing')); await settle(page);
    assert.equal(await page.$('.rooms-empty'), null, 'no "No rooms match" while a project\'s list is still out');
    // A project taken away while its list is out does not come back when the list lands.
    await page.evaluate(() => { window.__sidebar.store.setFilter(''); window.__sidebar.store.forgetProject('prj_a'); }); await settle(page);
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a); await settle(page);
    assert.equal(await page.evaluate(() => window.__sidebar.projects?.has?.('prj_a') ?? window.__sidebar.store.projects.has('prj_a')), false, 'a deleted project is not put back by its late list');
    await page.close();
  }
  // By latest activity the order depends on the rooms, so nothing is drawn until every list is in (no row is placed and then moved).
  {
    const page = await mount({ auto: {}, load: false, setup: "localStorage.setItem('aimebu2.ui.sidebar_sort.usr_me', 'activity')" });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
    for (const id of ['prj_a', 'prj_b', 'prj_c']) await waitRequest(page, 'GET', `/api/projects/${id}/rooms`);
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 200, rooms().prj_a); await settle(page);
    assert.equal(await page.$$eval('#projects .room-group', nodes => nodes.length), 0, 'by latest activity nothing is drawn while a list is missing');
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_b/rooms'), 200, rooms().prj_b);
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_c/rooms'), 200, rooms().prj_c);
    await answer(page, await waitRequest(page, 'GET', '/api/agent-dms'), 200, chats());
    await page.evaluate(() => window.__loading); await settle(page);
    assert.ok(await page.$$eval('#projects .room-group', nodes => nodes.length) > 2, 'then the whole list is drawn at once');
    await page.close();
  }
  // A failed list leaves no placeholder behind.
  {
    const page = await mount({ auto: {}, load: false, hold: true });
    await page.evaluate(() => { window.__loading = window.__sidebar.load().catch(error => error.message); });
    await answer(page, await waitRequest(page, 'GET', '/api/projects'), 200, projects);
    await answer(page, await waitRequest(page, 'GET', '/api/projects/prj_a/rooms'), 500, { error: 'boom' });
    await page.evaluate(() => window.__loading); await settle(page);
    assert.equal(await page.$$eval('#projects .project-placeholder', nodes => nodes.length), 0, 'after a failed read no placeholder is left waiting');
    await page.close();
  }
  {
    const page = await mount({ auto: {}, load: false });
    await page.evaluate(() => { window.__loading = window.__sidebar.load(); });
    const dms = await waitRequest(page, 'GET', '/api/agent-dms'), list = await waitRequest(page, 'GET', '/api/projects');
    await answer(page, list, 200, projects);
    for (const id of ['prj_a', 'prj_b', 'prj_c']) await waitRequest(page, 'GET', `/api/projects/${id}/rooms`);
    for (const [id, list] of Object.entries(rooms())) await answer(page, await waitRequest(page, 'GET', `/api/projects/${id}/rooms`), 200, list);
    await answer(page, dms, 200, chats());
    await page.evaluate(() => window.__loading); await settle(page);
    assert.deepEqual(await rows(page), [['rm_ah', '⌂ Alpha', '21', false], ['rm_gen', '# general', '3', true], ['rm_api', '# api-work', '12', true], ['rm_notes', '# notes', '1', true], ['rm_mute', '# muted-room', '5', true], ['rm_old', '# old-room', '', true],
      ['rm_bh', '⌂ Beta', '7', false], ['rm_bg', '# beta-general', '7', true], ['rm_ch', '⌂ Gamma', '', false], ['rm_cg', '# gamma-room', '', true], ['rm_dm', '@ ada', '2', false], ['rm_chat', '⇄ ada & bert', '1', false]]);
    const tail = await page.$$eval('#projects details', nodes => nodes.map(node => [node.className, node.querySelector('summary').textContent, node.open]));
    assert.deepEqual(tail, [['archived-rooms', 'Archived (1)', false], ['hidden-rooms', 'Hidden (1)', false], ['archived-projects', 'Archived projects (1)', false], ['agent-chat-offers', 'Agent chats you can join (1)', false]]);
    assert.equal(await page.$$eval('#projects details > summary', nodes => nodes.every(node => !!node.querySelector('svg') && getComputedStyle(node).listStyleType === 'none')), true, 'sidebar disclosures use SVG chevrons');
    await page.$eval('.archived-projects > summary', node => node.click());
    await page.$eval('.archived-project > .room-item .room-collapse', node => node.click());
    await page.waitForFunction(() => !document.querySelector('.archived-project').classList.contains('collapsed'));
    assert.deepEqual(await page.$eval('.archived-project', node => [node.classList.contains('collapsed'), node.querySelector('.room-item-icon').dataset.roomType, !!node.querySelector('.room-children [data-room-id=rm_cg]'), !!node.querySelector('.room-children .room-children')]),
      [false, 'home', true, false], 'archived project uses the live project row and child tree');
    await page.evaluate(() => document.documentElement.dataset.theme = 'high-contrast-dark');
    assert.equal(await page.$eval('.archived-project > .room-item', node => getComputedStyle(node).opacity), '1', 'archived row stays legible in high contrast');
    assert.deepEqual(await tops(page), ['room-filter', 'room-group collapsed', 'room-group collapsed', 'archived-projects', 'direct-rooms']);
    // The bells and the chip: a room with items waiting, a muted one, the project's name on a direct message when there are several projects.
    const marks = await page.evaluate(() => ({ attention: document.querySelector('[data-room-id=rm_api] .room-item-attention')?.title, muted: document.querySelector('[data-room-id=rm_mute] .room-muted')?.title.startsWith('Muted until'),
      chip: document.querySelector('[data-room-id=rm_dm] .room-project')?.textContent, none: document.querySelector('[data-room-id=rm_gen] .room-muted') === null, title: document.querySelector('[data-room-id=rm_api] .room-item-name').title,
      sidebarRooms: [...window.__sidebar.rooms.keys()].length, sidebarProjects: [...window.__sidebar.projects.keys()].join() }));
    assert.deepEqual(marks, { attention: '2 waiting for you', muted: true, chip: 'Alpha', none: true, title: 'api-work · 2 members', sidebarRooms: 13, sidebarProjects: 'prj_a,prj_b,prj_c' });
    const bells = () => page.evaluate(() => ['rm_ah', 'rm_api', 'rm_old'].map(id => document.querySelector(`[data-room-id=${id}] > .room-item .room-item-attention`)?.textContent || ''));
    await page.evaluate(() => { const rooms = window.__sidebar.rooms; rooms.get('rm_api').attention_count = 0; rooms.get('rm_hid').attention_count = 2; window.__sidebar.store.touch(); });
    await page.waitForFunction(() => !document.querySelector('[data-room-id=rm_ah] > .room-item .room-item-attention'));
    assert.deepEqual(await bells(), ['', '', ''], 'a hidden room cannot put an untraceable bell on its project');
    await page.evaluate(() => { const rooms = window.__sidebar.rooms; rooms.get('rm_hid').attention_count = 0; rooms.get('rm_old').attention_count = 2; window.__sidebar.store.touch(); });
    await page.waitForFunction(() => !!document.querySelector('[data-room-id=rm_old] > .room-item .room-item-attention'));
    assert.deepEqual(await bells(), ['🔔 2', '', '🔔 2'], 'an actionable archived item marks its own room and the project');
    await page.evaluate(() => { const rooms = window.__sidebar.rooms; rooms.get('rm_old').attention_count = 0; rooms.get('rm_api').attention_count = 2; window.__sidebar.store.touch(); });
    // The reopen button's icon is centred in its frame (the mask is an inline-block on the anonymous baseline of a 10px line in a
    // 14px-root line box; the button draws as flex, so its centring is measured, not a baseline offset).
    assert.deepEqual(await page.$eval('[data-room-id=rm_old] .reopen-room', button => {
      const frame = button.getBoundingClientRect(), icon = button.querySelector('.icon-mask').getBoundingClientRect();
      return [Math.round((icon.top + icon.bottom) / 2 - (frame.top + frame.bottom) / 2), Math.round((icon.left + icon.right) / 2 - (frame.left + frame.right) / 2), getComputedStyle(button).display];
    }), [0, 0, 'flex'], 'the reopen icon is centred in its button');
    // Rows open their room from the name and from the row beside it.
    await page.$eval('[data-room-id=rm_gen] .room-item-name', node => node.click());
    await page.$eval('[data-room-id=rm_api] .room-item', node => node.click());
    assert.deepEqual(await calls(page), [['select', 'rm_gen', ''], ['select', 'rm_api', '']]);
    await page.close();
  }

  // Home-room names use the width held by hover controls until those controls are actually shown.
  {
    const longTitle = 'Alpha workspace';
    const page = await mount({ auto: answerAll({ 'GET /api/projects': projects.map(item => item.id === 'prj_a' ? { ...item, name: longTitle } : item),
      'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => item.id === 'rm_ah' ? { ...item, title: longTitle } : { ...item, attention_count: 0 }) }) });
    await page.setViewport({ width: 1200, height: 800 });
    await page.$eval('[data-room-id=rm_ah] > .room-item > .room-collapse', node => node.click()); // a folded project shows an unread badge at the row end
    const measure = () => page.$eval('[data-room-id=rm_ah] > .room-item', row => {
      const name = row.querySelector('.room-item-name'), menu = row.querySelector('.project-menu-btn'), add = row.querySelector('.add-room:not([hidden])'), extra = row.querySelectorAll('.project-move:not(.project-menu-btn), .project-move-up, .project-move-down').length;
      const actions = getComputedStyle(row.querySelector('.room-item-actions')), info = row.querySelector('.room-item-info').getBoundingClientRect();
      return { nameRight: info.right, rowRight: row.getBoundingClientRect().right, rowHeight: row.getBoundingClientRect().height, info: [info.left, info.right, info.height], badges: [...row.querySelectorAll('.room-item-count, .room-item-attention, .room-item-unread, .room-health, .room-muted')].map(node => { const box = node.getBoundingClientRect(); const at = row.getBoundingClientRect(); return [node.className, box.left - at.left, box.top - at.top, box.width, box.height]; }),
        nameWidth: name.clientWidth, textWidth: name.scrollWidth, menu: !!menu, add: !!add, overlay: [actions.position, actions.opacity, actions.pointerEvents], extra };
    });
    await page.mouse.move(1100, 700);
    const idle = await measure();
    assert.deepEqual([idle.menu, idle.add, idle.extra, idle.overlay], [true, true, 0, ['absolute', '0', 'none']], 'a project row has the ☰ and + buttons and no arrows, in an overlay that is not seen and takes no room before hover');
    assert.ok(idle.textWidth <= idle.nameWidth, 'the name fits without an ellipsis');
    assert.ok(idle.rowRight - idle.nameRight < 12, `the name column (v1's .room-item-info) reaches the row end, less the row's padding, when controls are hidden: ${JSON.stringify(idle)}`);
    const overlayShown = () => page.waitForFunction(() => getComputedStyle(document.querySelector('[data-room-id=rm_ah] > .room-item .room-item-actions')).opacity === '1'); // past v1's 120ms fade
    await page.hover('[data-room-id=rm_ah] > .room-item'); await overlayShown();
    const hovered = await measure();
    assert.deepEqual([hovered.menu, hovered.add, hovered.extra, hovered.overlay], [true, true, 0, ['absolute', '1', 'auto']]);
    assert.ok(idle.badges.length > 0, 'the project row has a badge to keep in place');
    // The controls have a slot of their own at the end of the name: shown, they cover the name's tail and nothing else (no badge, not the preview line), and they
    // open from the slot, so a short name does not push them over the row's badges.
    const overlap = () => page.$eval('[data-room-id=rm_ah] > .room-item', row => {
      const box = node => { const rect = node.getBoundingClientRect(); return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom }; };
      const actions = box(row.querySelector('.room-item-actions')), name = box(row.querySelector('.room-item-name'));
      const hits = [...row.querySelectorAll('.room-item-count, .room-item-attention, .room-item-unread, .room-health, .room-muted, .room-item-preview')].filter(node => node.getBoundingClientRect().width > 0)
        .filter(node => { const other = box(node); return actions.left < other.right && other.left < actions.right && actions.top < other.bottom && other.top < actions.bottom; }).map(node => node.className);
      return { hits, actionsRight: actions.right, nameRight: name.right };
    });
    const shown = await overlap();
    assert.deepEqual(shown.hits, [], `the hover controls cover no badge and no preview: ${JSON.stringify(shown)}`);
    assert.ok(shown.actionsRight <= shown.nameRight + 1, `they end where the name ends: ${JSON.stringify(shown)}`);
    assert.deepEqual([hovered.rowHeight, hovered.info, hovered.nameWidth, hovered.badges], [idle.rowHeight, idle.info, idle.nameWidth, idle.badges], 'showing the controls changes neither the row nor its columns: they are laid over its end');
    await page.mouse.move(1100, 700);
    await page.focus('[data-room-id=rm_ah] .room-item-name'); await overlayShown();
    const focused = await measure();
    assert.deepEqual([focused.menu, focused.add, focused.extra, focused.overlay], [true, true, 0, ['absolute', '1', 'auto']], 'keyboard focus reveals the controls');
    assert.deepEqual([focused.rowHeight, focused.info, focused.nameWidth, focused.badges], [idle.rowHeight, idle.info, idle.nameWidth, idle.badges], 'and they do not change the row either');
    await page.evaluate(() => { document.activeElement.blur(); document.documentElement.dataset.theme = 'high-contrast-dark'; });
    await page.waitForFunction(() => getComputedStyle(document.querySelector('[data-room-id=rm_ah] > .room-item .room-item-actions')).opacity === '0'); // the fade out
    assert.deepEqual([(await measure()).overlay], [['absolute', '0', 'none']], 'high contrast keeps idle controls out of sight and out of layout');
    const session = await page.createCDPSession();
    await session.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 1 });
    assert.equal(await page.evaluate(() => matchMedia('(hover: none) and (pointer: coarse)').matches), true);
    const touch = await measure();
    assert.deepEqual([touch.menu, touch.add, touch.extra, touch.overlay], [true, true, 0, ['static', '1', 'auto']], 'touch keeps controls in the row, reachable without hover');
    await page.close();
  }

  // A refresh of one project changes its rows in place: the same elements stay, with the focus on one of their buttons and the fold the person
  // opened, the unread numbers and a new room follow, and an unchanged room keeps its record (the object the list holds for it).
  {
    const page = await mount();
    await page.$eval('[data-room-id=rm_ah] > .room-item > .room-collapse', node => node.click()); // a folded project hides its rooms, and a hidden room cannot hold focus
    await page.evaluate(() => {
      window.__gen = document.querySelector('[data-room-id=rm_gen]'); window.__api = document.querySelector('[data-room-id=rm_api]'); window.__add = window.__gen.querySelector('.room-item-name');
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

  // A turn raises several events in a row: scheduling a refresh of one project many times fetches its rooms once.
  {
    const page = await mount();
    await settle(page);
    const before = await count(page, 'GET', '/api/projects/prj_a/rooms');
    await page.evaluate(() => { for (let i = 0; i < 6; i++) window.__sidebar.scheduleRefresh('prj_a'); });
    await page.waitForFunction(n => window.__requests.filter(request => request.path.startsWith('/api/projects/prj_a/rooms')).length > n, {}, before);
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.equal(await count(page, 'GET', '/api/projects/prj_a/rooms'), before + 1, 'six scheduled refreshes of a project make one request');
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
    const active = () => page.evaluate(() => ({ rows: [...document.querySelectorAll('.room-item.active')].map(row => row.parentElement.dataset.roomId), previews: [...document.querySelectorAll('.room-item-preview')].map(node => [node.closest("[data-room-id]").dataset.roomId, node.textContent]) }));
    assert.deepEqual((await active()).rows, ['rm_gen']);
    assert.equal((await active()).previews.length, 1);
    assert.equal((await active()).previews[0][0], 'rm_gen');
    assert.deepEqual(await rows(page).then(list => list.find(row => row[0] === 'rm_gen').slice(2, 3)), [''], 'the open room counts as read');
    // While the Inbox (a view that is no room) is open, no room is the marked one: its highlight and its accent name colour are gone, and come back with the room.
    const mark = () => page.$eval('[data-room-id=rm_gen] > .room-item', row => ({ background: getComputedStyle(row).backgroundColor, name: getComputedStyle(row.querySelector('.room-item-name')).color }));
    const marked = await mark();
    await page.evaluate(() => document.querySelector('#app-view').classList.add('inbox-open'));
    await new Promise(resolve => setTimeout(resolve, 400)); // past the row's background transition
    const unmarked = await mark();
    assert.ok(unmarked.background === 'rgba(0, 0, 0, 0)' && unmarked.background !== marked.background && unmarked.name !== marked.name, `the open room is not highlighted under the Inbox: ${JSON.stringify({ marked, unmarked })}`);
    await page.evaluate(() => document.querySelector('#app-view').classList.remove('inbox-open'));
    await new Promise(resolve => setTimeout(resolve, 400));
    assert.deepEqual(await mark(), marked, 'the highlight returns with the room view');
    // The open room says so to assistive tech, and the member count says what it counts.
    assert.deepEqual(await page.$$eval('.room-item-name[aria-current]', nodes => nodes.map(node => node.closest('[data-room-id]').dataset.roomId)), ['rm_gen']);
    assert.equal(await page.$eval('[data-room-id=rm_gen] .room-item-count', node => node.getAttribute('aria-label')), '2 members');
    await page.evaluate(() => { localStorage.setItem('aimebu2.ui.sidebar_preview.usr_me', '1'); document.dispatchEvent(new CustomEvent('aimebu:sidebar-preview')); });
    assert.equal((await active()).previews.length > 5, true, 'every room previews');
    assert.equal((await active()).previews.find(item => item[0] === 'rm_api')[1], 'No messages yet');
    // A room that holds only system messages (the preview skips them) must not claim to be empty while its count says otherwise.
    await page.evaluate(() => { window.__sidebar.rooms.get('rm_notes').message_count = 3; window.__sidebar.rooms.get('rm_notes').user_message_count = 0; window.__sidebar.store.touch(); });
    await settle(page);
    assert.equal((await active()).previews.find(item => item[0] === 'rm_notes')[1], 'Only system messages');
    // A room whose newest message from a person is only an attachment (no text) has messages from people: it says so, not "only system messages".
    await page.evaluate(() => { window.__sidebar.rooms.get('rm_notes').user_message_count = 1; window.__sidebar.store.touch(); });
    await settle(page);
    assert.equal((await active()).previews.find(item => item[0] === 'rm_notes')[1], '📎 Attachment');
    assert.equal(await page.$eval('[data-room-id=rm_api] .room-item-unread', node => node.title), '12 unread messages', 'the unread chip says what it counts');
    await page.close();
  }

  // The room that becomes the open one is shown: its folded project opens, once for each time it becomes the open room; the fold the person makes after
  // that stays, and the Inbox or a link opening another room of a folded project opens that project too.
  {
    const page = await mount({ auto: answerAll({}) });
    const folded = id => page.$eval(`[data-room-id=${id}]`, node => node.classList.contains('collapsed'));
    await page.evaluate(() => window.__sidebar.setActive('rm_bg')); await settle(page);
    assert.equal(await folded('rm_ah'), true, 'Alpha (a project first seen) starts folded, and holds no open room');
    await page.evaluate(() => window.__sidebar.setActive('rm_gen')); await settle(page);
    assert.equal(await folded('rm_ah'), false, 'a folded project opens when one of its rooms becomes the open room');
    assert.deepEqual(await page.$$eval('.room-item.active', nodes => nodes.map(node => node.parentElement.dataset.roomId)), ['rm_gen'], 'and the open room is on screen and marked');
    await page.$eval('[data-room-id=rm_ah] > .room-item > .room-collapse', node => node.click()); // folding it again while its room is open is the person's choice
    await settle(page);
    assert.equal(await folded('rm_ah'), true, 'a fold made while the room is open stays');
    await page.evaluate(() => window.__sidebar.setActive('rm_bg')); await settle(page);
    await page.evaluate(() => window.__sidebar.setActive('rm_api')); await settle(page);
    assert.equal(await folded('rm_ah'), false, 'and the project opens again when another of its rooms becomes the open one');
    await page.close();
  }

  // A cold load draws the list before the app has opened a room: no project folds until it has, and the project holding that room (at any depth)
  // does not fold when it does; with no room to open the projects fold at once.
  {
    const page = await mount({ hold: true });
    const state = () => page.evaluate(() => ['rm_ah', 'rm_bh'].map(id => document.querySelector(`[data-room-id=${id}]`).classList.contains('collapsed')));
    assert.deepEqual(await state(), [false, true], 'while the app has not said which room it opens, the projects are drawn as they will be folded (nothing is saved yet), the first room\'s project open');
    assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.rooms.collapsed')), null, 'and nothing is remembered as folded');
    await page.evaluate(() => window.__sidebar.setActive('rm_api'));
    assert.deepEqual(await state(), [false, true], 'the project holding the room that opened stays open, the other folds');
    await page.close();
    const none = await mount({ hold: true });
    await none.evaluate(() => window.__sidebar.releaseFolds());
    assert.deepEqual(await none.evaluate(() => ['rm_ah', 'rm_bh'].map(id => document.querySelector(`[data-room-id=${id}]`).classList.contains('collapsed'))), [true, true], 'with no room to open every project folds');
    await none.close();
  }
  // A project starts folded, showing its rooms' unread total; the chevron opens it and the browser remembers that, collapse all and expand all work
  // on every root and say what the next click does, and a root the person opened is not folded again on the next load.
  {
    const page = await mount();
    const root = id => page.evaluate(id => { const group = document.querySelector(`[data-room-id=${id}]`); const toggle = group.querySelector(':scope > .room-item > .room-collapse'); const badge = group.querySelector(':scope > .room-item .room-item-unread'); return { collapsed: group.classList.contains('collapsed'), label: toggle.getAttribute('aria-label'), expanded: toggle.getAttribute('aria-expanded'), badge: badge && !badge.hidden ? badge.textContent : '' }; }, id);
    assert.deepEqual(await root('rm_ah'), { collapsed: true, label: 'Expand Alpha', expanded: 'false', badge: '21' });
    assert.deepEqual((await root('rm_bh')).collapsed, true, 'every project starts folded');
    await page.$eval('[data-room-id=rm_ah] > .room-item > .room-collapse', node => node.click());
    assert.deepEqual(await root('rm_ah'), { collapsed: false, label: 'Collapse Alpha', expanded: 'true', badge: '' });
    assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.rooms.collapsed')), '["rm_bh"]');
    const all = () => page.$eval('#rooms-fold', node => [node.title, node.classList.contains('is-collapsed'), node.disabled]);
    assert.deepEqual(await all(), ['Collapse all projects', false, false], 'one project open: the next click folds them all');
    await page.$eval('#rooms-fold', node => node.click());
    assert.deepEqual([await all(), (await root('rm_ah')).collapsed, (await root('rm_bh')).collapsed], [['Expand all projects', true, false], true, true]);
    await page.$eval('#rooms-fold', node => node.click());
    assert.deepEqual([await all(), (await root('rm_ah')).collapsed, (await root('rm_bh')).collapsed], [['Collapse all projects', false, false], false, false]);
    await page.$eval('[data-room-id=rm_bh] > .room-item > .room-collapse', node => node.click());
    // A project first seen with no rooms is open and stays open when its first room arrives, so the room just made is not hidden; one first seen with
    // rooms starts folded.
    assert.deepEqual(await page.evaluate(() => { const store = window.__sidebar.store; return [store.foldNew(new Set(), new Set(['rm_empty'])), store.foldNew(new Set(['rm_empty']), new Set(['rm_empty'])), store.collapsedRooms.has('rm_empty'), store.foldNew(new Set(['rm_full']), new Set(['rm_full'])), store.collapsedRooms.has('rm_full')]; }), [false, false, false, true, true]);
    await page.evaluate(() => { const store = window.__sidebar.store; store.collapsedRooms.delete('rm_full'); store.seenRooms.delete('rm_empty'); store.seenRooms.delete('rm_full'); localStorage.setItem('aimebu2.rooms.collapsed', JSON.stringify([...store.collapsedRooms])); localStorage.setItem('aimebu2.rooms.seen', JSON.stringify([...store.seenRooms])); });
    // The project that holds the open room is seen but not folded, so its row does not vanish under the person; Escape on a closed menu is left to the page.
    assert.deepEqual(await page.evaluate(() => { const store = window.__sidebar.store; const result = [store.foldNew(new Set(['rm_open']), new Set(['rm_open']), new Set(['rm_open'])), store.collapsedRooms.has('rm_open'), store.seenRooms.has('rm_open')]; store.seenRooms.delete('rm_open'); return result; }), [false, false, true]);
    assert.equal(await page.evaluate(() => { const button = document.querySelector('[data-room-id=rm_ah] > .room-item .project-menu-btn'); const event = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }); button.dispatchEvent(event); return event.defaultPrevented; }), false, 'a closed menu does not swallow Escape');
    // A reload keeps what the person did: the opened root stays open and the folded one stays folded, and nothing seen before is folded again.
    const saved = await page.evaluate(() => [localStorage.getItem('aimebu2.rooms.collapsed'), localStorage.getItem('aimebu2.rooms.seen')]);
    await page.close();
    // The sidebar is built before the setup runs, so the setup hands it what the browser held, as a reload would have.
    const reloaded = await mount({ setup: `localStorage.setItem('aimebu2.rooms.collapsed', ${JSON.stringify(saved[0])}); localStorage.setItem('aimebu2.rooms.seen', ${JSON.stringify(saved[1])}); window.__sidebar.collapsedRooms = new Set(${saved[0]}); window.__sidebar.seenRooms = new Set(${saved[1]});` });
    const states = await reloaded.$$eval('#projects > .room-group', nodes => nodes.map(node => [node.dataset.roomId, node.classList.contains('collapsed')]));
    assert.deepEqual(states.filter(item => item[0] === 'rm_ah' || item[0] === 'rm_bh'), [['rm_ah', false], ['rm_bh', true]], 'what the person did survives a reload');
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
    // In filter results the project chip tells rooms apart (a phone keeps it); outside a filter it only repeats the heading above (a phone hides it, see test-phone-pass-page).
    assert.equal(await page.$$eval('.room-item .room-project', nodes => nodes.length > 0 && nodes.every(node => node.classList.contains('room-project-needed'))), true, 'while filtering every chip is marked as needed');
    await page.evaluate(() => window.__sidebar.setActive('rm_gen'));
    assert.equal(await page.evaluate(() => document.querySelector('.room-filter-input') === window.__box && window.__box.value === 'gen'), true, 'a redraw keeps the box');
    await page.type('.room-filter-input', 'zzz');
    assert.equal(await page.$eval('.rooms-empty', node => node.textContent), 'No rooms match “genzzz”.');
    await page.$eval('.room-filter-input', node => { node.value = 'gamma'; node.dispatchEvent(new Event('input')); });
    assert.equal(await page.$eval('details.archived-projects', node => node.open), true, 'the archived projects open while filtering');
    assert.equal((await rows(page)).some(row => row[0] === 'rm_cg'), true);
    await page.$eval('.room-filter-input', node => { node.value = 'gamma-room'; node.dispatchEvent(new Event('input')); });
    assert.deepEqual(await page.$eval('.archived-project', node => [node.dataset.roomId, node.querySelector(':scope > .room-item .room-item-name')?.textContent, !!node.querySelector('.room-children [data-room-id=rm_cg]')]),
      ['rm_ch', 'Gamma', true], 'a task-room filter keeps the archived project name row');
    await page.$eval('.room-filter-input', node => { node.value = ''; node.dispatchEvent(new Event('input')); });
    assert.equal((await rows(page)).length > 8, true);
    assert.equal(await page.$$eval('.room-item .room-project', nodes => nodes.length > 0 && nodes.every(node => !node.classList.contains('room-project-needed'))), true, 'without a filter no chip is marked as needed');
    await page.close();
  }

  // A mute that runs out takes its bell with it without a reload; the agents with problems show on their room.
  {
    const soon = inMs(1500); // 1.5 s from when the list is answered: the bell is there when it is looked for, and it is gone well within the 5 s wait below
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    assert.equal(await page.$('[data-room-id=rm_mute] .room-muted:not([hidden])') !== null, true);
    await page.waitForFunction(() => !document.querySelector('[data-room-id=rm_mute] .room-muted:not([hidden])'), { timeout: 5000 });
    await page.evaluate(() => window.__sidebar.setHealth([{ name: 'ada', room_ids: ['rm_api'], flags: [{ code: 'stuck' }] }]));
    assert.deepEqual(await page.$eval('[data-room-id=rm_api] .room-health', node => [node.textContent, node.title, node.hidden]), ['⚠ 1', 'ada: stuck', false]);
    await page.evaluate(() => window.__sidebar.setHealth([]));
    assert.equal(await page.$eval('[data-room-id=rm_api] .room-health', node => node.hidden), true);
    await page.close();
  }
  // A nested row with every badge (project chip, health, mute, member count, attention, unread) at the narrowest rooms column: the name gives way, with an
  // ellipsis, the member count and the mute bell go (there is no room for them), and no badge or other part of the row reaches past the row's right edge, on a
  // desktop width and on a phone.
  for (const [label, viewport] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
    const far = new Date(Date.now() + 3600000).toISOString();
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_api' ? { ...item, member_count: 3, muted_until: far, title: 'api-work-with-a-rather-long-name' } : item)) }) });
    await page.setViewport(viewport);
    await page.evaluate(() => { document.documentElement.style.setProperty('--rooms-w', '180px'); document.querySelector('.sidebar').style.width = '180px'; window.__sidebar.setHealth([{ name: 'ada', room_ids: ['rm_api'], flags: [{ code: 'stuck' }] }]); });
    await page.evaluate(() => { const group = document.querySelector('[data-room-id=rm_ah]'); if (group.classList.contains('collapsed')) group.querySelector(':scope > .room-item > .room-collapse').click(); });
    await page.waitForFunction(() => document.querySelector('[data-room-id=rm_api] > .room-item').getBoundingClientRect().width > 0);
    const fit = await page.$eval('[data-room-id=rm_api] > .room-item', row => {
      const edge = row.getBoundingClientRect().right, over = [];
      for (const node of row.querySelectorAll('*')) { const box = node.getBoundingClientRect(); if (box.width > 0 && getComputedStyle(node).display !== 'none' && box.right > edge + 0.5) over.push(`${node.className || node.tagName}:${Math.round(box.right - edge)}`); }
      const name = row.querySelector('.room-item-name'), css = getComputedStyle(name);
      const kinds = ['.room-health', '.room-item-count', '.room-item-attention', '.room-item-unread', '.room-muted'].filter(selector => row.querySelector(selector) && getComputedStyle(row.querySelector(selector)).display !== 'none');
      return { over, ellipsis: css.textOverflow === 'ellipsis' && css.overflow === 'hidden', truncated: name.scrollWidth > name.clientWidth, kinds, list: document.querySelector('#projects').scrollWidth <= document.querySelector('#projects').clientWidth };
    });
    assert.deepEqual([fit.over, fit.ellipsis, fit.truncated, fit.list], [[], true, true, true], `${label}: a row's badges stay inside it and the name is cut with an ellipsis: ${JSON.stringify(fit)}`);
    // A row is as tall as its text: the project row, which has the fold chevron, is not taller than a row of one line without it (the chevron was 30px tall, a line
    // of the name is 18px), and the name starts close to the icon, which is close to the chevron (it did not: two 10px gaps and a margin).
    const sizes = await page.evaluate(() => {
      const row = document.querySelector('[data-room-id=rm_ah] > .room-item'), chevron = row.querySelector('.room-collapse').getBoundingClientRect(), icon = row.querySelector('.room-item-icon').getBoundingClientRect(), name = row.querySelector('.room-item-name').getBoundingClientRect();
      return { row: Math.round(row.getBoundingClientRect().height), line: Math.round(row.querySelector('.room-item-top').getBoundingClientRect().height), chevronToIcon: Math.round(icon.left - chevron.right), iconToName: Math.round(name.left - icon.right), chevronHeight: Math.round(chevron.height) };
    });
    assert.ok(sizes.row <= sizes.line + 24 && sizes.chevronToIcon <= 8 && sizes.iconToName <= 8 && sizes.chevronHeight <= 20, `${label}: a row is as tall as its text, and chevron, icon and name sit close: ${JSON.stringify(sizes)}`);
    const drawer = await page.evaluate(() => {
      const row = document.querySelector('[data-room-id=rm_ah] > .room-item'), chevron = row.querySelector('.room-collapse').getBoundingClientRect(), icon = row.querySelector('.room-item-icon').getBoundingClientRect();
      return { gap: Math.round(icon.left + icon.width / 2 - (chevron.left + chevron.width / 2)), close: getComputedStyle(document.querySelector('#rooms-close')).display, closeLabel: document.querySelector('#rooms-close').getAttribute('aria-label') };
    });
    if (label === 'phone') assert.ok(drawer.gap <= 28 && drawer.close !== 'none' && drawer.closeLabel === 'Close rooms', `phone: the chevron sits next to its icon and the drawer has a close control: ${JSON.stringify(drawer)}`);
    else assert.equal(drawer.close, 'none', 'a desktop width has no drawer, so no drawer close control');
    assert.deepEqual(fit.kinds, ['.room-health', '.room-item-attention', '.room-item-unread'], `${label}: the pills that ask for attention stay; the count and the mute bell give way when the row is that narrow: ${JSON.stringify(fit.kinds)}`);
    await page.close();
  }

  // A row with a member count, a health flag and a bell (attention) badge stays on one line at the narrowest column and at a wider one, in every theme
  // that draws it differently: the name, the count and each badge share one top (within 2px), no badge wraps inside itself, and the row is as tall as a
  // row without badges. A project's home row carries the bell as well, so both kinds are checked.
  for (const theme of ['dark', 'high-contrast-dark', 'light']) {
    for (const width of [180, 240]) {
      const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_api' ? { ...item, member_count: 3, title: 'test-general-room', attention_count: 1, unread_count: 0 } : item.id === 'rm_ah' ? { ...item, attention_count: 1 } : item)) }) });
      await page.evaluate((theme, width) => { document.documentElement.dataset.theme = theme; document.documentElement.style.setProperty('--rooms-w', `${width}px`); document.querySelector('.sidebar').style.width = `${width}px`; window.__sidebar.setHealth([{ name: 'ada', room_ids: ['rm_api'], flags: [{ code: 'stuck' }] }]); const group = document.querySelector('[data-room-id=rm_ah]'); if (group.classList.contains('collapsed')) group.querySelector(':scope > .room-item > .room-collapse').click(); }, theme, width);
      await page.waitForFunction(() => document.querySelector('[data-room-id=rm_api] > .room-item').getBoundingClientRect().width > 0);
      const lines = await page.evaluate(() => Object.fromEntries(['rm_api', 'rm_ah'].map(id => {
        const row = document.querySelector(`[data-room-id=${id}] > .room-item`), top = row.querySelector('.room-item-name').getBoundingClientRect().top;
        const badges = [...row.querySelectorAll('.room-item-top > :not(.room-item-name)')].filter(node => getComputedStyle(node).display !== 'none' && node.getBoundingClientRect().width > 0);
        const bell = row.querySelector('.room-item-attention');
        return [id, { bell: !!bell, bellHeight: bell ? Math.round(bell.getBoundingClientRect().height) : 0, tops: badges.map(node => Math.round(node.getBoundingClientRect().top - top)), top: Math.round(row.querySelector('.room-item-top').getBoundingClientRect().height), kinds: badges.map(node => node.className) }];
      })));
      for (const [id, line] of Object.entries(lines)) {
        assert.equal(line.bell, true, `${theme} ${width}: ${id} shows the bell`);
        assert.equal(line.tops.every(offset => Math.abs(offset) <= 2), true, `${theme} ${width}: ${id}'s badges share the name's line: ${JSON.stringify(line)}`);
        assert.equal(line.top <= 24, true, `${theme} ${width}: ${id}'s name line is one line high: ${JSON.stringify(line)}`);
      }
      await page.close();
    }
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
    assert.equal(await page.$eval('[data-room-id=rm_old] .room-item', node => node.classList.contains('archived')), false);
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
    // two reads of the chats between agents finish out of order (a join, then a leave and a join again): the older answer is dropped, so a chat that
    // was just joined is not taken out of the list by a read that started before it was
    page = await mount();
    const joined = { ...chats(), rooms: [...chats().rooms, room('rm_join', 'prj_agentdm', 'bert & cleo', { kind: 'dm', agent_chat: true })], joinable: [] };
    await hold(page, 'GET /api/agent-dms');
    await page.evaluate(() => { window.__older = window.__sidebar.refreshAgentChats(); });
    const older = await waitRequest(page, 'GET', '/api/agent-dms');
    await page.evaluate(() => { window.__newer = window.__sidebar.refreshAgentChats(); });
    await page.waitForFunction(() => window.__requests.filter(request => request.method === 'GET' && request.path === '/api/agent-dms' && !request.answered).length === 2);
    const newer = await page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'GET' && request.path === '/api/agent-dms' && !request.answered));
    await answer(page, newer, 200, joined); await page.evaluate(() => window.__newer); await settle(page);
    await answer(page, older, 200, chats()); await page.evaluate(() => window.__older); await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__sidebar.rooms.has('rm_join'), window.__sidebar.joinable.length]), [true, 0], 'an older read of the chats does not take back a chat that a newer one listed');
    await page.close();
    // create
    page = await mount();
    // The + of a project's row makes a room, as the ☰ menu's New room does (the second case below).
    await page.$eval('[data-room-id=rm_ah] > .room-item .add-room', node => node.click());
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
    await projectAct(page, 'rm_ah', 'add');
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
    await projectAct(page, 'rm_ah', 'add');
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
    const onlyBell = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => item.id === 'rm_api' ? { ...item, unread_count: 0, attention_count: 1 } : item) }) });
    assert.equal(await onlyBell.$eval('[data-room-id=rm_api] .room-item-attention', node => node.textContent.trim()), '🔔 1');
    assert.equal(await onlyBell.$eval('[data-room-id=rm_ah] .room-item-attention', node => node.textContent.trim()), '🔔 1', 'the home room includes child attention');
    await onlyBell.evaluate(() => { window.__shown = { rm_api: 40 }; window.__sidebar.setActive('rm_api'); window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_api')); });
    const read = await waitRequest(onlyBell, 'POST', '/api/rooms/rm_api/read');
    await answer(onlyBell, read, 200, { unread_count: 0, read_cursor: 40, attention_count: 0 }); await settle(onlyBell);
    assert.equal(await onlyBell.$('[data-room-id=rm_api] .room-item-attention'), null, 'opening a room clears its seen bell without unread messages');
    assert.equal(await onlyBell.$('[data-room-id=rm_ah] .room-item-attention'), null, 'the project aggregate clears too');
    await onlyBell.close();
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
    const soon = inMs(1200); // still pending when the list is reset (or destroyed) a moment after it is drawn, and over before the wait below ends
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    assert.equal(await page.evaluate(() => Date.parse(window.__sidebar.rooms.get('rm_mute').muted_until) > Date.now()), true, 'the mute is still running when the list is reset, so the wait below is about a pending timer');
    await page.evaluate(() => window.__sidebar.reset());
    await new Promise(resolve => setTimeout(resolve, 1800));
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
    assert.deepEqual(await page.evaluate(() => ['rm_ah', 'rm_bh'].map(id => {
      const row = document.querySelector(`[data-room-id=${id}] > .room-item`);
      return [!!row.querySelector('.project-menu-btn'), !!row.querySelector('.add-room:not([hidden])')];
    })), [[false, true], [false, true]], 'recent sort shows only the new-room action on project rows');
    await page.focus('[data-room-id=rm_ah] .room-item-name'); await page.keyboard.press('Tab');
    assert.deepEqual(await page.evaluate(() => [document.activeElement.classList.contains('add-room'), document.activeElement.getAttribute('aria-label')]), [true, 'New task room under Alpha'], 'keyboard focus reaches the new-room action without a menu button');
    await page.close();
  }

  // The manual order of projects: the move buttons of a home row, the drag of its handle onto another project's row, the end rows disabled, the order
  // kept in the browser and sent to the server, and dragging that is not a project drag changes nothing.
  {
    const page = await mount();
    const order = () => page.$$eval('#projects > .room-group', nodes => nodes.map(node => node.dataset.roomId));
    // Reads the move items of a project's menu: open it, look, close it with the button again.
    const state = async id => page.evaluate(async id => {
      const row = document.querySelector(`[data-room-id=${id}] > .room-item`), button = row.querySelector('.project-menu-btn'); button.click();
      await new Promise(resolve => setTimeout(resolve, 30));
      const result = { up: row.querySelector('.project-menu-up').disabled, down: row.querySelector('.project-menu-down').disabled, add: row.querySelector('.project-menu-add').disabled, label: button.getAttribute('aria-label'), title: button.title, popup: button.getAttribute('aria-haspopup'), expanded: button.getAttribute('aria-expanded') };
      button.click(); await new Promise(resolve => setTimeout(resolve, 30)); result.closed = !row.querySelector('.project-menu-list');
      return result;
    }, id);
    const moves = async () => { const first = await state('rm_ah'), last = await state('rm_bh'); return { upFirst: first.up, downFirst: first.down, downLast: last.down, grip: first.title, label: first.label, popup: first.popup, expanded: first.expanded, closed: first.closed && last.closed }; };
    assert.deepEqual(await moves(), { upFirst: true, downFirst: false, downLast: true, grip: 'Alpha actions (drag to move)', label: 'Alpha actions', popup: 'menu', expanded: 'true', closed: true });
    // The menu is reachable by keyboard: opening it focuses the first item, the arrows skip disabled ones, Escape closes it and hands focus back, and a
    // click elsewhere closes it too.
    const focused = () => page.evaluate(() => document.activeElement.className.replace(/row-btn |project-menu-item /g, ''));
    await page.focus('[data-room-id=rm_ah] .room-item-name'); await page.keyboard.press('Tab'); await page.keyboard.press('Enter'); await settle(page);
    assert.equal(await focused(), 'project-menu-add', 'opening the menu focuses its first item');
    await page.keyboard.press('ArrowDown'); assert.equal(await focused(), 'project-menu-down', 'the arrows skip a disabled item');
    await page.keyboard.press('ArrowDown'); assert.equal(await focused(), 'project-menu-add');
    await page.keyboard.press('Escape'); await settle(page);
    assert.deepEqual(await page.evaluate(() => [!!document.querySelector('.project-menu-list'), document.activeElement.classList.contains('project-menu-btn')]), [false, true], 'Escape closes the menu and returns focus to its button');
    await page.focus('[data-room-id=rm_ah] .room-item-name'); await page.keyboard.press('Tab'); await page.keyboard.press('Enter'); await settle(page);
    await page.$eval('#projects', node => node.click()); await settle(page);
    assert.equal(await page.evaluate(() => !!document.querySelector('.project-menu-list')), false, 'a click elsewhere closes the menu');
    await projectAct(page, 'rm_ah', 'down');
    assert.deepEqual(await order(), ['rm_bh', 'rm_ah']);
    assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.ui.sidebar_project_order.usr_me')), '["prj_b","prj_a","prj_c"]');
    const saved = await waitRequest(page, 'PUT', '/api/me/preferences');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, saved), { preferences: { sidebar_project_order: '["prj_b","prj_a","prj_c"]' } });
    await projectAct(page, 'rm_ah', 'up');
    assert.deepEqual(await order(), ['rm_ah', 'rm_bh']);
    const drag = (type, target, y) => page.evaluate((type, target, y) => { const row = document.querySelector(`[data-room-id=${target}] > .room-item`); const box = row.getBoundingClientRect(); window.__transfer ||= new DataTransfer();
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
      manual: page => projectAct(page, 'rm_ah', 'down'),
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
    for (const [what, move] of [['manual', page => projectAct(page, 'rm_ah', 'down')], ['sort', async page => { await page.$eval('#rooms-sort', node => node.click()); await page.$eval('.sort-item[data-sort=name]', node => node.click()); }]]) {
      const page = await mount();
      await hold(page, 'PUT /api/me/preferences');
      await move(page);
      const first = await waitRequest(page, 'PUT', '/api/me/preferences');
      if (what === 'manual') await projectAct(page, 'rm_bh', 'down');
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
  for (const [what, first, second] of [['manual', page => projectAct(page, 'rm_ah', 'down'), page => projectAct(page, 'rm_bh', 'down')],
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
    await projectAct(page, 'rm_ah', 'add');
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
    await projectAct(page, 'rm_ah', 'add');
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
    const soon = inMs(1200); // still pending when the list is reset (or destroyed) a moment after it is drawn, and over before the wait below ends
    page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': rooms().prj_a.map(item => (item.id === 'rm_mute' ? { ...item, muted_until: soon } : item)) }) });
    assert.equal(await page.evaluate(() => Date.parse(window.__sidebar.rooms.get('rm_mute').muted_until) > Date.now()), true, 'the mute is still running when the list is destroyed, so the wait below is about a pending timer');
    await destroy(page);
    await new Promise(resolve => setTimeout(resolve, 1800));
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
    const move = (page, id, direction) => projectAct(page, id, direction);
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
    await projectAct(page, 'rm_ah', 'down');
    const first = await waitRequest(page, 'PUT', '/api/me/preferences');
    await projectAct(page, 'rm_bh', 'down');
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

  // The rooms column keeps its header row and the filter where they are: only the list under them scrolls (the column itself does not).
  {
    const many = Array.from({ length: 60 }, (_, i) => project(`prj_p${i}`, `Project ${i}`));
    const extra = { 'GET /api/projects': many, ...Object.fromEntries(many.map((item, i) => [`GET /api/projects/${item.id}/rooms`, [room(`rm_p${i}`, item.id, `Project ${i}`, { kind: 'home' })]])) };
    const page = await mount({ auto: answerAll(extra) });
    await page.setViewport({ width: 1280, height: 600 });
    const geometry = () => page.evaluate(() => {
      const side = document.querySelector('.sidebar'), list = document.querySelector('#projects'), top = selector => Math.round(document.querySelector(selector).getBoundingClientRect().top);
      return { header: top('.sidebar > header'), filter: top('.room-filter'), list: top('#projects'), listScrolls: list.scrollHeight > list.clientHeight + 1, columnScrolls: side.scrollHeight > side.clientHeight + 1, scrollTop: list.scrollTop, overflow: getComputedStyle(side).overflowY };
    });
    const before = await geometry();
    assert.deepEqual([before.listScrolls, before.columnScrolls, before.overflow], [true, false, 'hidden'], 'the list is longer than the column and is what scrolls');
    await page.$eval('#projects', node => { node.scrollTop = 700; }); await settle(page);
    const after = await geometry();
    assert.ok(after.scrollTop > 300, `the list scrolled: ${after.scrollTop}`);
    assert.deepEqual([after.header, after.filter, after.list, after.columnScrolls], [before.header, before.filter, before.list, false], 'the header and the filter stayed where they were while the list scrolled');
    // the phone's drawer is the same column: the header and the filter hold there too
    await page.setViewport({ width: 390, height: 700 });
    await page.evaluate(() => { document.querySelector('.app').classList.add('chat-open', 'rooms-open'); }); await settle(page);
    await page.$eval('#projects', node => { node.scrollTop = 0; }); await settle(page);
    const phone = await geometry();
    await page.$eval('#projects', node => { node.scrollTop = 700; }); await settle(page);
    const phoneAfter = await geometry();
    assert.deepEqual([phone.listScrolls, phoneAfter.scrollTop > 300, phoneAfter.header, phoneAfter.filter], [true, true, phone.header, phone.filter], 'the phone drawer keeps its header and filter too');
    await page.close();
  }

  // Room kinds and task-room status: an icon (a mask with a tooltip, never text) per kind; a task or research room has no grey member count and no
  // blue count of everything unread but a count of what addresses the person and a small status; other rooms keep their counts.
  {
    const kindRooms = [room('rm_kh', 'prj_a', 'Alpha', { kind: 'home', purpose: 'task', member_count: 3, unread_count: 2 }),
      room('rm_task', 'prj_a', 'build', { parent_id: 'rm_kh', purpose: 'task', unread_count: 40, unread_addressed: 0, member_count: 4 }),
      room('rm_taskn', 'prj_a', 'build-addressed', { parent_id: 'rm_kh', purpose: 'task', unread_count: 40, unread_addressed: 3 }),
      room('rm_work', 'prj_a', 'working', { parent_id: 'rm_kh', purpose: 'task', working: true }),
      room('rm_wait', 'prj_a', 'waiting', { parent_id: 'rm_kh', purpose: 'task', working: true, attention_open: 1, attention_count: 0 }),
      room('rm_bell', 'prj_a', 'belled', { parent_id: 'rm_kh', purpose: 'task', attention_open: 2, attention_count: 2 }),
      room('rm_close', 'prj_a', 'closing', { parent_id: 'rm_kh', purpose: 'task', lifecycle: 'closing' }),
      room('rm_res', 'prj_a', 'compare', { parent_id: 'rm_kh', purpose: 'research', user_message_count: 2, unread_count: 9 }),
      room('rm_resnew', 'prj_a', 'compare-new', { parent_id: 'rm_kh', purpose: 'research' }),
      room('rm_gen2', 'prj_a', 'general', { parent_id: 'rm_kh', purpose: 'general', unread_count: 6, working: true, member_count: 5 }),
      room('rm_route', 'prj_a', 'routing', { parent_id: 'rm_kh', purpose: 'general', restricted: true, team_flow_room_key: 'routing' }),
      room('rm_dm2', 'prj_a', 'ada', { kind: 'dm', purpose: 'task', unread_count: 2, member_count: 2 })];
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': kindRooms, 'GET /api/projects/prj_b/rooms': [], 'GET /api/projects/prj_c/rooms': [], 'GET /api/agent-dms': { project_id: 'prj_agentdm', rooms: [room('rm_chat2', 'prj_agentdm', 'ada & bert', { kind: 'dm', agent_chat: true, unread_count: 1 })], joinable: [] } }) });
    await page.setViewport({ width: 1280, height: 700 }); await settle(page);
    const view = id => page.$eval(`[data-room-id=${id}] > .room-item`, row => {
      const icon = row.querySelector('.room-item-icon'), mask = icon.querySelector('.icon-mask'), status = row.querySelector('.room-status');
      return { type: icon.dataset.roomType, mask: mask?.className.replace('icon-mask ', ''), tooltip: icon.title, label: icon.getAttribute('aria-label'), text: icon.textContent.trim(), image: mask ? getComputedStyle(mask).webkitMaskImage.replace(/^url\("?|"?\)$/g, '').replace(/^.*\/icons\//, '') : '',
        status: status ? [status.textContent, status.className.replace('room-state room-status room-status-', ''), status.title] : null, count: !!row.querySelector('.room-item-count'), unread: row.querySelector('.room-item-unread')?.textContent || '',
        bell: row.querySelector('.room-item-attention')?.textContent || '', name: row.querySelector('.room-item-name').title };
    });
    const kinds = {};
    for (const id of ['rm_kh', 'rm_task', 'rm_res', 'rm_gen2', 'rm_route', 'rm_dm2', 'rm_chat2']) kinds[id] = await view(id);
    assert.deepEqual(Object.entries(kinds).map(([id, item]) => [id, item.type, item.mask, item.image, item.tooltip, item.label, item.text]), [
      ['rm_kh', 'home', 'icon-mask-house', 'house.svg', 'Project home room', 'Project home room', ''],
      ['rm_task', 'task', 'icon-mask-list-checks', 'list-checks.svg', 'Task room', 'Task room', ''],
      ['rm_res', 'research', 'icon-mask-search', 'search.svg', 'Research room', 'Research room', ''],
      ['rm_gen2', 'general', 'icon-mask-hash', 'hash.svg', 'General room', 'General room', ''],
      ['rm_route', 'routing', 'icon-mask-route', 'route.svg', 'Routing room', 'Routing room', ''],
      ['rm_dm2', 'dm', 'icon-mask-human', 'human.svg', 'Direct message', 'Direct message', ''],
      ['rm_chat2', 'agent-chat', 'icon-mask-switch', 'switch.svg', 'Chat between agents', 'Chat between agents', '']], 'every kind of room has its own SVG icon with a tooltip and no text glyph');
    // Task rooms: no grey count, and the unread chip counts only what addresses the person (so chatter alone leaves none).
    const task = kinds.rm_task, addressed = await view('rm_taskn');
    assert.deepEqual([task.count, task.unread, task.name], [false, '', 'build · 4 members'], 'a task room hides its member count (it is in the tooltip) and shows no count for chatter');
    assert.deepEqual([addressed.count, addressed.unread], [false, '3'], 'a task room still counts what addresses the person');
    assert.equal(await page.$eval('[data-room-id=rm_taskn] .room-item-unread', node => node.title), '3 unread messages');
    assert.deepEqual([kinds.rm_res.count, kinds.rm_res.unread, kinds.rm_res.name], [false, '', 'compare · 2 members'], 'a research room is told like a task room');
    // General and coordination rooms, the home room and direct messages keep the member count and the count of everything unread.
    assert.deepEqual([kinds.rm_gen2.count, kinds.rm_gen2.unread, kinds.rm_gen2.status], [true, '6', null], 'a general room keeps its counts and has no status (an agent working does not change that)');
    assert.deepEqual([kinds.rm_kh.count, kinds.rm_kh.unread, kinds.rm_kh.status], [true, '11', null], 'the folded home keeps its member count and totals its own 2, the 3 addressed to the person in a task room (not its 40) and the 6 of the general room');
    assert.deepEqual([kinds.rm_dm2.count, kinds.rm_dm2.unread, kinds.rm_dm2.status], [true, '2', null]);
    assert.deepEqual([kinds.rm_chat2.count, kinds.rm_chat2.unread], [true, '1']);
    // The status.
    const statuses = {};
    for (const id of ['rm_task', 'rm_work', 'rm_wait', 'rm_bell', 'rm_close', 'rm_res', 'rm_resnew']) statuses[id] = (await view(id)).status;
    assert.deepEqual(statuses, {
      rm_task: ['Idle', 'idle', 'Nothing is running in this room'], rm_work: ['Working', 'working', 'An agent in this room is working'],
      rm_wait: ['Waiting', 'waiting', 'Waiting for a person: an item needs an answer'], rm_bell: ['Waiting', 'waiting', 'Waiting for a person: an item needs an answer'],
      rm_close: ['Closing…', 'closing', 'This room is closing'], rm_res: ['Done', 'done', 'Quiet: no agent is working here'], rm_resnew: ['Idle', 'idle', 'Nothing is running in this room'] });
    // The warning and bell badges stay.
    assert.equal((await view('rm_bell')).bell, '🔔 2', 'the bell badge stays beside the status');
    assert.equal(await page.$eval('[data-room-id=rm_work] .room-health, [data-room-id=rm_bell] .room-item-attention', node => !!node), true);
    // A status is a bordered word in the current colour, legible in every theme; high contrast has no colour left to carry it.
    await page.evaluate(() => document.querySelectorAll('.room-group.collapsed').forEach(group => group.classList.remove('collapsed'))); // nested rows of a folded home are not drawn
    for (const theme of ['dark', 'light', 'high-contrast-dark', 'high-contrast-light']) {
      await page.evaluate(theme => document.documentElement.setAttribute('data-theme', theme), theme);
      const look = await page.evaluate(() => [...document.querySelectorAll('.room-status')].map(node => { const style = getComputedStyle(node); return [node.classList[2], style.borderTopWidth, style.borderTopStyle, style.color, style.backgroundColor, node.getBoundingClientRect().width > 0]; }));
      assert.ok(look.length >= 6 && look.every(item => item[1] === '1px' && item[2] === 'solid' && item[5]), `${theme}: every status is a drawn, bordered word ${JSON.stringify(look)}`);
      if (theme.startsWith('high-contrast')) {
        const [text, back] = await page.evaluate(() => [getComputedStyle(document.body).color, getComputedStyle(document.body).backgroundColor]);
        const hc = Object.fromEntries(look.map(item => [item[0], [item[3], item[4]]]));
        assert.deepEqual(hc['room-status-working'], [text, back], `${theme}: working is the page's text on the page's background`);
        assert.deepEqual(hc['room-status-waiting'], [text, back], `${theme}: waiting is the page's text on the page's background`);
      } else {
        const colours = await page.evaluate(() => Object.fromEntries([...document.querySelectorAll('.room-status')].map(node => [node.classList[2], getComputedStyle(node).color])));
        assert.notEqual(colours['room-status-working'], colours['room-status-idle'], `${theme}: working reads apart from idle`);
        assert.notEqual(colours['room-status-waiting'], colours['room-status-idle'], `${theme}: waiting reads apart from idle`);
      }
    }
    await page.evaluate(() => document.documentElement.removeAttribute('data-theme'));
    // The status follows the server: a refreshed list that says an agent started working changes the word.
    await page.evaluate(() => { const room = window.__sidebar.rooms.get('rm_task'); room.working = true; window.__sidebar.render(); }); await settle(page);
    assert.equal((await view('rm_task')).status[0], 'Working', 'the status redraws when the room says an agent is working');
    await page.close();
  }

  // Marking a task room read keeps its addressed count in step with the server's answer.
  {
    const page = await mount({ auto: answerAll({ 'GET /api/projects/prj_a/rooms': [room('rm_kh', 'prj_a', 'Alpha', { kind: 'home', purpose: 'task' }), room('rm_t', 'prj_a', 'task', { parent_id: 'rm_kh', purpose: 'task', unread_count: 6, unread_addressed: 2 })] }) });
    await settle(page);
    assert.equal(await page.$eval('[data-room-id=rm_t] .room-item-unread', node => node.textContent), '2');
    await page.evaluate(() => { window.__shown = { rm_t: 40 }; window.__sidebar.setActive('rm_t'); window.__sidebar.markSeen(window.__sidebar.rooms.get('rm_t')); });
    const mark = await waitRequest(page, 'POST', '/api/rooms/rm_t/read');
    await answer(page, mark, 200, { unread_count: 1, unread_addressed: 0, read_cursor: 40, attention_count: 0 }); await settle(page);
    assert.deepEqual(await page.evaluate(() => { const room = window.__sidebar.rooms.get('rm_t'); return [room.unread_count, room.unread_addressed]; }), [1, 0]);
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('sidebar page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
