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
      if (q.get('after')) list = list.filter(message => message.seq > Number(q.get('after'))).slice(0, limit);
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
    if (method === 'GET' && parts[1] === 'messages' && parts[3] === 'reactions') return ok(window.__data.reactions[parts[2]] || []);
    if (method === 'GET' && parts[1] === 'message-numbers') {
      for (const [id, room] of Object.entries(window.__data.rooms)) { const found = room.messages.find(message => message.no === Number(parts[2])); if (found) return ok({ id: found.id, room_id: id, no: found.no }); }
      return { status: 404, body: { message: 'not found' } };
    }
    if (method === 'GET' && parts[1] === 'rooms' && ['read-receipts', 'agents', 'mention-users'].includes(parts[3])) return ok([]);
    if (method === 'GET' && parts[1] === 'rooms' && parts[3] === 'members') return ok({ humans: [], agents: [], singletons: [] });
    return { status: 404, body: { message: `unexpected ${method} ${path}` } };
  };
  window.__respond = respond;
  window.fetch = (input, options = {}) => new Promise((resolve, reject) => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false, aborted: false };
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
  const mount = async ({ rooms = { rm_a: 130, rm_b: 5 }, setup = '' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width: 1280, height: 900 });
    await page.evaluateOnNewDocument(() => { try { localStorage.clear(); } catch {} });
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

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('chat page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
