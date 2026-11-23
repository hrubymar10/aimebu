// Guard: every script that starts an aimebu server must set AIMEBU_SENTRY_DSN (off, or a scratch DSN on purpose), so a sandbox run can never
// report to Sentry through a build's default DSN. Fails when a new helper calls startServer without mentioning it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const skip = new Set(['server-start.cjs', 'test-server-start.cjs', 'test-sentry-off.cjs']); // the helper itself, and its stand-in-server test
const missing = fs.readdirSync(__dirname).filter(name => name.endsWith('.cjs') && !skip.has(name))
  .filter(name => { const text = fs.readFileSync(path.join(__dirname, name), 'utf8'); return /\bstartServer\(/.test(text) && !text.includes('AIMEBU_SENTRY_DSN'); });
assert.deepEqual(missing, [], `these scripts start a server without AIMEBU_SENTRY_DSN: ${missing.join(', ')}`);
console.log('sentry-off: ok');
