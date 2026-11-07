// The browser side of Sentry reporting: what the scrub functions let through, that no DSN means nothing is loaded or started, and the SDK
// options (everything the SDK collects by default is off, spans go through the scrubber, and there is no Replay). Runs web/js/telemetry.js in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'telemetry.js'), 'utf8');
const context = { URL };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { SDK_URL, scrubUrl, routeOf, scrubText, scrubEvent, scrubBreadcrumb, sentryOptions, startTelemetry, resetTelemetryForTest };', context);
const api = context.api;
// The vendored SDK is the file the README records.
const vendor = path.join(__dirname, '..', 'web', 'vendor');
const sum = require('node:crypto').createHash('sha256').update(fs.readFileSync(path.join(vendor, 'sentry', 'bundle.tracing.min.js'))).digest('hex');
assert.ok(fs.readFileSync(path.join(vendor, 'README.md'), 'utf8').includes(sum), 'web/vendor/README.md records the SHA-256 of the vendored Sentry SDK');
assert.ok(fs.existsSync(path.join(vendor, 'sentry', 'LICENSE')), 'the SDK licence ships with it');
assert.ok(!/^\s*<script[^>]+sentry/im.test(fs.readFileSync(path.join(__dirname, '..', 'web', 'index.html'), 'utf8')), 'index.html never loads the SDK itself');
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(api.scrubUrl('https://user:pw@host.example:8080/api/rooms/42?token=abc#frag'), '/api/rooms/42');
assert.equal(api.scrubUrl('/api/x?cookie=1'), '/api/x');
assert.equal(api.scrubUrl(''), ''); assert.equal(api.scrubUrl(null), '');
const text = api.scrubText('open /Users/alice/x: Authorization: Bearer abc.def-1 token=hunter2 password: "p w" api_key=zzz');
for (const leak of ['/Users/alice', 'abc.def-1', 'hunter2', 'p w', 'zzz']) assert.ok(!text.includes(leak), `${leak} must not survive: ${text}`);
assert.ok(text.includes('~/x'));
assert.ok(api.scrubText('é'.repeat(1000)).length <= 301, 'text is bounded');

const event = plain({
  user: { id: '1', email: 'a@b.c', ip_address: '10.0.0.1' }, server_name: 'host', extra: { body: 'secret prompt' }, modules: { x: '1' },
  request: { url: 'https://h.example/app?room=secret#x', method: 'GET', headers: { Cookie: 'sid=1', Authorization: 'Bearer abc' }, data: 'secret body', cookies: { sid: '1' } },
  contexts: { trace: { trace_id: 't', data: { 'url.full': 'https://h.example/x?token=secret', 'url.path': '/api/me?q=1', deviceMemory: '16 GB', 'http.response.status_code': 200 } }, browser: { name: 'Chrome' }, os: { name: 'macOS' }, device: { model: 'mac' }, state: { value: 'secret' } },
  message: 'failed at /home/bob/app token=abc', transaction: 'https://h.example/rooms/3?x=1',
  exception: { values: [{ value: 'TypeError at /Users/alice/x password=pw', stacktrace: { frames: [{ filename: 'https://h.example/js/app.js?v=1', abs_path: 'https://h.example/js/app.js?v=1', vars: { secret: 1 }, context_line: 'const secret = 1', pre_context: ['a'], post_context: ['b'] }] } }] },
  breadcrumbs: [{ category: 'console', message: 'a message body' }, { category: 'ui.click', message: 'div > button "Send secret"' }, { category: 'ui.input', message: 'input' },
    { category: 'fetch', data: { url: 'https://h.example/api/rooms/1/messages?q=secret', method: 'POST', status_code: 200, request_body: 'secret' } }, { category: 'navigation', data: { from: '/a?x=1', to: '/b?y=2' } }],
  spans: [{ description: 'https://h.example/unload?token=zzz' }, { description: 'GET https://h.example/api/me?token=zzz', data: { url: 'https://h.example/api/me?token=zzz', 'http.response.status_code': 200, body: 'secret', 'http.request.header.authorization': 'x' }, tags: { a: 'b' } }],
  tags: { server_name: 'host', k: 'v' },
});
const out = JSON.stringify(api.scrubEvent(event));
for (const leak of ['a@b.c', '10.0.0.1', 'secret', 'sid=1', 'Bearer abc', 'room=secret', 'h.example', '/Users/alice', '/home/bob', 'token=abc', 'password=pw', 'zzz', 'Chrome"x', '"mac"', 'a message body', 'Send']) {
  assert.ok(!out.includes(leak), `${leak} survived: ${out}`);
}
for (const kept of ['/app', '/rooms/…', '/js/app.js', 'macOS', 'trace_id', '/api/rooms/…', 'GET /api/me', 'http.response.status_code', '"POST"', '/b']) assert.ok(out.includes(kept), `${kept} must survive: ${out}`);
for (const leak of ['/api/rooms/1/messages', 'room=secret', 'server.address', 'url.full', 'deviceMemory', 'token=secret']) assert.ok(!out.includes(leak), `${leak} must not survive: ${out}`);
assert.equal(api.scrubEvent(null), null);
// A URL becomes a route: no host, query, fragment, id or chosen name.
for (const [raw, route] of [['https://h.example/api/switcher/claude/profiles/my-private-profile?token=x#frag', '/api/switcher/…'], ['/api/me', '/api/me'], ['/rooms/3?x=1', '/rooms/…'], ['/', '/'], ['', '/'], ['https://h.example', '/']]) assert.equal(api.routeOf(raw), route, raw);
// Bare tokens are redacted wherever they appear.
for (const token of ['aak_0123456789abcdef', 'amk_0123456789abcdef', 'as_0123456789abcdef', 'sk-ant-api03-ABCDEFGH12345678', 'sk-proj-ABCDEFGH12345678']) assert.ok(!api.scrubText(`key ${token} here`).includes(token.slice(0, 12)), `${token} must be redacted`);
assert.equal(api.scrubText('a short as_x stays'), 'a short as_x stays');

