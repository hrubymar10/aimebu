// The System events panel (the bell) in Chromium on the real markup and styles of web/ (no app.js, no Go server): it shows only the notable events, one line each
// (what happened, which agent, which room, how long ago) with the detail behind a click or Enter, a start/stop/pause is one row naming who did it, and the bell's
// badge counts only what is shown. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const at = new Date().toISOString();
const event = (seq, kind, payload = {}, extra = {}) => ({ id: `evt_${seq}`, seq, kind, payload_json: JSON.stringify(payload), created_at: at, agent_id: 'agt_1', room_id: 'r1', names: { agt_1: 'owen@test', usr_1: 'wren' }, notable: true, ...extra });
// What the server answers to `?notable=1`: the notable events only (it decides which; the page keeps no list), newest first. A control that came from a recovery decision
// names its actor `actor`.
const history = [
  event(9, 'agent_control_requested', { desired: 'stopped', actor_kind: 'user', actor_id: 'usr_1' }),
  event(7, 'agent_spawn_failed', { error: 'folder not found' }),
  event(6, 'turn_process_exit', { exit_code: '137', evidence: 'Process exited with code 137.' }),
  event(5, 'agent_control_requested', { desired: 'active', actor: 'usr_1' }),
];

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  try {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width: 1100, height: 800 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; });
    await page.evaluate(async history => {
      window.__urls = [];
      window.fetch = async input => { window.__urls.push(String(input)); return new Response(JSON.stringify({ events: history, kinds: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } }); };
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner: true, admin: true });
      const { SystemEvents } = await import('/js/system-events.js');
      window.__events = new SystemEvents({ agentName: () => '', roomTitle: id => (id === 'r1' ? 'general' : '') });
      await window.__events.load();
    }, history);
    const rows = () => page.evaluate(() => [...document.querySelectorAll('#system-events-popover .system-event-row')].map(row => ({ kind: row.dataset.kind, text: row.querySelector('.system-event-text').textContent, where: row.querySelector('.system-event-where')?.textContent || '', expandable: row.classList.contains('system-event-expandable'), detailsShown: !!row.querySelector('.system-event-details') && !row.querySelector('.system-event-details').hidden })));
    await page.click('#system-events-toggle'); await page.waitForSelector('#system-events-popover:not([hidden]) .system-event-row');
    // The page asks the server for the notable events (and nothing else decides), newest first: the two controls (one from a recovery decision) and the failures.
    assert.deepEqual(await page.evaluate(() => window.__urls), ['/api/system-events?limit=100&notable=1'], 'the history is the server\'s notable feed');
    assert.deepEqual((await rows()).map(row => [row.kind, row.text]), [
      ['agent_control_requested', 'owen@test: stopped by wren'], ['agent_spawn_failed', 'Agent spawn failed'], ['turn_process_exit', 'Turn process exited'], ['agent_control_requested', 'owen@test: started by wren'],
    ], 'what the server sent, one row each');
    // One line each: the line holds what happened, the agent and room, and the time, and its height is one text line; the detail is behind the row.
    const shape = await page.evaluate(() => [...document.querySelectorAll('#system-events-popover .system-event-row')].map(row => { const line = row.querySelector('.system-event-line'), style = getComputedStyle(line), text = row.querySelector('.system-event-main'); return { lines: Math.round(line.getBoundingClientRect().height / parseFloat(getComputedStyle(row).lineHeight)), nowrap: getComputedStyle(text).whiteSpace, hasTime: !!row.querySelector('time'), where: row.querySelector('.system-event-where')?.textContent, flex: style.display }; }));
    assert.ok(shape.every(row => row.lines <= 1 && row.nowrap === 'nowrap' && row.hasTime && row.flex === 'flex'), `each row is one line: ${JSON.stringify(shape)}`);
    assert.deepEqual(shape.map(row => row.where), ['general', 'owen@test · general', 'owen@test · general', 'general'], 'the agent and the room, after the title');
    // The detail is hidden until the row is clicked (or Enter / Space on it); a row with no detail is not a button.
    const before = await rows();
    assert.deepEqual(before.map(row => [row.expandable, row.detailsShown]), [[false, false], [true, false], [true, false], [false, false]], 'details start folded');
    await page.click('.system-event-row[data-kind=turn_process_exit]');
    const clicked = await page.evaluate(() => { const row = document.querySelector('.system-event-row[data-kind=turn_process_exit]'); return { open: !row.querySelector('.system-event-details').hidden, expanded: row.getAttribute('aria-expanded'), text: row.querySelector('.system-event-details').textContent }; });
    assert.deepEqual(clicked, { open: true, expanded: 'true', text: 'exit code: 137' }, 'a click shows the exit code');
    await page.focus('.system-event-row[data-kind=agent_spawn_failed]'); await page.keyboard.press('Enter');
    assert.equal(await page.$eval('.system-event-row[data-kind=agent_spawn_failed] .system-event-details', el => [el.hidden, el.textContent].join('|')), 'false|error: folder not found', 'Enter does too');
    await page.keyboard.press(' ');
    assert.equal(await page.$eval('.system-event-row[data-kind=agent_spawn_failed] .system-event-details', el => el.hidden), true, 'and Space folds it again');
    // The bell's badge counts only what is shown: a live event is listed and counted when the server marked it `notable`, and not otherwise.
    await page.click('#system-events-toggle'); // close
    await page.evaluate(() => { const live = (seq, kind, payload = {}, notable = false) => ({ id: `evt_live_${seq}`, seq, kind, payload_json: JSON.stringify(payload), created_at: new Date().toISOString(), agent_id: 'agt_1', room_id: 'r1', names: { agt_1: 'owen@test' }, ...(notable ? { notable: true } : {}) });
      const feed = window.__events; feed.reset();
      for (const item of [live(1, 'turn_process_exit', { exit_code: '0' }), live(2, 'agent_key_rotated'), live(3, 'agent_control_applied'), live(4, 'room_renamed')]) feed.onEvent(item);
      window.__routine = document.querySelector('#system-events-count').hidden;
      for (const item of [live(5, 'turn_process_exit', { exit_code: '1' }, true), live(6, 'agent_spawn_failed', {}, true), live(7, 'agent_turn_budget_exceeded', {}, true)]) feed.onEvent(item); });
    assert.deepEqual([await page.evaluate(() => window.__routine), await page.$eval('#system-events-count', el => [el.hidden, el.textContent])], [true, [false, '3']], 'routine events leave the badge alone; each notable one counts');
    // The same alert again and again is one row with a count, and names whose login it is; a phone gets its own close control and the sheet ends above the tab bar.
    await page.evaluate(() => { const feed = window.__events; feed.reset(); for (let seq = 1; seq <= 4; seq++) feed.onEvent({ id: `evt_cred_${seq}`, seq, kind: 'credential_alert', payload_json: JSON.stringify({ kind: 'expiring', detail: 'claude/main: the access token expires in 4 min.' }), created_at: new Date().toISOString(), names: {}, notable: true }); });
    await page.click('#system-events-toggle'); await page.waitForSelector('#system-events-popover:not([hidden]) .system-event-row');
    // While the panel is open its button in the top bar is the marked one, as the Inbox's is.
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 500))); // (a button's colours fade over a moment)
    assert.equal(await page.evaluate(() => { const toggle = document.querySelector('#system-events-toggle'), probe = document.createElement('i'); probe.style.color = 'var(--accent)'; document.body.append(probe); const accent = getComputedStyle(probe).color; probe.remove(); return getComputedStyle(toggle).borderTopColor === accent && getComputedStyle(toggle).color === accent; }), true, 'the open panel marks its top-bar button');
    assert.deepEqual(await page.$$eval('#system-events-popover .system-event-row', rows => rows.map(row => [row.querySelector('.system-event-text').textContent, row.querySelector('.system-event-count')?.textContent, row.querySelector('.system-event-where')?.textContent])), [['Credential alert: expiring', '×4', 'claude/main']], 'identical alerts are one row with a count and the account');
    assert.equal(await page.$eval('.system-events-close', el => getComputedStyle(el).display), 'none', 'a desktop popover closes by a click outside, with no close control');
    await page.setViewport({ width: 390, height: 780 });
    assert.deepEqual(await page.evaluate(() => { const close = document.querySelector('.system-events-close'), panel = document.querySelector('#system-events-popover').getBoundingClientRect(); return [getComputedStyle(close).display !== 'none', close.getBoundingClientRect().width >= 36, panel.bottom <= innerHeight - 70]; }), [true, true, true], 'a phone has a close control, and the sheet leaves the bottom tab bar free');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 500))); // (colours fade over a moment)
    assert.equal(await page.evaluate(() => { const toggle = document.querySelector('#system-events-toggle'), probe = document.createElement('i'); probe.style.color = 'var(--accent)'; document.body.append(probe); const accent = getComputedStyle(probe).color; probe.remove(); return getComputedStyle(toggle).borderTopColor === accent; }), true, 'the open sheet marks its button on a phone too');
    await page.click('.system-events-close');
    assert.equal(await page.$eval('#system-events-popover', el => el.hidden), true, 'the close control closes the sheet');
    await page.setViewport({ width: 1100, height: 800 });
    // Nothing notable: the panel says so.
    await page.evaluate(() => { window.__events.reset(); }); await page.click('#system-events-toggle');
    assert.equal(await page.$eval('#system-events-popover .system-events-empty', el => el.textContent), 'Nothing needs a look.');
    assert.deepEqual(errors, [], `the page reported errors: ${errors.join(' | ')}`);
    await page.close();
  } finally { await browser.close(); server.close(); }
  console.log('system events page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
