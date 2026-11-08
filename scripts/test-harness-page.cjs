// The Preact Harnesses page in Chromium, with every request held until the test answers it, so the order of answers is chosen.
// web/ is served as is with its import map; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" open><nav></nav></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const launcher = (machine, id, extra = {}) => ({ config: { id, machine_id: machine, tool: 'codex', name: id, label: '', command: `/usr/bin/${id}`, args: [], env_refs: {}, enabled: true, containerized: false, agent_server_url: '', path_maps: [], visible_roots: [], supports_toolbox: true, ...extra }, resolved_command: `/usr/bin/${id}`, version: '1.0', available: true, active_agents: 0, models: [] });
const machines = { machines: [{ id: 'mch_a', name: 'A', approved: true }, { id: 'mch_b', name: 'B', approved: true }] };
const launchers = { mch_a: [launcher('mch_a', 'a1'), launcher('mch_a', 'a2')], mch_b: [launcher('mch_b', 'b1')] };

// fetch is held: each request waits for __answer(index, status, body) unless an `auto` rule answers it.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path}`];
    if (rule) request.answer(200, typeof rule === 'function' ? rule(request) : rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: '/usr/bin/chromium', headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async (auto = {}, { wanted = '', owner = true } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, { 'GET /api/machines': machines, 'GET /api/roles': [], 'GET /api/machines/mch_a/launchers': { launchers: launchers.mch_a, activity: [] }, 'GET /api/machines/mch_b/launchers': { launchers: launchers.mch_b, activity: [] }, ...auto });
    await page.evaluate(async (wanted, owner) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner, admin: true });
      const dialog = document.querySelector('#settings-dialog');
      const { HarnessSettings } = await import('/js/pages/harnesses/page.js');
      window.__page = new HarnessSettings(dialog);
      window.__page.wanted = wanted;
      window.__page.section.hidden = false; window.__page.section.classList.add('active'); window.__page.open();
    }, wanted, owner);
    await page.waitForFunction(() => document.querySelectorAll('#harnesses-list .machine-harness').length >= 1);
    return page;
  };
  const pending = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const hold = (page, rule) => page.evaluate(rule => { delete window.__auto[rule]; }, rule);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  const type = (page, selector, value) => page.$eval(selector, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
  const chooseMachine = (page, id) => page.$eval('#harness-machine', (el, id) => { el.value = id; el.dispatchEvent(new Event('change', { bubbles: true })); }, id);
  const card = id => `#harnesses-list .machine-harness[data-variant="${id}"]`;
  try {
    // A machine whose supervisor is not connected cannot detect or take a harness: both buttons are off, the reason is their title and a note says
    // how to connect it. A connected machine (or one the server does not report on) shows none of that.
    for (const connected of [false, true, undefined]) {
      const page = await mount({ 'GET /api/machines': { machines: [{ id: 'mch_a', name: 'A', approved: true, connected }, { id: 'mch_b', name: 'B', approved: true }] } });
      const state = await page.evaluate(() => ({ note: !document.querySelector('#harness-offline').hidden, add: [document.querySelector('#harness-add').disabled, document.querySelector('#harness-add').title.slice(0, 32)], detect: document.querySelector('#harness-detect').disabled }));
      if (connected === false) assert.deepEqual(state, { note: true, add: [true, "This machine's supervisor is not"], detect: true }, 'an unconnected machine cannot add or detect, and says why');
      else assert.deepEqual(state, { note: false, add: [false, ''], detect: false }, 'a connected machine can');
      await page.close();
    }
    // An admin who is not the owner may look but not change what a machine runs: the controls that write are off with the reason as their title, the
    // note says so, and the owner sees none of that.
    for (const owner of [true, false]) {
      const page = await mount({}, { owner });
      const state = await page.evaluate(card => ({
        note: !document.querySelector('#harness-readonly').hidden,
        add: [document.querySelector('#harness-add').disabled, document.querySelector('#harness-add').title],
        detect: document.querySelector('#harness-detect').disabled,
        enabled: document.querySelector(`${card} [name=enabled]`).disabled,
        fields: document.querySelector(`${card} .machine-harness-fields`).disabled,
        panels: document.querySelector(`${card} .machine-harness-readonly`).disabled,
        reason: document.querySelector(`${card} .machine-harness-fields`).title,
      }), card('a1'));
      if (owner) assert.deepEqual(state, { note: false, add: [false, ''], detect: false, enabled: false, fields: false, panels: false, reason: '' }, 'the owner can change harnesses');
      else assert.deepEqual(state, { note: true, add: [true, 'Only the owner can change what a machine runs.'], detect: true, enabled: true, fields: true, panels: true, reason: 'Only the owner can change what a machine runs.' }, 'an admin cannot, and is told why');
      if (!owner) {
        // Every disabled control looks disabled the same way: its own opacity times its ancestors' is one value (buttons used to be dimmed and inputs not).
        const seen = await page.evaluate(card => [...document.querySelectorAll(`${card} :is(input, select, textarea, button):disabled, ${card} .machine-harness-readonly :is(input, select, textarea, button)`)].map(el => {
          let opacity = 1; for (let node = el; node && node.id !== 'harnesses-list'; node = node.parentElement) opacity *= Number(getComputedStyle(node).opacity);
          return `${el.tagName}.${el.className}#${el.name || el.id}=${Math.round(opacity * 100) / 100}`;
        }), card('a1'));
        assert.ok(seen.length > 3 && seen.every(value => value.endsWith("=0.5")), `read-only controls dim alike: ${JSON.stringify(seen)}`);
      }
      await page.close();
    }
    // The list of harnesses and the card of the selected one start at the same height.
    {
      const page = await mount({}, { owner: true });
      const edges = await page.evaluate(() => [document.querySelector('.harness-master'), document.querySelector('.harness-split .machine-harness')].map(el => Math.round(el.getBoundingClientRect().top)));
      assert.equal(edges[0], edges[1], `top edges line up: ${JSON.stringify(edges)}`);
      await page.close();
    }
    // A late save on machine A, answered after B was chosen, leaves B's page alone: no read of A, B's own pending read still draws.
    {
      const page = await mount({ 'GET /api/machines/mch_b/launchers': null });
      await page.$eval(`${card('a1')} [name=enabled]`, el => el.click());
      const put = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1');
      await chooseMachine(page, 'mch_b');
      const readB = await pending(page, 'GET', '/api/machines/mch_b/launchers');
      await answer(page, put, 200, { config: launchers.mch_a[0].config });
      await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.path === '/api/machines/mch_a/launchers').length), 1, 'a late save of A does not read A again once B is chosen');
      await answer(page, readB, 200, { launchers: launchers.mch_b, activity: [] });
      await page.waitForSelector(card('b1'));
      assert.deepEqual(await page.$$eval('#harnesses-list .machine-harness', cards => cards.map(el => el.dataset.variant)), ['b1'], "B's list is drawn, not cleared by A's late answer");
      await page.close();
    }
    // Adding detected harnesses on A fails after B was chosen: the failure's re-read of A must not cancel or clear B's list.
    {
      const page = await mount({ 'POST /api/machines/mch_a/launchers/detect': { candidates: [{ family: 'pi', label: 'Pi', command: '/usr/bin/pi', containerized: false }] } });
      await page.$eval('#harness-detect', el => el.click());
      await page.waitForSelector('#harness-detect-panel input[type=checkbox]');
      await page.$eval('#harness-detect-panel input[type=checkbox]', el => el.click());
      await page.$eval('#harness-detect-panel', el => el.requestSubmit());
      const create = await pending(page, 'POST', '/api/machines/mch_a/launchers');
      await hold(page, 'GET /api/machines/mch_b/launchers');
      await chooseMachine(page, 'mch_b');
      const readB = await pending(page, 'GET', '/api/machines/mch_b/launchers');
      await answer(page, create, 500, { message: 'create failed' });
      await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.path === '/api/machines/mch_a/launchers' && request.method === 'GET').length), 1, 'a late failure on A does not read A again once B is chosen');
      await answer(page, readB, 200, { launchers: launchers.mch_b, activity: [] });
      await page.waitForSelector(card('b1'));
      assert.deepEqual(await page.$$eval('#harnesses-list .machine-harness', cards => cards.map(el => el.dataset.variant)), ['b1'], "B's pending read still draws");
      await page.close();
    }
    // Auto-detect on A answered after A -> B -> A: the answer belongs to an earlier view and opens no panel.
    {
      const page = await mount();
      await page.$eval('#harness-detect', el => el.click());
      const detect = await pending(page, 'POST', '/api/machines/mch_a/launchers/detect');
      await chooseMachine(page, 'mch_b'); await page.waitForSelector(card('b1'));
      await chooseMachine(page, 'mch_a'); await page.waitForSelector(card('a1'));
      await answer(page, detect, 200, { candidates: [{ family: 'pi', label: 'Pi', command: '/usr/bin/pi', containerized: false }] });
      await settle(page);
      assert.equal(await page.$('#harness-detect-panel'), null, 'a detect answer from an earlier view opens no panel');
      await page.close();
    }
    // A save whose re-read fails keeps what was saved, and the next save sends it.
    {
      const page = await mount();
      await hold(page, 'GET /api/machines/mch_a/launchers');
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await type(page, `${card('a1')} [name=command]`, '/opt/new');
      await page.$eval(`${card('a1')} button[type=submit]`, el => el.click());
      await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1'), 200, {});
      await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 500, { message: 'read failed' });
      await page.waitForFunction(() => document.querySelector('#settings-harnesses > .error').textContent === 'read failed');
      await settle(page);
      assert.equal(await page.$eval(`${card('a1')} [name=command]`, el => el.value), '/opt/new', 'a failed re-read keeps the saved command in the form');
      await page.$eval(`${card('a1')} button[type=submit]`, el => el.click());
      const again = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1');
      assert.equal(await page.evaluate(index => window.__requests[index].body.command, again), '/opt/new', 'the next save sends the saved value, not the old one');
      await answer(page, again, 200, {});
      await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, { launchers: launchers.mch_a, activity: [] });
      await type(page, `${card('a2')} .harness-model-add [name=slug]`, 'model-one');
      await page.$eval(`${card('a2')} .harness-model-add button[type=submit]`, el => el.click());
      await page.waitForSelector(`${card('a2')} .harness-model-row[data-slug="model-one"]`);
      const roleHint = await page.$eval(`${card('a2')} .harness-model-row[data-slug="model-one"] .harness-detail-roles-hint`, el => el.textContent);
      assert.ok(/none ticked/.test(roleHint) && /spawn by hand/.test(roleHint), `the role switches are explained: ${roleHint}`);
      await page.$eval(`${card('a2')} .harness-models .btn-primary`, el => el.click());
      await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a2/models'), 200, {});
      await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 500, { message: 'read failed' });
      await page.waitForFunction(sel => document.querySelector('#harness-tab-a2 .harness-master-count').textContent === '1 model', {});
      await settle(page);
      assert.ok(await page.$(`${card('a2')} .harness-model-row[data-slug="model-one"]`), 'a failed re-read keeps the saved model list');
      assert.deepEqual(await page.evaluate(() => [...window.__page.page.unsaved]), [], 'what was saved is not reported as unsaved (no draft left)');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false, 'what was saved is not reported as unsaved');
      await page.close();
    }
    // One write at a time per card: Save cannot start while Enabled is in flight (re-read included), nor Enabled while Save is.
    {
      const page = await mount();
      await hold(page, 'GET /api/machines/mch_a/launchers');
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await page.$eval(`${card('a1')} [name=enabled]`, el => el.click());
      const put = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1');
      await settle(page);
      assert.deepEqual(await page.$eval(card('a1'), el => [el.querySelector('[name=enabled]').disabled, el.querySelector('button[type=submit]').disabled, el.querySelector('.machine-harness-remove').disabled]), [true, true, true], 'every write control is disabled while Enabled saves');
      await page.$eval(`${card('a1')} form`, el => el.requestSubmit());
      await settle(page);
      const puts = () => page.evaluate(() => window.__requests.filter(request => request.method === 'PUT' && request.path === '/api/machines/mch_a/launchers/a1').length);
      assert.equal(await puts(), 1, 'Save does not start while Enabled is in flight');
      await answer(page, put, 200, {});
      const reread = await pending(page, 'GET', '/api/machines/mch_a/launchers');
      await page.$eval(`${card('a1')} form`, el => el.requestSubmit());
      await settle(page);
      assert.equal(await puts(), 1, 'nor during the re-read that follows it');
      await answer(page, reread, 200, { launchers: [launcher('mch_a', 'a1', { enabled: false }), launchers.mch_a[1]], activity: [] });
      await page.waitForFunction(sel => !document.querySelector(`${sel} button[type=submit]`).disabled, {}, card('a1'));
      await page.$eval(`${card('a1')} form`, el => el.requestSubmit());
      const save = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1');
      await page.$eval(`${card('a1')} [name=enabled]`, el => el.dispatchEvent(new Event('change', { bubbles: true })));
      await settle(page);
      assert.equal(await puts(), 2, 'Enabled does not start while Save is in flight');
      assert.equal(await page.$eval(`${card('a1')} [name=enabled]`, el => el.disabled), true);
      await answer(page, save, 200, {});
      await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, { launchers: launchers.mch_a, activity: [] });
      await page.waitForFunction(sel => !document.querySelector(`${sel} [name=enabled]`).disabled, {}, card('a1'));
      await page.close();
    }
    // Model detection runs once at a time: the button, Retry and repeated Enter while one is in flight start nothing more, so
    // two answers can never arrive out of order; a failure, then an Enter retry, then success shows the retry's candidates.
    {
      const page = await mount();
      const detectPath = '/api/machines/mch_a/launchers/a1/models/detect';
      const detects = () => page.evaluate(path => window.__requests.filter(request => request.path === path).length, detectPath);
      const panel = `${card('a1')} .harness-models`;
      await page.$eval(`${panel} .harness-model-detect`, el => el.click());
      const first = await pending(page, 'POST', detectPath);
      await page.$eval(panel, el => {
        el.querySelector('.harness-model-detect').click();
        el.querySelector('.harness-detect-retry-button').click();
      });
      await settle(page);
      assert.equal(await detects(), 1, 'the button and Retry start nothing while a detection is in flight');
      await answer(page, first, 502, { message: 'not running here' });
      await page.waitForFunction(sel => !document.querySelector(`${sel} .harness-detect-retry`).hidden, {}, panel);
      await type(page, `${panel} [name=detect-dir]`, '/work/one');
      await page.$eval(`${panel} [name=detect-dir]`, el => { for (let i = 0; i < 3; i++) el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })); });
      const second = await pending(page, 'POST', detectPath);
      await page.$eval(`${panel} .harness-detect-retry-button`, el => el.click());
      await settle(page);
      assert.equal(await detects(), 2, 'repeated Enter and Retry during a detection send one request');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, second), { dir: '/work/one' });
      await answer(page, second, 200, { source: 'harness', candidates: [{ slug: 'from-retry', label: 'From retry', note: '' }] });
      await page.waitForSelector(`${panel} .harness-model-candidates [data-slug="from-retry"]`);
      assert.deepEqual(await page.$eval(panel, el => [el.querySelector('.error').textContent, el.querySelector('.harness-detect-retry').hidden, el.querySelector('.harness-model-detect').disabled]), ['', true, false], 'the retry\'s answer replaces the failure and frees the button');
      await page.close();
    }
    // Typing while a save is under way: the newer text stays, and still counts as unsaved.
    {
      const page = await mount();
      await hold(page, 'GET /api/machines/mch_a/launchers');
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await type(page, `${card('a1')} [name=label]`, 'one');
      await page.$eval(`${card('a1')} button[type=submit]`, el => el.click());
      const put = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1');
      assert.equal(await page.evaluate(index => window.__requests[index].body.label, put), 'one');
      await type(page, `${card('a1')} [name=label]`, 'one two');
      await answer(page, put, 200, {});
      await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, { launchers: [launcher('mch_a', 'a1', { label: 'one' }), launchers.mch_a[1]], activity: [] });
      await page.waitForFunction(sel => document.querySelector(`${sel} .settings-label`).textContent === 'one', {}, card('a1'));
      await settle(page);
      assert.equal(await page.$eval(`${card('a1')} [name=label]`, el => el.value), 'one two', 'text typed during the save is kept');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true, 'and still counts as unsaved');
      await page.close();
    }
    // The same input node, the focus and the selection survive a refresh caused by another card and the card's own save.
    {
      const page = await mount();
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await page.$eval(`${card('a1')} [name=command]`, el => { window.__node = el; el.focus(); el.setSelectionRange(2, 5); });
      await page.$eval(`${card('a2')} [name=enabled]`, el => { el.checked = !el.checked; el.dispatchEvent(new Event('change', { bubbles: true })); });
      await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a2'), 200, {});
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines/mch_a/launchers').length === 2);
      await settle(page);
      assert.deepEqual(await page.$eval(`${card('a1')} [name=command]`, el => [el === window.__node, document.activeElement === el, el.selectionStart, el.selectionEnd]), [true, true, 2, 5], 'another card\'s refresh keeps the input node, the focus and the selection');
      await page.$eval(`${card('a1')} form`, el => el.requestSubmit());
      await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1'), 200, {});
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines/mch_a/launchers').length === 3);
      await settle(page);
      assert.equal(await page.$eval(`${card('a1')} [name=command]`, el => el === window.__node), true, 'the card\'s own save keeps the input node');
      await page.close();
    }
    // Removing a harness: the in-app confirm, then DELETE, then the list read again without it.
    {
      const page = await mount();
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await page.$eval(`${card('a1')} .machine-harness-remove`, el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Its models go with it'));
      await page.$eval('#app-dialog .app-dialog-ok', el => el.click());
      const remove = await pending(page, 'DELETE', '/api/machines/mch_a/launchers/a1');
      await page.evaluate(rest => { window.__auto['GET /api/machines/mch_a/launchers'] = { launchers: rest, activity: [] }; }, [launchers.mch_a[1]]);
      await answer(page, remove, 204);
      await page.waitForFunction(() => document.querySelectorAll('#harnesses-list .machine-harness').length === 1);
      assert.equal(await page.$(card('a1')), null, 'the removed harness is gone from the list');
      await page.close();
    }
    // Unmounting while a read is pending: the late answer draws nothing, throws nothing and starts nothing.
    {
      const page = await mount();
      await page.evaluate(() => { window.__auto = {}; window.__page.open(); });
      const read = await pending(page, 'GET', '/api/machines');
      await page.evaluate(() => window.__page.destroy());
      const made = await page.evaluate(() => window.__requests.length);
      await answer(page, read, 200, machines);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-harnesses').childElementCount, window.__requests.length]), [0, made], `an unmounted page stays empty and makes no request: ${JSON.stringify(await page.evaluate(made => window.__requests.slice(made).map(request => `${request.method} ${request.path}`), made))}`);
      await page.close();
    }
    // A draft's unsaved report goes when its card goes: typing on machine A, then choosing B, leaves nothing unsaved.
    {
      const page = await mount();
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await type(page, `${card('a1')} [name=label]`, 'draft');
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [...window.__page.page.unsaved]), ['form:a1']);
      await chooseMachine(page, 'mch_b');
      await page.waitForSelector(card('b1'));
      assert.deepEqual(await page.evaluate(() => [...window.__page.page.unsaved]), [], 'an unmounted card reports nothing unsaved');
      await page.close();
    }
    // The shell's onReady hears of a harness that can start an agent (enabled, with a model).
    {
      const ready = launcher('mch_a', 'a1', {});
      ready.models = [{ slug: 'm', label: 'M', note: '', default: true }];
      const page = await mount({ 'GET /api/machines/mch_a/launchers': { launchers: [ready, launchers.mch_a[1]], activity: [] } });
      await page.evaluate(() => { window.__ready = 0; window.__page.onReady = () => { window.__ready++; }; window.__page.open(); });
      await page.waitForFunction(() => window.__ready > 0);
      await page.close();
    }
    // Icon actions and the default star carry the legacy classes, tooltips and labels.
    {
      const modelled = launcher('mch_a', 'a1');
      modelled.models = [{ slug: 'one', label: 'One', note: 'full note', default: true }, { slug: 'two', label: 'Two', note: '', default: false }];
      const page = await mount({ 'GET /api/machines/mch_a/launchers': { launchers: [modelled, launchers.mch_a[1]], activity: [] } });
      assert.deepEqual(await page.$eval(card('a1'), el => [el.querySelector('.machine-harness-edit').title, !!el.querySelector('.machine-harness-edit .icon-mask-edit'), el.querySelector('.machine-harness-remove').getAttribute('aria-label'), !!el.querySelector('.machine-harness-remove .icon-mask-trash')]), ['Edit Codex', true, 'Remove Codex', true], 'card actions are labelled icons');
      assert.deepEqual(await page.$$eval(`${card('a1')} .harness-model-row`, rows => rows.map(row => [row.querySelector('.harness-model-star use').getAttribute('href'), row.querySelector('.harness-model-default-text').textContent, row.querySelector('.harness-model-default').title, row.querySelector('.harness-model-remove').getAttribute('aria-label'), !!row.querySelector('.icon-mask-trash')])), [['/icons/star.svg#star', 'Default', 'Default model', 'Remove one', true], ['/icons/star.svg#star', '', 'Make default', 'Remove two', true]], 'model rows: star default and trash');
      assert.equal(await page.$eval(`${card('a1')} .harness-model-row [name=note]`, el => el.title), 'full note', 'the full note is a tooltip');
      assert.deepEqual(await page.$eval(`${card('a1')} .harness-model-detect`, el => [el.title, !!el.querySelector('.icon-mask-refresh'), el.textContent.trim()]), ['Auto-detect models', true, ''], 'model detection is an icon');
      await page.close();
    }
    // The page hands its chosen machine to the next one: a fresh page asked to open on machine B shows B's harnesses.
    {
      const page = await mount();
      await chooseMachine(page, 'mch_b');
      await page.waitForSelector(card('b1'));
      assert.deepEqual(await page.evaluate(() => window.__page.viewSnapshot()), { wanted: 'mch_b' }, 'the page reports its machine');
      await page.close();
      const next = await mount({}, { wanted: 'mch_b' });
      assert.deepEqual(await next.evaluate(() => [document.querySelector('#harness-machine').value, [...document.querySelectorAll('#harnesses-list .machine-harness')].map(el => el.dataset.variant)]), ['mch_b', ['b1']], 'wanted opens on that machine');
      await next.close();
    }
    // Unfinished input counts as unsaved while it is there, and is gone with the page.
    const detectAuto = {
      'POST /api/machines/mch_a/launchers/detect': { candidates: [{ family: 'pi', label: 'Pi', command: '/usr/bin/pi', containerized: false }] },
      'POST /api/machines/mch_a/launchers/a1/models/detect': { source: 'harness', candidates: [{ slug: 'found-model', label: 'Found', note: '' }] },
    };
    const drafts = [
      { name: 'an Edit form', setup: async page => { await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click()); await type(page, `${card('a1')} [name=label]`, 'draft'); }, kept: page => page.$eval(`${card('a1')} [name=label]`, el => el.value === 'draft') },
      { name: 'the Add harness form', setup: async page => { await page.$eval('#harness-add', el => el.click()); await page.waitForSelector('#harness-add-form'); await type(page, '#harness-add-form [name=command]', '/opt/unsaved-tool'); }, kept: page => page.$eval('#harness-add-form [name=command]', el => el.value === '/opt/unsaved-tool') },
      { name: 'a model id not added yet', setup: async page => { await type(page, `${card('a1')} .harness-model-add [name=slug]`, 'typed-model'); }, kept: page => page.$eval(`${card('a1')} .harness-model-add [name=slug]`, el => el.value === 'typed-model') },
      { name: 'ticked harnesses from auto-detect', setup: async page => { await page.$eval('#harness-detect', el => el.click()); await page.waitForSelector('#harness-detect-panel input[type=checkbox]'); await page.$eval('#harness-detect-panel input[type=checkbox]', el => el.click()); }, kept: page => page.$eval('#harness-detect-panel input[type=checkbox]', el => el.checked) },
      { name: 'ticked models from auto-detect', setup: async page => { await page.$eval(`${card('a1')} .harness-models .harness-model-detect`, el => el.click()); await page.waitForSelector(`${card('a1')} .harness-model-candidates input[type=checkbox]`); await page.$eval(`${card('a1')} .harness-model-candidates input[type=checkbox]`, el => el.click()); }, kept: page => page.$eval(`${card('a1')} .harness-model-candidates input[type=checkbox]`, el => el.checked) },
    ];
    for (const draft of drafts) {
      const page = await mount(detectAuto);
      assert.equal(await page.$('#settings-harnesses > h3'), null, 'the page does not repeat the section title the dialog header shows, and has no experiment badge');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false, `nothing is unsaved before ${draft.name}`);
      await draft.setup(page);
      await settle(page);
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true, `${draft.name} counts as unsaved`);
      assert.equal(await draft.kept(page), true, `${draft.name} is kept`);
      await page.evaluate(() => window.__page.destroy());
      assert.deepEqual(await page.evaluate(() => [window.__page.hasUnsavedChanges(), !!document.querySelector('#harness-add-form, #harness-detect-panel, .harness-model-candidates')]), [false, false], `destroying the page drops ${draft.name}`);
      await page.close();
    }
    // The list beside the detail: the first harness is chosen and shown alone; a click or the arrow keys move the choice, and a reload keeps it.
    {
      const page = await mount();
      const shown = () => page.$$eval('#harnesses-list .machine-harness', cards => cards.map(el => !el.hidden));
      assert.deepEqual(await page.$$eval('.harness-master [role=tab]', tabs => tabs.map(tab => [tab.dataset.harnessId, tab.getAttribute('aria-selected'), tab.tabIndex, tab.getAttribute('aria-controls')])), [['a1', 'true', 0, 'harness-card-a1'], ['a2', 'false', -1, 'harness-card-a2']], 'the list names the harnesses, the first is chosen');
      assert.deepEqual(await shown(), [true, false], 'only the chosen harness is shown');
      assert.equal(await page.$('.machine-harness-models-toggle'), null, 'the models are always shown: no Models toggle');
      await page.$eval('#harness-tab-a2', el => el.click());
      assert.deepEqual(await shown(), [false, true], 'choosing a harness shows its detail');
      await page.$eval('#harness-tab-a2', el => el.focus());
      await page.keyboard.press('ArrowDown');
      assert.deepEqual([await shown(), await page.evaluate(() => document.activeElement.id)], [[true, false], 'harness-tab-a1'], 'the arrow keys wrap round and focus the chosen tab');
      await page.$eval('#harness-tab-a2', el => el.click());
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines/mch_a/launchers').length >= 2);
      await settle(page);
      assert.deepEqual(await shown(), [false, true], 'a reload keeps the chosen harness');
      await page.close();
    }
    // A save of the models names the revision the draft started from; one refused as stale drops the draft, reads again and says so.
    {
      const modelled = { ...launcher('mch_a', 'a1'), models: [{ slug: 'one', label: 'One', note: '', default: true }], models_rev: 'r1' };
      const page = await mount({ 'GET /api/machines/mch_a/launchers': { launchers: [modelled, launchers.mch_a[1]], activity: [] } });
      await type(page, `${card('a1')} .harness-model-row [name=label]`, 'Mine');
      await page.$eval(`${card('a1')} .machine-harness-edit`, el => el.click());
      await type(page, `${card('a1')} [name=command]`, '/opt/unsaved');
      await hold(page, 'GET /api/machines/mch_a/launchers');
      await page.$eval(`${card('a1')} .harness-models .btn-primary`, el => el.click());
      const put = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1/models');
      assert.equal(await page.evaluate(index => window.__requests[index].body.expected_rev, put), 'r1', 'the save names the revision of the list it started from');
      await answer(page, put, 409, { error: 'models_changed', message: 'The models changed' });
      const reread = await pending(page, 'GET', '/api/machines/mch_a/launchers');
      await answer(page, reread, 200, { launchers: [{ ...modelled, models: [{ slug: 'one', label: 'Theirs', note: '', default: true }], models_rev: 'r2' }, launchers.mch_a[1]], activity: [] });
      await page.waitForFunction(selector => document.querySelector(`${selector} .harness-model-row [name=label]`)?.value === 'Theirs', {}, card('a1'));
      assert.match(await page.$eval('#settings-harnesses > .error', el => el.textContent), /Someone changed the models of a1 in the meantime/, 'the page says the save was refused');
      assert.equal(await page.$eval(`${card('a1')} .harness-models .btn-primary`, el => el.disabled), true, 'the refused draft is gone');
      assert.equal(await page.$eval(`${card('a1')} [name=command]`, el => el.value), '/opt/unsaved', 'only the refused models draft is dropped: the Edit form keeps what was typed');
      assert.deepEqual(await page.evaluate(() => [...window.__page.page.unsaved]), ['form:a1'], 'the Edit form is still unsaved, the models are not');
      await page.close();
    }
    // The revision moves on with every save: an edit made while a save is in flight, and the save after it, start from the revision the
    // first save answered with, and so does the next edit when the re-read after a save failed; neither is refused as stale.
    {
      const modelled = { ...launcher('mch_a', 'a1'), models: [{ slug: 'one', label: 'One', note: '', default: true }], models_rev: 'r1' };
      const listing = rows => ({ launchers: [rows, launchers.mch_a[1]], activity: [] });
      const stored = (label, rev) => ({ ...modelled, models: [{ slug: 'one', label, note: '', default: true }], models_rev: rev });
      const saveButton = `${card('a1')} .harness-models .btn-primary`, label = `${card('a1')} .harness-model-row [name=label]`;
      const sentRev = (page, index) => page.evaluate(index => window.__requests[index].body.expected_rev, index);
      {
        const page = await mount({ 'GET /api/machines/mch_a/launchers': listing(modelled) });
        await type(page, label, 'First');
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(saveButton, el => el.click());
        const first = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1/models');
        assert.equal(await sentRev(page, first), 'r1');
        await type(page, label, 'Second');
        await answer(page, first, 200, { models: [{ slug: 'one', label: 'First', note: '', default: true }], models_rev: 'r2' });
        await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, listing(stored('First', 'r2')));
        await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, saveButton);
        assert.equal(await page.$eval(label, el => el.value), 'Second', 'an edit made during the save is kept');
        await page.$eval(saveButton, el => el.click());
        const second = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1/models');
        assert.equal(await sentRev(page, second), 'r2', 'the next save names the revision the first one answered with');
        await answer(page, second, 200, { models: [{ slug: 'one', label: 'Second', note: '', default: true }], models_rev: 'r3' });
        await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, listing(stored('Second', 'r3')));
        await settle(page);
        assert.equal(await page.$('#settings-harnesses > .error:not(:empty)'), null, 'no stale-save message');
        await page.close();
      }
      {
        const page = await mount({ 'GET /api/machines/mch_a/launchers': listing(modelled) });
        await type(page, label, 'First');
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(saveButton, el => el.click());
        await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1/models'), 200, { models: [{ slug: 'one', label: 'First', note: '', default: true }], models_rev: 'r2' });
        await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 500, { message: 'read failed' });
        await page.waitForFunction(() => document.querySelector('#settings-harnesses > .error').textContent === 'read failed');
        await settle(page);
        await type(page, label, 'Again');
        await page.$eval(saveButton, el => el.click());
        const next = await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1/models');
        assert.equal(await sentRev(page, next), 'r2', 'after a failed re-read the next edit starts from the saved revision, not a stale one');
        await answer(page, next, 200, { models: [{ slug: 'one', label: 'Again', note: '', default: true }], models_rev: 'r3' });
        await page.close();
      }
    }
    // Role defaults: a switch per role on every model, saved on its own; the role panel names its entries, including those of another machine,
    // which are read once; and a save made elsewhere shows here while the page is open, not while it is hidden.
    {
      const modelled = { ...launcher('mch_a', 'a1'), models: [{ slug: 'one', label: 'One', note: '', default: true }], models_rev: 'r1' };
      const role = { id: 'rol_w', rev: 3, role_key: 'worker', label: 'Worker', emoji: '', body: '', extends: '', cardinality: 'multi', cap_is_lead: false, cap_can_spawn: false, cap_singleton_writer: false, default_agents: [{ launcher_id: 'b1', model: 'far' }], pick_mode: 'in_turn' };
      const page = await mount({
        'GET /api/machines/mch_a/launchers': { launchers: [modelled, launchers.mch_a[1]], activity: [] },
        'GET /api/machines/mch_b/launchers': { launchers: [{ ...launcher('mch_b', 'b1'), models: [{ slug: 'far', label: 'Far', note: '', default: true }] }], activity: [] },
        'GET /api/roles': [role],
      });
      const entries = () => page.$$eval('#harness-roles-host .harness-role-entry', items => items.map(item => item.querySelector('.harness-role-entry-label').textContent));
      await page.waitForFunction(() => document.querySelector('#harness-roles-host .harness-role-entry-label')?.textContent === 'B · b1 · Far');
      assert.deepEqual(await entries(), ['B · b1 · Far'], "a default on another machine is named from that machine's harness");
      const reads = path => page.evaluate(path => window.__requests.filter(request => request.method === 'GET' && request.path === path).length, path);
      // A redraw of the list (here, after the Enabled switch saves) reads the other machine no more: it asks again only for an id not seen yet.
      await page.$eval(`${card('a1')} [name=enabled]`, el => el.click());
      await answer(page, await pending(page, 'PUT', '/api/machines/mch_a/launchers/a1'), 200, { config: modelled.config });
      await page.waitForFunction(() => window.__requests.filter(request => request.method === 'GET' && request.path === '/api/machines/mch_a/launchers').length >= 2);
      await settle(page);
      assert.equal(await reads('/api/machines/mch_b/launchers'), 1, 'the other machine is read once, not again on a redraw');
      // The switch on a model saves the role on its own, naming the harness and model.
      const box = `${card('a1')} .harness-detail-row[data-slug="one"] input[aria-label="Worker default: One"]`;
      assert.equal(await page.$eval(box, el => el.checked), false);
      await page.$eval(box, el => el.click());
      const put = await pending(page, 'PUT', '/api/roles/rol_w');
      assert.deepEqual(await page.evaluate(index => ({ rev: window.__requests[index].body.rev, agents: window.__requests[index].body.default_agents }), put), { rev: 3, agents: [{ launcher_id: 'b1', model: 'far' }, { launcher_id: 'a1', model: 'one' }] }, 'the save sends the role as it is with the model added');
      await answer(page, put, 200, { ...role, rev: 4, default_agents: [{ launcher_id: 'b1', model: 'far' }, { launcher_id: 'a1', model: 'one' }] });
      await page.waitForFunction(() => document.querySelectorAll('#harness-roles-host .harness-role-entry').length === 2);
      assert.deepEqual(await entries(), ['B · b1 · Far', 'A · a1 · One'], 'the role panel shows the new default named from this machine');
      assert.match(await page.$eval(`${card('a1')} .harness-detail-row[data-slug="one"] .harness-role-switch.is-on .harness-role-switch-position`, el => el.textContent), /#2 of 2/, 'a switched-on role shows its place');
      // A failed save says why, in the models panel for a switch and under the role panel for its own controls, and the views show what is stored.
      const stored = { ...role, rev: 4, default_agents: [{ launcher_id: 'b1', model: 'far' }, { launcher_id: 'a1', model: 'one' }] };
      await hold(page, 'GET /api/roles');
      await page.$eval(box, el => el.click());
      await answer(page, await pending(page, 'PUT', '/api/roles/rol_w'), 409, { error: 'conflict', message: 'The role changed elsewhere' });
      await answer(page, await pending(page, 'GET', '/api/roles'), 200, [stored]);
      await page.waitForFunction(selector => document.querySelector(`${selector} .harness-models .error`)?.textContent === 'The role changed elsewhere', {}, card('a1'));
      assert.equal(await page.$eval(box, el => el.checked), true, 'a refused switch-off leaves the switch on, as stored');
      await page.$eval('#harness-roles-host .harness-role-entry button[aria-label^="Remove"]', el => el.click());
      await answer(page, await pending(page, 'PUT', '/api/roles/rol_w'), 409, { error: 'conflict', message: 'The role changed again' });
      await answer(page, await pending(page, 'GET', '/api/roles'), 200, [stored]);
      await page.waitForFunction(() => document.querySelector('#harness-roles-host + .error')?.textContent === 'The role changed again');
      await page.evaluate(stored => { window.__auto['GET /api/roles'] = [stored]; }, stored);
      // Settings -> Roles saves a role: the page reads the roles again while it is shown, and does nothing while it is hidden.
      const roleReads = () => reads('/api/roles');
      const before = await roleReads();
      await page.evaluate(() => document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: { source: {} } })));
      await page.waitForFunction(count => window.__requests.filter(request => request.method === 'GET' && request.path === '/api/roles').length > count, {}, before);
      await settle(page);
      const after = await roleReads();
      await page.evaluate(() => { window.__page.section.hidden = true; document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: { source: {} } })); });
      await settle(page);
      assert.equal(await roleReads(), after, 'a role save elsewhere while the page is hidden reads nothing');
      await page.close();
    }
    // The onboarding step embeds the page: prefixed ids, no Add or Close on the detect panel, the tick count and adding what is ticked
    // through the page's own methods, onReady, and nothing left behind after destroy.
    {
      const ready = launcher('mch_a', 'a1', {});
      ready.models = [{ slug: 'm', label: 'M', note: '', default: true }];
      const created = launcher('mch_a', 'a3', { tool: 'pi' });
      const page = await browser.newPage();
      page.on('pageerror', error => errors.push(error.message));
      await page.goto(`${origin}/fixture.html`);
      await page.evaluate(installFetch, {
        'GET /api/machines': machines, 'GET /api/roles': [],
        'GET /api/machines/mch_a/launchers': { launchers: [ready], activity: [] },
        'POST /api/machines/mch_a/launchers/detect': { candidates: [{ family: 'pi', label: 'Pi', command: '/usr/bin/pi', containerized: false }] },
        'POST /api/machines/mch_a/launchers': { config: created.config },
        'POST /api/machines/mch_a/launchers/a3/models/detect': { candidates: [{ slug: 'found', label: 'Found', note: '' }] },
        'PUT /api/machines/mch_a/launchers/a3/models': {},
      });
      await page.evaluate(async () => {
        (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_me', owner: true, admin: true });
        const host = document.createElement('div'); host.id = 'host'; document.body.append(host);
        const { HarnessSettings } = await import('/js/pages/harnesses/page.js');
        window.__ticks = []; window.__ready = 0;
        window.__page = new HarnessSettings(null, { embedded: true, host });
        window.__page.onTick = count => window.__ticks.push(count);
        window.__page.onReady = () => { window.__ready++; };
        await window.__page.open();
      });
      assert.equal(await page.evaluate(() => window.__ready > 0), true, 'the embedded page tells onReady about a harness that can start an agent');
      assert.deepEqual(await page.evaluate(() => [!!document.querySelector('#host #onboarding-settings-harnesses'), !!document.querySelector('#onboarding-harness-add, #onboarding-harness-detect, #onboarding-harnesses-list, #onboarding-harness-machine'), !!document.querySelector('#harness-add, #harness-detect, #harnesses-list, #harness-machine, #settings-harnesses'), document.querySelector('#settings-harnesses-tab')]), [true, true, false, null], 'the ids carry the prefix and there is no tab');
      await page.$eval('#onboarding-harness-detect', el => el.click());
      await page.waitForSelector('#onboarding-harness-detect-panel input[type=checkbox]');
      assert.deepEqual(await page.$eval('#onboarding-harness-detect-panel', panel => [panel.querySelector('strong').textContent, !!panel.querySelector('button')]), ['Found on this machine — tick the ones to add, then press Continue', false], 'no Add or Close button in the onboarding');
      assert.equal(await page.evaluate(() => window.__page.tickedCount()), 0);
      assert.equal(await page.evaluate(() => window.__page.addTicked()), false, 'nothing ticked adds nothing');
      assert.equal(await page.$eval('#onboarding-harness-detect-panel .error', el => el.textContent), 'Tick at least one harness to add.');
      await page.$eval('#onboarding-harness-detect-panel input[type=checkbox]', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__page.tickedCount(), window.__ticks.at(-1)]), [1, 1], 'ticking reports the count');
      assert.equal(await page.evaluate(() => window.__page.addTicked()), true, 'adding what is ticked goes through');
      assert.deepEqual(await page.evaluate(() => [window.__page.tickedCount(), window.__ticks.at(-1), !!document.querySelector('#onboarding-harness-detect-panel'), window.__requests.filter(request => request.method === 'POST' && request.path.endsWith('/launchers')).length]), [0, 0, false, 1], 'the panel is gone, nothing is ticked and the harness was created once');
      await page.evaluate(() => window.__page.destroy());
      const before = await page.evaluate(() => window.__requests.length);
      await page.evaluate(() => document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: {} })));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#host').children[0]?.childElementCount ?? 0, window.__requests.length]), [0, before], 'a destroyed page leaves no content and no listener that asks for anything');
      await page.close();
    }
    // A cancelled Add harness form no longer counts as unsaved.
    {
      const page = await mount({});
      await page.$eval('#harness-add', el => el.click()); await page.waitForSelector('#harness-add-form');
      await type(page, '#harness-add-form [name=command]', '/opt/cancelled');
      await settle(page);
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true);
      await page.$eval('#harness-add-form .machine-harness-actions .btn:not(.btn-primary)', el => el.click());
      await settle(page);
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false, 'a cancelled Add harness form is not unsaved');
      await page.close();
    }

    // Custom providers of an OpenCode harness: only OpenCode has the section; rows, key status per provider (set, not set, machine offline, supervisor
    // update), a save that sends the whole list with the revision it started from and the limits as a pair, the refusals, and the import.
    {
      const base = launcher('mch_a', 'oc1');
      const provider = (id, extra = {}) => ({ id, name: id.toUpperCase(), npm: '@ai-sdk/openai-compatible', base_url: `https://${id}.example/v1`, key_env: `${id.toUpperCase()}_KEY`, ...extra });
      const opencode = { ...base, config: { ...base.config, tool: 'opencode' }, providers: [provider('acme'), provider('local', { key_env: '' })], providers_rev: 'p1', models: [{ slug: 'acme/big', label: 'Big', note: '', default: true }], models_rev: 'm1' };
      const list = { launchers: [opencode, launchers.mch_a[1]], activity: [] };
      const keyURL = '/api/machines/mch_a/launchers/oc1/providers/key-status', putURL = '/api/machines/mch_a/launchers/oc1/providers', panel = `${card('oc1')} .harness-providers`;
      const states = page => page.$$eval(`${panel} .harness-provider-row`, rows => rows.map(row => [row.dataset.provider, row.querySelector('.harness-provider-key').textContent]));
      const setup = (keys = { machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', key_env: 'ACME_KEY', set: true }] }, more = {}) => ({ 'GET /api/machines/mch_a/launchers': list, [`GET ${keyURL}`]: keys, ...more });
      {
        const page = await mount(setup());
        assert.equal(await page.$(`${card('a2')} .harness-providers`), null, 'a harness of another family has no providers section');
        await page.waitForFunction(selector => document.querySelector(`${selector} .harness-provider-key`)?.textContent === 'ACME_KEY is set', {}, panel);
        assert.deepEqual(await states(page), [['acme', 'ACME_KEY is set'], ['local', 'No key variable']], 'one row per provider, with its key status (never a value)');
        assert.equal(await page.$eval(`${panel} [name=key_env]`, el => el.value), 'ACME_KEY');
        assert.equal(await page.$eval(`${panel} .harness-provider-add`, el => el.title), 'Add a provider', 'the controls are icons with tooltips');
        await page.close();
      }
      for (const [keys, expected] of [[{ machine_online: false, supervisor_supported: true, keys: [] }, 'Machine offline: key not checked'], [{ machine_online: true, supervisor_supported: false, keys: [] }, 'Supervisor update needed to check the key'],
        [{ machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', key_env: 'ACME_KEY', set: false }] }, 'ACME_KEY is not set on that machine'], [{ machine_online: true, supervisor_supported: true, keys: [{ provider_id: 'acme', key_env: 'ACME_KEY', set: null }] }, 'Key not checked']]) {
        const page = await mount(setup(keys));
        await page.waitForFunction((selector, text) => document.querySelector(`${selector} .harness-provider-key`)?.textContent === text, {}, panel, expected);
        await page.close();
      }
      // Save: the whole list, the revision, the limits only as a pair; a key variable edited but not saved has no status to show.
      {
        const page = await mount(setup());
        await page.waitForSelector(panel);
        await page.$eval(`${panel} .harness-provider-add`, el => el.click());
        await page.waitForFunction(selector => document.querySelectorAll(`${selector} .harness-provider-row`).length === 3, {}, panel);
        const row = `${panel} .harness-provider-row:nth-of-type(3)`;
        await type(page, `${row} [name=id]`, 'Bad Id'); await type(page, `${row} [name=base_url]`, 'https://new.example/v1');
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        await settle(page);
        assert.match(await page.$eval(`${panel} .error`, el => el.textContent), /lowercase letters, digits and hyphens/, 'a bad id is refused inline');
        assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').length), 0, 'and nothing is sent');
        await type(page, `${row} [name=id]`, 'fresh'); await type(page, `${row} [name=context_tokens]`, '128000');
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        await settle(page);
        assert.match(await page.$eval(`${panel} .error`, el => el.textContent), /both token limits or neither/);
        await type(page, `${row} [name=output_tokens]`, '8000'); await type(page, `${row} [name=key_env]`, 'FRESH_KEY');
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        const put = await pending(page, 'PUT', putURL);
        assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { providers: [provider('acme'), { id: 'local', name: 'LOCAL', npm: '@ai-sdk/openai-compatible', base_url: 'https://local.example/v1' }, { id: 'fresh', name: 'fresh', npm: '@ai-sdk/openai-compatible', base_url: 'https://new.example/v1', key_env: 'FRESH_KEY', context_tokens: 128000, output_tokens: 8000 }], expected_rev: 'p1' }, 'the whole list, the revision it started from, the limits as a pair');
        await answer(page, put, 400, { error: 'invalid_providers', message: 'fresh: base_url must be https' });
        await page.waitForFunction(selector => /base_url must be https/.test(document.querySelector(`${selector} .error`).textContent), {}, panel);
        assert.equal(await page.$eval(`${panel} .btn-primary`, el => el.disabled), false, 'a refused list stays a draft');
        await page.close();
      }
      // A list changed elsewhere meanwhile: reload, drop the draft, say so.
      {
        const page = await mount(setup());
        await page.waitForSelector(panel);
        await type(page, `${panel} [name=name]`, 'Mine');
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        await answer(page, await pending(page, 'PUT', putURL), 409, { error: 'stale_providers', message: 'changed' });
        await answer(page, await pending(page, 'GET', '/api/machines/mch_a/launchers'), 200, { launchers: [{ ...opencode, providers: [provider('acme', { name: 'Theirs' }), provider('local', { key_env: '' })], providers_rev: 'p2' }, launchers.mch_a[1]], activity: [] });
        await page.waitForFunction(selector => document.querySelector(`${selector} [name=name]`)?.value === 'Theirs', {}, panel);
        assert.match(await page.$eval('#settings-harnesses > .error', el => el.textContent), /Someone changed the providers of oc1 in the meantime/);
        assert.equal(await page.$eval(`${panel} .btn-primary`, el => el.disabled), true, 'the refused draft is gone');
        await page.close();
      }
      // Removing a provider whose models are still listed offers to remove them with it, and sends remove_models.
      {
        const page = await mount(setup());
        await page.waitForSelector(panel);
        await page.$eval(`${panel} .harness-provider-row[data-provider=acme] .harness-provider-remove`, el => el.click());
        await page.$eval(`${panel} .harness-provider-row[data-provider=local] .harness-provider-remove`, el => el.click());
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        await answer(page, await pending(page, 'PUT', putURL), 409, { error: 'provider_in_use', message: 'acme is in use', provider_id: 'acme', model_count: 1, in_use: [{ provider_id: 'acme', model_count: 1 }, { provider_id: 'local', model_count: 2 }] });
        await page.waitForSelector(`${panel} .harness-provider-remove-models`);
        assert.equal(await page.$eval(`${panel} .harness-provider-remove-models`, el => el.title), 'Remove acme, local with their 3 models', 'the offer names every provider that would lose models');
        assert.match(await page.$eval(`${panel} .harness-provider-use`, el => el.textContent), /acme \(1\), local \(2\) still have models listed/);
        // Any edit withdraws the offer: it was made for the list as it was.
        await page.$eval(`${panel} .harness-provider-add`, el => el.click());
        await settle(page);
        assert.equal(await page.$(`${panel} .harness-provider-remove-models`), null, 'an edit clears the offer');
        await page.$eval(`${panel} .harness-provider-row:last-of-type .harness-provider-remove`, el => el.click());
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        await answer(page, await pending(page, 'PUT', putURL), 409, { error: 'provider_in_use', message: 'x', provider_id: 'acme', model_count: 1, in_use: [{ provider_id: 'acme', model_count: 1 }, { provider_id: 'local', model_count: 2 }] });
        await page.waitForSelector(`${panel} .harness-provider-remove-models`);
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(`${panel} .harness-provider-remove-models`, el => el.click());
        const again = await pending(page, 'PUT', putURL);
        assert.deepEqual(await page.evaluate(index => [window.__requests[index].body.remove_models, window.__requests[index].body.providers.map(item => item.id)], again), [true, []]);
        await page.close();
      }
      // An edit made while a save is in flight must not be offered the removal that was refused for the list that was sent.
      {
        const page = await mount(setup());
        await page.waitForSelector(panel);
        await page.$eval(`${panel} .harness-provider-row[data-provider=acme] .harness-provider-remove`, el => el.click());
        await hold(page, 'GET /api/machines/mch_a/launchers');
        await page.$eval(`${panel} .btn-primary`, el => el.click());
        const put = await pending(page, 'PUT', putURL);
        await type(page, `${panel} .harness-provider-row[data-provider=local] [name=name]`, 'Edited meanwhile');
        await answer(page, put, 409, { error: 'provider_in_use', message: 'acme is in use', provider_id: 'acme', model_count: 1, in_use: [{ provider_id: 'acme', model_count: 1 }] });
        await page.waitForFunction(selector => /changed while it was being saved/.test(document.querySelector(`${selector} .error`).textContent), {}, panel);
        assert.equal(await page.$(`${panel} .harness-provider-remove-models`), null, 'no offer for a list that was edited during the save');
        await page.close();
      }
      // The import: suggestions go into the unsaved list, not the server; one already there, one with a problem and one with an inline key say so.
      {
        const suggestions = { source: '~/.config/opencode/opencode.json', suggestions: [
          { provider: provider('acme'), models: [], already_added: true },
          { provider: provider('lab', { key_env: '' }), models: [{ slug: 'lab/x', label: 'X' }], inline_key: true },
          { provider: provider('broken', { base_url: 'http://example.com/v1' }), models: [], problem: 'plain http is only for this machine' },
          { provider: provider('team'), models: [] }], skipped: [{ id: 'anth', reason: 'not OpenAI-compatible' }] };
        const page = await mount(setup(undefined, { [`POST ${putURL}/import`]: suggestions }));
        await page.waitForSelector(panel);
        await page.$eval(`${panel} .harness-provider-import`, el => el.click());
        await page.waitForSelector(`${panel} .harness-provider-suggestions`);
        assert.deepEqual(await page.$$eval(`${panel} .harness-provider-suggestions input`, inputs => inputs.map(el => [el.getAttribute('aria-label'), el.disabled])), [['Add acme', true], ['Add lab', false], ['Add broken', true], ['Add team', false]]);
        const text = await page.$eval(`${panel} .harness-provider-suggestions`, el => el.textContent);
        assert.match(text, /already in the list/); assert.match(text, /models: lab\/x/, 'the model ids of a suggestion are shown'); assert.match(text, /plain http is only for this machine/); assert.match(text, /set an environment variable and enter its name/); assert.match(text, /Not offered: anth \(not OpenAI-compatible\)/);
        await page.$eval(`${panel} .harness-provider-suggestions [aria-label="Add lab"]`, el => el.click());
        await page.$eval(`${panel} .harness-provider-suggestions [aria-label="Add team"]`, el => el.click());
        await page.$eval(`${panel} .harness-provider-suggestions .btn-primary`, el => el.click());
        await page.waitForFunction(selector => document.querySelectorAll(`${selector} .harness-provider-row`).length === 4, {}, panel);
        assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').length), 0, 'nothing is saved by the import');
        assert.equal(await page.$eval(`${panel} .btn-primary`, el => el.disabled), false, 'the list is an unsaved draft');
        assert.match(await page.$eval(`${panel} [role=status]:last-child`, el => el.textContent), /Save providers to keep them/);
        await page.close();
      }
      // A listing that fails still names the endpoints that could not be asked.
      {
        const page = await mount(setup());
        await page.waitForSelector(`${card('oc1')} .harness-model-detect`);
        await hold(page, 'POST /api/machines/mch_a/launchers/oc1/models/detect');
        await page.$eval(`${card('oc1')} .harness-model-detect`, el => el.click());
        await answer(page, await pending(page, 'POST', '/api/machines/mch_a/launchers/oc1/models/detect'), 502, { error: 'detect_failed', message: "Couldn't detect the models: nothing usable was listed.", endpoint_problems: { local: 'the endpoint answered HTTP 500' } });
        await page.waitForSelector(`${card('oc1')} .harness-endpoint-problems`);
        assert.match(await page.$eval(`${card('oc1')} .harness-endpoint-problems`, el => el.textContent), /local: its endpoint could not be asked \(the endpoint answered HTTP 500\)/);
        await page.close();
      }
      // The import's refusals are shown.
      for (const [status, message, expected] of [[503, 'machine offline', /Can't read the OpenCode config now: machine offline/], [502, 'bad json', /could not be read: bad json/]]) {
        const page = await mount(setup(undefined, { [`POST ${putURL}/import`]: null }));
        await page.waitForSelector(panel);
        await page.$eval(`${panel} .harness-provider-import`, el => el.click());
        await answer(page, await pending(page, 'POST', `${putURL}/import`), status, { error: 'import_failed', message });
        await page.waitForFunction((selector, text) => new RegExp(text).test(document.querySelector(`${selector} .error`).textContent), {}, panel, expected.source);
        await page.close();
      }
      // Detecting models names the providers whose endpoint could not be asked.
      {
        const page = await mount(setup(undefined, { 'POST /api/machines/mch_a/launchers/oc1/models/detect': { candidates: [{ slug: 'acme/big', label: 'Big', already_listed: true }], source: 'harness', endpoint_problems: { local: 'connection refused' } } }));
        await page.waitForSelector(`${card('oc1')} .harness-model-detect`);
        await page.$eval(`${card('oc1')} .harness-model-detect`, el => el.click());
        await page.waitForSelector(`${card('oc1')} .harness-endpoint-problems`);
        assert.match(await page.$eval(`${card('oc1')} .harness-endpoint-problems`, el => el.textContent), /local: its endpoint could not be asked \(connection refused\)/);
        await page.close();
      }
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('harness page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });
