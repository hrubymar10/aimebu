// scripts/run-jobs.cjs: the order and count of jobs, a failing job stopping new ones (and --keep-going not), each job's own TMPDIR, --shard, --only,
// the timeout, the report file, and output printed whole per job. It runs real `node -e` jobs, with no browser and no network.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const runner = path.join(__dirname, 'run-jobs.cjs');
const work = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'run-jobs-test-'));
const run = (args, listLines) => {
  const list = path.join(work, `list-${Math.random().toString(36).slice(2)}.txt`);
  fs.writeFileSync(list, `${listLines.join('\n')}\n`);
  const result = spawnSync(process.execPath, [runner, '--label', 't', ...args, list], { encoding: 'utf8', cwd: work });
  return { status: result.status, out: result.stdout + result.stderr };
};
// A job is a shell command; a script that writes a line to a shared log shows the order, and `node -e` keeps the quoting simple.
const job = code => `node -e ${JSON.stringify(code)}`;
const log = path.join(work, 'log.txt');
const note = name => job(`require('fs').appendFileSync(${JSON.stringify(log)}, ${JSON.stringify(name)} + '\\n')`);
const lines = () => (fs.existsSync(log) ? fs.readFileSync(log, 'utf8').trim().split('\n') : []);
const reset = () => fs.rmSync(log, { force: true });

// All pass: one line each, the counts, exit 0, and the jobs are named from their script.
{
  reset();
  const { status, out } = run(['-j', '1'], [note('a'), note('b'), note('c')]);
  assert.equal(status, 0, out);
  assert.deepEqual(lines(), ['a', 'b', 'c'], 'one at a time they run in list order');
  assert.match(out, /3 jobs, 1 at a time/);
  assert.match(out, /finished in [\d.]+s: 3 ok, 0 failed/);
}

// A failing job prints everything it wrote, and no new job starts after it (the one already running finishes).
{
  reset();
  const { status, out } = run(['-j', '1'], [note('a'), job("console.log('the reason'); console.error('and more'); process.exit(3)"), note('c')]);
  assert.equal(status, 1);
  assert.deepEqual(lines(), ['a'], 'c never started');
  assert.match(out, /FAIL\s+node -e/);
  assert.match(out, /\[exit 3\]/);
  assert.match(out, /    the reason\n    and more/, 'the whole output of a failed job, indented');
  assert.match(out, /1 failed, 1 not started after the failure/);
}

// --keep-going runs the rest, and the run then reports every failure at the end.
{
  reset();
  const { status } = run(['-j', '1', '--keep-going'], [job('process.exit(1)'), note('after')]);
  assert.equal(status, 1);
  assert.deepEqual(lines(), ['after']);
}

// --keep-going (what `make test-ui` passes): every job runs even after a failure, the summary lists each failed job by name, and the exit
// status is non-zero; an all-green run still exits 0.
{
  reset();
  const { status, out } = run(['-j', '1', '--keep-going'], [note('a'), job("console.log('boom one'); process.exit(7)"), note('c'), job('process.exit(1)')]);
  assert.equal(status, 1);
  assert.deepEqual(lines(), ['a', 'c'], 'the jobs after the first failure still run');
  assert.match(out, /finished in [\d.]+s: 2 ok, 2 failed/);
  const summary = out.slice(out.indexOf('finished in'));
  assert.match(summary, /FAIL node -e.*boom one/, 'the failed job is named in the end summary');
  assert.match(summary, /FAIL node -e.*process\.exit\(1\)/, 'and the second one too');
  reset();
  assert.equal(run(['-j', '1', '--keep-going'], [note('x'), note('y')]).status, 0, 'all green with --keep-going: exit 0');
}

// Jobs run side by side: two jobs that wait for each other's file only finish when they overlap.
{
  const a = path.join(work, 'ready-a'), b = path.join(work, 'ready-b');
  for (const file of [a, b]) fs.rmSync(file, { force: true });
  const meet = (mine, theirs) => job(`const fs=require('fs');fs.writeFileSync(${JSON.stringify(mine)},'x');const t=Date.now();while(!fs.existsSync(${JSON.stringify(theirs)})){if(Date.now()-t>20000)process.exit(9)}`);
  const { status, out } = run(['-j', '2'], [meet(a, b), meet(b, a)]);
  assert.equal(status, 0, `the two jobs overlapped:\n${out}`);
  for (const file of [a, b]) fs.rmSync(file, { force: true });
  const alone = run(['-j', '1', '--timeout', '1s'], [meet(a, b)]);
  assert.equal(alone.status, 1, 'a job that waits for another one fails when it runs alone');
  assert.match(alone.out, /TIMEOUT/);
}

// Each job gets its own empty TMPDIR, removed afterwards, and its own JOB_INDEX.
{
  const seen = path.join(work, 'tmpdirs.txt');
  fs.rmSync(seen, { force: true });
  const probe = job(`const fs=require('fs');const d=process.env.TMPDIR;fs.appendFileSync(${JSON.stringify(seen)},d+' '+fs.readdirSync(d).length+' '+process.env.JOB_INDEX+'\\n');fs.writeFileSync(d+'/leftover','x')`);
  const { status } = run(['-j', '2'], [probe, probe, probe]);
  assert.equal(status, 0);
  const rows = fs.readFileSync(seen, 'utf8').trim().split('\n').map(line => line.split(' '));
  assert.equal(new Set(rows.map(row => row[0])).size, 3, 'three different directories');
  assert.ok(rows.every(row => row[1] === '0'), 'each starts empty');
  assert.deepEqual(rows.map(row => row[2]).sort(), ['1', '2', '3']);
  assert.ok(rows.every(row => !fs.existsSync(row[0])), 'and each is removed when the job ends');
}

// --shard i/n takes every n-th line; --only filters; the report says what happened.
{
  reset();
  const all = ['ja', 'jb', 'jc', 'jd', 'je'].map(note);
  assert.equal(run(['-j', '1', '--shard', '2/2'], all).status, 0);
  assert.deepEqual(lines(), ['jb', 'jd']);
  reset();
  assert.equal(run(['-j', '1', '--only', 'jc'], all).status, 0);
  assert.deepEqual(lines(), ['jc']);
  const report = path.join(work, 'report-%p.json');
  const { status, out } = run(['-j', '2', '--report', report], [note('x'), job('process.exit(5)')]);
  assert.equal(status, 1);
  const written = fs.readdirSync(work).filter(name => /^report-\d+\.json$/.test(name));
  assert.equal(written.length, 1, `%p became a process id: ${written} ${out}`);
  const data = JSON.parse(fs.readFileSync(path.join(work, written[0]), 'utf8'));
  assert.deepEqual(data.results.map(result => result.status).sort(), ['fail', 'ok']);
  assert.equal(data.results.find(result => result.status === 'fail').exit, 5);
}

// Bad input is refused with a message, not a stack trace.
{
  const { status, out } = run(['-j', '0'], [note('a')]);
  assert.equal(status, 2);
  assert.match(out, /-j must be a whole number of at least 1/);
  assert.equal(run(['--shard', '3/2'], [note('a')]).status, 2);
}

fs.rmSync(work, { recursive: true, force: true });
console.log('run-jobs: ok');
