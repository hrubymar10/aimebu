#!/usr/bin/env node
'use strict';
// Dry run of scripts/variant-gate.cjs with no model spend and no real CLIs.
//
// It builds aimebu, starts an isolated scratch server (with its local supervisor) with
// fake `claude`, `codex`, and `pi` programs first on PATH, and runs the gate's
// native variants against it. The fakes speak just enough of each tool's stream
// protocol for the real adapters to accept them: they answer the word a note
// asks for, and take eight seconds on a "Count" task so the kill step has a
// process to find. The docker variants have no launcher here, so they must come
// back as skips with a reason, which exercises that path too.
//
// Everything lives in a temp directory with HOME, CODEX_HOME, CLAUDE_CONFIG_DIR,
// and the aimebu config redirected into it; no real credential file is touched.

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const assert = require('node:assert/strict');
const { spawn, spawnSync } = require('node:child_process');

const { Client } = require('./variant-gate.cjs');
const { startServer, stopLocalSupervisor } = require('./server-start.cjs');
const root = path.resolve(__dirname, '..');
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'vg-selftest-'));
const home = path.join(tmp, 'home');
const bin = path.join(tmp, 'bin');
const folderBase = path.join(tmp, 'projects');
for (const dir of [home, bin, folderBase]) fs.mkdirSync(dir, { recursive: true });

