// The Inbox view (its cards by kind, answering, deciding, dismissing, the count and the live refresh) in Chromium, over a source whose
// calls are held until the test lets them go. web/ is served as is with its import map; no Go server. It runs unchanged against the page
// that replaced the hand-written view. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const INBOX = fs.existsSync(path.join(root, 'js', 'pages', 'inbox', 'page.js')) ? '/js/pages/inbox/page.js' : '/js/inbox.js';
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><main id="app-view" class="app"><header class="workspace-bar"><button id="inbox-toggle" class="icon-btn" type="button" title="Inbox" aria-label="Inbox"><span id="inbox-count" class="badge" hidden>0</span></button></header>
<aside class="sidebar"></aside><section class="chat"></section>
<section id="inbox-view" class="inbox-view" hidden><header><div><h1>Inbox</h1><p>Questions, approvals, and agent recovery</p></div></header><div id="inbox-list" class="inbox-list"></div><p id="inbox-error" class="error" role="alert"></p></section></main></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const base = { project_id: 'p1', room_id: 'r1', rev: 1, state: 'open', can_decide: true };
const items = () => [
  { ...base, id: 'q1', kind: 'question', question: 'Which option should the team use?', options: ['Option A', 'Option B'], free_text: true },
  { ...base, id: 'ap1', kind: 'approval', question: 'Approve the proposed change?', proposed_answers: ['Approve', 'Revise'] },
  { ...base, id: 'ro1', kind: 'question', question: 'Only the owner answers this', options: ['Yes'], can_decide: false },
  { ...base, id: 'rec1', kind: 'recovery', turn_id: 't1', question: 'Agent needs recovery', evidence: 'Work may have partially completed.', diagnostic_report: { status: 'clean', details: { checked: true, files: 3 } } },
  { ...base, id: 'rec2', kind: 'recovery', recovery_phase: 'quiescence_wait', question: 'Waiting for the old process to be confirmed gone', evidence: 'Still running.' },
  { ...base, id: 'rec3', kind: 'recovery', question: 'Recovery nobody here can decide', can_decide: false },
  { ...base, id: 'del1', kind: 'delivery_failed', question: 'An agent reply could not be delivered.', held_reason: 'room_closed', room_lifecycle: 'closed', candidate_preview: '{"messages":[{"body":"Hello there"}]}' },
  { ...base, id: 'bnd1', kind: 'approval', boundary_pending: true, boundary_machine: 'Remote', boundary_path: '/work', boundary_requester: 'lead', question: 'Approve agent on remote machine?' },
  { ...base, id: 'bnd2', kind: 'approval', boundary_pending: true, boundary_machine: 'Remote', boundary_path: '/work', boundary_requester: 'lead', question: 'Another request', can_decide: false },
  { id: 'ua1', project_id: '', room_id: '', kind: 'usage_alert', question: 'Codex session: 19.0% remaining', rev: 0, state: 'open' },
  { ...base, id: 'ul1', kind: 'usage_info', question: 'Codex reached its provider limit.', options: ['Retry now'] },
  { id: 'q2', project_id: 'p2', room_id: 'r2', kind: 'question', question: 'In another room', options: ['Fine'], rev: 1, state: 'open', can_decide: true },
];

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  // `held` names the source calls that wait for window.__release(name) (the oldest first); the list answers from window.__items.
  const mount = async ({ list = items(), held = [], refresh = true, owner = true, usages = true } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async ({ list, held, refresh, owner, usages, module }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner, admin: owner });
      const { Inbox } = await import(module);
      window.__items = list; window.__calls = []; window.__waiting = {}; window.__listeners = new Set(); window.__arrived = []; window.__jumps = []; window.__onItems = [];
      const wrap = (name, run) => (...args) => {
        window.__calls.push([name, ...args]);
        if (!held.includes(name)) return Promise.resolve(run(...args));
        return new Promise((resolve, reject) => { (window.__waiting[name] ||= []).push(() => Promise.resolve().then(() => run(...args)).then(resolve, reject)); });
      };
      window.__fail = {};
      const source = {
        list: wrap('list', async () => structuredClone(window.__items)),
        answer: wrap('answer', async (id, value) => { if (window.__fail.answer) throw Object.assign(new Error(window.__fail.answer), { code: window.__fail.code }); window.__items = window.__items.filter(item => item.id !== id); return {}; }),
        decision: wrap('decision', async (id) => { if (window.__fail.decision) throw Object.assign(new Error(window.__fail.decision), { code: window.__fail.code }); window.__items = window.__items.filter(item => item.id !== id); return {}; }),
        dismiss: wrap('dismiss', async (id) => { if (window.__fail.dismiss) throw new Error(window.__fail.dismiss); window.__items = window.__items.filter(item => item.id !== id); return {}; }),
        subscribe: listener => { window.__listeners.add(listener); return () => window.__listeners.delete(listener); },
      };
      window.__release = name => (window.__waiting[name] || []).shift()?.();
      const projects = new Map([['p1', { name: 'Alpha' }], ['p2', { name: 'Beta' }]]);
      const rooms = new Map([['r1', { id: 'r1', title: 'Fix login' }], ['r2', { id: 'r2', title: 'Docs' }]]);
      const notifications = { arrived: (arrived, p, r) => window.__arrived.push(arrived.map(item => item.id)) };
      window.__inbox = new Inbox(source, projects, rooms, item => window.__jumps.push(item.id), notifications, list => window.__onItems.push(list.map(item => item.id)));
      if (usages) window.__inbox.openUsages = () => window.__calls.push(['usages']);
      if (refresh) window.__first = window.__inbox.refresh({ notify: false });
    }, { list, held, refresh, owner, usages, module: INBOX });
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const cardCount = page => page.$$eval('.inbox-card', cards => cards.length);
  const calls = (page, name) => page.evaluate(name => window.__calls.filter(call => call[0] === name), name);
  const summary = page => page.$$eval('.inbox-card', cards => cards.map(card => ({ id: card.dataset.inboxId, kind: card.querySelector('small').textContent, kindClass: card.querySelector('small').className, question: card.querySelector('h3').textContent,
    buttons: [...card.querySelectorAll('button')].filter(b => !b.classList.contains('inbox-raw-toggle')).map(b => b.getAttribute('aria-label') || b.textContent), evidence: [...card.querySelectorAll('.inbox-evidence')].map(p => p.textContent),
    extra: [card.querySelector('.inbox-retry-warning')?.textContent ?? null, card.querySelector('details summary')?.textContent ?? null, card.querySelector('details pre')?.textContent ?? null, card.querySelector('.inbox-archived')?.textContent ?? null, /Read only/.test(card.textContent), !!card.querySelector('form.inbox-answer'), !!card.querySelector('.answer-controls')] })));
  const waitList = page => page.waitForFunction(() => document.querySelectorAll('.inbox-card').length > 0);
  try {
    // Nothing waiting: one calm line, a hidden count, and the toggle and the tab title say so; showing and hiding the view moves the chat aside.
    {
      const page = await mount({ list: [] });
      await page.evaluate(() => window.__first);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.inbox-empty').textContent, document.querySelector('#inbox-count').hidden, document.querySelector('#inbox-count').textContent, document.querySelector('#inbox-toggle').title, document.querySelector('#inbox-toggle').getAttribute('aria-label'), document.title, window.__onItems]),
        ['All caught up', true, '0', 'Inbox', 'Inbox', 'aimebu', [[]]]);
      await page.evaluate(() => window.__inbox.show());
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-view').hidden, document.querySelector('.chat').hidden, document.querySelector('#app-view').classList.contains('inbox-open')]), [false, true, true]);
      await page.evaluate(() => window.__inbox.hide());
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-view').hidden, document.querySelector('.chat').hidden, document.querySelector('.sidebar').hidden, document.querySelector('#app-view').classList.contains('inbox-open')]), [true, false, false, false]);
      await page.close();
    }
    // Every kind of item, grouped by project and room, with its words, its decisions and what it says to someone who may not decide it.
    {
      const page = await mount();
      await waitList(page);
      assert.deepEqual(await page.$$eval('.inbox-group', groups => groups.map(group => [group.querySelector('h2').textContent, group.querySelectorAll('.inbox-card').length])), [['Alpha / Fix login', 10], ['Usages', 1], ['Beta / Docs', 1]]);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-count').textContent, document.querySelector('#inbox-count').hidden, document.querySelector('#inbox-toggle').title, document.title]), ['12', false, 'Inbox, 12 waiting', '(12) aimebu']);
      const cards = await summary(page);
      const by = id => cards.find(card => card.id === id);
      assert.deepEqual([by('q1').kind, by('q1').buttons, by('q1').extra[5]], ['Question', ['View in room', 'Option A', 'Option B', 'Send answer'], true]);
      assert.deepEqual([by('ap1').kind, by('ap1').buttons, by('ap1').extra[6]], ['Approval', ['View in room', 'Approve', 'Revise'], true]);
      assert.deepEqual([by('ro1').buttons, by('ro1').extra[4]], [['View in room'], true]);
      assert.deepEqual([by('rec1').kind, by('rec1').buttons, by('rec1').evidence, by('rec1').extra.slice(0, 3)], ['Recovery', ['View in room', 'Resume', 'Retry', 'Abandon'], ['Work may have partially completed.'],
        ['Resume may repeat an action if the diagnostic missed an effect. Retry repeats the task.', 'Diagnostic report', 'status: clean\nchecked: Yes\nfiles: 3']]);
      assert.deepEqual([by('rec2').buttons, by('rec2').extra[0]], [['View in room', 'Override', 'Abandon'], 'The old process may still be running. Override can overlap it and repeat an action.']);
      assert.deepEqual([by('rec3').buttons, by('rec3').evidence], [['View in room'], ['No diagnostic report is available yet.', 'A project owner or operator must decide this recovery item.']]);
      assert.deepEqual([by('del1').kind, by('del1').kindClass, by('del1').buttons, by('del1').evidence, by('del1').extra[3]], ['Delivery failed', 'inbox-kind-error', ['View in room', 'Dismiss'], ['The room was archived before this reply could be posted.', 'Hello there'], 'In archived room']);
      assert.deepEqual([by('bnd1').buttons, by('bnd1').evidence], [['Approve', 'Deny'], ['Requested by lead · Machine Remote · Folder /work']]);
      assert.deepEqual([by('bnd2').buttons, by('bnd2').evidence], [[], ['Requested by lead · Machine Remote · Folder /work', 'The project owner must decide this request.']]);
      assert.deepEqual([by('ua1').kind, by('ua1').buttons], ['Usage alert', ['Open Usages', 'Dismiss usage alert']]);
      assert.deepEqual([by('ul1').kind, by('ul1').buttons], ['Usage notice', ['View in room', 'Retry now']]);
      assert.deepEqual([by('q2').buttons], [['View in room', 'Fine']]);
      assert.deepEqual(await page.evaluate(() => window.__onItems.length), 1);
      await page.close();
      const guest = await mount({ owner: false, usages: false });
      await waitList(guest);
      assert.deepEqual((await summary(guest)).find(card => card.id === 'ua1').buttons, ['Dismiss usage alert'], 'only the owner is offered the Usages page');
      await guest.close();
    }
    // The raw record is behind a switch; "View in room" jumps; "Open Usages" opens it.
    {
      const page = await mount();
      await waitList(page);
      assert.deepEqual(await page.$eval('[data-inbox-id=q1] .inbox-raw-toggle', el => [el.getAttribute('aria-expanded'), el.title, document.querySelector('[data-inbox-id=q1] .inbox-raw').hidden]), ['false', 'Open debug inspector', true]);
      await page.$eval('[data-inbox-id=q1] .inbox-raw-toggle', el => el.click());
      assert.deepEqual(await page.$eval('[data-inbox-id=q1] .inbox-raw-toggle', el => [el.getAttribute('aria-expanded'), JSON.parse(document.querySelector('[data-inbox-id=q1] .inbox-raw').textContent).id, document.querySelector('[data-inbox-id=q1] .inbox-raw').hidden]), ['true', 'q1', false]);
      await page.$eval('[data-inbox-id=q1] .inbox-jump', el => el.click());
      await page.$eval('[data-inbox-id=ua1] .btn-primary', el => el.click());
      assert.deepEqual(await page.evaluate(() => [window.__jumps, window.__calls.filter(call => call[0] === 'usages').length]), [['q1'], 1]);
      await page.close();
    }
    // A new list keeps the cards that did not change (the same nodes, what was typed in them), replaces a changed one, drops a gone one and
    // its group, and tells the notifier only about what is new.
    {
      const page = await mount();
      await waitList(page);
      await page.evaluate(() => { document.querySelector('[data-inbox-id=q1] .inbox-answer textarea').value = 'half an answer'; window.__marked = [...document.querySelectorAll('.inbox-card')]; window.__marked.forEach(card => { card.__marked = true; }); });
      await page.evaluate(() => { window.__items = window.__items.filter(item => item.id !== 'q2').map(item => (item.id === 'rec1' ? { ...item, rev: 2 } : item)); window.__items.push({ project_id: 'p1', room_id: 'r1', id: 'new1', kind: 'question', question: 'Brand new', options: ['Ok'], rev: 1, state: 'open', can_decide: true }); return window.__inbox.refresh(); });
      assert.deepEqual(await page.evaluate(() => ({ kept: ['q1', 'ap1', 'ro1'].every(id => document.querySelector(`[data-inbox-id=${id}]`).__marked), typed: document.querySelector('[data-inbox-id=q1] .inbox-answer textarea').value, replaced: !document.querySelector('[data-inbox-id=rec1]').__marked,
        gone: !document.querySelector('[data-inbox-id=q2]'), groups: [...document.querySelectorAll('.inbox-group h2')].map(h => h.textContent), arrived: window.__arrived, count: document.querySelector('#inbox-count').textContent })),
        { kept: true, typed: 'half an answer', replaced: true, gone: true, groups: ['Alpha / Fix login', 'Usages'], arrived: [['new1']], count: '12' });
      await page.close();
    }
    // Two reads answered out of order: the newer list is the one drawn.
    {
      const page = await mount({ held: ['list'], refresh: false });
      await page.evaluate(() => { window.__inbox.refresh(); window.__inbox.refresh(); });
      await page.waitForFunction(() => window.__waiting.list?.length === 2);
      await page.evaluate(() => { window.__all = window.__items; window.__items = window.__all.slice(0, 1); window.__release('list'); });
      await page.evaluate(() => { window.__items = window.__all.slice(0, 3); window.__release('list'); });
      await settle(page);
      assert.equal(await cardCount(page), 3);
      await page.close();
    }
    // A change the source announces reads the list again.
    {
      const page = await mount({ list: [items()[0]] });
      await waitList(page);
      await page.evaluate(() => { window.__items = [...window.__items, { ...window.__items[0], id: 'qx', question: 'Pushed' }]; window.__listeners.forEach(listener => listener()); });
      await page.waitForFunction(() => document.querySelectorAll('.inbox-card').length === 2);
      await page.close();
    }
    // Answering: the card's controls go off while it waits, one idempotency key serves a retry of the same answer, a refusal says why and puts them back.
    {
      const page = await mount({ held: ['answer'] });
      await waitList(page);
      await page.$eval('[data-inbox-id=q2] .inbox-choices button, [data-inbox-id=q2] .btn', el => el.click());
      await page.waitForFunction(() => window.__waiting.answer?.length === 1);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('[data-inbox-id=q2]').dataset.pending, [...document.querySelectorAll('[data-inbox-id=q2] button')].every(b => b.disabled), window.__calls.find(call => call[0] === 'answer').slice(0, 3)]), ['true', true, ['answer', 'q2', 'Fine']]);
      assert.equal(await page.evaluate(() => typeof window.__calls.find(call => call[0] === 'answer')[3]), 'string');
      await page.$eval('[data-inbox-id=q2] .btn:not([disabled])', el => el.click()).catch(() => {});
      assert.equal((await calls(page, 'answer')).length, 1, 'a card that is waiting is not answered twice');
      await page.evaluate(() => { window.__fail.answer = 'The server said no'; window.__release('answer'); });
      await page.waitForFunction(() => document.querySelector('#inbox-error').textContent === 'The server said no');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('[data-inbox-id=q2]').dataset.pending ?? null, [...document.querySelectorAll('[data-inbox-id=q2] button')].filter(b => !b.classList.contains('inbox-raw-toggle') && !b.classList.contains('inbox-jump')).every(b => !b.disabled)]), [null, true]);
      const firstKey = await page.evaluate(() => window.__calls.find(call => call[0] === 'answer')[3]);
      await page.evaluate(() => { window.__fail.answer = ''; });
      await page.$eval('[data-inbox-id=q2] .btn', el => el.click());
      await page.waitForFunction(() => window.__calls.filter(call => call[0] === 'answer').length === 2);
      assert.equal(await page.evaluate(() => window.__calls.filter(call => call[0] === 'answer')[1][3]), firstKey, 'the same answer retried keeps its key');
      await page.evaluate(() => window.__release('answer'));
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=q2]'));
      assert.equal(await page.$eval('#inbox-error', el => el.textContent), '');
      await page.close();
      const typed = await mount();
      await waitList(typed);
      await typed.$eval('[data-inbox-id=q1] .inbox-answer textarea', el => { el.value = '  my own answer  '; });
      await typed.$eval('[data-inbox-id=q1] .inbox-answer', el => el.requestSubmit());
      await typed.waitForFunction(() => window.__calls.some(call => call[0] === 'answer'));
      assert.deepEqual(await typed.evaluate(() => window.__calls.find(call => call[0] === 'answer').slice(1, 3)), ['q1', 'my own answer']);
      await typed.$eval('[data-inbox-id=ap1] .proposed-answer-btn', el => el.click());
      await typed.waitForFunction(() => window.__calls.filter(call => call[0] === 'answer').length === 2);
      assert.deepEqual(await typed.evaluate(() => window.__calls.filter(call => call[0] === 'answer')[1].slice(1, 3)), ['ap1', 'Approve']);
      await typed.close();
    }
    // Deciding: the choice goes with the item's revision; a stale item says so and is read again; other refusals show their message.
    {
      const page = await mount();
      await waitList(page);
      await page.evaluate(() => { window.__fail.decision = 'stale'; window.__fail.code = 'stale_rev'; });
      await page.$eval('[data-inbox-id=rec1] .decisions, [data-inbox-id=rec1] .inbox-decisions button', el => el.click());
      await page.waitForFunction(() => document.querySelector('#inbox-error').textContent === 'This item changed, reload item.');
      await page.waitForFunction(() => window.__calls.filter(call => call[0] === 'list').length === 2);
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'decision').slice(1, 3).concat([window.__calls.find(call => call[0] === 'decision')[4]])), ['rec1', 'resume', 1]);
      await page.evaluate(() => { window.__fail.decision = 'Not allowed'; window.__fail.code = ''; });
      await page.$eval('[data-inbox-id=rec2] .btn-danger', el => el.click());
      await page.waitForFunction(() => document.querySelector('#inbox-error').textContent === 'Not allowed');
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('[data-inbox-id=rec2] .inbox-decisions button')].map(b => b.disabled)), [false, false]);
      await page.evaluate(() => { window.__fail.decision = ''; });
      await page.$eval('[data-inbox-id=bnd1] .btn', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=bnd1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['bnd1', 'approve']);
      await page.$eval('[data-inbox-id=ul1] .btn-primary', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=ul1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['ul1', 'retry']);
      await page.close();
    }
    // Dismissing one, and every resolved delivery failure of an archived room at once.
    {
      const failures = [{ ...base, id: 'd1', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'First', held_reason: 'room_closed' }, { ...base, id: 'd2', kind: 'delivery_failed', room_lifecycle: 'closing', question: 'Second', held_reason: 'room_closed' }, { ...base, id: 'd3', kind: 'delivery_failed', room_lifecycle: 'open', question: 'Third', held_reason: 'x' }];
      const page = await mount({ list: [...failures, items()[11]] });
      await waitList(page);
      assert.deepEqual(await page.evaluate(() => { const b = document.querySelector('.inbox-dismiss-resolved'); return [b.hidden, b.title, b.querySelector('.inbox-dismiss-count').textContent, b.getAttribute('aria-label')]; }), [false, 'Dismiss all resolved (2): the delivery failures of archived rooms', '2', 'Dismiss all resolved (2)']);
      await page.$eval('[data-inbox-id=d3] .icon-btn:not(.inbox-raw-toggle):not(.inbox-jump)', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=d3]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'dismiss').slice(1, 3)), ['d3', 1]);
      await page.$eval('.inbox-dismiss-resolved', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=d1]') && !document.querySelector('[data-inbox-id=d2]'));
      assert.deepEqual(await page.evaluate(() => [window.__calls.filter(call => call[0] === 'dismiss').map(call => call[1]), document.querySelector('.inbox-dismiss-resolved').hidden]), [['d3', 'd1', 'd2'], true]);
      await page.close();
      const refused = await mount({ list: [failures[0], failures[1]] });
      await waitList(refused);
      await refused.evaluate(() => { window.__fail.dismiss = 'Cannot dismiss'; });
      await refused.$eval('.inbox-dismiss-resolved', el => el.click());
      await refused.waitForFunction(() => document.querySelector('#inbox-error').textContent === 'Cannot dismiss');
      assert.equal(await refused.$$eval('.inbox-card', cards => cards.length), 2);
      await refused.close();
    }
    // Resetting (sign-out) clears the list, the count and the title.
    {
      const page = await mount();
      await waitList(page);
      await page.evaluate(() => { window.__inbox.setError('Something'); window.__inbox.reset(); });
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('.inbox-card').length, document.querySelector('#inbox-count').hidden, document.querySelector('#inbox-count').textContent, document.querySelector('#inbox-error').textContent, document.querySelector('#inbox-view').hidden, window.__inbox.items === undefined]), [0, true, '0', '', true, true]);
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('inbox page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });
