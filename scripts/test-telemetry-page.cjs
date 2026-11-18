// Sentry reporting end to end, in Chromium against a scratch server. Without AIMEBU_SENTRY_DSN the page never requests the vendored SDK and
// /api/buildinfo carries no `sentry`; with a DSN (pointing at a local stand-in for Sentry) the SDK loads, an error reaches the stand-in, and
// nothing in what it receives holds a query string, a token or a message body. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch } = require('./scratch-server.cjs');

const SDK = '/vendor/sentry/bundle.tracing.min.js';

async function visit(server, page, requests) {
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await page.goto(server.origin);
  await page.waitForSelector('#login-view:not([hidden])');
  await server.signIn(page);
  await page.goto(server.origin);
  await page.waitForFunction(() => document.querySelector('#build-version')?.textContent !== 'v2', { timeout: 30000 });
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
  const server = await scratch({ env: { AIMEBU_SENTRY_DSN: dsn, AIMEBU_SENTRY_ENV: 'page-test', AIMEBU_SENTRY_TRACES_SAMPLE_RATE: '1' } });
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
    // The span budget: one sampled page load is a handful of spans (no span per loaded file), and the API requests it made are among them.
    const transactions = () => received.filter(item => item.body.includes('sentry.javascript.browser')).flatMap(item => item.body.split('\n').flatMap(line => { try { const parsed = JSON.parse(line); return parsed.type === 'transaction' ? [parsed] : []; } catch { return []; } }));
    for (let i = 0; i < 100 && !transactions().some(item => item.contexts?.trace?.op === 'pageload'); i++) await new Promise(resolve => setTimeout(resolve, 200));
    const pageload = transactions().find(item => item.contexts?.trace?.op === 'pageload');
    assert.ok(pageload, 'the page load is a transaction');
    assert.ok(pageload.spans.length <= 10, `a sampled page load makes at most ~10 spans, got ${pageload.spans.length}: ${pageload.spans.map(span => span.op).join(', ')}`);
    assert.deepEqual(pageload.spans.filter(span => /^resource\./.test(span.op)), [], 'no span per loaded file');
    assert.ok(pageload.spans.some(span => span.op === 'http.client'), 'the API requests of the load stay');
    assert.deepEqual(await page.evaluate(() => window.__csp), [], 'the policy blocked nothing the page did');
    assert.ok(!received.some(item => item.body.includes('sentry.javascript.browser') && item.body.includes('"type":"span"')), 'no spans are sent on their own (stream lifecycle skips the scrubber)');
    const host = new URL(server.origin).host;
    for (const received_item of received) {
      // The Go server names the unspecified address on purpose (it stops Sentry placing the event by the server's IP); any other address is a leak.
      const item = { ...received_item, body: received_item.body.replaceAll('"ip_address":"0.0.0.0"', '') };
      for (const leak of ['hunter2', '/Users/alice', 'Authorization', 'Cookie', 'cookie:', '?token', 'X-CSRF', 'SECRET-QUERY-VALUE', 'SECRET-FRAGMENT', host, 'HeadlessChrome', 'User-Agent', 'user_agent', '"ip_address":"', 'server.address', '/api/buildinfo?'])
        assert.ok(!item.body.includes(leak), `${leak} must not be sent: ...${item.body.slice(Math.max(0, item.body.indexOf(leak) - 160), item.body.indexOf(leak) + 120)}...`);
      for (const infer of item.body.match(/"infer_ip":"[a-z]+"/g) || []) assert.equal(infer, '"infer_ip":"never"', 'the IP address is never inferred');
    }
    // No IP inference, on any kind of envelope: the browser sends no session and no client report (a session carries "{{auto}}"), and every event, error or
    // transaction, says infer_ip "never" and names no user address.
    const browserItems = received.filter(item => item.body.includes('sentry.javascript.browser')).flatMap(item => item.body.split('\n').flatMap(line => { try { const parsed = JSON.parse(line); return [parsed]; } catch { return []; } }));
    assert.ok(!received.some(item => item.body.includes('{{auto}}')), 'nothing asks Sentry to infer an address');
    assert.deepEqual(browserItems.filter(item => ['session', 'sessions', 'client_report'].includes(item.type)), [], 'no session or client-report items');
    const events = browserItems.filter(item => item.event_id && item.sdk && (item.exception || item.type === 'transaction'));
    assert.ok(events.some(item => item.exception) && events.some(item => item.type === 'transaction'), 'both an error and a transaction were checked');
    const fingerprint = (await server.api(page, '/api/me/telemetry')).body.fingerprint;
    assert.match(fingerprint, /^[0-9a-f]{16}$/, 'a random 16-hex fingerprint');
    for (const event of events) { assert.equal(event.sdk?.settings?.infer_ip, 'never', 'infer_ip is never on an event'); assert.deepEqual(event.user, { id: fingerprint, ip_address: null }, 'the fingerprint as user.id, an explicitly absent address, and nothing else'); }
    assert.ok(received.some(item => item.body.includes('[redacted]')), 'the secret in the message was redacted, not dropped whole');

    // Who reports name: the server's own transactions carry the same fingerprint, and only that.
    const parsedItems = () => received.flatMap(item => item.body.split('\n').flatMap(line => { try { return [JSON.parse(line)]; } catch { return []; } }));
    for (let i = 0; i < 100 && !parsedItems().some(item => item.platform === 'go' && item.transaction === 'GET /api/me/telemetry'); i++) await new Promise(resolve => setTimeout(resolve, 100));
    const serverTransaction = parsedItems().find(item => item.platform === 'go' && item.transaction === 'GET /api/me/telemetry');
    assert.ok(serverTransaction, 'the server reported the identity request'); assert.deepEqual(serverTransaction.user, { id: fingerprint, ip_address: '0.0.0.0' }, 'a server report names its person by the fingerprint and the unspecified address, nothing else');
    // The page follows a change at once, without a reload: the user name when chosen, the new fingerprint after a roll.
    const errorUser = async message => {
      await page.evaluate(text => { setTimeout(() => { throw new Error(text); }, 0); }, message);
      for (let i = 0; i < 100 && !parsedItems().some(item => item.exception && JSON.stringify(item.exception).includes(message)); i++) await new Promise(resolve => setTimeout(resolve, 100));
      const event = parsedItems().find(item => item.exception && JSON.stringify(item.exception).includes(message));
      assert.ok(event, `${message} reached Sentry`);
      return event.user;
    };
    // Event timing (the INP's source): every press on this page, with the SDK running and the CPU four times slower, is handled and painted fast.
    await page.evaluate(() => { window.__timing = []; new PerformanceObserver(list => { for (const entry of list.getEntries()) window.__timing.push([entry.name, entry.target?.id || '', entry.duration]); }).observe({ type: 'event', durationThreshold: 16 }); });
    const cdp = await page.createCDPSession(); await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 });
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.click('#settings-telemetry-tab');
    await page.waitForFunction(() => document.querySelector('#telemetry-fingerprint')?.value);
    assert.deepEqual(await page.evaluate(() => ({ value: document.querySelector('#telemetry-fingerprint').value, readOnly: document.querySelector('#telemetry-fingerprint').readOnly, sendName: document.querySelector('#telemetry-send-name').checked })), { value: fingerprint, readOnly: true, sendName: false }, 'the field shows the fingerprint, read-only; the name is off by default');
    await page.click('#telemetry-send-name');
    await page.waitForFunction(() => /user name/.test(document.querySelector('#telemetry-status').textContent));
    assert.match(await page.evaluate(() => document.querySelector('#telemetry-fingerprint-desc').textContent), /login name/, 'the row describes the name mode');
    assert.deepEqual(await page.evaluate(() => ({ value: document.querySelector('#telemetry-fingerprint').value, readOnly: document.querySelector('#telemetry-fingerprint').readOnly, label: document.querySelector('label[for=telemetry-fingerprint]').textContent, copy: document.querySelector('#telemetry-fingerprint-copy'), dice: document.querySelector('#telemetry-fingerprint-roll').getAttribute('aria-label') })),
      { value: 'owner', readOnly: true, label: 'User name', copy: null, dice: 'Use a random fingerprint instead' }, 'with the name on, the field shows the user name, there is nothing to copy, and the dice says what it does');
    assert.ok(!await page.evaluate(fp => document.querySelector('#settings-telemetry').textContent.includes(fp) || [...document.querySelectorAll('#settings-telemetry input')].some(input => input.value === fp), fingerprint), 'the hash is not on the page while the name is on');
    assert.deepEqual(await errorUser('after-name boom'), { id: 'owner', ip_address: null }, 'with the name on, user.id is the login (not the fingerprint), and nothing else');
    assert.ok(!parsedItems().some(item => item.exception && JSON.stringify(item.exception).includes('after-name') && JSON.stringify(item).includes(fingerprint)), 'never both');
    // Rolling with the name on asks first; cancelling changes nothing.
    await page.click('#telemetry-fingerprint-roll');
    await page.waitForSelector('#app-dialog[open]');
    assert.match(await page.evaluate(() => document.querySelector('.app-dialog-message').textContent), /Rolling a new fingerprint turns off sending your user name and gives you a new anonymous id/);
    await page.click('.app-dialog-cancel');
    await page.waitForFunction(() => !document.querySelector('#app-dialog[open]'));
    assert.deepEqual((await server.api(page, '/api/me/telemetry')).body.fingerprint, fingerprint, 'cancel: the same fingerprint');
    assert.deepEqual(await page.evaluate(() => ({ value: document.querySelector('#telemetry-fingerprint').value, sendName: document.querySelector('#telemetry-send-name').checked })), { value: 'owner', sendName: true }, 'cancel leaves everything as it was (the name still shown)');
    // Confirming does both: the name goes off and the fingerprint is a new one.
    await page.click('#telemetry-fingerprint-roll');
    await page.waitForSelector('#app-dialog[open]');
    await page.click('.app-dialog-ok');
    await page.waitForFunction(() => document.querySelector('#telemetry-fingerprint').value !== 'owner');
    const rolled = await page.$eval('#telemetry-fingerprint', el => el.value);
    assert.match(rolled, /^[0-9a-f]{16}$/);
    assert.equal(await page.$eval('#telemetry-send-name', el => el.checked), false, 'the name is off after a confirmed roll');
    assert.deepEqual(await errorUser('after-roll boom'), { id: rolled, ip_address: null }, 'the new fingerprint, no reload needed');
    // Switching the name on and off again: the name while on, the hash again when off.
    await page.click('#telemetry-send-name'); await page.waitForFunction(() => document.querySelector('#telemetry-fingerprint').value === 'owner');
    await page.click('#telemetry-send-name'); await page.waitForFunction(rolledValue => document.querySelector('#telemetry-fingerprint').value === rolledValue, {}, rolled);
    // Rolling with the name off needs no question.
    await page.click('#telemetry-fingerprint-roll');
    await page.waitForFunction(old => document.querySelector('#telemetry-fingerprint').value !== old, {}, rolled);
    assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog[open]')), false, 'no question when the name is off');
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 });
    const presses = (await page.evaluate(() => window.__timing)).filter(([name]) => name === 'click' || name === 'keydown' || name === 'pointerdown');
    assert.ok(presses.some(([, id]) => id === 'telemetry-send-name') && presses.some(([, id]) => id === 'telemetry-fingerprint-roll'), `the presses were measured: ${JSON.stringify(presses)}`);
    assert.ok(presses.every(([, , duration]) => duration < 200), `every press is handled and painted in under 200 ms: ${JSON.stringify(presses.filter(([, , duration]) => duration >= 200))}`);
    // Whatever was sent, a user is only ever an id (and the browser's explicitly absent address).
    for (const item of parsedItems()) if (item.user) assert.ok(Object.keys(item.user).every(key => key === 'id' || key === 'ip_address'), `a user is only an id: ${JSON.stringify(item.user)}`);
  } finally { await server.stop(); sentry.close(); }
}

// The consent popup: shown to the owner once after the welcome, answered with Save, then the same two fields in Settings -> General.
async function popup() {
  const server = await scratch({ env: { AIMEBU_SENTRY_DSN: null } }); // the variable is unset: Settings decide, and nothing is reported before the popup is answered
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
    assert.equal(dialog.title, 'Telemetry'); assert.match(dialog.text, /Never sent: messages, prompts, tokens, file contents/);
    const before = (await server.api(page, '/api/telemetry/settings')).body;
    assert.equal(dialog.checked, !!before.default_dsn, 'the toggle starts on exactly when this build ships a default DSN'); assert.equal(dialog.app, true, 'the app waits behind the popup');
    assert.equal(await page.$eval('#app-dialog input[type=text]', el => el.value), before.default_dsn, 'the field holds the default');
    assert.equal(before.active, false, 'nothing is reported before the popup is answered');
    // The reset button sits to the left of the field; turning reports off greys the field and the button out (they stay), turning them on frees them.
    assert.ok(before.default_dsn, 'this build ships a default DSN (the reset button is tested against it)');
    const dsnState = () => page.evaluate(() => {
      const input = document.querySelector('#app-dialog input[type=text]'), reset = document.querySelector('#app-dialog .app-dialog-reset');
      return { fieldDisabled: input.disabled, fieldHidden: input.closest('.app-dialog-field').hidden, resetHidden: reset.hidden, resetDisabled: reset.disabled, resetLeft: reset.getBoundingClientRect().right <= input.getBoundingClientRect().left,
        label: reset.getAttribute('aria-label'), title: reset.title, value: input.value };
    });
    assert.deepEqual(await dsnState(), { fieldDisabled: !dialog.checked, fieldHidden: false, resetHidden: false, resetDisabled: true, resetLeft: true, label: 'Reset to default DSN', title: 'Reset to default DSN', value: before.default_dsn },
      'the field holds the default; the reset button is to its left and off, since the field already holds the default');
    await page.click('#app-dialog input[type=checkbox]'); // off (it starts on with a default)
    assert.deepEqual(await dsnState(), { fieldDisabled: true, fieldHidden: false, resetHidden: false, resetDisabled: true, resetLeft: true, label: 'Reset to default DSN', title: 'Reset to default DSN', value: before.default_dsn }, 'off: disabled, not hidden');
    await page.click('#app-dialog input[type=checkbox]'); // on again
    assert.equal((await dsnState()).fieldDisabled, false, 'on: the field is editable');
    await page.$eval('#app-dialog input[type=text]', el => { el.value = ''; });
    await page.type('#app-dialog input[type=text]', 'http://pagekey@127.0.0.1:9/5');
    assert.equal((await dsnState()).resetDisabled, false, 'a changed DSN can be reset');
    await page.click('#app-dialog .app-dialog-reset');
    assert.equal((await dsnState()).value, before.default_dsn, 'Reset puts the default back'); assert.equal((await dsnState()).resetDisabled, true);
    await page.$eval('#app-dialog input[type=text]', el => { el.value = ''; });
    await page.type('#app-dialog input[type=text]', 'http://pagekey@127.0.0.1:9/5');
    // Off disables the reset button too even though the field holds something other than the default (so the button would otherwise be useful).
    await page.click('#app-dialog input[type=checkbox]');
    assert.deepEqual(await page.evaluate(() => ({ field: document.querySelector('#app-dialog input[type=text]').disabled, reset: document.querySelector('#app-dialog .app-dialog-reset').disabled, value: document.querySelector('#app-dialog input[type=text]').value })), { field: true, reset: true, value: 'http://pagekey@127.0.0.1:9/5' }, 'off with a custom DSN: field and reset both disabled');
    await page.click('#app-dialog input[type=checkbox]');
    assert.equal((await dsnState()).resetDisabled, false, 'on again: the reset button is back');
    // The identity switch of the owner's own account sits under the DSN: off by default, greyed out while reports are off, worded as Settings -> Telemetry words it.
    const nameSwitch = () => page.evaluate(() => { const box = document.querySelectorAll('#app-dialog input[type=checkbox]')[1], dsn = document.querySelector('#app-dialog input[type=text]'); return { there: !!box, checked: box?.checked, disabled: box?.disabled, below: box ? box.getBoundingClientRect().top > dsn.getBoundingClientRect().top : null, label: box?.closest('label')?.textContent }; });
    assert.deepEqual(await nameSwitch(), { there: true, checked: false, disabled: false, below: true, label: 'Send my user name instead of a fingerprint' + 'Off by default. Your reports then carry your login name (nothing else) in place of the fingerprint, never both.' }, 'the popup has the name switch, off by default');
    await page.click('#app-dialog input[type=checkbox]'); // reports off
    assert.equal((await nameSwitch()).disabled, true, 'greyed out while Send telemetry is off');
    await page.click('#app-dialog input[type=checkbox]'); // on again
    assert.equal((await server.api(page, '/api/me/telemetry')).body.send_name, false, 'nothing is sent before Save');
    await page.evaluate(() => document.querySelectorAll('#app-dialog input[type=checkbox]')[1].click());
    assert.equal((await nameSwitch()).checked, true);
    await page.click('.app-dialog-ok');
    await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
    assert.equal((await server.api(page, '/api/me/telemetry')).body.send_name, true, 'Save with the switch ticked makes this account\'s reports carry its user name');
    assert.equal((await (await server.ownerClient())('/api/me/telemetry', { method: 'PUT', body: { send_name: false } })).status, 200); // the rest of this scenario starts from the fingerprint, as a new account does
    const saved = (await server.api(page, '/api/telemetry/settings')).body;
    assert.deepEqual({ prompted: saved.prompted, enabled: saved.enabled, dsn: saved.dsn, active: saved.active }, { prompted: true, enabled: true, dsn: 'http://pagekey@127.0.0.1:9/5', active: true });
    // Asked once: a reload goes straight to the app.
    await page.reload();
    await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
    assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog[open]')), false, 'the popup comes once');
    // The same fields in Settings -> General, editable.
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    // Its own section, not part of General: a nav entry of its own, nothing about it left under General.
    await page.waitForSelector('#settings-telemetry-tab:not([hidden])');
    assert.equal(await page.evaluate(() => document.querySelector('#settings-general').querySelector('[id^=telemetry-], #settings-telemetry') === null), true, 'General holds nothing of it');
    assert.equal(await page.evaluate(() => document.querySelector('#settings-telemetry').childElementCount), 0, 'the section is not drawn until it is selected');
    await page.click('#settings-telemetry-tab');
    await page.waitForSelector('#telemetry-dsn');
    await page.waitForSelector('#telemetry-facts'); // the Volume card is drawn once the settings are read; count the cards only after that, not before
    assert.equal(await page.evaluate(() => document.querySelector('#settings-title').textContent), 'Telemetry');
    // One card, the master switch first, then what depends on it (the Volume facts are a card of their own after it); no heading of its own repeating the page title.
    assert.deepEqual(await page.evaluate(() => ({ first: document.querySelector('#settings-telemetry .settings-row input').id, groups: document.querySelectorAll('#settings-telemetry .settings-group:not(:has(#telemetry-facts))').length, headings: document.querySelectorAll('#settings-telemetry h3').length,
      order: [...document.querySelectorAll('#settings-telemetry .settings-row')].map(row => (row.querySelector('input') || row.querySelector('button')).id) })),
      { first: 'telemetry-enabled', groups: 1, headings: 0, order: ['telemetry-enabled', 'telemetry-dsn', 'telemetry-fingerprint', 'telemetry-send-name'] }, 'the master switch comes first, in one card');
    // What bounds the volume is shown as facts, and they are the server's numbers.
    const facts = (await server.api(page, '/api/telemetry/settings')).body;
    assert.deepEqual([facts.traces_sample_rate, facts.max_db_spans], [1, 50], 'the defaults: every request, 50 database spans per request');
    assert.deepEqual(await page.evaluate(() => ({ rate: document.querySelector('#telemetry-sample-rate').textContent, db: document.querySelector('#telemetry-db-spans').textContent, files: document.querySelector('#telemetry-resource-spans').textContent, served: document.querySelector('#telemetry-static-files').textContent, inputs: document.querySelectorAll('#telemetry-facts input, #telemetry-facts button').length })),
      { rate: '100% of server requests and of page loads. A server setting changes it.', db: 'At most 50 per request; the rest are counted on the request, not traced.', files: 'Not traced: no span per script, stylesheet or image, and a page load keeps only its request and response phases.', served: 'Not traced: the scripts, styles and icons the server hands out are not requests of their own; the API and the pages are.', inputs: 0 }, 'the facts, read-only');
    // Closing Settings takes a clean section out of the page, and opening it again reads the settings again.
    await page.click('#settings-close');
    await page.waitForFunction(() => !document.querySelector('#settings-dialog').open && document.querySelector('#settings-telemetry').childElementCount === 0);
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.click('#settings-telemetry-tab');
    await page.waitForSelector('#telemetry-dsn:not([disabled])');
    assert.equal(await page.$eval('#telemetry-dsn', el => el.value), 'http://pagekey@127.0.0.1:9/5', 'read again on the next open');
    assert.equal(await page.$eval('#telemetry-dsn', el => el.value), 'http://pagekey@127.0.0.1:9/5');
    assert.equal(await page.$eval('#telemetry-enabled', el => el.checked), true);
    const toggleMaster = async () => { await page.click('#telemetry-enabled'); await page.waitForFunction(() => !document.querySelector('#telemetry-enabled').disabled); };
    const settingsState = () => page.evaluate(() => {
      const input = document.querySelector('#telemetry-dsn'), reset = document.querySelector('#telemetry-dsn-reset');
      return { value: input.value, fieldDisabled: input.disabled, reset: reset && { disabled: reset.disabled, label: reset.getAttribute('aria-label'), title: reset.title, left: reset.getBoundingClientRect().right <= input.getBoundingClientRect().left } };
    });
    assert.deepEqual(await settingsState(), { value: 'http://pagekey@127.0.0.1:9/5', fieldDisabled: false, reset: { disabled: false, label: 'Reset to default DSN', title: 'Reset to default DSN', left: true } }, 'a chosen DSN can be reset');
    await page.click('#telemetry-dsn-reset');
    assert.deepEqual(await settingsState(), { value: before.default_dsn, fieldDisabled: false, reset: { disabled: true, label: 'Reset to default DSN', title: 'Reset to default DSN', left: true } }, 'Reset puts the default in the field and turns itself off');
    await page.type('#telemetry-dsn', 'x'); assert.equal((await settingsState()).reset.disabled, false);
    await toggleMaster(); // off while the field holds a custom value
    assert.deepEqual(await page.evaluate(() => ({ field: document.querySelector('#telemetry-dsn').disabled, reset: document.querySelector('#telemetry-dsn-reset').disabled })), { field: true, reset: true }, 'off with a custom DSN: field and reset both disabled');
    await toggleMaster();
    assert.equal((await settingsState()).reset.disabled, false, 'on again: the reset button is back');
    await page.click('#telemetry-dsn-reset');
    // The master switch applies at once; off disables and mutes everything that depends on it, in every theme, and keeps it in the page.
    const everything = ['#telemetry-dsn', '#telemetry-dsn-reset', '#telemetry-save', '#telemetry-fingerprint', '#telemetry-fingerprint-copy', '#telemetry-fingerprint-roll', '#telemetry-send-name'];
    const dependent = () => page.evaluate(everything => ({
      disabled: everything.map(selector => document.querySelector(selector).disabled),
      muted: [...document.querySelectorAll('#settings-telemetry .settings-row')].map(row => row.classList.contains('telemetry-muted')), factsMuted: document.querySelector('#telemetry-facts').classList.contains('telemetry-muted'),
      opacity: getComputedStyle(document.querySelector('#telemetry-dsn').closest('.settings-row')).opacity,
      masterLabel: getComputedStyle(document.querySelector('label[for=telemetry-enabled]')).color, mutedLabel: getComputedStyle(document.querySelector('label[for=telemetry-dsn]')).color,
      dashed: ['#telemetry-dsn', '#telemetry-save', '#telemetry-fingerprint', '#telemetry-fingerprint-roll'].map(selector => getComputedStyle(document.querySelector(selector)).borderTopStyle),
      masterEnabled: !document.querySelector('#telemetry-enabled').disabled,
    }), everything);
    await toggleMaster();
    await page.waitForFunction(() => /Telemetry is off/.test(document.querySelector('#telemetry-status').textContent));
    assert.equal((await server.api(page, '/api/telemetry/settings')).body.active, false, 'turning it off in Settings stops reporting at once, with no Save');
    assert.deepEqual(await settingsState(), { value: 'http://pagekey@127.0.0.1:9/5', fieldDisabled: true, reset: { disabled: true, label: 'Reset to default DSN', title: 'Reset to default DSN', left: true } }, 'off: disabled, not hidden, the unsaved text put back to what is stored');
    let off = await dependent();
    assert.deepEqual(off.disabled, everything.map(() => true), 'off: the DSN, reset, Save, identity field, copy, dice and name switch are all disabled');
    assert.deepEqual(off.muted, [false, true, true, true], 'off: everything but the master switch is muted'); assert.equal(off.factsMuted, true, 'the volume facts too');
    assert.equal(off.masterEnabled, true, 'the master switch stays usable'); assert.notEqual(off.mutedLabel, off.masterLabel, 'a muted label has the muted colour');
    assert.equal(off.opacity, '0.5', 'dimmed where opacity is allowed'); assert.deepEqual(off.dashed, ['dashed', 'dashed', 'dashed', 'dashed'], 'and dashed');
    // High contrast has no opacity, so the off state is the theme's own muted text colour (v1's --text-muted): text, values, icons and borders all that grey, nothing in the accent or
    // the theme's cyan outline, and no stripe at the edge.
    await page.evaluate(() => { document.documentElement.dataset.theme = 'high-contrast-dark'; });
    await new Promise(resolve => setTimeout(resolve, 500)); // the controls' colours transition
    off = await dependent();
    assert.equal(off.opacity, '1', 'high contrast keeps full opacity...'); assert.notEqual(off.mutedLabel, off.masterLabel, '...and mutes by colour');
    const grey = await page.evaluate(everything => {
      const rgb = value => value.match(/\d+(\.\d+)?/g).slice(0, 3).map(Number), lum = value => { const [r, g, b] = rgb(value).map(c => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
      const root = getComputedStyle(document.documentElement), token = name => { const probe = document.createElement('i'); probe.style.color = root.getPropertyValue(name).trim(); document.body.append(probe); const color = getComputedStyle(probe).color; probe.remove(); return color; };
      const text = ['label[for=telemetry-dsn]', '#telemetry-dsn-desc', '#telemetry-dsn', '#telemetry-fingerprint', '#telemetry-save', '#telemetry-fingerprint-roll', '#telemetry-sample-rate', '#telemetry-facts dt'].map(selector => getComputedStyle(document.querySelector(selector)).color);
      const icon = ['#telemetry-fingerprint-roll', '#telemetry-dsn-reset'].map(selector => getComputedStyle(document.querySelector(`${selector} .icon-mask`)).backgroundColor);
      const borders = everything.filter(selector => selector !== '#telemetry-send-name').map(selector => getComputedStyle(document.querySelector(selector)).borderTopColor);
      const contrast = color => (lum(color) + 0.05) / 0.05; // on the black of this theme
      return { muted: token('--telemetry-off'), line: token('--telemetry-off-line'), accent: token('--accent'), cyan: token('--border'), text, icon, borders, rowStripe: getComputedStyle(document.querySelector('#telemetry-dsn').closest('.settings-row')).borderLeftWidth, textContrast: Math.min(...text.map(contrast)) };
    }, everything);
    assert.ok(grey.text.every(color => color === grey.muted), `labels, help text, values, buttons and the facts are all the muted grey: ${JSON.stringify(grey.text)} vs ${grey.muted}`);
    assert.ok(grey.icon.every(color => color === grey.muted), `the icons are muted too: ${JSON.stringify(grey.icon)}`);
    assert.ok(grey.borders.every(color => color === grey.line && color !== grey.accent && color !== grey.cyan), `control borders are the muted line, never the accent or the cyan outline: ${JSON.stringify(grey.borders)}`);
    assert.ok(grey.textContrast >= 4.5, `the muted text still reads on black (${grey.textContrast.toFixed(1)}:1)`);
    assert.equal(grey.rowStripe, '0px', 'no stripe at the edge of a muted row'); assert.deepEqual(off.dashed, ['dashed', 'dashed', 'dashed', 'dashed']);
    await page.evaluate(() => { delete document.documentElement.dataset.theme; });
    // Pressing the switch does not move the page: the rows and the facts keep their places whether the status line says something or not (no stripe, no
    // line pushed in), and a value too long for its field ends in an ellipsis.
    await page.evaluate(() => { delete document.documentElement.dataset.theme; });
    await new Promise(resolve => setTimeout(resolve, 500));
    const places = () => page.evaluate(() => ({ rows: [...document.querySelectorAll('#settings-telemetry .settings-row')].map(row => Math.round(row.getBoundingClientRect().top)), facts: Math.round(document.querySelector('#telemetry-facts').getBoundingClientRect().top), left: Math.round(document.querySelector('#telemetry-dsn').getBoundingClientRect().left), status: document.querySelector('#telemetry-status').textContent }));
    // The page as it opens (nothing said yet) against the page after the switch has been pressed twice (something said): nothing moved.
    await page.click('#settings-close');
    await page.waitForFunction(() => !document.querySelector('#settings-dialog').open && document.querySelector('#settings-telemetry').childElementCount === 0);
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.click('#settings-telemetry-tab'); await page.waitForSelector('#telemetry-dsn'); await page.waitForFunction(() => !document.querySelector('#telemetry-enabled').disabled); await new Promise(resolve => setTimeout(resolve, 300));
    const quiet = await places();
    assert.equal(quiet.status, '', 'nothing has been said yet');
    await toggleMaster();
    const said = await places();
    assert.ok(/Saved/.test(said.status), 'the switch has said something');
    assert.deepEqual([said.rows, said.facts, said.left], [quiet.rows, quiet.facts, quiet.left], 'switching moves nothing (rows, facts and the field\'s left edge stay put, the status line has its room already)');
    await toggleMaster();
    assert.equal(await page.$eval('#telemetry-dsn', el => getComputedStyle(el).textOverflow), 'ellipsis', 'a clipped DSN ends in an ellipsis');
    // On a phone the DSN and the fingerprint get a full-width line of their own, with their buttons after them.
    await page.setViewport({ width: 390, height: 844 }); await new Promise(resolve => setTimeout(resolve, 300));
    assert.deepEqual(await page.evaluate(() => { const box = selector => document.querySelector(selector).getBoundingClientRect(), row = selector => document.querySelector(selector).closest('.settings-control').getBoundingClientRect();
      return ['#telemetry-dsn', '#telemetry-fingerprint'].map(input => { const field = box(input), control = row(input), buttons = [...document.querySelector(input).closest('.settings-control').querySelectorAll('button')].map(button => button.getBoundingClientRect());
        return { fullWidth: field.width >= control.width - 2, buttonsAfter: buttons.every(button => button.top >= field.bottom - 1) }; }); }), [{ fullWidth: true, buttonsAfter: true }, { fullWidth: true, buttonsAfter: true }], 'on a phone each field has its own line and its buttons follow');
    await page.setViewport({ width: 1280, height: 900 }); await new Promise(resolve => setTimeout(resolve, 300));
    // An off switch keeps only the switch: the DSN chosen in the popup is still there.
    assert.equal(await page.$eval('#telemetry-dsn', el => el.value), 'http://pagekey@127.0.0.1:9/5', 'turning it off leaves the stored DSN alone');
    assert.equal((await server.api(page, '/api/telemetry/settings')).body.dsn, 'http://pagekey@127.0.0.1:9/5');
    await toggleMaster(); // on again: everything is enabled again and unmuted
    const on = await dependent();
    assert.deepEqual(on.disabled, everything.map(() => false), 'on: all enabled again'); assert.deepEqual(on.muted, [false, false, false, false]); assert.equal(on.factsMuted, false);
    assert.equal(on.opacity, '1');
    await page.$eval('#telemetry-dsn', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.type('#telemetry-dsn', 'nope');
    await page.click('#telemetry-save');
    await page.waitForFunction(() => /not a Sentry DSN/.test(document.querySelector('#telemetry-error').textContent));
    assert.deepEqual(server.errors, [], 'no page errors');
  } finally { await server.stop(); }
}

// A member who is neither the owner nor an admin: Settings -> Telemetry is theirs for their own fingerprint, and holds nothing that is the owner's.
async function member() {
  const server = await scratch({ env: { AIMEBU_SENTRY_DSN: null } });
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-tel-member-'));
  try {
    const call = await server.ownerClient();
    execFileSync('git', ['init', '-q'], { cwd: root });
    const project = (await call('/api/projects', { method: 'POST', body: { name: 'Member project', folder: root } })).body;
    const invite = (await call('/api/invites', { method: 'POST', body: { kind: 'join', project_id: project.id, role: 'viewer' } })).body;
    const login = 'member-one', password = 'a secure member password';
    const redeemed = await fetch(`${server.origin}/api/invites/redeem`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: invite.token, login, password }) });
    assert.equal(redeemed.status, 201, 'the member account is made');
    const page = await server.open({ width: 1280, height: 900, context: await server.context() });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    assert.equal((await server.signIn(page, { login, password })).status, 200);
    await page.goto(server.origin);
    await page.waitForSelector('#app-view:not([hidden])', { timeout: 30000 });
    assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog[open]')), false, 'a member is never asked about the owner\'s telemetry popup');
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.waitForSelector('#settings-telemetry-tab:not([hidden])');
    await page.click('#settings-telemetry-tab');
    await page.waitForFunction(() => document.querySelector('#telemetry-fingerprint')?.value);
    const view = await page.evaluate(() => {
      const visible = selector => { const node = document.querySelector(selector); return !!node && !node.closest('[hidden]') && node.getClientRects().length > 0; };
      return {
        fingerprint: document.querySelector('#telemetry-fingerprint').value, readOnly: document.querySelector('#telemetry-fingerprint').readOnly,
        roll: visible('#telemetry-fingerprint-roll'), rollLabel: document.querySelector('#telemetry-fingerprint-roll')?.getAttribute('aria-label'), rollEnabled: !document.querySelector('#telemetry-fingerprint-roll').disabled,
        sendName: visible('#telemetry-send-name'), sendNameEnabled: !document.querySelector('#telemetry-send-name').disabled, sendNameChecked: document.querySelector('#telemetry-send-name').checked,
        dsn: !!document.querySelector('#telemetry-dsn'), enabled: !!document.querySelector('#telemetry-enabled'), save: !!document.querySelector('#telemetry-save'),
        facts: !!document.querySelector('#telemetry-facts'), rate: !!document.querySelector('#telemetry-sample-rate'), note: document.querySelector('#settings-telemetry')?.textContent.includes('Only the owner or an admin'),
      };
    });
    assert.match(view.fingerprint, /^[0-9a-f]{16}$/);
    assert.deepEqual({ ...view, fingerprint: undefined }, { fingerprint: undefined, readOnly: true, roll: true, rollLabel: 'Generate a new anonymous ID (the current one stops being used)', rollEnabled: true, sendName: true, sendNameEnabled: true, sendNameChecked: false,
      dsn: false, enabled: false, save: false, facts: false, rate: false, note: true }, 'a member sees their own fingerprint, the re-roll and the name switch, and none of the owner\'s controls');
    // Their own fingerprint is theirs: not the owner's, and a roll changes it.
    const owner = (await call('/api/me/telemetry')).body.fingerprint;
    assert.notEqual(view.fingerprint, owner, 'a different person, a different fingerprint');
    await page.click('#telemetry-fingerprint-roll'); // no name sent: no question
    await page.waitForFunction(old => document.querySelector('#telemetry-fingerprint').value !== old, {}, view.fingerprint);
    assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog[open]')), false, 'no question while the name is off');
    // What is the owner's is refused to them by the server, whatever the page shows.
    const stored = JSON.stringify((await call('/api/telemetry/settings')).body);
    for (const [method, pathname, body] of [['GET', '/api/telemetry/settings'], ['PUT', '/api/telemetry/settings', { enabled: true, dsn: 'https://k@o1.ingest.de.sentry.io/1', prompted: true }]]) {
      const result = await server.api(page, pathname, { method, body });
      assert.equal(result.status, 403, `${method} ${pathname} is refused to a member`);
    }
    assert.equal(JSON.stringify((await call('/api/telemetry/settings')).body), stored, 'the refused write changed nothing');
    assert.deepEqual(server.errors, [], 'no page errors');
  } finally { await server.stop(); fs.rmSync(root, { recursive: true, force: true }); }
}

(async () => { await off(); await on(); await popup(); await member(); console.log('telemetry page tests passed'); })().catch(error => { console.error(error); process.exit(1); });
