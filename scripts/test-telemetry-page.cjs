// Sentry reporting end to end, in Chromium against a scratch server. Without AIMEBU2_SENTRY_DSN the page never requests the vendored SDK and
// /api/buildinfo carries no `sentry`; with a DSN (pointing at a local stand-in for Sentry) the SDK loads, an error reaches the stand-in, and
// nothing in what it receives holds a query string, a token or a message body. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const http = require('node:http');
const { scratch } = require('./scratch-server.cjs');

const SDK = '/vendor/sentry/bundle.tracing.min.js';

async function visit(server, page, requests) {
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await page.goto(server.origin);
  await page.waitForSelector('#login-view:not([hidden])');
  await server.signIn(page);
  await page.goto(server.origin);
  await page.waitForFunction(() => document.querySelector('#build-version')?.textContent !== 'v2 development', { timeout: 30000 });
}

async function off() {
  const server = await scratch();
  try {
    const page = await server.open();
    const requests = [];
    await visit(server, page, requests);
    const info = await server.api(page, '/api/buildinfo');
    assert.equal(info.status, 200);
    assert.ok(!('sentry' in info.body), 'no DSN: buildinfo has no sentry object');
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.ok(!requests.some(path => path.startsWith('/vendor/sentry')), 'no DSN: the SDK file is never requested');
    assert.equal(await page.evaluate(() => typeof window.Sentry), 'undefined', 'no DSN: no SDK in the page');
  } finally { await server.stop(); }
}

async function on() {
  const received = [];
  const sentry = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => { received.push({ url: req.url, body: Buffer.concat(chunks).toString('latin1') }); res.writeHead(200, { 'Access-Control-Allow-Origin': '*', 'Content-Type': 'application/json' }); res.end('{}'); });
    if (req.method === 'OPTIONS') { res.writeHead(204, { 'Access-Control-Allow-Origin': '*', 'Access-Control-Allow-Headers': '*', 'Access-Control-Allow-Methods': '*' }); res.end(); }
  });
  await new Promise(resolve => sentry.listen(0, '127.0.0.1', resolve));
  const dsn = `http://publickey@127.0.0.1:${sentry.address().port}/7`;
  const server = await scratch({ env: { AIMEBU2_SENTRY_DSN: dsn, AIMEBU2_SENTRY_ENV: 'page-test', AIMEBU2_SENTRY_TRACES_SAMPLE_RATE: '1' } });
  try {
    const page = await server.open();
    const requests = [];
    // The page runs under its Content-Security-Policy, which names the ingest origin of the DSN while reporting is on: the SDK loads, the error and
    // the transaction below reach the stand-in, and the browser reports no violation of the policy on the way.
    await page.evaluateOnNewDocument(() => { window.__csp = []; document.addEventListener('securitypolicyviolation', event => window.__csp.push(`${event.violatedDirective} ${event.blockedURI}`)); });
    await visit(server, page, requests);
    const policy = await page.evaluate(async () => (await fetch('/')).headers.get('content-security-policy'));
    assert.ok(policy.includes(`connect-src 'self'`) && policy.includes(`http://127.0.0.1:${sentry.address().port}`), `the policy names the ingest origin: ${policy}`);
    assert.ok(policy.includes(`worker-src 'self' blob:`), policy);
    const info = await server.api(page, '/api/buildinfo');
    assert.equal(info.body.sentry.dsn, dsn); assert.equal(info.body.sentry.environment, 'page-test');
    await page.waitForFunction(() => typeof window.Sentry?.getClient === 'function' && window.Sentry.getClient(), { timeout: 30000 });
    assert.ok(requests.includes(SDK), 'with a DSN the SDK file is requested');
    const options = await page.evaluate(() => { const o = window.Sentry.getClient().getOptions(); return { dsn: o.dsn, sendDefaultPii: o.sendDefaultPii, environment: o.environment }; });
    assert.deepEqual(options, { dsn, sendDefaultPii: false, environment: 'page-test' });
    // sendDefaultPii does nothing in this SDK: what it collects is dataCollection, and the SDK's own reading of it must be all off.
    const collected = await page.evaluate(() => window.Sentry.getClient().getDataCollectionOptions());
    assert.equal(collected.userInfo, false); assert.equal(collected.cookies, false); assert.equal(collected.urlQueryParams, false);
    assert.equal(collected.httpHeaders.request, false); assert.equal(collected.httpHeaders.response, false); assert.deepEqual(collected.httpBodies, []);
    // A request with a query string and a fragment, so a span for it is made (traces are sampled at 1 in this test).
    await page.evaluate(async () => { await fetch('/api/buildinfo?token=SECRET-QUERY-VALUE#SECRET-FRAGMENT'); });
    await page.evaluate(() => { setTimeout(() => { throw new Error('page-test boom token=hunter2 at /Users/alice/x'); }, 0); });
    await page.waitForFunction(() => true);
    for (let i = 0; i < 100 && !received.some(item => item.body.includes('page-test boom')); i++) await new Promise(resolve => setTimeout(resolve, 100));
    const error = received.find(item => item.body.includes('page-test boom'));
    assert.ok(error, `the error reached Sentry; got ${received.length} envelopes`);
    assert.ok(error.url.startsWith('/api/7/envelope/'), error.url);
    // The page-load / fetch transaction goes out too (traces at 1); give it time to finish its idle wait.
    // (The server reports to the same stand-in, with the Go SDK; the browser's envelopes carry sentry.javascript.browser.)
    const browserTransaction = () => received.some(item => item.body.includes('sentry.javascript.browser') && item.body.includes('"type":"transaction"'));
    for (let i = 0; i < 100 && !browserTransaction(); i++) await new Promise(resolve => setTimeout(resolve, 200));
    assert.ok(browserTransaction(), 'the browser sent a transaction (static lifecycle: the scrubber sees its spans)');
    assert.deepEqual(await page.evaluate(() => window.__csp), [], 'the policy blocked nothing the page did');
    assert.ok(!received.some(item => item.body.includes('sentry.javascript.browser') && item.body.includes('"type":"span"')), 'no spans are sent on their own (stream lifecycle skips the scrubber)');
    const host = new URL(server.origin).host;
    for (const item of received) {
      for (const leak of ['hunter2', '/Users/alice', 'Authorization', 'Cookie', 'cookie:', '?token', 'X-CSRF', 'SECRET-QUERY-VALUE', 'SECRET-FRAGMENT', host, 'HeadlessChrome', 'User-Agent', 'user_agent', '"ip_address"', 'server.address', '/api/buildinfo?'])
        assert.ok(!item.body.includes(leak), `${leak} must not be sent: ...${item.body.slice(Math.max(0, item.body.indexOf(leak) - 160), item.body.indexOf(leak) + 120)}...`);
      for (const infer of item.body.match(/"infer_ip":"[a-z]+"/g) || []) assert.equal(infer, '"infer_ip":"never"', 'the IP address is never inferred');
    }
    assert.ok(received.some(item => item.body.includes('[redacted]')), 'the secret in the message was redacted, not dropped whole');
  } finally { await server.stop(); sentry.close(); }
}

