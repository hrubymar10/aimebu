// scripts/dev-run.sh (`make dev`): a sandbox server never reports to Sentry unless AIMEBU_DEV_SENTRY_DSN says so. A stub binary prints its environment.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'dev-run-test-'));
const stub = path.join(work, 'stub');
fs.writeFileSync(stub, '#!/bin/sh\nenv | grep "^AIMEBU_" | sort\n');
fs.chmodSync(stub, 0o755);
const script = path.join(__dirname, 'dev-run.sh');
const run = extra => {
  const env = { PATH: process.env.PATH, HOME: work, ...extra };
  const result = spawnSync('sh', [script, stub], { env, encoding: 'utf8', timeout: 30000 });
  assert.equal(result.status, 0, result.stderr);
  return Object.fromEntries(result.stdout.split('\n').filter(Boolean).map(line => [line.slice(0, line.indexOf('=')), line.slice(line.indexOf('=') + 1)]));
};
try {
  assert.equal(run({}).AIMEBU_SENTRY_DSN, 'off', 'default: off');
  assert.equal(run({ AIMEBU_SENTRY_DSN: 'https://k@example.com/1' }).AIMEBU_SENTRY_DSN, 'off', 'an inherited DSN is dropped');
  const opted = run({ AIMEBU_DEV_SENTRY_DSN: 'https://k@example.com/1' });
  assert.equal(opted.AIMEBU_SENTRY_DSN, 'https://k@example.com/1', 'explicit opt-in');
  assert.equal(opted.AIMEBU_SENTRY_ENV, 'development');
  console.log('dev-run: ok');
} finally { fs.rmSync(work, { recursive: true, force: true }); }
