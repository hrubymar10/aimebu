// The real app from its bundle, in Chromium, on a scratch server: the page fetches one script and no module of the app, the bundle comes gzipped with
// the version in its name, and the app works from it (sign in, a room, a message, the Settings, Room settings and Spawn dialogs) without a page error.
// A second server with AIMEBU_WEB_BUNDLE=off proves the switch: its page loads the modules and not the bundle. The tests of the app's own modules
// (test-ui, the fixture page tests) run on modules, because they reach into module instances a bundle does not have.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');

// What the page requests while it loads and signs in, as the browser sees it.
async function load(server, label) {
  const page = await server.open({ width: 1280, height: 900, context: await server.context() });
  // `requests` is what one load of the page asked for (the test clears it before a reload); `every` is everything the page asked for, from the first
  // visit to the last click, so a module of the app fetched at any point while bundled fails the test.
  const requests = [], every = [], responses = new Map();
  page.on('request', request => { const pathname = new URL(request.url()).pathname; requests.push(pathname); every.push(pathname); });
  page.on('response', response => responses.set(new URL(response.url()).pathname, response));
  await page.goto(server.origin);
  await page.waitForSelector('#login-view:not([hidden])');
  await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
  await page.$eval('#login-form', form => form.requestSubmit());
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
  await page.waitForSelector('#app-view:not([hidden])');
  return { page, requests, every, responses, label };
}

async function main() {
  const server = await scratch(); // the bundle is the default
  try {
    const { page, requests, every, responses } = await load(server, 'bundle');
    // A project and a room with a message, then a reload: the page loads again, signed in, from the bundle.
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'bundle-')); execFileSync('git', ['init', '-q'], { cwd: folder });
    const call = (url, method, payload) => page.evaluate(async (url, method, payload) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
      const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
    }, url, method, payload);
    const project = (await call('/api/projects', 'POST', { name: 'Bundle', folder })).body;
    const flow = (await call(`/api/projects/${project.id}/team-flow`, 'GET')).body;
    assert.equal((await call(`/api/projects/${project.id}/team-flow`, 'PUT', { key: 'custom', rev: flow.rev })).status, 200);
    const home = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body.find(room => room.kind === 'home');
    assert.equal((await call(`/api/rooms/${home.id}/messages`, 'POST', { body: 'a message from the bundle' })).status, 201);
    assert.equal((await call('/api/me/preferences', 'PUT', { preferences: { harness_setup_skipped: '1' } })).status, 200);
    requests.length = 0; responses.clear();
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    await page.waitForSelector(`[data-room-id="${home.id}"] .room-item-name`);
    await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), home.id);
    await page.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Bundle' && document.querySelectorAll('#messages .chat-msg').length >= 1);

    const modules = requests.filter(pathname => pathname.startsWith('/js/'));
    const scripts = requests.filter(pathname => pathname.endsWith('.js') || pathname.endsWith('.mjs'));
    assert.deepEqual(modules, [], `the page fetched modules of the app: ${modules.slice(0, 5)}`);
    assert.deepEqual(requests.filter(pathname => pathname === '/bundle/app.js'), ['/bundle/app.js'], 'the bundle is fetched once');
    assert.ok(requests.length <= 60, `a page load is ${requests.length} requests (it was about 290 with the modules): ${requests.join(' ')}`);
    assert.ok(scripts.length <= 6, `scripts fetched: ${scripts}`); // the entry and the chunks it shares with the lazy ones
    assert.ok(!scripts.some(pathname => /settings-dialog|import-v1/.test(pathname)), `Settings or the import wizard was fetched before it was needed: ${scripts}`);
    const bundle = responses.get('/bundle/app.js');
    assert.equal(bundle.headers()['content-encoding'], 'gzip', 'the bundle is gzipped');
    assert.equal(bundle.headers()['cache-control'], 'public, max-age=31536000, immutable', 'and cached for good under its version');
    assert.ok(/^\/bundle\/app\.js$/.test(new URL(bundle.url()).pathname) && /\?v=[0-9a-f]+$/.test(new URL(bundle.url()).search), 'under a name that carries the version');
    assert.ok(!requests.includes('/bundle/app.js.map'), 'the source map is fetched only by developer tools');
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('script[src]')].map(script => new URL(script.src).pathname).join()), '/bundle/app.js', 'the page has one script');
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('link[rel=modulepreload]')].filter(link => !new URL(link.href).pathname.startsWith('/bundle/chunk-')).length), 0, 'and no hint for modules it does not fetch (only for the entry\'s shared chunks)');

    // The app works: a message goes out, the dialogs open (they are built from the bundled modules), nothing is thrown.
    await page.$eval('#composer textarea[name=body]', node => { node.value = 'sent from the bundle'; node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.$eval('#composer button[type=submit]', node => node.click());
    await page.waitForFunction(() => [...document.querySelectorAll('#messages .chat-msg')].some(node => node.textContent.includes('sent from the bundle')));
    await page.$eval('#room-info', node => node.click());
    await page.waitForFunction(() => document.querySelector('#room-info-dialog').open && document.querySelector('#room-info-title').textContent === 'Project settings');
    await page.$eval('#room-info-close', node => node.click());
    await page.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
    await page.$eval('#settings-toggle', node => node.click());
    await page.waitForFunction(() => document.querySelector('#settings-dialog').open);
    assert.ok(every.some(pathname => /^\/bundle\/settings-dialog-[A-Za-z0-9]+\.js$/.test(pathname)), 'Settings is a chunk fetched when it is opened');
    await page.$eval('#settings-close', node => node.click());
    await page.$eval('#spawn-open', node => node.click());
    await page.waitForFunction(() => document.querySelector('#spawn-dialog').open);
    // A lazily fetched vendored library still loads (the emoji picker's data is a file of its own; the editor and highlighter are fetched when needed).
    await page.$eval('#emoji-button', node => node.click());
    await page.waitForSelector('.emoji-picker .emoji-cell');
    assert.deepEqual(server.errors.filter(text => !/Failed to load resource/.test(text)), [], 'no page error from the bundle');
    // Nothing the page did, from its first visit through the dialogs and the lazily loaded picker, fetched a module of the app.
    assert.deepEqual(every.filter(pathname => /^\/js\/.*\.m?js$/.test(pathname)), [], 'a module of the app was requested while the page was bundled');

    // The source map: the app's files by name, nothing of this machine.
    const map = await (await fetch(`${server.origin}/bundle/app.js.map`)).json();
    assert.ok(map.sources.some(source => source.endsWith('js/chat.js')) && !map.sources.some(source => source.includes(os.homedir())), 'the source map names the app\'s files');
  } finally { await server.stop(); }

  // The switch: with the bundle off the same page loads the modules and not the bundle.
  const modules = await scratch({ env: MODULES });
  try {
    const { requests } = await load(modules, 'modules');
    assert.ok(requests.filter(pathname => pathname.startsWith('/js/')).length > 150, 'with the bundle off the page loads the modules');
    assert.ok(!requests.includes('/bundle/app.js'), 'and not the bundle');
  } finally { await modules.stop(); }
  console.log('bundle page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });
