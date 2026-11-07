// A request that got no answer is marked as a network failure, an answer the server gave (4xx, 5xx) is not, a request the page cancelled is
// not. api.js has no DOM, so this runs it in a vm with a fetch that fails the way the test says.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'api.js'), 'utf8').replace(/^export /gm, '');
const context = { Object, Promise, Error, String, JSON, URLSearchParams, Response: undefined, FormData: class { append() {} }, fetch: undefined };
vm.createContext(context);
vm.runInContext(source + '\nglobalThis.api = { request, download, upload, isNetworkError };', context);
const { request, download, upload, isNetworkError } = context.api;

const answer = (status, body = {}) => ({ ok: status >= 200 && status < 300, status, json: async () => body, blob: async () => ({}), headers: { get: () => '' } });
const failing = error => { context.fetch = async () => { throw error; }; };
const settle = promise => promise.then(value => ({ value }), error => ({ error }));

(async () => {
  // The browser's own words for "no answer": Chromium's and Safari's.
  for (const message of ['Failed to fetch', 'Load failed', 'NetworkError when attempting to fetch resource.']) {
    failing(Object.assign(new Error(message), { name: 'TypeError' }));
    for (const [name, call] of [['request', () => request('/api/x')], ['download', () => download('/api/x')], ['upload', () => upload('/api/x', {})]]) {
      const { error } = await settle(call());
      assert.ok(error && error.message === message && isNetworkError(error), `${name}: "${message}" is marked as a network failure and keeps its message`);
    }
  }
  // A request the page cancelled is not a failure of the network.
  failing(Object.assign(new Error('aborted'), { name: 'AbortError' }));
  assert.equal(isNetworkError((await settle(request('/api/x'))).error), false, 'an abort is not a network failure');
  // An answer from the server is not either: it has a status, and a message of its own.
  context.fetch = async () => answer(503, { message: 'down for maintenance', error: 'maintenance' });
  const refused = (await settle(request('/api/x'))).error;
  assert.deepEqual([isNetworkError(refused), refused.status, refused.message, refused.code], [false, 503, 'down for maintenance', 'maintenance']);

  console.log('api network: ok');
})().catch(error => { console.error(error); process.exit(1); });
