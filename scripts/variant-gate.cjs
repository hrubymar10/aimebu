#!/usr/bin/env node
'use strict';
// Cutover gate: every tool/container variant must pass the core task, control,
// and recovery path against a running aimebu v2 server.
//
// Per variant (claude, codex, pi) x (native, docker), plus opencode native, the script drives the
// normal HTTP API as the instance owner:
//   1 room      create a scratch task room (the script holds it to a $1 cost cap and 6 turns)
//   2 spawn     add one agent with the cheapest model
//   3 task      ask for the word PONG and assert the reply
//   4 pause     pause the room, post, assert no turn runs; resume, assert the turn
//   5 kill      SIGKILL the harness process mid-turn, assert the agent answers again (the server
//               continues the interrupted turn itself; a recovery item, if one surfaces, is decided Resume)
//   6 cleanup   stop the agent, archive the room
//
// It spends real model money, so it needs --yes and an explicit --server. Usage, flags, and the safety rails are in --help.
// Credentials come only from VG_LOGIN and VG_PASSWORD, or from a one-time invitation, which the caller supplies.

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawnSync } = require('node:child_process');

const TOOLS = ['claude', 'codex', 'pi', 'opencode'];
const FORMS = ['native', 'docker'];
// The gate drives OpenCode native only; its container variant is not covered yet.
const CHEAP_MODEL = { claude: 'haiku', codex: 'gpt-6-luna', pi: 'ollama-cloud/gemma4:31b', opencode: 'ollama-cloud/gemma4:31b' };
const STEPS = ['room', 'spawn', 'task', 'pause', 'resume', 'kill', 'recovery', 'stop', 'archive'];

function usage() {
  return `variant-gate: run the cutover gate against an aimebu v2 server.

  VG_LOGIN=owner VG_PASSWORD=... node scripts/variant-gate.cjs [options]
  node scripts/variant-gate.cjs --invite '<link or token>' --project variant-gate [options]

Options
  --server URL          server to drive (required; $VG_SERVER also works). There is no default, so a run can never
                        reach the dev server by omission
  --variants LIST       comma list of tool-form pairs (default all seven), e.g. claude-native,pi-docker,opencode-native
  --invite LINK|TOKEN   instead of a password: redeem a one-hour project invitation (role owner or operator) for a new
                        account the script names and gives a random password; $VG_INVITE also works. Needs --project.
  --project NAME|ID     reuse a project; default finds or creates one named "variant-gate"
  --folder PATH         folder for a project this run creates (default <tmp>/vg-<ts>); must exist on the server host
  --budget USD          stop starting variants once measured cost reaches this (default 6)
  --variant-cap USD     per-variant cost cap the script enforces from its room's usage (default 1)
  --turn-cap N          per-variant turn cap the script enforces (default 6)
  --task-timeout S      wait for each agent reply (default 240)
  --pause-wait S        how long to prove no turn runs while paused (default 20)
  --kill local|skip     SIGKILL the harness on this host, or skip that step (default local)
  --supervisor-match RE command line of the supervisor whose child is the harness (default "aimebu.* (supervisor|server)")
  --report-dir DIR      where the JSON and markdown reports go (default <tmp>/variant-gate-<ts>)
  --list                print the plan and exit without any network call
  --yes                 required to run at all: the gate spends real model money (not needed for --list or --help)
  --help

Exit status: 0 when every attempted variant passes every step (skips are fine),
1 when any step fails, 2 for a usage error.`;
}

function parseArgs(argv) {
  const opts = {
    server: process.env.VG_SERVER || '', variants: null, project: '', invite: process.env.VG_INVITE || '', folder: '', budget: 6, variantCap: 1,
    turnCap: 6, taskTimeout: 240, pauseWait: 20, kill: 'local', supervisorMatch: 'aimebu.* (supervisor|server)', reportDir: '',
    list: false, yes: false,
  };
  const need = (name, i) => { if (i + 1 >= argv.length) throw new Error(`${name} needs a value`); return argv[i + 1]; };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--help' || a === '-h') { console.log(usage()); process.exit(0); }
    else if (a === '--list') opts.list = true;
    else if (a === '--yes') opts.yes = true;
    else if (a === '--server') opts.server = need(a, i++);
    else if (a === '--variants') opts.variants = need(a, i++).split(',').map(s => s.trim()).filter(Boolean);
    else if (a === '--project') opts.project = need(a, i++);
    else if (a === '--invite') opts.invite = need(a, i++);
    else if (a === '--folder') opts.folder = need(a, i++);
    else if (a === '--budget') opts.budget = Number(need(a, i++));
    else if (a === '--variant-cap') opts.variantCap = Number(need(a, i++));
    else if (a === '--turn-cap') opts.turnCap = Number(need(a, i++));
    else if (a === '--task-timeout') opts.taskTimeout = Number(need(a, i++));
    else if (a === '--pause-wait') opts.pauseWait = Number(need(a, i++));
    else if (a === '--kill') opts.kill = need(a, i++);
    else if (a === '--supervisor-match') opts.supervisorMatch = need(a, i++);
    else if (a === '--report-dir') opts.reportDir = need(a, i++);
    else throw new Error(`unknown option ${a}`);
  }
  for (const [name, value] of [['budget', opts.budget], ['variant-cap', opts.variantCap], ['turn-cap', opts.turnCap], ['task-timeout', opts.taskTimeout], ['pause-wait', opts.pauseWait]]) {
    if (!Number.isFinite(value) || value <= 0) throw new Error(`--${name} must be a positive number`);
  }
  if (!['local', 'skip'].includes(opts.kill)) throw new Error('--kill must be local or skip');
  if (opts.invite && !opts.project) throw new Error('--invite needs --project: an invited account cannot create a project');
  const all = TOOLS.flatMap(tool => FORMS.filter(form => tool !== 'opencode' || form === 'native').map(form => `${tool}-${form}`));
  opts.variants = opts.variants || all;
  for (const variant of opts.variants) if (!all.includes(variant)) throw new Error(`unknown variant ${variant}; choose from ${all.join(', ')}`);
  return opts;
}

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const stamp = () => new Date().toISOString().replace(/[-:]/g, '').replace(/\..*/, 'Z');