// One body for all three fakes; the first argument picks the protocol.
const fakeSource = `#!/usr/bin/env node
const tool = require('node:path').basename(process.argv[1]);
const args = process.argv.slice(2);
if (args.includes('--version')) { console.log({ claude: '2.1.0 (fake)', codex: 'codex-cli 0.162.1', pi: '1.1.0', opencode: '1.19.0' }[tool]); process.exit(0); }
const arg = name => { const i = args.indexOf(name); return i >= 0 ? args[i + 1] : ''; };
let input = '';
process.stdin.on('data', d => input += d);
process.stdin.on('end', async () => {
  const text = input.slice(Math.max(0, input.indexOf('{"turn_id"')));
  let turn = {}; try { turn = JSON.parse(text); } catch {}
  const notes = turn.notes || [];
  const bodies = notes.map(n => n.body || '').join('\\n');
  const room = (turn.context?.rooms || [])[0]?.room_id || '';
  const word = (bodies.match(/word ([A-Z]+)/) || [])[1] || 'OK';
  const count = /Count from 1 to 40/.test(bodies);
  if (count) await new Promise(r => setTimeout(r, 8000));
  const answer = JSON.stringify({ messages: room && bodies ? [{ room, body: count ? 'DONE' : word, reply_to: null, thread_root: null, attachments: null, ask: null, handoff: null, visual_plan: null, appendix_pages: null, needs_attention: null, proposed_answers: null, open_questions: null }] : [] });
  const emit = o => console.log(JSON.stringify(o));
  if (tool === 'claude') {
    const session = arg('--resume') || arg('--session-id') || '11111111-1111-4111-8111-111111111111';
    emit({ type: 'system', subtype: 'init', session_id: session });
    emit({ type: 'result', result: answer, usage: { input_tokens: 10, output_tokens: 5 }, modelUsage: { fake: { costUSD: 0.001, contextWindow: 200000 } } });
  } else if (tool === 'codex') {
    const resumeAt = args.indexOf('resume');
    const thread = resumeAt >= 0 ? args[args.indexOf('-', resumeAt) - 1] : '22222222-2222-4222-8222-222222222222';
    emit({ type: 'thread.started', thread_id: thread });
    emit({ type: 'turn.started' });
    emit({ type: 'item.completed', item: { type: 'agent_message', text: answer } });
    emit({ type: 'turn.completed', usage: { input_tokens: 10, output_tokens: 5 } });
  } else if (tool === 'opencode') {
    const session = arg('-s') || 'ses_selftest0001';
    emit({ type: 'step_start', sessionID: session, part: { type: 'step-start' } });
    emit({ type: 'text', sessionID: session, part: { type: 'text', text: answer } });
    emit({ type: 'step_finish', sessionID: session, part: { type: 'step-finish', reason: 'stop', tokens: { total: 15, input: 10, output: 5, reasoning: 0, cache: { read: 0, write: 0 } }, cost: 0.001 } });
  } else {
    emit({ type: 'session', version: 3, id: arg('--session-id') });
    emit({ type: 'agent_start' }); emit({ type: 'turn_start' });
    const message = { role: 'assistant', content: [{ type: 'text', text: answer }], usage: { input: 10, output: 5, cost: { total: 0.001 } } };
    emit({ type: 'message_end', message }); emit({ type: 'turn_end', message }); emit({ type: 'agent_settled', aborted: false });
  }
});
`;
for (const tool of ['claude', 'codex', 'pi', 'opencode']) fs.writeFileSync(path.join(bin, tool), fakeSource, { mode: 0o755 });

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function main() {
  const aimebu = path.join(tmp, 'aimebu');
  const build = spawnSync('go', ['build', '-o', aimebu, './cmd/aimebu'], { cwd: root, stdio: 'inherit' });
  if (build.status !== 0) throw new Error('go build failed');
  const inheritedEnv = { ...process.env };
  for (const name of ['AIMEBU_URL', 'AIMEBU_BIND', 'AIMEBU_ALLOW', 'AIMEBU_ALLOWED_HOSTS', 'AIMEBU_TLS_CERT', 'AIMEBU_TLS_KEY', 'AIMEBU_TLS_PORT', 'AIMEBU_OWNER_LOGIN', 'AIMEBU_OWNER_PASSWORD']) delete inheritedEnv[name];
  let serverEnv;
  const { child: server, origin, logs: serverLogs } = await startServer(aimebu, port => (serverEnv = { ...inheritedEnv, HOME: home, CODEX_HOME: path.join(home, '.codex'), CLAUDE_CONFIG_DIR: path.join(home, '.claude'), AIMEBU_CONFIG_DIR: path.join(tmp, 'config'),
    PATH: `${bin}${path.delimiter}${process.env.PATH}`, AIMEBU_ALLOW_NATIVE: '1', AIMEBU_USAGES: '0', AIMEBU_SENTRY_DSN: 'off', AIMEBU_PORT: String(port), AIMEBU_OWNER_LOGIN: 'owner', AIMEBU_OWNER_PASSWORD: 'selftest-password-123' }), { label: 'variant-gate server', cwd: tmp });
  try {
    // Nothing is seeded: add the four native harnesses the fakes stand in for, each with its cheap model, as an owner would.
    const api = new Client(origin);
    await api.login('owner', 'selftest-password-123');
    // The local supervisor is a process of its own that connects a moment after the server answers; launchers need it connected.
    let host;
    for (let i = 0; i < 60; i++) {
      const machines = (await api.must('GET', '/api/machines')).machines;
      host = machines.find(m => m.is_host) || machines[0];
      if (host && host.connected) break;
      await sleep(500);
    }
    if (!host || !host.connected) throw new Error('the local supervisor did not connect within 30 s');
    const cheap = { claude: 'haiku', codex: 'gpt-6-luna', pi: 'ollama-cloud/gemma4:31b', opencode: 'ollama-cloud/gemma4:31b' };
    for (const tool of Object.keys(cheap)) {
      const made = await api.must('POST', `/api/machines/${host.id}/launchers`, { tool, command: tool, args: [], env_refs: {}, enabled: true, containerized: false, path_maps: [], visible_roots: [], supports_toolbox: true });
      await api.must('PUT', `/api/machines/${host.id}/launchers/${made.config.id}/models`, { models: [{ slug: cheap[tool], label: cheap[tool], note: '', default: true }] });
    }
    for (let i = 0; i < 60; i++) {
      const catalog = await api.must('GET', '/api/spawn-catalog');
      if (['claude', 'codex', 'pi', 'opencode'].every(tool => catalog.tools.find(t => t.tool === tool && t.variant === 'native' && t.available))) break;
      await sleep(1000);
      if (i === 59) throw new Error(`native variants never became available: ${JSON.stringify(catalog.tools.map(t => [t.tool, t.variant, t.available, t.unavailable_reason]))}`);
    }
    const reportDir = path.join(tmp, 'report');
    const gate = spawnSync(process.execPath, [path.join(__dirname, 'variant-gate.cjs'), '--server', origin, '--yes', '--variants', 'claude-native,codex-native,pi-native,opencode-native,claude-docker',
      '--folder', path.join(folderBase, 'gate'), '--pause-wait', '4', '--task-timeout', '90', '--report-dir', reportDir, '--supervisor-match', 'aimebu.* supervisor'],
      { env: { ...process.env, VG_LOGIN: 'owner', VG_PASSWORD: 'selftest-password-123' }, encoding: 'utf8', timeout: 20 * 60 * 1000 });
    process.stdout.write(gate.stdout || ''); process.stderr.write(gate.stderr || '');
    const report = JSON.parse(fs.readFileSync(path.join(reportDir, 'report.json'), 'utf8'));
    const byName = Object.fromEntries(report.variants.map(v => [v.variant, v]));
    for (const name of ['claude-native', 'codex-native', 'pi-native', 'opencode-native']) {
      const v = byName[name];
      assert.notEqual(v.status, 'skipped', `${name} must run in the selftest, but was skipped: ${v.skipped}`);
      assert.equal(v.status, 'pass', `${name}: ${JSON.stringify(v.steps, null, 1)}`);
      assert.ok(v.killed && v.killed.pid, `${name}: the kill step did not record a process`);
    }
    assert.equal(report.account, 'owner', 'the owner run records its account');
    assert.equal(byName['claude-docker'].status, 'skipped', 'a variant with no launcher must be skipped, not failed');
    assert.ok(byName['claude-docker'].skipped.length > 0, 'a skip must say why');
    assert.ok(report.variants.every(v => v.status !== 'fail'), 'no variant may fail');
    // The password-free path: a one-hour project invitation for a throwaway account.
    const project = report.project;
    const invite = await api.must('POST', '/api/invites', { kind: 'join', project_id: project.id, role: 'owner' });
    const invited = spawnSync(process.execPath, [path.join(__dirname, 'variant-gate.cjs'), '--server', origin, '--yes', '--invite', invite.url, '--project', project.name, '--variants', 'claude-native',
      '--pause-wait', '3', '--task-timeout', '90', '--kill', 'skip', '--report-dir', path.join(tmp, 'report-invite')], { env: { ...process.env, VG_LOGIN: '', VG_PASSWORD: '' }, encoding: 'utf8', timeout: 10 * 60 * 1000 });
    process.stderr.write(invited.stderr || '');
    const invitedReport = JSON.parse(fs.readFileSync(path.join(tmp, 'report-invite', 'report.json'), 'utf8'));
    assert.match(invitedReport.account, /^variant-gate-[0-9a-f]{8}$/, 'the invited run uses its own account');
    assert.equal(invitedReport.variants[0].status, 'pass', `invited claude-native: ${JSON.stringify(invitedReport.variants[0].steps, null, 1)}`);
    assert.ok(!JSON.stringify(invitedReport).includes(invite.token), 'the invitation token must not appear in the report');
    const reuse = spawnSync(process.execPath, [path.join(__dirname, 'variant-gate.cjs'), '--server', origin, '--invite', invite.url, '--project', project.name, '--list'], { encoding: 'utf8' });
    assert.equal(reuse.status, 0, '--list never touches the network or the invitation');
    const unconfirmed = spawnSync(process.execPath, [path.join(__dirname, 'variant-gate.cjs'), '--server', origin, '--invite', invite.url, '--project', project.name], { encoding: 'utf8' });
    assert.equal(unconfirmed.status, 2, 'a run without --yes must refuse, even against loopback');
    const noServer = spawnSync(process.execPath, [path.join(__dirname, 'variant-gate.cjs'), '--yes', '--invite', invite.url, '--project', project.name], { encoding: 'utf8', env: { ...process.env, VG_SERVER: '' } });
    assert.equal(noServer.status, 2, 'a run with no --server must refuse; there is no default');
    const spent = await (async () => { const second = new Client(origin); return second.request('POST', '/api/invites/redeem', { token: invite.token, login: 'late-comer', password: 'x'.repeat(16) }, { noCsrf: true }); })();
    assert.notEqual(spent.status, 201, 'an invitation works once');
    console.log('variant-gate selftest passed');
  } catch (error) {
    console.error(`--- server log tail ---\n${serverLogs().slice(-4000)}`);
    throw error;
  } finally {
    const exited = new Promise(resolve => { if (server.exitCode !== null || server.signalCode) resolve(); else server.once('close', resolve); });
    server.kill('SIGTERM');
    await Promise.race([exited, sleep(15000)]);
    stopLocalSupervisor(aimebu, serverEnv); // the local supervisor that ran the fakes outlives the server
    if (!process.env.VG_SELFTEST_KEEP) fs.rmSync(tmp, { recursive: true, force: true }); else console.log(`kept ${tmp}`);
  }
}
main().catch(error => { console.error(error.stack || error.message); process.exitCode = 1; });
