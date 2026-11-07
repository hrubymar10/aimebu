// The whole app with the network down. Work nobody waits for (the agents refresh on its timer) must not leave an unhandled rejection, and is done again
// when the event stream is live; a room a person clicks while the link is down says so in words instead of failing silently; a refusal the server
// gives (a real error) is not swallowed. A scratch server and a real page: only the network is cut, through the browser's own offline mode.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER } = require('./scratch-server.cjs');

(async () => {
  const server = await scratch();
  try {
    const page = await server.open({ width: 1280, height: 900, context: await server.context(), reduced: true });
    // A 300 ms tick instead of the 30 s one, a record of every promise nobody handled, and of every request the page makes (the browser's own resource
    // timing keeps only the first 250, and loading the modules uses them up).
    await page.evaluateOnNewDocument(() => {
      window.__backgroundTickMs = 300; window.__rejections = []; window.__fetches = [];
      const fetchNow = window.fetch.bind(window);
      window.fetch = (input, options) => { window.__fetches.push(new URL(typeof input === 'string' ? input : input.url, location.href).pathname); return fetchNow(input, options); };
      window.addEventListener('unhandledrejection', event => { const reason = event.reason; window.__rejections.push(`${reason && reason.name}: ${reason && reason.message}`); });
    });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'offline-')); execFileSync('git', ['init', '-q'], { cwd: folder });
    const call = (url, method, payload) => page.evaluate(async (url, method, payload) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
      const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
    }, url, method, payload);
    const project = (await call('/api/projects', 'POST', { name: 'Offline', folder })).body;
    const rooms = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body;
    const home = rooms.find(room => room.kind === 'home');
    const other = (await call(`/api/projects/${project.id}/rooms`, 'POST', { title: 'Second' })).body;
    assert.equal((await call('/api/me/preferences', 'PUT', { preferences: { harness_setup_skipped: '1' } })).status, 200);
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    await page.waitForSelector(`[data-room-id="${home.id}"] .room-item-name`);
    await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), home.id);
    await page.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Offline');
    const agentLists = () => page.evaluate(() => window.__fetches.filter(path => /^\/api\/rooms\/[^/]+\/agents$/.test(path)).length);
    // With the link up the timer is asking: wait until it has, so the counts below mean something.
    await page.waitForFunction(() => window.__fetches.filter(path => /^\/api\/rooms\/[^/]+\/agents$/.test(path)).length >= 3, { timeout: 15000 }).catch(async error => { throw new Error(`${error.message}; requests so far: ${JSON.stringify(await page.evaluate(() => window.__fetches.slice(-30)))}`); });
    assert.deepEqual(await page.evaluate(() => window.__rejections), [], 'nothing is unhandled while the network is up');

    // The network goes: the timer fires a dozen times against nothing, and nothing is left unhandled.
    const cdp = await page.createCDPSession(); await cdp.send('Network.enable');
    await cdp.send('Network.emulateNetworkConditions', { offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await new Promise(resolve => setTimeout(resolve, 3500));
    assert.deepEqual(await page.evaluate(() => window.__rejections), [], 'a refresh the network refused leaves no unhandled rejection');

    // A person clicks another room while it is down: it is not opened, and the chat says why. Nothing is unhandled.
    await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), other.id);
    await page.waitForFunction(() => /cannot be reached/.test(document.querySelector('#chat-error')?.textContent || ''), { timeout: 10000 });
    assert.equal(await page.$eval('#room-title', node => node.textContent), 'Offline', 'the room that was open stays open');
    assert.deepEqual(await page.evaluate(() => window.__rejections), [], 'a room opened by a person while the link is down is not an unhandled rejection either');

    // The network is back: the timer's next tick, or the stream going live, asks again, and the refresh goes through.
    await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    const before = await agentLists();
    await page.waitForFunction(count => window.__fetches.filter(path => /^\/api\/rooms\/[^/]+\/agents$/.test(path)).length > count, { timeout: 15000 }, before);
    await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), other.id);
    await page.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Second', { timeout: 15000 });
    assert.deepEqual(await page.evaluate(() => window.__rejections), [], 'and nothing was left over from the outage');
    assert.deepEqual(server.errors.filter(text => !/Failed to load resource|Failed to fetch|WebSocket/i.test(text)), [], 'no page error but the refused requests themselves');
    console.log('offline page: ok');
  } finally { await server.stop(); }
})().catch(error => { console.error(error); process.exit(1); });
