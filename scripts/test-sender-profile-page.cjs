// The read-only profile of a sender that is no longer a live member (imported history or an agent that left) in Chromium with every request held
// until the test answers it. web/ is served as is with its import map and markup, without app.js; no Go server. It runs unchanged against the page
// that replaced the hand-written dialog. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const MODULE = fs.existsSync(path.join(root, 'js', 'pages', 'sender-profile', 'page.js')) ? '/js/pages/sender-profile/page.js' : '/js/sender-profile.js';
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const start = { resumable: true, cwd: '/work/old', outside_folder: true, v1_active: true };
const messages = [
  { id: 'm1', seq: 2, author_kind: 'agent', author_id: 'old-bot', author_model: 'sonnet4.6', author_harness: 'codex', created_at: '2026-01-01T10:06:00Z' },
  { id: 'm2', seq: 1, author_kind: 'agent', author_id: 'old-bot', created_at: '2026-01-01T10:00:00Z' },
  { id: 'm3', seq: 3, author_kind: 'user', author_id: 'old-bot', created_at: '2026-01-01T10:07:00Z' },
  { id: 'm4', seq: 4, author_kind: 'agent', author_id: 'other', created_at: '2026-01-01T10:08:00Z' }];

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
    await page.evaluate(async ({ module, messages }) => {
      window.__sender = await import(module);
      window.__messages = messages; window.__calls = [];
      const button = document.createElement('button'); button.id = 'invoker'; button.textContent = 'old-bot'; document.body.append(button);
    }, { module: MODULE, messages });
    return page;
  };
  const openProfile = (page, patch = {}) => page.evaluate(patch => {
    const options = { name: 'old-bot', authorID: 'old-bot', kind: 'historical', roomID: 'r1', messages: window.__messages, identity: { icon: '/icons/human.svg', title: 'sonnet4.6 · Codex (historical)' },
      onStartAgent: start => window.__calls.push(['start', start]), onOpenAgent: id => window.__calls.push(['open', id]), ...patch };
    document.querySelector('#invoker').focus();
    window.__opening = window.__sender.openSenderProfile(options);
  }, patch);
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const dialog = page => page.evaluate(() => { const d = document.querySelector('#sender-profile-dialog'); return d && { open: d.open, title: d.querySelector('h2').textContent, badge: d.querySelector('.sender-profile-badge').textContent, badgeClass: d.querySelector('.sender-profile-badge').className,
    facts: [...d.querySelectorAll('.profile-fact')].map(row => [row.querySelector('.profile-fact-label').textContent, row.querySelector('.profile-fact-value').textContent.replace(/ · .*$/, ' · …')]),
    note: d.querySelector(':scope > .sender-profile-note')?.textContent, extra: [...d.querySelectorAll('.sender-profile-more .profile-fact-label')].map(node => node.textContent), start: d.querySelector('.sender-profile-start') ? d.querySelector('.sender-profile-start').textContent : null,
    labelled: d.getAttribute('aria-labelledby'), icon: d.querySelector('.sender-profile-icon').getAttribute('src') }; });
  const gone = async page => { await settle(page); return (await page.$('#sender-profile-dialog')) === null; };
  const calls = page => page.evaluate(() => window.__calls.splice(0));

  // What the loaded messages say about one sender: its own agent messages in order of seq, with the newest model and harness named.
  {
    const page = await mount();
    const facts = await page.evaluate(() => window.__sender.senderFacts(window.__messages, 'old-bot'));
    assert.deepEqual(facts, { count: 2, first: '2026-01-01T10:00:00Z', last: '2026-01-01T10:06:00Z', model: 'sonnet4.6', harness: 'codex' });
    assert.deepEqual(await page.evaluate(() => window.__sender.senderFacts([], 'nobody')), { count: 0, first: '', last: '', model: '', harness: '' });
    await page.close();
  }

  // A historical sender: the facts from its messages at once, the session facts and the offer to start it once the profile is read.
  {
    const page = await mount();
    await openProfile(page);
    const first = await dialog(page);
    assert.deepEqual([first.open, first.title, first.badge, first.badgeClass, first.labelled, first.icon], [true, 'old-bot', 'Historical (imported)', 'sender-profile-badge sender-profile-historical', 'sender-profile-title', '/icons/human.svg']);
    assert.deepEqual(first.facts.map(fact => fact[0]), ['Runtime', 'Model', 'Harness', 'First heard', 'Last heard', 'Messages loaded']);
    assert.deepEqual(first.facts[0], ['Runtime', 'sonnet4.6 · …']);
    assert.equal(first.facts.at(-1)[1], '2');
    assert.equal(await page.evaluate(() => window.__requests[0].path), '/api/rooms/r1/agents/old-bot/profile');
    assert.equal(first.note, '', 'the note waits for the profile');
    await answer(page, 0, 200, { native_session_id: 'v1-sess', resume_command: 'codex resume v1-sess', cwd: '/work/old', imported: true, last_seen: '2026-01-01T10:00:00Z', project_name: 'Alpha', start });
    await page.evaluate(() => window.__opening);
    const full = await dialog(page);
    assert.deepEqual(full.extra, ['Session', 'Resume', 'CWD', 'Project', 'Last active in v1']);
    assert.equal(full.note, 'This sender comes from imported v1 history.');
    assert.match(full.start, /Start as v2 agent/); assert.match(full.start, /Continues the v1 session in \/work\/old\. That folder is outside the project folder; the owner approves it first\. v1 saw it a moment ago/);
    assert.deepEqual(await page.$eval('#sender-profile-start', node => [node.title, node.getAttribute('aria-label'), node.querySelector('.icon-mask-play') !== null]), ['Start as v2 agent', 'Start as v2 agent', true], 'the start button carries its tooltip and name');
    await page.$eval('#sender-profile-start', node => node.click());
    assert.deepEqual(await calls(page), [['start', { ...start, legacy_id: 'old-bot' }]]);
    assert.equal(await gone(page), true, 'starting closes the profile');
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('#invoker')), true, 'the focus returns to the name that opened it');
    await page.close();
  }

  // A sender already started as a v2 agent says so and points at the agent; one with no session says that only what it wrote is shown; a start
  // that cannot be made says why, and without a handler the button is off.
  {
    const page = await mount();
    await openProfile(page); await answer(page, 0, 200, { imported: true, start: { started_agent_id: 'agt_x', started_agent_slug: 'ada' } }); await page.evaluate(() => window.__opening);
    assert.match((await dialog(page)).start, /Running as v2 agent ada\./);
    await page.$eval('.sender-profile-open-agent', node => node.click());
    assert.deepEqual(await calls(page), [['open', 'agt_x']]);
    assert.equal(await gone(page), true);
    await openProfile(page); await answer(page, 1, 200, { imported: true, start: { resumable: false, reason: 'No session was kept.' } }); await page.evaluate(() => window.__opening);
    assert.match((await dialog(page)).start, /Start as v2 agent\s*No session was kept\./);
    assert.equal(await page.$eval('#sender-profile-start', node => node.disabled), false);
    await openProfile(page, { onStartAgent: null }); await answer(page, 2, 200, { imported: true, start: { resumable: false, reason: 'x' } }); await page.evaluate(() => window.__opening);
    assert.equal(await page.$eval('#sender-profile-start', node => node.disabled), true);
    await openProfile(page); await answer(page, 3, 404, { message: 'no profile' }); await page.evaluate(() => window.__opening);
    assert.equal((await dialog(page)).note, 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.');
    await openProfile(page, { roomID: '' }); await page.evaluate(() => window.__opening);
    assert.equal(await page.evaluate(() => window.__requests.length), 4, 'with no room nothing is read');
    assert.match((await dialog(page)).note, /no session was kept/);
    await page.close();
  }

  // A departed agent: a plain note, the session facts the server still has and no offer to start it.
  {
    const page = await mount();
    await openProfile(page, { name: 'Agent GONE00', authorID: 'agt_GONE00', kind: 'departed' });
    const first = await dialog(page);
    assert.deepEqual([first.badge, first.badgeClass, first.note], ['Departed', 'sender-profile-badge sender-profile-departed', 'This agent is no longer in the room.']);
    await answer(page, 0, 200, { native_session_id: 'sess', cwd: '/work', branch: 'main', machine_name: 'Host', spawned_by: 'owner' }); await page.evaluate(() => window.__opening);
    const full = await dialog(page);
    assert.deepEqual(full.extra, ['Session', 'CWD', 'Branch', 'Machine', 'Spawned']);
    assert.deepEqual([full.start, full.note], [null, 'This agent is no longer in the room.']);
    await page.close();
  }

  // The close button and Escape remove the dialog and give the focus back; a profile answered after the dialog was closed, or after another
  // sender's profile opened over it, draws into neither.
  {
    const page = await mount();
    await openProfile(page); await page.$eval('.sender-profile-close', node => node.click());
    assert.equal(await gone(page), true);
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('#invoker')), true);
    await openProfile(page); await page.keyboard.press('Escape'); await settle(page);
    assert.equal(await gone(page), true);
    const heldClosed = await page.evaluate(() => window.__requests.length - 1);
    await answer(page, heldClosed, 200, { cwd: '/late', start }); await settle(page);
    assert.equal(await page.evaluate(() => window.__opening.then(dialog => dialog === undefined)), true, 'the call that was left resolves to nothing');
    assert.equal(await gone(page), true, 'an answer after the dialog went draws nothing');
    assert.deepEqual(await calls(page), []);
    await openProfile(page, { name: 'first', authorID: 'first' });
    const first = await page.evaluate(() => window.__requests.length - 1);
    await openProfile(page, { name: 'second', authorID: 'second' });
    const second = await page.evaluate(() => window.__requests.length - 1);
    assert.equal(await page.$$eval('#sender-profile-dialog', nodes => nodes.length), 1, 'a new profile replaces the open one');
    await answer(page, first, 200, { cwd: '/first', start }); await settle(page);
    assert.deepEqual([(await dialog(page)).title, (await dialog(page)).extra], ['second', []], 'the first sender\'s answer fills nothing in the second');
    await answer(page, second, 200, { cwd: '/second' }); await settle(page);
    assert.deepEqual((await dialog(page)).extra, ['CWD']);
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('sender profile page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
