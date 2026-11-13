// The room settings' Schedules section in Chromium with every request held until the test answers it. web/ is served as is with its import map and markup, without
// app.js; no Go server. Part of `make test-ui`.
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


const roomOf = (id, patch = {}) => ({ id, project_id: 'p1', kind: 'task', title: id, lifecycle: 'open', ...patch });
const item = (id, patch = {}) => ({ id, room_id: 'r1', body: `note ${id}`, kind: 'interval', timezone: 'Europe/Prague', enabled: true, rev: 1, target_lead: true, target_agent_id: '', next_run_at: '2026-01-01T10:00:00Z', ...patch });

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
    await page.evaluate(installFetch, {});
    await page.evaluate(async () => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner: true, admin: true });
      const { SchedulesPanel } = await import('/js/pages/room-panel/schedules.js');
      document.querySelector('#room-info-dialog').show();
      window.__panel = new SchedulesPanel(document.querySelector('#room-schedules'));
    });
    return page;
  };
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const load = async (page, room, schedules = [], agents = [{ id: 'a1', slug: 'ada', role_key: 'worker' }]) => {
    await page.evaluate(room => { window.__loading = window.__panel.load(room); }, room);
    if (room.kind !== 'task' && room.kind !== 'home') { await page.evaluate(() => window.__loading); return; }
    await answer(page, await waitRequest(page, 'GET', `/api/rooms/${room.id}/schedules`), 200, { schedules });
    await answer(page, await waitRequest(page, 'GET', `/api/rooms/${room.id}/agents`), 200, agents);
    await page.evaluate(() => window.__loading); await settle(page);
  };
  const view = page => page.evaluate(() => {
    const root = document.querySelector('#room-schedules'), shown = selector => [...root.querySelectorAll(selector)].filter(el => !el.hidden).length;
    return { hidden: root.hidden, list: root.querySelector('.schedule-list').textContent, rows: root.querySelectorAll('.schedule-row').length, error: root.querySelector('.schedule-error').textContent,
      fields: { once: shown('.schedule-once'), interval: shown('.schedule-interval'), civil: shown('.schedule-civil'), weekly: shown('.schedule-weekly') }, submit: root.querySelector('button[type=submit]').disabled,
      targets: [...root.querySelectorAll('[name=target] option')].map(option => [option.value, option.textContent]), zone: root.querySelector('[name=timezone]').value };
  });

  // zonedOnce: a wall-clock time in a named zone, and the two errors.
  {
    const page = await mount();
    const out = await page.evaluate(async () => {
      const { zonedOnce } = await import('/js/pages/room-panel/schedules.js');
      const fail = (value, zone) => { try { zonedOnce(value, zone); return 'ok'; } catch (error) { return error.message; } };
      return { prague: zonedOnce('2026-01-15T12:00', 'Europe/Prague'), utc: zonedOnce('2026-06-01T08:30', 'UTC'), gap: zonedOnce('2026-03-29T02:30', 'Europe/Prague'), none: fail('', 'UTC'), bad: fail('2026-01-15T12:00', 'Nowhere/Zone') };
    });
    assert.equal(out.prague, '2026-01-15T11:00:00.000Z'); assert.equal(out.utc, '2026-06-01T08:30:00.000Z');
    assert.equal(out.gap, '2026-03-29T01:00:00.000Z', 'a minute that does not exist moves to the next one that does');
    assert.equal(out.none, 'Choose a date and time.'); assert.notEqual(out.bad, 'ok');
    await page.close();
  }

  // Hidden outside task and home rooms; a task room loads its list and its targets; the repeat kind shows its own fields.
  {
    const page = await mount();
    await load(page, roomOf('d1', { kind: 'dm' }));
    assert.equal((await view(page)).hidden, true, 'a private chat has no schedules');
    await load(page, roomOf('r1'));
    let v = await view(page);
    assert.deepEqual([v.hidden, v.list, v.rows, v.submit], [false, 'No schedules', 0, false]);
    assert.deepEqual(v.targets, [['lead', 'Current room leader'], ['a1', 'ada (worker)']]);
    assert.equal(v.zone.length > 0, true, 'the time zone starts as the browser\'s');
    assert.deepEqual(v.fields, { once: 1, interval: 0, civil: 0, weekly: 0 });
    for (const [kind, want] of [['interval', { once: 0, interval: 1, civil: 0, weekly: 0 }], ['daily', { once: 0, interval: 0, civil: 1, weekly: 0 }], ['weekly', { once: 0, interval: 0, civil: 1, weekly: 1 }], ['once', { once: 1, interval: 0, civil: 0, weekly: 0 }]]) {
      await page.select('#room-schedules [name=kind]', kind); await settle(page);
      assert.deepEqual((await view(page)).fields, want, kind);
    }
    await page.close();
  }

  // Rows say what they are; creating sends the fields of the kind; pause and resume and delete patch and delete the one row; failures are said.
  {
    const page = await mount();
    await load(page, roomOf('r1'), [item('s1', { last_outcome: 'sent' })]);
    const rowInfo = () => page.$eval('#room-schedules .schedule-row', row => ({ text: row.firstElementChild.textContent, toggle: [row.querySelector('[data-action=toggle]').getAttribute('aria-label'), !!row.querySelector('[data-action=toggle] .icon-mask-pause'), row.querySelector('[data-action=toggle]').textContent], del: [row.querySelector('[data-action=delete]').title, !!row.querySelector('[data-action=delete] .icon-mask-trash'), row.querySelector('[data-action=delete]').classList.contains('btn-danger')] }));
    let info = await rowInfo();
    assert.match(info.text, /^note s1 → room lead · interval · Europe\/Prague · next .* \(your time\) · last sent$/);
    assert.deepEqual([info.toggle, info.del], [['Pause', true, ''], ['Delete', true, true]]);
    await page.type('#room-schedules [name=body]', 'water the plants');
    await page.select('#room-schedules [name=kind]', 'interval'); await settle(page);
    await page.$eval('#room-schedules [name=minutes]', el => { el.value = '5'; });
    await page.$eval('#room-schedules [name=target]', el => { el.value = 'a1'; });
    await page.click('#room-schedules button[type=submit]');
    const post = await waitRequest(page, 'POST', '/api/rooms/r1/schedules');
    const sent = await page.evaluate(index => window.__requests[index].body, post);
    assert.deepEqual([sent.body, sent.kind, sent.interval_seconds, sent.target_lead, sent.target_agent_id, typeof sent.timezone], ['water the plants', 'interval', 300, false, 'a1', 'string']);
    await answer(page, post, 201, item('s2', { room_id: 'r1', body: 'water the plants', target_lead: false, target_agent_id: 'a1' })); await settle(page);
    assert.deepEqual([(await view(page)).rows, await page.$eval('#room-schedules [name=body]', el => el.value)], [2, ''], 'the new row is listed and the note field is emptied');
    // Pause: one PATCH with the row's revision; the answer replaces the row.
    await page.click('#room-schedules .schedule-row [data-action=toggle]');
    const patch = await waitRequest(page, 'PATCH', '/api/rooms/r1/schedules/s1');
    assert.deepEqual(await page.evaluate(index => window.__requests[index].body, patch), { enabled: false, rev: 1 });
    assert.equal(await page.$eval('#room-schedules .schedule-row [data-action=toggle]', el => el.disabled), true, 'the button is off while it is saved');
    await answer(page, patch, 200, { ...item('s1'), enabled: false, rev: 2, next_run_at: '' }); await settle(page);
    info = await rowInfo();
    assert.match(info.text, /paused/); assert.deepEqual(info.toggle.slice(0, 2), ['Resume', false]);
    // A refused change is said and the button comes back.
    await page.click('#room-schedules .schedule-row [data-action=toggle]');
    await answer(page, await waitRequest(page, 'PATCH', '/api/rooms/r1/schedules/s1'), 409, { error: 'conflict', message: 'the schedule changed' }); await settle(page);
    assert.deepEqual([(await view(page)).error, await page.$eval('#room-schedules .schedule-row [data-action=toggle]', el => el.disabled)], ['the schedule changed', false]);
    // Delete.
    await page.click('#room-schedules .schedule-row [data-action=delete]');
    await answer(page, await waitRequest(page, 'DELETE', '/api/rooms/r1/schedules/s1'), 204, null); await settle(page);
    assert.deepEqual([(await view(page)).rows, (await view(page)).error], [1, '']);
    // A bad time is said without a request.
    const before = await page.evaluate(() => window.__requests.length);
    await page.select('#room-schedules [name=kind]', 'once'); await settle(page);
    await page.type('#room-schedules [name=body]', 'x');
    await page.click('#room-schedules button[type=submit]'); await settle(page);
    assert.equal((await view(page)).error, 'Choose a date and time.');
    assert.equal(await page.evaluate(() => window.__requests.length), before, 'no request for a time that is not there');
    await page.close();
  }

  // A closed room cannot get a new schedule or resume a paused one; a read that fails says why; an answer for a room that was left is dropped.
  {
    const page = await mount();
    await load(page, roomOf('r1', { lifecycle: 'archived' }), [item('s1', { enabled: false })]);
    assert.equal((await view(page)).submit, true, 'no new schedule in a closed room');
    assert.equal(await page.$eval('#room-schedules [data-action=toggle]', el => el.disabled), true, 'a paused one cannot be resumed there');
    await page.evaluate(() => { window.__loading = window.__panel.load({ id: 'r2', project_id: 'p1', kind: 'task', lifecycle: 'open' }); });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r2/schedules'), 500, { error: 'boom', message: 'could not read schedules' });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r2/agents'), 200, []);
    await page.evaluate(() => window.__loading); await settle(page);
    assert.equal((await view(page)).error, 'could not read schedules');
    // Left before the answer: nothing of it is shown.
    await page.evaluate(() => { window.__loading = window.__panel.load({ id: 'r3', project_id: 'p1', kind: 'task', lifecycle: 'open' }); });
    const slow = await waitRequest(page, 'GET', '/api/rooms/r3/schedules');
    await page.evaluate(() => window.__panel.reset());
    await answer(page, slow, 200, { schedules: [item('late', { room_id: 'r3' })] });
    await answer(page, await waitRequest(page, 'GET', '/api/rooms/r3/agents'), 200, []);
    await page.evaluate(() => window.__loading); await settle(page);
    const v = await view(page);
    assert.deepEqual([v.hidden, v.rows], [true, 0], 'a reset panel is hidden and empty, whatever answers later');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('schedules page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
