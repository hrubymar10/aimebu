// scripts/dev-run.sh (`make dev`): a sandbox server never reports to Sentry unless AIMEBU_DEV_SENTRY_DSN says so. A stub binary prints its environment.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'dev-run-test-'));
// dev-run.sh runs `$bin server`. The stub is /bin/sh and a script named `server` in the working directory, so nothing has to be executable
// from the temp directory: the Docker runner mounts /tmp noexec, where an executable stub never ran (the output was empty).
const stub = '/bin/sh';
fs.writeFileSync(path.join(work, 'server'), 'env | grep "^AIMEBU_" | sort\n');
const script = path.join(__dirname, 'dev-run.sh');
const run = extra => {
  const env = { PATH: process.env.PATH, HOME: work, ...extra };
  const result = spawnSync('sh', [script, stub], { env, cwd: work, encoding: 'utf8', timeout: 30000 });
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
