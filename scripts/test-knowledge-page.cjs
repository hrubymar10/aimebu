// The Memory viewer (project facts, user profiles, shared notes, task notes) in Chromium with every request held until the test answers it
// or answered by rule. web/ is served as is with its import map; no Go server. It runs unchanged against the page that replaced the
// hand-written viewer. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const KNOWLEDGE = fs.existsSync(path.join(root, 'js', 'pages', 'knowledge', 'page.js')) ? '/js/pages/knowledge/page.js' : '/js/knowledge.js';
const indexHtml = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(indexHtml)[0];
const dialog = /<dialog id="knowledge-dialog".*?<\/dialog>/s.exec(indexHtml)[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><button id="knowledge-toggle" type="button">Memory</button><button id="task-notes-toggle" type="button" hidden>Notes</button>${dialog}</body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const record = (id, body, extra = {}) => ({ id, body, rev: 2, scope_id: 'p1', author_kind: 'user', author_id: 'usr_1', updated_at: '2020-01-01T00:00:00Z', active: true, ...extra });
const PROJECT = '/api/projects/p1/memory', OTHER = '/api/projects/p2/memory', PROFILE = '/api/global-memory/user_profile', SHARED = '/api/global-memory/agent_shared_notes';
const base = () => ({
  [`GET ${PROJECT}`]: { records: [record('k1', 'Deploy with ops/deploy.sh'), record('k2', 'Small commits')], enabled: true, can_edit: true, cap_bytes: 4096 },
  [`GET ${OTHER}`]: { records: [], enabled: false, can_edit: true, cap_bytes: 4096 },
  [`GET ${PROFILE}`]: { records: [record('g1', 'Answers in Czech', { scope_id: 'usr_1', scope_label: 'owner' })], can_edit: true, cap_bytes: 2048 },
  [`GET ${SHARED}`]: { records: [record('g2', 'Use the staging bucket', { scope_id: 'global', active: false })], can_edit: false, cap_bytes: 2048 },
});

function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path.split('?')[0]}`];
    if (rule) request.answer(rule.status || 200, rule.status ? rule.body : rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async ({ auto = base(), projects = [{ id: 'p1', name: 'Alpha' }, { id: 'p2', name: 'Beta' }], room = { id: 'r1', kind: 'task', title: 'Fix login', project_id: 'p1' }, selected = 'p1' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ projects, room, selected, module }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: true, admin: true });
      const { KnowledgePanel } = await import(module);
      window.__room = room; window.__dialogs = [];
      window.__knowledge = new KnowledgePanel(() => ({ projects, selectedProjectID: selected }), () => window.__room);
      window.__knowledge.setRoom(room);
    }, { projects, room, selected, module: KNOWLEDGE });
    return page;
  };
  const view = page => page.evaluate(() => {
    const d = document.querySelector('#knowledge-dialog'), q = s => d.querySelector(s);
    return { open: d.open, title: q('#knowledge-title').textContent, scope: [q('#knowledge-scope-label').hidden, q('#knowledge-scope').value], project: [q('#knowledge-project-label').hidden, q('#knowledge-project-label').firstChild.textContent, q('#knowledge-project').value],
      enabled: [q('#knowledge-enabled-label').hidden, q('#knowledge-enabled').checked, q('#knowledge-enabled').disabled], status: q('#knowledge-status').textContent, add: [q('#knowledge-add').hidden, q('#knowledge-scope-key-label').hidden],
      clean: [q('.knowledge-actions:not(.knowledge-record .knowledge-actions)')?.hidden, q('#knowledge-clean')?.textContent, q('#knowledge-clean')?.disabled], error: q('#knowledge-error').textContent,
      records: [...d.querySelectorAll('.knowledge-record')].map(card => [card.querySelector('small').textContent.replace(/ · [^·]*$/, ''), card.querySelector('textarea').value, card.querySelector('textarea').readOnly, [...card.querySelectorAll('.knowledge-actions button')].map(b => b.title || b.textContent)]),
      empty: q('.knowledge-records > p')?.textContent ?? null, usage: [...d.querySelectorAll('.knowledge-usage-row')].map(row => [row.querySelector('.knowledge-usage-meta').textContent, row.querySelector('[role=progressbar]').getAttribute('aria-valuenow'), row.querySelector('[role=progressbar] span').style.width]) };
  });
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const calls = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).map(request => [request.path, request.body]), method);
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path.split('?')[0] === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path.split('?')[0] === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const opened = async (page, scope) => { await page.$eval('#knowledge-toggle', el => el.click()); await page.waitForFunction(() => document.querySelector('#knowledge-dialog').open && !/Loading/.test(document.querySelector('#knowledge-status').textContent) && document.querySelector('#knowledge-status').textContent); if (scope) { await page.select('#knowledge-scope', scope); await page.waitForFunction(() => !/Loading/.test(document.querySelector('#knowledge-status').textContent)); await settle(page); } };
  const confirmOk = async page => { await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.$eval('#app-dialog .app-dialog-ok', el => el.click()); };
  try {
    // The viewer opens on everything visible: one read per scope, the status line, usage per scope, records read-only where the person cannot edit.
    {
      const page = await mount();
      await opened(page);
      const all = await view(page);
      assert.deepEqual({ ...all, records: all.records.map(([meta, text, readOnly, buttons]) => [meta, text, readOnly, buttons]) }, {
        open: true, title: 'All visible memory', scope: [false, 'all'], project: [false, 'Project facts from', 'p1'], enabled: [true, true, false],
        status: '4 records: 2 project facts of Alpha, 1 user profiles, 1 shared notes. Other projects\' facts are in the Project facts view.', add: [true, true], clean: [false, 'Clear all', false],
        error: '', empty: null,
        records: [['Project facts · Revision 2 · user usr_1', 'Deploy with ops/deploy.sh', false, ['Save', 'Delete']], ['Project facts · Revision 2 · user usr_1', 'Small commits', false, ['Save', 'Delete']],
          ['User profile owner · Revision 2 · user usr_1', 'Answers in Czech', false, ['Save', 'Delete', 'Deactivate']], ['Shared agent notes · Inactive · Revision 2 · user usr_1', 'Use the staging bucket', true, []]],
        usage: [['Project facts · Alpha · 38 / 4096 bytes · 2 records', '38', '1%'], ['User profile · owner · 16 / 2048 bytes · 1 record', '16', '1%'], ['Shared agent notes · 22 / 2048 bytes · 1 record', '22', '1%']] });
      assert.deepEqual((await calls(page, 'GET')).map(([p]) => p).sort(), [PROJECT, PROFILE, SHARED].sort());
      assert.deepEqual(await page.$$eval('#knowledge-project option', options => options.map(o => [o.value, o.textContent])), [['p1', 'Alpha'], ['p2', 'Beta']]);
      await page.$eval('#knowledge-close', el => el.click());
      assert.equal(await page.$eval('#knowledge-dialog', el => el.open), false);
      await page.close();
    }
    // The scopes: project facts (with the memory switch), user profiles (with the user box) and shared notes; each reads its own path.
    {
      const page = await mount({ auto: { ...base(), [`PUT ${PROJECT}-enabled`]: {}, 'PUT /api/projects/p1/memory-enabled': {} } });
      await opened(page, 'project');
      const project = await view(page);
      assert.deepEqual([project.title, project.scope, project.project, project.enabled, project.status, project.add, project.clean], ['Project facts', [false, 'project'], [false, 'Project', 'p1'], [false, true, false], '38 / 4096 bytes · 2 records', [false, true], [false, 'Clear filtered', false]]);
      await page.$eval('#knowledge-enabled', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT'));
      assert.deepEqual((await calls(page, 'PUT'))[0], ['/api/projects/p1/memory-enabled', { enabled: false }]);
      await page.select('#knowledge-scope', 'user_profile');
      await page.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'User profiles' && !/Loading/.test(document.querySelector('#knowledge-status').textContent));
      await settle(page);
      const profile = await view(page);
      assert.deepEqual([profile.project[0], profile.enabled[0], profile.add, profile.status], [true, true, [false, false], '1 records · 1 profiles · 2048 bytes per profile']);
      assert.equal(profile.records[0][0], 'owner · 16 / 2048 bytes · Revision 2 · user usr_1');
      await page.select('#knowledge-scope', 'agent_shared_notes');
      await page.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'Shared agent notes' && !/Loading/.test(document.querySelector('#knowledge-status').textContent));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#knowledge-add').hidden, document.querySelector('#knowledge-status').textContent]), [true, '22 / 2048 bytes · 1 records']);
      await page.close();
    }
    // Records: add, save, delete (asked first, with the revision), the activation of a global one, and clearing the view one request each.
    {
      const page = await mount({ auto: { ...base(), [`POST ${PROJECT}`]: {}, [`PUT ${PROJECT}/k1`]: {}, [`DELETE ${PROJECT}/k2`]: {}, [`PUT ${PROFILE}/g1/activation`]: {}, [`DELETE ${PROFILE}/g1`]: {} } });
      await opened(page, 'project');
      await page.evaluate(() => { const form = document.querySelector('#knowledge-add'); form.elements.body.value = 'A new fact'; form.requestSubmit(); });
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'POST'));
      assert.deepEqual((await calls(page, 'POST'))[0], [PROJECT, { body: 'A new fact' }]);
      await page.waitForFunction(() => document.querySelector('#knowledge-add').elements.body.value === '');
      await page.$eval('.knowledge-record:first-child textarea', el => { el.value = 'Edited'; });
      await page.$eval('.knowledge-record:first-child .btn-primary', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT'));
      assert.deepEqual((await calls(page, 'PUT'))[0], [`${PROJECT}/k1`, { body: 'Edited', rev: 2, scope_key: 'p1' }]);
      await page.$eval('.knowledge-record:nth-child(2) .btn-danger', el => el.click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Delete this record?'));
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'DELETE'));
      assert.deepEqual((await calls(page, 'DELETE'))[0], [`${PROJECT}/k2?rev=2&scope_key=p1`, null]);
      await page.select('#knowledge-scope', 'user_profile');
      await page.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'User profiles' && document.querySelector('.knowledge-record'));
      await page.evaluate(() => [...document.querySelectorAll('.knowledge-record .knowledge-actions button')].find(b => b.title === 'Deactivate').click());
      await page.waitForFunction(() => window.__requests.some(request => request.path.endsWith('/activation')));
      assert.deepEqual((await calls(page, 'PUT')).at(-1), [`${PROFILE}/g1/activation`, { scope_key: 'usr_1', rev: 2, active: false }]);
      await page.$eval('#knowledge-clean', el => el.click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Delete 1 of the records in this view? This cannot be undone.'));
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.filter(request => request.method === 'DELETE').length === 2);
      assert.equal((await calls(page, 'DELETE')).at(-1)[0], `${PROFILE}/g1?rev=2&scope_key=usr_1`);
      await page.close();
      const failing = await mount({ auto: { ...base(), [`DELETE ${PROJECT}/k1`]: {}, [`DELETE ${PROJECT}/k2`]: { status: 409, body: { message: 'It changed' } }, [`PUT ${PROJECT}/k1`]: { status: 409, body: { message: 'Revision is stale' } }, [`POST ${PROJECT}`]: { status: 400, body: { message: 'Too long' } } } });
      await opened(failing, 'project');
      await failing.evaluate(() => { const form = document.querySelector('#knowledge-add'); form.elements.body.value = 'x'; form.requestSubmit(); });
      await failing.waitForFunction(() => document.querySelector('#knowledge-error').textContent === 'Too long');
      await failing.$eval('.knowledge-record:first-child .btn-primary', el => el.click());
      await failing.waitForFunction(() => document.querySelector('#knowledge-error').textContent === 'Revision is stale');
      await failing.$eval('#knowledge-clean', el => el.click());
      await confirmOk(failing);
      await failing.waitForFunction(() => window.__requests.filter(request => request.method === 'DELETE').length === 2);
      await settle(failing);
      assert.deepEqual((await calls(failing, 'DELETE')).map(([p]) => p), [`${PROJECT}/k1?rev=2&scope_key=p1`, `${PROJECT}/k2?rev=2&scope_key=p1`], 'each record is one request, and a refusal does not stop the list being read again');
      await failing.close();
    }
    // A read that is answered late: the newer view wins and the late answer draws nothing.
    {
      const page = await mount({ auto: {} });
      await page.$eval('#knowledge-toggle', el => el.click());
      await page.waitForFunction(() => window.__requests.length === 3);
      await page.evaluate(() => { document.querySelector('#knowledge-scope').value = 'project'; document.querySelector('#knowledge-scope').dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => window.__requests.length === 4);
      await answer(page, 3, 200, base()[`GET ${PROJECT}`]);
      await page.waitForFunction(() => /38 \/ 4096/.test(document.querySelector('#knowledge-status').textContent));
      await answer(page, 0, 200, { records: [record('old', 'Late answer')], enabled: true, can_edit: true, cap_bytes: 4096 });
      await answer(page, 1, 200, base()[`GET ${PROFILE}`]); await answer(page, 2, 200, base()[`GET ${SHARED}`]);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#knowledge-title').textContent, [...document.querySelectorAll('.knowledge-record textarea')].map(t => t.value)]), ['Project facts', ['Deploy with ops/deploy.sh', 'Small commits']], 'a view the person left does not draw');
      await page.evaluate(other => { window.__auto['GET /api/projects/p2/memory'] = other; }, base()[`GET ${OTHER}`]);
      await page.select('#knowledge-project', 'p2');
      await page.waitForFunction(() => /0 \/ 4096/.test(document.querySelector('#knowledge-status').textContent));
      assert.equal(await page.$eval('#knowledge-status', el => el.textContent), '0 / 4096 bytes · 0 records · Memory is off for Beta: agents do not get its project facts, and saved records stay available.');
      assert.equal(await page.$eval('.knowledge-records > p', el => el.textContent), 'No project facts saved.');
      await page.close();
    }
    // An event for what is on screen reads it again and keeps the records; an event for something else, or with the viewer closed, does nothing.
    {
      const page = await mount();
      await opened(page, 'project');
      const before = (await calls(page, 'GET')).length;
      await page.evaluate(() => { const k = window.__knowledge; k.onEvent({ kind: 'knowledge_created', project_id: 'p2' }); k.onEvent({ kind: 'message_created', project_id: 'p1' }); k.onEvent({ kind: 'global_knowledge_created' }); });
      await settle(page);
      assert.equal((await calls(page, 'GET')).length, before, 'events about other things send no read');
      await page.evaluate(() => { window.__auto[`GET /api/projects/p1/memory`] = { records: [{ id: 'k9', body: 'Fresh', rev: 1, scope_id: 'p1', author_kind: 'user', author_id: 'u', updated_at: '2020-01-01T00:00:00Z' }], enabled: true, can_edit: true, cap_bytes: 4096 }; window.__knowledge.onEvent({ kind: 'knowledge_created', project_id: 'p1' }); });
      await page.waitForFunction(() => document.querySelector('.knowledge-record textarea')?.value === 'Fresh');
      await page.$eval('#knowledge-close', el => el.click());
      const closed = (await calls(page, 'GET')).length;
      await page.evaluate(() => window.__knowledge.onEvent({ kind: 'knowledge_created', project_id: 'p1' }));
      await settle(page);
      assert.equal((await calls(page, 'GET')).length, closed, 'a closed viewer reads nothing');
      await page.close();
    }
    // Task notes and the project's own facts open from the room's button; a room that is neither has no button; no project says so.
    {
      const page = await mount({ auto: { ...base(), 'GET /api/rooms/r1/task-notes': { records: [record('t1', 'Check the redirect', { scope_id: 'r1' })], can_edit: true, cap_bytes: 1000 } } });
      assert.deepEqual(await page.evaluate(() => { const b = document.querySelector('#task-notes-toggle'); return [b.hidden, b.title, b.getAttribute('aria-label')]; }), [false, 'Task notes', 'Task notes']);
      await page.$eval('#task-notes-toggle', el => el.click());
      await page.waitForFunction(() => document.querySelector('.knowledge-record'));
      const task = await view(page);
      assert.deepEqual([task.title, task.scope[0], task.project[0], task.enabled[0], task.status, task.add[0], task.records.map(r => r[1])], ['Notes · Fix login', true, true, true, '18 / 1000 bytes · 1 records', false, ['Check the redirect']]);
      await page.$eval('#knowledge-close', el => el.click());
      await page.evaluate(() => { window.__room = { id: 'h', kind: 'home', project_id: 'p1' }; window.__knowledge.setRoom(window.__room); });
      assert.deepEqual(await page.evaluate(() => { const b = document.querySelector('#task-notes-toggle'); return [b.hidden, b.title]; }), [false, 'Project memory']);
      await page.$eval('#task-notes-toggle', el => el.click());
      await page.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'Project facts' && document.querySelector('.knowledge-record'));
      await page.$eval('#knowledge-close', el => el.click());
      await page.evaluate(() => { window.__knowledge.setRoom({ id: 'd', kind: 'dm' }); });
      assert.equal(await page.$eval('#task-notes-toggle', el => el.hidden), true);
      await page.close();
      const none = await mount({ projects: [], selected: '' });
      await none.$eval('#knowledge-toggle', el => el.click());
      await none.waitForFunction(() => document.querySelector('#knowledge-status').textContent === 'Choose a project.');
      assert.equal(await none.$eval('#knowledge-add', el => el.hidden), true);
      await none.close();
    }
    // A read that fails says why and hides the form.
    {
      const page = await mount({ auto: { ...base(), [`GET ${PROJECT}`]: { status: 500, body: { message: 'The store is down' } } } });
      await opened(page, 'project').catch(() => {});
      await page.waitForFunction(() => document.querySelector('#knowledge-error').textContent === 'The store is down');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#knowledge-status').textContent, document.querySelector('#knowledge-add').hidden]), ['', true]);
      await page.close();
    }
    // What was begun on a view the person has left does not touch the view they are on; a destroyed viewer sends and draws nothing.
    {
      const page = await mount();
      await opened(page, 'project');
      await page.$eval('#knowledge-enabled', el => el.click());
      const toggle = await waitRequest(page, 'PUT', '/api/projects/p1/memory-enabled');
      await page.select('#knowledge-project', 'p2');
      await page.waitForFunction(() => /0 \/ 4096/.test(document.querySelector('#knowledge-status').textContent));
      const reads = (await calls(page, 'GET')).length;
      await answer(page, toggle, 500, { message: 'Could not save' });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#knowledge-error').textContent, document.querySelector('#knowledge-project').value]), ['', 'p2'], 'a refusal for the project that was left is not shown on the new one');
      assert.equal((await calls(page, 'GET')).length, reads, 'and nothing is read again for it');
      await page.close();
      const clearing = await mount({ auto: { ...base(), [`DELETE ${PROJECT}/k2`]: {} } });
      await opened(clearing, 'project');
      await clearing.$eval('#knowledge-clean', el => el.click());
      await confirmOk(clearing);
      const first = await waitRequest(clearing, 'DELETE', `${PROJECT}/k1`);
      await clearing.select('#knowledge-scope', 'user_profile');
      await answer(clearing, first, 200, {});
      await settle(clearing);
      assert.deepEqual((await calls(clearing, 'DELETE')).map(([p]) => p.split('?')[0]), [`${PROJECT}/k1`], 'clearing stops once the person has left the view');
      await clearing.close();
      const asking = await mount();
      await opened(asking, 'project');
      await asking.$eval('.knowledge-record:first-child .btn-danger', el => el.click());
      await asking.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await asking.evaluate(() => { document.querySelector('#knowledge-scope').value = 'user_profile'; document.querySelector('#knowledge-scope').dispatchEvent(new Event('change', { bubbles: true })); });
      await asking.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await settle(asking);
      assert.equal((await calls(asking, 'DELETE')).length, 0, 'a deletion confirmed after the view changed sends nothing');
      await asking.close();
      const adding = await mount({ auto: { ...base() } });
      await opened(adding, 'project');
      await adding.evaluate(() => { const form = document.querySelector('#knowledge-add'); form.elements.body.value = 'Old draft'; form.requestSubmit(); });
      const post = await waitRequest(adding, 'POST', PROJECT);
      await adding.select('#knowledge-scope', 'user_profile');
      await adding.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'User profiles' && document.querySelector('.knowledge-record'));
      await adding.evaluate(() => { document.querySelector('#knowledge-add').elements.body.value = 'New draft'; });
      const readsBefore = (await calls(adding, 'GET')).length;
      await answer(adding, post, 200, {});
      await settle(adding);
      assert.deepEqual(await adding.evaluate(() => document.querySelector('#knowledge-add').elements.body.value), 'New draft', 'an add answered after the view changed does not clear the new view\'s draft');
      assert.equal((await calls(adding, 'GET')).length, readsBefore, 'and reads nothing for it');
      await adding.close();
      const sweeping = await mount({ auto: { ...base() } });
      await opened(sweeping, 'project');
      await sweeping.$eval('#knowledge-clean', el => el.click());
      await confirmOk(sweeping);
      const del = await waitRequest(sweeping, 'DELETE', `${PROJECT}/k1`);
      await sweeping.select('#knowledge-scope', 'user_profile');
      await sweeping.waitForFunction(() => document.querySelector('#knowledge-title').textContent === 'User profiles' && document.querySelector('.knowledge-record'));
      const sweepReads = (await calls(sweeping, 'GET')).length;
      await answer(sweeping, del, 200, {});
      await settle(sweeping);
      assert.equal((await calls(sweeping, 'GET')).length, sweepReads, 'a clear finishing after the view changed reads nothing for the new view');
      assert.equal(await sweeping.$eval('#knowledge-clean', el => el.disabled), false, 'and does not leave the new view locked');
      await sweeping.close();
      const toggles = await mount();
      await opened(toggles, 'project');
      await toggles.$eval('#knowledge-enabled', el => el.click());
      const older = await waitRequest(toggles, 'PUT', '/api/projects/p1/memory-enabled');
      await toggles.select('#knowledge-project', 'p2');
      await toggles.waitForFunction(() => /0 \/ 4096/.test(document.querySelector('#knowledge-status').textContent));
      await toggles.evaluate(() => { document.querySelector('#knowledge-enabled').disabled = false; });
      await toggles.$eval('#knowledge-enabled', el => el.click());
      const newer = await waitRequest(toggles, 'PUT', '/api/projects/p2/memory-enabled');
      await answer(toggles, older, 200, {});
      await settle(toggles);
      assert.equal(await toggles.$eval('#knowledge-enabled', el => el.disabled), true, 'the older toggle finishing does not unlock the newer one');
      await answer(toggles, newer, 200, {});
      await toggles.waitForFunction(() => !document.querySelector('#knowledge-enabled').disabled);
      await toggles.close();
      const closing = await mount();
      await opened(closing, 'project');
      await closing.$eval('#knowledge-enabled', el => el.click());
      const held = await waitRequest(closing, 'PUT', '/api/projects/p1/memory-enabled');
      await closing.$eval('#knowledge-close', el => el.click());
      const closedReads = (await calls(closing, 'GET')).length;
      await answer(closing, held, 200, {});
      await settle(closing);
      assert.equal((await calls(closing, 'GET')).length, closedReads, 'a memory switch answered after the viewer was closed reads nothing');
      await closing.close();
      const gone = await mount({ auto: {} });
      await gone.$eval('#knowledge-toggle', el => el.click());
      await gone.waitForFunction(() => window.__requests.length === 3);
      await gone.evaluate(() => window.__knowledge.destroy());
      await answer(gone, 0, 200, base()[`GET ${PROJECT}`]); await answer(gone, 1, 200, base()[`GET ${PROFILE}`]); await answer(gone, 2, 200, base()[`GET ${SHARED}`]);
      await settle(gone);
      assert.deepEqual(await gone.evaluate(() => [document.querySelectorAll('.knowledge-record').length, window.__requests.length]), [0, 3], 'a read answered after destroy draws nothing');
      await gone.evaluate(async () => { const k = window.__knowledge; k.onEvent({ kind: 'knowledge_created', project_id: 'p1' }); await k.openProject(); await k.openTask(); await k.store.add('x'); await k.store.save({ id: 'k', rev: 1, scope_id: 'p1' }, 'y'); await k.store.changeEnabled(true); await k.store.clean(); });
      assert.equal(await gone.evaluate(() => window.__requests.length), 3, 'a destroyed viewer sends nothing');
      await gone.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('knowledge page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });
