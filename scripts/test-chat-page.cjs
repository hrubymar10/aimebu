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
const ROWS = fs.existsSync(path.join(root, 'js', 'pages', 'chat', 'rows.js'));
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
      let list = room.messages.filter(message => thread ? message.thread_root === thread : true);
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
      const message = { id: `${parts[2]}_m${seq}`, seq, no: seq, room_id: parts[2], author_kind: 'user', author_id: 'usr_me', author_name: 'me', body: body.body, created_at: new Date().toISOString(), reply_to: body.reply_to || '', thread_root: '' };
      // as the server derives it: a reply belongs to its parent's conversation (the parent's root, or the parent itself)
      const parent = body.reply_to && room.messages.find(item => item.id === body.reply_to); if (parent) message.thread_root = parent.thread_root || parent.id;
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
  // `openPage` is the size of the page a room opens on: the tests below were written for 100 messages, and one block checks the default.
  const mount = async ({ rooms = { rm_a: 130, rm_b: 5 }, setup = '', plain = false, openPage = 100 } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => { errors.push(error.message); if (process.env.DEBUG_ERRORS) console.error('PAGEERROR', error.message, error.stack?.split('\n').slice(0, 4).join(' | ')); });
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) { errors.push(message.text()); if (process.env.DEBUG_ERRORS) console.error('CONSOLE', message.text().slice(0, 400)); } });
    await page.setViewport({ width: 1280, height: 900 });
    await page.evaluateOnNewDocument((plain, openPage) => { try { localStorage.clear(); if (plain) localStorage.setItem('aimebu2.composer.formatting', 'off'); } catch {} if (openPage) window.__openPageSize = openPage; }, plain, openPage);
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
  // Hovers an element once it is really there: in the document, with a box, scrolled into view. The row a test hovers is drawn after a request the test answered, and on a loaded
  // machine the hover came first ("Node is either not clickable or not an Element"). A row that is drawn again between finding it and moving to it (the node is gone) is looked for again.
  // Nothing is waited for by time: the test's own timeout is the only bound.
  const hover = async (page, selector) => {
    for (;;) {
      await page.waitForFunction(selector => {
        const node = document.querySelector(selector);
        if (!node) return false;
        node.scrollIntoView({ block: 'center' });
        const box = node.getBoundingClientRect();
        return box.width > 0 && box.height > 0;
      }, {}, selector);
      try { await page.hover(selector); return; } catch (error) {
        if (!/not clickable|not an Element|detached|No node|Node is/i.test(String(error && error.message))) throw error;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      }
    }
  };
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
  const ids = page => page.$$eval('#messages > article.chat-msg[data-message-id]', nodes => nodes.map(node => node.dataset.messageId));
  const openRoom = (page, id, extra = {}) => page.evaluate((room, id) => { window.__opening = window.__chat.open(room); return null; }, { id, project_id: 'prj_a', title: id, kind: 'task', lifecycle: 'open', member_count: 2, ...extra }, id);
  const opened = page => page.evaluate(() => window.__opening);

  // Opening a room draws its newest page (100 messages) oldest first, scrolled to the bottom, marks it read once, asks for what came after it and
  // knows there is more behind it.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const state = await page.evaluate(() => { const list = document.querySelector('#messages'); return { first: list.firstElementChild.dataset.messageId, last: list.lastElementChild.dataset.messageId, rows: list.querySelectorAll(':scope > article.chat-msg').length, atBottom: list.scrollHeight - list.scrollTop - list.clientHeight < 2, hasOlder: list.dataset.hasOlder, empty: list.dataset.empty, title: document.querySelector('#room-title').textContent }; });
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
    await hover(page, '[data-message-id=rm_a_m130] .chat-msg-bubble'); // the row's toolbar is made when it is reached
    await page.evaluate(() => { document.querySelector('#composer textarea[name=body]').value = 'half a thought'; document.querySelector('[data-message-id=rm_a_m130] .message-action')?.focus(); window.__focus = document.activeElement; });
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live one', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => { const list = document.querySelector('#messages'); return [list.lastElementChild.dataset.messageId, list.scrollHeight - list.scrollTop - list.clientHeight < 2, document.activeElement === window.__focus, document.querySelector('#composer textarea[name=body]').value]; }), ['rm_a_m131', true, true, 'half a thought']);
    assert.deepEqual(await calls(page), [['latest', 'rm_a'], ['latest', 'rm_a']], 'the new page is marked read');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = 600; window.__top = list.scrollTop; window.__anchor = document.querySelector('[data-message-id=rm_a_m100]').getBoundingClientRect().top; });
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

  // Sign-in reads the newest page of the room it expects to open beside its other first reads (prefetchLatest); opening that room takes the page
  // instead of asking again, once; another room, or a later open, asks for its own.
  {
    const page = await mount({ openPage: 0 });
    const pageOf = id => `rooms/${id}/messages\\?limit=25`;
    await page.evaluate(() => window.__chat.prefetchLatest('rm_a'));
    assert.equal(await count(page, 'GET', pageOf('rm_a')), 1, 'the page is read at once');
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await count(page, 'GET', pageOf('rm_a')), 1, 'opening that room used the page it had read');
    assert.deepEqual(await ids(page).then(list => [list.length, list[0], list.at(-1)]), [25, 'rm_a_m106', 'rm_a_m130'], 'and drew it');
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await count(page, 'GET', pageOf('rm_a')), 2, 'the page is taken once: the next open asks again');
    await page.evaluate(() => window.__chat.prefetchLatest('rm_a'));
    await openRoom(page, 'rm_b'); await opened(page); await settle(page);
    assert.equal(await count(page, 'GET', pageOf('rm_b')), 1, 'another room asks for its own page');
    assert.deepEqual(await ids(page).then(list => [list.length, list[0]]), [5, 'rm_b_m1'], 'and shows it, not the one that was read for the other room');
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await count(page, 'GET', pageOf('rm_a')), 4, 'a page read for a room that was not opened is not kept for later');
    await page.evaluate(() => { window.__chat.prefetchLatest('rm_gone'); });
    await page.evaluate(() => { window.__chat.prefetchLatest(''); });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await count(page, 'GET', pageOf('rm_a')), 5, 'a prefetch that was replaced (or empty) leaves nothing behind');
    await page.close();
  }

  // A reconnect keeps the reader's place (syncInPlace): scrolled up with the older pages loaded and a thread open, what happened meanwhile (a new
  // message, a new thread reply, a changed reaction) comes in without the room being opened again, the list moving or the thread closing.
  {
    const page = await mount({ openPage: 100 });
    await page.evaluate(() => {
      const list = window.__data.rooms.rm_a.messages;
      for (const [i, id] of ['rm_a_t1', 'rm_a_t2'].entries()) list.push({ id, seq: 131 + i, no: 131 + i, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: `reply ${i + 1}`, created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: 'rm_a_m120' });
    });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = 0; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_a_m1'); await settle(page);
    await page.evaluate(() => { window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120')); }); await page.evaluate(() => window.__open); await settle(page);
    const place = () => page.evaluate(() => { const row = document.querySelector('[data-message-id=rm_a_m50]'); return { top: Math.round(row.getBoundingClientRect().top), scrollTop: Math.round(document.querySelector('#messages').scrollTop), thread: window.__chat.threadRoot?.id || null, threadShown: !document.querySelector('#thread-panel').hidden, rows: document.querySelectorAll('#messages > article.chat-msg').length }; });
    const before = await place();
    assert.deepEqual([before.thread, before.threadShown, before.rows], ['rm_a_m120', true, 132], 'scrolled up with every older page loaded and a thread open');
    await page.evaluate(() => {
      const list = window.__data.rooms.rm_a.messages;
      list.push({ id: 'rm_a_m134', seq: 133, no: 133, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'while the socket was down', created_at: '2026-01-10T12:05:00Z', reply_to: '', thread_root: '' });
      list.push({ id: 'rm_a_t3', seq: 134, no: 134, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'a reply while away', created_at: '2026-01-10T12:06:00Z', reply_to: '', thread_root: 'rm_a_m120' });
      list.find(message => message.id === 'rm_a_m60').reactions = [{ emoji: '🎉', member_kind: 'user', member_id: 'usr_ann', member_name: 'ann' }];
    });
    const opens = await count(page, 'GET', 'rooms/rm_a/messages\\?limit=');
    await page.evaluate(() => { window.__sync = window.__chat.syncInPlace(); }); await page.evaluate(() => window.__sync); await settle(page);
    const after = await place();
    assert.deepEqual([after.top, after.scrollTop, after.thread, after.threadShown], [before.top, before.scrollTop, 'rm_a_m120', true], 'the reader stays where they were, with the thread open');
    assert.equal(after.rows, 134, 'the messages that arrived meanwhile, replies included, are on the list');
    assert.equal(await page.evaluate(() => window.__chat.messages.has('rm_a_t3')), true, 'and the reply to the open thread is known');
    assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('[data-message-id=rm_a_m60] .reaction-pill')].map(node => [node.textContent, node.title, node.getAttribute('aria-pressed'), node.dataset.emoji])), [['🎉 1', 'ann', 'false', '🎉']], 'a reaction that changed meanwhile is shown');
    assert.equal(await count(page, 'GET', 'rooms/rm_a/messages\\?limit='), opens, 'the room was not opened again');
    // nothing changed: the same call changes nothing
    await page.evaluate(() => { window.__sync = window.__chat.syncInPlace(); }); await page.evaluate(() => window.__sync); await settle(page);
    assert.deepEqual(await place(), after, 'a catch-up with nothing new leaves the view alone');
    await page.close();
  }

  // Older messages: reaching the top asks for the page before the oldest row (50 at a time), puts it above without moving what the reader looks at, and
  // knows when there is no more.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { window.__anchor = document.querySelector('[data-message-id=rm_a_m35]'); window.__list = document.querySelector('#messages'); window.__list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); window.__list.scrollTop = 0; window.__before = window.__anchor.getBoundingClientRect().top; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_a_m1');
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [window.__data.rooms.rm_a.messages.length, document.querySelectorAll('#messages > article.chat-msg').length, document.querySelector('#messages').dataset.hasOlder, Math.abs(window.__anchor.getBoundingClientRect().top - window.__before) < 3]), [130, 130, 'false', true], 'all 130 rows, nothing more behind');
    assert.equal(await count(page, 'GET', 'messages\\?before=31&limit=50'), 1);
    await page.close();
  }

  // A room opens on its newest 25 messages (fewer rows to build, style and lay out before the first paint), and the older ones follow as the list is
  // scrolled to the top, 50 at a time.
  {
    const page = await mount({ openPage: 0 });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const state = await page.evaluate(() => { const list = document.querySelector('#messages'); return { first: list.firstElementChild.dataset.messageId, last: list.lastElementChild.dataset.messageId, rows: list.querySelectorAll(':scope > article.chat-msg').length, atBottom: list.scrollHeight - list.scrollTop - list.clientHeight < 3, hasOlder: list.dataset.hasOlder }; });
    assert.deepEqual(state, { first: 'rm_a_m106', last: 'rm_a_m130', rows: 25, atBottom: true, hasOlder: 'true' });
    assert.equal(await count(page, 'GET', 'messages\\?limit=25'), 1);
    await page.evaluate(() => { window.__anchor = document.querySelector('[data-message-id=rm_a_m110]'); window.__list = document.querySelector('#messages'); window.__list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); window.__list.scrollTop = 0; window.__before = window.__anchor.getBoundingClientRect().top; });
    await page.waitForFunction(() => document.querySelector('#messages > article:first-child')?.dataset.messageId === 'rm_a_m56');
    await settle(page);
    assert.equal(await count(page, 'GET', 'messages\\?before=106&limit=50'), 1);
    assert.equal(await page.evaluate(() => Math.abs(window.__anchor.getBoundingClientRect().top - window.__before) < 3), true, 'what the reader looks at stayed put');
    // Each scroll to the top asks for the page before the oldest row: 106 -> 56 -> 6, and the last one is short, so nothing is behind it.
    for (const first of ['rm_a_m6', 'rm_a_m1']) {
      await page.evaluate(() => { window.__list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); window.__list.scrollTop = 0; });
      await page.waitForFunction(first => document.querySelector('#messages > article:first-child')?.dataset.messageId === first, {}, first);
      await settle(page);
    }
    assert.equal(await count(page, 'GET', 'messages\\?before=56&limit=50'), 1); assert.equal(await count(page, 'GET', 'messages\\?before=6&limit=50'), 1);
    assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#messages > article.chat-msg').length, document.querySelector('#messages').dataset.hasOlder]), [130, 'false'], 'all 130 rows, nothing more behind');
    await page.close();
  }

  // A message outside the first page (a link to it, a search hit, an inbox item) is still reachable: the page around it is read, shown as a history
  // window, and Back to latest returns to the newest page.
  {
    const page = await mount({ openPage: 0 });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await page.evaluate(() => [document.querySelectorAll('#messages > article.chat-msg').length, !!document.querySelector('[data-message-id=rm_a_m10]')]).then(JSON.stringify), JSON.stringify([25, false]), 'the first page is the newest 25 and does not hold message 10');
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_m10'));
    await page.waitForFunction(() => !!document.querySelector('[data-message-id=rm_a_m10]'), { timeout: 5000 });
    await settle(page);
    assert.equal(await count(page, 'GET', 'messages\\?around='), 1, 'the page around it was read');
    assert.deepEqual(await page.evaluate(() => [window.__chat.contextMode, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden]), [true, false], 'a history window, with Back to latest offered');
    await page.evaluate(() => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').click());
    await page.waitForFunction(() => !!document.querySelector('[data-message-id=rm_a_m130]') && !document.querySelector('[data-message-id=rm_a_m10]'), { timeout: 5000 });
    await page.close();
  }

  // An older page still reading when the room changes or the chat is reset is dropped, and the next room can load its own older pages.
  for (const how of ['open', 'reset']) {
    const page = await mount({ rooms: { rm_a: 130, rm_b: 130 } });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hold(page, 'GET', 'rooms/rm_a/messages\\?before=');
    await page.evaluate(() => { { const reader = document.querySelector('#messages'); reader.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); reader.scrollTop = 0; } });
    const held = await waitRequest(page, 'GET', 'rooms/rm_a/messages\\?before=');
    if (how === 'open') { await openRoom(page, 'rm_b'); await opened(page); await settle(page); } else { await page.evaluate(() => window.__chat.reset()); await openRoom(page, 'rm_b'); await opened(page); await settle(page); }
    await serve(page, held); await settle(page);
    assert.deepEqual(await ids(page).then(list => [list.length, list[0]]), [100, 'rm_b_m31'], `${how}: the old room's older page is not drawn`);
    await page.evaluate(() => { { const reader = document.querySelector('#messages'); reader.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); reader.scrollTop = 0; } });
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
    assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#messages > article.chat-msg').length, document.querySelector('#messages > article.chat-msg').dataset.messageId, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, window.__chat.contextMode, document.querySelector('#messages').dataset.hasOlder, document.querySelector('[data-message-id=rm_a_m10] .chat-msg-bubble').classList.contains('message-flash') || document.querySelector('[data-message-id=rm_a_m10]').classList.contains('message-flash')]), [100, 'rm_a_m1', false, true, 'true', true]);
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
    assert.deepEqual(await page.evaluate(() => [window.__chat.contextMode, document.querySelectorAll('#messages > article.chat-msg').length, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, document.querySelector('#chat-error').textContent]), [false, 5, true, ''], `${how}: a late jump leaves the new room alone`);
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
  const pending = page => page.evaluate(() => [...document.querySelectorAll('#pending-attachments .attachment-pending-item')].map(node => node.querySelector('.attachment-pending-name, .attachment-pending-status').textContent.trim()));

  // The thread beside the room (UX review F1-F4): the composer toolbar stays one row in the narrowed column, the thread's composer offers the same attach and
  // emoji controls (with files sent along with the reply), a reply looks like a message in the room (the bubble, bold not link blue), and a system line keeps its
  // time on its own line.
  {
    const page = await mount({ openPage: 100 });
    await page.evaluate(() => {
      const list = window.__data.rooms.rm_a.messages;
      list.find(message => message.id === 'rm_a_m120').body = 'room **bold** text';
      list.push({ id: 'rm_a_t1', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_me', author_name: 'me', body: 'reply **bold** text', created_at: '2026-01-10T12:00:00Z', reply_to: 'rm_a_m120', thread_root: 'rm_a_m120' });
      list.push({ id: 'rm_a_sys', seq: 132, no: 132, room_id: 'rm_a', author_kind: 'system', author_id: '', author_name: 'system', body: 'hana joined as worker; spawned per role default: claude/docker/claude-sonnet-5-5 and a good deal more words to make it wrap', created_at: '2026-01-10T12:01:00Z', reply_to: '', thread_root: '' });
    });
    await page.setViewport({ width: 1100, height: 900 });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120')); }); await page.evaluate(() => window.__open); await settle(page);
    // F1: one row of tools, however narrow the column.
    assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.composer-toolbar')].filter(bar => bar.offsetParent).map(bar => { const tops = new Set([...bar.querySelectorAll('.composer-format')].map(button => Math.round(button.getBoundingClientRect().top))); return [bar.closest('form').id, tops.size]; })),
      [['composer', 1], ['thread-composer', 1]], 'the toolbar is one row in both composers with the thread open');
    await page.setViewport({ width: 800, height: 900 }); await settle(page);
    assert.equal(await page.evaluate(() => { const tops = new Set([...document.querySelectorAll('#composer .composer-format')].map(button => Math.round(button.getBoundingClientRect().top))); return tops.size; }), 1, 'and still one row in a narrower column (it scrolls)');
    await page.setViewport({ width: 1100, height: 900 }); await settle(page);
    // F2: the thread's toolbar carries attach and emoji, as the room's does.
    assert.deepEqual(await page.evaluate(() => ['#composer', '#thread-composer'].map(form => [...document.querySelectorAll(`${form} .composer-toolbar > .attachment-picker-btn`)].map(node => node.getAttribute('aria-label')))), [['Attach files', 'Insert emoji'], ['Attach files', 'Insert emoji']], 'attach and emoji in both toolbars');
    await page.evaluate(() => { document.querySelector('#thread-composer textarea').focus(); });
    await page.click('#thread-emoji-button'); await page.waitForSelector('.emoji-picker');
    assert.equal(await page.evaluate(() => document.querySelector('#thread-emoji-button').getAttribute('aria-expanded')), 'true', 'the emoji button of the thread opens the picker');
    await page.keyboard.press('Escape');
    await page.evaluate(() => window.__chat.insertEmoji('🎉', document.querySelector('#thread-composer textarea'))); await settle(page);
    assert.equal(await page.evaluate(() => /\p{Extended_Pictographic}/u.test(document.querySelector('#thread-composer textarea').value)), true, 'an emoji picked from the thread goes into the reply');
    await page.evaluate(() => { const draft = document.querySelector('#thread-composer textarea'); draft.value = ''; draft.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.evaluate(() => { window.__adding = window.__chat.store.addThreadFiles([new File(['x'.repeat(5)], 'note.txt', { type: 'text/plain' })]); }); await page.evaluate(() => window.__adding); await settle(page);
    assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#thread-pending-attachments .attachment-pending-item')].map(node => node.textContent.trim())).then(list => list.length), 1, 'a file picked in the thread waits under it');
    assert.equal(await page.evaluate(() => document.querySelectorAll('#pending-attachments .attachment-pending-item').length), 0, 'and is not the room composer\'s');
    await page.evaluate(() => document.querySelector('#thread-composer').requestSubmit()); await settle(page);
    assert.deepEqual((await page.evaluate(() => window.__posted)).at(-1), { body: '', reply_to: 'rm_a_m120', attachments: ['att_1'] }, 'a reply of only a file is sent in the thread with it');
    assert.equal(await page.evaluate(() => document.querySelectorAll('#thread-pending-attachments .attachment-pending-item').length), 0, 'the strip empties');
    await page.evaluate(() => { window.__adding = window.__chat.store.addThreadFiles([new File(['y'], 'gone.txt', { type: 'text/plain' })]); }); await page.evaluate(() => window.__adding); await settle(page);
    await page.evaluate(() => window.__chat.closeThread()); await settle(page);
    assert.equal(await count(page, 'DELETE', '/attachments/att_2$'), 1, 'closing the thread drops its waiting file');
    // F3: a reply is drawn like a room message: the same bubble (own messages green), bold in the text colour.
    await page.evaluate(() => { window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120')); }); await page.evaluate(() => window.__open); await settle(page);
    assert.deepEqual(await page.evaluate(() => {
      const style = (node, props) => props.map(prop => getComputedStyle(node)[prop]);
      const reply = document.querySelector('#thread-messages .thread-entry[data-message-id=rm_a_t1]'), room = document.querySelector('#messages [data-message-id=rm_a_t1]'); // the same message, as a row of the room
      return { sameBubble: JSON.stringify(style(reply.querySelector('.chat-msg-bubble'), ['borderTopWidth', 'borderTopStyle', 'borderRadius', 'backgroundColor'])) === JSON.stringify(style(room.querySelector('.chat-msg-bubble'), ['borderTopWidth', 'borderTopStyle', 'borderRadius', 'backgroundColor'])),
        bold: getComputedStyle(reply.querySelector('.chat-msg-body strong')).color === getComputedStyle(room.querySelector('.chat-msg-body strong')).color, name: getComputedStyle(reply.querySelector('.chat-msg-from-name')).color === getComputedStyle(room.querySelector('.chat-msg-from-name')).color };
    }), { sameBubble: true, bold: true, name: true }, 'a reply in the panel has the bubble, the bold and the name colour of the same message in the room');
    // The thread's title is in the room header's row (the icons end before the thread, the close button is at the panel's right edge), and a reply has the same toolbar as a room message.
    assert.deepEqual(await page.evaluate(() => { const head = document.querySelector('.chat > header').getBoundingClientRect(), title = document.querySelector('#thread-panel > header h2').getBoundingClientRect(), close = document.querySelector('#close-thread').getBoundingClientRect(), thread = document.querySelector('#thread-panel').getBoundingClientRect(), last = [...document.querySelectorAll('.chat > header button')].filter(node => node.offsetParent).map(node => node.getBoundingClientRect().right).reduce((a, b) => Math.max(a, b), 0);
      return { sameRow: Math.abs(title.top + title.height / 2 - (head.top + head.height / 2)) <= 2, closeAtRightEdge: close.left >= title.right && thread.right - close.right >= 0 && thread.right - close.right <= 16, iconsBeforeThread: last <= thread.left + 1 }; }),
      { sameRow: true, closeAtRightEdge: true, iconsBeforeThread: true }, 'the thread header is in the room header row, with the close button after the title');
    await hover(page, '#thread-messages .thread-entry[data-message-id=rm_a_t1] .chat-msg-bubble'); await settle(page); await new Promise(resolve => setTimeout(resolve, 500));
    assert.deepEqual(await page.evaluate(() => { const bar = document.querySelector('#thread-messages [data-message-id=rm_a_t1] .message-actions'); return { shown: getComputedStyle(bar).opacity, labels: [...bar.querySelectorAll('button')].filter(b => !b.closest('[hidden]') && !b.classList.contains('message-actions-wake')).map(b => b.getAttribute('aria-label') || b.title) }; }).then(x => ({ shown: x.shown, hasReact: x.labels.some(l => /react/i.test(l)), hasCopy: x.labels.some(l => /copy/i.test(l)), hasReply: x.labels.some(l => /^reply to/i.test(l)), noThread: !x.labels.some(l => /thread/i.test(l)) })),
      { shown: '1', hasReact: true, hasCopy: true, hasReply: false, noThread: true }, 'a reply in the thread has the message toolbar (react, copy; no reply or thread button, a send answers the last message)');
    // A plain send from the panel answers the conversation's last message (no chip, no thread_root sent).
    // What a send answers: the conversation's last message that is not the viewer's own.
    const lastEntry = await page.evaluate(async () => { const { currentUser } = await import('/js/api.js'); return window.__chat.store.threadEntries.findLast(message => !(message.author_kind === 'user' && message.author_id === currentUser)).id; });
    await page.evaluate(() => { window.__chat.threadDraft.value = 'to the last one'; return window.__chat.sendThread(); }); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__posted.at(-1)), await page.evaluate(() => !document.querySelector('#thread-reply-chip'))], [{ body: 'to the last one', reply_to: lastEntry }, true], 'a send from the panel answers the last message of the conversation');
    await page.evaluate(() => { window.__chat.threadDraft.value = 'and again'; return window.__chat.sendThread(); }); await settle(page);
    assert.equal((await page.evaluate(() => window.__posted.at(-1))).reply_to, lastEntry, 'a second send in a row answers the same message: the last one is now its own');
    // The panel is the room's rows and composer in another container: the same row gap, header and name size, composer inset, and a clear button centred
    // in its box, in a plain theme and a high-contrast one, at a desktop and a phone width.
    for (const [theme, width] of [['', 1280], ['high-contrast-dark', 1280], ['', 390], ['high-contrast-dark', 390]]) {
      await page.setViewport({ width, height: 900 }); await settle(page);
      const shared = await page.evaluate(theme => {
        if (theme) document.documentElement.dataset.theme = theme; else delete document.documentElement.dataset.theme;
        const cs = (node, prop) => getComputedStyle(node)[prop];
        // Row styles are compared as computed CSS (the container's gap and padding, the row's padding, header and name size): this page's list has no height to lay rows out in.
        const rows = (selector, container) => { const row = [...document.querySelectorAll(selector)].find(node => node.querySelector('.chat-msg-header')); if (!row) throw new Error(`${selector}: no row with a header`); const box = document.querySelector(container); return { gap: cs(box, 'rowGap'), box: cs(box, 'padding'), top: cs(row, 'paddingTop'), border: cs(row, 'borderBottomWidth'), header: cs(row.querySelector('.chat-msg-header'), 'fontSize'), name: cs(row.querySelector('.chat-msg-from-name'), 'fontSize'), margin: cs(row.querySelector('.chat-msg-header'), 'marginBottom') }; };
        const bar = selector => { const node = document.querySelector(selector); return [cs(node, 'paddingLeft'), cs(node, 'paddingTop'), cs(node, 'borderTopWidth')]; };
        window.__chat.setReply(window.__chat.messages.get('rm_a_m120'));
        const clear = document.querySelector('#cancel-reply'), glyph = clear.querySelector('.icon-mask'), box = clear.getBoundingClientRect(), mark = glyph.getBoundingClientRect();
        const centre = [Math.abs(mark.left + mark.width / 2 - (box.left + box.width / 2)), Math.abs(mark.top + mark.height / 2 - (box.top + box.height / 2))];
        window.__chat.setReply(null);
        return { list: rows('#messages > .chat-msg', '#messages'), panel: rows('#thread-messages > .thread-entry', '#thread-messages'), bars: [bar('.chat > .send-bar'), bar('#thread-panel > .send-bar')], centre };
      }, theme);
      assert.deepEqual(shared.panel, shared.list, `panel rows have the room's gap, padding and header and name size (${theme || 'plain'}, ${width}px)`);
      assert.deepEqual(shared.bars[1], shared.bars[0], `the panel's composer bar has the room's padding and border (${theme || 'plain'}, ${width}px)`);
      assert.ok(shared.centre.every(offset => offset <= 1), `the reply chip's clear glyph is centred in its button: ${shared.centre} (${theme || 'plain'}, ${width}px)`);
    }
    await page.setViewport({ width: 1280, height: 900 }); await page.evaluate(() => { delete document.documentElement.dataset.theme; }); await settle(page);
    // F4: the time of a system line stays beside its text.
    assert.deepEqual(await page.evaluate(() => { const row = document.querySelector('[data-message-id=rm_a_sys] .chat-msg-system-row'), body = row.firstElementChild, stamp = row.querySelector('.chat-msg-time'); const b = body.getBoundingClientRect(), s = stamp.getBoundingClientRect(); return { sideBySide: s.left >= b.right - 1 && s.top < b.bottom, nowrap: getComputedStyle(row).flexWrap }; }), { sideBySide: true, nowrap: 'nowrap' }, 'the time of a system line is beside its text, not alone on a line below');
    // Seen-by markers sit at the column's right edge whatever they follow: one on a centred system line lines up with one on someone else's message
    // (the strips are added the way read-receipts.js adds them, as the row's last child), and the system line's own content is centred in the column.
    assert.deepEqual(await page.evaluate(() => {
      const strip = node => { const div = document.createElement('div'); div.className = 'message-receipts'; div.setAttribute('role', 'group'); const avatar = document.createElement('span'); avatar.className = 'receipt-avatar'; avatar.textContent = 'MA'; div.append(avatar); node.append(div); return avatar; };
      const system = document.querySelector('#messages [data-message-id=rm_a_sys]'), other = [...document.querySelectorAll('#messages > .chat-msg:not(.self):not(.chat-msg-system)')].at(-1);
      // A short line is the case that showed it: a long one wraps across the whole column anyway.
      const text = system.querySelector('.chat-msg-system-row').firstElementChild, words = text.textContent; text.textContent = 'ian looks offline';
      const onSystem = strip(system).getBoundingClientRect(), onOther = strip(other).getBoundingClientRect();
      const list = document.querySelector('#messages'), cs = getComputedStyle(list), box = list.getBoundingClientRect(), row = system.querySelector('.chat-msg-system-row'), rowBox = row.getBoundingClientRect();
      const parts = [...row.children].map(node => node.getBoundingClientRect()).filter(part => part.width > 0);
      const left = Math.min(...parts.map(part => part.left)), right = Math.max(...parts.map(part => part.right));
      document.querySelectorAll('#messages .message-receipts').forEach(node => node.remove()); text.textContent = words;
      return { sameRight: Math.abs(onSystem.right - onOther.right) <= 1, rowLeft: Math.abs(rowBox.left - (box.left + parseFloat(cs.paddingLeft))) <= 2, centred: Math.abs((left + right) / 2 - (rowBox.left + rowBox.right) / 2) <= 2, style: getComputedStyle(row).fontStyle };
    }), { sameRight: true, rowLeft: true, centred: true, style: 'normal' }, 'a receipt after a system line is right-aligned with the ones after other messages, and the line itself is a centred non-italic line');
    // The action toolbar sits at the top-right corner of the message for every message: on an own (right-aligned) one it covers the right end of the
    // sender line, which the owner accepts. The toolbar anchors to the bubble, so this holds in the thread panel and the room alike.
    assert.deepEqual(await page.evaluate(() => {
      const root = document.querySelector('#thread-messages [data-message-id=rm_a_t1]');
      const bubble = root.querySelector('.chat-msg-bubble').getBoundingClientRect(), bar = root.querySelector('.message-actions').getBoundingClientRect();
      return { atTopRight: Math.abs(bar.right - (bubble.right - 6)) < 2 && bar.top < bubble.top && bar.bottom > bubble.top };
    }), { atTopRight: true }, 'an own message keeps its toolbar at the top-right corner of the message too');
    // Message layout B: every message on the left, so the top right of each one is free for the toolbar. Layout A is the default again after it.
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_own', seq: 200, no: 200, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_me', author_name: 'me', body: 'mine alone', created_at: '2026-01-10T12:09:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); }); await settle(page);
    const layoutOf = () => page.evaluate(() => { const row = document.querySelector('#messages [data-message-id=rm_a_own]'); return { align: getComputedStyle(row).alignSelf, width: getComputedStyle(row).maxWidth }; });
    assert.deepEqual(await layoutOf(), { align: 'flex-end', width: '75%' }, 'layout A: an own message sits on the right at 75% of the column');
    const headerLeft = () => page.evaluate(() => { const own = document.querySelector('#messages [data-message-id=rm_a_own] .chat-msg-header'); const other = [...document.querySelectorAll('#messages > .chat-msg:not(.self):not(.chat-msg-system) .chat-msg-header')].at(-1); return own && other ? Math.round(own.getBoundingClientRect().left - other.getBoundingClientRect().left) : null; });
    await page.evaluate(() => { document.documentElement.dataset.msgLayout = 'b'; });
    assert.deepEqual(await layoutOf(), { align: 'stretch', width: 'none' }, 'layout B: an own message spans the column like everyone else\'s');
    assert.equal(await headerLeft(), 0, 'layout B: an own message\'s header starts where other people\'s do');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('#messages [data-message-id=rm_a_own] .chat-msg-header')).justifyContent), 'flex-start', 'layout B lays the own header out left, in the shared order');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('#messages [data-message-id=rm_a_own] .chat-msg-bubble')).alignSelf), 'auto', 'layout B: the own bubble is not pushed right inside the full-width row');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('#thread-messages [data-message-id=rm_a_t1] > .chat-msg-bubble')).alignSelf), 'flex-start', 'layout B centres nothing in the thread panel either: its own bubble goes left');
    // The seen-by strip keeps its bottom-right place under the viewer's own message in B, like under everyone else's: its right edge is the message row's right edge.
    assert.deepEqual(await page.evaluate(() => {
      const own = document.querySelector('#messages [data-message-id=rm_a_own]');
      const strip = document.createElement('div'); strip.className = 'message-receipts'; strip.setAttribute('role', 'group');
      const avatar = document.createElement('span'); avatar.className = 'receipt-avatar'; avatar.textContent = 'QU'; strip.append(avatar); own.append(strip);
      const justify = getComputedStyle(strip).justifyContent;
      const stripBox = strip.getBoundingClientRect(), ownBox = own.getBoundingClientRect();
      strip.remove();
      return { atRight: Math.abs(stripBox.right - ownBox.right) < 2, justify };
    }), { atRight: true, justify: 'flex-end' }, 'layout B: the receipts strip under an own message is right-aligned with its right edge');
    await page.evaluate(() => { delete document.documentElement.dataset.msgLayout; });
    assert.deepEqual(await layoutOf(), { align: 'flex-end', width: '75%' }, 'layout A is back once the choice is gone');
    await page.close();
  }

  // Long system notices show one line and open a readable, copyable Markdown dialog.
  {
    const page = await mount();
    await page.evaluate(() => window.__data.rooms.rm_a.messages.push(
      { id: 'rm_a_long_system', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: 'The team flow changed.\n\n## Instructions\n- **Review** the task with @tess. See #42.\n\n```text\nHand off active work.\n```', created_at: '2026-01-10T12:01:00Z' },
      { id: 'rm_a_recovery_handoff', seq: 132, no: 132, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: 'Recovery handed to the lead', created_at: '2026-01-10T12:02:00Z' },
      { id: 'rm_a_recovery_resolved', seq: 133, no: 133, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: 'Recovery resolved by the lead: resume', created_at: '2026-01-10T12:03:00Z' }));
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.match(await page.$eval('[data-message-id=rm_a_recovery_handoff]', row => row.textContent), /Recovery handed to the lead/);
    assert.match(await page.$eval('[data-message-id=rm_a_recovery_resolved]', row => row.textContent), /Recovery resolved by the lead: resume/);
    await page.evaluate(() => window.__chat.setAgents([{ id: 'agt_tess', slug: 'tess', name: 'tess', project_id: 'prj_a', room_id: 'rm_a', status: 'online' }])); await settle(page);
    assert.deepEqual(await page.$eval('[data-message-id=rm_a_long_system]', row => {
      const link = row.querySelector('.system-popup-link'), preview = row.querySelector('.system-notice-preview');
      return [preview.textContent, link.tagName, link.textContent, getComputedStyle(link).borderStyle, getComputedStyle(link).fontSize === getComputedStyle(preview).fontSize, !!row.querySelector('.system-popup-show'), row.querySelector('dialog').open];
    }), ['The team flow changed.', 'A', 'Show more', 'none', true, false, false], 'the trigger is an inline text link with no button frame');
    await page.$eval('[data-message-id=rm_a_long_system] .system-popup-link', node => node.click());
    assert.deepEqual(await page.$eval('[data-message-id=rm_a_long_system]', row => {
      const dialog = row.querySelector('dialog'), body = dialog.querySelector('.system-message-dialog-body');
      return [dialog.open, body.querySelector('h2')?.textContent, body.querySelector('li strong')?.textContent, body.querySelector('pre code')?.textContent, body.querySelector('.mention[role=button]')?.textContent, getComputedStyle(body).textAlign];
    }), [true, 'Instructions', 'Review', 'Hand off active work.', '@tess', 'left']);
    // The dialog is in the top layer, so it is centred in the window whatever the list around it is: the list is a size container (for the toolbar's query), and a
    // container's layout containment must not become the dialog's containing block, with the list scrolled to its end or not.
    for (const atEnd of [false, true]) {
      await page.evaluate(end => { const list = document.querySelector('#messages'); list.scrollTop = end ? list.scrollHeight : 0; }, atEnd);
      assert.deepEqual(await page.evaluate(() => { const box = document.querySelector('[data-message-id=rm_a_long_system] dialog').getBoundingClientRect(); return [Math.abs(box.left + box.width / 2 - innerWidth / 2) < 2, box.top >= 0, box.bottom <= innerHeight, box.width > 200]; }), [true, true, true, true], `the open notice dialog is centred in the window (list scrolled ${atEnd ? 'to its end' : 'to its start'})`);
    }
    await page.evaluate(() => window.__chat.setRaw(true)); await settle(page);
    assert.deepEqual(await page.$eval('[data-message-id=rm_a_long_system] dialog .system-message-dialog-body', body => [body.classList.contains('md-rendered'), body.textContent.includes('## Instructions'), !!body.querySelector('h2'), !!body.querySelector('.mention'), getComputedStyle(body).whiteSpace]),
      [false, true, false, false, 'pre-wrap'], 'raw mode preserves source line breaks in the open dialog');
    await page.evaluate(() => window.__chat.setRaw(false)); await settle(page);
    assert.equal(await page.$eval('[data-message-id=rm_a_long_system] dialog .system-message-dialog-body', body => body.querySelector('.mention[role=button]')?.textContent), '@tess', 'rendered mode restores mentions');
    await page.evaluate(() => { window.__chat.openMention = text => { window.__mentionFromDialog = [text, document.querySelector('[data-message-id=rm_a_long_system] dialog').open]; }; });
    await page.$eval('[data-message-id=rm_a_long_system] dialog .mention[role=button]', node => node.click());
    assert.deepEqual(await page.evaluate(() => window.__mentionFromDialog), ['@tess', false], 'opening a mentioned profile closes the dialog first');
    await page.$eval('[data-message-id=rm_a_long_system] .system-popup-link', node => node.click());
    await page.evaluate(() => { window.__mentionFromDialog = null; });
    await page.focus('[data-message-id=rm_a_long_system] dialog .mention[role=button]');
    await page.keyboard.press('Enter');
    assert.deepEqual(await page.evaluate(() => window.__mentionFromDialog), ['@tess', false], 'keyboard activation closes the dialog before opening a mentioned profile');
    await page.$eval('[data-message-id=rm_a_long_system] .system-popup-link', node => node.click());
    await page.evaluate(() => { window.__copiedSystemMessage = ''; navigator.clipboard.writeText = async value => { window.__copiedSystemMessage = value; }; });
    await page.$eval('[data-message-id=rm_a_long_system] dialog header button:first-of-type', node => node.click());
    assert.match(await page.evaluate(() => window.__copiedSystemMessage), /## Instructions/, 'Copy keeps the message source');
    await page.evaluate(() => { window.__chat.jumpToMessage = no => { window.__jumpFromDialog = [no, document.querySelector('[data-message-id=rm_a_long_system] dialog').open]; }; });
    await page.$eval('[data-message-id=rm_a_long_system] dialog a.msg-ref', node => node.click());
    assert.deepEqual(await page.evaluate(() => window.__jumpFromDialog), [42, false], 'message references close the dialog before navigating');
    await page.close();
  }

  // A model switch handoff opens the same formatted dialog.
  {
    const page = await mount();
    await page.evaluate(() => window.__data.rooms.rm_a.messages.push(
      { id: 'rm_a_switch', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: '@agt_01RAW Switch handoff (written).\nCurrent task: **review** the patch.', created_at: '2026-01-10T12:01:00Z', switch_handoff: { agent_slug: 'gus', old_tool: 'claude', old_model: 'claude-opus-5-5', new_tool: 'claude', new_model: 'claude-sonnet-5-5', source: 'written' } },
      { id: 'rm_a_switch_old', seq: 132, no: 132, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: '@agt_01OLD Switch handoff (written).\nCurrent task: review.', created_at: '2026-01-10T12:02:00Z' },
      { id: 'rm_a_other', seq: 133, no: 133, room_id: 'rm_a', author_kind: 'system', author_id: 'system', body: '@agt_01OLD please read the notice.', created_at: '2026-01-10T12:03:00Z' }));
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.deepEqual(await page.$eval('[data-message-id=rm_a_switch]', row => [row.querySelector('dialog').open, row.querySelector('.system-notice-preview').textContent, row.querySelector('.system-popup-show').textContent]),
      [false, 'Switch handoff: gus · claude-opus-5-5 → claude-sonnet-5-5 (written by the agent)', 'Show']);
    await page.$eval('[data-message-id=rm_a_switch] .system-popup-show', node => node.click());
    assert.deepEqual(await page.$eval('[data-message-id=rm_a_switch]', row => [row.querySelector('dialog').open, row.querySelector('.system-message-dialog-body strong')?.textContent, getComputedStyle(row.querySelector('.system-message-dialog-body')).textAlign]),
      [true, 'review', 'left'], 'the handoff dialog uses left-aligned Markdown');
    assert.deepEqual(await page.evaluate(() => ['rm_a_switch_old', 'rm_a_other'].map(id => document.querySelector(`[data-message-id=${id}] .chat-msg-system-row`).textContent.includes('@agt_01OLD'))),
      [false, true], 'legacy handoffs lose the routing prefix while ordinary system messages keep their body');
    await page.close();
  }

  // Sending: the text goes to the open room and the draft empties, the reply chip goes, the message is drawn at the bottom; a failure keeps the draft and
  // says why in the composer.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await typeDraft(page, 'hello there'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await posts(page), await draftText(page), await ids(page).then(list => list.at(-1)), await page.evaluate(() => { const list = document.querySelector('#messages'); return list.scrollHeight - list.scrollTop - list.clientHeight < 2; })], [[{ body: 'hello there', attachments: [] }], '', 'rm_a_m131', true]);
    // Your own message carries your name like anyone's, not "You" (the bubble is still the own-message one): in the list, in the quote of a reply to it, and in the chip.
    assert.deepEqual(await page.evaluate(() => { const own = document.querySelector('#messages > article.chat-msg:last-of-type'); return [own.classList.contains('self'), own.querySelector('.chat-msg-from-name').textContent.trim()]; }), [true, 'me'], 'an own message is named by its author, not "You"');
    await page.evaluate(() => { const own = window.__chat.messages.get('rm_a_m131'); window.__chat.setReply(own); });
    assert.equal(await page.evaluate(() => document.querySelector('#reply-chip-jump').textContent.startsWith('Replying to me: ')), true, 'the chip names you by your name');
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

  // A superseded ask is out of the Inbox but not over: its chat controls stay active and answer it through the Inbox like an open ask.
  {
    const page = await mount();
    await page.evaluate(() => {
      const messages = window.__data.rooms.rm_a.messages;
      messages[118] = { ...messages[118], author_kind: 'agent', author_id: 'agt_zed', body: 'Which one?', ask_json: '{"kind":"question","text":"Which?"}', answer_state: 'superseded' };
      messages[119] = { ...messages[119], author_kind: 'agent', author_id: 'agt_zed', body: 'Pick one', proposed_answers: ['alpha', 'beta'], answer_state: 'superseded' };
      window.__answered = [];
    });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => {
      window.__chat.agents = new Map([['agt_zed', { slug: 'zed' }]]);
      window.__chat.setInbox({ answer: async (item, text) => { window.__answered.push([item.id, text]); return true; } });
      window.__chat.updateAnswerControls([
        { id: 'inb_sup_options', rev: 1, kind: 'question', message_id: 'rm_a_m119', state: 'superseded', options: ['A', 'B'], free_text: true, can_decide: true },
        { id: 'inb_sup_proposed', rev: 1, kind: 'question', message_id: 'rm_a_m120', state: 'superseded', proposed_answers: ['alpha', 'beta'], options: [], free_text: false, can_decide: true },
      ]);
    });
    await page.waitForSelector('#messages [data-message-id=rm_a_m120] .answer-controls .proposed-answer-btn');
    assert.deepEqual(await page.evaluate(() => ['rm_a_m119', 'rm_a_m120'].map(id => {
      const row = document.querySelector(`#messages [data-message-id=${id}]`);
      return [!!row.querySelector('.answer-controls'), [...row.querySelectorAll('.answer-controls button')].map(button => button.disabled), row.textContent.includes('No longer pending')];
    })), [[true, [false, false], false], [true, [false, false], false]], 'a superseded ask keeps its active answer controls and no "No longer pending"');
    await page.$eval('#messages [data-message-id=rm_a_m120] .answer-controls .proposed-answer-btn', button => button.click());
    await page.waitForFunction(() => window.__answered.length === 1);
    assert.deepEqual(await page.evaluate(() => window.__answered), [['inb_sup_proposed', '@zed alpha']], 'answering a superseded ask goes through the Inbox like an open one');
    await page.close();
  }

  // With no answer control to show, a superseded ask is drawn quiet while a closed one keeps its "No longer pending".
  {
    const page = await mount();
    await page.evaluate(() => {
      const messages = window.__data.rooms.rm_a.messages;
      messages[118] = { ...messages[118], author_kind: 'agent', author_id: 'agt_zed', body: 'Pick one', proposed_answers: ['alpha', 'beta'], answer_state: 'superseded' };
      messages[119] = { ...messages[119], author_kind: 'agent', author_id: 'agt_zed', body: 'Pick one', proposed_answers: ['alpha', 'beta'], answer_state: 'withdrawn' };
    });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.deepEqual(await page.evaluate(() => ['rm_a_m119', 'rm_a_m120'].map(id => {
      const row = document.querySelector(`#messages [data-message-id=${id}]`);
      return [!!row.querySelector('.answer-history'), row.querySelector('.answer-history-status')?.textContent || '', row.querySelectorAll('.answer-history button').length];
    })), [[false, '', 0], [true, 'No longer pending', 0]], 'a superseded ask has no history; a withdrawn one shows one status and no chips');
    await page.close();
  }

  // A reply to a question open for this person that is one of its proposed answers answers it through the Inbox instead of posting a message; a refusal
  // An agent that was removed has no record any more; the server sends the name it had, and its row and its quotes say it is removed.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page);
    const names = await page.evaluate(() => {
      window.__chat.agents = new Map();
      const removed = { author_kind: 'agent', author_id: 'agt_gone', author_name: 'opal', author_project_id: '' };
      return [window.__chat.authorName(removed), window.__chat.authorName({ author_kind: 'agent', author_id: 'agt_0123456789' }), window.__chat.authorName({ author_kind: 'agent', author_id: 'old-bot' })];
    });
    assert.deepEqual(names, ['opal (removed)', 'Agent 456789', 'old-bot'], 'a removed agent is named and marked; an agent with no name stays an id');
    await page.close();
  }
  // says so, keeps the text and posts nothing.
  {
    const page = await mount();
    await page.evaluate(() => { window.__data.rooms.rm_a.messages[119] = { ...window.__data.rooms.rm_a.messages[119], author_kind: 'agent', author_id: 'agt_zed', author_name: 'zed', body: 'Which one?', proposed_answers: ['alpha', 'beta'] }; window.__answered = []; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => {
      window.__chat.agents = new Map([['agt_zed', { slug: 'zed' }]]);
      window.__chat.setInbox({ answer: async (item, text) => { window.__answered.push([item.id, text]); return window.__answerResult ?? true; } });
      window.__chat.updateAnswerControls([{ id: 'inb_1', rev: 1, kind: 'question', message_id: 'rm_a_m120', proposed_answers: ['alpha', 'beta'], options: [], free_text: false, can_decide: true }]);
    });
    await page.waitForSelector('#messages [data-message-id=rm_a_m120] .proposed-answer-btn');
    const answerPosition = await page.$eval('#messages [data-message-id=rm_a_m120]', row => {
      const bubble = row.querySelector('.chat-msg-bubble').getBoundingClientRect();
      const answer = row.querySelector('.proposed-answer-btn').getBoundingClientRect();
      return { gap: answer.top - bubble.bottom, offset: answer.left - bubble.left };
    });
    assert.ok(answerPosition.gap >= 0 && answerPosition.gap <= 12 && Math.abs(answerPosition.offset) <= 2,
      `proposed answers sit immediately under the message: ${JSON.stringify(answerPosition)}`);
    await page.$eval('#messages [data-message-id=rm_a_m120] .proposed-answer-btn', button => button.dispatchEvent(new MouseEvent('click', { shiftKey: true, bubbles: true, cancelable: true })));
    assert.deepEqual({ draft: await draftText(page), reply: await page.evaluate(() => window.__chat.reply?.id), sent: await page.evaluate(() => window.__answered), posts: await posts(page) },
      { draft: '@zed alpha', reply: 'rm_a_m120', sent: [], posts: [] }, 'Shift-clicking a proposed answer composes it as a linked reply without sending');
    await typeDraft(page, '@zed beta'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__answered), (await posts(page)).length, await draftText(page), await page.evaluate(() => document.querySelector('#reply-chip').hidden)], [[['inb_1', '@zed beta']], 0, '', true], 'an answer goes through the inbox, not as a message');
    await page.evaluate(() => { window.__answerResult = false; });
    await page.$eval('#messages [data-message-id=rm_a_m120] .proposed-answer-btn', button => button.dispatchEvent(new MouseEvent('click', { shiftKey: true, bubbles: true, cancelable: true })));
    await typeDraft(page, '@zed alpha'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await noticeText(page), await draftText(page), (await posts(page)).length], ['Could not send the answer', '@zed alpha', 0], 'a refused answer keeps the text');
    await page.evaluate(() => { window.__answerResult = true; window.__chat.setReply(window.__chat.messages.get('rm_a_m120')); });
    await typeDraft(page, 'something else entirely'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), (await page.evaluate(() => window.__answered)).length], [{ body: 'something else entirely', attachments: [], reply_to: 'rm_a_m120' }, 2], 'text that is not an option is an ordinary reply');
    await page.evaluate(() => {
      const question = window.__data.rooms.rm_a.messages.find(message => message.id === 'rm_a_m120');
      question.answer_state = 'resolved'; question.answer_body = '@zed beta'; question.answer_author_name = 'owner'; question.answer_message_no = window.__data.rooms.rm_a.messages[120].no;
      window.__chat.updateAnswerControls([]);
    });
    await page.waitForFunction(() => document.querySelector('#messages [data-message-id=rm_a_m120] .answer-history-link')?.textContent.includes('beta'));
    const history = async (body, values, questions = []) => {
      await page.evaluate(({ body, values, questions }) => {
        const question = window.__chat.store.messages.get('rm_a_m120');
        question.answer_body = body; question.proposed_answers = values; question.open_questions = questions;
        window.__chat.updateAnswerControls([]);
      }, { body, values, questions });
      await settle(page);
      return page.$eval('#messages [data-message-id=rm_a_m120] .answer-history', box => ({
        text: box.querySelector('.answer-history-link')?.textContent.trim(), href: box.querySelector('a')?.getAttribute('href'),
        buttons: box.querySelectorAll('button').length, icon: !!box.querySelector('.icon-mask-check'),
      }));
    };
    assert.deepEqual(await history('@zed beta', ['alpha', 'beta']),
      { text: 'Answered by owner: beta', href: `#${await page.evaluate(() => window.__chat.store.messages.get('rm_a_m120').answer_message_no)}`, buttons: 0, icon: true }, 'a resolved answer is one linked line with an SVG check');
    const choices = ['B after alpha.3', 'B after alpha.3 (trial)', 'Stay on D'];
    assert.deepEqual(await history('@zed B after alpha.3 (trial) - remind me pls', choices),
      { text: 'Answered by owner: B after alpha.3 (trial)', href: `#${await page.evaluate(() => window.__chat.store.messages.get('rm_a_m120').answer_message_no)}`, buttons: 0, icon: true }, 'an edited answer uses the longest matching option without repeating the note');
    assert.deepEqual(await history('@zed B after alpha.3 (trialist)', choices),
      { text: 'Answered by owner: B after alpha.3', href: `#${await page.evaluate(() => window.__chat.store.messages.get('rm_a_m120').answer_message_no)}`, buttons: 0, icon: true }, 'a proposed answer must end at a word or punctuation boundary');
    assert.deepEqual(await history('@zed something else entirely', choices),
      { text: 'Answered by owner: something else entirely', href: `#${await page.evaluate(() => window.__chat.store.messages.get('rm_a_m120').answer_message_no)}`, buttons: 0, icon: true }, 'free text omits the leading mention');
    const longAnswer = `@zed ${'a'.repeat(65)}`;
    assert.equal((await history(longAnswer, choices)).text, `Answered by owner: ${'a'.repeat(60)}…`, 'a long free-text answer has a short preview');
    assert.equal((await history('@zed Q1) a) A\nQ2) a) Now', [], [{ question: 'Route?' }, { question: 'When?' }])).text,
      'Answered by owner (2/2)', 'Open Questions show only their count');
    await page.$eval('#messages [data-message-id=rm_a_m120] .answer-history-link', link => link.click());
    await page.waitForFunction(() => document.querySelector('#messages [data-message-id=rm_a_m121]')?.classList.contains('message-flash') || !!document.querySelector('#messages [data-message-id=rm_a_m121] .message-flash'));
    await page.close();
  }

  // Shift-clicking the review submit composes a structural reply, ready for
  // editing, and leaves the request open until that draft is sent.
  {
    const page = await mount();
    await page.evaluate(() => {
      window.__data.rooms.rm_a.messages[119] = { ...window.__data.rooms.rm_a.messages[119], author_kind: 'agent', author_id: 'agt_zed', author_name: 'zed', body: 'Choose a route', open_questions: [
        { question: 'Route?', options: ['A', 'B'] }, { question: 'When?', options: ['Now', 'Later'] },
      ] };
      window.__answered = [];
    });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => {
      window.__chat.agents = new Map([['agt_zed', { slug: 'zed' }]]);
      window.__chat.setInbox({ answer: async (item, text) => { window.__answered.push([item.id, text]); return true; } });
      window.__chat.updateAnswerControls([{ id: 'inb_questions', rev: 1, kind: 'question', message_id: 'rm_a_m120', open_questions: [
        { question: 'Route?', options: ['A', 'B'] }, { question: 'When?', options: ['Now', 'Later'] },
      ], free_text: true, can_decide: true }]);
    });
    await page.$eval('#messages [data-message-id=rm_a_m120] .open-questions-open', button => button.click());
    await page.waitForSelector('#open-questions-dialog[open]');
    await page.$eval('#open-questions-dialog [name=question-0][value="0"]', input => input.click());
    await page.$eval('#open-questions-dialog [name=question-1][value="0"]', input => input.click());
    await page.waitForFunction(() => !document.querySelector('#open-questions-dialog .open-questions-send').hidden);
    const hint = await page.$eval('#open-questions-dialog .open-questions-send', button => button.title);
    await page.$eval('#open-questions-dialog .open-questions-send', button => button.dispatchEvent(new MouseEvent('click', { shiftKey: true, bubbles: true, cancelable: true })));
    await page.waitForFunction(() => !document.querySelector('#open-questions-dialog')?.open && document.activeElement?.getAttribute('aria-label') === 'Message');
    assert.deepEqual({
      hint, draft: await draftText(page), reply: await page.evaluate(() => window.__chat.reply?.id),
      focused: await page.evaluate(() => document.activeElement?.isContentEditable && document.activeElement.getAttribute('aria-label') === 'Message'),
      sent: await page.evaluate(() => window.__answered), posts: await posts(page),
    }, { hint: 'Send answers (Shift-click to edit them in the composer first)', draft: '@zed Q1) a) A\nQ2) a) Now', reply: 'rm_a_m120', focused: true, sent: [], posts: [] }, 'Shift-click puts Open Questions answers in the composer with the reply link without sending');
    await page.evaluate(() => { window.__chat.setReply(null); window.__chat.setReply(window.__chat.messages.get('rm_a_m120')); });
    await typeDraft(page, '2) check the host architecture'); await submit(page); await page.evaluate(() => window.__sending); await settle(page);
    assert.deepEqual([await page.evaluate(() => window.__answered), (await posts(page)).at(-1), await page.$eval('#messages [data-message-id=rm_a_m120] .open-questions-open', button => button.disabled)],
      [[], { body: '2) check the host architecture', attachments: [], reply_to: 'rm_a_m120' }, false], 'an ordinary reply does not answer or disable Open Questions');
    await openRoom(page, 'rm_b'); await opened(page); await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await page.$eval('#messages [data-message-id=rm_a_m120] .open-questions-open', button => button.disabled), false, 'the unanswered request remains active after changing rooms');
    await page.evaluate(() => {
      const question = window.__data.rooms.rm_a.messages.find(message => message.id === 'rm_a_m120');
      question.answer_state = 'resolved'; question.answer_body = 'Q1) a) A\nQ2) a) Now'; question.answer_author_name = 'owner'; question.answer_message_no = 121;
      window.__chat.updateAnswerControls([]);
    });
    await page.waitForFunction(() => document.querySelector('#messages [data-message-id=rm_a_m120] .answer-history-link')?.textContent.includes('Answered by owner (2/2)'));
    assert.deepEqual(await page.$eval('#messages [data-message-id=rm_a_m120] .answer-history', box => [box.querySelector('.answer-history-link')?.textContent.trim(), box.querySelector('a')?.getAttribute('href'), box.querySelectorAll('button').length]),
      ['Answered by owner (2/2)', '#121', 0], 'a submitted Open Questions answer is one linked summary');
    await openRoom(page, 'rm_b'); await opened(page); await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    assert.equal(await page.$eval('#messages [data-message-id=rm_a_m120] .answer-history-link', node => node.textContent), 'Answered by owner (2/2)', 'the answer summary survives a room change from message state');
    await page.close();
  }

  // Opens a root's conversation the way its chip does; with replies as rows of the room the root may lie outside the loaded window, and then it is fetched as a jump would.
  const chip = (page, root) => page.evaluate(async root => {
    const node = document.querySelector(`[data-message-id=${root}] .thread-chip`);
    if (node) { node.click(); return; }
    const found = (await (await fetch(`/api/rooms/rm_a/messages?around=${root}&limit=1`)).json()).find(item => item.id === root);
    window.__open = window.__chat.openThread(found); await window.__open;
  }, root);
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
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    assert.deepEqual(await panel(page), { hidden: false, title: 'Replies · 3', entries: ['rm_a_m120', 'rm_a_t1', 'rm_a_t2', 'rm_a_t3'], error: '', focus: true });
    assert.equal(await count(page, 'GET', 'thread_root=rm_a_m120&limit=100'), 1);
    await page.evaluate(() => { const draft = window.__chat.threadDraft; draft.value = 'my reply'; });
    await page.evaluate(() => window.__chat.sendThread()); await settle(page);
    assert.deepEqual([(await posts(page)).at(-1), (await panel(page)).entries.at(-1), await page.evaluate(() => window.__chat.threadDraft.value)], [{ body: 'my reply', reply_to: 'rm_a_t3' }, (await panel(page)).entries.at(-1), ''], 'a reply is posted into the thread (answering its last message) and the box empties');
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
    await chip(page, 'rm_a_m120');
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
    await chip(page, 'rm_a_m120');
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
    await chip(page, 'rm_a_m120');
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
    await chip(page, 'rm_a_m120');
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
    await chip(failing, 'rm_a_m120');
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
    await chip(page, 'rm_a_m120');
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
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 101); await settle(page);
    await hold(page, 'GET', 'thread_root=rm_a_m120.*before=');
    await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
    const first = await waitRequest(page, 'GET', 'thread_root=rm_a_m120.*before=');
    await chip(page, 'rm_a_m110');
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
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(page);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_live', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live reply', created_at: '2026-01-10T12:00:00Z', reply_to: 'rm_a_m120', thread_root: 'rm_a_m120', thread_count: 3 }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual([(await panel(page)).entries.at(-1), await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m120] .thread-chip').textContent), (await panel(page)).title], ['rm_a_live', '3 replies', 'Replies · 3']);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_other', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'other reply', created_at: '2026-01-10T12:01:00Z', reply_to: 'rm_a_m110', thread_root: 'rm_a_m110', thread_count: 1 }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual([await page.evaluate(() => document.querySelector('[data-message-id=rm_a_m110] .thread-chip').textContent), (await panel(page)).entries.length], ['1 reply', 4]);
    await page.close();
  }

  // A reply is a row of the room like any message: a jump to it shows that row (flashing) and leaves the panel alone; opening its conversation from the reply opens
  // its root's panel with the reply in view.
  {
    const page = await mount();
    await addReplies(page, 130);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => window.__chat.jumpToMessage('rm_a_t5')); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#thread-panel').hidden, !!document.querySelector('#messages [data-message-id=rm_a_t5]'), document.querySelector('#messages [data-message-id=rm_a_t5] .chat-msg-bubble')?.classList.contains('message-flash')]), [true, true, true], 'a jump to a reply shows its row in the room');
    await page.evaluate(() => { window.__open = window.__chat.openConversation(window.__chat.messages.get('rm_a_t5')); }); await page.evaluate(() => window.__open); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#thread-panel').hidden, window.__chat.threadRoot?.id, !!document.querySelector('#thread-messages [data-message-id=rm_a_t5]')]), [false, 'rm_a_m120', true], 'a reply opens its root conversation with itself in it');
    await page.close();
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
    await page.$eval('[data-message-id=rm_a_m130] .chat-msg-bubble', node => node.scrollIntoView());
    await hover(page, '[data-message-id=rm_a_m130] .chat-msg-bubble'); // a row's toolbar is made when the row is reached
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
    for (const id of ['rm_a_m120', 'rm_a_m121']) {
      await hover(page, `[data-message-id=${id}] .chat-msg-bubble`);
      assert.equal(await page.$eval(`[data-message-id=${id}]`, row => {
        const bubble = row.querySelector('.chat-msg-bubble').getBoundingClientRect();
        const actions = row.querySelector('.message-actions').getBoundingClientRect();
        const parts = [...row.querySelectorAll('.chat-msg-header *')].map(node => node.getBoundingClientRect()).filter(box => box.width > 0 && box.height > 0);
        const header = { left: Math.min(...parts.map(box => box.left)), right: Math.max(...parts.map(box => box.right)), top: Math.min(...parts.map(box => box.top)), bottom: Math.max(...parts.map(box => box.bottom)) };
        const clear = actions.right <= header.left + 1 || actions.left >= header.right - 1 || actions.bottom <= header.top + 1 || actions.top >= header.bottom - 1;
        // Every toolbar is at the bubble's top-right corner; someone else's sender line is at the left, clear of it, and the viewer's own
        // right-aligned line is covered at its right end, which the owner accepts.
        return Math.abs(bubble.right - actions.right - 6) < 2 && (row.classList.contains('self') || clear) ? true : JSON.stringify({ self: row.classList.contains('self'), bubble, actions, header });
      }), true, `message actions sit at the bubble's top-right corner for ${id}`);
    }
    await hover(page, '[data-message-id=rm_a_m120] .chat-msg-bubble');
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

  // The same history in the rich composer (the editor has the focus and the caret, not the textarea) and in the thread panel's composer, over that conversation's own
  // messages. The keys are the editor's own: Up at the start of the document, Down at its end, a picker that is open keeps them, a modifier or composing is no history.
  {
    const page = await mount();
    await page.evaluate(() => { for (const [seq, body] of [[121, 'mine one'], [124, 'mine two'], [129, 'mine three']]) Object.assign(window.__data.rooms.rm_a.messages[seq - 1], { author_id: 'usr_me', author_name: 'me', body }); });
    await addReplies(page, 3);
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; for (const [id, body] of [['rm_a_t1', 'thread mine one'], ['rm_a_t3', 'thread mine two']]) Object.assign(messages.find(message => message.id === id), { author_id: 'usr_me', author_name: 'me', body }); });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.waitForSelector('#composer .ProseMirror');
    const text = (thread = false) => page.evaluate(thread => (thread ? window.__chat.threadFormatting : window.__chat.formatting).input.value, thread);
    const press = async (key, modifier) => { if (modifier) await page.keyboard.down(modifier); await page.keyboard.press(key); if (modifier) await page.keyboard.up(modifier); await settle(page); };
    await page.focus('#composer .ProseMirror');
    // (A reply is a message of the room, so the room's history has the conversation's own replies too, in the order they were sent.)
    await press('ArrowUp'); assert.equal(await text(), 'thread mine two', 'Up in an empty rich composer recalls the newest own message');
    await press('ArrowUp'); assert.equal(await text(), 'thread mine one');
    await press('ArrowUp'); assert.equal(await text(), 'mine three');
    await press('ArrowDown'); assert.equal(await text(), 'thread mine one');
    await press('Escape'); assert.equal(await text(), '', 'Escape puts the draft back');
    await page.keyboard.type('line one'); await page.keyboard.down('Shift'); await page.keyboard.press('Enter'); await page.keyboard.up('Shift'); await page.keyboard.type('line two'); await settle(page);
    await press('ArrowUp'); assert.equal(await text(), 'line one\nline two', 'Up from the second line of a draft moves the caret, it is not history');
    await page.evaluate(() => window.__chat.formatting.input.setSelectionRange(0, 0)); await press('ArrowUp'); assert.equal(await text(), 'thread mine two', 'at the very start of the document it is');
    for (let i = 0; i < 2; i++) await press('ArrowDown');
    assert.equal(await text(), 'line one\nline two', 'past the newest the draft is back');
    await press('Escape'); await page.evaluate(() => { window.__chat.formatting.input.value = ''; });
    await press('ArrowUp', 'Control'); assert.equal(await text(), '', 'a modifier makes it no history');
    // A formatted draft survives a walk: Up saves the editor's document, and Escape (or Down past the newest) puts it back as it was, with its bold and list; a recalled
    // message that has formatting loads as formatting, not as asterisks.
    await page.evaluate(() => { window.__chat.formatting.input.value = ''; });
    await page.evaluate(() => window.__chat.formatting.loadMarkdown('plain start **bold draft**\n\n- item a\n- item b'));
    const shape = () => page.evaluate(() => { const root = document.querySelector('#composer .ProseMirror'); return { strong: [...root.querySelectorAll('strong')].map(node => node.textContent), items: [...root.querySelectorAll('li')].map(node => node.textContent), markdown: window.__chat.draft.value }; });
    const drafted = await shape();
    assert.deepEqual([drafted.strong, drafted.items], [['bold draft'], ['item a', 'item b']], `the formatted draft is in the editor as formatting: ${JSON.stringify(drafted)}`);
    await page.focus('#composer .ProseMirror'); await page.evaluate(() => window.__chat.formatting.input.setSelectionRange(0, 0));
    await press('ArrowUp');
    const recalled = await shape();
    assert.ok(recalled.strong.length > 0 || recalled.items.length > 0 || recalled.markdown.length > 0, 'something was recalled');
    await press('Escape');
    const restored = await shape();
    assert.deepEqual([restored.strong, restored.items, restored.markdown], [drafted.strong, drafted.items, drafted.markdown], `Escape puts the formatted draft back as it was: ${JSON.stringify(restored)}`);
    await page.evaluate(() => window.__chat.formatting.input.setSelectionRange(0, 0)); await press('ArrowUp');
    for (let i = 0; i < 6; i++) await press('ArrowDown');
    const back = await shape();
    assert.deepEqual([back.strong, back.items, back.markdown], [drafted.strong, drafted.items, drafted.markdown], `and so does Down past the newest: ${JSON.stringify(back)}`);
    // A recalled message with formatting is loaded as formatting.
    await page.evaluate(() => { window.__chat.formatting.input.value = ''; });
    await page.evaluate(() => { const own = window.__chat.messages.get('rm_a_m129'); own.body = 'a **bold** word and a list:\n\n- one\n- two'; });
    await page.evaluate(() => window.__chat.formatting.loadMarkdown(''));
    await page.focus('#composer .ProseMirror'); await press('ArrowUp'); await press('ArrowUp'); await press('ArrowUp');
    const fromHistory = await shape();
    assert.ok(fromHistory.strong.includes('bold') && fromHistory.items.length === 2, `a recalled message keeps its formatting: ${JSON.stringify(fromHistory)}`);
    await page.evaluate(() => { window.__chat.formatting.input.value = ''; window.__chat.recall.index = null; });
    // An open picker keeps the arrow keys.
    await page.keyboard.type('@'); await settle(page);
    const open = await page.evaluate(() => window.__chat.mentionAutocomplete.popup.hidden === false);
    if (open) { await press('ArrowUp'); assert.equal(await text(), '@', 'the open mention picker takes Up'); }
    await page.evaluate(() => { window.__chat.formatting.input.value = ''; });
    // The thread panel: the same over the conversation's own messages (the room's own messages are not in it).
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    await page.waitForSelector('#thread-composer .ProseMirror');
    await page.focus('#thread-composer .ProseMirror');
    await press('ArrowUp'); assert.equal(await text(true), 'thread mine two', 'Up in the thread composer recalls the newest own message of the conversation');
    await press('ArrowUp'); assert.equal(await text(true), 'thread mine one');
    await press('ArrowUp'); assert.equal(await text(true), 'thread mine one', 'and stays at the oldest');
    await press('ArrowDown'); await press('ArrowDown'); assert.equal(await text(true), '', 'past the newest the (empty) draft is back');
    assert.equal(await text(), '', 'the room composer is untouched');
    await page.close();
  }

  // Enter on a touch screen with no fine pointer makes a new line and the send button sends; with a fine pointer Enter sends. It is read at every key, so a device that gets a
  // pointer behaves like a desktop at once. Both composers (room and thread panel), the rich editor and the plain textarea, and an open picker still takes Enter.
  for (const plain of [false, true]) {
    const page = await mount(plain ? { plain: true } : {});
    await addReplies(page, 3);
    // The page has no touch device: the media query that asks is answered by a switch the test flips.
    await page.evaluate(() => { const ask = window.matchMedia.bind(window); window.__coarse = true; window.matchMedia = query => (/hover:\s*none/.test(query) && /pointer:\s*coarse/.test(query) ? { matches: window.__coarse, media: query, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} } : ask(query)); });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const room = plain ? '#composer textarea[name=body]' : '#composer .ProseMirror', thread = plain ? '#thread-composer textarea' : '#thread-composer .ProseMirror';
    const value = which => page.evaluate(which => (which === 'thread' ? window.__chat.threadFormatting : window.__chat.formatting).input.value, which);
    const sent = () => page.evaluate(() => window.__requests.filter(request => request.method === 'POST' && /\/messages$/.test(request.path)).length);
    const coarse = on => page.evaluate(on => { window.__coarse = on; }, on);
    // Room composer, touch: Enter is a new line and nothing is sent; the send button sends.
    if (!plain) await page.waitForSelector('#composer .ProseMirror');
    await page.focus(room); await page.keyboard.type('line one'); await page.keyboard.press('Enter'); await page.keyboard.type('line two'); await settle(page);
    assert.deepEqual([await value('room'), await sent()], ['line one\nline two', 0], `${plain ? 'plain' : 'rich'}: Enter is a new line on a touch screen, nothing is sent`);
    await page.click('#composer button[type=submit]'); await settle(page);
    assert.equal(await sent(), 1, 'the send button sends');
    // A pointer appears (a trackpad on an iPad): the next Enter sends.
    await coarse(false);
    await page.focus(room); await page.keyboard.type('now a desktop'); await page.keyboard.press('Enter'); await settle(page);
    assert.equal(await sent(), 2, 'with a fine pointer Enter sends again, with no reload');
    // Thread composer, touch.
    await coarse(true);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length >= 4); await settle(page);
    if (!plain) await page.waitForSelector('#thread-composer .ProseMirror');
    await page.focus(thread); await page.keyboard.type('reply one'); await page.keyboard.press('Enter'); await page.keyboard.type('reply two'); await settle(page);
    assert.deepEqual([await value('thread'), await sent()], ['reply one\nreply two', 2], 'the thread composer: Enter is a new line too');
    await page.click('#thread-composer button[type=submit]'); await settle(page);
    assert.equal(await sent(), 3, 'its send button sends');
    await coarse(false);
    await page.focus(thread); await page.keyboard.type('thread desktop'); await page.keyboard.press('Enter'); await settle(page);
    assert.equal(await sent(), 4, 'the thread composer sends with Enter again with a fine pointer');
    // Shift+Enter is a new line either way, and an open picker still takes Enter on a touch screen.
    await coarse(true);
    await page.focus(room); await page.keyboard.type('@'); await settle(page);
    const open = await page.evaluate(() => window.__chat.mentionAutocomplete.popup.hidden === false);
    if (open) { const before = await value('room'); await page.keyboard.press('Enter'); await settle(page); assert.notEqual(await value('room'), before, 'Enter picks in an open picker'); assert.equal(await sent(), 4, 'and sends nothing'); }
    await page.close();
  }

  // The reader's place survives a live update: the composer keeps its text and caret and the focus, the thread's composer keeps its text, and a button of
  // a message row that has the focus keeps it while rows are added below, and after an older page is put above.
  {
    const page = await mount({ plain: true });
    await addReplies(page, 2);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 3); await settle(page);
    await page.evaluate(() => { window.__chat.threadDraft.value = 'thread draft'; });
    await page.focus('#composer textarea[name=body]'); await page.keyboard.type('main draft'); await page.keyboard.press('Home');
    await page.evaluate(() => { const messages = window.__data.rooms.rm_a.messages; messages.push({ id: 'rm_a_live', seq: messages.length + 1, no: messages.length + 1, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); return window.__chat.refreshMessages(); });
    await settle(page);
    assert.deepEqual(await page.evaluate(() => { const draft = document.querySelector('#composer textarea[name=body]'); return [document.activeElement === draft, draft.value, draft.selectionStart, window.__chat.threadDraft.value, !!document.querySelector('[data-message-id=rm_a_live]')]; }), [true, 'main draft', 0, 'thread draft', true]);
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = 0; });
    await hover(page, '[data-message-id=rm_a_m40] .chat-msg-bubble'); // the row's toolbar is made when it is reached
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
  {
    const page = await panelRoom();
    await page.evaluate(() => window.__chat.loadWorktree());
    await page.waitForFunction(() => !window.__chat.worktreeSection.querySelector('button.btn-danger')?.hidden);
    assert.equal(await page.evaluate(() => window.__chat.worktreeSection.querySelector('button.btn-danger').textContent), 'Remove worktree', 'a live room offers worktree removal');
    await page.evaluate(() => { window.__panel = window.__chat.removeWorktree(); });
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    assert.match(await page.$eval('#app-dialog', node => node.textContent), /Archive this room and remove its worktree.*Agents homed here stop/s);
    await dialogOk(page);
    await page.waitForFunction(() => window.__requests.some(request => request.method === 'DELETE' && request.path === '/api/rooms/rm_a/worktree'));
    await page.close();
  }
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
    // The pane is inset on a desktop (24px above and below, never under the top bar or off the bottom), and Hide room is a button that says what it is (an icon and its name), with a tooltip.
    assert.deepEqual(await page.evaluate(() => { const pane = document.querySelector('#room-info-dialog .settings-pane').getBoundingClientRect(); return [Math.round(pane.top), Math.round(innerHeight - pane.bottom)]; }), [24, 24], 'the room settings pane is inset 24px from the top and the bottom of the window');
    assert.deepEqual(await page.$eval('#room-hide', el => [el.textContent.trim(), !!el.querySelector('.icon-mask-eye-off'), el.title.length > 10, el.classList.contains('btn-icon'), el.getAttribute('aria-label')]), ['', true, true, true, 'Hide room']);
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
    const grow = (page, id, height) => page.evaluate((id, height) => { const body = document.querySelector(`[data-message-id=${id}] .chat-msg-body`); const filler = document.createElement('div'); filler.className = 'late-growth'; filler.style.height = `${height}px`; body.append(filler); }, id, height);
    await grow(page, 'rm_a_m120', 400); await settle(page); await settle(page);
    assert.equal(await page.evaluate(() => { const list = document.querySelector('#messages'); return list.scrollHeight - list.scrollTop - list.clientHeight < 3; }), true, 'a list at the bottom stays pinned when a row above grows');
    await page.evaluate(() => { const list = document.querySelector('#messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = 1500; window.__before = document.querySelector('[data-message-id=rm_a_m60]').getBoundingClientRect().top; window.__top = list.scrollTop; });
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
    assert.deepEqual(await page.evaluate(() => { const rows = [...document.querySelectorAll('#messages > article.chat-msg')]; return [rows.length, rows[0].dataset.messageId, rows.at(-1).dataset.messageId, document.querySelector('#messages').dataset.hasOlder, window.__chat.contextMode]; }), [60, 'rm_a_m72', 'rm_a_m131', 'true', false], 'the live end keeps its cap');
    await page.close();
    const history = await mount({ setup: 'window.__renderCap = 60; window.__liveCap = 60;' });
    await openRoom(history, 'rm_a'); await opened(history); await settle(history);
    await history.evaluate(() => { { const reader = document.querySelector('#messages'); reader.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); reader.scrollTop = 0; } });
    await history.waitForFunction(() => window.__chat.contextMode === true, { timeout: 5000 }); await settle(history);
    assert.deepEqual(await history.evaluate(() => { const rows = [...document.querySelectorAll('#messages > article.chat-msg')]; return [rows.length, rows[0].dataset.messageId, document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden]; }), [60, 'rm_a_m1', false], 'paging back past the cap drops the newest rows and shows a window');
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
    // A restricted room shows a lock beside its title, and an open one does not.
    const lock = () => page.evaluate(() => { const node = document.querySelector('#room-restricted'); return node && [node.hidden, node.title, node.previousElementSibling.id]; });
    await openRoom(page, 'rm_a', { restricted: true }); await opened(page); await settle(page);
    assert.deepEqual(await lock(), [false, 'Restricted room', 'room-title'], 'the header of a restricted room carries a lock beside the title');
    await openRoom(page, 'rm_a', {}); await opened(page); await settle(page);
    assert.equal((await lock())[0], true, 'an unrestricted room has no lock');
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
    assert.deepEqual([state.open, state.rename, state.facts, state.debug], [true, [true, 'rm_a'], ['Kind', 'Task room', 'Status', 'Open', 'Members', '2', 'Room ID', 'rm_a'], 4], 'an owner renames the task room in its own field');
    assert.deepEqual([state.lifecycle, state.leave.shown, state.remove, state.hide], [{ shown: true, disabled: false, label: 'Archive room', title: 'Archive room' }, false, { shown: true, disabled: false, label: 'Delete room', title: 'Delete this room and its messages for good' }, true]);
    assert.deepEqual([state.notes, state.worktree.shown, state.memory.shown], [{ shown: true, text: 'Task notes Notes the agents keep for this task. They stay with this room. Open task notes' }, true, true]);
    await page.$eval('#room-info-close', node => node.click());
    assert.equal((await dialog(page)).open, false, 'Close closes the dialog');
    await page.evaluate(() => { window.__chat.setRoomFacts({ ...window.__chat.room, lifecycle: 'closing' }); window.__chat.showRoomInfo(); }); await settle(page); await settle(page);
    assert.deepEqual((await dialog(page)).lifecycle, { shown: true, disabled: true, label: 'Closing…', title: 'Closing…' }, 'a room that is closing cannot be archived again');
    await page.evaluate(() => window.__chat.setRoomFacts({ ...window.__chat.room, lifecycle: 'closed' })); await settle(page);
    state = await dialog(page);
    assert.deepEqual([state.lifecycle, state.remove], [{ shown: true, disabled: false, label: 'Reopen room', title: 'Reopen room' }, { shown: true, disabled: false, label: 'Delete room', title: 'Delete this room and its messages for good' }], 'the open dialog follows the room\'s lifecycle');
    await page.close();
    page = await show({});
    await page.$eval('#room-remove', node => node.click());
    await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
    assert.match(await page.$eval('#app-dialog', node => node.textContent), /Agents homed here stop.*cannot be restored/s, 'deleting an open room explains the permanent stop');
    await page.$eval('#app-dialog .app-dialog-ok', node => node.click());
    await page.waitForFunction(() => window.__requests.some(request => request.method === 'DELETE' && request.path === '/api/rooms/rm_a'));
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
    await page.$eval('#room-info-agents-tab', node => node.click()); await settle(page);
    assert.deepEqual(await page.$$eval('#room-info-dialog .settings-sub-row', nodes => nodes.map(node => node.textContent.trim())), ['Schedules'], 'a private chat has no agent Control subcategory');
    await page.$eval('#room-info-workspace-tab', node => node.click()); await settle(page);
    assert.deepEqual(await page.$$eval('#room-info-dialog .settings-sub-row', nodes => nodes.map(node => node.textContent.trim())), ['Usage'], 'a private chat has no Git worktree subcategory');
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
  // The rows, the thread's entries and the counts are the store's: the chat holds none of its own and the page shows what the store holds.
  if (ROWS) {
    const page = await mount();
    await addReplies(page, 3);
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.find(message => message.id === 'rm_a_m125').reply_to = 'rm_a_m100'; });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    const own = names => page.evaluate(names => names.map(name => Object.hasOwn(window.__chat, name)), names);
    assert.deepEqual(await own(['rows', 'threadEntries', 'threadEntryIDs', 'threadNewReplyIDs', 'quotes', 'expiry', 'messages', 'threadCounts']), Array(8).fill(false), 'the chat keeps no state of its own for the lists');
    const shown = () => page.evaluate(() => { const store = window.__chat.store; return [store.rows.map(row => row.id).join() === [...document.querySelectorAll('#messages > article.chat-msg')].map(node => node.dataset.messageId).join(), store.threadEntries.map(row => row.id).join() === [...document.querySelectorAll('#thread-messages > article')].map(node => node.dataset.messageId).join(), store.rows.length, store.threadEntries.length]; });
    assert.deepEqual(await shown(), [true, true, 100, 4], 'the page shows what the store holds');
    // computing a row's view writes nothing, and the page's effects change no store state: the transitions are the store's
    const probe = await page.evaluate(() => {
      const chat = window.__chat, store = chat.store, reply = store.rows.find(row => row.reply_to), item = { id: 'inb-x', message_id: 'x', kind: 'question', rev: 0, proposed_answers: ['Go'], can_decide: true };
      const out = {};
      store.quotes.delete(reply.id); chat.rowView(reply, true); out.quoteRead = store.quotes.has(reply.id);
      store.answeredMessages.add('x'); store.answerItems = new Map([['x', item]]); chat.answerView({ id: 'x' }); out.answeredRead = store.answeredMessages.has('x');
      store.updateAnswerControls([item]); out.answeredCleared = !store.answeredMessages.has('x');
      store.updateAnswerControls([]); out.answeredSet = store.answeredMessages.has('x') ? 'kept' : 'not set (no row)';
      const files = [{ id: 'att_1', mime: 'image/png', url: '/x.png', name: 'x.png' }]; store.pendingAttachments = files;
      chat.sentMessage(); out.sentKept = store.pendingAttachments === files;
      const room = chat.room; chat.beginOpen({ ...room, id: 'rm_other' }); out.openKept = store.pendingAttachments === files; chat.room = room;
      store.pendingAttachments = []; store.answeredMessages.delete('x');
      return out;
    });
    assert.deepEqual(probe, { quoteRead: false, answeredRead: true, answeredCleared: true, answeredSet: 'not set (no row)', sentKept: true, openKept: true }, 'a row view and the page effects write no store state');
    await page.evaluate(() => window.__chat.store.dropRows(window.__chat.store.rows.slice(0, 10).map(row => row.id)));
    assert.deepEqual(await shown(), [true, true, 90, 4], 'and follows a drop');
    await page.evaluate(() => window.__chat.reset());
    assert.deepEqual(await shown(), [true, true, 0, 0], 'and a reset');
    await page.close();
  }
  // A row's toolbar is not in the page until the row is reached: 100 rows used to carry 3000 nodes of buttons nobody was looking at. Hovering, focusing
  // or touching a row makes its toolbar, only the last three made are kept, and a row that holds the focus keeps its own.
  if (ROWS) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const built = () => page.evaluate(() => [...document.querySelectorAll('#messages .message-actions')].filter(bar => bar.dataset.built).map(bar => bar.closest('.chat-msg').dataset.messageId));
    const rowsNow = await page.evaluate(() => document.querySelectorAll('#messages > article.chat-msg[data-message-id]').length);
    assert.deepEqual([rowsNow > 5, await built(), await page.evaluate(() => document.querySelectorAll('#messages .message-actions button:not(.message-actions-wake)').length)], [true, [], 0], 'no row has a toolbar before it is reached');
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('#messages .message-actions')).width), '1px', 'and an unmade toolbar is not drawn, only its hidden "Message actions" button is left');
    await hover(page, '#messages [data-message-id=rm_a_m120] .chat-msg-bubble');
    assert.deepEqual(await built(), ['rm_a_m120'], 'a hovered row makes its toolbar');
    assert.equal(await page.evaluate(() => document.querySelectorAll('[data-message-id=rm_a_m120] .message-actions > button').length), 6, 'with its six buttons');
    for (const id of ['rm_a_m121', 'rm_a_m122', 'rm_a_m123', 'rm_a_m124']) await hover(page, `#messages [data-message-id=${id}] .chat-msg-bubble`);
    const live = await built();
    assert.equal(live.length <= 3 && live.includes('rm_a_m124') && !live.includes('rm_a_m120'), true, `only the last three rows reached keep a toolbar: ${live}`);
    // The keyboard reaches a row's toolbar too: focus lands on something in the row, the toolbar is made, and the next Tab is its first button.
    await page.mouse.move(2, 2);
    await page.evaluate(() => document.querySelector('#messages [data-message-id=rm_a_m125] .chat-msg-id').focus());
    assert.equal((await built()).includes('rm_a_m125'), true, 'focus in a row makes its toolbar');
    await page.keyboard.press('Tab'); await page.keyboard.press('Tab');
    const reached = await page.evaluate(() => document.activeElement.closest('.message-actions')?.closest('.chat-msg')?.dataset.messageId || '');
    assert.equal(reached, 'rm_a_m125', 'and Tab goes on into it');
    // Shift+Tab from the row below lands in the row's toolbar (its last button), not before an empty one.
    await page.evaluate(() => document.activeElement.blur());
    await page.mouse.move(2, 2);
    await page.evaluate(() => { document.querySelector('#messages [data-message-id=rm_a_m129] .chat-msg-id').focus(); });
    const before = 'rm_a_m128';
    await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    const back = await page.evaluate(() => ({ row: document.activeElement.closest('.chat-msg')?.dataset.messageId, inBar: !!document.activeElement.closest('.message-actions'), last: document.activeElement === document.activeElement.closest('.message-actions')?.querySelector(':scope > button:last-of-type') }));
    assert.deepEqual([back.row, back.inBar, back.last], [before, true, true], `reverse Tab from the next row enters the toolbar at its last button: ${JSON.stringify(back)}`);
    // A row that holds the focus keeps its toolbar however many others are reached.
    await page.evaluate(() => document.querySelector('#messages [data-message-id=rm_a_m125] .chat-msg-id').focus());
    for (const id of ['rm_a_m126', 'rm_a_m127', 'rm_a_m128', 'rm_a_m129']) await page.evaluate(id => document.querySelector(`#messages [data-message-id=${id}] .chat-msg-bubble`).dispatchEvent(new PointerEvent('pointerover', { bubbles: true })), id);
    assert.equal((await built()).includes('rm_a_m125'), true, 'a focused row keeps its toolbar');
    await page.close();
  }
  // The message list's rows follow the expiry window and their thread's count without a redraw of the others.
  if (ROWS) {
    const page = await mount();
    await addReplies(page, 3);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await hover(page, '#messages [data-message-id=rm_a_m120] .chat-msg-bubble'); // its toolbar is made when the row is reached
    const state = () => page.evaluate(() => {
      const rows = [...document.querySelectorAll('#messages > article.chat-msg[data-message-id]')];
      const root = document.querySelector('#messages [data-message-id=rm_a_m120]');
      return { expired: rows.filter(node => node.classList.contains('chat-msg-expired')).length, marks: document.querySelectorAll('#messages .chat-msg-expired-icon').length,
        boundary: rows.filter(node => node.classList.contains('expiry-boundary')).map(node => node.dataset.seq), labels: document.querySelectorAll('#messages .expiry-label').length,
        chip: root.querySelector(':scope > .chat-msg-bubble > .thread-chip').textContent, action: root.querySelector('.thread-action').title, toolbar: root.querySelector('.message-actions').getAttribute('aria-label') };
    });
    const before = await state();
    assert.equal(before.chip, '3 replies');
    await page.evaluate(async () => {
      const { retention } = await import('/js/retention.js'), chat = window.__chat;
      chat.rows = chat.rows.map(message => ({ ...message, expirable: true, created_at: message.seq <= 60 ? '2020-01-01T00:00:00Z' : new Date().toISOString() }));
      retention.seconds = 3600; chat.store.syncExpiry();
    });
    const marked = await state();
    assert.deepEqual([marked.expired, marked.marks, marked.boundary, marked.labels], [27, 27, ['61'], 1], 'the old rows carry the hourglass and the first newer one the boundary');
    await page.evaluate(async () => { const { retention } = await import('/js/retention.js'); retention.seconds = 0; window.__chat.store.syncExpiry(); });
    assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#messages .chat-msg-expired').length, document.querySelectorAll('#messages .expiry-boundary').length, document.querySelectorAll('#messages .chat-msg-expired-icon, #messages .expiry-label').length]), [0, 0, 0], 'and they leave when expiry is turned off');
    await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_t9', seq: 998, no: 998, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'one more', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: 'rm_a_m120' }); return window.__chat.refreshMessages(); });
    await settle(page);
    const grown = await state();
    assert.deepEqual([grown.chip, grown.action === before.action], ['4 replies', false], 'a new reply updates its root\'s chip and thread action');
    await page.evaluate(() => { const chat = window.__chat; chat.rows = chat.rows.map(message => message.id === 'rm_a_m120' ? { ...message, no: 4242 } : message); chat.listUpdate(); });
    assert.equal((await state()).toolbar, 'Actions for message #4242', 'a changed record draws its toolbar again');
    assert.equal(await page.evaluate(() => document.querySelector('#messages [data-message-id=rm_a_m120] .message-reply').title), 'Reply to #4242', 'and its buttons');
    await page.close();
  }

  // The thread panel's rows draw only when their record or their keys change, and leave with their thread.
  if (ROWS) {
    const renders = page => page.evaluate(() => import('/js/pages/chat/rows.js').then(rows => rows.rowRenders.thread));
    const entries = page => page.evaluate(() => [...document.querySelectorAll('#thread-messages > article.thread-entry')].map(node => node.dataset.messageId));
    const opened_ = async () => {
      const page = await mount();
      await addReplies(page, 3);
      await page.evaluate(() => {
        const messages = window.__data.rooms.rm_a.messages, last = messages.find(message => message.id === 'rm_a_t2');
        last.author_kind = 'agent'; last.author_id = 'agt_x'; last.author_name = 'tess'; last.author_tool = 'claude'; last.author_model = 'm';
        window.__data.agents = [{ id: 'agt_x', room_id: 'rm_a', slug: 'tess', name: 'tess', tool: 'claude', model: 'm', role_emoji: '🛠', role: 'dev', project_id: 'prj_a' }];
      });
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await chip(page, 'rm_a_m120');
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
      return page;
    };
    {
      const page = await opened_();
      const delta = async action => { const before = await renders(page); await page.evaluate(action); await settle(page); return (await renders(page)) - before; };
      assert.deepEqual(await entries(page), ['rm_a_m120', 'rm_a_t1', 'rm_a_t2', 'rm_a_t3']);
      await page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_t4', seq: 999, no: 999, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'live reply', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: 'rm_a_m120' }); });
      assert.equal(await delta(() => window.__chat.refreshMessages()), 1, 'a live reply draws one row');
      await page.evaluate(() => { const chat = window.__chat; chat.threadEntries = chat.threadEntries.map(message => message.id === 'rm_a_t3' ? { ...message, body: 'a **bold** b' } : message); chat.threadUpdate(); });
      const bold = () => page.evaluate(() => !!document.querySelector('#thread-messages [data-message-id=rm_a_t3] .chat-msg-body strong'));
      assert.equal(await bold(), true, 'rendered Markdown');
      assert.equal(await delta(() => window.__chat.setRaw(true)), 5, 'the Markdown mode draws every row');
      assert.equal(await bold(), false, 'and shows the raw text');
      assert.equal(await delta(() => window.__chat.setRaw(false)), 5, 'and back');
      assert.equal(await delta(() => { window.__chat.mentionIndex = { signature: 'other' }; window.__chat.threadUpdate(); }), 5, 'a new mention index draws every row');
      assert.equal(await delta(() => { window.__chat.mentionIndex = null; window.__chat.threadUpdate(); }), 5, 'and back');
      assert.equal(await delta(() => window.__chat.threadUpdate()), 0, 'an update with nothing new draws nothing');
      const item = { id: 'inb', message_id: 'rm_a_t1', kind: 'question', rev: 0, proposed_answers: ['Go'], free_text: true, can_decide: true };
      assert.equal(await delta(() => window.__chat.updateAnswerControls([{ id: 'inb', message_id: 'rm_a_t1', kind: 'question', rev: 0, proposed_answers: ['Go'], free_text: true, can_decide: true }])), 1, 'an answer for one reply draws that row');
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#thread-messages .answer-controls')].map(node => node.closest('article').dataset.messageId)), ['rm_a_t1']);
      const goButtons = () => page.evaluate(() => [...document.querySelectorAll('#thread-messages [data-message-id=rm_a_t1] .answer-controls .proposed-answer-btn')].map(button => button.textContent).join());
      assert.equal(await goButtons(), 'Go', 'the buttons of the question');
      assert.equal(await delta(() => window.__chat.updateAnswerControls([{ id: 'inb', message_id: 'rm_a_t1', kind: 'question', rev: 1, proposed_answers: ['Go', 'Other'], free_text: true, can_decide: true }])), 1, 'a new revision draws it again');
      assert.equal(await goButtons(), 'Go,Other', 'with the new revision\'s buttons');
      assert.equal(await delta(() => window.__chat.updateAnswerControls([])), 1, 'a closed question draws its row');
      assert.equal(await page.evaluate(() => !!document.querySelector('#thread-messages [data-message-id=rm_a_t1] .answer-done')), true, 'and leaves "Answered"');
      await page.evaluate(() => window.__chat.setAgents([{ id: 'agt_x', room_id: 'rm_a', slug: 'tess', name: 'tess', tool: 'claude', model: 'm', role_emoji: '🛠', role: 'dev', project_id: 'prj_a' }])); await settle(page);
      assert.equal(await delta(() => { window.__chat.setAgents([{ id: 'agt_x', room_id: 'rm_a', slug: 'tess', name: 'tess', tool: 'claude', model: 'm', role_emoji: '🔧', role: 'dev', project_id: 'prj_a' }]); }), 1, 'a changed sender draws only its rows');
      assert.equal(await page.evaluate(() => document.querySelector('#thread-messages [data-message-id=rm_a_t2] .message-role').textContent), '🔧');
      assert.equal(await delta(() => { window.__chat.setAgents([{ id: 'agt_x', room_id: 'rm_a', slug: 'tess', name: 'tess', tool: 'codex', model: 'm', role_emoji: '🔧', role: 'dev', project_id: 'prj_a' }]); }), 1, 'a changed agent redraws only its rows');
      assert.match(await page.evaluate(() => document.querySelector('#thread-messages [data-message-id=rm_a_t2] .msg-sender-icon').getAttribute('src')), /claude/, 'the old message keeps its recorded harness icon');
      // a changed record draws its row, one field at a time
      const fields = [
        ['no', { no: 4242 }, () => document.querySelector('#thread-messages [data-message-id=rm_a_t3] .chat-msg-id').textContent === '#4242'],
        ['body', { body: 'changed words' }, () => document.querySelector('#thread-messages [data-message-id=rm_a_t3] .chat-msg-body').textContent.includes('changed words')],
        ['created_at', { created_at: '2020-02-02T02:02:02Z' }, () => document.querySelector('#thread-messages [data-message-id=rm_a_t3] time').dateTime === '2020-02-02T02:02:02Z'],
        ['needs_attention', { needs_attention: true }, () => !!document.querySelector('#thread-messages [data-message-id=rm_a_t3] .chat-msg-attention-icon') && document.querySelector('#thread-messages [data-message-id=rm_a_t3]').classList.contains('needs-attention')],
        ['usage', { usage: { input_tokens: 5, output_tokens: 6 } }, () => !!document.querySelector('#thread-messages [data-message-id=rm_a_t3] .chat-msg-usage-toggle')],
        ['visual_plan', { visual_plan: [{ type: 'markdown', title: 'Plan', data: { markdown: 'Plan **text**' } }] }, () => !!document.querySelector('#thread-messages [data-message-id=rm_a_t3] .visual-plan .visual-plan-block')],
        ['proposed_answers', { proposed_answers: ['Proceed', 'Hold'] }, () => document.querySelector('#thread-messages [data-message-id=rm_a_t3] .answer-history')?.textContent === 'No longer pending'],
        ['proposed_answers again', { proposed_answers: ['Other'] }, () => document.querySelector('#thread-messages [data-message-id=rm_a_t3] .answer-history')?.textContent === 'No longer pending'],
      ];
      for (const [name, change, check] of fields) {
        const drawn = await page.evaluate(async (change, source) => {
          const rows = await import('/js/pages/chat/rows.js'), chat = window.__chat, before = rows.rowRenders.thread;
          chat.threadEntries = chat.threadEntries.map(message => message.id === 'rm_a_t3' ? { ...message, ...change } : message); chat.threadUpdate();
          return rows.rowRenders.thread - before;
        }, change);
        assert.equal(drawn, 1, `a changed ${name} draws one row`);
        assert.equal(await page.evaluate(check), true, `a changed ${name} shows`);
      }
      // the rows leave with their thread
      await page.evaluate(() => window.__chat.closeThread()); await settle(page);
      assert.deepEqual(await entries(page), [], 'closing the thread removes its rows');
      await chip(page, 'rm_a_m120');
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await openRoom(page, 'rm_b'); await opened(page); await settle(page);
      assert.deepEqual(await entries(page), [], 'leaving the room removes them too');
      await page.close();
    }
  }

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
        for (const name of ['beginOpen', 'pageDrawn', 'showNewest', 'loadOlderIfShort', 'announce', 'updateLatestLabel', 'showOlderStatus', 'holdPlace', 'say', 'anchorUpdate', 'showLatestButton', 'beginWindow', 'windowDrawn', 'showWindow', 'showReply', 'showRow', 'beginThread', 'showThreadPage', 'showThreadOlderStatus', 'holdThreadPlace', 'beginThreadSend', 'sentThreadReply', 'threadSendFailed', 'restoreThreadDraft', 'sendInput', 'beginSend', 'sendAsAnswer', 'sentMessage', 'sendFailed', 'sendFinished', 'listUpdate', 'threadUpdate', 'threadClosed', 'updateThreadNewMarker', 'hideThreadLatest', 'noteRow', 'scheduleMentions', 'showThreadCount', 'followList', 'renderPending']) { const original = chat[name]; chat[name] = function (...args) { hostCalls.push(name); return original.apply(this, args); }; }
        chat.room = { id: 'rm_a', lifecycle: 'open' }; store.expiry = { expired: new Set(['zz']), divider: null }; store.hasMoreOlder = true; store.oldestSeq = 5; chat.nodes.set('rm_a_m1', document.createElement('article'));
        chat.threadRoot = { id: 'rm_a_m120', seq: 120 }; store.threadHasMoreOlder = true; store.threadOldestSeq = 5; chat.threadDraft.value = 'late reply'; chat.draft.value = 'late message';
        for (const name of ['open', 'openThread']) { const original = store[name]; store[name] = function (...args) { hostCalls.push(`store.${name}`); return original.apply(this, args); }; }
        store.windowSeq = 5; store.contextMode = true; store.newestSeq = 9; const before = [store.windowSeq, store.contextMode, store.newestSeq, store.catchingUp, store.threadGeneration, store.loadingOlderThread];
        for (const call of [() => store.open({ id: 'rm_a' }), () => store.refresh(), () => store.loadOlder(), () => store.loadNewer(), () => store.leaveWindow(), () => store.jump('rm_a_m10'), () => store.jump(7), () => store.refreshReactions('rm_a_m1'), () => store.toggleReaction('rm_a_m1', '👍'), () => store.openThread({ id: 'rm_a_m120', seq: 120 }), () => store.loadOlderThread(), () => store.sendThread(), () => store.send(), () => store.reopenLatest(), () => store.syncExpiry(), () => store.addFiles([new File(['x'], 'a.txt')]), () => store.removeAttachment({ id: 'att_1' })]) { try { await call(); } catch (error) { hostCalls.push(`threw ${error.message}`); } }
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
      await page.evaluate(() => { window.__settled = 0; const store = window.__chat.store, settled = store.settleThreadLatest; store.settleThreadLatest = function (...args) { window.__settled++; return settled.apply(this, args); }; window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120'), { id: 'rm_a_t2' }); });
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
      await chip(page, 'rm_a_m120');
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
      await chip(page, 'rm_a_m120');
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
    // older replies go above the first reply without moving what the reader looks at
    {
      const page = await mount();
      await addReplies(page, 130);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await chip(page, 'rm_a_m120');
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 50); await settle(page);
      await hold(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { document.querySelector('#thread-messages').scrollTop = 0; });
      const held = await waitRequest(page, 'GET', 'thread_root=.*before=');
      await page.evaluate(() => { window.__holds.length = 0; });
      const before = await page.evaluate(() => { const node = document.querySelector('#thread-messages').children[1]; return { id: node.dataset.messageId, top: node.getBoundingClientRect().top }; });
      await serve(page, held);
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article').length > 101, { timeout: 5000 }); await settle(page);
      const after = await page.evaluate(id => document.querySelector(`#thread-messages [data-message-id=${id}]`).getBoundingClientRect().top, before.id);
      assert.ok(Math.abs(after - before.top) < 1.5, `the reader's reply stays where it was (${before.top} -> ${after})`);
      await page.close();
    }
    // a failed older-replies read of a thread that was closed shows no error in the next
    {
      const page = await mount();
      await addReplies(page, 130);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await chip(page, 'rm_a_m120');
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
      await page.evaluate(() => { window.__drawn = 0; const draw = window.__chat.updateThreadNewMarker; window.__chat.updateThreadNewMarker = function (...args) { window.__drawn++; return draw.apply(this, args); }; window.__open = window.__chat.openThread(window.__chat.messages.get('rm_a_m120')); });
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
      await chip(page, 'rm_a_m120');
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
      await chip(page, 'rm_a_m120');
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await hold(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__shown = 0; window.__failed = 0; const sent = window.__chat.sentThreadReply; window.__chat.sentThreadReply = function (...args) { window.__shown++; return sent.apply(this, args); }; window.__chat.threadDraft.value = 'late'; window.__late = window.__chat.sendThread(); });
      const held = await waitRequest(page, 'POST', '/messages$');
      await page.evaluate(() => { window.__holds.length = 0; });
      if (how === 'destroy') await page.evaluate(() => window.__chat.destroy());
      else {
        await openRoom(page, 'rm_b'); await opened(page); await settle(page);
        await openRoom(page, 'rm_a'); await opened(page); await settle(page);
        await chip(page, 'rm_a_m120');
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
      await chip(page, 'rm_a_m120');
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
    // each message leaf is its builder's root plus its fill
    {
      const page = await mount();
      const same = await page.evaluate(async () => {
        const plan = await import('/js/visual-plan.js'), sender = await import('/js/sender-identity.js');
        const make = ({ tag, class: className, ...attributes }, fill) => { const root = document.createElement(tag); root.className = className; for (const [name, value] of Object.entries(attributes)) root.setAttribute(name, value); fill(root); return root.outerHTML; };
        const message = { id: 'm1', no: 7, visual_plan: [{ type: 'markdown', title: 'Plan', data: { markdown: 'Plan **text**' } }], appendix_pages: [{ title: 'Page', body: 'Body' }], proposed_answers: ['Proceed', 'Hold'], open_questions: [{ question: 'Scope?', options: ['Small', 'Large'] }] };
        return [
          [plan.renderVisualPlan(message).outerHTML, make(plan.VISUAL_PLAN_ROOT, root => plan.fillVisualPlan(root, message))],
          [sender.createSenderPresence().outerHTML, make(sender.SENDER_PRESENCE_ROOT, root => sender.fillSenderPresence(root))],
        ].map(([built, filled]) => built === filled);
      });
      assert.deepEqual(same, [true, true], 'each leaf equals its root plus fill');
      await page.close();
    }
    // a pick of several files stops once the room has changed
    {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'POST', '/attachments$');
      await page.evaluate(() => { window.__adding = window.__chat.addFiles([new File(['one'], 'one.txt'), new File(['two'], 'two.txt')]); });
      const first = await waitRequest(page, 'POST', '/attachments$');
      await page.evaluate(() => { window.__holds.length = 0; });
      await openRoom(page, 'rm_b'); await opened(page); await settle(page);
      await serve(page, first); await page.evaluate(() => window.__adding); await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'POST' && /attachments$/.test(request.path)).length), 1, 'the second file of a pick is not sent from another room');
      await page.close();
    }
    // removing a pending file: the answer of a room that was left changes and says nothing
    for (const how of ['removed', 'failed', 'failed here']) {
      const page = await mount();
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await hold(page, 'POST', '/attachments$');
      await addFile(page, 'x.txt');
      await serve(page, await waitRequest(page, 'POST', '/attachments$')); await page.evaluate(() => window.__adding); await settle(page);
      await page.evaluate(() => { window.__holds.length = 0; });
      await hold(page, 'DELETE', '/attachments/att_1$');
      await page.$eval('#pending-attachments .attachment-pending-item button', node => node.click());
      const held = await waitRequest(page, 'DELETE', '/attachments/att_1$');
      await page.evaluate(() => { window.__holds.length = 0; window.__redrawn = 0; window.__said = 0; const chat = window.__chat, pending = chat.renderPending, say = chat.say; chat.renderPending = function (...args) { window.__redrawn++; return pending.apply(this, args); }; chat.say = function (...args) { window.__said++; return say.apply(this, args); }; });
      if (how !== 'failed here') { await openRoom(page, 'rm_b'); await opened(page); await settle(page); await page.evaluate(() => { window.__redrawn = 0; window.__said = 0; }); }
      if (how === 'removed') await serve(page, held); else await answer(page, held, 500, { error: 'cannot remove' });
      await settle(page);
      const result = await page.evaluate(() => [window.__redrawn, window.__said, document.querySelector('#chat-error').textContent]);
      assert.deepEqual(result, how === 'failed here' ? [0, 1, 'Request failed (500)'] : [0, 0, ''], `a removal ${how === 'failed here' ? 'that failed here says why' : `that ${how} after the room changed changes and says nothing`}`);
      await page.close();
    }
    // the live end keeps its cap, but not while an older page is in flight
    {
      const page = await mount();
      await page.evaluate(() => { window.__liveCap = 20; });
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      const rows = () => page.evaluate(() => window.__chat.store.rows.length);
      assert.equal(await rows(), 100, 'a page of rows');
      await page.evaluate(() => { const store = window.__chat.store; store.loadingOlder = true; store.trimLive(); });
      assert.equal(await rows(), 100, 'nothing is dropped while an older page is out');
      await page.evaluate(() => { const store = window.__chat.store; store.loadingOlder = false; store.trimLive(); });
      assert.equal(await rows(), 20, 'and the live cap applies once it is back');
      await page.close();
    }
    // with no room the list calls resolve and send nothing
    {
      const page = await mount();
      const outcome = await page.evaluate(async () => { document.querySelector('#composer textarea[name=body]').value = 'x'; window.__chat.threadDraft.value = 'x'; const store = window.__chat.store, sent = window.__requests.length, results = []; for (const call of [() => store.refresh(), () => store.loadOlder(), () => store.loadNewer(), () => store.jump('rm_a_m1'), () => store.jump(7), () => store.toggleReaction('rm_a_m1', '👍'), () => store.loadOlderThread(), () => store.sendThread(), () => store.send(), () => store.addFiles([new File(['x'], 'a.txt')]), () => store.removeAttachment({ id: 'att_1' })]) { try { await call(); results.push('ok'); } catch (error) { results.push(error.message); } } return [results, window.__requests.length - sent]; });
      assert.deepEqual(outcome, [['ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok'], 0], 'no room: the calls resolve and send nothing');
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

  // The thread panel's root is destroyed with the chat: it is unmounted once and later updates draw nothing.
  if (ROWS) {
    const page = await mount();
    await addReplies(page, 3);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    const result = await page.evaluate(() => {
      const chat = window.__chat, root = chat.threadView, destroy = root.destroy; let destroyed = 0;
      root.destroy = (...args) => { destroyed++; return destroy.apply(root, args); };
      const entries = chat.threadEntries.slice();
      chat.destroy();
      chat.threadEntries = entries; chat.threadUpdate();
      return { destroyed, drawn: document.querySelectorAll('#thread-messages > *').length };
    });
    assert.deepEqual(result, { destroyed: 1, drawn: 0 }, 'a destroyed chat unmounts the thread panel once and draws nothing after');
    await page.close();
  }
  // The message list's root goes the same way.
  if (ROWS) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const result = await page.evaluate(() => {
      const chat = window.__chat, root = chat.listView, destroy = root.destroy; let destroyed = 0;
      root.destroy = (...args) => { destroyed++; return destroy.apply(root, args); };
      const rows = chat.rows.slice();
      chat.destroy();
      chat.rows = rows; chat.listUpdate(); chat.setAgents([]);
      return { destroyed, drawn: document.querySelectorAll('#messages > *').length };
    });
    assert.deepEqual(result, { destroyed: 1, drawn: 0 }, 'a destroyed chat unmounts the message list once and draws nothing after');
    await page.close();
  }
  // So does the pending attachments' root.
  if (ROWS) {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    const result = await page.evaluate(() => {
      const chat = window.__chat, root = chat.pendingView, destroy = root.destroy; let destroyed = 0;
      root.destroy = (...args) => { destroyed++; return destroy.apply(root, args); };
      chat.destroy();
      chat.pendingAttachments = [{ id: 'att_1', mime: 'image/png', url: '/x.png', name: 'x.png' }]; chat.renderPending();
      return { destroyed, drawn: document.querySelectorAll('#pending-attachments > *').length };
    });
    assert.deepEqual(result, { destroyed: 1, drawn: 0 }, 'a destroyed chat unmounts the pending attachments once and draws nothing after');
    await page.close();
  }

  // A destroyed chat (the replacement's adapter has destroy(); the hand-written chat does not): what was still out when it was destroyed changes nothing
  // and starts no request, and none of its entries sends one afterwards.
  if (REPLACED || STORE) {
    const destroyed = [
      ['open', {}, page => openRoom(page, 'rm_a'), 'GET', 'rooms/rm_a/messages\\?limit=100'],
      ['refresh', { opened: true }, page => page.evaluate(() => { window.__data.rooms.rm_a.messages.push({ id: 'rm_a_m131', seq: 131, no: 131, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'late', created_at: '2026-01-10T12:00:00Z', reply_to: '', thread_root: '' }); window.__late = window.__chat.refreshMessages(); }), 'GET', 'rooms/rm_a/messages\\?after='],
      ['older', { opened: true }, page => page.evaluate(() => { { const reader = document.querySelector('#messages'); reader.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); reader.scrollTop = 0; } }), 'GET', 'rooms/rm_a/messages\\?before='],
      ['jump', { opened: true }, page => page.evaluate(() => { window.__late = window.__chat.jumpToMessage('rm_a_m10'); }), 'GET', 'rooms/rm_a/messages\\?around='],
      ['number lookup', { opened: true, other: true }, page => page.evaluate(() => { window.__late = window.__chat.jumpToMessage(1003); }), 'GET', 'message-numbers/'],
      ['forward page', { opened: true, windowed: true }, page => page.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; }), 'GET', 'rooms/rm_a/messages\\?after=100'],
      ['send', { opened: true }, async page => { await typeDraft(page, 'out the door'); await submit(page); }, 'POST', '/messages$'],
      ['thread read', { opened: true, replies: true }, page => chip(page, 'rm_a_m120'), 'GET', 'thread_root='],
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
      if (setup.threadOpen) { await chip(page, 'rm_a_m120'); await page.waitForFunction(count => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1, {}, 1); await settle(page); }
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

  // The thread panel's chrome: both composers' toolbar buttons share one top (dark and both high-contrast themes), the close button is the panel header's right end, with
  // the members column collapsed the button that brings it back is the right-most control after the close button (and stays in the room header without a thread), the
  // thread's resize handle has no grip dots (the others keep theirs), and a pressed format button shows in the high-contrast themes.
  {
    const page = await mount();
    await addReplies(page, 3);
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.$eval('[data-message-id=rm_a_m120] .thread-chip', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 4); await settle(page);
    for (const theme of ['dark', 'high-contrast-dark', 'high-contrast-light']) {
      await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await settle(page);
      const seen = await page.evaluate(() => {
        const tops = form => [...document.querySelectorAll(`${form} :is(.attachment-picker-btn, .composer-format)`)].map(el => Math.round(el.getBoundingClientRect().top * 10) / 10);
        const main = tops('#composer'), thread = tops('#thread-composer');
        const panel = document.querySelector('#thread-panel').getBoundingClientRect(), close = document.querySelector('#close-thread').getBoundingClientRect();
        return { main: [...new Set(main)], thread: [...new Set(thread)], counts: [main.length, thread.length], closeGap: Math.round(panel.right - close.right) };
      });
      assert.deepEqual([...new Set(seen.main.concat(seen.thread))].length, 1, `every toolbar button of both composers has the same top in ${theme}: ${JSON.stringify(seen)}`);
      assert.ok(seen.counts[0] > 3 && seen.counts[1] > 3, `both toolbars have their buttons: ${JSON.stringify(seen.counts)}`);
      assert.ok(seen.closeGap >= 0 && seen.closeGap <= 16, `the close button is at the thread header's right edge in ${theme}: ${seen.closeGap}px`);
      // A pressed format button reads as pressed in every theme (in the high-contrast ones it used to look like the others).
      const look = () => page.evaluate(() => { const style = getComputedStyle(document.querySelector('#thread-composer .composer-format')); return [style.backgroundColor, style.borderTopColor].join('|'); });
      const flip = async value => { await page.evaluate(value => document.querySelector('#thread-composer .composer-format').setAttribute('aria-pressed', value), value); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 300))); return look(); }; // (colours fade over a moment)
      const pressed = { off: await flip('false'), on: await flip('true') }; await flip('false');
      assert.notEqual(pressed.on, pressed.off, `a pressed format button looks different in ${theme}: ${JSON.stringify(pressed)}`);
    }
    // The members column collapsed, a thread open: the expand button is the right-most control of the row, right of the close button, inside the chat's right edge.
    await page.evaluate(() => document.querySelector('.chat').classList.add('agents-collapsed'));
    await settle(page);
    const withThread = await page.evaluate(() => { const expand = document.querySelector('#edge-agents-expand').getBoundingClientRect(), close = document.querySelector('#close-thread').getBoundingClientRect(), chat = document.querySelector('.chat').getBoundingClientRect(), title = document.querySelector('#thread-panel h2').getBoundingClientRect(); return { expand: getComputedStyle(document.querySelector('#edge-agents-expand')).display !== 'none' && expand.left >= close.right, insideChat: expand.right <= chat.right + 0.5, sameRow: Math.abs(expand.top + expand.height / 2 - (title.top + title.height / 2)) <= 3 }; });
    assert.deepEqual(withThread, { expand: true, insideChat: true, sameRow: true }, 'with the members collapsed and a thread open, the expand button follows the close button on the same row');
    await page.$eval('#close-thread', node => node.click()); await settle(page);
    const noThread = await page.evaluate(() => { const expand = document.querySelector('#edge-agents-expand'), style = getComputedStyle(expand), header = document.querySelector('.chat > header').getBoundingClientRect(); const rect = expand.getBoundingClientRect(); return { position: style.position, inHeader: rect.right <= header.right + 0.5 && rect.left >= header.left }; });
    assert.deepEqual(noThread, { position: 'static', inHeader: true }, 'without a thread the expand button stays at the end of the room header');
    await page.close();
  }
  // A reply's quote names its author the way a message header does, also an agent the page learns of after the row was drawn, and is v1's: a left bar, the accent
  // text, no fill, across the bubble's width.
  {
    const page = await mount();
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await page.evaluate(() => { document.documentElement.dataset.theme = 'high-contrast-dark'; window.__chat.store.add({ id: 'q_late', seq: 951, no: 951, room_id: 'rm_a', author_kind: 'user', author_id: 'usr_ann', author_name: 'ann', body: 'ola', created_at: new Date().toISOString(), reply_to: 'unloaded_parent', reply_preview: { no: 77, author_kind: 'agent', author_id: 'agt_3f9a2b', snippet: 'earlier text' } }); });
    await settle(page);
    const quote = () => page.evaluate(() => { const el = document.querySelector('[data-message-id=q_late] .quote'), style = getComputedStyle(el), bubble = el.closest('.chat-msg-bubble'), cell = getComputedStyle(bubble); return { text: el.textContent, fill: style.backgroundColor, bar: style.borderLeftWidth, full: Math.abs(el.getBoundingClientRect().width - (bubble.getBoundingClientRect().width - parseFloat(cell.paddingLeft) - parseFloat(cell.paddingRight) - parseFloat(cell.borderLeftWidth) - parseFloat(cell.borderRightWidth))) <= 1 }; });
    const before = await quote();
    assert.match(before.text, /^↪ #77 Agent 3f9a2b: earlier text$/, 'an agent the page does not know yet reads as its short id');
    assert.deepEqual([before.fill, before.bar, before.full], ['rgba(0, 0, 0, 0)', '2px', true], `the quote has no fill and runs across the bubble: ${JSON.stringify(before)}`);
    await page.evaluate(() => window.__chat.setAgents([{ id: 'agt_3f9a2b', slug: 'owen', name: 'owen', project_id: 'prj_a', room_id: 'rm_a', status: 'online' }]));
    await settle(page);
    const after = await quote();
    assert.match(after.text, /^↪ #77 owen\S*: earlier text$/, `once the agent is known the quote names it, as the message header does: ${after.text}`);
    await page.close();
  }
  // What a plain send in the panel answers when the newest replies are all the viewer's own: the search goes back, past the loaded window and past a server page, to the last
  // reply of someone else; with only the viewer's own replies it falls back to the first message.
  if (ROWS) {
    const send = async (page, fixture) => {
      await page.evaluate(fixture => {
        const messages = window.__data.rooms.rm_a.messages, start = messages.length;
        fixture.forEach((item, i) => messages.push({ id: `rm_a_${item.id}`, seq: start + 1 + i, no: start + 1 + i, room_id: 'rm_a', author_kind: 'user', author_id: item.mine ? 'usr_me' : 'usr_ann', author_name: item.mine ? 'me' : 'ann', body: item.id, created_at: new Date(Date.parse('2026-01-10T11:00:00Z') + i * 1000).toISOString(), reply_to: item.reply || 'rm_a_m120', thread_root: 'rm_a_m120', thread_count: fixture.length }));
        messages.find(message => message.id === 'rm_a_m120').thread_count = fixture.length;
      }, fixture);
      await openRoom(page, 'rm_a'); await opened(page); await settle(page);
      await chip(page, 'rm_a_m120');
      await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length > 1); await settle(page);
      await page.evaluate(() => { window.__chat.threadDraft.value = 'answer'; return window.__chat.sendThread(); }); await settle(page);
      return page.evaluate(() => window.__posted.at(-1).reply_to);
    };
    const mine = id => ({ id, mine: true }), someone = id => ({ id, mine: false });
    // A reply of someone else, then 130 of the viewer's own: the panel holds the newest 100 (and the first message), so the answer is further back than the panel, in the page before it.
    let page = await mount();
    assert.equal(await send(page, [someone('older'), ...Array.from({ length: 130 }, (_, i) => mine(`own${i}`))]), 'rm_a_older', 'a send answers the last reply of someone else, beyond the loaded window');
    await page.close();
    // The same, but the other person's reply is further back than one server page of 50 beyond the window.
    page = await mount();
    assert.equal(await send(page, [someone('farback'), ...Array.from({ length: 190 }, (_, i) => mine(`own${i}`))]), 'rm_a_farback', 'and beyond more than one page');
    await page.close();
    // Only the viewer's own replies: the first message.
    page = await mount();
    assert.equal(await send(page, Array.from({ length: 120 }, (_, i) => mine(`own${i}`))), 'rm_a_m120', 'with only their own replies a send answers the first message');
    await page.close();
  }
  // The thread panel shows the conversation like `tree`: the rows depth first with the connectors in a gutter. A first message m120, a chain c1..c8 under it (eight levels: the
  // cap is six), a fork from c2 (f1, the viewer's own) and a second reply to the first message (f2).
  if (ROWS) {
    const page = await mount();
    await page.evaluate(() => {
      const messages = window.__data.rooms.rm_a.messages, start = messages.length;
      const chain = ['c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7', 'c8'];
      const base = chain.map((name, i) => [name, i ? chain[i - 1] : 'm120', 'usr_ann']), spec = [...base.slice(0, 4), ['f1', 'c2', 'usr_me'], ...base.slice(4), ['f2', 'm120', 'usr_ann']];
      spec.forEach(([name, parent, author], i) => messages.push({ id: `rm_a_${name}`, seq: start + 1 + i, no: start + 1 + i, room_id: 'rm_a', author_kind: 'user', author_id: author, author_name: author === 'usr_me' ? 'me' : 'ann', body: `tree reply ${name}`, created_at: new Date(Date.parse('2026-01-10T11:00:00Z') + i * 1000).toISOString(), reply_to: parent === 'm120' ? 'rm_a_m120' : `rm_a_${parent}`, thread_root: 'rm_a_m120', thread_count: spec.length }));
      messages.find(message => message.id === 'rm_a_m120').thread_count = spec.length;
    });
    await openRoom(page, 'rm_a'); await opened(page); await settle(page);
    await chip(page, 'rm_a_m120');
    await page.waitForFunction(() => document.querySelectorAll('#thread-messages > article.thread-entry').length === 11); await settle(page);
    const ids = () => page.evaluate(() => [...document.querySelectorAll('#thread-messages > article.thread-entry')].map(row => row.dataset.messageId.replace('rm_a_', '')));
    // m120 / c1 / c2 / c3..c8 chain, then f1 (second reply to c2), then f2 (second reply to the first message): depth first.
    const TREE = ['m120', 'c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7', 'c8', 'f1', 'f2'];
    assert.deepEqual(await ids(), TREE, 'depth first: a message, its first reply\'s subtree, then its next reply (f1 was written before c5 but comes after the chain it forks from)');
    const read = () => page.evaluate(() => {
      const list = document.querySelector('#thread-messages'), box = node => node.getBoundingClientRect();
      return {
        step: getComputedStyle(document.documentElement).getPropertyValue('--tree-step').trim(), overflow: list.scrollWidth > list.clientWidth,
        rows: Object.fromEntries([...list.querySelectorAll(':scope > article.thread-entry')].map(row => {
          const style = getComputedStyle(row), gutter = row.querySelector(':scope > .tree-gutter'), cells = [...(gutter?.querySelectorAll(':scope > span') || [])], label = row.querySelector(':scope > .tree-label');
          const cell = name => cells.find(item => item.classList.contains(`tree-${name}`));
          return [row.dataset.messageId.replace('rm_a_', ''), {
            levels: row.dataset.tree, pad: style.paddingLeft, cells: cells.map(item => item.className.replace('tree-', '')), seen: gutter ? getComputedStyle(gutter).display !== 'none' : false,
            at: gutter ? Math.round(box(gutter).left - box(row).left) : null, connector: (cell('tee') || cell('elbow')) ? Math.round(box(cell('tee') || cell('elbow')).left - box(row).left) : null,
            label: label && getComputedStyle(label).display !== 'none' && box(label).width > 2 ? label.textContent : '', words: label ? label.textContent : '',
            lines: [...cells].map(item => item.className.replace('tree-', '') + ':' + [getComputedStyle(item, '::before').borderLeftStyle, getComputedStyle(item, '::after').borderTopStyle].join('/')),
            bubbleRight: Math.round(box(list).right - box(row.querySelector(':scope > .chat-msg-bubble')).right), own: row.classList.contains('self'),
          }];
        })),
      };
    });
    let tree = await read();
    // A level is 18px wide, and a row is that many levels in from the left, six at most.
    assert.equal(tree.step, '18px');
    assert.deepEqual(Object.values(tree.rows).map(row => row.levels), ['0', '1', '2', '3', '4', '5', '6', '6', '6', '3', '1'], 'a row is as many levels in as it is deep, six at most');
    // `tree` for this conversation (the cells of each row, ancestors first):
    //   m120
    //   \u251c\u2500\u2500 c1             c1: tee           (f2 follows)
    //   \u2502   \u2514\u2500\u2500 c2         c2: pipe elbow
    //   \u2502       \u251c\u2500\u2500 c3     c3: pipe blank tee (f1 follows)
    //   \u2502       \u2502   \u2514\u2500\u2500 c4 ...
    //   \u2502       \u2514\u2500\u2500 f1
    //   \u2514\u2500\u2500 f2
    assert.deepEqual(Object.fromEntries(Object.entries(tree.rows).map(([name, row]) => [name, row.cells.join(' ')])), { m120: '', c1: 'tee', c2: 'pipe elbow', c3: 'pipe blank tee', c4: 'pipe blank pipe elbow', c5: 'pipe blank pipe blank elbow', c6: 'pipe blank pipe blank blank elbow', c7: 'pipe blank pipe blank blank elbow', c8: 'pipe blank pipe blank blank elbow', f1: 'pipe blank elbow', f2: 'elbow' });
    assert.deepEqual([tree.rows.m120.cells.length, tree.rows.c1.seen], [0, true], 'the first message has nothing in its gutter; every other row has its lines');
    assert.equal(tree.rows.m120.pad, '0px');
    assert.deepEqual(['c1', 'c3', 'c5', 'c6'].map(name => tree.rows[name].pad), ['18px', '54px', '90px', '108px'], 'the row\'s own padding is its levels times the step');
    assert.deepEqual(['c6', 'c7', 'c8'].map(name => tree.rows[name].levels), ['6', '6', '6'], 'past six levels no further step');
    assert.deepEqual([tree.rows.c6.label, tree.rows.c7.label, tree.rows.c8.label, tree.rows.c5.label], ['', '\u21b3 to #137', '\u21b3 to #138', ''], 'a row too deep to show another step says which message it answers; the others have it for a screen reader only');
    assert.match(tree.rows.c5.words, /^\u21b3 to #\d+$/);
    assert.equal(tree.rows.c1.at, 0, 'the gutter starts at the row\'s left edge');
    assert.equal(tree.rows.f1.connector, tree.rows.c3.connector, 'a sibling\'s connector is in the same column, even for an own row');
    assert.ok(tree.rows.f1.own && Math.abs(tree.rows.f1.bubbleRight - tree.rows.c3.bubbleRight) <= 1, `an own row's bubble ends where the others' do (right-aligned): ${tree.rows.f1.bubbleRight} vs ${tree.rows.c3.bubbleRight}`);
    assert.deepEqual(tree.rows.c3.lines, ['pipe:solid/none', 'blank:none/none', 'tee:solid/solid'], 'a pipe is a vertical line, a tee a vertical line and an arm');
    assert.equal(tree.rows.f1.lines.at(-1), 'elbow:solid/solid');
    assert.equal(tree.overflow, false, 'the panel does not grow sideways');
    // A narrower window makes the step smaller.
    await page.setViewport({ width: 700, height: 800 });
    await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--tree-step').trim() === '11px');
    tree = await read();
    assert.deepEqual([tree.rows.c1.pad, tree.rows.c6.pad, tree.overflow], ['11px', '66px', false], 'a narrow window: smaller steps, still inside the panel');
    await page.setViewport({ width: 1280, height: 800 });
    // The live paths of the panel, in a window short enough that the panel scrolls.
    await page.setViewport({ width: 1280, height: 430 });
    await settle(page);
    const view = () => page.evaluate(() => {
      const list = document.querySelector('#thread-messages'), box = list.getBoundingClientRect(), pill = document.querySelector('.thread-tree-new');
      const visible = id => { const row = list.querySelector(`:scope > [data-message-id="rm_a_${id}"]`); if (!row) return null; const at = row.getBoundingClientRect(); return at.top >= box.top - 1 && at.bottom <= box.bottom + 1; };
      return { scrolls: list.scrollHeight > list.clientHeight + 40, pill: !pill.hidden, label: pill.getAttribute('aria-label'), dir: pill.dataset.dir, count: pill.querySelector('.badge')?.textContent || '', visible: Object.fromEntries(['x1', 'x2', 'f2', 'c1'].map(id => [id, visible(id)])), flashed: [...list.querySelectorAll('.message-flash')].map(row => row.dataset.messageId.replace('rm_a_', '')), ids: [...list.querySelectorAll(':scope > article.thread-entry')].map(row => row.dataset.messageId.replace('rm_a_', '')) };
    });
    const live = (name, parent, author = 'usr_ann') => page.evaluate(async ({ name, parent, author }) => {
      const messages = window.__data.rooms.rm_a.messages, seq = Math.max(...messages.map(message => message.seq)) + 1;
      messages.push({ id: `rm_a_${name}`, seq, no: seq, room_id: 'rm_a', author_kind: 'user', author_id: author, author_name: author === 'usr_me' ? 'me' : 'ann', body: `live ${name}`, created_at: new Date(Date.parse('2026-01-10T12:00:00Z') + seq * 1000).toISOString(), reply_to: `rm_a_${parent}`, thread_root: 'rm_a_m120', thread_count: 20 });
      await window.__chat.refreshMessages();
    }, { name, parent, author });
    const toEnd = () => page.evaluate(async () => { const list = document.querySelector('#thread-messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = list.scrollHeight; await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))); });
    // A tail under f2 (three replies at the end), so a reply in the middle is well above what the reader at the bottom sees.
    await live('t1', 'f2'); await live('t2', 't1'); await live('t3', 't2'); await settle(page);
    await toEnd();
    assert.equal((await view()).scrolls, true, 'the panel scrolls in this window');
    // 1. A reply to c5 (under it, after c6's whole chain: in the middle of the tree, above f1 and f2) while the reader is at the bottom: it is marked, and the pill leads to it.
    await live('x1', 'c5'); await settle(page);
    let state = await view();
    assert.deepEqual(state.ids.slice(-6), ['x1', 'f1', 'f2', 't1', 't2', 't3'], 'the new reply is under its parent, not at the end');
    assert.deepEqual([state.pill, state.dir, state.count, state.visible.x1, state.label], [true, 'up', '1', false, '1 new reply in the conversation above'], `a reply in the middle of the tree is marked for a reader at the bottom: ${JSON.stringify(state)}`);
    await page.evaluate(() => document.querySelector('.thread-tree-new').click()); await settle(page);
    state = await view();
    assert.deepEqual([state.visible.x1, state.flashed, state.pill], [true, ['x1'], false], `the pill takes the reader to it, highlighted, and goes: ${JSON.stringify(state)}`);
    // A reply at the end (to t3) is where a live reply always was: no marker.
    await toEnd(); await live('x2', 't3'); await settle(page);
    state = await view();
    assert.deepEqual([state.ids.at(-1), state.pill], ['x2', false], 'a reply at the end needs no marker');
    // 2. The viewer's own send is shown where it lands: the panel goes to it and highlights it, not to the end. The reply answers the newest of someone else's (x3, a reply to c1: it lands in the middle, above f2).
    await live('x3', 'c1'); await settle(page);
    await page.evaluate(() => { const list = document.querySelector('#thread-messages'); list.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); list.scrollTop = 0; });
    await page.evaluate(() => { window.__chat.threadDraft.value = 'my reply'; return window.__chat.sendThread(); }); await settle(page);
    state = await view();
    const mineAt = await page.evaluate(() => { const list = document.querySelector('#thread-messages'), box = list.getBoundingClientRect(), rows = [...list.querySelectorAll(':scope > article.thread-entry')]; const row = rows.find(item => item.querySelector('.chat-msg-body')?.textContent === 'my reply'); const at = row?.getBoundingClientRect(); return { found: !!row, visible: !!at && at.top >= box.top - 1 && at.bottom <= box.bottom + 1, flashed: !!row?.classList.contains('message-flash'), last: rows.at(-1) === row, before: row?.previousElementSibling?.querySelector('.chat-msg-body')?.textContent }; });
    assert.deepEqual([mineAt.found, mineAt.visible, mineAt.flashed], [true, true, true], `a send shows the sent row, highlighted: ${JSON.stringify(mineAt)}`);
    await page.close();
  }
  assert.deepEqual(errors, []);
  await browser.close(); server.close();
  console.log('chat page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
