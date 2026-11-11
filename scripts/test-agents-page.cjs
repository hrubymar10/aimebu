// The Agents panel (cards, the people of the room, actions, live updates) in Chromium with every request held until the test answers it or
// answered by rule. web/ is served as is with its import map and markup, without app.js; no Go server. It runs unchanged against the page
// that replaced the hand-written panel. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const REPLACED = fs.existsSync(path.join(root, 'js', 'pages', 'agents', 'page.js'));
const PANEL = REPLACED ? '/js/pages/agents/page.js' : '/js/agents.js';
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
  health_flags: [], last_activity_at: at, ...patch });
const people = { humans: [{ id: 'usr_me', login: 'me', role: 'owner' }, { id: 'usr_bob', login: 'bob', role: 'viewer' }] };

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
  const mount = async ({ auto = {}, owner = true, role = 'owner' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; });
    await page.evaluate(async ({ module, owner, role }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner, admin: owner });
      const { AgentsPanel } = await import(module);
      const { HttpAgentSource } = await import('/js/agent-source.js');
      window.__calls = [];
      window.__panel = new AgentsPanel(new HttpAgentSource(), agent => window.__calls.push(['recovery', agent.id]), list => window.__calls.push(['change', list.map(item => item.id).join(',')]),
        agent => window.__calls.push(['private', agent.id]), human => window.__calls.push(['human', human.id]));
      window.__panel.projectRole = () => role;
    }, { module: PANEL, owner, role });
    return page;
  };
  const room = (id, patch = {}) => ({ id, project_id: 'p1', kind: 'task', title: id, lifecycle: 'open', ...patch });
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const count = (page, method, requestPath) => page.evaluate((method, requestPath) => window.__requests.filter(request => request.method === method && request.path === requestPath).length, method, requestPath);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const cards = page => page.$$eval('#agents-list .agent-card', nodes => nodes.map(node => [node.dataset.agentId, node.querySelector('.agent-name').textContent, node.querySelector('.agent-state-label').textContent]));
  const humans = page => page.$$eval('#agents-humans .human-card', nodes => nodes.map(node => [node.dataset.humanId, node.querySelector('.human-name').textContent, node.querySelector('.human-role').textContent, node.classList.contains('is-you'), !!node.querySelector('.human-dm')]));
  const calls = page => page.evaluate(() => window.__calls.splice(0));
  // Sets the room the panel shows and answers its first reads (agents and members).
  const show = async (page, id, list, members = people) => {
    await page.evaluate(room => { window.__shown = window.__panel.setRoom(room); }, room(id));
    await answer(page, await waitRequest(page, 'GET', `/api/rooms/${id}/agents`), 200, list.map(item => ({ ...item, room_id: id })));
    await answer(page, await waitRequest(page, 'GET', `/api/rooms/${id}/members`), 200, members);
    await page.evaluate(() => window.__shown); await settle(page);
  };
  const refresh = async (page, id, list, members = people) => {
    const before = await page.evaluate(() => window.__requests.length);
    await page.evaluate(() => { window.__refreshing = window.__panel.refresh(); });
    await page.waitForFunction(before => window.__requests.slice(before).some(request => request.path.endsWith('/agents') && !request.answered), {}, before);
    const index = await page.evaluate(before => window.__requests.findIndex((request, position) => position >= before && request.path.endsWith('/agents') && !request.answered), before);
    await answer(page, index, 200, list.map(item => ({ ...item, room_id: id })));
    await answer(page, await waitRequest(page, 'GET', `/api/rooms/${id}/members`), 200, members);
    await page.evaluate(() => window.__refreshing); await settle(page);
  };

  // The cards are drawn in the order the server sends them, with their state and actions, and the people of the room follow with a private-message
  // button for everyone but the person themselves.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert', { desired: 'stopped', observed: 'stopped' }), agent('a3', 'cleo', { desired: 'not_accepting' })]);
    assert.deepEqual(await cards(page), [['a1', 'ada', 'working'], ['a2', 'bert', 'stopped'], ['a3', 'cleo', 'paused']]);
    assert.deepEqual(await humans(page), [['usr_me', 'me', 'owner', true, false], ['usr_bob', 'bob', 'viewer', false, true]]);
    // The role label ends at the same right edge on every person's card, yours (no private-chat button) and another's (with one).
    const edges = await page.$$eval('#agents-humans .human-card .human-role', nodes => nodes.map(node => Math.round(node.getBoundingClientRect().right * 10) / 10));
    assert.ok(edges.length === 2 && edges[0] === edges[1], `role labels share a right edge: ${edges}`);
    // A person's status dot is the same size as an agent's, so the dots of the members panel line up.
    const dotBox = selector => page.$eval(selector, node => { const box = node.getBoundingClientRect(); return [Math.round(box.width * 10) / 10, Math.round(box.height * 10) / 10, Math.round((box.left + box.width / 2) * 10) / 10]; });
    const human = await dotBox('#agents-humans .human-card .agent-online-dot'), agentDot = await dotBox('[data-agent-id=a1] .agent-online-dot');
    assert.deepEqual([human[0], human[1], agentDot[0], agentDot[1]], [8, 8, 8, 8], `a person's dot and an agent's are both 8px: ${human} / ${agentDot}`);
    assert.equal(human[2], agentDot[2], 'and they are centred on the same line');
    const row = id => page.$eval(`[data-agent-id=${id}]`, node => ({ activity: node.querySelector('.agent-activity').textContent, dot: node.querySelector('.agent-dot').dataset.state,
      pause: [node.querySelector('.agent-act-pause').disabled, node.querySelector('.agent-act-pause').title], stop: node.querySelector('.agent-act-stop').disabled, reactivate: node.querySelector('.agent-reactivate').disabled,
      hint: [...node.querySelectorAll('.agent-action-hint:not([hidden])')].map(hint => hint.textContent) }));
    const working = await row('a1'), stopped = await row('a2'), paused = await row('a3');
    assert.equal(working.activity, 'Working…'); assert.equal(working.dot, 'working'); assert.deepEqual(working.pause, [false, 'Pause: finish the current turn, then accept no new work']);
    assert.deepEqual([stopped.pause[0], stopped.stop, stopped.reactivate], [true, true, false]);
    assert.deepEqual(stopped.hint, ['Pause is unavailable: it is stopped; start it first.', 'Stop is unavailable: it is already stopped.', 'Switch is unavailable: start it first.']);
    assert.equal(paused.pause[1], 'Pause is unavailable: it is already paused');
    assert.deepEqual((await calls(page)).filter(call => call[0] === 'change'), [['change', ''], ['change', 'a1,a2,a3']]);
    await page.close();
  }

  // Legacy writer metadata does not disable Start.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a2', 'bert', { desired: 'stopped', observed: 'stopped', writer_blocked_by: { holder: 'ada@test' } })]);
    assert.equal(await page.$eval('[data-agent-id=a2] .agent-reactivate', node => node.disabled), false);
    await page.close();
  }

  // An idle agent that will not start does not look idle: one that needs a decision and one the limit guard holds show an orange dot, a state
  // that says which, and the reason on the card; an ordinary idle agent stays green.
  {
    const page = await mount();
    const hold = { code: 'limit_guard', text: 'claude session 95%', detail: 'The limit guard is holding new claude turns: the session limit of the active account is 95% full.' };
    await show(page, 'r1', [agent('a1', 'ada'), agent('a2', 'bert', { recovery_block: true, last_exit_evidence: 'Failed to authenticate' }), agent('a3', 'cleo', { hold })]);
    const look = id => page.$eval(`[data-agent-id=${id}]`, node => ({ label: node.querySelector('.agent-state-label').textContent, dot: node.querySelector('.agent-dot').dataset.state, state: node.querySelector('.agent-state').dataset.state,
      activity: node.querySelector('.agent-activity').textContent, color: getComputedStyle(node.querySelector('.agent-dot')).backgroundColor, stateColor: getComputedStyle(node.querySelector('.agent-state-badge')).color }));
    const [ordinary, decision, held] = [await look('a1'), await look('a2'), await look('a3')];
    assert.deepEqual([ordinary.label, ordinary.dot, ordinary.activity], ['idle', 'idle', '']);
    assert.deepEqual([decision.label, decision.dot, decision.state], ['needs decision', 'attention', 'attention']);
    assert.match(decision.activity, /^Needs your decision: .*decide what to do in Inbox\.$/);
    assert.deepEqual([held.label, held.dot, held.state, held.activity], ['held: claude session 95%', 'attention', 'attention', hold.detail]);
    assert.equal(decision.color, held.color, 'both uncertain states share one dot colour');
    // v1 centres the hover buttons on their containing block; a collapsed card can have more than one line (the recovery card has its button and a second line),
    // so the name row is that block while the card is closed, and the card itself once it is open (the buttons sit at its top).
    const box = id => page.$eval(`[data-agent-id=${id}]`, card => ({ row: getComputedStyle(card.querySelector('.agent-id')).position, actions: getComputedStyle(card.querySelector('.agent-card-actions')).position, tall: card.getBoundingClientRect().height > card.querySelector('.agent-id').getBoundingClientRect().height + 20 }));
    const closed = await box('a2');
    assert.equal(closed.tall, true, 'a card with a recovery decision has more than its name row');
    assert.equal(closed.row, 'relative', 'a collapsed card centres its hover buttons on the name row, not on the whole card');
    await page.$eval('[data-agent-id=a2] .agent-expand', button => button.click()); await settle(page);
    assert.equal((await box('a2')).row, 'static', 'an open card keeps the card as their box');
    await page.$eval('#agent-profile-close', button => button.click()); await settle(page);
    assert.notEqual(decision.color, ordinary.color, 'and it is not the green of an idle agent');
    assert.notEqual(decision.stateColor, ordinary.stateColor, 'the state text is not green either');
    await page.close();
  }

  // An empty room says so, and the line goes when the first agent arrives.
  {
    const page = await mount();
    await show(page, 'r1', []);
    assert.equal(await page.$eval('#agents-list .agents-empty', node => node.textContent), 'No agents in this room');
    await refresh(page, 'r1', [agent('a1', 'ada')]);
    assert.equal(await page.$('#agents-list .agents-empty'), null);
    await refresh(page, 'r1', []);
    assert.deepEqual(await cards(page), []);
    assert.equal(await page.$eval('#agents-list .agents-empty', node => node.textContent), 'No agents in this room');
    await page.close();
  }

  // A refresh changes a card in place: the same element stays, with its focus, its text selection and the open Output section of the profile,
  // whether the refresh was asked for or came from the event stream.
  for (const source of ['refresh', 'event']) {
    const page = await mount({ auto: { 'GET /api/rooms/r1/agents/a1/profile': { turns_today: 2 }, 'GET /api/rooms/r1/agents/a1/output?lines=200': { text: 'stderr line', truncated: false } } });
    await show(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert')]);
    await page.evaluate(() => { window.__node = document.querySelector('[data-agent-id=a1]'); window.__other = document.querySelector('[data-agent-id=a2]'); });
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.waitForSelector('[data-agent-id=a1] .profile-output');
    await page.$eval('[data-agent-id=a1] .profile-output summary', node => node.click());
    await page.waitForFunction(() => document.querySelector('[data-agent-id=a1] .profile-output-text')?.textContent === 'stderr line');
    await page.evaluate(() => {
      window.__output = document.querySelector('[data-agent-id=a1] .profile-output');
      const name = document.querySelector('[data-agent-id=a1] .agent-name'); const range = document.createRange(); range.selectNodeContents(name); const selection = getSelection(); selection.removeAllRanges(); selection.addRange(range);
      document.querySelector('[data-agent-id=a1] .agent-act-pause').focus();
    });
    const next = [agent('a1', 'ada', { observed: 'idle', activity: 'Waiting', desired_rev: 2 }), agent('a2', 'bert', { desired: 'not_accepting', desired_rev: 2 })];
    if (source === 'refresh') await refresh(page, 'r1', next);
    else {
      await page.evaluate(() => { window.__event = window.__panel.source.onEvent({ agent_id: 'a1', kind: 'agent_activity', payload_json: JSON.stringify({ summary: 'event summary' }), created_at: '2026-01-01T10:05:00Z' }); });
      await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, next.map(item => ({ ...item, room_id: 'r1' })));
      await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/members'), 200, people);
      await page.evaluate(() => window.__event); await settle(page);
    }
    assert.deepEqual(await cards(page), [['a1', 'ada', 'idle'], ['a2', 'bert', 'paused']], source);
    const kept = await page.evaluate(() => ({ same: document.querySelector('[data-agent-id=a1]') === window.__node && document.querySelector('[data-agent-id=a2]') === window.__other,
      output: document.querySelector('[data-agent-id=a1] .profile-output') === window.__output && window.__output.open, text: document.querySelector('[data-agent-id=a1] .profile-output-text').textContent,
      focus: document.activeElement === document.querySelector('[data-agent-id=a1] .agent-act-pause'), selected: getSelection().toString(), activity: document.querySelector('[data-agent-id=a1] .agent-activity').textContent }));
    assert.deepEqual([kept.same, kept.output, kept.text, kept.focus], [true, true, 'stderr line', true], `${source}: ${JSON.stringify(kept)}`);
    // The hand-written panel rewrote the text of a card on every refresh, which collapsed a selection in it, and re-attached the Output section,
    // which dropped the focus inside it; the page that replaced it touches only what changed.
    if (REPLACED) assert.equal(kept.selected, 'ada', `${source}: the selection in the name stays`);
    if (source === 'event') assert.equal(kept.activity, 'event summary');
    await page.close();
  }

  // A refresh with the same data changes nothing: the page keeps the record it holds for each agent, so no card renders and nothing in the list is
  // written (the clock's own text apart); one that changed renders that card alone. The hand-written panel rewrote every card on every refresh.
  if (REPLACED) {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert')]);
    await page.evaluate(() => {
      window.__heldMap = window.__panel.cards; window.__held = new Map(window.__heldMap); window.__written = [];
      new MutationObserver(records => { for (const record of records) { const node = record.target.nodeType === 1 ? record.target : record.target.parentElement; if (!node.closest('.agent-state-age, .agent-live-chip')) window.__written.push(`${record.type}:${node.className}`); } })
        .observe(document.querySelector('#agents-list'), { subtree: true, childList: true, characterData: true, attributes: true });
    });
    await refresh(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert')]);
    assert.deepEqual(await page.evaluate(() => [window.__panel.cards === window.__heldMap ? 'same map' : 'new map', window.__written]), ['same map', []], 'unchanged data is not written');
    await refresh(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert', { desired: 'not_accepting' })]);
    const kept = await page.evaluate(() => ({ a1: window.__panel.cards.get('a1') === window.__held.get('a1'), a2: window.__panel.cards.get('a2') === window.__held.get('a2'), written: [...new Set(window.__written)] }));
    assert.deepEqual([kept.a1, kept.a2], [true, false], 'the record of the unchanged agent is kept');
    assert.equal(kept.written.length > 0 && kept.written.every(entry => entry.length > 0), true);
    assert.equal(await page.$$eval('[data-agent-id=a1] *', nodes => nodes.length > 0), true);
    await page.close();
  }

  // A card keeps its place when another one goes and a new one arrives, and the one that went takes its node with it.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada'), agent('a2', 'bert')]);
    await page.evaluate(() => { window.__node = document.querySelector('[data-agent-id=a2]'); });
    await refresh(page, 'r1', [agent('a2', 'bert'), agent('a3', 'cleo')]);
    assert.deepEqual((await cards(page)).map(card => card[0]), ['a2', 'a3']);
    assert.equal(await page.evaluate(() => document.querySelector('[data-agent-id=a2]') === window.__node), true);
    await page.close();
  }

  // The phone's members drawer stays open when the same room is selected again (a late second selection, a refresh) and closes when the person
  // moves to another room, or the session is reset.
  {
    const page = await mount();
    const drawer = () => page.evaluate(() => [document.querySelector('#agents-panel').classList.contains('mobile-open'), document.querySelector('.chat').classList.contains('agents-open'), document.querySelector('#agents-toggle').getAttribute('aria-expanded')]);
    await show(page, 'r1', [agent('a1', 'ada')]);
    await page.evaluate(() => window.__panel.setMobileOpen(true));
    assert.deepEqual(await drawer(), [true, true, 'true'], 'the drawer is open');
    await show(page, 'r1', [agent('a1', 'ada')]); // the same room again
    assert.deepEqual(await drawer(), [true, true, 'true'], 'selecting the same room again leaves the drawer open');
    await refresh(page, 'r1', [agent('a1', 'ada')]);
    assert.deepEqual(await drawer(), [true, true, 'true'], 'a refresh leaves it open');
    await show(page, 'r2', [agent('b1', 'bea', { room_id: 'r2' })]);
    assert.deepEqual(await drawer(), [false, false, 'false'], 'another room closes it');
    await page.evaluate(() => window.__panel.setMobileOpen(true));
    await show(page, 'r2', []);
    assert.deepEqual((await drawer())[0], true, 'open again, and the same room still leaves it');
    await show(page, 'r1', [agent('a1', 'ada')]);
    assert.deepEqual((await drawer())[0], false, 'going back to the first room is a move too');
    await page.evaluate(() => window.__panel.setMobileOpen(true)); await page.evaluate(() => window.__panel.reset());
    assert.deepEqual((await drawer())[0], false, 'a reset closes it');
    await show(page, 'r1', [agent('a1', 'ada')]); await page.evaluate(() => window.__panel.setMobileOpen(true)); await show(page, 'r1', [agent('a1', 'ada')]);
    assert.deepEqual((await drawer())[0], true, 'after a reset the room is new to the panel: the first selection closes it, the next one leaves it');
    await page.close();
  }

  // An open profile has one way out: the back arrow, first in the panel's header. The edge arrow and the phone's close button wait until it is closed, and
  // the card has no button of its own; a path or an id is cut at its separators and has the whole width; the phone's Agents tab opens the members, not the
  // profile left open from before.
  {
    const page = await mount();
    await page.setViewport({ width: 1280, height: 800 });
    await show(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn' }), agent('a2', 'bert')]);
    const controls = () => page.evaluate(() => {
      const visible = el => !!el && !el.hidden && getComputedStyle(el).display !== 'none';
      const header = [...document.querySelectorAll('#agents-panel > header > *')].filter(visible).map(el => el.id || el.tagName.toLowerCase());
      const card = document.querySelector('[data-agent-id=a1]');
      return { header, back: [document.querySelector('#agent-profile-close').getAttribute('aria-label'), !!document.querySelector('#agent-profile-close .icon-mask-left')], cardButton: !!card.querySelector('.agent-expand'), closeLooking: [...document.querySelectorAll('#agents-panel button')].filter(visible).filter(button => /close|hide|back/i.test(button.getAttribute('aria-label') || '')).map(button => button.id || button.getAttribute('aria-label')) };
    });
    assert.deepEqual((await controls()).header.slice(0, 1), ['edge-agents'], 'with the members list shown the edge arrow hides the panel');
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const profile = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    await answer(page, profile, 200, { native_session_id: '2c081365-8e68-4ebb-9d7f-a083a581d530', cwd: '/Users/someone/work/some-project/tmp', machine_name: 'Host' }); await settle(page);
    const open = await controls();
    assert.deepEqual([open.header[0], open.header.includes('edge-agents'), open.header.includes('agents-close'), open.back, open.cardButton, open.closeLooking], ['agent-profile-close', false, false, ['Back to members', true], false, ['agent-profile-close']], `one way out of a profile, first in the header: ${JSON.stringify(open)}`);
    // The path and the id: cut after / - _ . only, whole width under the label, text unchanged.
    const facts = await page.$$eval('[data-agent-id=a1] .profile-fact-mono', rows => rows.map(row => {
      const label = row.querySelector('.profile-fact-label').getBoundingClientRect(), value = row.querySelector('.profile-fact-value');
      const box = value.getBoundingClientRect(), panel = document.querySelector('#agents-panel').getBoundingClientRect();
      return { label: row.querySelector('.profile-fact-label').textContent, text: value.textContent, breaks: value.querySelectorAll('wbr').length, below: box.top >= label.bottom - 1, left: Math.round(box.left - label.left), fits: box.right <= panel.right + 1,
        after: [...value.querySelectorAll('wbr')].every(mark => /[/\-_.]$/.test(mark.previousSibling?.textContent || '')) };
    }));
    const by = name => facts.find(fact => fact.label === name);
    assert.deepEqual([by('CWD').text, by('CWD').breaks, by('CWD').after], ['/Users/someone/work/some-project/tmp', 6, true], 'the path can break after each slash and hyphen, and the text is the same');
    assert.deepEqual([by('Session').text, by('Session').after, by('Agent ID').text, by('Agent ID').after], ['2c081365-8e68-4ebb-9d7f-a083a581d530', true, 'a1', true]);
    for (const fact of facts) assert.deepEqual([fact.below, fact.left, fact.fits], [true, 0, true], `${fact.label} has the width under its label`);
    // No line of the path ends in the middle of a name: every line starts after a separator.
    const lines = await page.evaluate(() => {
      const value = [...document.querySelectorAll('[data-agent-id=a1] .profile-fact-mono')].find(row => row.querySelector('.profile-fact-label').textContent === 'CWD').querySelector('.profile-fact-value');
      const range = document.createRange(), text = value.firstChild, rows = new Map();
      for (let node of value.childNodes) { if (node.nodeType !== 3) continue; for (let i = 0; i < node.length; i++) { range.setStart(node, i); range.setEnd(node, i + 1); const rect = range.getBoundingClientRect(); const top = Math.round(rect.top); if (!rows.has(top)) rows.set(top, ''); rows.set(top, rows.get(top) + node.data[i]); } }
      return [...rows.values()];
    });
    assert.ok(lines.slice(0, -1).every(line => /[/\-_.]$/.test(line)), `a line of the path ends inside a name: ${JSON.stringify(lines)}`);
    // The back arrow closes it and the focus goes back to the card's open button.
    await page.$eval('#agent-profile-close', node => node.click()); await settle(page);
    // (the product puts the focus back a frame after the card is drawn closed, so the test waits for it instead of reading it at once)
    await page.waitForFunction(() => document.activeElement === document.querySelector('[data-agent-id=a1] .agent-expand')).catch(() => {});
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('[data-agent-id=a1] .agent-expand')), true, 'the focus is back on the card');
    // A phone: the Agents tab opens the members list even when a profile was left open.
    await page.setViewport({ width: 390, height: 800 });
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click()); await settle(page);
    assert.equal(await page.evaluate(() => document.querySelector('#agents-panel').classList.contains('profile-open')), true);
    await page.$eval('#agents-toggle', node => node.click()); // opens the drawer
    await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#agents-panel').classList.contains('mobile-open'), document.querySelector('#agents-panel').classList.contains('profile-open'), document.querySelector('#agents-panel header h2').textContent]), [true, false, 'Members'], 'the Agents tab shows the members, not a profile left open');
    // A tap on a member in the drawer opens its profile, which has the back arrow, and the drawer stays open.
    await page.$eval('[data-agent-id=a2] .agent-expand', node => node.click()); await settle(page);
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#agents-panel').classList.contains('mobile-open'), document.querySelector('#agents-panel').classList.contains('profile-open'), getComputedStyle(document.querySelector('#agent-profile-close')).display !== 'none']), [true, true, true]);
    await page.close();
  }

  // Answers for a room the panel has left draw nothing: the agents, the people of the room, and everything after reset().
  {
    const page = await mount();
    await page.evaluate(room => { window.__first = window.__panel.setRoom(room); }, room('r1'));
    const stale = await waitRequest(page, 'GET', '/api/rooms/r1/agents');
    await page.evaluate(room => { window.__second = window.__panel.setRoom(room); }, room('r2'));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r2/agents'), 200, [agent('b1', 'bea', { room_id: 'r2' })]);
    await answer(page, stale, 200, [agent('a1', 'ada')]);
    await page.evaluate(() => Promise.all([window.__first, window.__second]));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r2/members'), 200, { humans: [{ id: 'usr_bob', login: 'bob', role: 'viewer' }] });
    await settle(page);
    assert.deepEqual((await cards(page)).map(card => card[0]), ['b1']);
    assert.deepEqual((await humans(page)).map(human => human[0]), ['usr_bob']);
    assert.equal((await page.evaluate(() => [...window.__panel.cards.keys()])).join(), 'b1');
    // The people of room r2 arrive after the panel moved to r3.
    await page.evaluate(room => { window.__third = window.__panel.setRoom(room); }, room('r3'));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r3/agents'), 200, []);
    await page.evaluate(() => window.__third); await settle(page);
    assert.deepEqual(await humans(page), [], 'the people of the previous room are gone');
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r3/members'), 200, { humans: [{ id: 'usr_me', login: 'me', role: 'owner' }] });
    await settle(page);
    assert.deepEqual((await humans(page)).map(human => human[0]), ['usr_me']);
    // After reset() a held read draws nothing.
    await page.evaluate(() => { window.__again = window.__panel.refresh(); });
    const held = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/rooms/r3/agents'));
    await page.evaluate(() => window.__panel.reset());
    await answer(page, held, 200, [agent('c1', 'cy', { room_id: 'r3' })]);
    await page.evaluate(() => window.__again); await settle(page);
    assert.equal(await page.$('[data-agent-id=c1]'), null);
    assert.equal(await count(page, 'GET', '/api/rooms/r3/members'), 1, 'a read that was dropped does not go on to read the members');
    await page.close();
  }

  // waitForCard answers true when the card is there or arrives for the same room, and false when the time is up or the room is another one.
  {
    const page = await mount();
    await page.evaluate(room => { window.__shown = window.__panel.setRoom(room); }, room('r1'));
    const found = await page.evaluate(() => { window.__wait = window.__panel.waitForCard('a1', 'r1', 3000); return 'pending'; });
    assert.equal(found, 'pending');
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    assert.equal(await page.evaluate(() => window.__wait), true);
    assert.equal(await page.evaluate(() => window.__panel.waitForCard('a1', 'r1')), true, 'a card that is already there answers at once');
    assert.equal(await page.evaluate(() => window.__panel.waitForCard('a1', 'r2', 100)), false, 'another room');
    assert.equal(await page.evaluate(() => window.__panel.waitForCard('nobody', 'r1', 100)), false, 'no such agent');
    await page.close();
  }

  // The buttons of a card do what they say: control calls carry the revision the card was drawn from and the list is read again afterwards; a
  // refusal is shown and the card stays; the card's other buttons hand the agent to the page.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada', { recovery_block: true, desired_rev: 4 })]);
    await page.$eval('[data-agent-id=a1] .agent-act-pause', node => node.click());
    const control = await waitRequest(page, 'POST', '/api/agents/a1/control');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, control), { desired: 'not_accepting', rev: 4 });
    await answer(page, control, 200, agent('a1', 'ada', { desired: 'not_accepting', desired_rev: 5 }));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada', { desired: 'not_accepting', desired_rev: 5, recovery_block: true })]);
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/members'), 200, people);
    await settle(page);
    assert.deepEqual(await cards(page), [['a1', 'ada', 'paused']]);
    await page.$eval('[data-agent-id=a1] .agent-act-stop', node => node.click());
    await answer(page, await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/agents/a1/control')), 409, { message: 'The agent changed; read it again', error: 'stale' });
    await settle(page);
    assert.equal(await page.$eval('#agents-error', node => node.textContent), 'The agent changed; read it again');
    assert.deepEqual(await cards(page), [['a1', 'ada', 'paused']]);
    await calls(page);
    await page.$eval('[data-agent-id=a1] .agent-message', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-private', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-recovery', node => node.click());
    await page.$eval('[data-human-id=usr_bob] .human-dm', node => node.click());
    assert.deepEqual(await calls(page), [['private', 'a1'], ['private', 'a1'], ['recovery', 'a1'], ['human', 'usr_bob']]);
    await page.close();
  }

  // A viewer or a closed room cannot spawn; the button follows the room the panel shows.
  {
    const page = await mount({ role: 'viewer' });
    await show(page, 'r1', []);
    assert.deepEqual(await page.$eval('#spawn-open', node => [node.disabled, node.title]), [true, 'Viewers cannot spawn agents']);
    await page.close();
    const open = await mount();
    await show(open, 'r1', []);
    assert.equal(await open.$eval('#spawn-open', node => node.disabled), false);
    await open.evaluate(room => window.__panel.setRoomFacts(room), room('r1', { lifecycle: 'archived' }));
    assert.equal(await open.$eval('#spawn-open', node => node.disabled), true);
    await open.evaluate(room => window.__panel.setRoomFacts(room), room('r1', { kind: 'dm' }));
    assert.equal(await open.$eval('#spawn-open', node => node.disabled), true);
    await open.close();
  }

  // The profile replaces the list of members in place: the title, the close button, the expanded card and its facts, first from the card and then
  // with what the profile read adds; the close button, the same button again, Escape and the agent leaving all bring the list back, and Escape
  // leaves an open dialog alone.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada', { observed: 'in_turn', role_emoji: '🔧' }), agent('a2', 'bert')]);
    const state = () => page.evaluate(() => ({ title: document.querySelector('#agents-panel header h2').textContent, closeHidden: document.querySelector('#agent-profile-close').hidden, open: document.querySelector('#agents-panel').classList.contains('profile-open'),
      expanded: [...document.querySelectorAll('[data-agent-id]')].map(card => [card.dataset.agentId, card.classList.contains('expanded'), card.querySelector('.agent-expand')?.getAttribute('aria-expanded') ?? null, card.querySelector('.agent-expand')?.getAttribute('aria-label') ?? null]),
      facts: [...document.querySelectorAll('[data-agent-id=a1] .profile-fact')].map(row => [row.querySelector('.profile-fact-label').textContent, row.querySelector('.profile-fact-value').textContent]) }));
    const closed = await state();
    assert.deepEqual([closed.title, closed.closeHidden, closed.open], ['Members', true, false]);
    assert.deepEqual(closed.facts, []);
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const profile = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    const opened = await state();
    assert.deepEqual([opened.title, opened.closeHidden, opened.open], ['Agent profile', false, true]);
    assert.deepEqual(opened.expanded, [['a1', true, null, null], ['a2', false, 'false', 'Open agent profile']], 'an open card has no button of its own: the panel\'s back arrow is the one way out');
    const labels = opened.facts.map(fact => fact[0]);
    assert.deepEqual(labels, ['Runtime', 'Agent ID'], 'the card alone gives these until the profile is read');
    assert.equal(opened.facts.some(fact => fact[0] === 'Role'), false, 'the role is on the card, not repeated in the facts');
    await answer(page, profile, 200, { native_session_id: 'sess-1', resume_command: 'codex resume sess-1', cwd: '/work', branch: 'main', machine_name: 'Host', project_name: 'Alpha', spawned_by: 'owner', turns_today: 3, cost_today: 0.42 });
    await settle(page);
    const full = await state();
    assert.deepEqual(full.facts.map(fact => fact[0]), ['Runtime', 'Session', 'Resume', 'CWD', 'Branch', 'Machine', 'Project', 'Spawned', 'Last 24 h', 'Agent ID']);
    assert.deepEqual(full.facts.find(fact => fact[0] === 'Last 24 h'), ['Last 24 h', '3 turns · $0.42']);
    assert.equal(await page.$eval('[data-agent-id=a1] .profile-copy', node => node.getAttribute('aria-label')), 'Copy resume');
    await page.$eval('#agent-profile-close', node => node.click());
    assert.deepEqual((await state()).title, 'Members');
    await page.waitForFunction(() => document.activeElement === document.querySelector('[data-agent-id=a1] .agent-expand')).catch(() => {}); // the focus goes back a frame after the card is drawn closed
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('[data-agent-id=a1] .agent-expand')), true, 'closing returns the focus to the card');
    // The back arrow closes it again; a click on the card outside its buttons opens it.
    await page.$eval('[data-agent-id=a2]', node => node.click());
    assert.equal((await state()).open, true);
    await page.$eval('#agent-profile-close', node => node.click());
    assert.equal((await state()).open, false);
    // Escape closes it, unless a dialog is open.
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.evaluate(() => document.querySelector('#spawn-dialog').showModal());
    await page.keyboard.press('Escape'); await page.evaluate(() => document.querySelector('#spawn-dialog').close());
    assert.equal((await state()).open, true, 'Escape belongs to the dialog');
    await page.keyboard.press('Escape');
    assert.equal((await state()).open, false);
    // The agent leaving the room closes it.
    await page.$eval('[data-agent-id=a2] .agent-expand', node => node.click());
    await refresh(page, 'r1', [agent('a1', 'ada')]);
    assert.deepEqual([(await state()).title, (await state()).open], ['Members', false]);
    await page.close();
  }

  // Answers of a profile that is not the open one any more draw nothing: after it was closed, after another agent was opened, after the room changed
  // and after reset().
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada'), agent('a2', 'bert')]);
    const facts = id => page.$$eval(`[data-agent-id=${id}] .profile-fact-label`, nodes => nodes.map(node => node.textContent));
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const first = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    await page.$eval('[data-agent-id=a2] .agent-expand', node => node.click());
    const second = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a2/profile');
    await answer(page, first, 200, { cwd: '/first' }); await settle(page);
    assert.deepEqual([(await facts('a1')).includes('CWD'), (await facts('a2')).includes('CWD')], [false, false], 'the profile of the agent that was left for another draws nothing');
    await answer(page, second, 200, { cwd: '/second' }); await settle(page);
    assert.equal((await facts('a2')).includes('CWD'), true);
    await page.$eval('#agent-profile-close', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    const held = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/rooms/r1/agents/a1/profile'));
    await page.$eval('#agent-profile-close', node => node.click());
    const beforeLate = await facts('a1');
    await answer(page, held, 200, { cwd: '/late' }); await settle(page);
    assert.deepEqual(await facts('a1'), beforeLate, 'the answer for a profile that was closed adds nothing');
    assert.equal(beforeLate.includes('CWD'), false);
    // A room change and a reset drop the read of a profile that is still open.
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const open = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/rooms/r1/agents/a1/profile'));
    await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2'));
    await answer(page, open, 200, { cwd: '/other-room' }); await settle(page);
    assert.deepEqual([(await page.$$('.profile-fact')).length, await page.$eval('#agents-panel header h2', node => node.textContent)], [0, 'Members']);
    await page.close();
  }

  // The Output section is for the project's owner or an admin: collapsed until opened, read from the machine on opening and on request, with the
  // end of the output, a note when nothing was captured, the server's own words when it fails, and a read for a profile that was closed drawn nowhere.
  {
    const viewer = await mount({ owner: false, role: 'viewer' });
    await show(viewer, 'r1', [agent('a1', 'ada')]);
    await viewer.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    assert.equal(await viewer.$('[data-agent-id=a1] .profile-output'), null, 'a viewer has no Output section');
    await viewer.close();
    const page = await mount({ owner: false, role: 'owner' });
    await show(page, 'r1', [agent('a1', 'ada'), agent('a2', 'bert')]);
    const output = () => page.$eval('[data-agent-id=a1] .profile-output', node => ({ open: node.open, text: node.querySelector('.profile-output-text').textContent, hidden: node.querySelector('.profile-output-text').hidden, status: node.querySelector('.profile-output-status').textContent,
      refresh: node.querySelector('.profile-output-refresh').disabled, download: node.querySelector('.profile-output-download').disabled }));
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    assert.deepEqual(await output(), { open: false, text: '', hidden: true, status: '', refresh: false, download: true });
    assert.equal(await count(page, 'GET', '/api/rooms/r1/agents/a1/output?lines=200'), 0, 'nothing is read before the section is opened');
    await page.$eval('[data-agent-id=a1] .profile-output summary', node => node.click());
    const read = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/output?lines=200');
    assert.deepEqual([(await output()).status, (await output()).refresh], ['Loading…', true]);
    await answer(page, read, 200, { text: 'line one\nline two\n', truncated: true }); await settle(page);
    assert.deepEqual(await output(), { open: true, text: 'line one\nline two\n', hidden: false, status: 'Earlier output is not shown.', refresh: false, download: false });
    await page.$eval('[data-agent-id=a1] .profile-output-refresh', node => node.click());
    await answer(page, await page.evaluate(() => window.__requests.findLastIndex(request => request.path.includes('/output'))), 200, { text: '', truncated: false }); await settle(page);
    assert.deepEqual([(await output()).status, (await output()).hidden, (await output()).download], ['No output has been captured for this agent yet.', true, true]);
    await page.$eval('[data-agent-id=a1] .profile-output-refresh', node => node.click());
    await answer(page, await page.evaluate(() => window.__requests.findLastIndex(request => request.path.includes('/output'))), 404, { message: 'No output', error: 'no_output' }); await settle(page);
    assert.equal((await output()).status, 'No output has been captured for this agent yet.');
    await page.$eval('[data-agent-id=a1] .profile-output-refresh', node => node.click());
    await answer(page, await page.evaluate(() => window.__requests.findLastIndex(request => request.path.includes('/output'))), 502, { message: 'The machine is offline' }); await settle(page);
    assert.equal((await output()).status, 'The machine is offline');
    // A read that is still held when the profile closes, and opens again, writes into the new section only.
    await page.$eval('[data-agent-id=a1] .profile-output-refresh', node => node.click());
    const stale = await page.evaluate(() => window.__requests.findLastIndex(request => request.path.includes('/output')));
    await page.$eval('#agent-profile-close', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await answer(page, stale, 200, { text: 'old read', truncated: false }); await settle(page);
    assert.deepEqual([(await output()).open, (await output()).text], [false, ''], 'a new section starts closed and empty');
    await page.close();
  }

  // A control call that is still out when the panel moves to another room or is reset changes nothing there: no read of the new room, no error
  // in its panel, the cards it shows stay as they are.
  for (const [how, status] of [['room', 200], ['reset', 200], ['room', 409], ['reset', 409]]) {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada')]);
    await page.$eval('[data-agent-id=a1] .agent-act-pause', node => node.click());
    const held = await waitRequest(page, 'POST', '/api/agents/a1/control');
    if (how === 'room') await page.evaluate(room => window.__panel.setRoomFacts(room), room('r2')); else await page.evaluate(() => window.__panel.reset());
    const reads = () => page.evaluate(() => window.__requests.filter(request => request.method === 'GET').map(request => request.path));
    const before = await reads();
    await answer(page, held, status, status === 200 ? agent('a1', 'ada', { desired: 'not_accepting', desired_rev: 2 }) : { message: 'The agent changed', error: 'stale' });
    await settle(page);
    const after = await page.evaluate(() => ({ error: document.querySelector('#agents-error').textContent, cards: document.querySelectorAll('[data-agent-id]').length }));
    assert.deepEqual(after, { error: '', cards: 0 }, `${how} ${status}: a call of the room that was left shows nothing`);
    const added = (await reads()).slice(before.length);
    assert.equal(added.some(path => path.startsWith('/api/rooms/r1/')), false, `${how} ${status}: the room that was left is not read again`);
    // The source tells the panel to refresh when a control call ends, so the room the panel has moved to may be read once; nothing else is.
    assert.equal(added.every(path => path === '/api/rooms/r2/agents') && added.length <= 1, true, `${how} ${status}: at most one read of the room on show, got ${added}`);
    await page.close();
  }

  // Two reads for the same room can finish in either order: the older one answered last must not replace what the newer one drew, neither the
  // cards nor the people.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada')], { humans: [{ id: 'usr_me', login: 'me', role: 'owner' }] });
    const last = (path) => page.evaluate(path => window.__requests.findLastIndex(request => request.path === path && !request.answered), path);
    // The agent list: the first read is held, the second is answered with newer data, then the first is released with the older.
    await page.evaluate(() => { window.__first = window.__panel.refresh(); });
    const first = await waitRequest(page, 'GET', '/api/rooms/r1/agents');
    await page.evaluate(() => { window.__second = window.__panel.refresh(); });
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/rooms/r1/agents' && !request.answered).length === 2);
    const second = await last('/api/rooms/r1/agents');
    assert.notEqual(first, second);
    await answer(page, second, 200, [agent('a1', 'ada', { desired: 'not_accepting', desired_rev: 2 })]);
    await answer(page, first, 200, [agent('a1', 'ada'), agent('a2', 'old')]);
    await page.evaluate(() => Promise.all([window.__first, window.__second])); await settle(page);
    assert.deepEqual(await cards(page), [['a1', 'ada', 'paused']], 'the older list does not replace the newer');
    assert.equal(await count(page, 'GET', '/api/rooms/r1/members'), 2, 'the older list starts no read of the people');
    for (const index of await page.evaluate(() => window.__requests.map((request, position) => (request.path === '/api/rooms/r1/members' && !request.answered ? position : -1)).filter(position => position >= 0))) await answer(page, index, 200, { humans: [{ id: 'usr_me', login: 'me', role: 'owner' }] });
    await settle(page);
    // The people: the first read of the people is held, the second is answered with newer data, then the first is released.
    await page.evaluate(() => { window.__one = window.__panel.refresh(); });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    const olderPeople = await waitRequest(page, 'GET', '/api/rooms/r1/members');
    await page.evaluate(() => { window.__two = window.__panel.refresh(); });
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/rooms/r1/agents' && !request.answered).length === 1);
    await answer(page, await last('/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/rooms/r1/members' && !request.answered).length === 2);
    const newerPeople = await last('/api/rooms/r1/members');
    assert.notEqual(olderPeople, newerPeople);
    await answer(page, newerPeople, 200, { humans: [{ id: 'usr_bob', login: 'bob', role: 'viewer' }] });
    await answer(page, olderPeople, 200, { humans: [{ id: 'usr_old', login: 'old', role: 'viewer' }] });
    await page.evaluate(() => Promise.all([window.__one, window.__two])); await settle(page);
    assert.deepEqual((await humans(page)).map(human => human[0]), ['usr_bob'], 'the older answer for the people does not replace the newer');
    await page.close();
  }

  // The profile belongs to one opening: an answer for an earlier opening of the same agent, or for an earlier reload, does not fill the one that
  // replaced it.
  {
    const page = await mount({ auto: { 'GET /api/rooms/r1/agents/a1/profile': undefined } });
    await show(page, 'r1', [agent('a1', 'ada')]);
    const cwd = () => page.$$eval('[data-agent-id=a1] .profile-fact', rows => rows.filter(row => row.querySelector('.profile-fact-label').textContent === 'CWD').map(row => row.querySelector('.profile-fact-value').textContent));
    const pending = () => page.evaluate(() => window.__requests.map((request, position) => (request.path === '/api/rooms/r1/agents/a1/profile' && !request.answered ? position : -1)).filter(position => position >= 0));
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const old = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    await page.$eval('#agent-profile-close', node => node.click());
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/rooms/r1/agents/a1/profile' && !request.answered).length === 2);
    const fresh = (await pending()).find(index => index !== old);
    await answer(page, fresh, 200, { cwd: '/new' });
    await answer(page, old, 200, { cwd: '/old' }); await settle(page);
    assert.deepEqual(await cwd(), ['/new'], 'the answer of the earlier opening does not fill the reopened profile');
    // Two reloads of an open profile that are out together: the newer answers first, and the older one then changes nothing.
    await page.evaluate(() => { window.__panel.store.profileLoadedAt = 0; });
    await page.evaluate(() => { window.__one = window.__panel.refresh(); });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/members'), 200, people);
    const olderReload = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    await page.evaluate(() => { window.__panel.store.profileLoadedAt = 0; window.__two = window.__panel.refresh(); });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/members'), 200, people);
    await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/rooms/r1/agents/a1/profile' && !request.answered).length === 2);
    const newerReload = (await pending()).find(index => index !== olderReload);
    await answer(page, newerReload, 200, { cwd: '/newer' });
    await answer(page, olderReload, 200, { cwd: '/older' }); await settle(page);
    assert.deepEqual(await cwd(), ['/newer'], 'an older reload does not replace the newer');
    await page.close();
  }

  // A panel that is destroyed while a read or a call is out takes the answer and does nothing with it: no card, no person, no profile or output,
  // no read that would follow, no error in the panel, and nothing is drawn into the emptied lists.
  {
    const reads = page => page.evaluate(() => window.__requests.filter(request => request.method === 'GET').length);
    const empty = page => page.evaluate(() => [document.querySelectorAll('#agents-list *').length, document.querySelectorAll('#agents-humans *').length, document.querySelector('#agents-error').textContent]);
    // The agent read.
    let page = await mount();
    await page.evaluate(room => { window.__shown = window.__panel.setRoom(room); }, room('r1'));
    const list = await waitRequest(page, 'GET', '/api/rooms/r1/agents');
    await calls(page);
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, list, 200, [agent('a1', 'ada')]); await page.evaluate(() => window.__shown); await settle(page);
    assert.deepEqual([await empty(page), await count(page, 'GET', '/api/rooms/r1/members'), await calls(page)], [[0, 0, ''], 0, []], 'a destroyed panel draws no card, tells nobody and reads no members');
    await page.close();
    // The members read.
    page = await mount();
    await page.evaluate(room => { window.__shown = window.__panel.setRoom(room); }, room('r1'));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada')]);
    const members = await waitRequest(page, 'GET', '/api/rooms/r1/members');
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, members, 200, people); await settle(page);
    assert.deepEqual([await empty(page), await page.evaluate(() => window.__panel.store.humans.value.length)], [[0, 0, ''], 0], 'a destroyed panel keeps and draws no person');
    await page.close();
    // The profile and the Output reads.
    page = await mount({ owner: true, auto: { 'GET /api/rooms/r1/agents/a1/profile': undefined, 'GET /api/rooms/r1/agents/a1/output?lines=200': undefined } });
    await show(page, 'r1', [agent('a1', 'ada')]);
    await page.$eval('[data-agent-id=a1] .agent-expand', node => node.click());
    const profile = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/profile');
    await page.$eval('[data-agent-id=a1] .profile-output summary', node => node.click());
    const output = await waitRequest(page, 'GET', '/api/rooms/r1/agents/a1/output?lines=200');
    const before = await reads(page);
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, profile, 200, { cwd: '/late' }); await answer(page, output, 200, { text: 'late', truncated: false }); await settle(page);
    assert.deepEqual([await empty(page), await reads(page), await page.evaluate(() => [window.__panel.store.profileData.value, window.__panel.store.output.value?.text])], [[0, 0, ''], before, [null, '']], 'a destroyed panel keeps neither the profile nor the output and reads nothing more');
    await page.close();
    // A control call.
    page = await mount();
    await show(page, 'r1', [agent('a1', 'ada')]);
    await page.$eval('[data-agent-id=a1] .agent-act-pause', node => node.click());
    const control = await waitRequest(page, 'POST', '/api/agents/a1/control');
    const total = await page.evaluate(() => window.__requests.length);
    await page.evaluate(() => window.__panel.destroy());
    await answer(page, control, 409, { message: 'The agent changed', error: 'stale' }); await settle(page);
    assert.deepEqual([await empty(page), await page.evaluate(() => window.__requests.length)], [[0, 0, ''], total], 'a destroyed panel shows no refusal and reads nothing');
    await page.close();
    // A storm of agent events does not become a storm of reads: events of other rooms' agents read nothing, and a burst about this room is one read at once and
    // one more after it, however many events there are (each used to start its own agents and members reads).
    page = await mount();
    await show(page, 'r1', [agent('a1', 'ada')]);
    await page.evaluate((list, members) => { window.__auto['GET /api/rooms/r1/agents'] = list; window.__auto['GET /api/rooms/r1/members'] = members; }, [agent('a1', 'ada')], people); // (later reads are answered at once)
    const roomReads = () => page.evaluate(() => window.__requests.filter(request => request.method === 'GET' && /\/api\/rooms\/r1\/(agents|members)$/.test(request.path)).length);
    const readsBefore = await roomReads();
    await page.evaluate(async () => { const source = window.__panel.source; for (let i = 0; i < 40; i++) await source.onEvent({ agent_id: 'other', room_id: 'r2', kind: 'turn_recorded', payload_json: '{}', created_at: '2026-01-01T10:05:00Z' }); });
    await settle(page);
    assert.equal(await roomReads(), readsBefore, 'events about another room\'s agents read nothing');
    await page.evaluate(async () => { const source = window.__panel.source; const events = []; for (let i = 0; i < 60; i++) events.push(source.onEvent({ agent_id: 'a1', room_id: 'r1', kind: 'turn_recorded', payload_json: '{}', created_at: '2026-01-01T10:05:00Z' })); await Promise.all(events); await new Promise(resolve => setTimeout(resolve, 900)); });
    const burst = (await roomReads()) - readsBefore;
    assert.ok(burst >= 2 && burst <= 4, `60 events about this room read the agents and members once, and once more after: ${burst} reads`);
    await page.close();
    // The hover buttons follow the agent's state: an active one has Pause and Stop; one being stopped (stop asked, or the harness interrupting) has only a Stop that is
    // already in effect (it cannot be started yet); a stopped one has Start (reactivate) instead of both; a paused one has Resume (the same action) in place of its disabled Pause, and Stop. One word per action: Start for a stopped agent, Resume for a paused one, never "Reactivate".
    page = await mount();
    await show(page, 'r1', [agent('act', 'active'), agent('stopping', 'stopping', { desired: 'stopped', observed: 'in_turn' }), agent('interrupting', 'interrupting', { desired: 'active', observed: 'interrupting' }),
      agent('stopped', 'stopped', { desired: 'stopped', observed: 'stopped' }), agent('paused', 'paused', { desired: 'not_accepting' })]);
    const quick = await page.evaluate(() => Object.fromEntries([...document.querySelectorAll('#agents-list .agent-card')].map(card => [card.dataset.agentId, [...card.querySelectorAll('.agent-quick button')].map(button => [button.getAttribute('aria-label'), button.disabled, button.querySelector('svg') ? 'svg' : 'no icon']).filter(([label]) => !/privately|Open agent profile/.test(label))])));
    assert.deepEqual(quick, {
      act: [['Pause', false, 'svg'], ['Stop', false, 'svg']],
      stopping: [['Stop', true, 'svg']],
      interrupting: [['Stop', false, 'svg']],
      stopped: [['Start', false, 'svg']],
      paused: [['Resume', false, 'svg'], ['Stop', false, 'svg']],
    }, 'the hover buttons follow the state');
    assert.deepEqual(await page.evaluate(() => Object.fromEntries(['stopped', 'paused'].map(id => [id, document.querySelector(`[data-agent-id=${id}] .agent-reactivate`)]).map(([id, button]) => [id, [button.getAttribute('aria-label'), button.title]]))), { stopped: ['Start', 'Start: run this agent again and accept new work'], paused: ['Resume', 'Resume: accept new work again'] }, 'the profile action is named for the state too');
    // What a person reads: visible text, tooltips and accessible names. (Class names and ids such as agent-reactivate are the action's own name in the code.)
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('#agents-list, #agents-list *')].some(node => /Reactivate/i.test([node.childNodes.length === 1 && node.firstChild.nodeType === 3 ? node.textContent : '', node.getAttribute('title'), node.getAttribute('aria-label')].join(' ')))), false, 'no Reactivate in what the cards show');
    await page.$eval('[data-agent-id=stopped] .agent-start', button => button.click());
    const startRequest = await waitRequest(page, 'POST', '/api/agents/stopped/control');
    assert.equal(await page.evaluate(index => window.__requests[index].body.desired, startRequest), 'active', 'Start asks for the agent to be active again');
    await page.close();
    // An event that reaches a destroyed panel through the source starts no read.
    page = await mount();
    await show(page, 'r1', [agent('a1', 'ada')]);
    const base = await page.evaluate(() => window.__requests.length);
    await page.evaluate(() => { const panel = window.__panel; panel.destroy(); panel.source.changed(); });
    await settle(page);
    assert.equal(await page.evaluate(() => window.__requests.length), base, 'a destroyed panel does not read again');
    await page.close();
  }

  // The members column keeps its header row (the collapse button, the title, Spawn) where it is: only the people and the cards under it scroll.
  {
    const page = await mount();
    await page.setViewport({ width: 1280, height: 600 });
    await show(page, 'r1', Array.from({ length: 24 }, (_, i) => agent(`a${i}`, `agent${i}`)));
    const geometry = () => page.evaluate(() => {
      const panel = document.querySelector('#agents-panel'), scroller = document.querySelector('.agents-scroll'), top = selector => Math.round(document.querySelector(selector).getBoundingClientRect().top);
      return { header: top('#agents-panel > header'), spawn: top('#spawn-open'), scroller: top('.agents-scroll'), scrolls: scroller.scrollHeight > scroller.clientHeight + 1, panelScrolls: panel.scrollHeight > panel.clientHeight + 1, scrollTop: scroller.scrollTop, overflow: getComputedStyle(panel).overflowY };
    });
    const before = await geometry();
    assert.deepEqual([before.scrolls, before.panelScrolls, before.overflow], [true, false, 'hidden'], 'the cards are taller than the column and are what scrolls');
    await page.$eval('.agents-scroll', node => { node.scrollTop = 500; }); await settle(page);
    const after = await geometry();
    assert.ok(after.scrollTop > 200, `the list scrolled: ${after.scrollTop}`);
    assert.deepEqual([after.header, after.spawn, after.scroller, after.panelScrolls], [before.header, before.spawn, before.scroller, false], 'the header row stayed where it was while the list scrolled');
    assert.equal(await page.$eval('#agents-panel header h2', node => node.textContent), 'Members', 'the column is titled Members');
    await page.close();
  }

  // Remove: only a stopped agent can be removed (the button says why otherwise), it asks first, naming the agent, and a refused removal stays on the
  // panel; a removed agent leaves the list once the room is read again.
  {
    const page = await mount();
    await show(page, 'r1', [agent('a1', 'ada'), agent('a2', 'bert', { desired: 'stopped', observed: 'stopped' }), agent('a3', 'cleo', { desired: 'stopped', observed: 'stopped' })]);
    const remove = id => page.$eval(`[data-agent-id=${id}] .agent-remove`, node => ({ disabled: node.disabled, title: node.title, label: node.getAttribute('aria-label'), glyph: node.querySelector('.icon-mask').className }));
    assert.deepEqual(await remove('a1'), { disabled: true, title: 'Remove is unavailable: stop it first', label: 'Remove', glyph: 'icon-mask icon-mask-trash' });
    assert.equal((await remove('a2')).disabled, false, 'a stopped agent can be removed');
    const ask = async id => {
      await page.$eval(`[data-agent-id=${id}] .agent-remove`, node => node.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      return page.evaluate(() => ({ title: document.querySelector('#app-dialog-title').textContent, message: document.querySelector('.app-dialog-message').textContent, ok: document.querySelector('.app-dialog-ok').textContent }));
    };
    // Cancelling sends nothing.
    assert.deepEqual(await ask('a2'), { title: 'Remove agent', message: "Remove bert? Its messages stay; it can't be started again.", ok: 'Remove' });
    await page.$eval('.app-dialog-cancel', node => node.click()); await settle(page);
    assert.equal(await count(page, 'DELETE', '/api/agents/a2'), 0);
    // Confirming removes it, and the room is read again.
    await ask('a2');
    await page.$eval('.app-dialog-ok', node => node.click());
    await answer(page, await waitRequest(page, 'DELETE', '/api/agents/a2'), 200, { agent_id: 'a2', slug: 'bert', worktree: {} });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/agents'), 200, [agent('a1', 'ada'), agent('a3', 'cleo', { desired: 'stopped', observed: 'stopped' })].map(item => ({ ...item, room_id: 'r1' })));
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r1/members'), 200, people); await settle(page);
    assert.deepEqual((await cards(page)).map(row => row[0]), ['a1', 'a3']);
    // A refusal is said on the panel and the card stays.
    await ask('a3');
    await page.$eval('.app-dialog-ok', node => node.click());
    await answer(page, await waitRequest(page, 'DELETE', '/api/agents/a3'), 409, { error: 'agent_not_stopped', message: 'stop cleo first: only a stopped agent can be removed' }); await settle(page);
    assert.equal(await page.$eval('#agents-error', node => node.textContent), 'stop cleo first: only a stopped agent can be removed');
    assert.deepEqual((await cards(page)).map(row => row[0]), ['a1', 'a3']);
    await page.close();
  }

  // On a collapsed card the state pill of an idle agent says just "idle" (how long is detail for the open card); a working agent keeps its time, and an open card shows it too.
  {
    const page = await mount();
    await show(page, 'r1', [agent('t1', 'idle1', { last_activity_at: new Date(Date.now() - 31 * 60000).toISOString() }), agent('t2', 'busy1', { observed: 'in_turn', activity_at: new Date(Date.now() - 90000).toISOString() })]);
    const pill = id => page.$eval(`[data-agent-id=${id}] .agent-state-pill`, node => node.textContent.replace(/\s+/g, ' ').trim());
    assert.deepEqual([await pill('t1'), /^working \d/.test(await pill('t2'))], ['idle', true], 'a collapsed idle card says no time, a working one does');
    await page.$eval('[data-agent-id=t1] .agent-name', node => node.click()); await settle(page);
    assert.match(await pill('t1'), /^idle \d/, 'an open idle card shows how long');
    await page.$eval('[data-agent-id=t1] .agent-name', node => node.click()); await settle(page);
    assert.equal(await pill('t1'), 'idle', 'and a collapsed one again says just idle');
    await page.close();
  }

  // An agent's one status dot says online or offline and warm. Green is online and cold, green with a ring that pulses is online and warm (v1's presence dot),
  // red is offline, orange is a state not known (a paused or waiting agent, a warm state the supervisor did not tell). No icon and no second dot, in every theme.
  {
    const page = await mount();
    const soon = new Date(Date.now() + 4 * 60000 + 30000).toISOString();
    await show(page, 'r1', [agent('f1', 'warm1', { tool: 'claude', warm: { state: 'warm', sleeps_at: soon } }), agent('f2', 'cold1', { tool: 'claude', warm: { state: 'sleeping' } }),
      agent('f3', 'work1', { tool: 'claude', observed: 'in_turn' }), agent('f4', 'unk1', { tool: 'claude', warm: { state: 'unknown' } }), agent('f5', 'off1', { tool: 'claude', desired: 'stopped', observed: 'stopped' }),
      agent('f6', 'pause1', { tool: 'claude', desired: 'not_accepting' }), agent('f7', 'plain1', { tool: 'codex' })]);
    const read = id => page.$eval(`[data-agent-id=${id}]`, node => {
      const dot = node.querySelector('.agent-dot'), style = getComputedStyle(dot), words = node.querySelector('.dot-words'), sr = node.querySelector('.dot-sr');
      return { kind: dot.dataset.dot, background: style.backgroundColor, ring: style.boxShadow !== 'none', animation: style.animationName, size: [Math.round(dot.getBoundingClientRect().width), Math.round(dot.getBoundingClientRect().height)],
        tooltip: words.title, spoken: sr.textContent, hiddenVisually: sr.getBoundingClientRect().width <= 1,
        icons: [...node.querySelectorAll('.warm-badge, .warm-sr, .icon-mask-warm, .icon-mask-cold')].length, dots: node.querySelectorAll('.agent-online-dot').length };
    });
    const colours = () => page.evaluate(() => {
      const root = getComputedStyle(document.documentElement), probe = value => { const el = document.createElement('i'); el.style.color = value; document.body.append(el); const c = getComputedStyle(el).color; el.remove(); return c; };
      return { green: probe(root.getPropertyValue('--green-text')), orange: probe(root.getPropertyValue('--orange')), red: probe(root.getPropertyValue('--red')) };
    });
    for (const theme of ['dark', 'light', 'high-contrast-dark', 'high-contrast-light']) {
      await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      const want = await colours();
      const states = Object.fromEntries(await Promise.all(['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7'].map(async id => [id, await read(id)])));
      assert.deepEqual(['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7'].map(id => states[id].kind), ['warm', 'cold', 'working', 'unknown', 'offline', 'unknown', 'cold'], `${theme}: what each card's dot says`);
      assert.deepEqual(['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7'].map(id => states[id].background), [want.green, want.green, want.green, want.orange, want.red, want.orange, want.green], `${theme}: green online, orange not known, red offline, from the theme`);
      assert.deepEqual(['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7'].map(id => states[id].ring), [true, false, false, false, false, false, false], `${theme}: only a warm dot has the ring`);
      assert.equal(states.f1.animation, 'presence-pulse', `${theme}: and it pulses`);
      assert.deepEqual(['f2', 'f3', 'f4', 'f5'].map(id => states[id].animation), Array(4).fill('none'), `${theme}: nothing else moves`);
      assert.deepEqual(['f1', 'f2', 'f5'].map(id => states[id].size.join('x')), Array(3).fill('8x8'), `${theme}: every dot is the same size, so nothing shifts when a card turns warm`);
      assert.deepEqual([Object.values(states).map(item => item.icons), Object.values(states).map(item => item.dots)], [Array(7).fill(0), Array(7).fill(1)], `${theme}: no icon, one dot and no other warm words`);
    }
    await page.evaluate(() => { document.documentElement.removeAttribute('data-theme'); });
    const f1 = await read('f1');
    assert.match(f1.tooltip, /^Online \u00b7 warm, the next answer skips the start \(\u2248[45] min left\)$/, 'the warm dot says so in its tooltip');
    assert.deepEqual([f1.spoken === f1.tooltip, f1.hiddenVisually], [true, true], 'and in words for a screen reader, hidden visually');
    assert.equal((await read('f2')).tooltip, 'Online \u00b7 cold, the next answer starts the agent');
    assert.match((await read('f3')).tooltip, /^Online \u00b7 working/);
    assert.match((await read('f4')).tooltip, /^State unknown/, 'an unknown warm state says so');
    assert.match((await read('f5')).tooltip, /^Offline/);
    assert.equal((await read('f6')).tooltip, 'Paused; no new work is accepted.', 'a paused agent is orange and says it is paused');
    // Reduced motion: the ring stays, nothing moves.
    await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
    let reduced = await read('f1');
    assert.deepEqual([reduced.ring, reduced.animation], [true, 'none'], 'under reduced motion the warm ring stays and does not pulse');
    await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'no-preference' }]);
    reduced = await read('f1');
    assert.deepEqual([reduced.ring, reduced.animation], [true, 'presence-pulse'], 'and pulses again when motion is allowed');
    // The minutes in the tooltip follow the clock, with no request of its own.
    const before = await count(page, 'GET', '/api/rooms/r1/agents');
    await page.evaluate(() => { window.__panel.store.now.value = Date.now() + 3 * 60000; });
    await settle(page);
    assert.match((await read('f1')).tooltip, /\u2248[12] min left\)$/, 'three minutes later the tooltip counts fewer minutes');
    assert.equal(await count(page, 'GET', '/api/rooms/r1/agents'), before, 'no new request for it');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('agents page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