// The consent popup: shown to the owner once after the welcome, answered with Save, then the same two fields in Settings -> General.
async function popup() {
  const server = await scratch({ env: { AIMEBU2_SENTRY_DSN: null } }); // the variable is unset: Settings decide, and nothing is reported before the popup is answered
  try {
    const page = await server.open();
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await server.signIn(page);
    await page.goto(server.origin);
    await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue');
    await page.waitForSelector('#app-dialog[open]', { timeout: 30000 });
    const dialog = await page.evaluate(() => ({ title: document.querySelector('#app-dialog-title').textContent, text: document.querySelector('.app-dialog-message').textContent,
      checked: document.querySelector('#app-dialog input[type=checkbox]').checked, app: document.querySelector('#app-view').hidden }));
    assert.equal(dialog.title, 'Error reports'); assert.match(dialog.text, /Never sent: messages, prompts, tokens, file contents/);
    const before = (await server.api(page, '/api/telemetry/settings')).body;
    assert.equal(dialog.checked, !!before.default_dsn, 'the toggle starts on exactly when this build ships a default DSN'); assert.equal(dialog.app, true, 'the app waits behind the popup');
    assert.equal(await page.$eval('#app-dialog input[type=text]', el => el.value), before.default_dsn, 'the field holds the default');
    assert.equal(before.active, false, 'nothing is reported before the popup is answered');
    if (!dialog.checked) await page.click('#app-dialog input[type=checkbox]');
    await page.$eval('#app-dialog input[type=text]', el => { el.value = ''; });
    await page.type('#app-dialog input[type=text]', 'http://pagekey@127.0.0.1:9/5');
    await page.click('.app-dialog-ok');
    await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
    const saved = (await server.api(page, '/api/telemetry/settings')).body;
    assert.deepEqual({ prompted: saved.prompted, enabled: saved.enabled, dsn: saved.dsn, active: saved.active }, { prompted: true, enabled: true, dsn: 'http://pagekey@127.0.0.1:9/5', active: true });
    // Asked once: a reload goes straight to the app.
    await page.reload();
    await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
    assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog[open]')), false, 'the popup comes once');
    // The same fields in Settings -> General, editable.
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.waitForSelector('#error-reports-dsn');
    assert.equal(await page.$eval('#error-reports-dsn', el => el.value), 'http://pagekey@127.0.0.1:9/5');
    assert.equal(await page.$eval('#error-reports-enabled', el => el.checked), true);
    await page.click('#error-reports-enabled');
    await page.click('#error-reports-save');
    await page.waitForFunction(() => /Error reports are off/.test(document.querySelector('#error-reports-section [role=status]').textContent));
    assert.equal((await server.api(page, '/api/telemetry/settings')).body.active, false, 'turning it off in Settings stops reporting at once');
    await page.$eval('#error-reports-dsn', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.type('#error-reports-dsn', 'nope');
    await page.click('#error-reports-enabled'); await page.click('#error-reports-save');
    await page.waitForFunction(() => /not a Sentry DSN/.test(document.querySelector('#error-reports-error').textContent));
    assert.deepEqual(server.errors, [], 'no page errors');
  } finally { await server.stop(); }
}

(async () => { await off(); await on(); await popup(); console.log('telemetry page tests passed'); })().catch(error => { console.error(error); process.exit(1); });
