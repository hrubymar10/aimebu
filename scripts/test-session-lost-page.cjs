// A session that is gone (revoked, expired, the database replaced under an open tab): the first 401 on a call made while signed in sends the page to
// sign-in, as signing out does; nothing polls after that, and the rejection that reaches a caller nobody waits for is not an unhandled error.
// Against a scratch server: the page's cookie is dropped, so the server answers 401 from then on. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function main() {
  const server = await scratch({ env: MODULES }); // the page's own api.js is reached through its module URL, so the bundle is off
  try {
    const page = await server.open({ width: 1280, height: 800, context: await server.context() });
    await page.evaluateOnNewDocument(() => { globalThis.__backgroundTickMs = 300; }); // the 30 s polling tick, shortened
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    let apiCalls = []; page.on('request', request => { const path = new URL(request.url()).pathname; if (path.startsWith('/api/')) apiCalls.push(`${request.method()} ${path}`); });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    await sleep(1500); // the app is up and polling
    assert.ok(apiCalls.some(call => call === 'GET /api/health'), `the page polls health while signed in: ${apiCalls.slice(-8)}`);

    // The session is revoked: the server answers 401 to everything from now on.
    await page.deleteCookie(...await page.cookies());
    await page.waitForSelector('#login-view:not([hidden])', { timeout: 15000 });
    assert.equal(await page.evaluate(() => document.querySelector('#app-view').hidden), true, 'the app is gone and sign-in shows');
    // Nothing polls any more: several ticks pass with no authenticated call (the sign-in page asks only about the session).
    await sleep(800); apiCalls = [];
    await sleep(2500);
    const polls = apiCalls.filter(call => !/\/api\/(session|login|setup)/.test(call));
    assert.deepEqual(polls, [], `no polling after the session is lost: ${polls}`);
    assert.deepEqual(errors, [], 'no unhandled rejection or error reached the page');
    // Several calls in flight when the session goes: every one is marked as a lost session, and the page is sent to sign-in once, not once per call.
    // (Run from a fresh sign-in, then drop the cookie again.)
    const typeLogin = async () => {
      await page.$eval('#login-form [name=login]', el => { el.value = ''; }); await page.$eval('#login-form [name=password]', el => { el.value = ''; });
      await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
      await page.$eval('#login-form', form => form.requestSubmit());
      await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
    };
    await typeLogin(); await sleep(1500);
    await page.setRequestInterception(true);
    const held = [];
    page.on('request', request => { if (/\/api\/me$/.test(new URL(request.url()).pathname) && request.method() === 'GET' && held.hold) held.push(request); else request.continue().catch(() => {}); });
    const api = () => page.evaluate(async () => {
      const { request } = await import('/js/api.js' + new URL(document.querySelector('script[type=module]').src).search);
      window.__lostCalls = 0; window.__api = request; return true;
    });
    await api();
    // A call sent with this session and held on its way; then the session is lost by other calls (all concurrent), and the person signs in again.
    held.hold = true;
    await page.evaluate(() => { window.__late = window.__api('/api/me').then(() => 'ok', error => `${error.status}:${!!error.unauthorized}`); });
    await page.waitForFunction(() => true); while (held.length < 1) await sleep(50);
    held.hold = false;
    await page.deleteCookie(...await page.cookies());
    const outcomes = await page.evaluate(async () => (await Promise.all([window.__api('/api/telemetry/settings'), window.__api('/api/health'), window.__api('/api/projects'), window.__api('/api/me/preferences')].map(call => call.then(() => 'ok', error => `${error.status}:${!!error.unauthorized}`)))));
    assert.deepEqual(outcomes, ['401:true', '401:true', '401:true', '401:true'], 'every call in flight is marked as a lost session, not only the first');
    await page.waitForSelector('#login-view:not([hidden])', { timeout: 15000 });
    assert.deepEqual(errors, [], 'no page error from the concurrent calls');
    await typeLogin(); // a new session
    // The held call was sent with the old session: its 401 arrives now, is marked, and must not sign the new session out.
    await held[0].respond({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: 'unauthorized', message: 'sign in' }) });
    assert.equal(await page.evaluate(() => window.__late), '401:true', 'the late 401 is marked');
    await sleep(800);
    assert.equal(await page.evaluate(() => document.querySelector('#app-view').hidden), false, 'a late 401 from before the sign-out does not end the new session');
    assert.deepEqual(errors, []);
    assert.deepEqual(server.errors, []);
    console.log('session lost page test passed');
  } finally { await server.stop(); }
}
main().catch(error => { console.error(error); process.exit(1); });
