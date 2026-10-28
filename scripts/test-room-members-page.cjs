// The Members section of the room dialog in Chromium with every request held until the test answers it. web/ is served as is with its import map
// and markup, without app.js; no Go server. It runs unchanged against the page that replaced the hand-written list. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const MODULE = fs.existsSync(path.join(root, 'js', 'pages', 'members', 'page.js')) ? '/js/pages/members/page.js' : '/js/room-members.js';
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const members = {
  humans: [{ login: 'owner', role: 'owner' }, { login: 'bob', role: '' }],
  agents: [
    { slug: 'ada', role_emoji: '🔧', role_label: 'Worker', role_key: 'worker', desired: 'active', observed: 'in_turn' },
    { slug: 'bert', role_key: 'reviewer', singleton: true, desired: 'active', observed: 'idle' },
    { slug: 'cleo', role_label: 'Worker', desired: 'not_accepting', observed: 'idle' },
    { slug: 'dara', role_label: 'Worker', desired: 'stopped', observed: 'stopped', role_deleted: true },
    { slug: 'eli', role_label: 'Worker', desired: 'active', observed: 'lost' },
    { slug: 'gus', role_label: 'Worker', desired: 'stopped', observed: 'in_turn' },
    { slug: 'ivy', role_label: 'Worker', desired: 'active', observed: 'interrupting' },
    { slug: 'old', role_label: 'Worker', revoked: true, desired: 'stopped', observed: 'stopped' }],
  singletons: [{ role_key: 'lead', label: 'Lead', emoji: '🧭', held_by: { slug: 'zeno', in_this_room: true } }, { role_key: 'reviewer', held_by: { slug: 'rex', in_this_room: false } }, { role_key: 'tester', label: 'Tester', held_by: null }],
};

function installFetch() {
  window.__requests = [];
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async () => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch);
    await page.evaluate(async module => {
      const { RoomMembers } = await import(module);
      window.__members = new RoomMembers(document.querySelector('#room-member-list'));
    }, MODULE);
    return page;
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const load = (page, id) => page.evaluate(id => { window.__loading = window.__members.load(id); }, id);
  const status = page => page.$eval('#room-member-list .room-members-status', node => node.textContent);
  const groups = page => page.$$eval('#room-member-list .room-members-body > *', nodes => nodes.map(node => (node.className === 'room-members-group'
    ? [node.querySelector('h4').textContent, node.querySelector('ul').getAttribute('aria-label'), [...node.querySelectorAll('li')].map(item => [...item.children].map(child => `${child.className}:${child.textContent}`))]
    : [node.className, node.textContent])));

  // The list says it is loading, then shows the people with their roles, the agents with their role and state words and the singleton roles with
  // who holds them; everything the server sends is text.
  {
    const page = await mount();
    await load(page, 'r1');
    assert.equal(await status(page), 'Loading members…');
    assert.equal(await page.evaluate(() => window.__requests[0].path), '/api/rooms/r1/members');
    await answer(page, 0, 200, members); await page.evaluate(() => window.__loading);
    assert.equal(await status(page), '');
    const [people, agents, singletons] = await groups(page);
    assert.deepEqual(people, ['People (2)', 'People in this room', [['room-member-name:owner', 'room-member-role:owner'], ['room-member-name:bob']]]);
    assert.equal(agents[0], 'Agents (8)'); assert.equal(agents[1], 'Agents in this room');
    assert.deepEqual(agents[2][0], ['room-member-emoji:🔧', 'room-member-name:ada', 'room-member-role:Worker', 'room-member-state:Active · Working']);
    assert.deepEqual(agents[2].slice(1).map(row => row.map(cell => cell.replace(/^[a-z-]+:/, ''))), [
      ['bert', 'reviewer', 'singleton', 'Active · Idle'], ['cleo', 'Worker', 'Not accepting · Idle'], ['dara', 'Worker', 'role removed', 'Stopped · Stopped'], ['eli', 'Worker', 'Active · Lost'],
      ['gus', 'Worker', 'Interrupt requested · Working'], ['ivy', 'Worker', 'Interrupt requested · Interrupting'], ['old', 'Worker', 'Revoked']]);
    assert.deepEqual(singletons, ['Singleton roles (3)', 'Singleton roles', [['room-member-emoji:🧭', 'room-member-name:Lead', 'room-member-state:held by zeno'], ['room-member-name:reviewer', 'room-member-state:held by rex (another room)'], ['room-member-name:Tester', 'room-member-state:vacant']]]);
    assert.equal(await page.$eval('#room-member-list .room-member-emoji', node => node.getAttribute('aria-hidden')), 'true');
    await page.close();
  }

  // A room with no agents says so and has no singleton group; a failure is shown in the status line with the server's words, or a fallback.
  {
    const page = await mount();
    await load(page, 'r1'); await answer(page, 0, 200, { humans: [{ login: 'owner', role: 'owner' }], agents: [], singletons: [] }); await page.evaluate(() => window.__loading);
    assert.deepEqual((await groups(page)).map(group => group[0]), ['People (1)', 'room-members-empty']);
    assert.equal((await groups(page))[1][1], 'No agents in this room.');
    await load(page, 'r1');
    assert.deepEqual([await status(page), (await groups(page)).length], ['Loading members…', 0], 'a new load clears the old list while it waits');
    await answer(page, 1, 500, { message: 'The member list is down' }); await page.evaluate(() => window.__loading);
    assert.deepEqual([await status(page), (await groups(page)).length], ['The member list is down', 0]);
    await load(page, 'r1'); await answer(page, 2, 500, {}); await page.evaluate(() => window.__loading);
    assert.equal(await status(page), 'Request failed (500)');
    await page.close();
  }

  // Only the newest load draws: a slow answer for a room that was replaced, an answer after reset(), and a failure of an earlier load all do nothing.
  {
    const page = await mount();
    await load(page, 'r1'); await load(page, 'r2');
    await answer(page, 1, 200, { humans: [{ login: 'second', role: 'owner' }], agents: [], singletons: [] });
    await answer(page, 0, 200, { humans: [{ login: 'first', role: 'owner' }], agents: [], singletons: [] });
    await settle(page);
    assert.deepEqual((await groups(page))[0][2], [['room-member-name:second', 'room-member-role:owner']]);
    await load(page, 'r1'); await load(page, 'r2');
    await answer(page, 3, 200, { humans: [{ login: 'kept', role: 'owner' }], agents: [], singletons: [] });
    await answer(page, 2, 500, { message: 'late failure' }); await settle(page);
    assert.deepEqual([await status(page), (await groups(page))[0][2][0][0]], ['', 'room-member-name:kept']);
    await load(page, 'r3');
    await page.evaluate(() => window.__members.reset());
    assert.deepEqual([await status(page), (await groups(page)).length], ['', 0]);
    await answer(page, 4, 200, members); await settle(page);
    assert.deepEqual([await status(page), (await groups(page)).length], ['', 0], 'an answer after reset draws nothing');
    await load(page, 'r4'); await page.evaluate(() => window.__members.reset()); await answer(page, 5, 500, { message: 'after reset' }); await settle(page);
    assert.equal(await status(page), '', 'a failure after reset shows nothing');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('room members page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
