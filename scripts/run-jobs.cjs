#!/usr/bin/env node
// Runs a list of commands, a few at a time, and reports them together. It is what `make test-ui` and `make test-web` use, so the list of
// scripts lives in one file each (scripts/ui-tests.list, the web tests in the Makefile) and the way they run in one place.
//
// usage: node scripts/run-jobs.cjs [-j N] [--label ui] [--report file.json] [--keep-going] [--timeout 15m] [--only substring]
//                                  [--shard i/n] [--cwd dir] <list-file | -- command...>
//
// A list file has one command per line (`node scripts/test-chat-page.cjs`); blank lines and lines starting with # are skipped. Each job runs in
// its own process group with its own TMPDIR (a new directory, removed afterwards) so two jobs, or two runs of this script, never share a path
// under the temporary directory. Output is buffered per job and printed whole when the job ends, so lines of different jobs never interleave: a
// passing job prints one line, a failing one prints everything it wrote.
//
// After the first failure no new job starts (the running ones finish), unless --keep-going. The exit status is 0 only when every job passed.
// The summary says how long the run took against how long the jobs would take one after the other, and lists the slowest jobs; --report
// writes every job's result as JSON (name, status, seconds, exit code, the tail of its output); %p in the path is this run's process id.
//
// -j defaults to $JOBS, else to a quarter of the CPUs (at least 1, at most 4): each page test starts its own Chromium, and the page tests wait
// on timers, so more jobs than the machine can run at once make them slow and flaky instead of fast. --shard i/n runs every n-th job starting
// at i (1-based) of the list in its given order: how several machines (or several invocations) split one list without talking to each other.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');

function parseDuration(text) {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)?$/.exec(text);
  if (!match) throw new Error(`bad duration ${text}`);
  return Number(match[1]) * { ms: 1, s: 1000, m: 60000, h: 3600000, undefined: 1000 }[match[2]];
}

function parseArgs(argv) {
  const options = { jobs: Number(process.env.JOBS) || Math.max(1, Math.min(4, Math.floor(os.cpus().length / 4))), label: 'jobs', keepGoing: false, timeout: 15 * 60 * 1000, cwd: process.cwd() };
  let rest = [];
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === '--') { rest = argv.slice(i + 1); options.command = true; break; }
    else if (arg === '-j' || arg === '--jobs') options.jobs = Number(argv[++i]);
    else if (arg === '--label') options.label = argv[++i];
    else if (arg === '--report') options.report = argv[++i];
    else if (arg === '--keep-going') options.keepGoing = true;
    else if (arg === '--timeout') options.timeout = parseDuration(argv[++i]);
    else if (arg === '--only') options.only = argv[++i];
    else if (arg === '--shard') options.shard = argv[++i];
    else if (arg === '--cwd') options.cwd = path.resolve(argv[++i]);
    else if (arg.startsWith('-')) throw new Error(`unknown flag ${arg}`);
    else rest.push(arg);
  }
  if (!Number.isInteger(options.jobs) || options.jobs < 1) throw new Error(`-j must be a whole number of at least 1, got ${options.jobs}`);
  options.rest = rest;
  return options;
}

function readList(file) {
  return fs.readFileSync(file, 'utf8').split('\n').map(line => line.trim()).filter(line => line && !line.startsWith('#'));
}

// The name a job goes by in the output: the script's file name without its directory, prefix and extension, with its arguments.
function nameOf(command) {
  const words = command.split(/\s+/);
  const script = words.findIndex(word => /\.(cjs|js|sh)$/.test(word));
  if (script < 0) return command;
  const base = path.basename(words[script]).replace(/^test-/, '').replace(/\.(cjs|js|sh)$/, '');
  return [base, ...words.slice(script + 1)].join(' ');
}

const pad = (n, width) => String(n).padStart(width);
const seconds = ms => (ms / 1000).toFixed(1);

async function main() {
  const options = parseArgs(process.argv.slice(2));
  let commands = options.command ? [options.rest.join(' ')] : readList(options.rest[0] || (() => { throw new Error('give a list file or -- command'); })());
  if (options.only) commands = commands.filter(command => command.includes(options.only));
  if (options.shard) {
    const [index, count] = options.shard.split('/').map(Number);
    if (!(index >= 1 && index <= count)) throw new Error(`bad --shard ${options.shard}`);
    commands = commands.filter((_, i) => i % count === index - 1);
  }
  if (!commands.length) { console.error(`[${options.label}] nothing to run`); process.exit(1); }
  const total = commands.length, width = String(total).length, jobs = Math.min(options.jobs, total);
  console.log(`[${options.label}] ${total} job${total === 1 ? '' : 's'}, ${jobs} at a time`);

  const started = Date.now();
  const results = new Array(total);
  let next = 0, failed = false, running = 0, finished = 0;
  const children = new Set();
  const stop = signal => { for (const child of children) { try { process.kill(-child.pid, signal); } catch { /* already gone */ } } };
  for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { stop('SIGTERM'); process.exit(130); });

  function runOne(index) {
    const command = commands[index], name = nameOf(command);
    const tmp = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), `${options.label}-job-`));
    const begin = Date.now();
    console.log(`[${options.label} ${pad(index + 1, width)}/${total}] start  ${name}`);
    return new Promise(resolve => {
      const child = spawn('sh', ['-c', command], { cwd: options.cwd, env: { ...process.env, TMPDIR: tmp, JOB_INDEX: String(index + 1) }, detached: true, stdio: ['ignore', 'pipe', 'pipe'] });
      children.add(child);
      let output = '', timedOut = false;
      const collect = data => { output += data; };
      child.stdout.on('data', collect); child.stderr.on('data', collect);
      const timer = setTimeout(() => { timedOut = true; try { process.kill(-child.pid, 'SIGKILL'); } catch { /* gone */ } }, options.timeout);
      child.on('close', (code, signal) => {
        clearTimeout(timer); children.delete(child);
        fs.rmSync(tmp, { recursive: true, force: true });
        const ms = Date.now() - begin, ok = code === 0 && !timedOut;
        results[index] = { name, command, status: ok ? 'ok' : timedOut ? 'timeout' : 'fail', seconds: Number((ms / 1000).toFixed(1)), exit: code, signal, output: output.length > 20000 ? `...${output.slice(-20000)}` : output };
        finished++;
        if (ok) console.log(`[${options.label} ${pad(index + 1, width)}/${total}] ok     ${name} (${seconds(ms)}s)  [${finished}/${total} done]`);
        else {
          console.log(`[${options.label} ${pad(index + 1, width)}/${total}] ${timedOut ? 'TIMEOUT' : 'FAIL'}   ${name} (${seconds(ms)}s)  [exit ${code}${signal ? `, ${signal}` : ''}]`);
          console.log(output.replace(/\n$/, '').split('\n').map(line => `    ${line}`).join('\n'));
        }
        resolve();
      });
    });
  }

  await new Promise(resolveAll => {
    const launch = () => {
      while (running < jobs && next < total && (options.keepGoing || !failed)) {
        const index = next++; running++;
        runOne(index).then(() => { running--; if (results[index].status !== 'ok') failed = true; launch(); });
      }
      if (running === 0) resolveAll();
    };
    launch();
  });

  const done = results.filter(Boolean), bad = done.filter(result => result.status !== 'ok'), notRun = total - done.length;
  const wall = Date.now() - started, sum = done.reduce((all, result) => all + result.seconds * 1000, 0);
  console.log(`[${options.label}] finished in ${seconds(wall)}s: ${done.length - bad.length} ok, ${bad.length} failed${notRun ? `, ${notRun} not started after the failure` : ''}; the jobs add up to ${seconds(sum)}s (${(sum / Math.max(wall, 1)).toFixed(1)}x)`);
  const slow = [...done].sort((a, b) => b.seconds - a.seconds).slice(0, 5).map(result => `${result.name} ${result.seconds.toFixed(0)}s`);
  console.log(`[${options.label}] slowest: ${slow.join(', ')}`);
  for (const result of bad) console.log(`[${options.label}] ${result.status.toUpperCase()} ${result.name}`);
  if (options.report) {
    options.report = options.report.replace('%p', String(process.pid)); // one file per run, so two runs never write the same one
    fs.mkdirSync(path.dirname(path.resolve(options.report)), { recursive: true });
    fs.writeFileSync(options.report, JSON.stringify({ label: options.label, jobs, wall_seconds: Number((wall / 1000).toFixed(1)), jobs_seconds: Number((sum / 1000).toFixed(1)), results: results.map((result, index) => result || { name: nameOf(commands[index]), command: commands[index], status: 'not_run' }) }, null, 2));
  }
  process.exit(bad.length || notRun ? 1 : 0);
}

main().catch(error => { console.error(error.message); process.exit(2); });
