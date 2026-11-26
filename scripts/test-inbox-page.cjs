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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><main id="app-view" class="app"><header class="topbar"><button id="inbox-toggle" class="btn-icon" type="button" title="Inbox" aria-label="Inbox"><span id="inbox-count" class="badge" hidden>0</span></button></header>
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
  { ...base, id: 'rec1', kind: 'recovery', turn_id: 't1', question: 'Agent needs recovery', evidence: 'Work may have partially completed.', withdraw_notes: 3, withdraw_rooms: 2, diagnostic_report: { status: 'clean', details: { checked: true, files: 3 } } },
  { ...base, id: 'rec2', kind: 'recovery', recovery_phase: 'quiescence_wait', question: 'Waiting for the old process to be confirmed gone', evidence: 'Still running.' },
  { ...base, id: 'rec4', kind: 'recovery', recovery_phase: 'broken_session', turn_id: 't4', question: 'The agent answered twice in a row with an empty answer' },
  { ...base, id: 'del2', kind: 'delivery_failed', recovery_phase: 'answer_held', held_reason: 'correction_unavailable', question: 'An agent reply could not be delivered.' },
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
      window.__notices = [];
      const notifications = { arrived: (arrived, p, r) => window.__arrived.push(arrived.map(item => item.id)), notice: item => window.__notices.push(item.id) };
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
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-view').hidden, document.querySelector('.chat').hidden, document.querySelector('#app-view').classList.contains('inbox-open'), document.querySelector('#inbox-toggle').getAttribute('aria-pressed')]), [false, true, true, 'true'], 'the Inbox button is the marked one while the Inbox is open');
      await page.evaluate(() => window.__inbox.hide());
      assert.equal(await page.evaluate(() => document.querySelector('#inbox-toggle').getAttribute('aria-pressed')), 'false', 'and not once it is closed');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-view').hidden, document.querySelector('.chat').hidden, document.querySelector('.sidebar').hidden, document.querySelector('#app-view').classList.contains('inbox-open')]), [true, false, false, false]);
      await page.close();
    }
    // An open attention item outside the home room (inbox_hidden) is not listed or counted either, but the chat gets it and keeps its controls.
    {
      const page = await mount({ list: [...items(), { ...base, id: 'hid1', kind: 'question', question: 'Asked in a task room', options: ['A', 'B'], state: 'open', inbox_hidden: true }] });
      await waitList(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('.inbox-card').length, document.querySelector('#inbox-count').textContent, window.__onItems.at(-1).includes('hid1'), !!document.querySelector('[data-inbox-id=hid1]')]),
        [items().length, String(items().length), true, false], 'a hidden attention item is not in the Inbox but is handed to the chat');
      await page.close();
    }
    // A superseded ask is out of the Inbox (no card, not counted) but still reaches the chat, which answers it there.
    {
      const page = await mount({ list: [...items(), { ...base, id: 'sup1', kind: 'question', question: 'A question a newer message overtook', options: ['A', 'B'], state: 'superseded' }] });
      await waitList(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('.inbox-card').length, document.querySelector('#inbox-count').textContent, document.querySelector('#inbox-count').hidden, window.__onItems.at(-1).includes('sup1'), !!document.querySelector('[data-inbox-id=sup1]')]),
        [items().length, String(items().length), false, true, false], 'a superseded ask is not in the Inbox but is handed to the chat');
      await page.close();
    }
    // A withdrawn question with structured choices (dismissed from the Inbox) stays answerable in the chat; the Inbox list, its count and its badge do not grow.
    {
      const page = await mount({ list: [...items(), { ...base, id: 'wd1', kind: 'question', question: 'Pick', proposed_answers: ['A', 'B'], options: [], state: 'withdrawn' }] });
      await waitList(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('.inbox-card').length, document.querySelector('#inbox-count').textContent, window.__onItems.at(-1).includes('wd1'), !!document.querySelector('[data-inbox-id=wd1]')]),
        [items().length, String(items().length), true, false], 'a withdrawn structured card is handed to the chat and is not in the Inbox list or its count');
      await page.close();
    }
    // A recovery card of a turn that failed before it did anything shows the sentence the harness gave, the raw output folded away behind a
    // toggle, Retry and Abandon only (nothing to resume), and a repeated failure says so in its question.
    {
      const sentence = 'Failed to authenticate: OAuth session expired · Please run /login';
      const raw = 'Process exited with code 1.\nClaude Code error result: {"type":"result","is_error":true,"result":"Failed to authenticate"}';
      const failed = { ...base, id: 'def1', kind: 'recovery', turn_id: 't9', definite_failure: true, result_text: sentence, evidence: raw,
        question: 'Same failure again — fix the cause first; doing the same thing will fail the same way. The turn failed before it did anything: ' + sentence };
      const page = await mount({ list: [failed] });
      await waitList(page);
      const card = await page.$eval('[data-inbox-id=def1]', node => ({ question: node.querySelector('h3').textContent, buttons: [...node.querySelectorAll('.inbox-decisions button')].map(b => b.textContent),
        evidence: [...node.querySelectorAll('.inbox-evidence')].map(p => p.textContent), summary: node.querySelector('details.inbox-details summary')?.textContent, raw: node.querySelector('details.inbox-details pre')?.textContent,
        label: node.querySelector('details.inbox-details small')?.textContent, open: node.querySelector('details.inbox-details')?.open, warning: node.querySelector('.inbox-retry-warning')?.textContent }));
      assert.match(card.question, /^Same failure again — fix the cause first/);
      assert.deepEqual(card.buttons, ['Retry', 'Abandon', 'Abandon and stop']);
      assert.deepEqual(card.evidence, [sentence], 'the card reads the harness sentence, not the raw JSON');
      assert.deepEqual([card.summary, card.label, card.open], ['Details', 'Raw output', false], 'the raw output is folded away under Details');
      assert.equal(card.raw, raw);
      assert.match(card.warning, /^Retry is safe: the turn failed before it did anything/);
      await page.close();
    }
    // A recovery card whose harness gave no sentence of its own, only a wall of output: one line made from it comes first, the decision is next, and the
    // output (all of it) is folded under Details, so Resume, Retry and Abandon are on screen without scrolling past it.
    {
      const wall = ['Process exited with code 1.', 'SessionEnd hook [node "${CLAUDE_PLUGIN_ROOT}/scripts/x.mjs"] failed: /bin/sh: node: command not found',
        'Codex error event: {"is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed","type":"result","usage":{"input_tokens":0}}', 'x'.repeat(3000)].join('\n');
      const uncertain = { ...base, id: 'wall1', kind: 'recovery', turn_id: 't7', question: 'An agent turn has an uncertain outcome. Review the evidence before continuing.', evidence: wall };
      const page = await mount({ list: [uncertain] });
      await waitList(page);
      await page.setViewport({ width: 1280, height: 800 });
      await page.evaluate(() => window.__inbox.show()); // shown, so the layout is real
      const shown = await page.$eval('[data-inbox-id=wall1]', node => {
        const card = node.getBoundingClientRect(), details = node.querySelector('details.inbox-details'), summary = node.querySelector('.inbox-summary'), decisions = node.querySelector('.inbox-decisions');
        return { summary: summary.textContent, buttons: [...decisions.querySelectorAll('button')].map(b => b.textContent), open: details.open, label: details.querySelector('small').textContent, text: details.querySelector('pre').textContent,
          order: [summary, decisions, details].map(el => Math.round(el.getBoundingClientRect().top)), cardHeight: Math.round(card.height), decisionsBottom: Math.round(decisions.getBoundingClientRect().bottom - card.top) };
      });
      assert.equal(shown.summary, "The agent's process exited with code 1: Failed to authenticate: OAuth session expired and could not be refreshed");
      assert.deepEqual([shown.buttons, shown.open, shown.label], [['Resume', 'Retry', 'Abandon', 'Abandon and stop'], false, 'Output'], 'the decision is shown and the output is folded');
      assert.equal(shown.text, wall, 'and all of the output is behind Details');
      assert.deepEqual([...shown.order].sort((a, b) => a - b), shown.order, 'summary, then the decision, then Details');
      assert.ok(shown.decisionsBottom < 420 && shown.cardHeight < 520, `the decision is near the top of the card: its bottom edge is at ${shown.decisionsBottom}px of a card ${shown.cardHeight}px tall`);
      // Opening Details shows it.
      await page.click('[data-inbox-id=wall1] details.inbox-details summary');
      await page.waitForFunction(() => document.querySelector('[data-inbox-id=wall1] details.inbox-details')?.open === true, { timeout: 5000 });
      assert.equal(await page.$eval('[data-inbox-id=wall1] details.inbox-details pre', el => el.offsetParent !== null && el.getBoundingClientRect().height > 20), true);
      await page.close();
    }
    // A shared agent note waiting for approval: the words of the note, Approve and Decline; Approve decides "approve", Decline decides "deny".
    {
      const noteItems = [{ ...base, id: 'note1', kind: 'approval', note_id: 'knw_1', question: 'zoe wrote a note for the shared agent notes, which every project\'s agents would read once approved: “use tabs”', options: ['Approve', 'Decline'] },
        { ...base, id: 'note2', kind: 'approval', note_id: 'knw_2', question: 'Another note', options: ['Approve', 'Decline'], can_decide: false }];
      const page = await mount({ list: noteItems });
      await waitList(page);
      const cards = await summary(page);
      const note = id => cards.find(card => card.id === id);
      assert.deepEqual([note('note1').kind, note('note1').buttons], ['Approval', ['Approve', 'Decline']]);
      assert.match(await page.$eval('[data-inbox-id=note1] h3', el => el.textContent), /use tabs/);
      assert.deepEqual(note('note2').buttons, [], 'someone who may not decide it only reads it');
      assert.match(await page.$eval('[data-inbox-id=note2]', el => el.textContent), /An owner or admin must decide this note/);
      await page.$eval('[data-inbox-id=note1] .btn-danger', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=note1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['note1', 'deny']);
      await page.close();
    }
    // Every kind of item, grouped by project and room, with its words, its decisions and what it says to someone who may not decide it.
    {
      const page = await mount();
      await waitList(page);
      assert.deepEqual(await page.$$eval('.inbox-group', groups => groups.map(group => [group.querySelector('h2').textContent, group.querySelectorAll('.inbox-card').length])), [['Alpha / Fix login', 12], ['Usages', 1], ['Beta / Docs', 1]]);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#inbox-count').textContent, document.querySelector('#inbox-count').hidden, document.querySelector('#inbox-toggle').title, document.title]), ['14', false, 'Inbox, 14 waiting', '(14) aimebu']);
      const cards = await summary(page);
      const by = id => cards.find(card => card.id === id);
      assert.deepEqual([by('q1').kind, by('q1').buttons, by('q1').extra[5]], ['Question', ['View in room', 'Option A', 'Option B', 'Send answer', 'Dismiss'], true]);
      assert.deepEqual([by('ap1').kind, by('ap1').buttons, by('ap1').extra[6]], ['Approval', ['View in room', 'Approve', 'Revise', 'Dismiss'], true]);
      assert.deepEqual([by('ro1').buttons, by('ro1').extra[4]], [['View in room'], true]);
      assert.deepEqual([by('rec1').kind, by('rec1').buttons, by('rec1').evidence, by('rec1').extra.slice(0, 3)], ['Recovery', ['View in room', 'Resume', 'Retry', 'Abandon', 'Abandon and stop'], ['Work may have partially completed.'],
        ['Resume may repeat an action if the diagnostic missed an effect. Retry repeats the task.', 'Details', 'status: clean\nchecked: Yes\nfiles: 3']]); // the diagnostic report is the first labelled text under Details
      assert.deepEqual([by('rec2').buttons, by('rec2').extra[0]], [['View in room', 'Override', 'Abandon', 'Abandon and stop'], 'The old process may still be running. Override can overlap it and repeat an action.']);
      assert.deepEqual([by('rec4').buttons, by('del2').buttons], [['View in room', 'Restart', 'Abandon', 'Abandon and stop'], ['View in room', 'Retry', 'Abandon', 'Abandon and stop']], 'a broken session offers a fresh session, a held answer offers a retry');
      assert.deepEqual([by('rec3').buttons, by('rec3').evidence], [['View in room'], ['No diagnostic report is available yet.', 'A project owner or operator must decide this recovery item.']]);
      assert.deepEqual([by('del1').kind, by('del1').kindClass, by('del1').buttons, by('del1').evidence, by('del1').extra[3]], ['Delivery failed', 'inbox-kind-error', ['View in room', 'Dismiss'], ['The room was archived before this reply could be posted.', 'Hello there'], 'In archived room']);
      assert.deepEqual([by('bnd1').buttons, by('bnd1').evidence], [['Approve', 'Deny'], ['Requested by lead · Machine Remote · Folder /work']]);
      assert.deepEqual([by('bnd2').buttons, by('bnd2').evidence], [[], ['Requested by lead · Machine Remote · Folder /work', 'The project owner must decide this request.']]);
      assert.deepEqual([by('ua1').kind, by('ua1').buttons], ['Usage alert', ['Open Usages', 'Dismiss usage alert']]);
      assert.deepEqual([by('ul1').kind, by('ul1').buttons], ['Usage notice', ['View in room', 'Retry now']]);
      assert.deepEqual([by('q2').buttons], [['View in room', 'Fine', 'Dismiss']]);
      assert.deepEqual(await page.evaluate(() => window.__onItems.length), 1);
      await page.close();
      const guest = await mount({ owner: false, usages: false });
      await waitList(guest);
      assert.deepEqual((await summary(guest)).find(card => card.id === 'ua1').buttons, ['Dismiss usage alert'], 'only the owner is offered the Usages page');
      await guest.close();
    }
    // A seen request remains answerable until Dismiss closes it without an answer.
    {
      const page = await mount({ list: [{ ...base, id: 'seen1', kind: 'question', seen: true, question: 'Choose?', options: ['A'], can_decide: true }] });
      await waitList(page);
      assert.equal(await page.$eval('[data-inbox-id=seen1] .inbox-choices button', node => node.disabled), false);
      await page.$eval('[data-inbox-id=seen1] .inbox-dismiss', node => node.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=seen1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'dismiss' || call[0] === 'answer').map(call => call[0])), ['dismiss']);
      await page.close();
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
    // A usage alert card draws its window's chart (the Usages panel's own block): the bar, the numbers and, for a pace alert, the runs-out line. A card
    // whose provider has no data draws no chart and no error, and the chart follows the usage data when it changes.
    {
      const alert = (id, provider, profile, window) => ({ id, project_id: '', room_id: '', kind: 'usage_alert', question: `${provider} · ${profile} · ${window}: projected to run out; 98.0% remaining.`, provider, profile, window, rev: 0, state: 'open' });
      const page = await mount({ list: [alert('uc1', 'codex', 'main', 'session'), alert('uc2', 'mistral', 'local', 'local'), { ...alert('uc3', 'codex', 'main', 'weekly'), window: 'weekly' }] });
      await page.evaluate(() => {
        window.__usage = { settings: { percent_display: 'remaining', refresh_interval_sec: 300 }, providers: [{ provider_name: 'codex', profiles: [{ profile_name: 'main', last_refresh_at: new Date().toISOString(),
          windows: [{ key: 'session', percent_used: 40, reset_at: new Date(Date.now() + 3 * 3600e3).toISOString(), pace: { state: 'deficit', delta_percent: 20, expected_percent: 20, eta_seconds: 3600 } }] }] }] };
        window.__inbox.setUsageList(async () => structuredClone(window.__usage));
        return window.__inbox.refresh({ notify: false });
      });
      await page.waitForSelector('[data-inbox-id=uc1] .inbox-usage-chart .usages-progress');
      assert.deepEqual(await page.$eval('[data-inbox-id=uc1] .inbox-usage-chart', node => [node.querySelector('.usages-window-meta strong').textContent, /Runs out in/.test(node.querySelector('.usages-pace-text')?.textContent || '')]), ['60% left', true]);
      assert.deepEqual(await page.$$eval('[data-inbox-id=uc2] .inbox-usage-chart, [data-inbox-id=uc3] .inbox-usage-chart', nodes => nodes.length), 0, 'no data for the window: no chart');
      assert.deepEqual(await page.$$eval('.error:not(:empty)', nodes => nodes.length), 0, 'a missing chart is not an error');
      await page.evaluate(() => { window.__usage.providers[0].profiles[0].windows[0].percent_used = 70; return window.__inbox.usagesChanged(); });
      await page.waitForFunction(() => document.querySelector('[data-inbox-id=uc1] .usages-window-meta strong')?.textContent === '30% left');
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
        { kept: true, typed: 'half an answer', replaced: true, gone: true, groups: ['Alpha / Fix login', 'Usages'], arrived: [['new1']], count: '14' });
      await page.close();
    }
    // A usage reset notice is never an Inbox card nor counted: a notice already there on the first read stays quiet, a new one is shown once
    // (the notifier's toast), and a later read of the same notice does not repeat it.
    {
      const notice = id => ({ id, project_id: '', room_id: '', kind: 'usage_notice', question: 'Codex · work · session: the window reset; a fresh allowance is available.', rev: 0, state: 'notice' });
      const page = await mount({ list: [...items(), notice('un_old')] });
      await waitList(page);
      await page.evaluate(() => window.__first);
      const count = await page.$eval('#inbox-count', el => el.textContent);
      await page.evaluate(item => { window.__items.push(item); return window.__inbox.refresh(); }, notice('un_new'));
      await page.evaluate(() => window.__inbox.refresh());
      assert.deepEqual(await page.evaluate(() => ({ cards: ['un_old', 'un_new'].filter(id => document.querySelector(`[data-inbox-id=${id}]`)), notices: window.__notices, arrived: window.__arrived, count: document.querySelector('#inbox-count').textContent })),
        { cards: [], notices: ['un_new'], arrived: [[], []], count }, 'a notice is a toast, never an Inbox card or count');
      assert.equal(count, String(items().length), 'the count leaves the notice out');
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
    // Abandon and Abandon and stop ask first and say how many queued notes they withdraw and in how many rooms (the server's counts on the item);
    // cancelling decides nothing, and Abandon and stop sends its own choice and says the agent will be stopped.
    {
      const page = await mount();
      await waitList(page);
      const decisions = () => page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').map(call => call.slice(1, 3)));
      await page.$eval('[data-inbox-id=rec1] [data-choice=abandon_stop]', el => el.click());
      await page.waitForSelector('#app-dialog[open] .app-dialog-ok');
      assert.equal(await page.$eval('#app-dialog .app-dialog-ok', node => node.textContent), 'Abandon and stop');
      const stopText = await page.$eval('#app-dialog', node => node.textContent);
      assert.match(stopText, /withdraws 3 queued notes in 2 rooms/);
      assert.match(stopText, /The agent will be stopped/);
      await page.click('#app-dialog .app-dialog-cancel');
      assert.deepEqual(await decisions(), [], 'cancelling decides nothing');
      await page.$eval('[data-inbox-id=rec1] [data-choice=abandon]', el => el.click());
      await page.waitForSelector('#app-dialog[open] .app-dialog-ok');
      const abandonText = await page.$eval('#app-dialog', node => node.textContent);
      assert.match(abandonText, /withdraws 3 queued notes in 2 rooms/);
      assert.ok(!/will be stopped/.test(abandonText), 'plain Abandon does not say the agent is stopped');
      await page.click('#app-dialog .app-dialog-cancel');
      await page.$eval('[data-inbox-id=rec1] [data-choice=resume]', el => el.click());
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'decision'));
      assert.deepEqual(await decisions(), [['rec1', 'resume']], 'Resume asks nothing');
      await page.evaluate(() => { window.__items = window.__items.map(item => (item.id === 'rec2' ? { ...item, withdraw_notes: 1, withdraw_rooms: 1 } : item)); return window.__inbox.refresh(); });
      await page.$eval('[data-inbox-id=rec2] [data-choice=abandon_stop]', el => el.click());
      await page.waitForSelector('#app-dialog[open] .app-dialog-ok');
      await page.click('#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => window.__calls.filter(call => call[0] === 'decision').length === 2);
      assert.deepEqual(await decisions(), [['rec1', 'resume'], ['rec2', 'abandon_stop']]);
      await page.close();
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
      await page.waitForSelector('#app-dialog[open] .app-dialog-ok');
      await page.click('#app-dialog .app-dialog-ok'); // Abandon asks first
      await page.waitForFunction(() => document.querySelector('#inbox-error').textContent === 'Not allowed');
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('[data-inbox-id=rec2] .inbox-decisions button')].map(b => b.disabled)), [false, false, false]);
      await page.evaluate(() => { window.__fail.decision = ''; });
      await page.$eval('[data-inbox-id=bnd1] .btn', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=bnd1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['bnd1', 'approve']);
      await page.$eval('[data-inbox-id=ul1] .btn-primary', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=ul1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['ul1', 'retry']);
      await page.close();
    }
    // A pool change: one card with the room-grouped list of agents it would switch (each counted once under its own room, never an id), and the
    // two decisions, the respawn first and the default. Someone who cannot decide sees no buttons.
    {
      const pool = { ...base, id: 'pc1', kind: 'pool_change', question: "A role's default pool changed: 3 running agents use a harness or model the pool no longer lists.", options: ['Respawn on the pool model', 'Keep the current model'],
        pool_agents: [{ agent_id: 'agt_01', slug: 'lead', role_label: 'Leader', room_id: 'r1', room_title: 'Home', respawn_error: 'respawn failed: machine_unavailable' }, { agent_id: 'agt_02', role_label: 'Worker', room_id: 'r9', room_title: 'A1: task' }, { agent_id: 'agt_03', role_label: 'Worker', room_id: 'r9', room_title: 'A1: task' }] };
      const page = await mount({ list: [pool, { ...pool, id: 'pc2', can_decide: false, pool_agents: [pool.pool_agents[1]] }] });
      await waitList(page);
      const card = await page.$eval('[data-inbox-id=pc1]', node => ({ kind: node.querySelector('small').textContent, lines: [...node.querySelectorAll('.inbox-pool-list li')].map(li => li.textContent),
        buttons: [...node.querySelectorAll('.inbox-decisions button')].map(b => [b.textContent.trim(), b.dataset.choice, b.className, b.title, !!b.querySelector('.icon-mask')]) }));
      assert.equal(card.kind, 'Pool change');
      assert.deepEqual(card.lines, ['Home: leader', 'A1: task: 2 workers']);
      assert.ok(!(await page.$eval('[data-inbox-id=pc1]', node => { const copy = node.cloneNode(true); copy.querySelector('.inbox-raw-holder').remove(); return copy.textContent; })).includes('agt_'), 'no raw agent id is shown');
      assert.equal(await page.$eval('[data-inbox-id=pc1] .pool-failed', el => el.textContent), 'lead: respawn failed: machine_unavailable', 'a failed respawn is shown with its reason');
      assert.deepEqual(card.buttons, [['Respawn on the pool model', 'respawn', 'btn btn-primary', 'Respawn on the pool model', true], ['Keep the current model', 'keep', 'btn', 'Keep the current model', true]], 'the respawn is the default, first and primary');
      assert.deepEqual(await page.$eval('[data-inbox-id=pc2]', node => [node.querySelectorAll('.inbox-decisions button').length, node.querySelector('.inbox-pool-list li').textContent]), [0, 'A1: task: worker']);
      await page.$eval('[data-inbox-id=pc1] [data-choice=respawn]', el => el.click());
      await page.waitForFunction(() => !document.querySelector('[data-inbox-id=pc1]'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'decision').at(-1).slice(1, 3)), ['pc1', 'respawn']);
      await page.close();
    }
    // Dismissing one, and every resolved delivery failure of an archived room at once.
    {
      const failures = [{ ...base, id: 'd1', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'First', held_reason: 'room_closed' }, { ...base, id: 'd2', kind: 'delivery_failed', room_lifecycle: 'closing', question: 'Second', held_reason: 'room_closed' }, { ...base, id: 'd3', kind: 'delivery_failed', room_lifecycle: 'open', question: 'Third', held_reason: 'correction_unavailable', delivery_error: 'invalid_answer: message 0 needs at least two proposed answers' }];
      const page = await mount({ list: [...failures, items()[11]] });
      await waitList(page);
      assert.deepEqual(await page.evaluate(() => { const b = document.querySelector('.inbox-dismiss-resolved'); return [b.hidden, b.title, b.querySelector('.inbox-dismiss-count').textContent, b.getAttribute('aria-label')]; }), [false, 'Dismiss all resolved (2): the delivery failures of archived rooms', '2', 'Dismiss all resolved (2)']);
      assert.match(await page.$eval('[data-inbox-id=d3]', el => el.textContent), /Reason: invalid_answer: message 0 needs at least two proposed answers/, 'the card says why the reply was held');
      await page.$eval('[data-inbox-id=d3] .btn-icon:not(.inbox-raw-toggle):not(.inbox-jump)', el => el.click());
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
    // A view destroyed while a read or a write is held draws nothing and sends nothing more; it lets go of the source, and an event it was told of
    // after that reads nothing.
    {
      const read = await mount({ held: ['list'], refresh: false });
      await read.evaluate(() => { window.__listener = [...window.__listeners][0]; window.__inbox.refresh({ notify: false }); });
      await read.waitForFunction(() => window.__waiting.list?.length === 1);
      await read.evaluate(() => { window.__inbox.destroy(); window.__release('list'); });
      await settle(read);
      assert.deepEqual(await read.evaluate(() => [document.querySelectorAll('.inbox-card').length, window.__listeners.size, window.__onItems.length]), [0, 0, 0], 'a list answered after destroy draws nothing and the source is let go');
      await read.evaluate(() => { window.__listener(); window.__inbox.refresh(); });
      await settle(read);
      assert.equal((await calls(read, 'list')).length, 1, 'an event after destroy reads nothing');
      await read.close();
      const answering = await mount({ held: ['answer'] });
      await waitList(answering);
      await answering.$eval('[data-inbox-id=q2] .btn', el => el.click());
      await answering.waitForFunction(() => window.__waiting.answer?.length === 1);
      await answering.evaluate(() => { window.__inbox.destroy(); window.__release('answer'); });
      await settle(answering);
      assert.equal((await calls(answering, 'list')).length, 1, 'an answer finishing after destroy reads the list no more');
      assert.deepEqual(await answering.evaluate(async () => { const before = window.__calls.length; const inbox = window.__inbox; await inbox.answer(window.__items[0], 'x'); await inbox.store.decide(window.__items[0], 'resume'); await inbox.store.dismiss(window.__items[0]); await inbox.store.dismissResolved(); return window.__calls.length - before; }), 0, 'a destroyed view sends nothing');
      await answering.close();
      const failures = [{ ...base, id: 'd1', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'First', held_reason: 'room_closed' }, { ...base, id: 'd2', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'Second', held_reason: 'room_closed' }];
      const bulk = await mount({ list: failures, held: ['dismiss'] });
      await waitList(bulk);
      await bulk.$eval('.inbox-dismiss-resolved', el => el.click());
      await bulk.waitForFunction(() => window.__waiting.dismiss?.length === 1);
      await bulk.evaluate(() => { window.__inbox.destroy(); window.__release('dismiss'); });
      await settle(bulk);
      assert.equal((await calls(bulk, 'dismiss')).length, 1, 'the dismissal stops where the view was destroyed');
      await bulk.close();
    }
    // A write begun before the list is reset (sign-out) does not bring the Inbox back when it finishes, and a bulk dismissal stops where it was.
    {
      for (const [name, id, press] of [['answer', 'q2', '[data-inbox-id=q2] .btn'], ['decision', 'rec1', '[data-inbox-id=rec1] .inbox-decisions .btn'], ['dismiss', 'del1', '[data-inbox-id=del1] .btn-icon:not(.inbox-raw-toggle):not(.inbox-jump)']]) {
        const page = await mount({ held: [name] });
        await waitList(page);
        await page.$eval(press, el => el.click());
        await page.waitForFunction(name => window.__waiting[name]?.length === 1, {}, name);
        await page.evaluate(() => window.__inbox.reset());
        await page.evaluate(name => window.__release(name), name);
        await settle(page);
        assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('.inbox-card').length, window.__calls.filter(call => call[0] === 'list').length, window.__inbox.items === undefined]), [0, 1, true], `a ${name} finishing after a reset does not read or draw the list (${id})`);
        await page.close();
      }
      const failures = [{ ...base, id: 'd1', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'First', held_reason: 'room_closed' }, { ...base, id: 'd2', kind: 'delivery_failed', room_lifecycle: 'closed', question: 'Second', held_reason: 'room_closed' }];
      const bulk = await mount({ list: failures, held: ['dismiss'] });
      await waitList(bulk);
      await bulk.$eval('.inbox-dismiss-resolved', el => el.click());
      await bulk.waitForFunction(() => window.__waiting.dismiss?.length === 1);
      await bulk.evaluate(() => window.__inbox.reset());
      await bulk.evaluate(() => window.__release('dismiss'));
      await settle(bulk);
      assert.deepEqual(await bulk.evaluate(() => [window.__calls.filter(call => call[0] === 'dismiss').length, window.__calls.filter(call => call[0] === 'list').length, document.querySelector('.inbox-dismiss-resolved').disabled]), [1, 1, false], 'a bulk dismissal stops after its current item on a reset, reads nothing and is free to run again');
      await bulk.close();
    }
    // An item revised while its card is open keeps the card, the node and what was typed in it, and is answered at the new revision.
    {
      const page = await mount();
      await waitList(page);
      await page.$eval('[data-inbox-id=q1] .inbox-answer textarea', el => { el.value = 'my draft'; el.__node = true; });
      await page.evaluate(() => { window.__items = window.__items.map(item => ({ ...item, rev: 2 })); return window.__inbox.refresh(); });
      assert.deepEqual(await page.$eval('[data-inbox-id=q1] .inbox-answer textarea', el => [el.__node === true, el.value, document.querySelector('[data-inbox-id=q1]').dataset.rev]), [true, 'my draft', '2'], 'the draft and its node survive the revision');
      await page.$eval('[data-inbox-id=q2] .btn', el => el.click());
      await page.$eval('[data-inbox-id=ap1] .proposed-answer-btn', el => el.click());
      await page.$eval('[data-inbox-id=q1] .inbox-answer', el => el.requestSubmit());
      await page.waitForFunction(() => window.__calls.filter(call => call[0] === 'answer').length === 3);
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'answer').map(call => [call[1], call[2], call[4]])), [['q2', 'Fine', 2], ['ap1', 'Approve', 2], ['q1', 'my draft', 2]]);
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
