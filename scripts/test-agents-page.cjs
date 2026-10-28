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
    const row = id => page.$eval(`[data-agent-id=${id}]`, node => ({ activity: node.querySelector('.agent-activity').textContent, dot: node.querySelector('.agent-dot').dataset.state,
      pause: [node.querySelector('.agent-act-pause').disabled, node.querySelector('.agent-act-pause').title], stop: node.querySelector('.agent-act-stop').disabled, reactivate: node.querySelector('.agent-reactivate').disabled,
      hint: [...node.querySelectorAll('.agent-action-hint:not([hidden])')].map(hint => hint.textContent) }));
    const working = await row('a1'), stopped = await row('a2'), paused = await row('a3');
    assert.equal(working.activity, 'Working…'); assert.equal(working.dot, 'working'); assert.deepEqual(working.pause, [false, 'Pause: finish the current turn, then accept no new work']);
    assert.deepEqual([stopped.pause[0], stopped.stop, stopped.reactivate], [true, true, false]);
    assert.deepEqual(stopped.hint, ['Pause is unavailable: it is stopped; reactivate it first.', 'Stop is unavailable: it is already stopped.', 'Switch is unavailable: reactivate it first.']);
    assert.equal(paused.pause[1], 'Pause is unavailable: it is already paused');
    assert.deepEqual((await calls(page)).filter(call => call[0] === 'change'), [['change', ''], ['change', 'a1,a2,a3']]);
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

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('agents page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