assert.equal(api.scrubBreadcrumb({ category: 'console' }), null);

// Nothing is loaded or started without a DSN.
(async () => {
  let loaded = 0;
  const deps = { load: async url => { loaded++; assert.equal(url, api.SDK_URL); }, Sentry: () => sentry };
  const calls = [];
  const sentry = { init: options => calls.push(options), browserTracingIntegration: () => 'tracing' };
  for (const config of [undefined, null, {}, { dsn: '' }, { dsn: 3 }]) { assert.equal(await api.startTelemetry(config, deps), false); api.resetTelemetryForTest(); }
  assert.equal(loaded, 0, 'no DSN: the SDK is never requested');
  assert.equal(calls.length, 0);
  assert.equal(await api.startTelemetry({ dsn: 'https://k@h.example/1', environment: 'prod', release: 'v1', traces_sample_rate: 0.25 }, deps), true);
  assert.equal(loaded, 1);
  assert.equal(await api.startTelemetry({ dsn: 'https://k@h.example/1' }, deps), false, 'started once');
  const options = calls[0];
  assert.equal(options.dsn, 'https://k@h.example/1'); assert.equal(options.environment, 'prod'); assert.equal(options.release, 'v1');
  assert.equal(options.sendDefaultPii, false);
  assert.equal(options.tracesSampleRate, 0.25);
  assert.equal(options.replaysSessionSampleRate, undefined); assert.equal(options.replaysOnErrorSampleRate, undefined); assert.deepEqual(plain(options.integrations), ['tracing'], 'tracing is the only integration: there is no Replay');
  const collection = plain(options.dataCollection);
assert.equal(collection.userInfo, false); assert.equal(collection.cookies, false); assert.equal(collection.httpHeaders, false); assert.deepEqual(collection.httpBodies, []); assert.equal(collection.urlQueryParams, false);
assert.equal(collection.stackFrameVariables, false); assert.equal(collection.frameContextLines, 0); assert.equal(options.traceLifecycle, 'static', 'spans go through beforeSendTransaction');
assert.equal(options.beforeSend, api.scrubEvent); assert.equal(options.beforeSendTransaction, api.scrubEvent); assert.equal(options.beforeBreadcrumb, api.scrubBreadcrumb);
  // A failing load or a missing SDK never throws.
  api.resetTelemetryForTest();
  assert.equal(await api.startTelemetry({ dsn: 'https://k@h/1' }, { load: async () => { throw new Error('offline'); } }), false);
  api.resetTelemetryForTest();
  assert.equal(await api.startTelemetry({ dsn: 'https://k@h/1' }, { load: async () => {}, Sentry: () => undefined }), false);
  console.log('telemetry test passed');
})().catch(error => { console.error(error); process.exit(1); });
