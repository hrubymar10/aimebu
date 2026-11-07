// The error-reports consent popup's logic: its fields follow the server's default, Not now turns reports off, and it is asked once.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'telemetry-consent.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { CONSENT_TEXT, dsnProblem, consentFields, consentBody, askTelemetryConsent };', context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.ok(/errors/.test(api.CONSENT_TEXT) && /traces/.test(api.CONSENT_TEXT) && !/replay/i.test(api.CONSENT_TEXT), 'says what is sent');
assert.ok(/messages, prompts, tokens, file contents/.test(api.CONSENT_TEXT), 'says what is never sent');
assert.equal(api.dsnProblem(''), ''); assert.equal(api.dsnProblem('https://k@o1.ingest.de.sentry.io/42'), '');
for (const good of ['http://k@127.0.0.1:9000/1', 'http://k@localhost:9000/1', 'http://k@[::1]:9000/1']) assert.equal(api.dsnProblem(good), '', good);
for (const bad of ['x', 'https://host/1', 'https://k@h/', 'ftp://k@h/1', 'http://k@sentry.example.com/1', 'http://k@10.0.0.5/1']) assert.notEqual(api.dsnProblem(bad), '', bad);

// The toggle and the field start as the server says: on with the default filled in when a default ships, off and empty when none does.
let fields = api.consentFields({ enabled: true, dsn: 'https://k@h.example/1', default_dsn: 'https://k@h.example/1' });
assert.equal(fields[0].value, true); assert.equal(fields[1].value, 'https://k@h.example/1');
fields = api.consentFields({ enabled: false, dsn: '', default_dsn: '' });
assert.equal(fields[0].value, false); assert.equal(fields[1].value, '');
assert.equal(fields[1].showIf({ enabled: false }), false, 'the field shows only while reports are on');
assert.match(fields[1].validate('', { enabled: true }), /Enter a DSN/); assert.equal(fields[1].validate('', { enabled: false }), '');
assert.notEqual(fields[1].validate('nope', { enabled: true }), '');

assert.deepEqual(plain(api.consentBody(null)), { prompted: true, enabled: false }, 'Not now / Escape turns reports off');
assert.deepEqual(plain(api.consentBody({ enabled: false, dsn: 'x' })), { prompted: true, enabled: false });
assert.deepEqual(plain(api.consentBody({ enabled: true, dsn: 'https://k@h/1' })), { prompted: true, enabled: true, dsn: 'https://k@h/1' });

(async () => {
  const calls = [];
  const request = view => async (url, options = {}) => { calls.push([options.method || 'GET', url, options.body]); return view; };
  let dialogs = 0, answer = { enabled: true, dsn: 'https://k@h/1' };
  const dialog = async options => { dialogs++; assert.equal(options.title, 'Error reports'); assert.equal(options.confirm, 'Save'); assert.equal(options.cancel, 'Not now'); return answer; };
  await api.askTelemetryConsent(request({ prompted: true, enabled: true, dsn: 'x' }), dialog);
  assert.equal(dialogs, 0, 'already answered: never asked again'); assert.equal(calls.length, 1);
  calls.length = 0;
  await api.askTelemetryConsent(request({ prompted: false, enabled: true, dsn: 'https://d@h/1' }), dialog);
  assert.equal(dialogs, 1); assert.deepEqual(plain(calls[1]), ['PUT', '/api/telemetry/settings', { prompted: true, enabled: true, dsn: 'https://k@h/1' }]);
  calls.length = 0; answer = null;
  await api.askTelemetryConsent(request({ prompted: false }), dialog);
  assert.deepEqual(plain(calls[1]), ['PUT', '/api/telemetry/settings', { prompted: true, enabled: false }]);
  await api.askTelemetryConsent(async () => { throw new Error('offline'); }, dialog); // never throws
  console.log('telemetry consent test passed');
})().catch(error => { console.error(error); process.exit(1); });
