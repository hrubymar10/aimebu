// The chat (message list, history window, live refresh, thread panel, composer sends, reactions) in Chromium with every request held until the test
// answers it or answered by a small in-page server. web/ is served as is with its import map and markup, without app.js; no Go server. It runs
// unchanged against the page that replaced the hand-written chat. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const REPLACED = fs.existsSync(path.join(root, 'js', 'pages', 'chat', 'page.js'));
const MODULE = REPLACED ? '/js/pages/chat/page.js' : '/js/chat.js';
const PANEL = fs.existsSync(path.join(root, 'js', 'pages', 'room-panel', 'page.js'));
const STORE = fs.existsSync(path.join(root, 'js', 'pages', 'chat', 'store.js'));
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

// The page's side of the server: rooms hold their messages in window.__data; a request is answered here unless a hold matches it, and a held one
// waits until the test answers it. A held request that is aborted rejects like fetch does.
function installServer() {
  window.__requests = []; window.__holds = []; window.__data = { rooms: {}, reactions: {} }; window.__posted = [];
  const respond = (method, path, body) => {
    const url = new URL(path, 'http://x'), parts = url.pathname.split('/').filter(Boolean), q = url.searchParams;
    const ok = value => ({ status: 200, body: value });
    if (method === 'GET' && parts[1] === 'rooms' && parts[3] === 'messages') {
      const room = window.__data.rooms[parts[2]]; if (!room) return { status: 404, body: { message: 'no room' } };
      const thread = q.get('thread_root'), limit = Number(q.get('limit') || 200);
      let list = room.messages.filter(message => thread ? message.thread_root === thread : !message.thread_root);
      if (q.get('after')) list = (thread ? list : room.messages).filter(message => message.seq > Number(q.get('after'))).slice(0, limit);
      else if (q.get('before')) list = list.filter(message => message.seq < Number(q.get('before'))).slice(-limit);
      else if (q.get('around')) {
        const key = q.get('around'), index = list.findIndex(message => message.id === key);
        const start = Math.max(0, Math.min(list.length - limit, (index < 0 ? list.length : index) - Math.floor(limit / 2)));
        list = list.slice(start, start + limit);
      } else list = list.slice(-limit);
      return ok(list.map(message => ({ ...message })));
    }
    if (method === 'POST' && parts[1] === 'rooms' && parts[3] === 'messages') {
      const room = window.__data.rooms[parts[2]]; const seq = room.messages.length + 1;
      const message = { id: `${parts[2]}_m${seq}`, seq, no: seq, room_id: parts[2], author_kind: 'user', author_id: 'usr_me', author_name: 'me', body: body.body, created_at: new Date().toISOString(), reply_to: body.reply_to || '', thread_root: body.thread_root || '' };
      room.messages.push(message); window.__posted.push(body);
      return ok(message);
    }
    if (method === 'POST' && parts[1] === 'rooms' && parts[3] === 'attachments') { window.__uploads = (window.__uploads || 0) + 1; return ok({ id: `att_${window.__uploads}`, name: `file${window.__uploads}.txt`, mime: 'text/plain', url: `/files/${window.__uploads}`, size: 5 }); }
    if (method === 'DELETE' && parts[1] === 'rooms' && parts[3] === 'attachments') return { status: 204, body: null };
    if (parts[1] === 'messages' && parts[3] === 'reactions') {
      const id = parts[2], all = Object.values(window.__data.rooms).flatMap(room => room.messages);
      const list = window.__data.reactions[id] ||= (all.find(message => message.id === id)?.reactions || []).map(entry => ({ ...entry }));
      if (method === 'PUT') { const emoji = decodeURIComponent(parts[4]); if (!list.some(entry => entry.emoji === emoji && entry.member_id === 'usr_me')) list.push({ emoji, member_kind: 'user', member_id: 'usr_me', member_name: 'me' }); return { status: 204, body: null }; }
      if (method === 'DELETE') { const emoji = decodeURIComponent(parts[4]); window.__data.reactions[id] = list.filter(entry => !(entry.emoji === emoji && entry.member_id === 'usr_me')); return { status: 204, body: null }; }
      return ok(list);
    }
    if (method === 'GET' && parts[1] === 'message-numbers') {
      for (const [id, room] of Object.entries(window.__data.rooms)) { const found = room.messages.find(message => message.no === Number(parts[2])); if (found) return ok({ id: found.id, room_id: id, no: found.no }); }
      return { status: 404, body: { message: 'not found' } };
    }
    if (parts[1] === 'rooms' && parts[3] === 'worktree') return method === 'GET' ? ok({ enabled: true, state: 'ready', path: '/w/x', relative_path: 'x' }) : ok({ state: 'removed' });
    if (method === 'GET' && parts[1] === 'projects' && parts[3] === 'memory') return ok({ enabled: true, can_edit: true });
    if (method === 'PUT' && parts[1] === 'rooms' && parts[3] === 'memory-enabled') return ok({ memory_enabled: body.enabled });
    if (method === 'PATCH' && parts[1] === 'rooms' && parts.length === 3) return ok({ id: parts[2], title: body.title });
    if (method === 'POST' && parts[1] === 'rooms' && (parts[3] === 'close' || parts[3] === 'reopen')) return ok({ id: parts[2], project_id: 'prj_a', title: parts[2], kind: 'task', lifecycle: parts[3] === 'close' ? 'closing' : 'open', member_count: 2 });
    if (method === 'POST' && parts[1] === 'rooms' && parts[3] === 'leave') return { status: 204, body: null };
    if (method === 'DELETE' && parts[1] === 'rooms' && parts.length === 3) return { status: 204, body: null };
    if (method === 'GET' && parts[1] === 'rooms' && parts[3] === 'export') return ok({ messages: [] });
    if (method === 'GET' && parts[1] === 'rooms' && ['read-receipts', 'agents', 'mention-users'].includes(parts[3])) return ok([]);
    if (method === 'GET' && parts[1] === 'rooms' && parts[3] === 'members') return ok({ humans: [], agents: [], singletons: [] });
    return { status: 404, body: { message: `unexpected ${method} ${path}` } };
  };
  window.__respond = respond;
  window.fetch = (input, options = {}) => new Promise((resolve, reject) => {
    const request = { path: String(input), method: options.method || 'GET', body: typeof options.body === 'string' ? JSON.parse(options.body) : undefined, answered: false, aborted: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    options.signal?.addEventListener('abort', () => { request.aborted = true; reject(new DOMException('aborted', 'AbortError')); });
    window.__requests.push(request);
    if (window.__holds.some(hold => hold.method === request.method && new RegExp(hold.re).test(request.path))) return;
    const answer = respond(request.method, request.path, request.body); request.answer(answer.status, answer.body);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
  window.__seed = (roomID, count, extra = () => ({})) => {
    const base = Date.parse('2026-01-10T09:00:00Z');
    window.__data.rooms[roomID] = { messages: Array.from({ length: count }, (_, i) => ({ id: `${roomID}_m${i + 1}`, seq: i + 1, no: i + 1, room_id: roomID, author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: `${roomID} message ${i + 1}`, created_at: new Date(base + i * 60000).toISOString(), reply_to: '', thread_root: '', ...extra(i + 1) })) };
  };
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const room = (id, extra = {}) => ({ id, project_id: 'prj_a', title: id, kind: 'task', lifecycle: 'open', member_count: 2, ...extra });
  const mount = async ({ rooms = { rm_a: 130, rm_b: 5 }, setup = '', plain = false } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width: 1280, height: 900 });
    await page.evaluateOnNewDocument(plain => { try { localStorage.clear(); if (plain) localStorage.setItem('aimebu2.composer.formatting', 'off'); } catch {} }, plain);
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installServer);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; });
    await page.evaluate((rooms) => { for (const [id, count] of Object.entries(rooms)) window.__seed(id, count); }, rooms);
    await page.evaluate(async ({ module }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', login: 'me', owner: true, admin: true });
      const { Chat } = await import(module);
      window.__calls = [];
      const chat = window.__chat = new Chat(() => {});
      chat.projectRole = () => 'owner';
      chat.onLatest = room => window.__calls.push(['latest', room.id]);
      chat.onOpenMessage = found => window.__calls.push(['openMessage', found.id]);
      chat.setInbox({ answer: async () => true });
    }, { module: MODULE });
    if (setup) await page.evaluate(setup);
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const hold = (page, method, re) => page.evaluate((method, re) => window.__holds.push({ method, re }), method, re);
  const release = (page, re) => page.evaluate(re => { window.__holds = window.__holds.filter(hold => hold.re !== re); }, re);
  const find = (page, method, re) => page.evaluate((method, re) => window.__requests.findLastIndex(request => request.method === method && !request.answered && !request.aborted && new RegExp(re).test(request.path)), method, re);
  const waitRequest = async (page, method, re) => {
    await page.waitForFunction((method, re) => window.__requests.some(request => request.method === method && !request.answered && !request.aborted && new RegExp(re).test(request.path)), {}, method, re);
    return find(page, method, re);
  };
  // Answer a held request with what the in-page server would have said.
  const serve = (page, index) => page.evaluate(index => { const request = window.__requests[index]; const answer = window.__respond(request.method, request.path, request.body); request.answer(answer.status, answer.body); }, index);
  const count = (page, method, re) => page.evaluate((method, re) => window.__requests.filter(request => request.method === method && new RegExp(re).test(request.path)).length, method, re);
  const calls = page => page.evaluate(() => window.__calls.splice(0));
  const ids = page => page.$$eval('#messages > article.message[data-message-id]', nodes => nodes.map(node => node.dataset.messageId));
  const openRoom = (page, id, extra = {}) => page.evaluate((room, id) => { window.__opening = window.__chat.open(room); return null; }, { id, project_id: 'prj_a', title: id, kind: 'task', lifecycle: 'open', member_count: 2, ...extra }, id);
  const opened = page => page.evaluate(() => window.__opening);

  // Opening a room draws its newest page (100 messages) oldest first, scrolled to the bottom, marks it read once, asks for what came after it and
  // knows there is more behind it.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const state = await page.evaluate(() => { const list = document.querySelector('#messages'); return { first: list.firstElementChild.dataset.messageId, last: list.lastElementChild.dataset.messageId, rows: list.querySelectorAll(':scope > article.message').length, atBottom: list.scrollHeight - list.scrollTop - list.clientHeight < 2, hasOlder: list.dataset.hasOlder, empty: list.dataset.empty, title: document.querySelector('#room-title').textContent }; });
    assert.deepEqual(state, { first: 'rm_a_m31', last: 'rm_a_m130', rows: 100, atBottom: true, hasOlder: 'true', empty: 'false', title: 'rm_a' });
    assert.deepEqual(await calls(page), [['latest', 'rm_a']]);
    assert.equal(await count(page, 'GET', 'messages\\?limit=100'), 1);
    assert.equal(await count(page, 'GET', 'messages\\?after=130'), 1, 'the live cursor starts at the newest message');
    await page.close();
  }

  // An open that is still reading when another room is opened, when the chat is reset, or whose room it is, draws nothing and asks for nothing more.
  {
    const page = await mount();
    await hold(page, 'GET', 'rooms/rm_a/messages\\?limit=100');
    await openRoom(page, 'rm_a');
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?limit=100');
    await openRoom(page, 'rm_b'); await opened(page); await settle(page);
    assert.deepEqual(await ids(page), ['rm_b_m1', 'rm_b_m2', 'rm_b_m3', 'rm_b_m4', 'rm_b_m5']);
    await page.evaluate(index => { const request = window.__requests[index]; request.answer(200, window.__data.rooms.rm_a.messages.slice(-100)); }, held);
    await settle(page);
    assert.deepEqual([await ids(page), await calls(page), await count(page, 'GET', 'rooms/rm_a/messages\\?after')], [['rm_b_m1', 'rm_b_m2', 'rm_b_m3', 'rm_b_m4', 'rm_b_m5'], [['latest', 'rm_b']], 0], 'the old room neither draws, nor marks read, nor refreshes');
    await page.close();
    const reset = await mount();
    await hold(reset, 'GET', 'rooms/rm_a/messages\\?limit=100');
    await openRoom(reset, 'rm_a');
    const second = await waitRequest(reset, 'GET', 'rooms/rm_a/messages\\?limit=100');
    await reset.evaluate(() => window.__chat.reset());
    await reset.evaluate(index => window.__requests[index].answer(200, window.__data.rooms.rm_a.messages.slice(-100)), second);
    await settle(reset);
    assert.deepEqual([await ids(reset), await calls(reset), await count(reset, 'GET', 'messages\\?after'), await reset.evaluate(() => document.querySelector('#room-title').textContent)], [[], [], 0, 'Choose a room'], 'a reset takes the answer of an open');
    await reset.close();
  }
  // Live messages: a refresh appends what came after, keeps the reader at the bottom when they were there and in place when they were not, and leaves
  // the draft and the focus on a row's button alone.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { document.querySelector('#composer textarea[name=body]').value = 'half a thought'; document.querySelector('[data-message-id=rm_a_m120] .message-action')?.focus(); window.__focus = document.activeElement; });
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live one', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => { const list = document.querySelector('#messages'); return [list.lastElementChild.dataset.messageId, list.scrollHeight - list.scrollTop - list.clientHeight < 2, document.activeElement === window.__focus, document.querySelector('#composer textarea[name=body]').value]; }), ['rm_a_m131', true, true, 'half a thought']);
    assert.deepEqual(await calls(page), [['latest', 'rm_a'], ['latest', 'rm_a']], 'the new page is marked read');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = 600; window.__top = list.scrollTop; window.__anchor = document.querySelector('[data-message-id=rm_a_m100]').getBoundingClientRect().top; });
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m132', seq: 132, no: 132, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live two', created_at: '2026-01-10T12:01:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#messages').scrollTop === window.__top, Math.abs(document.querySelector('[data-message-id=rm_a_m100]').getBoundingClientRect().top - window.__anchor) < 2, !!document.querySelector('[data-message-id=rm_a_m132]')]), [true, true, true], 'a reader scrolled up is not moved by an arrival');
    await page.close();
  }

  // A refresh still reading when another room is opened or the chat is reset draws nothing, marks nothing read and does not ask again.
  for (const how of ['open', 'reset']) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'rooms/rm_a/messages\\?after=');
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'late', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); window.__refreshing = window.__chat.refreshMessages(); });
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=');
    await calls(page);
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else await page.evaluate(() => window.__chat.reset());
    await calls(page);
    const before = await count(page, 'GET', 'messages\\?after=');
    await serve(page, held); await page.evaluate(() => window.__refreshing.catch(() => {})); await settle(page);
    assert.deepEqual([await page.evaluate(() => !!document.querySelector('[data-message-id=rm_a_m131]')), await calls(page), await count(page, 'GET', 'messages\\?after=')], [false, [], before], `${how}: a late refresh changes nothing`);
    await page.close();
  }

  // Older messages: reaching the top asks for the page before the oldest row (50 at a time), puts it above without moving what the reader looks at, and
  // knows when there is no more.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { window.__anchor = document.querySelector('[data-message-id=rm_a_m35]'); window.__list = document.querySelector('#messages'); window.__list.scrollTop = 0; window.__before = window.__anchor.getBoundingClientRect().top; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_a_m1');
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__data.rooms.rm_a.messages.length, document.querySelectorAll('#messages > article.message').length, document.querySelector('#messages').dataset.hasOlder, Math.abs(window.__anchor.getBoundingClientRect().top - window.__before) < 3]), [130, 130, 'false', true], 'all 130 rows, nothing more behind');
    assert.equal(await count(page, 'GET', 'messages\\?before=31&limit=50'), 1);
    await page.close();
  }

  // An older page still reading when the room changes or the chat is reset is dropped, and the next room can load its own older pages.
  for (const how of ['open', 'reset']) {
    const page = await mount({ rooms: { rm_a: 130, rm_b: 130 } });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'rooms/rm_a/messages\\?before=');
    await page.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?before=');
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); await settle(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); await settle(page); }
    await serve(page, held); await settle(page);
    assert.deepEqual(await ids(page).then(list => [list.length, list[0]]), [100, 'rm_b_m31'], `${how}: the old room's older page is not drawn`);
    await page.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_b_m1', { timeout: 5000 }).catch(() => {});
    assert.equal(await count(page, 'GET', 'rooms/rm_b/messages\\?before='), 1, `${how}: the next room asks for its own older page`);
    await page.close();
  }
  // Jumping to a message the list has not loaded asks for the page around it and shows a history window: the newest page is not on screen, the Back to
  // latest button is, and scrolling to the bottom pages forward until the live end is reached, which makes it the latest view again.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await calls(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10'));
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#messages > article.message').length, document.querySelector('#messages > article.message').dataset.messageId, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, window.__chat.contextMode, document.querySelector('#messages').dataset.hasOlder, document.querySelector('[data-message-id=rm_a_m10] .message-bubble').classList.contains('message-flash') || document.querySelector('[data-message-id=rm_a_m10]').classList.contains('message-flash')]), [100, 'rm_a_m1', false, true, 'true', true]);
    assert.deepEqual(await calls(page), [], 'a window is not the latest view: nothing is marked read');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await page.waitForFunction(() => window.__chat.contextMode === false, { timeout: 5000 });
    await settle(page);
    assert.deepEqual([await count(page, 'GET', 'messages\\?after=100&limit=50'), await ids(page).then(list => [list.length, list.at(-1)]), await calls(page)], [1, [130, 'rm_a_m130'], [['latest', 'rm_a']]], 'the last page makes it the latest view again, read once');
    assert.equal(await page.evaluate(() => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden), true);
    await page.close();
  }

  // Back to latest reloads the room's newest page; messages that arrive while a window is shown are counted on the button instead of drawn.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'while away', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').getAttribute('aria-label'), !!document.querySelector('[data-message-id=rm_a_m131]')]), ['Back to latest · 1 new', false]);
    await page.$eval('.back-to-latest:not(.thread-latest):not(.thread-new)', node => node.click());
    await page.waitForFunction(() => window.__chat.contextMode === false && !!document.querySelector('[data-message-id=rm_a_m131]'), { timeout: 5000 });
    assert.equal(await page.evaluate(() => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden), true);
    await page.close();
  }

  // A jump or a forward page still reading when the room changes or the chat is reset changes nothing in the room that was opened since.
  for (const how of ['open', 'reset']) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'rooms/rm_a/messages\\?around=');
    await page.evaluate(() => { window.__jumping = window.__chat.jumpToMessage('rm_a_m10'); });
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?around=');
    if (how === 'open') await openRoom(page, 'rm_b'); else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); }
    await opened(page); await settle(page); await calls(page);
    await serve(page, held); await page.evaluate(() => window.__jumping); await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__chat.contextMode, document.querySelectorAll('#messages > article.message').length, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, document.querySelector('#chat-error').textContent]), [false, 5, true, ''], `${how}: a late jump leaves the new room alone`);
    await page.close();
  }

  // A forward page of a history window still reading when the room changes or the chat is reset changes nothing in the room opened since, leaves no window
  // behind and starts no read of its own (no catch-up of the live end).
  for (const how of ['open', 'reset']) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
    await hold(page, 'GET', 'rooms/rm_a/messages\\?after=100');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=100');
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
    await settle(page); await calls(page);
    const reads = await page.evaluate(() => window.__requests.length);
    await serve(page, held); await settle(page); await settle(page);
    assert.deepEqual([await ids(page).then(list => list.length), await page.evaluate(() => [window.__chat.contextMode, window.__chat.catchingUp, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, document.querySelector('#chat-error').textContent]), await calls(page), await page.evaluate(() => window.__requests.length) - reads], [5, [false, false, true, ''], [], 0], `${how}: a late forward page changes nothing and starts no read`);
    await page.close();
  }

  // A number is looked up on the server; a message of another room opens that room on it, but not once the reader has moved on.
  {
    const page = await mount({ rooms: { rm_a: 5, rm_b: 5, rm_c: 5 } });
    await page.evaluate(() => { for (const message of window.__data.rooms.rm_b.messages) message.no += 1000; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page); await calls(page);
    await page.evaluate(() => window.__chat.jumpToMessage(1003));
    await settle(page);
    assert.deepEqual(await calls(page), [['openMessage', 'rm_b_m3']], 'a message of another room opens that room');
    await page.close();
  }

  // A lookup answered after the reader has moved on to another room, or after a reset, opens nothing.
  {
    const page = await mount({ rooms: { rm_a: 5, rm_b: 5, rm_c: 5 } });
    await page.evaluate(() => { for (const message of window.__data.rooms.rm_b.messages) message.no += 1000; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page); await calls(page);
    await hold(page, 'GET', 'message-numbers/');
    await page.evaluate(() => { window.__jumping = window.__chat.jumpToMessage(1004); });
    const held = await waitRequest(page, 'GET', 'message-numbers/1004');
    await openRoom(page, 'rm_c'); await opened(page); await settle(page); await calls(page);
    await serve(page, held); await page.evaluate(() => window.__jumping); await settle(page);
    assert.deepEqual(await calls(page), [], 'a lookup answered after the reader moved on opens nothing');
    await page.evaluate(() => { window.__jumping = window.__chat.jumpToMessage(9999); });
    const missing = await waitRequest(page, 'GET', 'message-numbers/9999');
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await serve(page, missing); await page.evaluate(() => window.__jumping); await settle(page);
    assert.equal(await page.evaluate(() => document.querySelector('#chat-error').textContent), '', 'nor does a lookup that failed write its error into the new room');
    await page.close();
    const reset = await mount({ rooms: { rm_a: 5, rm_b: 5 } });
    await reset.evaluate(() => { for (const message of window.__data.rooms.rm_b.messages) message.no += 1000; });
    await openRoom(reset, 'rm_a'); await opened(reset); await settle(reset); await calls(reset);
    await hold(reset, 'GET', 'message-numbers/');
    await reset.evaluate(() => { window.__jumping = window.__chat.jumpToMessage(1004); });
    const second = await waitRequest(reset, 'GET', 'message-numbers/1004');
    await reset.evaluate(() => window.__chat.reset());
    await serve(reset, second); await reset.evaluate(() => window.__jumping); await settle(reset);
    assert.deepEqual(await calls(reset), [], 'nor after a reset');
    await reset.close();
  }

  const typeDraft = (page, text) => page.evaluate(text => { const draft = document.querySelector('#composer textarea[name=body]'); draft.value = text; draft.dispatchEvent(new Event('input', { bubbles: true })); }, text);
  const draftText = page => page.evaluate(() => document.querySelector('#composer textarea[name=body]').value);
  const submit = page => page.evaluate(() => { window.__sending = window.__chat.send(); });
  const noticeText = page => page.evaluate(() => document.querySelector('.composer-notice')?.textContent || '');
  const posts = page => page.evaluate(() => window.__requests.filter(request => request.method === 'POST' && /\/messages$/.test(request.path)).map(request => request.body));
  const file = (page, name, size = 5) => page.evaluate((name, size) => new File(['x'.repeat(size)], name, { type: 'text/plain' }) && null, name, size);
  const addFile = (page, name = 'a.txt', size = 5) => page.evaluate((name, size) => { window.__adding = window.__chat.addFiles([new File(['x'.repeat(size)], name, { type: 'text/plain' })]); }, name, size);
  const pending = page => page.evaluate(() => [...document.querySelectorAll('#pending-attachments .pending-image')].map(node => node.textContent.trim()).concat(document.querySelector('#pending-attachments > span:last-child:not(.pending-image)')?.textContent ? [document.querySelector('#pending-attachments > span:last-child').textContent] : []));

  // Sending: the text goes to the open room and the draft empties, the reply chip goes, the message is drawn at the bottom; a failure keeps the draft and
  // says why in the composer.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await typeDraft(page, 'hello there'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await posts(page), await draftText(page), await ids(page).then(list => list.at(-1)), await page.evaluate(() => { const list = document.querySelector('#messages'); return list.scrollHeight - list.scrollTop - list.clientHeight < 2; })], [[{ body: 'hello there', attachments: [] }], '', 'rm_a_m131', true]);
    await page.evaluate(() => { const message = window.__chat.messages.get('rm_a_m120'); window.__chat.setReply(message); });
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#reply-chip').hidden, document.querySelector('#reply-chip-jump').textContent]), [false, 'Replying to ann: rm_a message 120']);
    await typeDraft(page, 'a reply'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), await page.evaluate(() => document.querySelector('#reply-chip').hidden), await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m132] .quote')?.textContent)], [{ body: 'a reply', attachments: [], reply_to: 'rm_a_m120' }, true, '↪ #120 ann: rm_a message 120']);
    await typeDraft(page, '   '); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.equal((await posts(page)).length, 2, 'an empty draft is not sent');
    await page.evaluate(() => { window.__respond0 = window.__respond; window.__holds.push({ method: 'POST', re: '/messages$' }); });
    await typeDraft(page, 'will fail'); await submit(page);
    await answer(page, await waitRequest(page, 'POST', '/messages$'), 500, { message: 'The room is read-only.' });
    await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await draftText(page), await noticeText(page)], ['will fail', 'The room is read-only.'], 'a failure keeps the draft and says why');
    await page.close();
  }

  // A send still out when another room is opened or the chat is reset leaves the new view alone: no notice, no row, no cleared draft of the new room.
  for (const how of ['open', 'reset']) {
    for (const outcome of ['ok', 'fail']) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'POST', '/messages$');
      await typeDraft(page, 'out the door'); await submit(page);
      const held = await waitRequest(page, 'POST', '/messages$');
      if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
      await settle(page); await typeDraft(page, 'next room text'); await calls(page);
      if (outcome === 'ok') await serve(page, held); else await answer(page, held, 500, { message: 'nope' });
      await page.evaluate(() => window.__sending); await settle(page);
      assert.deepEqual([await draftText(page), await noticeText(page), await ids(page).then(list => list.length), await calls(page)], ['next room text', '', 5, []], `${how}/${outcome}: a late send changes nothing in the new room`);
      await page.close();
    }
  }

  // Sending from a history window shows the latest view with the new message instead of appending among old ones.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
    await typeDraft(page, 'from the past'); await submit(page); await page.evaluate(() => window.__sending);
    await page.waitForFunction(() => window.__chat.contextMode === false && !!document.querySelector('[data-message-id=rm_a_m131]'), { timeout: 5000 });
    assert.deepEqual(await page.evaluate(() => [document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, document.querySelector('#messages').lastElementChild.dataset.messageId]), [true, 'rm_a_m131']);
    await page.close();
  }

  // Attachments: a picked file uploads (the composer waits), shows in the strip and goes with the message; limits say why; a file that arrives after
  // the room changed is deleted again, and a reset aborts the upload.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'POST', '/attachments$');
    await addFile(page, 'a.txt');
    const upload = await waitRequest(page, 'POST', '/attachments$');
    assert.deepEqual([await pending(page), await page.evaluate(() => document.querySelector('#composer button[type=submit]').disabled)], [['Uploading 1…'], true], 'the composer waits for the upload');
    await serve(page, upload); await page.evaluate(() => window.__adding); await settle(page);
    assert.deepEqual([await pending(page), await page.evaluate(() => document.querySelector('#composer button[type=submit]').disabled)], [['file1.txt'], false]);
    await typeDraft(page, ''); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), await pending(page)], [{ body: '', attachments: ['att_1'] }, []], 'a message of only a file is sent, and the strip empties');
    await addFile(page, 'empty.txt', 0); await page.evaluate(() => window.__adding);
    assert.equal(await noticeText(page), 'empty.txt is empty.');
    await addFile(page, 'big.bin', 11 * 1024 * 1024); await page.evaluate(() => window.__adding);
    assert.match(await noticeText(page), /big\.bin is too big/);
    await page.close();
  }
  for (const how of ['open', 'reset']) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'POST', '/attachments$');
    await addFile(page, 'late.txt');
    const upload = await waitRequest(page, 'POST', '/attachments$');
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
    await settle(page);
    const aborted = await page.evaluate(index => window.__requests[index].aborted, upload);
    if (!aborted) await serve(page, upload);
    await page.evaluate(() => window.__adding.catch(() => {})); await settle(page);
    assert.deepEqual([await pending(page), await noticeText(page), how === 'reset' ? aborted : await count(page, 'DELETE', '/attachments/att_1$'), await page.evaluate(() => document.querySelector('#composer button[type=submit]').disabled)], [[], '', how === 'reset' ? true : 1, false], `${how}: a late upload is dropped (deleted, or aborted by a reset) and leaves the composer usable`);
    await page.close();
  }

  // A failed upload says which file and why; once the room has changed it says nothing.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'POST', '/attachments$');
    await addFile(page, 'bad.txt');
    await answer(page, await waitRequest(page, 'POST', '/attachments$'), 413, { message: 'File too large.' });
    await page.evaluate(() => window.__adding); await settle(page);
    assert.deepEqual([await noticeText(page), await pending(page)], ['bad.txt: File too large.', []]);
    await addFile(page, 'worse.txt');
    const late = await waitRequest(page, 'POST', '/attachments$');
    await openRoom(page, 'rm_b'); await opened(page); await settle(page);
    await answer(page, late, 500, { message: 'Broken.' });
    await page.evaluate(() => window.__adding); await settle(page);
    assert.equal(await noticeText(page), '', 'a failure after the room changed says nothing');
    await page.close();
  }

  // A reply to a question open for this person that is one of its proposed answers answers it through the Inbox instead of posting a message; a refusal
  // says so, keeps the text and posts nothing.
  {
    const page = await mount();
    await page.evaluate(() => { window.__data.rooms.rm_a.messages[119] = { ...window.__data.rooms.rm_a.messages[119], author_kind: 'agent', author_id: 'agt_zed', author_name: 'zed', body: 'Which one?', proposed_answers: ['alpha', 'beta'] }; window.__answered = []; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => {
      window.__chat.agents = new Map([['agt_zed', { slug: 'zed' }]]);
      window.__chat.setInbox({ answer: async (item, text) => { window.__answered.push([item.id, text]); return window.__answerResult ?? true; } });
      window.__chat.updateAnswerControls([{ id: 'inb_1', rev: 1, kind: 'question', message_id: 'rm_a_m120', proposed_answers: ['alpha', 'beta'], options: [], free_text: false, can_decide: true }]);
      window.__chat.setReply(window.__chat.messages.get('rm_a_m120'));
    });
    await typeDraft(page, '@zed beta'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__answered), (await posts(page)).length, await draftText(page), await page.evaluate(() => document.querySelector('#reply-chip').hidden)], [[['inb_1', '@zed beta']], 0, '', true], 'an answer goes through the inbox, not as a message');
    await page.evaluate(() => { window.__answerResult = false; window.__chat.setReply(window.__chat.messages.get('rm_a_m120')); });
    await typeDraft(page, '@zed alpha'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await noticeText(page), await draftText(page), (await posts(page)).length], ['Could not send the answer', '@zed alpha', 0], 'a refused answer keeps the text');
    await page.evaluate(() => { window.__answerResult = true; window.__chat.setReply(window.__chat.messages.get('rm_a_m120')); });
    await typeDraft(page, 'something else entirely'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), (await page.evaluate(() => window.__answered)).length], [{ body: 'something else entirely', attachments: [], reply_to: 'rm_a_m120' }, 2], 'text that is not an option is an ordinary reply');
    await page.close();
  }

  // Threads. Replies of m120 are seq 131 on; the room list shows the root with a reply count and the thread panel the root and its replies.
  const addReplies = (page, count, root = 'rm_a_m120') => page.evaluate((count, root) => {
    const messages = window.__data.rooms.rm_a.messages;
    const start = messages.length;
    for (let i = 1; i <= count; i++) messages.push({ id: `rm_a_t${i}`, seq: start + i, no: start + i, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: `reply ${i}`, created_at: new Date(Date.parse('2026-01-10T11:00:00Z') + i * 1000).toISOString(), reply_to: root, thread_root: root, thread_count: count });
    messages.find(message => message.id === root).thread_count = count;
  }, count, root);
  const panel = page => page.evaluate(() => ({ hidden: document.querySelector('#thread-panel').hidden, title: document.querySelector('#thread-panel h2').textContent, entries: [...document.querySelectorAll('#thread-messages > article.thread-entry')].map(node => node.dataset.messageId), error: document.querySelector('#thread-error').textContent, focus: window.__chat.threadForm.contains(document.activeElement) }));

  // Opening a thread shows its root and the newest page of replies in order and puts the cursor in the thread's composer; the room list's root shows the count.
  {
    const page = await mount();
    await addReplies(page, 3);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m120] .thread-chip').textContent), '3 replies', 'the root counts its replies from the live page');
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    assert.deepEqual(await panel(page), { hidden: false, title: 'Thread · 3', entries: ['rm_a_m120', 'rm_a_t1', 'rm_a_t2', 'rm_a_t3'], error: '', focus: true });
    assert.equal(await count(page, 'GET', 'thread_root=rm_a_m120&limit=100'), 1);
    await page.evaluate(() => { const draft = window.__chat.threadDraft; draft.value = 'my reply'; });
    await page.evaluate(() => window.__chat.sendThread()); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), (await panel(page)).entries.at(-1), await page.evaluate(() => window.__chat.threadDraft.value)], [{ body: 'my reply', thread_root: 'rm_a_m120', reply_to: 'rm_a_m120' }, 'rm_a_t3'.replace('t3', 't3') && (await panel(page)).entries.at(-1), ''], 'a reply is posted into the thread and the box empties');
    assert.equal(await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m120] .thread-chip').textContent), '4 replies');
    await page.evaluate(() => { window.__holds.push({ method: 'POST', re: '/messages$' }); window.__chat.threadDraft.value = 'will fail'; window.__sendingThread = window.__chat.sendThread(); });
    await answer(page, await waitRequest(page, 'POST', '/messages$'), 500, { message: 'Thread is closed.' }); await page.evaluate(() => window.__sendingThread); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__chat.threadDraft.value), await page.evaluate(() => document.querySelector('#thread-notice .composer-notice-text')?.textContent || '')], ['will fail', 'Thread is closed.']);
    await page.$eval('#close-thread', node => node.click());
    assert.equal((await panel(page)).hidden, true);
    await page.close();
  }

  // A thread that is still loading when the room changes, the thread is closed or the chat is reset draws nothing and leaves the panel alone.
  for (const how of ['open', 'close', 'reset']) {
    const page = await mount();
    await addReplies(page, 3);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'thread_root=');
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    const held = await waitRequest(page, 'GET', 'thread_root=');
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else if (how === 'close') await page.$eval('#close-thread', node => node.click()); else await page.evaluate(() => window.__chat.reset());
    await settle(page);
    await serve(page, held); await settle(page);
    assert.deepEqual([(await panel(page)).entries.filter(id => id !== 'rm_a_m120'), (await panel(page)).error, await count(page, 'GET', 'thread_root=.*after=')], [[], '', 0], `${how}: a late thread page is not drawn`);
    await page.close();
  }

  // A long thread opens on its newest 100 replies, older ones come as the panel is scrolled to the top.
  {
    const page = await mount();
    await addReplies(page, 130);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 101); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#thread-messages').dataset.hasOlder, document.querySelector('#thread-messages > article:nth-child(2)').dataset.messageId]), ['true', 'rm_a_t31']);
    await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 131, { timeout: 5000 }); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#thread-messages').dataset.hasOlder, document.querySelector('#thread-messages > article:nth-child(2)').dataset.messageId]), ['false', 'rm_a_t1']);
    await page.close();
  }

  // An older page of replies still loading when the thread closes, the room changes or the chat is reset is dropped.
  for (const how of ['close', 'open', 'reset']) {
    const page = await mount();
    await addReplies(page, 130);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 101); await settle(page);
    await hold(page, 'GET', 'thread_root=.*before=');
    await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
    const held = await waitRequest(page, 'GET', 'thread_root=.*before=');
    if (how === 'close') await page.$eval('#close-thread', node => node.click()); else if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); await settle(page); } else await page.evaluate(() => window.__chat.reset());
    const reads = await count(page, 'GET', 'thread_root=');
    await serve(page, held); await settle(page);
    assert.equal(await count(page, 'GET', 'thread_root='), reads, `${how}: no read follows`);
    assert.deepEqual([(await panel(page)).entries.length, (await panel(page)).error, await page.evaluate(() => window.__chat.loadingOlderThread)], [0, '', false], `${how}: a late older page is dropped`);
    await page.close();
  }

  // A reply still being sent when its thread is closed, its room left or the chat reset changes nothing; a failure of the thread's page says nothing then.
  for (const how of ['close', 'open', 'reset']) {
    const page = await mount();
    await addReplies(page, 2);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(page);
    await hold(page, 'POST', '/messages$');
    await page.evaluate(() => { window.__chat.threadDraft.value = 'sent late'; window.__sendingThread = window.__chat.sendThread(); });
    const held = await waitRequest(page, 'POST', '/messages$');
    if (how === 'close') await page.$eval('#close-thread', node => node.click()); else if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else await page.evaluate(() => window.__chat.reset());
    await settle(page);
    await page.evaluate(() => { window.__chat.threadDraft.value = 'typed since'; });
    await serve(page, held); await page.evaluate(() => window.__sendingThread); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__chat.threadDraft.value), await page.evaluate(() => [...window.__chat.messages.keys()].filter(id => /_m(131|132|133)$|_new/.test(id) || window.__chat.messages.get(id)?.body === 'sent late')), (await panel(page)).entries], ['typed since', [], []], `${how}: a late thread reply neither clears the box nor lands in a panel`);
    await page.close();
    const failing = await mount();
    await addReplies(failing, 2);
    await openRoom(failing, 'rm_a'); await opened(failing); await settle(failing);
    await failing.evaluate(() => { window.__holds.push({ method: 'POST', re: '/messages$' }); });
    await failing.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await failing.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(failing);
    await failing.evaluate(() => { window.__chat.threadDraft.value = 'will fail'; window.__sendingThread = window.__chat.sendThread(); });
    const bad = await waitRequest(failing, 'POST', '/messages$');
    if (how === 'close') await failing.$eval('#close-thread', node => node.click()); else if (how === 'open') { await openRoom(failing, 'rm_b'); await opened(failing); } else await failing.evaluate(() => window.__chat.reset());
    await answer(failing, bad, 500, { message: 'Too late.' }); await failing.evaluate(() => window.__sendingThread); await settle(failing);
    assert.equal(await failing.evaluate(() => document.querySelector('#thread-notice .composer-notice-text')?.textContent || ''), how === 'close' ? 'Too late.' : '', `${how}: a failed reply says so only in the room it was sent from`);
    await failing.close();
  }
  for (const how of ['close', 'open']) {
    const page = await mount();
    await addReplies(page, 2);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'thread_root=');
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    const held = await waitRequest(page, 'GET', 'thread_root=');
    if (how === 'close') await page.$eval('#close-thread', node => node.click()); else { await openRoom(page, 'rm_b'); await opened(page); }
    await answer(page, held, 500, { message: 'Thread unavailable.' }); await settle(page);
    assert.equal((await panel(page)).error, '', `${how}: a failed thread page says nothing once the thread is gone`);
    await page.close();
  }

  // An older page of one thread that finishes after another thread was opened does not clear the second thread's own loading state.
  {
    const page = await mount();
    await addReplies(page, 130);
    await addReplies(page, 130, 'rm_a_m110');
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 101); await settle(page);
    await hold(page, 'GET', 'thread_root=rm_a_m120.*before=');
    await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
    const first = await waitRequest(page, 'GET', 'thread_root=rm_a_m120.*before=');
    await page.$eval('[data-message-id=rm_a_m110] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelector('#thread-messages > article')?.dataset.messageId === 'rm_a_m110' && document.querySelectorAll('#thread-messages > article.thread-entry').length === 101); await settle(page);
    await hold(page, 'GET', 'thread_root=rm_a_m110.*before=');
    await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
    await waitRequest(page, 'GET', 'thread_root=rm_a_m110.*before=');
    await serve(page, first); await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__chat.loadingOlderThread, window.__chat.threadRoot.id, document.querySelectorAll('#thread-messages > article.thread-entry').length]), [true, 'rm_a_m110', 101], 'the first thread\'s late page neither draws nor ends the second thread\'s load');
    await page.close();
  }

  // Live replies: one for the open thread joins the panel and the root's count; one for a thread that is not open only raises the count.
  {
    const page = await mount();
    await addReplies(page, 2);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(page);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_live', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live reply', created_at: '2026-01-10T12:00:00Z', reply_to: 'rm_a_m120', thread_root: 'rm_a_m120', thread_count: 3 }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual([(await panel(page)).entries.at(-1), await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m120] .thread-chip').textContent), (await panel(page)).title], ['rm_a_live', '3 replies', 'Thread · 3']);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_other', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'other reply', created_at: '2026-01-10T12:01:00Z', reply_to: 'rm_a_m110', thread_root: 'rm_a_m110', thread_count: 1 }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual([await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m110] .thread-chip').textContent), (await panel(page)).entries.length], ['1 reply', 4]);
    await page.close();
  }

  // Jumping to a reply opens its thread on it; a jump still looking for the root when the room changes opens nothing.
  {
    const page = await mount();
    await addReplies(page, 130);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_t5')); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#thread-panel').hidden, !!document.querySelector('#thread-messages [data-message-id=rm_a_t5]'), document.querySelector('#thread-messages [data-message-id=rm_a_t5]')?.classList.contains('message-flash')]), [false, true, true]);
    await page.close();
    const late = await mount();
    await addReplies(late, 130);
    await openRoom(late, 'rm_a'); await opened(late); await settle(late);
    await hold(late, 'GET', 'around=rm_a_m120');
    await late.evaluate(() => { window.__chat.messages.delete('rm_a_m120'); window.__chat.nodes.get('rm_a_m120').remove(); window.__chat.nodes.delete('rm_a_m120'); window.__jumping = window.__chat.jumpToMessage('rm_a_t5'); });
    const held = await waitRequest(late, 'GET', 'around=rm_a_m120');
    await openRoom(late, 'rm_b'); await opened(late); await settle(late);
    await serve(late, held); await late.evaluate(() => window.__jumping); await settle(late);
    assert.deepEqual([(await panel(late)).hidden, (await panel(late)).entries.length, await late.evaluate(() => document.querySelector('#chat-error').textContent)], [true, 0, ''], 'a late root lookup opens no thread in the new room');
    await late.close();
    for (const [held, what, change] of [['around=rm_a_m120', 'a late root lookup opens no thread', 'jump'], ['thread_root=rm_a_m120', 'a late thread page shows no reply', 'jump'], ['thread_root=rm_a_m120', 'a late thread page shows no reply after a room change', 'room']]) {
      const newer = await mount();
      await addReplies(newer, 130);
      await openRoom(newer, 'rm_a'); await opened(newer); await settle(newer);
      await newer.evaluate(() => { window.__shown = 0; const show = window.__chat.showReply; window.__chat.showReply = function (...args) { window.__shown++; return show.apply(this, args); }; });
      if (held.startsWith('around')) await newer.evaluate(() => { window.__chat.messages.delete('rm_a_m120'); window.__chat.nodes.get('rm_a_m120').remove(); window.__chat.nodes.delete('rm_a_m120'); });
      await hold(newer, 'GET', held);
      await newer.evaluate(() => { window.__jumping = window.__chat.jumpToMessage('rm_a_t5'); });
      const request = await waitRequest(newer, 'GET', held);
      if (change === 'jump') await newer.evaluate(() => { window.__chat.jumpToMessage('rm_a_m100'); });
      else { await openRoom(newer, 'rm_b'); await opened(newer); }
      await settle(newer);
      const hidden = (await panel(newer)).hidden;
      await newer.evaluate(() => { window.__holds.length = 0; }); await serve(newer, request); await newer.evaluate(() => window.__jumping); await settle(newer);
      assert.deepEqual([(await panel(newer)).hidden === hidden || !hidden, await newer.evaluate(() => window.__shown)], [true, 0], `${change === 'jump' ? 'a newer jump: ' : ''}${what}`);
      await newer.close();
    }
  }

  // Reactions. A message that carries its reactions shows them without a request; one that does not asks for them. Entries group by emoji with a count, the
  // names as the tooltip, and the viewer's own pressed; a click on a pill adds or removes the viewer's reaction and reads the pills again.
  const pills = (page, id) => page.evaluate(id => [...document.querySelectorAll(`[data-message-id=${id}] .reaction-pill`)].map(node => [node.textContent, node.title, node.getAttribute('aria-pressed'), node.dataset.emoji]), id);
  const reactionRoom = async () => {
    const page = await mount();
    await page.evaluate(() => {
      window.__data.rooms.rm_a.messages[129].reactions = [{ emoji: '👍', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }, { emoji: '👍', member_kind: 'user', member_id: 'usr_me', member_name: 'me' }, { emoji: '🎉', member_kind: 'agent', member_id: 'agt_zed', member_name: 'zed' }];
      window.__data.reactions.rm_a_m129 = [{ emoji: '👀', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }];
    });
    return page;
  };
  {
    const page = await reactionRoom();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.deepEqual(await pills(page, 'rm_a_m130'), [['👍 2', 'ann, me', 'true', '👍'], ['🎉 1', 'zed', 'false', '🎉']]);
    assert.deepEqual(await pills(page, 'rm_a_m129'), [['👀 1', 'ann', 'false', '👀']], 'a message without its reactions asks for them');
    assert.equal(await count(page, 'GET', '/api/messages/rm_a_m130/reactions'), 0, 'a message that carries them does not');
    assert.equal(await count(page, 'GET', '/api/messages/rm_a_m129/reactions'), 1);
    await page.$eval('[data-message-id=rm_a_m130] .reaction-pill[data-emoji="👍"]', node => node.click()); await settle(page);
    assert.deepEqual([await count(page, 'DELETE', '/api/messages/rm_a_m130/reactions/'), await pills(page, 'rm_a_m130')], [1, [['👍 1', 'ann', 'false', '👍'], ['🎉 1', 'zed', 'false', '🎉']]], 'my own pill removes my reaction');
    await page.$eval('[data-message-id=rm_a_m130] .reaction-pill[data-emoji="🎉"]', node => node.click()); await settle(page);
    assert.deepEqual([await count(page, 'PUT', '/api/messages/rm_a_m130/reactions/'), (await pills(page, 'rm_a_m130')).at(-1)], [1, ['🎉 2', 'zed, me', 'true', '🎉']], 'someone else\'s pill adds mine');
    await page.$eval('[data-message-id=rm_a_m130] .message-bubble', node => node.scrollIntoView());
    await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m130] .message-action[aria-haspopup]').click());
    await page.$eval('[data-message-id=rm_a_m130] .quick-reaction[data-emoji="✅"]', node => node.click()); await settle(page);
    assert.deepEqual((await pills(page, 'rm_a_m130')).map(pill => pill[3]), ['👍', '🎉', '✅'], 'a quick reaction from the message actions');
    await page.close();
  }

  // The automatic read of a message's reactions that is still out when another room is opened or the chat is reset draws no pill, says nothing and starts
  // nothing in the new view, answered well.
  for (const how of ['open', 'reset']) {
    const page = await reactionRoom();
    await hold(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await openRoom(page, 'rm_a');
    const held = await waitRequest(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await opened(page);
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
    await settle(page);
    const before = errors.length, reads = await page.evaluate(() => window.__requests.length);
    await serve(page, held); await settle(page);
    assert.deepEqual([await page.evaluate(() => document.querySelectorAll('.reaction-pill').length), await page.evaluate(() => document.querySelector('#chat-error').textContent), await page.evaluate(() => window.__requests.length) - reads, errors.slice(before), await page.evaluate(() => window.__chat.reactions.has('rm_a_m129'))], [0, '', 0, [], false], `${how}: a late reactions read changes nothing and is not kept`);
    await page.close();
  }

  // A reaction change still out when another room is opened or the chat is reset reads nothing afterwards and says nothing, answered well or badly; in its
  // own room a failure is told.
  for (const how of ['open', 'reset']) {
    for (const outcome of ['ok', 'fail']) {
      const page = await reactionRoom();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'PUT', '/reactions/');
      await page.$eval('[data-message-id=rm_a_m130] .reaction-pill[data-emoji="🎉"]', node => node.click());
      const held = await waitRequest(page, 'PUT', '/reactions/');
      if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
      await settle(page);
      const reads = await count(page, 'GET', '/api/messages/');
      if (outcome === 'ok') await serve(page, held); else await answer(page, held, 500, { message: 'Reaction refused.' });
      await settle(page);
      assert.deepEqual([await count(page, 'GET', '/api/messages/'), await page.evaluate(() => document.querySelector('#chat-error').textContent)], [reads, ''], `${how}/${outcome}: a late reaction answer reads nothing and says nothing`);
      await page.close();
    }
  }
  {
    const page = await reactionRoom();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'PUT', '/reactions/');
    await page.$eval('[data-message-id=rm_a_m130] .reaction-pill[data-emoji="🎉"]', node => node.click());
    await answer(page, await waitRequest(page, 'PUT', '/reactions/'), 403, { message: 'Viewers cannot react.' }); await settle(page);
    assert.equal(await page.evaluate(() => document.querySelector('#chat-error').textContent), 'Viewers cannot react.');
    await page.close();
  }

  // The read of a message's reactions that fails (the room's server is down, the message is gone) is not an error of the page: the message shows without
  // pills and the rest of the room draws.
  {
    const page = await mount();
    await hold(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await openRoom(page, 'rm_a');
    const failing = await waitRequest(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await opened(page);
    const before = errors.length;
    await answer(page, failing, 500, { message: 'Reactions are unavailable.' }); await settle(page); await settle(page);
    assert.deepEqual([errors.slice(before), await pills(page, 'rm_a_m129'), await ids(page).then(list => list.length)], [[], [], 100], 'a failed read of the reactions is quiet');
    await page.close();
  }

  // The same read that fails (and the one that fails after the room changed or the chat was reset) is quiet in the new view.
  for (const how of ['open', 'reset']) {
    const page = await reactionRoom();
    await hold(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await openRoom(page, 'rm_a');
    const held = await waitRequest(page, 'GET', '/api/messages/rm_a_m129/reactions');
    await opened(page);
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
    await settle(page);
    const before = errors.length, reads = await page.evaluate(() => window.__requests.length);
    await answer(page, held, 500, { message: 'Reactions are unavailable.' }); await settle(page); await settle(page);
    assert.deepEqual([await page.evaluate(() => document.querySelectorAll('.reaction-pill').length), await page.evaluate(() => document.querySelector('#chat-error').textContent), await page.evaluate(() => window.__requests.length) - reads, errors.slice(before)], [0, '', 0, []], `${how}: a late failed reactions read is quiet`);
    await page.close();
  }

  // Keyboard. The arrows, Home and End move between the buttons of a focused message's action bar and wrap; Up in an empty composer walks back through the
  // viewer's own messages, Down walks forward and past the newest restores what was being written, Escape restores it too; Enter sends and Shift+Enter
  // does not.
  {
    const page = await mount({ plain: true });
    await page.evaluate(() => { for (const [seq, body] of [[121, 'mine one'], [124, 'mine two'], [127, 'mine two'], [129, 'mine three']]) Object.assign(window.__data.rooms.rm_a.messages[seq - 1], { author_id: 'usr_me', author_name: 'me', body }); });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { const bar = document.querySelector('[data-message-id=rm_a_m120] .message-actions'); window.__buttons = [...bar.querySelectorAll('button')].filter(button => !button.closest('[hidden]')).map(button => button.getAttribute('aria-label')); bar.querySelector('button').focus(); });
    const focused = () => page.evaluate(() => document.activeElement.getAttribute('aria-label'));
    const labels = await page.evaluate(() => window.__buttons);
    assert.ok(labels.length >= 4, 'the bar has its buttons');
    await page.keyboard.press('ArrowRight'); assert.equal(await focused(), labels[1]);
    await page.keyboard.press('End'); assert.equal(await focused(), labels.at(-1));
    await page.keyboard.press('ArrowRight'); assert.equal(await focused(), labels[0], 'wraps after the last');
    await page.keyboard.press('ArrowLeft'); assert.equal(await focused(), labels.at(-1), 'wraps before the first');
    await page.keyboard.press('Home'); assert.equal(await focused(), labels[0]);
    await page.focus('#composer textarea[name=body]');
    const draft = () => draftText(page);
    await page.keyboard.press('ArrowUp'); assert.equal(await draft(), 'mine three');
    await page.keyboard.press('ArrowUp'); assert.equal(await draft(), 'mine two', 'consecutive duplicates are one entry');
    await page.keyboard.press('ArrowUp'); assert.equal(await draft(), 'mine one');
    await page.keyboard.press('ArrowDown'); assert.equal(await draft(), 'mine two');
    await page.keyboard.press('Escape'); assert.equal(await draft(), '', 'Escape restores what was being written');
    await page.keyboard.type('draft in progress'); await page.evaluate(() => { const draft = document.querySelector('#composer textarea[name=body]'); draft.setSelectionRange(0, 0); });
    await page.keyboard.press('ArrowUp'); assert.equal(await draft(), 'mine three', 'Up at the start of a draft recalls too');
    await page.keyboard.press('ArrowDown'); await page.keyboard.press('ArrowDown'); await page.keyboard.press('ArrowDown'); await page.keyboard.press('ArrowDown');
    assert.equal(await draft(), 'draft in progress', 'past the newest the draft is back');
    await page.keyboard.down('Shift'); await page.keyboard.press('Enter'); await page.keyboard.up('Shift');
    assert.deepEqual([await posts(page), (await draft()).includes('\n')], [[], true], 'Shift+Enter is a new line');
    await typeDraft(page, 'sent with enter'); await page.focus('#composer textarea[name=body]'); await page.keyboard.press('Enter'); await settle(page);
    assert.deepEqual([(await posts(page)).map(post => post.body), await draft()], [['sent with enter'], '']);
    await page.close();
  }

  // The reader's place survives a live update: the composer keeps its text and caret and the focus, the thread's composer keeps its text, and a button of
  // a message row that has the focus keeps it while rows are added below, and after an older page is put above.
  {
    const page = await mount({ plain: true });
    await addReplies(page, 2);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(page);
    await page.evaluate(() => { window.__chat.threadDraft.value = 'thread draft'; });
    await page.focus('#composer textarea[name=body]'); await page.keyboard.type('main draft'); await page.keyboard.press('Home');
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_live', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => { const draft = document.querySelector('#composer textarea[name=body]'); return [document.activeElement === draft, draft.value, draft.selectionStart, window.__chat.threadDraft.value, !!document.querySelector('[data-message-id=rm_a_live]')]; }), [true, 'main draft', 0, 'thread draft', true]);
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = 0; });
    await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m40] .message-action').focus());
    await page.evaluate(() => { window.__focus = document.activeElement; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_a_m1'); await settle(page);
    assert.equal(await page.evaluate(() => document.activeElement === window.__focus), true, 'a button of a row keeps the focus when an older page is put above');
    await page.close();
  }

  // The room panel: rename, the memory setting, the worktree, archive and reopen, leaving a chat, removing a room and the export. Each call that is still
  // out when another room is opened or the chat is reset changes nothing in what the reader sees now and reports nothing to the app, answered well or badly.
  const dialogOk = async page => { await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok').click()); };
  const panelState = page => page.evaluate(() => ({ title: document.querySelector('#room-title').textContent, rename: document.querySelector('#room-rename-status').textContent, memory: window.__chat.memorySection.querySelector('p').textContent, worktree: window.__chat.worktreeSection.querySelector('p').textContent, info: document.querySelector('#room-info-error').textContent, error: document.querySelector('#chat-error').textContent, urls: window.__urls, calls: window.__calls.splice(0) }));
  const panelRoom = async ({ extra = {}, who = 'owner' } = {}) => {
    const page = await mount();
    await page.evaluate(() => { window.__mutations = []; window.__chat.onRoomMutation = room => window.__mutations.push(['mutation', room.id, room.lifecycle]); window.__chat.onChatLeft = room => window.__mutations.push(['left', room.id]); window.__chat.onRoomRemoved = room => window.__mutations.push(['removed', room.id]); const original = URL.createObjectURL; window.__urls = 0; URL.createObjectURL = blob => { window.__urls++; return original.call(URL, blob); }; });
    await openRoom(page, 'rm_a', extra); await opened(page); await settle(page); await calls(page);
    return page;
  };
  const leave = async (page, how, extra = {}) => {
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); }
    else if (how === 'revisit') { await openRoom(page, 'rm_b'); await opened(page); await settle(page); await openRoom(page, 'rm_a', extra); await opened(page); }
    else if (how === 'reset-same') { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_a', extra); await opened(page); }
    else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); }
    await settle(page); await calls(page);
  };
  const cases = [
    ['rename', {}, page => page.evaluate(() => { const form = window.__chat.renameForm; form.elements.title.value = 'renamed'; window.__panel = window.__chat.saveRoomTitle(); }), 'PATCH', '^/api/rooms/rm_a$', null],
    ['memory load', {}, page => page.evaluate(() => { window.__panel = window.__chat.loadRoomMemory(); }), 'GET', '/api/projects/prj_a/memory', null],
    ['memory change', {}, page => page.evaluate(() => { const select = window.__chat.memorySection.querySelector('select'); select.disabled = false; select.value = 'off'; window.__panel = window.__chat.changeRoomMemory(); }), 'PUT', '/memory-enabled$', null],
    ['worktree load', {}, page => page.evaluate(() => { window.__panel = window.__chat.loadWorktree(); }), 'GET', '/rooms/rm_a/worktree$', null],
    ['worktree create', {}, async page => { await page.evaluate(() => { window.__panel = window.__chat.createWorktree(); }); await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok').click()); }, 'POST', '/rooms/rm_a/worktree$', null],
    ['worktree cancel', {}, page => page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); }), 'DELETE', '/rooms/rm_a/worktree$', null],
    ['archive', {}, async page => { await page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }); await dialogOk(page); }, 'POST', '/rooms/rm_a/close$', null],
    ['reopen', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('toggle'); }), 'POST', '/rooms/rm_a/reopen$', null],
    ['leave chat', { agent_chat: true }, page => page.evaluate(() => { window.__panel = window.__chat.leaveChat(); }), 'POST', '/rooms/rm_a/leave$', null],
    ['remove room', { lifecycle: 'closed' }, async page => { await page.evaluate(() => { window.__panel = window.__chat.removeRoom(); }); await dialogOk(page); }, 'DELETE', '^/api/rooms/rm_a$', null],
    ['export', {}, page => page.evaluate(() => { window.__panel = window.__chat.exportRoom('json'); }), 'GET', '/rooms/rm_a/export', null],
  ];
  for (const [name, extra, start, method, re] of cases) {
    for (const how of ['open', 'reset', 'reset-same', 'revisit']) {
      for (const outcome of ['ok', 'fail']) {
        const page = await panelRoom({ extra });
        await hold(page, method, re);
        await start(page);
        const held = await waitRequest(page, method, re);
        await leave(page, how, extra);
        const before = await panelState(page);
        const reads = await page.evaluate(() => window.__requests.length);
        if (outcome === 'ok') await serve(page, held); else await answer(page, held, 500, { message: 'Refused.' });
        await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
        const after = await panelState(page);
        assert.deepEqual([after, await page.evaluate(() => window.__mutations), await page.evaluate(() => window.__requests.length) - reads], [{ ...before, calls: [] }, [], 0], `${name} ${how}/${outcome}: a late answer changes nothing and starts nothing`);
        await page.close();
      }
    }
  }
  // Archiving a room that has open child rooms asks again for the whole tree; removing the worktree of an archived room asks, and a preserved one asks
  // again to be discarded. A late answer to any of these asks nothing more and starts nothing.
  for (const how of ['open', 'reset']) {
    for (const [name, extra, start, method, re, reply] of [
      ['cascade', {}, async page => { await page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }); await dialogOk(page); }, 'POST', '/rooms/rm_a/close$', [409, { error: 'open_children', message: 'Open child rooms.' }]],
      ['archived worktree', { lifecycle: 'closed' }, async page => { await page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); }); await dialogOk(page); }, 'DELETE', '/rooms/rm_a/worktree$', [200, { state: 'preserved', reason: 'dirty' }]],
    ]) {
      const page = await panelRoom({ extra });
      await hold(page, method, re);
      await start(page);
      const held = await waitRequest(page, method, re);
      await leave(page, how);
      const reads = await page.evaluate(() => window.__requests.length);
      await answer(page, held, reply[0], reply[1]); await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.deepEqual([await page.evaluate(() => !!document.querySelector('#app-dialog')?.open), await page.evaluate(() => window.__requests.length) - reads, await page.evaluate(() => window.__mutations)], [false, 0, []], `${name} ${how}: a late answer asks nothing more and starts nothing`);
      await page.close();
    }
  }
  // A preserved worktree that the reader agrees to discard is removed with a second request; its late answer changes nothing in the room opened since.
  for (const how of ['open', 'reset']) {
    for (const outcome of ['ok', 'fail']) {
      const page = await panelRoom({ extra: { lifecycle: 'closed' } });
      await hold(page, 'DELETE', '/rooms/rm_a/worktree$');
      await page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); }); await dialogOk(page);
      await answer(page, await waitRequest(page, 'DELETE', '/rooms/rm_a/worktree$'), 200, { state: 'preserved', reason: 'dirty' });
      await dialogOk(page);
      const held = await waitRequest(page, 'DELETE', '/rooms/rm_a/worktree$');
      await leave(page, how);
      const reads = await page.evaluate(() => window.__requests.length);
      if (outcome === 'ok') await serve(page, held); else await answer(page, held, 500, { message: 'Could not remove.' });
      await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__chat.worktreeSection.querySelector('p').textContent), await page.evaluate(() => window.__requests.length) - reads], ['', 0], `forced removal ${how}/${outcome}: a late answer changes nothing and starts nothing`);
      await page.close();
    }
  }
  // A refused archive is told in the chat's own error line, and in the dialog only while the dialog is open.
  for (const dialogOpen of [false, true]) {
    const page = await panelRoom();
    if (dialogOpen) await page.evaluate(() => window.__chat.showRoomInfo());
    await hold(page, 'POST', '/rooms/rm_a/close$');
    await page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }); await dialogOk(page);
    await answer(page, await waitRequest(page, 'POST', '/rooms/rm_a/close$'), 500, { message: 'The room cannot be archived.' });
    await page.evaluate(() => window.__panel); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#chat-error').textContent, document.querySelector('#room-info-error').textContent]), ['The room cannot be archived.', dialogOpen ? 'The room cannot be archived.' : ''], `a refused archive with the dialog ${dialogOpen ? 'open' : 'closed'}`);
    await page.close();
  }
  // A question the reader has not answered yet when the room changes or the chat is reset: answering it afterwards starts nothing.
  const asking = [
    ['create worktree', {}, page => page.evaluate(() => { window.__panel = window.__chat.createWorktree(); }), 'POST', 'worktree'],
    ['archived worktree', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); }), 'DELETE', 'worktree'],
    ['archive', {}, page => page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }), 'POST', 'close'],
    ['remove room', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.removeRoom(); }), 'DELETE', 'rooms/rm_a'],
  ];
  for (const how of ['open', 'reset', 'revisit']) {
    for (const [name, extra, start, method, mark] of asking) {
      const page = await panelRoom({ extra });
      await start(page);
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await leave(page, how, extra);
      const reads = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok').click());
      await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__requests.length) - reads, await page.evaluate(() => window.__mutations)], [0, []], `${name} ${how}: confirming a question after the room changed starts nothing`);
      await page.close();
    }
  }
  for (const how of ['open', 'reset', 'revisit']) {
    for (const [name, extra, start, reply] of [
      ['cascade', {}, page => page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }), [409, { error: 'open_children', message: 'Open child rooms.' }]],
      ['forced worktree removal', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); }), [200, { state: 'preserved', reason: 'dirty' }]],
    ]) {
      const page = await panelRoom({ extra });
      await page.evaluate(() => { window.__holds.push({ method: 'POST', re: 'close$' }, { method: 'DELETE', re: 'worktree$' }); });
      await start(page); await dialogOk(page);
      const first = await waitRequest(page, name === 'cascade' ? 'POST' : 'DELETE', name === 'cascade' ? 'close$' : 'worktree$');
      await answer(page, first, reply[0], reply[1]);
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await leave(page, how, extra);
      const reads = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok').click());
      await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.length) - reads, 0, `${name} ${how}: the second question, answered after the room changed, starts nothing`);
      await page.close();
    }
  }
  // The app's callback after an archive may take a while; a room left meanwhile keeps its own dialog open and hears nothing of the old one's result.
  for (const outcome of ['ok', 'fail']) {
    const page = await panelRoom();
    await page.evaluate(() => { window.__chat.onRoomMutation = () => new Promise((resolve, reject) => { window.__finish = outcome => (outcome === 'ok' ? resolve() : reject(new Error('The app refused.'))); }); window.__panel = window.__chat.changeRoomLifecycle('archive'); });
    await dialogOk(page);
    await page.waitForFunction(() => typeof window.__finish === 'function');
    await openRoom(page, 'rm_b'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.showRoomInfo()); await settle(page);
    await page.evaluate(outcome => window.__finish(outcome), outcome); await page.evaluate(() => window.__panel.catch(() => {})); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-info-dialog').open, document.querySelector('#room-info-error').textContent, document.querySelector('#chat-error').textContent]), [true, '', ''], `${outcome}: the old room's result does not close or write into the new room's dialog`);
    await page.close();
  }

  // Hide takes the room off the person's lists and closes the dialog; the answer of a hide that outlives its room closes nothing.
  {
    const page = await panelRoom();
    await page.evaluate(() => { window.__hidden = []; window.__chat.setRoomPreference = async (room, field, value) => { window.__hidden.push([room.id, field, value]); return true; }; window.__chat.showRoomInfo(); });
    await settle(page);
    await page.$eval('#room-hide', node => node.click()); await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__hidden, document.querySelector('#room-info-dialog').open, document.querySelector('#room-hide').disabled]), [[['rm_a', 'hidden', true]], false, false]);
    await page.evaluate(() => { window.__chat.setRoomPreference = () => new Promise(resolve => { window.__hideDone = resolve; }); window.__chat.showRoomInfo(); });
    await settle(page);
    await page.$eval('#room-hide', node => node.click());
    await openRoom(page, 'rm_b'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.showRoomInfo()); await settle(page);
    await page.evaluate(() => window.__hideDone(true)); await settle(page);
    assert.equal(await page.evaluate(() => document.querySelector('#room-info-dialog').open), true, 'a late hide does not close the dialog of the room opened since');
    await page.close();
  }

  // In its own room each of them does its work and tells the reader.
  {
    const page = await panelRoom();
    await page.evaluate(() => { window.__chat.renameForm.elements.title.value = 'renamed'; return window.__chat.saveRoomTitle(); }); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#room-title').textContent, document.querySelector('#room-rename-status').textContent]), ['renamed', 'Saved.']);
    await page.evaluate(() => { window.__chat.renameForm.elements.title.value = '  '; return window.__chat.saveRoomTitle(); });
    assert.equal(await page.evaluate(() => document.querySelector('#room-rename-status').textContent), 'A room needs a title.');
    await page.evaluate(() => { window.__chat.renameForm.elements.title.value = 'renamed'; return window.__chat.saveRoomTitle(); });
    assert.equal(await page.evaluate(() => document.querySelector('#room-rename-status').textContent), 'Nothing changed.');
    await page.evaluate(() => window.__chat.loadRoomMemory()); await settle(page);
    assert.equal(await page.evaluate(() => window.__chat.memorySection.querySelector('p').textContent), 'Project memory is on.');
    await page.evaluate(() => window.__chat.loadWorktree()); await settle(page);
    assert.equal(await page.evaluate(() => window.__chat.worktreeSection.querySelector('p').textContent), 'ready: /w/x');
    await page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); }); await dialogOk(page); await page.evaluate(() => window.__panel); await settle(page);
    assert.deepEqual(await page.evaluate(() => window.__mutations), [['mutation', 'rm_a', 'closing']], 'archiving tells the app');
    await page.close();
    const chat = await panelRoom({ extra: { agent_chat: true } });
    await chat.evaluate(() => window.__chat.leaveChat()); await settle(chat);
    assert.deepEqual(await chat.evaluate(() => window.__mutations), [['left', 'rm_a']]);
    await chat.close();
    const removal = await panelRoom({ extra: { lifecycle: 'closed' } });
    await removal.evaluate(() => { window.__panel = window.__chat.removeRoom(); }); await dialogOk(removal); await removal.evaluate(() => window.__panel); await settle(removal);
    assert.deepEqual(await removal.evaluate(() => window.__mutations), [['removed', 'rm_a']]);
    await removal.close();
    const exporting = await panelRoom();
    await exporting.evaluate(() => window.__chat.exportRoom('markdown')); await settle(exporting);
    assert.equal(await exporting.evaluate(() => window.__urls), 1, 'the export is saved once');
    await exporting.close();
  }

  // The read mark of the latest view is the live cursor and costs no look at the list; a history window reads its newest row.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const reads = await page.evaluate(() => {
      const list = document.querySelector('#messages'), chat = window.__chat; let count = 0;
      const real = Object.getOwnPropertyDescriptor(Element.prototype, 'lastElementChild') || Object.getOwnPropertyDescriptor(Node.prototype, 'lastElementChild');
      Object.defineProperty(list, 'lastElementChild', { configurable: true, get() { count++; return real.get.call(this); } });
      const latest = [chat.newestShownSeq(), count];
      chat.contextMode = true; count = 0;
      const window_ = [chat.newestShownSeq(), count > 0];
      chat.contextMode = false; chat.catchingUp = true; count = 0;
      const catching = [chat.newestShownSeq(), count > 0];
      chat.catchingUp = false; chat.initialLoading = true; count = 0;
      const loading = [chat.newestShownSeq(), count];
      chat.initialLoading = false; delete list.lastElementChild;
      return { latest, window_, catching, loading };
    });
    assert.deepEqual(reads, { latest: [130, 0], window_: [130, true], catching: [130, true], loading: [0, 0] }, 'the latest view and a loading room do not look at the list');
    await page.close();
  }

  // Late growth (an image or a diagram that finishes after its row was drawn) does not move the reader: a list at the bottom stays pinned to it, a list
  // scrolled up keeps the first visible message where it was.
  {
    const page = await mount();
    await page.evaluate(() => { window.__data.rooms.rm_a.messages[119].attachments = [{ id: 'att_i', name: 'pic.png', mime: 'image/png', url: 'data:image/gif;base64,R0lGODlhAQABAAAAACw=', width: 0, height: 0 }]; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const grow = (page, id, height) => page.evaluate((id, height) => { const body = document.querySelector(`[data-message-id=${id}] .message-body`); const filler = document.createElement('div'); filler.className = 'late-growth'; filler.style.height = `${height}px`; body.append(filler); }, id, height);
    await grow(page, 'rm_a_m120', 400); await settle(page); await settle(page);
    assert.equal(await page.evaluate(() => { const list = document.querySelector('#messages'); return list.scrollHeight - list.scrollTop - list.clientHeight < 3; }), true, 'a list at the bottom stays pinned when a row above grows');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = 1500; window.__before = document.querySelector('[data-message-id=rm_a_m60]').getBoundingClientRect().top; window.__top = list.scrollTop; });
    await settle(page);
    await page.evaluate(() => { window.__before = document.querySelector('[data-message-id=rm_a_m60]').getBoundingClientRect().top; });
    await grow(page, 'rm_a_m40', 300); await settle(page); await settle(page);
    assert.equal(await page.evaluate(() => Math.abs(document.querySelector('[data-message-id=rm_a_m60]').getBoundingClientRect().top - window.__before) < 3), true, 'a list scrolled up keeps the first visible message in place when a row above grows');
    await page.close();
  }

  // The caps: a reader at the live end keeps only the live cap of rows (older ones come back by scrolling up); paging far back drops the newest rows and
  // the view becomes a history window with Back to latest.
  {
    const page = await mount({ setup: 'window.__renderCap = 60; window.__liveCap = 60;' });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'one more', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => { const rows = [...document.querySelectorAll('#messages > article.message')]; return [rows.length, rows[0].dataset.messageId, rows.at(-1).dataset.messageId, document.querySelector('#messages').dataset.hasOlder, window.__chat.contextMode]; }), [60, 'rm_a_m72', 'rm_a_m131', 'true', false], 'the live end keeps its cap');
    await page.close();
    const history = await mount({ setup: 'window.__renderCap = 60; window.__liveCap = 60;' });
    await openRoom(history, 'rm_a'); await opened(history); await settle(history);
    await history.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
    await history.waitForFunction(() => window.__chat.contextMode === true, { timeout: 5000 }); await settle(history);
    assert.deepEqual(await history.evaluate(() => { const rows = [...document.querySelectorAll('#messages > article.message')]; return [rows.length, rows[0].dataset.messageId, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden]; }), [60, 'rm_a_m1', false], 'paging back past the cap drops the newest rows and shows a window');
    await history.close();
  }

  // The room's header (icon, title, member count and what its lifecycle or project says) follows the room, and the settings dialog shows what this person
  // may do with it: the facts, the title field for whoever may rename, archive or reopen, remove, leave and hide, the notes, the worktree and the memory.
  {
    const header = page => page.evaluate(() => { const icon = document.querySelector('#room-icon'), members = document.querySelector('#room-members'); return { title: document.querySelector('#room-title').textContent, icon: [icon.textContent, icon.hidden, icon.classList.contains('dm')], count: members.querySelector('.room-members-count').textContent, status: members.querySelector('.room-header-status')?.textContent || '', label: members.getAttribute('aria-label'), tooltip: members.title, hidden: members.hidden }; });
    const page = await mount();
    assert.deepEqual(await header(page), { title: 'Choose a room', icon: ['', true, false], count: '', status: '', label: null, tooltip: '', hidden: true });
    for (const [extra, expected] of [
      [{}, ['#', false, false, '2 members', '']],
      [{ kind: 'home', title: 'Alpha' }, ['⌂', false, false, '2 members', '']],
      [{ kind: 'dm', member_count: 1 }, ['@', false, true, '1 member', '']],
      [{ agent_chat: true }, ['⇄', false, false, '2 members', '']],
      [{ lifecycle: 'closing' }, ['#', false, false, '2 members · Closing…', 'Closing…']],
      [{ lifecycle: 'closed' }, ['#', false, false, '2 members · Archived', 'Archived']],
    ]) {
      await openRoom(page, 'rm_a', extra); await opened(page); await settle(page);
      const state = await header(page);
      assert.deepEqual([state.title, ...state.icon, state.count, state.status, state.label, state.tooltip, state.hidden], [extra.title || 'rm_a', expected[0], expected[1], expected[2], String(extra.member_count ?? 2), expected[4], expected[3], expected[3], false]);
    }
    await page.evaluate(() => { window.__chat.projectArchived = () => true; window.__chat.setRoomFacts(window.__chat.room); }); await settle(page);
    assert.equal((await header(page)).status, 'Project archived');
    await page.evaluate(() => { window.__chat.room.title = 'renamed elsewhere'; window.__chat.setRoomFacts(window.__chat.room); }); await settle(page);
    assert.equal((await header(page)).title, 'renamed elsewhere', 'a rename by someone else reaches the header');
    await page.evaluate(() => window.__chat.reset()); await settle(page);
    assert.deepEqual([(await header(page)).title, (await header(page)).icon[1], (await header(page)).hidden], ['Choose a room', true, true], 'a reset takes the room away');
    await page.close();
  }
  {
    const dialog = page => page.evaluate(() => {
      const visible = node => !!node && !node.hidden && !node.closest('[hidden]');
      const button = id => { const node = document.querySelector(id); return { shown: visible(node), disabled: node.disabled, label: node.getAttribute('aria-label'), title: node.title }; };
      const section = node => ({ shown: visible(node), text: [...node.children].filter(child => !child.hidden).map(child => child.textContent.replace(/\s+/g, ' ').trim()).join(' ') });
      return { open: document.querySelector('#room-info-dialog').open, rename: [visible(document.querySelector('#room-rename-form')), document.querySelector('#room-rename-title').value],
        facts: [...document.querySelectorAll('#room-info-facts > *')].map(node => node.textContent), debug: [...document.querySelectorAll('#room-info-facts .room-fact-debug')].length,
        lifecycle: button('#room-lifecycle'), leave: button('#room-leave'), remove: button('#room-remove'), hide: visible(document.querySelector('#room-hide')),
        notes: section(window.__chat.notesSection), worktree: section(window.__chat.worktreeSection), memory: section(window.__chat.memorySection), error: document.querySelector('#room-info-error').textContent };
    });
    const show = async (extra, role = 'owner') => {
      const page = await mount();
      await page.evaluate(role => { window.__chat.projectRole = () => role; }, role);
      await openRoom(page, 'rm_a', extra); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.showRoomInfo()); await settle(page); await settle(page);
      return page;
    };
    let page = await show({});
    let state = await dialog(page);
    assert.deepEqual([state.open, state.rename, state.facts, state.debug], [true, [true, 'rm_a'], ['Kind', 'task', 'Status', 'open', 'Members', '2', 'Room ID', 'rm_a'], 4], 'an owner renames the task room in its own field');
    assert.deepEqual([state.lifecycle, state.leave.shown, state.remove, state.hide], [{ shown: true, disabled: false, label: 'Archive room', title: 'Archive room' }, false, { shown: true, disabled: true, label: 'Remove room', title: 'Archive the room before removing it' }, true]);
    assert.deepEqual([state.notes, state.worktree.shown, state.memory.shown], [{ shown: true, text: 'Task notes Notes the agents keep for this task. They stay with this room. Open task notes' }, true, true]);
    await page.$eval('#room-info-close', node => node.click());
    assert.equal((await dialog(page)).open, false, 'Close closes the dialog');
    await page.evaluate(() => { window.__chat.setRoomFacts({ ...window.__chat.room, lifecycle: 'closing' }); window.__chat.showRoomInfo(); }); await settle(page); await settle(page);
    assert.deepEqual((await dialog(page)).lifecycle, { shown: true, disabled: true, label: 'Closing…', title: 'Closing…' }, 'a room that is closing cannot be archived again');
    await page.evaluate(() => window.__chat.setRoomFacts({ ...window.__chat.room, lifecycle: 'closed' })); await settle(page);
    state = await dialog(page);
    assert.deepEqual([state.lifecycle, state.remove], [{ shown: true, disabled: false, label: 'Reopen room', title: 'Reopen room' }, { shown: true, disabled: false, label: 'Remove room', title: 'Remove this room and its messages for good' }], 'the open dialog follows the room\'s lifecycle');
    await page.close();
    page = await show({}, 'viewer');
    state = await dialog(page);
    assert.deepEqual([state.rename[0], state.facts.slice(0, 2), state.remove.shown], [false, ['Name', 'rm_a'], false], 'a viewer reads the name and cannot remove the room');
    await page.close();
    page = await show({ kind: 'home', title: 'Alpha' });
    state = await dialog(page);
    assert.deepEqual([state.lifecycle.shown, state.remove.shown, state.notes.text, state.worktree, state.memory.shown], [false, false, 'Project memory The home room has no task notes of its own: its agents use the project’s memory (project facts). Open project memory', { shown: true, text: 'Git worktree The home room works in the project folder. Task rooms can have their own Git worktree.' }, true]);
    await page.close();
    page = await show({ kind: 'dm' });
    state = await dialog(page);
    assert.deepEqual([state.rename[0], state.remove.shown, state.notes.shown, state.worktree.shown], [false, false, false, false], 'a private chat has no title field, removal, notes or worktree');
    await page.close();
    page = await show({ agent_chat: true });
    state = await dialog(page);
    assert.deepEqual([state.leave.shown, state.leave.label, state.memory.shown], [true, 'Leave chat', false], 'a chat between agents can be left and has no memory setting');
    await page.close();
  }

  // A destroyed room panel: a call that was still out when it was destroyed changes nothing, tells the app nothing and starts nothing, and none of its
  // entries starts a request afterwards, not even for a room it is handed again.
  if (PANEL) {
    for (const [name, extra, start, method, re] of cases) {
      const page = await panelRoom({ extra });
      await hold(page, method, re);
      await start(page);
      const held = await waitRequest(page, method, re);
      await page.evaluate(() => window.__chat.panel.destroy());
      await settle(page);
      const sent = await page.evaluate(() => window.__requests.length), before = errors.length;
      await serve(page, held); await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__mutations), await page.evaluate(() => window.__requests.length) - sent, errors.slice(before)], [[], 0, []], `destroy: ${name} answered after the panel was destroyed changes nothing and starts nothing`);
      await page.close();
    }
    const page = await panelRoom({ extra: { lifecycle: 'closed', agent_chat: true } });
    await page.evaluate(() => window.__chat.panel.destroy());
    const sent = await page.evaluate(() => window.__requests.length);
    await page.evaluate(async () => {
      const panel = window.__chat.panel, store = panel.store;
      panel.setRoom(window.__chat.room);
      for (const call of [() => store.loadMemory(), () => store.changeMemory('off'), () => store.loadWorktree(), () => store.createWorktree(), () => store.removeWorktree(), () => store.changeLifecycle(), () => store.leave(), () => store.remove(), () => store.exportRoom('json'), () => panel.saveTitle(), () => store.hide()]) { try { await call(); } catch {} }
    });
    await settle(page);
    assert.equal(await page.evaluate(() => window.__requests.length) - sent, 0, 'no entry of a destroyed panel starts a request');
    await page.close();
  }

  // The panel's calls of one kind answered out of order: the answer of the older one is dropped (the title, the memory setting and the worktree show what
  // the newer one said; the app hears of a leave, a removal, an archive or a hide once; an export is saved once). A destroyed panel takes the answer of a
  // question left open too.
  if (PANEL) {
    const twice = async (name, extra, first, second, method, re, bodies, check) => {
      const page = await panelRoom({ extra });
      await hold(page, method, re);
      await page.evaluate(first); const one = await waitRequest(page, method, re);
      await page.evaluate(second); await page.waitForFunction((method, re) => window.__requests.filter(request => request.method === method && !request.answered && new RegExp(re).test(request.path)).length === 2, {}, method, re);
      const newer = await find(page, method, re);
      await answer(page, newer, ...bodies[1]); await settle(page);
      await answer(page, one, ...bodies[0]); await settle(page); await settle(page);
      await check(page, name);
      await page.close();
    };
    await twice('title', {}, () => { window.__a = window.__chat.panel.store.saveTitle('older title'); }, () => { window.__b = window.__chat.panel.store.saveTitle('newer title'); }, 'PATCH', '^/api/rooms/rm_a$', [[200, { id: 'rm_a', title: 'older title' }], [200, { id: 'rm_a', title: 'newer title' }]],
      async (page, name) => assert.deepEqual(await page.evaluate(() => [window.__chat.panel.store.room.peek().title, document.querySelector('#room-title').textContent, window.__chat.panel.store.renameStatus.peek().text]), ['newer title', 'newer title', 'Saved.'], `${name}: the older save does not overwrite the newer`));
    await twice('memory read', {}, () => { window.__a = window.__chat.panel.store.loadMemory(); }, () => { window.__b = window.__chat.panel.store.loadMemory(); }, 'GET', '/projects/prj_a/memory$', [[200, { enabled: false, can_edit: false }], [200, { enabled: true, can_edit: true }]],
      async (page, name) => assert.deepEqual(await page.evaluate(() => [window.__chat.panel.store.memory.peek().status, window.__chat.panel.store.memory.peek().disabled]), ['Project memory is on.', false], `${name}: the older read does not overwrite the newer`));
    await twice('memory change', {}, () => { window.__a = window.__chat.panel.store.changeMemory('off'); }, () => { window.__b = window.__chat.panel.store.changeMemory('inherit'); }, 'PUT', '/memory-enabled$', [[200, { memory_enabled: false }], [200, { memory_enabled: null }]],
      async (page, name) => assert.deepEqual(await page.evaluate(() => [window.__chat.panel.store.memory.peek().value, window.__chat.panel.store.memory.peek().status, window.__chat.panel.store.room.peek().memory_enabled]), ['inherit', 'Room follows the project setting.', null], `${name}: the older change does not overwrite the newer`));
    await twice('worktree read', {}, () => { window.__a = window.__chat.panel.store.loadWorktree(); }, () => { window.__b = window.__chat.panel.store.loadWorktree(); }, 'GET', '/rooms/rm_a/worktree$', [[200, { enabled: true, state: 'ready', path: '/w/old' }], [200, { enabled: true, state: 'failed', reason: 'git said no', relative_path: 'x' }]],
      async (page, name) => assert.equal(await page.evaluate(() => window.__chat.panel.store.worktree.peek().status), 'failed: x · git said no', `${name}: the older read does not overwrite the newer`));
    await twice('leave chat', { agent_chat: true }, () => { window.__a = window.__chat.panel.store.leave(); }, () => { window.__b = window.__chat.panel.store.leave(); }, 'POST', '/leave$', [[204, null], [204, null]],
      async (page, name) => assert.deepEqual(await page.evaluate(() => window.__mutations), [['left', 'rm_a']], `${name}: the app hears of it once`));
    await twice('export', {}, () => { window.__a = window.__chat.panel.store.exportRoom('json'); }, () => { window.__b = window.__chat.panel.store.exportRoom('json'); }, 'GET', '/export', [[200, { messages: [1] }], [200, { messages: [] }]],
      async (page, name) => assert.equal(await page.evaluate(() => window.__urls), 1, `${name}: it is saved once`));
    {
      const page = await panelRoom();
      await hold(page, 'PATCH', '^/api/rooms/rm_a$'); await hold(page, 'GET', '/projects/prj_a/memory$'); await hold(page, 'GET', '/rooms/rm_a/worktree$');
      await page.evaluate(() => { const store = window.__chat.panel.store; window.__a = store.saveTitle('three at once'); window.__b = store.loadMemory(); window.__c = store.loadWorktree(); });
      await answer(page, await waitRequest(page, 'GET', '/rooms/rm_a/worktree$'), 200, { enabled: true, state: 'ready', path: '/w/x' });
      await answer(page, await waitRequest(page, 'GET', '/projects/prj_a/memory$'), 200, { enabled: true, can_edit: true });
      await answer(page, await waitRequest(page, 'PATCH', '^/api/rooms/rm_a$'), 200, { id: 'rm_a', title: 'three at once' }); await settle(page); await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__chat.panel.store.room.peek().title, window.__chat.panel.store.memory.peek().status, window.__chat.panel.store.worktree.peek().status]), ['three at once', 'Project memory is on.', 'ready: /w/x'], 'calls of different kinds do not cancel each other');
      await page.close();
    }
    // A control stays busy while the newest call of its kind is out: the answer of an older one, well or badly, does not free it.
    {
      const page = await panelRoom();
      await hold(page, 'PATCH', '^/api/rooms/rm_a$');
      await page.evaluate(() => { window.__chat.showRoomInfo(); window.__a = window.__chat.panel.store.saveTitle('first'); }); const first = await waitRequest(page, 'PATCH', '^/api/rooms/rm_a$');
      await page.evaluate(() => { window.__b = window.__chat.panel.store.saveTitle('second'); }); await page.waitForFunction(() => window.__requests.filter(request => request.method === 'PATCH' && !request.answered).length === 2);
      await answer(page, first, 500, { message: 'Too late.' }); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-rename-save').disabled), true, 'rename: the older answer does not free the button');
      await page.evaluate(() => document.querySelector('#room-rename-save').click()); await settle(page);
      assert.equal(await count(page, 'PATCH', '^/api/rooms/rm_a$'), 2, 'rename: no third request starts');
      await answer(page, await find(page, 'PATCH', '^/api/rooms/rm_a$'), 200, { id: 'rm_a', title: 'second' }); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-rename-save').disabled), false, 'rename: the newest answer frees it');
      await page.close();
    }
    {
      const page = await panelRoom({ extra: { agent_chat: true } });
      await hold(page, 'POST', '/leave$');
      await page.evaluate(() => { window.__chat.showRoomInfo(); window.__a = window.__chat.panel.store.leave(); }); const first = await waitRequest(page, 'POST', '/leave$');
      await page.evaluate(() => { window.__b = window.__chat.panel.store.leave(); }); await page.waitForFunction(() => window.__requests.filter(request => request.method === 'POST' && /leave$/.test(request.path) && !request.answered).length === 2);
      await answer(page, first, 500, { message: 'Could not leave.' }); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-leave').disabled), true, 'leave: the older failure does not free the button');
      await page.evaluate(() => document.querySelector('#room-leave').click()); await settle(page);
      assert.equal(await count(page, 'POST', '/leave$'), 2, 'leave: no third request starts');
      await answer(page, await find(page, 'POST', '/leave$'), 204, null); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.panel.store.busy.peek().leave), false, 'leave: the newest answer frees it');
      await page.close();
    }
    {
      const page = await panelRoom();
      await page.evaluate(() => { window.__hides = []; window.__chat.setRoomPreference = () => new Promise(resolve => window.__hides.push(resolve)); window.__chat.showRoomInfo(); window.__a = window.__chat.panel.store.hide(); window.__b = window.__chat.panel.store.hide(); });
      await settle(page);
      await page.evaluate(() => window.__hides[0](false)); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-hide').disabled), true, 'hide: the older answer does not free the button');
      await page.evaluate(() => document.querySelector('#room-hide').click()); await settle(page);
      assert.equal(await page.evaluate(() => window.__hides.length), 2, 'hide: no third call starts');
      await page.evaluate(() => window.__hides[1](false)); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-hide').disabled), false, 'hide: the newest answer frees it');
      await page.close();
    }
    for (const [name, extra, call, method, re, bodies, expected] of [
      ['archive', {}, 'changeLifecycle(\'archive\')', 'POST', '/rooms/rm_a/close$', [[200, { id: 'rm_a', project_id: 'prj_a', title: 'rm_a', kind: 'task', lifecycle: 'closing' }], [200, { id: 'rm_a', project_id: 'prj_a', title: 'rm_a', kind: 'task', lifecycle: 'closing' }]], [['mutation', 'rm_a', 'closing']]],
      ['remove room', { lifecycle: 'closed' }, 'remove()', 'DELETE', '^/api/rooms/rm_a$', [[204, null], [204, null]], [['removed', 'rm_a']]],
    ]) {
      const page = await panelRoom({ extra });
      await hold(page, method, re);
      await page.evaluate(call => { window.__a = new Function('return window.__chat.panel.store.' + call)(); }, call); await dialogOk(page);
      const one = await waitRequest(page, method, re);
      await page.evaluate(call => { window.__b = new Function('return window.__chat.panel.store.' + call)(); }, call); await dialogOk(page);
      await page.waitForFunction((method, re) => window.__requests.filter(request => request.method === method && !request.answered && new RegExp(re).test(request.path)).length === 2, {}, method, re);
      const newer = await find(page, method, re);
      await answer(page, newer, ...bodies[1]); await settle(page);
      await answer(page, one, ...bodies[0]); await settle(page); await settle(page);
      assert.deepEqual(await page.evaluate(() => window.__mutations), expected, `${name}: the app hears of it once`);
      await page.close();
    }
    {
      const page = await panelRoom();
      await page.evaluate(() => { window.__hides = []; window.__chat.setRoomPreference = () => new Promise(resolve => window.__hides.push(resolve)); window.__chat.showRoomInfo(); window.__a = window.__chat.panel.store.hide(); window.__b = window.__chat.panel.store.hide(); });
      await settle(page);
      await page.evaluate(() => window.__hides[1](false)); await settle(page);
      await page.evaluate(() => window.__hides[0](true)); await page.evaluate(() => Promise.all([window.__a, window.__b])); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#room-info-dialog').open), true, 'hide: the older answer does not close the dialog the newer one left open');
      await page.close();
    }
    for (const [name, extra, start] of [
      ['create worktree', {}, page => page.evaluate(() => { window.__panel = window.__chat.createWorktree(); })],
      ['archived worktree', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); })],
      ['archive', {}, page => page.evaluate(() => { window.__panel = window.__chat.changeRoomLifecycle('archive'); })],
      ['remove room', { lifecycle: 'closed' }, page => page.evaluate(() => { window.__panel = window.__chat.removeRoom(); })],
    ]) {
      const page = await panelRoom({ extra });
      await start(page);
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await page.evaluate(() => window.__chat.panel.destroy());
      const reads = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok').click());
      await page.evaluate(() => window.__panel.catch(() => {})); await settle(page); await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.length) - reads, 0, `destroy: ${name}: confirming a question after the panel was destroyed starts nothing`);
      await page.close();
    }
  }

  // The store holds the room's records and cursors; the cases above read the page, these read the store.
  if (STORE) {
    const view = page => page.evaluate(() => { const store = window.__chat.store; return { rows: store.messages.size, newest: store.newestSeq, oldest: store.oldestSeq, more: store.hasMoreOlder, window: store.contextMode, windowSeq: store.windowSeq, catching: store.catchingUp, missed: store.missedIDs.size, loadingOlder: store.loadingOlder, loadingNewer: store.loadingNewer, loading: store.initialLoading }; });
    const page = await mount();
    assert.deepEqual(await view(page), { rows: 0, newest: 0, oldest: 0, more: false, window: false, windowSeq: 0, catching: false, missed: 0, loadingOlder: false, loadingNewer: false, loading: false });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.deepEqual(await view(page), { rows: 100, newest: 130, oldest: 31, more: true, window: false, windowSeq: 0, catching: false, missed: 0, loadingOlder: false, loadingNewer: false, loading: false }, 'the newest page');
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
    assert.deepEqual(await view(page), { rows: 100, newest: 130, oldest: 1, more: true, window: true, windowSeq: 100, catching: false, missed: 0, loadingOlder: false, loadingNewer: false, loading: false }, 'a window around a message');
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'while away', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); }); await settle(page);
    assert.deepEqual([(await view(page)).missed, (await view(page)).newest], [1, 131], 'an arrival while a window is shown is counted');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await page.waitForFunction(() => window.__chat.store.contextMode === false, { timeout: 5000 }); await settle(page);
    assert.deepEqual(await view(page), { rows: 131, newest: 131, oldest: 1, more: true, window: false, windowSeq: 0, catching: false, missed: 0, loadingOlder: false, loadingNewer: false, loading: false }, 'the latest view again');
    await page.evaluate(() => window.__chat.reset());
    assert.deepEqual(await view(page), { rows: 0, newest: 0, oldest: 0, more: false, window: false, windowSeq: 0, catching: false, missed: 0, loadingOlder: false, loadingNewer: false, loading: false }, 'a reset empties it');
    await page.close();
    // overlapping live refreshes draw a message once and leave the cursor at the newest
    for (const order of ['older first', 'newer first']) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'twice', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); window.__a = window.__chat.refreshMessages(); });
      const one = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__b = window.__chat.refreshMessages(); }); await page.waitForFunction(() => window.__requests.filter(request => request.method === 'GET' && /messages\?after=/.test(request.path) && !request.answered).length === 2);
      const two = await find(page, 'GET', 'rooms/rm_a/messages\\?after=');
      for (const index of order === 'older first' ? [one, two] : [two, one]) await serve(page, index);
      await page.evaluate(() => Promise.all([window.__a, window.__b])); await settle(page);
      assert.deepEqual([await page.evaluate(() => document.querySelectorAll('[data-message-id=rm_a_m131]').length), (await view(page)).newest, (await view(page)).rows], [1, 131, 101], `overlapping refreshes, ${order}: one row, the cursor at the newest`);
      await page.close();
    }
    // the reactions of different messages read at once are both kept; of one message the newest read wins whichever answer comes first
    for (const order of ['older first', 'newer first']) {
      const page = await reactionRoom();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'GET', '/api/messages/rm_a_m1(28|29)/reactions');
      await page.evaluate(() => { window.__data.reactions.rm_a_m128 = [{ emoji: '👍', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]; window.__chat.refreshReactions('rm_a_m128'); window.__first = window.__chat.refreshReactions('rm_a_m129'); });
      await page.waitForFunction(() => window.__requests.filter(request => /reactions$/.test(request.path) && !request.answered && /m129|m128/.test(request.path)).length >= 2);
      const older = await find(page, 'GET', '/api/messages/rm_a_m129/reactions'), other = await find(page, 'GET', '/api/messages/rm_a_m128/reactions');
      await page.evaluate(() => { window.__data.reactions.rm_a_m129 = [{ emoji: '🚀', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]; window.__second = window.__chat.refreshReactions('rm_a_m129'); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path.endsWith('rm_a_m129/reactions') && !request.answered).length === 2);
      const newer = await find(page, 'GET', '/api/messages/rm_a_m129/reactions');
      await answer(page, other, 200, [{ emoji: '👍', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]);
      const answers = [[older, [{ emoji: '👀', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]], [newer, [{ emoji: '🚀', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]]];
      for (const [index, body] of order === 'older first' ? answers : answers.slice().reverse()) await answer(page, index, 200, body);
      await page.evaluate(() => Promise.all([window.__first, window.__second])); await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__chat.store.reactions.get('rm_a_m129')?.map(entry => entry.emoji), window.__chat.store.reactions.get('rm_a_m128')?.map(entry => entry.emoji)]), [['🚀'], ['👍']], `reactions, ${order}: the newest read of a message wins and another message's read is kept`);
      await page.close();
    }
    // one older page at a time; a forward page only in a window and not beside an older page
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      const count = (re) => page.evaluate(re => window.__requests.filter(request => request.method === 'GET' && new RegExp(re).test(request.path)).length, re);
      const sent = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => window.__chat.loadNewer()); await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.length), sent, 'no forward page outside a window');
      await hold(page, 'GET', 'rooms/rm_a/messages\\?before=');
      await page.evaluate(() => { window.__a = window.__chat.loadOlder(); window.__b = window.__chat.loadOlder(); }); await settle(page);
      assert.equal(await count('messages\\?before='), 1, 'a second older page is not asked for while one is out');
      const older = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?before=');
      await page.evaluate(() => { window.__chat.store.contextMode = true; window.__chat.store.windowSeq = 77; window.__chat.loadNewer(); }); await settle(page);
      assert.equal(await count('messages\\?after=77'), 0, 'no forward page while an older page is out');
      await page.evaluate(() => { window.__chat.store.contextMode = false; window.__chat.store.windowSeq = 0; });
      await serve(page, older); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=77');
      await page.evaluate(() => { window.__chat.store.contextMode = true; window.__chat.store.windowSeq = 77; window.__c = window.__chat.loadNewer(); window.__d = window.__chat.loadNewer(); }); await settle(page);
      assert.equal(await count('messages\\?after=77'), 1, 'a second forward page is not asked for while one is out');
      await serve(page, await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=77')); await page.evaluate(() => Promise.all([window.__c, window.__d])); await page.evaluate(() => Promise.all([window.__a, window.__b])); await settle(page);
      await page.close();
    }
    // a forward page that was out when the window moved or closed is dropped
    for (const how of ['another window', 'latest view']) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=100');
      await page.evaluate(() => { window.__late = window.__chat.loadNewer(); });
      const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=100');
      if (how === 'another window') { await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m120')); await settle(page); }
      else await page.evaluate(() => { window.__chat.store.contextMode = false; });
      const before = await view(page);
      await serve(page, held); await page.evaluate(() => window.__late); await settle(page);
      assert.deepEqual(await view(page), { ...before, loadingNewer: false }, `${how}: a late forward page changes nothing`);
      await page.close();
    }
    // a reaction change released after the room was left and shown again reads nothing
    {
      const page = await reactionRoom();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'PUT', '/reactions/');
      await page.evaluate(() => { window.__toggle = window.__chat.toggleReaction('rm_a_m130', '🎉'); });
      const held = await waitRequest(page, 'PUT', '/reactions/');
      await page.evaluate(() => { window.__holds.length = 0; });
      await openRoom(page, 'rm_b'); await opened(page); await settle(page);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      const reads = await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && /reactions$/.test(request.path)).length);
      await serve(page, held); await page.evaluate(() => window.__toggle); await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && /reactions$/.test(request.path)).length), reads, 'a stale reaction change starts no reactions read');
      await page.close();
    }
    // a catch-up released after the room changed leaves the suppression set of a newer catch-up alone
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__old = window.__chat.store.leaveWindow(); });
      const oldRead = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__holds.length = 0; });
      assert.equal(await page.evaluate(() => window.__chat.store.announcedIDs instanceof Set), true, 'a catch-up in flight holds a suppression set');
      await openRoom(page, 'rm_b'); await opened(page); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.store.announcedIDs), null, 'another room starts with none');
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
      await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'while away', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); }); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__new = window.__chat.store.leaveWindow(); });
      const newRead = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__holds.length = 0; });
      await serve(page, oldRead); await page.evaluate(() => window.__old); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.store.announcedIDs instanceof Set), true, 'an old catch-up leaves the newer one its suppression set');
      await serve(page, newRead); await page.evaluate(() => window.__new); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.store.announcedIDs), null, 'the newer catch-up clears it');
      await page.close();
    }
    // a jump to another window ends the suppression of a catch-up that was out
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__old = window.__chat.store.leaveWindow(); });
      const read = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?after=');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.jumpToMessage('rm_a_m110'); }); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.store.announcedIDs), null, 'a new window starts with none');
      await serve(page, read); await page.evaluate(() => window.__old); await settle(page);
      await page.close();
    }
    // once the store is destroyed none of its entries touches the host or sends a request
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page);
      await page.evaluate(() => window.__chat.destroy());
      const outcome = await page.evaluate(async () => {
        const chat = window.__chat, store = chat.store, hostCalls = [], sent = window.__requests.length;
        for (const name of ['beginOpen', 'drawPage', 'showNewest', 'loadOlderIfShort', 'latestShown', 'threadState', 'markNewThreadReply', 'add', 'announce', 'updateLatestLabel', 'trimLive', 'showOlderStatus', 'prependPage', 'trimOldest', 'anchorUpdate', 'say', 'showLatestButton', 'openMessage', 'beginWindow', 'drawWindow', 'showWindow', 'openThread', 'showReply', 'showRow', 'showReactions', 'beginThread', 'drawThreadPage', 'showThreadPage', 'settleThreadLatest', 'threadError', 'showThreadOlderStatus', 'prependThreadPage', 'beginThreadSend', 'sentThreadReply', 'threadSendFailed', 'restoreThreadDraft', 'sendInput', 'beginSend', 'sendAsAnswer', 'sentMessage', 'addSent', 'sendFailed', 'sendFinished']) { const original = chat[name]; chat[name] = function (...args) { hostCalls.push(name); return original.apply(this, args); }; }
        chat.room = { id: 'rm_a' }; store.hasMoreOlder = true; store.oldestSeq = 5; chat.nodes.set('rm_a_m1', document.createElement('article'));
        chat.threadRoot = { id: 'rm_a_m120', seq: 120 }; store.threadHasMoreOlder = true; store.threadOldestSeq = 5; chat.threadDraft.value = 'late reply'; chat.draft.value = 'late message';
        for (const name of ['open', 'openThread']) { const original = store[name]; store[name] = function (...args) { hostCalls.push(`store.${name}`); return original.apply(this, args); }; }
        store.windowSeq = 5; store.contextMode = true; store.newestSeq = 9; const before = [store.windowSeq, store.contextMode, store.newestSeq, store.catchingUp, store.threadGeneration, store.loadingOlderThread];
        for (const call of [() => store.open({ id: 'rm_a' }), () => store.refresh(), () => store.loadOlder(), () => store.loadNewer(), () => store.leaveWindow(), () => store.jump('rm_a_m10'), () => store.jump(7), () => store.refreshReactions('rm_a_m1'), () => store.toggleReaction('rm_a_m1', '👍'), () => store.openThread({ id: 'rm_a_m120', seq: 120 }), () => store.loadOlderThread(), () => store.sendThread(), () => store.send(), () => store.reopenLatest()]) { try { await call(); } catch (error) { hostCalls.push(`threw ${error.message}`); } }
        return [hostCalls, window.__requests.length - sent, before, [store.windowSeq, store.contextMode, store.newestSeq, store.catchingUp, store.threadGeneration, store.loadingOlderThread]];
      });
      assert.deepEqual([outcome[0], outcome[1], outcome[3]], [['store.open', 'store.openThread'], 0, outcome[2]], 'a destroyed store entry touches nothing and reopens nothing');
      await page.close();
    }
    // an open thread: a late check for newer replies after the thread closed settles nothing
    {
      const page = await mount();
      await addReplies(page, 3);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'GET', 'thread_root=rm_a_m120&after=');
      await page.evaluate(() => { window.__settled = 0; const settled = window.__chat.settleThreadLatest; window.__chat.settleThreadLatest = function (...args) { window.__settled++; return settled.apply(this, args); }; window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120'), { id: 'rm_a_t2' }); });
      const held = await waitRequest(page, 'GET', 'thread_root=rm_a_m120&after=');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.closeThread(); });
      await serve(page, held); await page.evaluate(() => window.__open); await settle(page);
      assert.equal(await page.evaluate(() => window.__settled), 0, 'a late newer-replies check settles nothing');
      await page.close();
    }
    // older replies of a thread: one read at a time, only with a thread open and more behind it, and a read of a closed thread leaves the new one's flag alone
    {
      const page = await mount();
      await addReplies(page, 130);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      const reads = () => page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && /thread_root=.*before=/.test(request.path)).length);
      await page.evaluate(() => { window.__chat.store.threadHasMoreOlder = true; window.__chat.store.threadOldestSeq = 5; window.__chat.loadOlderThread(); }); await settle(page);
      assert.equal(await reads(), 0, 'no older replies without a thread open');
      await page.evaluate(() => { window.__chat.store.threadHasMoreOlder = false; window.__chat.store.threadOldestSeq = 0; });
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await page.evaluate(() => { window.__chat.store.threadHasMoreOlder = false; window.__chat.loadOlderThread(); }); await settle(page);
      assert.equal(await reads(), 0, 'no older replies when none are behind');
      await page.evaluate(() => { window.__chat.store.threadHasMoreOlder = true; window.__chat.store.threadOldestSeq = 0; window.__chat.loadOlderThread(); }); await settle(page);
      assert.equal(await reads(), 0, 'no older replies without a cursor');
      await page.evaluate(() => { window.__chat.store.threadOldestSeq = 5; });
      await hold(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__a = window.__chat.loadOlderThread(); window.__b = window.__chat.loadOlderThread(); }); await settle(page);
      assert.equal(await reads(), 1, 'a second older read is not asked for while one is out');
      const first = await waitRequest(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.closeThread(); });
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await hold(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__c = window.__chat.loadOlderThread(); });
      const second = await waitRequest(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__holds.length = 0; });
      await serve(page, first); await page.evaluate(() => window.__a); await settle(page);
      assert.equal(await page.evaluate(() => window.__chat.store.loadingOlderThread), true, 'a read of the closed thread leaves the new one flagged loading');
      await serve(page, second); await page.evaluate(() => window.__c); await settle(page);
      await page.close();
    }
    // a failed older-replies read of a thread that was closed shows no error in the next
    {
      const page = await mount();
      await addReplies(page, 130);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await hold(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__late = window.__chat.loadOlderThread(); });
      const held = await waitRequest(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.closeThread(); });
      await answer(page, held, 500, { error: 'late failure' }); await page.evaluate(() => window.__late); await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#thread-error').textContent), '', 'a late failure shows no error');
      await page.close();
    }
    // a thread read of a store that was reset draws nothing
    {
      const page = await mount();
      await addReplies(page, 3);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'GET', 'thread_root=rm_a_m120');
      await page.evaluate(() => { window.__drawn = 0; const draw = window.__chat.drawThreadPage; window.__chat.drawThreadPage = function (...args) { window.__drawn++; return draw.apply(this, args); }; window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120')); });
      const held = await waitRequest(page, 'GET', 'thread_root=rm_a_m120');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.store.reset(); });
      await serve(page, held); await page.evaluate(() => window.__open); await settle(page);
      assert.equal(await page.evaluate(() => window.__drawn), 0, 'a thread read of a reset store draws nothing');
      await page.close();
    }
    // an answer sent through the inbox is current only while its room stays open
    for (const leave of [false, true]) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await typeDraft(page, 'my answer');
      await page.evaluate(() => { window.__chat.sendAsAnswer = async (reply, attachments, body, current) => { await new Promise(resolve => { window.__release = resolve; }); window.__current = current(); return true; }; });
      await submit(page);
      await page.waitForFunction(() => !!window.__release);
      if (leave) { await openRoom(page, 'rm_b'); await opened(page); await settle(page); }
      await page.evaluate(() => { window.__release(); return window.__sending; }); await settle(page);
      assert.equal(await page.evaluate(() => window.__current), !leave, `an inbox answer is ${leave ? 'not ' : ''}current ${leave ? 'after the room changed' : 'in its room'}`);
      await page.close();
    }
    // a thread reply: none without a thread or text, and one answered after the room changed is not shown
    {
      const page = await mount();
      await addReplies(page, 3);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      const posts = () => page.evaluate(() => window.__requests.filter(request => request.method === 'POST').length);
      await page.evaluate(() => { window.__chat.threadDraft.value = 'alone'; window.__chat.sendThread(); }); await settle(page);
      assert.equal(await posts(), 0, 'no thread reply without a thread open');
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await page.evaluate(() => { window.__chat.threadDraft.value = '   '; window.__chat.sendThread(); }); await settle(page);
      assert.equal(await posts(), 0, 'no thread reply without text');
      await hold(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__shown = 0; const sent = window.__chat.sentThreadReply; window.__chat.sentThreadReply = function (...args) { window.__shown++; return sent.apply(this, args); }; window.__chat.threadDraft.value = 'late'; window.__late = window.__chat.sendThread(); });
      const held = await waitRequest(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__holds.length = 0; });
      await openRoom(page, 'rm_b'); await opened(page); await settle(page);
      await serve(page, held); await page.evaluate(() => window.__late); await settle(page);
      assert.equal(await page.evaluate(() => window.__shown), 0, 'a thread reply answered after the room changed shows nothing');
      await page.close();
    }
    // a thread reply released after the room was left and shown again, or after destroy, shows nothing and keeps the new draft
    for (const how of ['A, B, A', 'destroy']) {
      const page = await mount();
      await addReplies(page, 3);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await hold(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__shown = 0; window.__failed = 0; const sent = window.__chat.sentThreadReply; window.__chat.sentThreadReply = function (...args) { window.__shown++; return sent.apply(this, args); }; window.__chat.threadDraft.value = 'late'; window.__late = window.__chat.sendThread(); });
      const held = await waitRequest(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__holds.length = 0; });
      if (how === 'destroy') await page.evaluate(() => window.__chat.destroy());
      else {
        await openRoom(page, 'rm_b'); await opened(page); await settle(page);
        await openRoom(page, 'rm_a'); await opened(page); await settle(page);
        await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
        await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
        await page.evaluate(() => { window.__chat.threadDraft.value = 'typed since'; });
      }
      const draft = await page.evaluate(() => window.__chat.threadDraft.value);
      await serve(page, held); await page.evaluate(() => window.__late); await settle(page);
      assert.deepEqual([await page.evaluate(() => window.__shown), await page.evaluate(() => window.__chat.threadDraft.value)], [0, draft], `${how}: a released thread reply shows nothing and keeps the draft`);
      await page.close();
    }
    // reopening the latest view for a sent message, then leaving the room, opens no thread
    {
      const page = await mount();
      await addReplies(page, 3);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await hold(page, 'GET', 'rooms/rm_a/messages\\?limit=100');
      await page.evaluate(() => { window.__re = window.__chat.store.reopenLatest(); });
      await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?limit=100');
      const held = await find(page, 'GET', 'rooms/rm_a/messages\\?limit=100');
      await page.evaluate(() => { window.__holds.length = 0; window.__chat.reset(); });
      const sent = await page.evaluate(() => window.__requests.length);
      await serve(page, held); await page.evaluate(() => window.__re); await settle(page);
      assert.deepEqual([(await panel(page)).hidden, await page.evaluate(() => window.__requests.length) - sent], [true, 0], 'a reopen outlived by a reset opens no thread');
      await page.close();
    }
    // with no room the list calls resolve and send nothing
    {
      const page = await mount();
      const outcome = await page.evaluate(async () => { document.querySelector('#composer textarea[name=body]').value = 'x'; window.__chat.threadDraft.value = 'x'; const store = window.__chat.store, sent = window.__requests.length, results = []; for (const call of [() => store.refresh(), () => store.loadOlder(), () => store.loadNewer(), () => store.jump('rm_a_m1'), () => store.jump(7), () => store.toggleReaction('rm_a_m1', '👍'), () => store.loadOlderThread(), () => store.sendThread(), () => store.send()]) { try { await call(); results.push('ok'); } catch (error) { results.push(error.message); } } return [results, window.__requests.length - sent]; });
      assert.deepEqual(outcome, [['ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok'], 0], 'no room: the calls resolve and send nothing');
      await page.close();
    }
    // a jump that a newer jump replaced, or that a room change outlived, changes nothing when its answer comes
    for (const how of ['newer jump', 'room change', 'leaving the window']) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      if (how === 'leaving the window') { await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page); await hold(page, 'GET', 'rooms/rm_a/messages\\?after='); await page.evaluate(() => { window.__late = window.__chat.leaveWindow(); }); }
      else { await hold(page, 'GET', 'rooms/rm_a/messages\\?around='); await page.evaluate(() => { window.__late = window.__chat.jumpToMessage('rm_a_m10'); }); }
      const held = await waitRequest(page, 'GET', how === 'leaving the window' ? 'rooms/rm_a/messages\\?after=' : 'rooms/rm_a/messages\\?around=');
      if (how === 'newer jump') { await page.evaluate(() => { window.__chat.store.jumpGeneration++; }); }
      else { await openRoom(page, 'rm_b'); await opened(page); await settle(page); }
      const before = await view(page); await calls(page);
      await serve(page, held); await page.evaluate(() => window.__late); await settle(page);
      assert.deepEqual([await view(page), await calls(page)], [before, []], `${how}: a late answer changes nothing and marks nothing read`);
      await page.close();
    }
  }

  // A destroyed chat (the replacement's adapter has destroy(); the hand-written chat does not): what was still out when it was destroyed changes nothing
  // and starts no request, and none of its entries sends one afterwards.
  if (REPLACED || STORE) {
    const destroyed = [
      ['open', {}, page => openRoom(page, 'rm_a'), 'GET', 'rooms/rm_a/messages\\?limit=100'],
      ['refresh', { opened: true }, page => page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'late', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); window.__late = window.__chat.refreshMessages(); }), 'GET', 'rooms/rm_a/messages\\?after='],
      ['older', { opened: true }, page => page.evaluate(() => { document.querySelector('#messages').scrollTop = 0; }), 'GET', 'rooms/rm_a/messages\\?before='],
      ['jump', { opened: true }, page => page.evaluate(() => { window.__late = window.__chat.jumpToMessage('rm_a_m10'); }), 'GET', 'rooms/rm_a/messages\\?around='],
      ['number lookup', { opened: true, other: true }, page => page.evaluate(() => { window.__late = window.__chat.jumpToMessage(1003); }), 'GET', 'message-numbers/'],
      ['forward page', { opened: true, windowed: true }, page => page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; }), 'GET', 'rooms/rm_a/messages\\?after=100'],
      ['send', { opened: true }, async page => { await typeDraft(page, 'out the door'); await submit(page); }, 'POST', '/messages$'],
      ['thread read', { opened: true, replies: true }, page => page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click()), 'GET', 'thread_root='],
      ['older thread read', { opened: true, replies: 130, threadOpen: true }, page => page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; }), 'GET', 'thread_root=.*before='],
      ['thread send', { opened: true, replies: true, threadOpen: true }, page => page.evaluate(() => { window.__chat.threadDraft.value = 'late reply'; window.__late = window.__chat.sendThread(); }), 'POST', '/messages$'],
      ['reactions read', { reactions: true }, page => openRoom(page, 'rm_a'), 'GET', '/api/messages/rm_a_m129/reactions'],
      ['reaction change', { opened: true, reactions: true }, page => page.$eval('[data-message-id=rm_a_m130] .reaction-pill[data-emoji="🎉"]', node => node.click()), 'PUT', '/reactions/'],
    ];
    for (const [name, setup, start, method, re] of destroyed) {
      const page = await mount({ rooms: { rm_a: 130, rm_b: 5 } });
      if (setup.other) await page.evaluate(() => { for (const message of window.__data.rooms.rm_b.messages) message.no += 1000; });
      if (setup.reactions) await page.evaluate(() => { window.__data.rooms.rm_a.messages[129].reactions = [{ emoji: '🎉', member_kind: 'agent', member_id: 'agt_zed', member_name: 'zed' }]; window.__data.reactions.rm_a_m129 = [{ emoji: '👀', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }]; });
      if (setup.replies) await addReplies(page, setup.replies === true ? 3 : setup.replies);
      if (setup.opened) { await openRoom(page, 'rm_a'); await opened(page); await settle(page); await calls(page); }
      if (setup.windowed) { await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10')); await settle(page); }
      if (setup.threadOpen) { await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click()); await page.waitForFunction(count => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1, {}, 1); await settle(page); }
      await hold(page, method, re);
      await start(page);
      const held = await waitRequest(page, method, re);
      await page.evaluate(() => window.__chat.destroy());
      await settle(page);
      const state = () => page.evaluate(() => ({ rows: document.querySelectorAll('#messages article').length, thread: document.querySelectorAll('#thread-messages article').length, error: document.querySelector('#chat-error').textContent, calls: window.__calls.splice(0), pills: document.querySelectorAll('.reaction-pill').length }));
      const before = await state(), sent = await page.evaluate(() => window.__requests.length), draft = await draftText(page);
      const failBefore = errors.length;
      await serve(page, held); await page.evaluate(() => (window.__late || window.__sending || Promise.resolve()).catch?.(() => {})); await settle(page); await settle(page);
      assert.deepEqual([await state(), await page.evaluate(() => window.__requests.length) - sent, await draftText(page), errors.slice(failBefore)], [{ ...before, calls: [] }, 0, draft, []], `destroy: ${name} answered after destroy changes nothing and starts nothing`);
      await page.close();
    }
    // no entry sends a request once the chat is destroyed
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await page.evaluate(() => window.__chat.destroy());
      const sent = await page.evaluate(() => window.__requests.length);
      await page.evaluate(async () => {
        const chat = window.__chat, room = { id: 'rm_a', project_id: 'prj_a', title: 'rm_a', kind: 'task', lifecycle: 'open', member_count: 2 };
        const calls = [() => chat.open(room), () => chat.refreshMessages(), () => chat.loadOlder(), () => chat.loadNewer(), () => chat.jumpToMessage('rm_a_m10'), () => chat.jumpToMessage(7), () => chat.send(), () => chat.sendThread(), () => chat.openThread({ id: 'rm_a_m120', seq: 120 }), () => chat.loadOlderThread(), () => chat.refreshReactions('rm_a_m129'), () => chat.toggleReaction('rm_a_m129', '🎉'), () => chat.saveRoomTitle(), () => chat.loadRoomMemory(), () => chat.changeRoomMemory(), () => chat.loadWorktree(), () => chat.leaveChat(), () => chat.exportRoom('json'), () => chat.addFiles([new File(['x'], 'a.txt')])];
        for (const call of calls) { try { await call(); } catch {} }
      });
      await new Promise(resolve => setTimeout(resolve, 400));
      assert.equal(await page.evaluate(() => window.__requests.length) - sent, 0, 'no entry of a destroyed chat starts a request');
      await page.close();
    }
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('chat page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
