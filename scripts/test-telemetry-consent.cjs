// The telemetry consent popup's logic: its fields follow the server's default, Not now turns reports off, and it is asked once.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'telemetry-consent.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { CONSENT_TEXT, SEND_NAME_LABEL, SEND_NAME_DESC, dsnProblem, consentFields, consentBody, askTelemetryConsent };', context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.ok(/Settings → Telemetry/.test(api.CONSENT_TEXT), 'says where to change it'); assert.equal(api.consentFields({ enabled: true, dsn: '' })[0].label, 'Send telemetry');
assert.ok(/errors/.test(api.CONSENT_TEXT) && /traces/.test(api.CONSENT_TEXT) && !/replay/i.test(api.CONSENT_TEXT), 'says what is sent');
assert.ok(/messages, prompts, tokens, file contents/.test(api.CONSENT_TEXT), 'says what is never sent');
assert.equal(api.dsnProblem(''), ''); assert.equal(api.dsnProblem('https://k@o1.ingest.de.sentry.io/42'), '');
for (const good of ['http://k@127.0.0.1:9000/1', 'http://k@localhost:9000/1', 'http://k@[::1]:9000/1']) assert.equal(api.dsnProblem(good), '', good);
for (const bad of ['x', 'https://host/1', 'https://k@h/', 'ftp://k@h/1', 'http://k@sentry.example.com/1', 'http://k@10.0.0.5/1']) assert.notEqual(api.dsnProblem(bad), '', bad);

// The toggle and the field start as the server says: on with the default filled in when a default ships, off and empty when none does.
let fields = api.consentFields({ enabled: true, dsn: 'https://k@h.example/1', default_dsn: 'https://k@h.example/1' });
assert.equal(fields[0].value, true); assert.equal(fields[1].value, 'https://k@h.example/1'); assert.equal(fields[1].reset.value, 'https://k@h.example/1', 'Reset puts the shipped default back');
fields = api.consentFields({ enabled: false, dsn: '', default_dsn: '' });
assert.equal(fields[0].value, false); assert.equal(fields[1].value, '');
assert.equal(fields[1].showIf, undefined, 'the field is never hidden'); assert.equal(fields[1].disabledIf({ enabled: false }), true, 'off disables the field'); assert.equal(fields[1].disabledIf({ enabled: true }), false);
assert.equal(fields[1].reset.value, '', 'no default shipped: nothing to reset to (the button is hidden)'); assert.equal(fields[1].reset.label, 'Reset to default DSN');
assert.match(fields[1].validate('', { enabled: true }), /Enter a DSN/); assert.equal(fields[1].validate('', { enabled: false }), '');
assert.notEqual(fields[1].validate('nope', { enabled: true }), ''); assert.equal(fields[1].validate('nope', { enabled: false }), '', 'a greyed-out field is not validated');

// The identity switch: under the DSN, off by default, greyed out while reports are off, worded as Settings -> Telemetry words it.
assert.deepEqual(plain(fields.map(field => field.name)), ['enabled', 'dsn', 'send_name']);
assert.deepEqual([fields[2].type, fields[2].value, fields[2].label, fields[2].hint], ['checkbox', false, api.SEND_NAME_LABEL, api.SEND_NAME_DESC]);
assert.equal(fields[2].disabledIf({ enabled: false }), true, 'off greys the switch out'); assert.equal(fields[2].disabledIf({ enabled: true }), false);
assert.deepEqual(plain(api.consentBody({ enabled: true, dsn: 'https://k@h/1', send_name: true })), { prompted: true, enabled: true, dsn: 'https://k@h/1' }, 'the settings body does not carry the identity switch');

assert.deepEqual(plain(api.consentBody(null)), { prompted: true, enabled: false }, 'Not now / Escape turns reports off');
assert.deepEqual(plain(api.consentBody({ enabled: false, dsn: 'x' })), { prompted: true, enabled: false });
assert.deepEqual(plain(api.consentBody({ enabled: true, dsn: 'https://k@h/1' })), { prompted: true, enabled: true, dsn: 'https://k@h/1' });

