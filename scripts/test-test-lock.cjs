// scripts/test-lock.sh: the shared test lock is first come first served, shows who holds it, and is the same flock plain `flock` takes.
// Every case uses its own lock and queue under a temp directory, never the real /tmp/aimebu-test.lock.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'test-lock-test-'));
const script = path.join(__dirname, 'test-lock.sh');
const env = { PATH: process.env.PATH, HOME: work, AIMEBU_TEST_LOCK: path.join(work, 'lock'), AIMEBU_TEST_QUEUE: path.join(work, 'queue'),
  AIMEBU_TEST_LOCK_POLL: '0.1', AIMEBU_TEST_LOCK_REPORT: '60' };
const sleep = ms => new Promise(r => setTimeout(r, ms));
const lockRun = (cmd, args = []) => {
  const child = spawn('sh', [script, cmd, ...args], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  const run = { child, out: '', err: '' };
  child.stdout.on('data', d => { run.out += d; });
  child.stderr.on('data', d => { run.err += d; });
  run.done = new Promise(r => child.on('close', code => r(code)));
  return run;
};
const flockN = () => spawnSync('flock', ['-n', env.AIMEBU_TEST_LOCK, 'true'], { env }).status;
const order = path.join(work, 'order');
// A holder that runs until a release file appears, so no case depends on how fast the machine is.
const until = file => `while [ ! -e ${file} ]; do sleep 0.1; done`;
const waitFor = async (cond, what) => { for (let i = 0; i < 300 && !cond(); i++) await sleep(100); assert.ok(cond(), what); };

(async () => {
  // Three waiters behind one holder are served in the order they asked, and the waiting ones say who holds the lock and where they stand.
  const holder = lockRun('sh', ['-c', `echo holder-started; ${until(path.join(work, 'rel1'))}`]);
  await waitFor(() => holder.out.includes('holder-started') && fs.existsSync(path.join(env.AIMEBU_TEST_QUEUE, 'holder')), 'the holder is running');
  assert.equal(flockN(), 1, 'the script holds the lock while its command runs');
  const meta = fs.readFileSync(path.join(env.AIMEBU_TEST_QUEUE, 'holder'), 'utf8');
  assert.match(meta, /^cmd=sh -c echo holder-started; while /m, 'holder metadata has the command line');
  assert.match(meta, /^since=\d{4}-\d\d-\d\dT[\d:]+Z$/m);
  const waiters = [];
  for (const name of ['w1', 'w2', 'w3']) {
    waiters.push(lockRun('sh', ['-c', `echo ${name} >> ${order}; sleep 0.3`]));
    await waitFor(() => waiters[waiters.length - 1].err.includes('waiting for the test lock'), 'the waiter reports');
  }
  fs.writeFileSync(path.join(work, 'rel1'), '');
  const codes = await Promise.all([holder.done, ...waiters.map(w => w.done)]);
  assert.deepEqual(codes, [0, 0, 0, 0]);
  assert.equal(holder.out, 'holder-started\n');
  assert.deepEqual(fs.readFileSync(order, 'utf8').split('\n').filter(Boolean), ['w1', 'w2', 'w3'], 'served in ticket order');
  waiters.forEach((w, i) => {
    assert.match(w.err, new RegExp(`waiting for the test lock \\(position ${i + 1}, held by sh -c echo holder-started; while .* since \\d{4}-`), `waiter ${i + 1} reports its position and the holder`);
  });
  assert.equal(flockN(), 0, 'the lock is free again');
  assert.equal(fs.existsSync(path.join(env.AIMEBU_TEST_QUEUE, 'holder')), false, 'the holder file is removed');
  assert.deepEqual(fs.readdirSync(env.AIMEBU_TEST_QUEUE).filter(f => f.endsWith('.ticket')), [], 'no tickets are left');

  // A steady queue stays quiet; a changed position reports immediately even before the time limit.
  const quietHolder = lockRun('sh', ['-c', `echo quiet-holder-started; ${until(path.join(work, 'rel-quiet-holder'))}`]);
  await waitFor(() => quietHolder.out.includes('quiet-holder-started'), 'the quiet holder is running');
  const first = lockRun('sh', ['-c', `echo first-started; ${until(path.join(work, 'rel-first'))}`]);
  await waitFor(() => first.err.includes('waiting for the test lock'), 'the first waiter reports on arrival');
  const second = lockRun('echo', ['second-ran']);
  await waitFor(() => second.err.includes('waiting for the test lock'), 'the second waiter reports on arrival');
  const waitLines = run => run.err.split('\n').filter(line => line.startsWith('waiting for the test lock'));
  assert.match(second.err, /position 2, held by sh -c echo quiet-holder-started/);
  await sleep(2200);
  const quietReports = waitLines(second).length;
  fs.writeFileSync(path.join(work, 'rel-quiet-holder'), '');
  await waitFor(() => first.out.includes('first-started'), 'the first waiter takes the lock');
  await sleep(500);
  const changedReports = waitLines(second).filter(line => /position 1, held by sh -c echo first-started/.test(line));
  fs.writeFileSync(path.join(work, 'rel-first'), '');
  assert.deepEqual(await Promise.all([quietHolder.done, first.done, second.done]), [0, 0, 0]);
  assert.equal(second.out, 'second-ran\n', 'the second waiter runs after the lock frees');
  assert.equal(quietReports, 1, 'no repeated report while holder and position stay the same');
  assert.equal(changedReports.length, 1, 'the waiter reports its new position immediately');

  // Priority lane: after the holder, a priority caller runs before normal waiters (never before the holder), priority callers keep their own
  // order, normal waiters keep theirs, and an unset variable changes nothing (the cases above). Private lock and queue, as everywhere here.
  const penv = { ...env, AIMEBU_TEST_LOCK: path.join(work, 'plock'), AIMEBU_TEST_QUEUE: path.join(work, 'pqueue') };
  const pRun = (extra, name) => {
    const child = spawn('sh', [script, 'sh', '-c', name === 'holder' ? `echo holder-started; ${until(path.join(work, 'rel-prio'))}` : `echo ${name} >> ${order}-prio; sleep 0.2`], { env: { ...penv, ...extra }, stdio: ['ignore', 'pipe', 'pipe'] });
    const run = { out: '', err: '' };
    child.stdout.on('data', d => { run.out += d; });
    child.stderr.on('data', d => { run.err += d; });
    run.done = new Promise(r => child.on('close', code => r(code)));
    return run;
  };
  const pHolder = pRun({}, 'holder');
  await waitFor(() => pHolder.out.includes('holder-started'), 'the priority-case holder is running');
  const pw = {};
  for (const [name, extra] of [['n1', {}], ['n2', {}], ['p1', { AIMEBU_TEST_LOCK_PRIORITY: '1' }], ['p2', { AIMEBU_TEST_LOCK_PRIORITY: '1' }]]) {
    pw[name] = pRun(extra, name);
    await waitFor(() => pw[name].err.includes('waiting for the test lock'), `${name} reports`);
  }
  assert.match(pw.p1.err, /position 1, priority lane, held by sh -c echo holder-started/, 'a priority waiter jumps ahead of the normal ones and says so');
  assert.match(pw.p2.err, /position 2, priority lane, /);
  await sleep(300);
  assert.match(pw.n1.err, /position 3, held by/, 'normal waiters are pushed back and do not claim the lane');
  assert.doesNotMatch(pw.n1.err, /priority lane/);
  assert.equal(fs.existsSync(`${order}-prio`), false, 'the priority waiter does not preempt the holder');
  fs.writeFileSync(path.join(work, 'rel-prio'), '');
  assert.deepEqual(await Promise.all([pHolder.done, ...Object.values(pw).map(w => w.done)]), [0, 0, 0, 0, 0]);
  assert.deepEqual(fs.readFileSync(`${order}-prio`, 'utf8').split('\n').filter(Boolean), ['p1', 'p2', 'n1', 'n2'], 'priority callers first, each lane in ticket order');

  // The exit status of the command is the script's.
  assert.equal(await lockRun('sh', ['-c', 'exit 7']).done, 7);

  // A ticket nobody holds a lock on is stale and dropped, even if the pid it names is alive here (pid 1): ownership is the flock, which the
  // kernel frees when the owner dies in any PID namespace or container, so no pid is ever looked up.
  const q = env.AIMEBU_TEST_QUEUE;
  fs.writeFileSync(path.join(q, '000000000001.ticket'), 'pid=1\nid=ghost\nsince=2000-01-01T00:00:00Z\ncmd=ghost\n');
  const afterStale = lockRun('echo', ['after-stale']);
  assert.equal(await afterStale.done, 0);
  assert.equal(afterStale.out, 'after-stale\n');
  assert.equal(fs.existsSync(path.join(q, '000000000001.ticket')), false, 'the unlocked ticket is dropped');

  // A ticket whose owner holds its flock is live, even when the pid it names is dead here: that is a waiter from another PID namespace. It is
  // ahead in the queue, so the script waits for it, and goes once the owner (and so its lock) is gone.
  const foreign = path.join(q, '000000000002.ticket');
  fs.writeFileSync(foreign, 'pid=2999999\nid=foreign\nsince=2000-01-01T00:00:00Z\ncmd=other container\n');
  const owner = spawn('flock', [foreign, 'sh', '-c', until(path.join(work, 'rel2'))], { env });
  await sleep(300);
  const behindForeign = lockRun('echo', ['after-foreign']);
  await waitFor(() => behindForeign.err.includes('waiting for the test lock'), 'the waiter reports');
  await sleep(500);
  assert.equal(behindForeign.out, '', 'a live foreign ticket keeps its place in the queue');
  assert.equal(fs.existsSync(foreign), true);
  assert.match(behindForeign.err, /position 2,/);
  fs.writeFileSync(path.join(work, 'rel2'), '');
  assert.equal(await behindForeign.done, 0);
  assert.equal(behindForeign.out, 'after-foreign\n');
  assert.equal(fs.existsSync(foreign), false, 'the ticket is dropped once its owner is gone');

  // A real second PID namespace, when the sandbox allows one: a holder there is seen as a holder here.
  const ns = spawnSync('unshare', ['--pid', '--fork', '--mount-proc', 'true'], { env });
  if (ns.status === 0) {
    const inNs = spawn('unshare', ['--pid', '--fork', '--mount-proc', 'sh', script, 'sh', '-c', `echo in-ns; ${until(path.join(work, 'rel3'))}`], { env });
    await sleep(800);
    const behindNs = lockRun('echo', ['after-ns']);
    await waitFor(() => behindNs.err.includes('waiting for the test lock'), 'the waiter reports');
    assert.equal(behindNs.out, '', 'a holder in another PID namespace is waited for');
    assert.match(behindNs.err, /held by sh -c echo in-ns; while .* since/);
    fs.writeFileSync(path.join(work, 'rel3'), '');
    assert.equal(await behindNs.done, 0);
  } else console.log('test-lock: no PID namespace here, skipped the unshare case');

  // Plain flock on the same lock excludes the script, and the script excludes plain flock.
  // A holder file nobody holds a lock on is not believed: a plain flock holder is an unknown holder, whatever a stale file says.
  fs.writeFileSync(path.join(q, 'holder'), 'id=old\nsince=2000-01-01T00:00:00Z\ncmd=crashed run\n');
  const plain = spawn('flock', [env.AIMEBU_TEST_LOCK, 'sh', '-c', until(path.join(work, 'rel4'))], { env });
  await sleep(400);
  const behind = lockRun('echo', ['after-plain']);
  await waitFor(() => behind.err.includes('waiting for the test lock'), 'the waiter reports');
  await sleep(500);
  assert.equal(behind.out, '', 'the script waits for a plain flock holder');
  assert.match(behind.err, /held by an unknown holder/);
  assert.doesNotMatch(behind.err, /crashed run/);
  fs.writeFileSync(path.join(work, 'rel4'), '');
  assert.equal(await behind.done, 0);
  assert.equal(behind.out, 'after-plain\n');
  plain.kill();
  assert.equal(spawnSync('sh', [script], { env }).status, 2, 'no command is a usage error');
  console.log('test-lock: ok');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(() => fs.rmSync(work, { recursive: true, force: true }));