class Client {
  constructor(server) { this.server = server.replace(/\/$/, ''); this.cookies = new Map(); this.csrf = ''; }
  async login(login, password) {
    const { status, body } = await this.request('POST', '/api/login', { login, password }, { noCsrf: true });
    if (status !== 200 || !body?.csrf_token) throw new Error(`login failed (${status}${body?.error ? ' ' + body.error : ''})`);
    this.csrf = body.csrf_token;
  }
  async request(method, path, body, { idempotency = false, noCsrf = false, retry = true } = {}) {
    const headers = { Accept: 'application/json', Origin: this.server };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (!noCsrf && method !== 'GET') headers['X-CSRF-Token'] = this.csrf;
    if (idempotency) headers['Idempotency-Key'] = crypto.randomUUID();
    if (this.cookies.size) headers.Cookie = [...this.cookies].map(([k, v]) => `${k}=${v}`).join('; ');
    for (let attempt = 0; ; attempt++) {
      const response = await fetch(this.server + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
      for (const raw of response.headers.getSetCookie?.() || []) { const [pair] = raw.split(';'); const eq = pair.indexOf('='); this.cookies.set(pair.slice(0, eq), pair.slice(eq + 1)); }
      let parsed = null; const text = await response.text(); try { parsed = text ? JSON.parse(text) : null; } catch { parsed = { raw: text.slice(0, 200) }; }
      if (response.status === 429 && retry && attempt < 3) { await sleep(Math.min(30, Number(response.headers.get('Retry-After')) || 2) * 1000); continue; }
      return { status: response.status, body: parsed };
    }
  }
  async must(method, path, body, options) {
    const { status, body: parsed } = await this.request(method, path, body, options);
    if (status < 200 || status >= 300) throw new Error(`${method} ${path} -> ${status} ${parsed?.error || ''} ${parsed?.message || ''}`.trim());
    return parsed;
  }
}

// An invitation stands in for the owner's password: the caller mints a one-hour
// single-use project invite, and the script creates a fresh account with a random
// password that lives only in this process.
function inviteToken(value) {
  const match = String(value).match(/[#&?]invite=([^&\s]+)/);
  return decodeURIComponent(match ? match[1] : String(value).trim());
}

async function redeemInvite(api, invite) {
  const login = `variant-gate-${crypto.randomBytes(4).toString('hex')}`;
  const password = crypto.randomBytes(18).toString('base64url');
  const { status, body } = await api.request('POST', '/api/invites/redeem', { token: inviteToken(invite), login, password }, { noCsrf: true });
  if (status !== 201 || body?.kind !== 'join') throw new Error(`invitation not accepted (${status}${body?.error ? ' ' + body.error : ''}); it must be an unused, unexpired join invite`);
  await api.login(body.login || login, password);
  return { login: body.login || login, role: body.role };
}

// --- process finding (the kill step) -------------------------------------

function ps() {
  const out = spawnSync('ps', ['-axo', 'pid=,ppid=,pgid=,command='], { encoding: 'utf8' });
  if (out.status !== 0) return [];
  return out.stdout.split('\n').map(line => line.match(/^\s*(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/)).filter(Boolean)
    .map(m => ({ pid: Number(m[1]), ppid: Number(m[2]), pgid: Number(m[3]), command: m[4] }));
}

function cwdOf(pid) {
  try { return fs.realpathSync(`/proc/${pid}/cwd`); } catch { /* not Linux or gone */ }
  const out = spawnSync('lsof', ['-a', '-d', 'cwd', '-p', String(pid), '-Fn'], { encoding: 'utf8' });
  const line = (out.stdout || '').split('\n').find(l => l.startsWith('n'));
  return line ? line.slice(1) : '';
}

// The harness is a direct child of the supervisor whose working directory is the
// agent's checkout. Falls back to any process in the tool's binary family there.
function findHarness({ folder, supervisorMatch, tool, form }) {
  const table = ps(); const bySelf = new Map(table.map(p => [p.pid, p]));
  const supervisors = new Set(table.filter(p => p.pid !== process.pid && new RegExp(supervisorMatch).test(p.command)).map(p => p.pid));
  const want = fs.existsSync(folder) ? fs.realpathSync(folder) : folder;
  const binaries = form === 'docker' ? [`${tool}-docker`, tool] : [tool];
  const named = p => binaries.includes(path.basename(p.command.split(/\s+/)[0]));
  const here = p => { const cwd = cwdOf(p.pid); return cwd === want || cwd.startsWith(want + path.sep); };
  const direct = table.filter(p => supervisors.has(p.ppid) && here(p));
  const pick = direct[0] || table.find(p => p.pid !== process.pid && named(p) && here(p) && !bySelf.get(p.ppid)?.command.includes('variant-gate'));
  return pick || null;
}

// --- steps ----------------------------------------------------------------

class Variant {
  constructor(ctx, name) {
    this.ctx = ctx; this.name = name; [this.tool, this.form] = name.split('-');
    this.steps = {}; this.cost = 0; this.turns = 0; this.notes = []; this.skip = ''; this.started = Date.now();
  }
  async step(name, fn, blockedBy) {
    if (blockedBy) { this.steps[name] = { status: 'skipped', detail: `blocked by ${blockedBy}`, ms: 0 }; return false; }
    const t0 = Date.now();
    try {
      const detail = await fn();
      this.steps[name] = { status: 'pass', detail: detail || '', ms: Date.now() - t0 };
      return true;
    } catch (error) {
      this.steps[name] = { status: 'fail', detail: String(error.message || error), ms: Date.now() - t0 };
      return false;
    }
  }
  failedStep() { return STEPS.find(name => this.steps[name]?.status === 'fail'); }

  async agentMessage(afterSeq, pattern, timeoutS, watchFailure = true) {
    const { api } = this.ctx; const deadline = Date.now() + timeoutS * 1000; let last = afterSeq;
    while (Date.now() < deadline) {
      const messages = await api.must('GET', `/api/rooms/${this.room.id}/messages?after=${last}&limit=100`);
      for (const message of messages) {
        last = Math.max(last, message.seq);
        if (message.author_kind === 'agent' && message.author_id === this.agent.id && (!pattern || pattern.test(message.body))) return message;
      }
      if (watchFailure) { const failure = await this.failureEvidence(); if (failure) throw new Error(failure); }
      await sleep(2000);
    }
    throw new Error(`no matching reply within ${timeoutS}s`);
  }

  async card() { return (await this.ctx.api.must('GET', `/api/rooms/${this.room.id}/agents`)).find(c => c.id === this.agent.id); }

  // A provider limit or an agent stuck in recovery means waiting longer cannot help.
  async failureEvidence() {
    const card = await this.card();
    if (!card) return 'agent disappeared';
    const flag = (card.health_flags || []).find(f => f.code === 'out_of_quota');
    if (flag) { this.notes.push('provider limit reached'); return `provider usage limit reached${flag.reset_at ? ` until ${flag.reset_at}` : ''}`; }
    if (card.recovery_block && !this.expectRecovery) return `agent entered recovery: ${(card.last_exit_evidence || 'no evidence').slice(0, 300)}`;
    return '';
  }

  async post(body) {
    const message = await this.ctx.api.must('POST', `/api/rooms/${this.room.id}/messages`, { body });
    return message;
  }

  async measure() {
    try {
      const usage = await this.ctx.api.must('GET', `/api/rooms/${this.room.id}/usage?range=today`);
      this.cost = Number(usage.cost) || 0; this.turns = Number(usage.turns) || 0; this.coverage = usage.cost_coverage;
    } catch { /* keep the last reading */ }
    return this.cost;
  }
  // Usage limits are per project now (all rooms together), so the per-variant caps are the script's own, read from the room's usage.
  overCap() { return this.cost >= this.ctx.opts.variantCap || this.turns >= this.ctx.opts.turnCap; }

  async run() {
    const { api, opts, catalog } = this.ctx;
    const entry = catalog.tools.find(t => t.tool === this.tool && t.variant === this.form);
    if (!entry) { this.skip = 'the server does not list this variant'; return; }
    if (!entry.available) { this.skip = `unavailable: ${entry.unavailable_reason || 'no reason given'}`; return; }
    const model = (entry.models || []).find(m => m.id === CHEAP_MODEL[this.tool]);
    if (!model) { this.skip = `cheapest model ${CHEAP_MODEL[this.tool]} is not listed on this harness (listed: ${(entry.models || []).map(m => m.id).join(', ') || 'none'}); add it under Settings → Harnesses & models`; return; }
    this.model = model.id;
    const ts = stamp();
    let ok = await this.step('room', async () => {
      this.room = await api.must('POST', `/api/projects/${this.ctx.project.id}/rooms`, { title: `vg-${this.name}-${ts}` });
      return `room ${this.room.id}, script caps $${opts.variantCap} / ${opts.turnCap} turns`;
    });
    ok = await this.step('spawn', async () => {
      const created = await api.must('POST', `/api/rooms/${this.room.id}/agents`, { role_key: 'worker', tool: this.tool, variant: this.form, model: this.model }, { idempotency: true });
      this.agent = created;
      const deadline = Date.now() + 120000; let card;
      while (Date.now() < deadline) {
        card = await this.card();
        if (card && card.desired === 'active' && card.observed === 'idle' && !card.recovery_block && !card.switch && !card.boundary_pending) break;
        if (card?.recovery_block) throw new Error(`spawned agent entered recovery: ${(card.last_exit_evidence || '').slice(0, 300)}`);
        card = null; await sleep(2000);
      }
      if (!card) throw new Error('agent did not become idle within 120s');
      this.agent = card;
      return `${card.slug} (${this.model}), generation ${card.generation}`;
    }, ok ? '' : 'room');
    const alive = ok;
    ok = await this.step('task', async () => {
      const posted = await this.post(`@${this.agent.slug} Reply with exactly the word PONG and nothing else.`);
      const reply = await this.agentMessage(posted.seq, /\bPONG\b/i, opts.taskTimeout);
      await this.measure();
      return `reply #${reply.seq}: ${reply.body.slice(0, 60).replace(/\s+/g, ' ')}`;
    }, alive ? '' : 'spawn');
    const tasked = ok;
    ok = await this.step('pause', async () => {
      const control = await api.must('GET', `/api/rooms/${this.room.id}/control`);
      await api.must('POST', `/api/rooms/${this.room.id}/control`, { action: 'pause', rev: control.rev }, { idempotency: true });
      const posted = await this.post(`@${this.agent.slug} Reply with exactly the word RESUMED and nothing else.`);
      this.pausedSeq = posted.seq;
      const until = Date.now() + opts.pauseWait * 1000;
      while (Date.now() < until) {
        const card = await this.card();
        if (card.observed === 'in_turn') throw new Error('a turn ran while the room was paused');
        const messages = await api.must('GET', `/api/rooms/${this.room.id}/messages?after=${posted.seq}&limit=50`);
        if (messages.some(m => m.author_kind === 'agent')) throw new Error('the agent replied while the room was paused');
        await sleep(1500);
      }
      return `no turn for ${opts.pauseWait}s while paused (desired ${(await this.card()).desired})`;
    }, tasked ? '' : 'task');
    const paused = ok;
    ok = await this.step('resume', async () => {
      const control = await api.must('GET', `/api/rooms/${this.room.id}/control`);
      await api.must('POST', `/api/rooms/${this.room.id}/control`, { action: 'resume', rev: control.rev }, { idempotency: true });
      const reply = await this.agentMessage(this.pausedSeq, /\bRESUMED\b/i, opts.taskTimeout);
      await this.measure();
      return `queued note delivered after resume, reply #${reply.seq}`;
    }, paused ? '' : (tasked ? 'pause' : 'task'));
    const resumed = ok;
    const killBlocked = resumed ? '' : 'resume';
    if (opts.kill === 'skip') {
      this.steps.kill = { status: 'skipped', detail: '--kill skip', ms: 0 }; this.steps.recovery = { status: 'skipped', detail: '--kill skip', ms: 0 };
    } else {
      this.expectRecovery = true;
      ok = await this.step('kill', async () => {
        if (this.overCap()) throw new Error(`variant cap reached ($${this.cost.toFixed(2)}, ${this.turns} turns)`);
        const posted = await this.post(`@${this.agent.slug} Count from 1 to 40, one number per line, then reply with the word DONE.`);
        this.killPostSeq = posted.seq;
        const deadline = Date.now() + 120000; let found = null;
        while (Date.now() < deadline && !found) {
          found = findHarness({ folder: this.ctx.project.folder, supervisorMatch: opts.supervisorMatch, tool: this.tool, form: this.form });
          if (!found) await sleep(150);
        }
        if (!found) throw new Error('no harness process of this agent was visible on this host within 120s (is this the supervisor machine?)');
        try { process.kill(-found.pgid, 'SIGKILL'); } catch (error) { try { process.kill(found.pid, 'SIGKILL'); } catch { throw new Error(`could not kill pid ${found.pid}: ${error.message}`); } }
        this.killed = { pid: found.pid, pgid: found.pgid, command: found.command.slice(0, 120) };
        return `SIGKILL pgid ${found.pgid} (pid ${found.pid}: ${found.command.slice(0, 60)})`;
      }, killBlocked);
      const killed = ok;
      // A harness cut off by a signal is continued once by the server on its own (the note moves to a child turn); only a repeat, or an exit the
      // server cannot explain, raises a recovery item. Either way the agent must answer again, and a recovery item is decided Resume.
      await this.step('recovery', async () => {
        const deadline = Date.now() + 180000; let item = null; let reply = null; let last = this.killPostSeq;
        while (Date.now() < deadline && !item && !reply) {
          const messages = await api.must('GET', `/api/rooms/${this.room.id}/messages?after=${last}&limit=100`);
          for (const message of messages) {
            last = Math.max(last, message.seq);
            if (message.author_kind === 'agent' && message.author_id === this.agent.id && !reply) reply = message;
          }
          if (reply) break;
          const inbox = await api.must('GET', '/api/inbox');
          item = inbox.find(i => i.kind === 'recovery' && i.agent_id === this.agent.id && i.state === 'open' && i.can_decide);
          if (!item) await sleep(2000);
        }
        if (reply && !item) {
          const card = await this.card();
          if (card.recovery_block) throw new Error('the agent answered but is blocked in recovery');
          await this.measure();
          return `the server continued the interrupted turn on its own -> reply #${reply.seq}`;
        }
        if (!item) {
          const card = await this.card(); const inbox = await api.must('GET', '/api/inbox');
          const seen = inbox.filter(i => i.agent_id === this.agent.id).map(i => ({ kind: i.kind, state: i.state, can_decide: i.can_decide, phase: i.recovery_phase, hidden: i.inbox_hidden }));
          throw new Error(`neither a continuation reply nor a recovery item within 180s after the kill (agent: observed ${card?.observed}, desired ${card?.desired}, recovery_block ${card?.recovery_block}, evidence ${(card?.last_exit_evidence || 'none').slice(0, 200)}; inbox items for it: ${JSON.stringify(seen)})`);
        }
        const choices = (item.options || []).map(o => String(o).toLowerCase());
        const choice = item.recovery_phase === 'quiescence_wait' ? 'override' : (choices.includes('resume') ? 'resume' : (choices.includes('retry') ? 'retry' : choices[0]));
        if (!choice) throw new Error(`recovery item offers no choice: ${JSON.stringify(item.options)}`);
        await api.must('POST', `/api/inbox/${item.id}/decision`, { choice, rev: item.rev }, { idempotency: true });
        const answer = await this.agentMessage(this.killPostSeq, null, opts.taskTimeout, false);
        const card = await this.card();
        if (card.recovery_block) throw new Error('the agent is still blocked after the decision');
        await this.measure();
        return `recovery surfaced (${(item.evidence || 'no evidence').slice(0, 80).replace(/\s+/g, ' ')}); ${choice} -> reply #${answer.seq}`;
      }, killed ? '' : 'kill');
      this.expectRecovery = false;
    }
    await this.cleanup();
  }

  async cleanup() {
    const { api } = this.ctx;
    if (!this.room) return;
    await this.step('stop', async () => {
      if (!this.agent?.id) return 'no agent to stop';
      const card = await this.card();
      if (!card) return 'agent already gone';
      await api.must('POST', `/api/agents/${this.agent.id}/control`, { desired: 'stopped', rev: card.desired_rev });
      const deadline = Date.now() + 90000;
      while (Date.now() < deadline) { if ((await this.card()).observed === 'stopped') { await this.measure(); return 'stopped'; } await sleep(2000); }
      throw new Error('agent did not reach stopped within 90s');
    });
    await this.step('archive', async () => {
      const room = await api.must('GET', `/api/rooms/${this.room.id}`);
      await api.must('POST', `/api/rooms/${this.room.id}/close`, { lifecycle_rev: room.lifecycle_rev });
      const deadline = Date.now() + 90000;
      while (Date.now() < deadline) { if ((await api.must('GET', `/api/rooms/${this.room.id}`)).lifecycle === 'closed') return 'closed'; await sleep(2000); }
      throw new Error('room did not close within 90s');
    });
    await this.measure();
  }

  result() {
    const steps = Object.fromEntries(STEPS.map(name => [name, this.steps[name] || { status: 'skipped', detail: this.skip || 'not reached', ms: 0 }]));
    const failed = STEPS.filter(name => steps[name].status === 'fail');
    return { variant: this.name, tool: this.tool, form: this.form, model: this.model || '', skipped: this.skip, status: this.skip ? 'skipped' : (failed.length ? 'fail' : 'pass'),
      failed_steps: failed, steps, cost_usd: this.cost, cost_coverage: this.coverage || '', turns: this.turns, killed: this.killed || null, notes: this.notes, duration_ms: Date.now() - this.started };
  }
}

// --- reports --------------------------------------------------------------

function markdown(report) {
  const lines = [`# Variant gate`, '', `Server ${report.server}, ${report.started}, total cost $${report.total_cost_usd.toFixed(2)} (budget $${report.budget_usd}).`, '',
    '| variant | result | ' + STEPS.join(' | ') + ' | cost | time |', '| --- | --- | ' + STEPS.map(() => '---').join(' | ') + ' | --- | --- |'];
  const mark = s => ({ pass: 'pass', fail: 'FAIL', skipped: '-' }[s] || s);
  for (const v of report.variants) lines.push(`| ${v.variant} | ${v.status.toUpperCase()} | ${STEPS.map(n => mark(v.steps[n].status)).join(' | ')} | $${v.cost_usd.toFixed(3)} | ${Math.round(v.duration_ms / 1000)}s |`);
  lines.push('');
  for (const v of report.variants) {
    lines.push(`## ${v.variant} - ${v.status}`);
    if (v.skipped) lines.push(`Skipped: ${v.skipped}`);
    for (const name of STEPS) { const s = v.steps[name]; if (s.status !== 'skipped' || !v.skipped) lines.push(`- ${name}: ${s.status}${s.detail ? ` - ${s.detail}` : ''}${s.ms ? ` (${(s.ms / 1000).toFixed(1)}s)` : ''}`); }
    if (v.notes.length) lines.push(`- notes: ${v.notes.join('; ')}`);
    lines.push('');
  }
  return lines.join('\n');
}

// --- main -----------------------------------------------------------------

async function main() {
  let opts;
  try { opts = parseArgs(process.argv.slice(2)); } catch (error) { console.error(`variant-gate: ${error.message}\n`); console.error(usage()); return 2; }
  if (opts.list) {
    console.log(`Plan for ${opts.server || '(no server given)'}: ${opts.variants.join(', ')}\nSteps: ${STEPS.join(' > ')}\nCaps: $${opts.variantCap} and ${opts.turnCap} turns per variant, $${opts.budget} total.`);
    return 0;
  }
  if (!opts.server) { console.error('variant-gate: pass --server URL (or set VG_SERVER); there is no default.'); return 2; }
  try { new URL(opts.server); } catch { console.error(`variant-gate: ${opts.server} is not a URL`); return 2; }
  if (!opts.yes) { console.error(`variant-gate: this run spends real model money on ${opts.server}. Re-run with --yes to confirm.`); return 2; }
  const login = process.env.VG_LOGIN, password = process.env.VG_PASSWORD;
  if (!opts.invite && (!login || !password)) { console.error('variant-gate: set VG_LOGIN and VG_PASSWORD (the instance owner), or pass --invite.'); return 2; }
  const reportDir = opts.reportDir || path.join(os.tmpdir(), `variant-gate-${stamp()}`);
  fs.mkdirSync(reportDir, { recursive: true });
  const api = new Client(opts.server);
  let account = null;
  if (opts.invite) {
    account = await redeemInvite(api, opts.invite);
    console.error(`variant-gate: created account ${account.login} (${account.role}); deactivate it in Settings -> Users when the run is done`);
  } else await api.login(login, password);
  const me = await api.must('GET', '/api/me');
  const catalog = await api.must('GET', '/api/spawn-catalog');
  const projects = await api.must('GET', '/api/projects');
  let project = projects.find(p => p.id === opts.project || p.name === (opts.project || 'variant-gate'));
  if (!project && opts.invite) throw new Error(`project ${opts.project} is not visible to the invited account; the invitation must be for that project`);
  if (!project) {
    const folder = opts.folder || path.join(os.tmpdir(), `vg-${stamp()}`);
    fs.mkdirSync(folder, { recursive: true });
    project = await api.must('POST', '/api/projects', { name: opts.project || 'variant-gate', folder });
  }
  const ctx = { opts, api, catalog, project, me };
  const report = { server: opts.server, account: account ? account.login : login, started: new Date().toISOString(), budget_usd: opts.budget, variant_cap_usd: opts.variantCap, project: { id: project.id, name: project.name }, variants: [], total_cost_usd: 0 };
  let spent = 0;
  for (const name of opts.variants) {
    const variant = new Variant(ctx, name);
    if (spent >= opts.budget) { variant.skip = `budget $${opts.budget} reached ($${spent.toFixed(2)} spent)`; }
    else {
      console.error(`variant-gate: ${name} ...`);
      try { await variant.run(); } catch (error) { variant.steps.room = variant.steps.room || { status: 'fail', detail: `unexpected: ${error.message}`, ms: 0 }; }
    }
    const result = variant.result(); spent += result.cost_usd; report.variants.push(result); report.total_cost_usd = spent;
    console.error(`variant-gate: ${name} ${result.status}${result.failed_steps.length ? ` (${result.failed_steps.join(', ')})` : ''}${result.skipped ? ` - ${result.skipped}` : ''}`);
    fs.writeFileSync(path.join(reportDir, 'report.json'), JSON.stringify(report, null, 2));
    fs.writeFileSync(path.join(reportDir, 'report.md'), markdown(report));
  }
  console.log(markdown(report));
  console.error(`variant-gate: reports in ${reportDir}`);
  return report.variants.some(v => v.status === 'fail') ? 1 : 0;
}

if (require.main === module) main().then(code => process.exit(code), error => { console.error(`variant-gate: ${error.message}`); process.exit(1); });
module.exports = { Client, inviteToken, parseArgs, findHarness, Variant, markdown, STEPS };