(async () => {
  const calls = [];
  const request = view => async (url, options = {}) => { calls.push([options.method || 'GET', url, options.body]); return view; };
  let dialogs = 0, answer = { enabled: true, dsn: 'https://k@h/1' };
  const dialog = async options => { dialogs++; assert.equal(options.title, 'Telemetry'); assert.equal(options.confirm, 'Save'); assert.equal(options.cancel, 'Not now'); return answer; };
  await api.askTelemetryConsent(request({ prompted: true, enabled: true, dsn: 'x' }), dialog);
  assert.equal(dialogs, 0, 'already answered: never asked again'); assert.equal(calls.length, 1);
  calls.length = 0;
  await api.askTelemetryConsent(request({ prompted: false, enabled: true, dsn: 'https://d@h/1' }), dialog);
  assert.equal(dialogs, 1); assert.deepEqual(plain(calls[1]), ['PUT', '/api/telemetry/settings', { prompted: true, enabled: true, dsn: 'https://k@h/1' }]);
  // Save sends send_name only when it is ticked and reports are on; unticked, or reports off, no identity call at all.
  calls.length = 0; answer = { enabled: true, dsn: 'https://k@h/1', send_name: true };
  await api.askTelemetryConsent(request({ prompted: false, enabled: true, dsn: 'https://d@h/1' }), dialog);
  assert.deepEqual(plain(calls[2]), ['PUT', '/api/me/telemetry', { send_name: true }]); assert.equal(calls.length, 3);
  // A failed identity save keeps the popup: it comes back with what was chosen and the reason, and the second Save goes through.
  calls.length = 0; answer = { enabled: true, dsn: 'https://k@h/1', send_name: true }; dialogs = 0; let shown = [], failures = 1;
  const flaky = async (url, options = {}) => { calls.push([options.method || 'GET', url, options.body]); if (url === '/api/me/telemetry' && failures-- > 0) throw new Error('offline'); return { prompted: false, enabled: true, dsn: 'https://d@h/1' }; };
  await api.askTelemetryConsent(flaky, async options => { shown.push(plain(options)); return answer; });
  assert.equal(shown.length, 2, 'asked again after the identity save failed'); assert.match(shown[1].message, /Could not save your user-name choice \(offline\)/);
  assert.equal(shown[1].fields[2].value, true, 'with the switch as it was ticked'); assert.equal(calls.filter(call => call[1] === '/api/me/telemetry').length, 2);
  // A lost session during the identity save ends the popup quietly; three failures end it with a note saying the choice was not saved.
  calls.length = 0; shown = []; failures = 1;
  const lostSession = async (url, options = {}) => { calls.push([options.method || 'GET', url]); if (url === '/api/me/telemetry') throw Object.assign(new Error('Request failed (401)'), { status: 401, unauthorized: true }); return { prompted: false, enabled: true, dsn: 'https://d@h/1' }; };
  answer = { enabled: true, dsn: 'https://k@h/1', send_name: true };
  await api.askTelemetryConsent(lostSession, async options => { shown.push(plain(options)); return answer; });
  assert.equal(shown.length, 1, 'a lost session does not bring the popup back over sign-in');
  calls.length = 0; shown = [];
  await api.askTelemetryConsent(async (url, options = {}) => { if (url === '/api/me/telemetry') throw new Error('offline'); return { prompted: false, enabled: true, dsn: 'https://d@h/1' }; }, async options => { shown.push(plain(options)); return options.notice ? true : answer; });
  assert.equal(shown.length, 4, 'three tries, then the note'); assert.equal(shown[3].notice, true); assert.match(shown[3].message, /could not be saved.*Settings → Telemetry/);
  calls.length = 0; answer = { enabled: true, dsn: 'https://k@h/1', send_name: false };
  await api.askTelemetryConsent(request({ prompted: false }), dialog); assert.equal(calls.length, 2, 'unticked: no identity request');
  calls.length = 0; answer = { enabled: false, dsn: '', send_name: true };
  await api.askTelemetryConsent(request({ prompted: false }), dialog); assert.equal(calls.length, 2, 'reports off: no identity request');
  calls.length = 0; answer = null;
  await api.askTelemetryConsent(request({ prompted: false }), dialog);
  assert.deepEqual(plain(calls[1]), ['PUT', '/api/telemetry/settings', { prompted: true, enabled: false }]);
  await api.askTelemetryConsent(async () => { throw new Error('offline'); }, dialog); // never throws
  console.log('telemetry consent test passed');
})().catch(error => { console.error(error); process.exit(1); });
